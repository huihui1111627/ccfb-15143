package vdisk

// 头部双槽位提交：
//
//	槽位 A、B 各占一个簇，各自带序号和 CRC。
//	每次提交先写"旧槽位"（序号较小者），Sync 后再写另一个并 Sync。
//	任何时刻断电：两个槽位要么都是完整的旧版本，要么一新一旧，
//	绝不会出现两个都撕裂的情况，打开时取序号最大的完好槽位。
//
// 改底包路径时扩展区随头部整段重写，因此满足"整段重写头部、断电可开"。

// currentHeader 按内存状态构造头部。
func (img *Image) currentHeader() *header {
	return &header{
		shift:           img.shift,
		dirty:           img.dirty,
		incompat:        img.incompatFeatures,
		compat:          img.compatFeatures,
		virtualClusters: img.virtualClusters,
		maxClusters:     img.maxClusters,
		l1Offset:        img.l1Start * int64(img.clusterSize),
		l1Clusters:      img.l1Clusters(),
		refL1Offset:     img.refL1Start * int64(img.clusterSize),
		refL1Clusters:   img.refL1Clusters(),
		sequence:        img.sequence,
		ext:             encodeExt(img.headerExt),
	}
}

// commitHeader 提交一个给定头部（用于 Create，特性位由参数带入）。
func (img *Image) commitHeader(h *header) error {
	if img.readOnly {
		return ErrReadOnly
	}
	if hOffExt+len(h.ext) > img.clusterSize {
		return corruptf("扩展区 %d 字节超出头部簇空间", len(h.ext))
	}
	cs := int64(img.clusterSize)
	// 读两个槽位当前序号，决定先写哪个。
	slot := make([][]byte, headerSlots)
	for i := range slot {
		b := make([]byte, cs)
		if _, err := img.blk.ReadAt(b, int64(i)*cs); err != nil {
			return err
		}
		slot[i] = b
	}
	seqs := make([]uint64, headerSlots)
	for i := range slot {
		if ph, err := parseSlot(slot[i]); err == nil {
			seqs[i] = ph.sequence
		}
	}
	first, second := 0, 1
	if seqs[1] < seqs[0] {
		first, second = 1, 0
	}
	h.sequence = img.sequence + 1
	img.sequence = h.sequence
	buf := h.encodeSlot(img.clusterSize)
	if _, err := img.blk.WriteAt(buf, int64(first)*cs); err != nil {
		return err
	}
	if err := img.blk.Sync(); err != nil {
		return err
	}
	if _, err := img.blk.WriteAt(buf, int64(second)*cs); err != nil {
		return err
	}
	if err := img.blk.Sync(); err != nil {
		return err
	}
	return nil
}

// commitCurrentHeader 用内存当前状态提交头部。
func (img *Image) commitCurrentHeader() error {
	return img.commitHeader(img.currentHeader())
}

// SetBasePath 修改底包路径。扩展区整段随头部重写（未知类型 TLV 原样保留），
// 双槽位 + Sync 保证中途断电仍可用旧头部打开。
func (img *Image) SetBasePath(path string) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.readOnly {
		return ErrReadOnly
	}
	img.headerExt = extSet(img.headerExt, extBasePath, path)
	// 先在内存里记下新路径，若头部提交失败则回滚。
	old := img.basePath
	img.basePath = path
	if err := img.commitCurrentHeader(); err != nil {
		img.basePath = old
		img.headerExt = extSet(img.headerExt, extBasePath, old)
		return err
	}
	// 重新挂载底包链（若调用方提供了 loader，则在 Open 时已挂；
	// 这里只更新记录，实际重挂由 Reopen 或下次打开完成）。
	return nil
}

// BasePath 返回头部扩展区里记录的底包路径。
func (img *Image) BasePath() string {
	img.mu.RLock()
	defer img.mu.RUnlock()
	return img.basePath
}

// Extensions 返回扩展区 TLV 的拷贝（含未知类型，type 与 value 分开）。
func (img *Image) Extensions() []ExtItem {
	img.mu.RLock()
	defer img.mu.RUnlock()
	return img.extensionsLocked()
}

func (img *Image) extensionsLocked() []ExtItem {
	out := make([]ExtItem, 0, len(img.headerExt))
	for _, r := range img.headerExt {
		out = append(out, ExtItem{Type: r.typ, Value: append([]byte(nil), r.buf...)})
	}
	return out
}

// ExtItem 是扩展区一项的外部视图。
type ExtItem struct {
	Type  uint16
	Value []byte
}

// MarkDirty / markClean 供写路径与 Close 使用。
func (img *Image) markDirtyLocked() error {
	if !img.dirty {
		img.dirty = true
		return img.commitCurrentHeader()
	}
	return nil
}
