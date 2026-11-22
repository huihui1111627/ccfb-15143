package vdisk

// ---- 物理簇与偏移换算 ----

func (img *Image) clusterToOffset(c int64) int64 { return c * int64(img.clusterSize) }

// checkAligned 校验物理偏移落在数据区、按簇对齐且不超 maxClusters。
// 所有表项在"读出来使用"和"写进去之前"都走这一道检查。
func (img *Image) checkAligned(offset uint64, where string) error {
	if offset == 0 {
		return corruptf("%s 出现零偏移表项", where)
	}
	if offset%uint64(img.clusterSize) != 0 {
		return corruptf("%s 偏移 %d 未按簇对齐", where, offset)
	}
	c := int64(offset / uint64(img.clusterSize))
	if c < firstDataCluster || c >= img.maxClusters {
		return corruptf("%s 偏移 %d 指向越界簇 %d（合法范围 [%d,%d)）",
			where, offset, c, firstDataCluster, img.maxClusters)
	}
	return nil
}

// validateMapEntry 校验一个映射表项（L1 或 L2）。
// 返回目标物理簇号；零项返回 0。
func (img *Image) validateMapEntry(raw uint64, where string) (target int64, isZero bool, err error) {
	flags := raw & entryFlagsMask
	body := raw & entryOffsetMask
	switch flags {
	case 0:
		// 未分配项：低位必须全零。
		if body != 0 {
			return 0, false, corruptf("%s 未分配项带非零体 %#x", where, raw)
		}
		return 0, false, nil
	case entryValid:
		if err := img.checkAligned(body, where); err != nil {
			return 0, false, err
		}
		return int64(body / uint64(img.clusterSize)), false, nil
	case entryZero:
		// 全零标记项：不允许再带偏移。
		if body != 0 {
			return 0, false, corruptf("%s 全零标记项带非零偏移 %#x", where, raw)
		}
		return 0, true, nil
	default:
		// valid|zero 同时出现属于非法组合。
		return 0, false, corruptf("%s 表项标志位组合非法: %#x", where, raw)
	}
}

// validateRefEntry 校验引用计数项：保留位必须为零。
func validateRefEntry(raw uint64, where string) (int64, error) {
	if raw&refReservedMask != 0 {
		return 0, corruptf("%s 引用计数项保留位非零: %#x", where, raw)
	}
	v := int64(raw & refCountMask)
	if v < 0 {
		return 0, corruptf("%s 引用计数超出 int64", where)
	}
	return v, nil
}

// ---- 整簇 IO ----

func (img *Image) readCluster(c int64) ([]byte, error) {
	buf := make([]byte, img.clusterSize)
	if _, err := img.blk.ReadAt(buf, img.clusterToOffset(c)); err != nil {
		return nil, err
	}
	return buf, nil
}

func (img *Image) writeCluster(c int64, buf []byte) error {
	if len(buf) != img.clusterSize {
		return corruptf("写簇 %d 缓冲长度 %d != 簇大小 %d", c, len(buf), img.clusterSize)
	}
	if _, err := img.blk.WriteAt(buf, img.clusterToOffset(c)); err != nil {
		return err
	}
	return nil
}

// ---- 表项读写（统一大端定长）----

func readEntry(cluster []byte, idx int64) uint64 {
	pos := idx * entrySize
	return be64(cluster[pos : pos+entrySize])
}

func writeEntry(cluster []byte, idx int64, raw uint64) {
	pos := idx * entrySize
	putBE64(cluster[pos:pos+entrySize], raw)
}

func makeValidEntry(c int64, clusterSize int) uint64 {
	return entryValid | uint64(c*int64(clusterSize))
}

// ---- 根表加载 ----

// loadRootTables 读入 L1/refL1 的全部簇并逐项校验，建立内存索引。
func (img *Image) loadRootTables(h *header) error {
	cs := int64(img.clusterSize)
	if h.l1Clusters != (img.virtualClusters+img.entriesPer-1)/img.entriesPer {
		return corruptf("L1 簇数 %d 与虚拟簇数 %d 不自洽", h.l1Clusters, img.virtualClusters)
	}
	if h.refL1Clusters != (img.maxClusters+img.entriesPer-1)/img.entriesPer {
		return corruptf("refL1 簇数 %d 与物理簇上限 %d 不自洽", h.refL1Clusters, img.maxClusters)
	}
	// L1 根表本身的物理簇范围。
	l1Start := h.l1Offset / cs
	refL1Start := h.refL1Offset / cs
	l1End := l1Start + h.l1Clusters
	refL1End := refL1Start + h.refL1Clusters
	if l1End > img.maxClusters || refL1End > img.maxClusters {
		return corruptf("根表范围越过物理簇上限")
	}
	// 两块根表不得重叠。
	if !(l1End <= refL1Start || refL1End <= l1Start) {
		return corruptf("L1 与 refL1 物理范围重叠")
	}

	// L1 的逻辑项数 = 虚拟簇组数 ceil(V/E)，而不是虚拟簇数。
	l1Groups := (img.virtualClusters + img.entriesPer - 1) / img.entriesPer
	idx := int64(0)
	for ci := int64(0); ci < h.l1Clusters; ci++ {
		buf, err := img.readCluster(l1Start + ci)
		if err != nil {
			return err
		}
		for j := int64(0); j < img.entriesPer && idx < l1Groups; j++ {
			raw := readEntry(buf, j)
			target, isZero, err := img.validateMapEntry(raw, "L1")
			if err != nil {
				return err
			}
			if isZero {
				return corruptf("L1 项不允许全零标记")
			}
			img.l1[idx] = target
			idx++
		}
	}
	idx = 0
	for ci := int64(0); ci < h.refL1Clusters; ci++ {
		buf, err := img.readCluster(refL1Start + ci)
		if err != nil {
			return err
		}
		for j := int64(0); j < img.entriesPer && idx < img.maxClusters; j++ {
			raw := readEntry(buf, j)
			target, isZero, err := img.validateMapEntry(raw, "refL1")
			if err != nil {
				return err
			}
			if isZero {
				return corruptf("refL1 项不允许全零标记")
			}
			img.refL1[idx] = target
			idx++
		}
	}
	img.l1Start = l1Start
	img.refL1Start = refL1Start
	return nil
}

// loadAllRefCounts 顺着 refL1 -> refL2 把所有引用计数项读入内存，
// 同时校验保留位、对齐和角色冲突。
func (img *Image) loadAllRefCounts() error {
	// 预分配布局下，同一组的 refL1 项应全部指向同一个 refL2，
	// 且这些 refL2 必须是 refL1 根表之后连续的 B 个簇。
	img.refL2Start = img.refL1Start + img.refL1Clusters()
	b := img.refL1Clusters()
	for g := int64(0); g < b; g++ {
		wantRefL2 := img.refL2Start + g
		for j := int64(0); j < img.entriesPer; j++ {
			pc := g*img.entriesPer + j
			if pc >= img.maxClusters {
				break
			}
			if got := img.refL1[pc]; got != wantRefL2 {
				return corruptf("refL1 项 pc=%d 指向 %d，与预分配 refL2 %d 不符", pc, got, wantRefL2)
			}
		}
	}
	// 每个 refL2 整簇读一次，校验组内全部计数项的保留位。
	for g := int64(0); g < b; g++ {
		refL2 := img.refL2Start + g
		buf, err := img.readCluster(refL2)
		if err != nil {
			return err
		}
		for j := int64(0); j < img.entriesPer; j++ {
			pc := g*img.entriesPer + j
			if pc >= img.maxClusters {
				break
			}
			v, err := validateRefEntry(readEntry(buf, j), "refL2")
			if err != nil {
				return err
			}
			img.refCount[pc] = v
		}
	}
	// 角色冲突：同一个物理簇不能同时担任多种表/数据角色。
	role := map[int64]string{}
	for c := img.l1Start; c < img.l1Start+img.l1Clusters(); c++ {
		role[c] = "L1"
	}
	for c := img.refL1Start; c < img.refL1Start+img.refL1Clusters(); c++ {
		if r, ok := role[c]; ok {
			return corruptf("簇 %d 角色冲突: %s/refL1", c, r)
		}
		role[c] = "refL1"
	}
	for c := img.refL2Start; c < img.refL2Start+img.refL1Clusters(); c++ {
		if r, ok := role[c]; ok {
			return corruptf("簇 %d 角色冲突: %s/refL2", c, r)
		}
		role[c] = "refL2"
	}
	for g, l2 := range img.l1 {
		if l2 == 0 {
			continue
		}
		if r, ok := role[l2]; ok {
			return corruptf("簇 %d 角色冲突: %s/L2(group=%d)", l2, r, g)
		}
		role[l2] = "L2"
	}
	// 数据簇角色在扫描 L2 时再查（loadRootTables 阶段还没读 L2）。
	if err := img.checkL2Roles(role); err != nil {
		return err
	}
	return nil
}

// checkL2Roles 读所有 L2 表，校验项合法性并登记数据簇角色。
// checkL2Roles 按组读所有 L2 表，校验项合法性并登记数据簇角色。
func (img *Image) checkL2Roles(role map[int64]string) error {
	l1Groups := (img.virtualClusters + img.entriesPer - 1) / img.entriesPer
	for g := int64(0); g < l1Groups; g++ {
		l2c := img.l1[g]
		if l2c == 0 {
			continue
		}
		buf, err := img.readCluster(l2c)
		if err != nil {
			return err
		}
		groupBase := g * img.entriesPer
		for j := int64(0); j < img.entriesPer; j++ {
			targetVC := groupBase + j
			if targetVC >= img.virtualClusters {
				// 尾组里超出虚拟盘范围的表项不参与寻址，必须为零。
				if readEntry(buf, j) != 0 {
					return corruptf("L2(簇 %d) 尾部越界项非零", l2c)
				}
				continue
			}
			raw := readEntry(buf, j)
			target, isZero, err := img.validateMapEntry(raw, "L2")
			if err != nil {
				return err
			}
			if target == 0 || isZero {
				continue
			}
			if r, ok := role[target]; ok {
				return corruptf("簇 %d 角色冲突: %s/数据(vc=%d)", target, r, targetVC)
			}
			role[target] = "data"
		}
	}
	return nil
}

// l1Clusters / refL1Clusters 便于各处复用。
func (img *Image) l1Clusters() int64    { return (img.virtualClusters + img.entriesPer - 1) / img.entriesPer }
func (img *Image) refL1Clusters() int64 { return (img.maxClusters + img.entriesPer - 1) / img.entriesPer }
