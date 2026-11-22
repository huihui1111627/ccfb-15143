package vdisk

// 本文件处理物理簇分配与引用计数的递归更新。
//
// 递归的根源：给引用计数表分配新 refL2 簇时，这个新簇本身又要被计数。
// 处理办法是把"分配簇"和"把簇挂进某张表"拆成两个阶段：
//
//  1. allocCluster 只从 freeSet/水位线拿一个未发布的裸簇，初始计数为 0；
//  2. 调用方先把裸簇内容写好并 Sync（元数据只指向已落盘的数据）；
//  3. 再更新指向它的父表项并把它的引用计数 +1。
//
// 这样 refL2 可以安全地记录"包括表簇在内的任意物理簇"，不会自咬。

// buildFreeSet 在打开时确定文件高水位，空闲集合初始为空。
//
// 打开阶段不把"引用计数为 0 的簇"自动登记为空闲：这类簇既可能是运行期正常
// 释放的，也可能是断电产生的崩溃孤儿，磁盘表示完全相同，应统一交给 Check
// 作为泄漏报告、再由 Repair 回收。分配器 allocCluster 在 freeSet 为空时，
// 会直接复用高水位内任意零计数非根簇（复用即回收），因此空间不会只增不减。
// freeSet 只在运行期 dropRef 归零、或 Repair 回收时显式填充。
func (img *Image) buildFreeSet() {
	img.freeSet = map[int64]struct{}{}
	img.highWater = firstDataCluster
	if size := img.blk.Size(); size > 0 {
		img.highWater = size / int64(img.clusterSize)
		if size%int64(img.clusterSize) != 0 {
			img.highWater++
		}
	}
}

func (img *Image) isRootCluster(c int64) bool {
	if c >= img.l1Start && c < img.l1Start+img.l1Clusters() {
		return true
	}
	if c >= img.refL1Start && c < img.refL1Start+img.refL1Clusters() {
		return true
	}
	// refL2 表簇在创建时整段预分配，同样属于不可回收的根区。
	if c >= img.refL2Start && c < img.refL2Start+img.refL1Clusters() {
		return true
	}
	return false
}

// allocCluster 取一个未发布的裸数据/表簇。
// 返回的簇内容已清零并落盘，引用计数初始为 0，由调用方负责挂表与加计数。
func (img *Image) allocCluster() (int64, error) {
	if img.readOnly {
		return 0, ErrReadOnly
	}
	// 优先复用 freeSet 里的簇（取簇号最小的，行为可预测）。
	if pick := img.pickFree(); pick >= 0 {
		zero := make([]byte, img.clusterSize)
		if err := img.writeCluster(pick, zero); err != nil {
			return 0, err
		}
		return pick, nil
	}
	// freeSet 空：在高水位内寻找任何引用计数为 0 的非根簇。
	// 这同时覆盖了"打开后未显式 Repair 的崩溃孤儿/历史空闲簇"，
	// 复用即回收，保证文件不会只增不减而耗尽物理簇上限。
	for c := int64(firstDataCluster); c < img.highWater; c++ {
		if img.isRootCluster(c) || img.refCount[c] != 0 {
			continue
		}
		zero := make([]byte, img.clusterSize)
		if err := img.writeCluster(c, zero); err != nil {
			return 0, err
		}
		return c, nil
	}
	if img.highWater >= img.maxClusters {
		return 0, ErrNoSpace
	}
	c := img.highWater
	img.highWater++
	need := (c + 1) * int64(img.clusterSize)
	if img.blk.Size() < need {
		if err := img.blk.Truncate(need); err != nil {
			return 0, err
		}
	}
	zero := make([]byte, img.clusterSize)
	if err := img.writeCluster(c, zero); err != nil {
		return 0, err
	}
	return c, nil
}

// pickFree 从空闲集合取簇号最小的簇，没有则返回 -1。
func (img *Image) pickFree() int64 {
	if len(img.freeSet) == 0 {
		return -1
	}
	pick := int64(-1)
	for c := range img.freeSet {
		if pick == -1 || c < pick {
			pick = c
		}
	}
	delete(img.freeSet, pick)
	return pick
}

// ---- 引用计数读取/落盘 ----

func (img *Image) getRefCount(c int64) int64 { return img.refCount[c] }

// storeRefCount 把簇 c 的引用计数写成 v（v>=0）。
// refL2 表簇在创建时已全部预分配，所以这里只做两级查表后写定长项，
// 不存在"给计数表分簇又要给该簇计数"的递归。
func (img *Image) storeRefCount(c, v int64) error {
	if c < firstDataCluster || c >= img.maxClusters {
		return corruptf("引用计数簇号 %d 越界", c)
	}
	if v < 0 || v > maxRefCount {
		return corruptf("引用计数 %d 越界", v)
	}
	img.refCount[c] = v
	return img.writeRefCountEntry(c, v)
}

// addRef / dropRef 调整引用计数；dropRef 归零时把簇放回 freeSet。
func (img *Image) addRef(c int64) error {
	v := img.getRefCount(c) + 1
	if err := img.storeRefCount(c, v); err != nil {
		return err
	}
	return nil
}

func (img *Image) dropRef(c int64) error {
	v := img.getRefCount(c)
	if v <= 0 {
		return corruptf("簇 %d 引用计数已为 %d，无法再减", c, v)
	}
	v--
	if err := img.storeRefCount(c, v); err != nil {
		return err
	}
	if v == 0 && !img.isRootCluster(c) {
		img.freeSet[c] = struct{}{}
	}
	return nil
}

// writeL1Entry 把第 group 个 L1 项写回物理根表。
// 注意：L1 的逻辑项就是按"组"编号的（l1[] 长度 = 组数 ceil(V/E)），
// 因此组号直接是 L1 表内的项偏移，只需按每簇 E 项定位到具体根簇，不能再除一次 E。
func (img *Image) writeL1Entry(group, l2 int64) error {
	rootIdx := group / img.entriesPer
	entryIdx := group % img.entriesPer
	if rootIdx >= img.l1Clusters() {
		return corruptf("L1 组号 %d 超出根表簇数 %d", group, img.l1Clusters())
	}
	rootCluster := img.l1Start + rootIdx
	buf, err := img.readCluster(rootCluster)
	if err != nil {
		return err
	}
	var raw uint64
	if l2 != 0 {
		raw = makeValidEntry(l2, img.clusterSize)
	}
	writeEntry(buf, entryIdx, raw)
	return img.writeCluster(rootCluster, buf)
}

// ensureL2 保证虚拟簇 vc 所在组的 L2 表存在，返回 L2 物理簇号与组内表项号。
// 一个 L2 表簇覆盖连续 E 个虚拟簇，L1 按组 g=vc/E 指向它。
func (img *Image) ensureL2(vc int64) (l2c, entryIdx int64, err error) {
	g := vc / img.entriesPer
	entryIdx = vc % img.entriesPer
	if l2 := img.l1[g]; l2 != 0 {
		return l2, entryIdx, nil
	}
	l2, err := img.allocCluster()
	if err != nil {
		return 0, 0, err
	}
	zero := make([]byte, img.clusterSize)
	if err := img.writeCluster(l2, zero); err != nil {
		return 0, 0, err
	}
	if err := img.blk.Sync(); err != nil {
		return 0, 0, err
	}
	if err := img.writeL1Entry(g, l2); err != nil {
		return 0, 0, err
	}
	img.l1[g] = l2
	if err := img.addRef(l2); err != nil {
		return 0, 0, err
	}
	return l2, entryIdx, nil
}

// l2Group / l2EntryIndex 把虚拟簇号换算成 L1 组号与 L2 组内表项号。
func (img *Image) l2Group(vc int64) int64   { return vc / img.entriesPer }
func (img *Image) l2EntryIndex(vc int64) int64 { return vc % img.entriesPer }

// readL2Entry / writeL2Entry 操作单个二级表项。
func (img *Image) readL2Entry(l2, idx int64) (uint64, error) {
	buf, err := img.readCluster(l2)
	if err != nil {
		return 0, err
	}
	return readEntry(buf, idx), nil
}

func (img *Image) writeL2Entry(l2, idx int64, raw uint64) error {
	buf, err := img.readCluster(l2)
	if err != nil {
		return err
	}
	writeEntry(buf, idx, raw)
	return img.writeCluster(l2, buf)
}
