package vdisk

// Create 在给定块上格式化一个新镜像。
//
// 物理布局（头部两簇之后依次连续排布）：
//
//	L1 根表(A 簇) → refL1 根表(B 簇) → refL2 表(B 簇)
//
// 关键决定：所有 refL2 表簇在创建时一次性预分配。这样运行期写引用计数
// 只是"两级查表后改一个定长项"，不再有"给计数表分簇又要给这个簇计数"
// 的递归；容量上限在 planCapacity 的不动点里已经把这 B 个 refL2 算进去。
//
// 落盘顺序：先把全部元数据/数据簇写好并 Sync，再发布头部；
// 头部两个槽位先后写入并各自 Sync，中途断电至少有一个完整槽位可用。
func Create(blk Block, opts Options) (*Image, error) {
	shift := opts.ClusterShift
	if shift == 0 {
		shift = defaultShift
	}
	if err := validateShift(shift); err != nil {
		return nil, err
	}
	if opts.VirtualSize <= 0 {
		return nil, corruptf("虚拟盘大小必须为正，得到 %d", opts.VirtualSize)
	}
	clusterSize := 1 << shift
	entriesPer := int64(clusterSize / entrySize)
	virtualClusters := (opts.VirtualSize + int64(clusterSize) - 1) / int64(clusterSize)

	maxClusters, l1Clusters, refL1Clusters := planCapacity(virtualClusters, entriesPer)
	if maxClusters >= int64(1<<(63-shift)) {
		return nil, corruptf("虚拟盘过大，簇偏移超出 62 位")
	}

	ext := encodeExt([]extRecord{{typ: extBasePath, buf: []byte(opts.BasePath)}})
	if hOffExt+len(ext) > clusterSize {
		return nil, corruptf("扩展区 %d 字节超出头部簇剩余空间 %d", len(ext), clusterSize-hOffExt)
	}

	img := &Image{
		shift:           shift,
		clusterSize:     clusterSize,
		entriesPer:      entriesPer,
		virtualClusters: virtualClusters,
		maxClusters:     maxClusters,
		l1:              make([]int64, (virtualClusters+entriesPer-1)/entriesPer),
		refL1:           make([]int64, maxClusters),
		refCount:        map[int64]int64{},
		freeSet:         map[int64]struct{}{},
		basePath:        opts.BasePath,
		headerExt:       []extRecord{{typ: extBasePath, buf: []byte(opts.BasePath)}},
		incompatFeatures: opts.IncompatFeature,
		compatFeatures:   opts.CompatFeature,
		dirty:           true, // 刚创建、尚未正常关闭
		sequence:        1,
	}
	img.wrapStats(blk)

	// 1) 扩展文件到头部两簇，先落全零头部。
	if err := blk.Truncate(int64(headerSlots * clusterSize)); err != nil {
		return nil, err
	}
	zero := make([]byte, clusterSize)
	for i := 0; i < headerSlots; i++ {
		if _, err := blk.WriteAt(zero, int64(i)*int64(clusterSize)); err != nil {
			return nil, err
		}
	}

	// 2) 连续预留根区：L1(A) + refL1(B) + refL2(B)。
	img.highWater = firstDataCluster
	l1Start := img.highWater
	img.highWater += l1Clusters
	refL1Start := img.highWater
	img.highWater += refL1Clusters
	refL2Start := img.highWater
	img.highWater += refL1Clusters // refL2 数量与 refL1 相同，都是 B
	img.l1Start = l1Start
	img.refL1Start = refL1Start
	img.refL2Start = refL2Start

	// 3) 写全零的 L1、refL1、refL2 根/表簇并扩文件。
	for c := l1Start; c < img.highWater; c++ {
		if err := img.writeCluster(c, zero); err != nil {
			return nil, err
		}
	}
	need := img.highWater * int64(clusterSize)
	if blk.Size() < need {
		if err := blk.Truncate(need); err != nil {
			return nil, err
		}
	}
	if err := blk.Sync(); err != nil {
		return nil, err
	}

	// 4) 建立 refL1 -> refL2 的映射（组 g 的计数项全部放进 refL2Start+g），
	//    并把根区所有簇的初始引用计数写好。
	for g := int64(0); g < refL1Clusters; g++ {
		refL2 := refL2Start + g
		for pc := g * entriesPer; pc < (g+1)*entriesPer && pc < maxClusters; pc++ {
			img.refL1[pc] = refL2
		}
		if err := img.writeRefL1Cluster(g, refL2); err != nil {
			return nil, err
		}
	}
	// L1、refL1 根簇各被头部引用一次，计数 1；
	// refL2 簇被其组内全部 refL1 项指向，计数 = 该组实际项数。
	for c := l1Start; c < refL2Start; c++ {
		img.refCount[c] = 1
		if err := img.writeRefCountEntry(c, 1); err != nil {
			return nil, err
		}
	}
	for g := int64(0); g < refL1Clusters; g++ {
		refL2 := refL2Start + g
		groupSize := entriesPer
		if rem := maxClusters - g*entriesPer; rem < groupSize {
			groupSize = rem
		}
		img.refCount[refL2] = groupSize
		if err := img.writeRefCountEntry(refL2, groupSize); err != nil {
			return nil, err
		}
	}
	if err := blk.Sync(); err != nil {
		return nil, err
	}

	// 5) 发布两个头部槽位。
	h := img.buildHeader(opts.IncompatFeature, opts.CompatFeature, l1Start, l1Clusters, refL1Start, refL1Clusters)
	if err := img.commitHeader(h); err != nil {
		return nil, err
	}
	return img, nil
}

func (img *Image) buildHeader(incompat, compat uint32, l1Start, l1Clusters, refL1Start, refL1Clusters int64) *header {
	return &header{
		shift:           img.shift,
		dirty:           img.dirty,
		incompat:        incompat,
		compat:          compat,
		virtualClusters: img.virtualClusters,
		maxClusters:     img.maxClusters,
		l1Offset:        l1Start * int64(img.clusterSize),
		l1Clusters:      l1Clusters,
		refL1Offset:     refL1Start * int64(img.clusterSize),
		refL1Clusters:   refL1Clusters,
		sequence:        img.sequence,
		ext:             encodeExt(img.headerExt),
	}
}

// writeRefL1Cluster 把第 g 个 refL1 根簇整簇写成"组内每项都指向 refL2"。
func (img *Image) writeRefL1Cluster(g, refL2 int64) error {
	buf := make([]byte, img.clusterSize)
	raw := makeValidEntry(refL2, img.clusterSize)
	base := g * img.entriesPer
	for j := int64(0); j < img.entriesPer && base+j < img.maxClusters; j++ {
		writeEntry(buf, j, raw)
	}
	return img.writeCluster(img.refL1Start+g, buf)
}

// writeRefCountEntry 只做两级查表后写一个计数项，不分配任何簇。
func (img *Image) writeRefCountEntry(c, v int64) error {
	refL2 := img.refL1[c]
	if refL2 == 0 {
		return corruptf("簇 %d 缺少预分配的 refL2 表", c)
	}
	buf, err := img.readCluster(refL2)
	if err != nil {
		return err
	}
	writeEntry(buf, c%img.entriesPer, uint64(v))
	return img.writeCluster(refL2, buf)
}
