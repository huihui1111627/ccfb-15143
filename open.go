package vdisk

import (
	"errors"
	"hash/crc64"
)

// BaseLoader 按扩展区里记录的路径打开只读底包。
// 库本身不解析文件系统路径之外的东西，测试可以传入内存块版本。
type BaseLoader func(path string) (*Image, error)

// parseSlot 解析并严格校验一个头部槽位。校验不过即视为该槽位损坏。
func parseSlot(buf []byte) (*header, error) {
	if len(buf) < hOffExt {
		return nil, corruptf("头部簇长度不足")
	}
	if string(buf[hOffMagic:hOffMagic+len(magic)]) != magic {
		return nil, corruptf("魔数不匹配")
	}
	if buf[hOffVersion] != version {
		return nil, corruptf("版本号 %d 不被支持（期望 %d）", buf[hOffVersion], version)
	}
	shift := uint(buf[hOffClusterShift])
	if err := validateShift(shift); err != nil {
		return nil, err
	}
	clusterSize := 1 << shift
	if len(buf) < clusterSize {
		return nil, corruptf("头部缓冲 %d 小于簇大小 %d", len(buf), clusterSize)
	}
	if buf[hOffReserved] != 0 {
		return nil, corruptf("头部保留字节非零: %#x", buf[hOffReserved])
	}
	dirtyByte := buf[hOffDirty]
	if dirtyByte > 1 {
		return nil, corruptf("脏标记取值非法: %d", dirtyByte)
	}
	storedCRC := be64(buf[hOffCRC:])
	// 校验时把 CRC 字段视为零，覆盖槽位固定前缀。
	calcCRC := crc64.Checksum(buf[:hOffCRC], crcTable)
	if storedCRC != calcCRC {
		return nil, corruptf("头部 CRC 校验失败（存 %#x 算 %#x）", storedCRC, calcCRC)
	}
	h := &header{
		shift:           shift,
		dirty:           dirtyByte == 1,
		incompat:        be32(buf[hOffIncompat:]),
		compat:          be32(buf[hOffCompat:]),
		virtualClusters: int64(be64(buf[hOffVirtualClusters:])),
		maxClusters:     int64(be64(buf[hOffMaxClusters:])),
		l1Offset:        int64(be64(buf[hOffL1Offset:])),
		l1Clusters:      int64(be64(buf[hOffL1Clusters:])),
		refL1Offset:     int64(be64(buf[hOffRefL1Offset:])),
		refL1Clusters:   int64(be64(buf[hOffRefL1Clusters:])),
		sequence:        be64(buf[hOffSequence:]),
	}
	// 偏移必须按簇对齐；表长度必须为正。
	cs := int64(clusterSize)
	for _, off := range []int64{h.l1Offset, h.refL1Offset} {
		if off < cs*headerSlots || off%cs != 0 {
			return nil, corruptf("根表偏移 %d 未按簇对齐或落在头部区内", off)
		}
	}
	if h.l1Clusters <= 0 || h.refL1Clusters <= 0 || h.virtualClusters <= 0 || h.maxClusters <= 0 {
		return nil, corruptf("头部长度字段出现非正值")
	}
	extLen := int(be32(buf[hOffExtLen:]))
	if extLen < 0 || hOffExt+extLen > clusterSize {
		return nil, corruptf("扩展区长度 %d 越界", extLen)
	}
	h.ext = append([]byte(nil), buf[hOffExt:hOffExt+extLen]...)
	if _, err := parseExt(h.ext); err != nil {
		return nil, err
	}
	return h, nil
}

// chooseSlot 从两个槽位中选出最新的完好槽位。
// 规则：CRC 合法且序号最大者胜；两个都坏才报损坏。
func chooseSlot(a, b []byte) (*header, error) {
	ha, errA := parseSlot(a)
	hb, errB := parseSlot(b)
	switch {
	case errA == nil && errB == nil:
		if hb.sequence > ha.sequence {
			return hb, nil
		}
		return ha, nil
	case errA == nil:
		return ha, nil
	case errB == nil:
		return hb, nil
	default:
		return nil, corruptf("两个头部槽位均损坏：A(%v) B(%v)", errA, errB)
	}
}

// Open 打开镜像。readOnly 时不允许写，也不会修改脏标记。
// loader 用于按头部扩展区里的底包路径打开下一层；为 nil 表示无底包（或路径留空）。
func Open(blk Block, readOnly bool, loader BaseLoader) (*Image, error) {
	// 先读簇 0 拿到簇大小（簇大小本身在槽位内，所以先按最小簇读两次）。
	probe := make([]byte, 1<<maxClusterShift)
	if _, err := blk.ReadAt(probe, 0); err != nil {
		return nil, err
	}
	shift := uint(probe[hOffClusterShift])
	if err := validateShift(shift); err != nil {
		return nil, err
	}
	cs := 1 << shift
	slotA := make([]byte, cs)
	slotB := make([]byte, cs)
	if _, err := blk.ReadAt(slotA, 0); err != nil {
		return nil, err
	}
	if _, err := blk.ReadAt(slotB, int64(cs)); err != nil {
		return nil, err
	}
	h, err := chooseSlot(slotA, slotB)
	if err != nil {
		return nil, err
	}
	// 不认识的不兼容特性位：拒绝打开。兼容位忽略即可。
	if unknown := h.incompat &^ knownIncompatFeatures; unknown != 0 {
		return nil, errors.Join(ErrIncompatible, corruptf("未知不兼容特性位 %#x", unknown))
	}

	entriesPer := int64((1 << h.shift) / entrySize)
	l1Groups := (h.virtualClusters + entriesPer - 1) / entriesPer
	img := &Image{
		readOnly:        readOnly,
		shift:           h.shift,
		clusterSize:     1 << h.shift,
		entriesPer:      entriesPer,
		virtualClusters: h.virtualClusters,
		maxClusters:     h.maxClusters,
		l1:              make([]int64, l1Groups),
		refL1:           make([]int64, h.maxClusters),
		refCount:        map[int64]int64{},
		freeSet:         map[int64]struct{}{},
		dirty:           h.dirty,
		sequence:        h.sequence,
		incompatFeatures: h.incompat,
		compatFeatures:   h.compat,
	}
	img.wrapStats(blk)

	recs, err := parseExt(h.ext)
	if err != nil {
		return nil, err
	}
	img.basePath = extGet(recs, extBasePath)
	img.headerExt = recs

	// 载入并逐项校验 L1 与 refL1。
	if err := img.loadRootTables(h); err != nil {
		return nil, err
	}
	// 载入全部引用计数项（顺带校验保留位/对齐）。
	if err := img.loadAllRefCounts(); err != nil {
		return nil, err
	}

	if img.basePath != "" && loader != nil {
		base, err := loader(img.basePath)
		if err != nil {
			// 底包缺失可降级；其它错误（损坏、版本不符等）仍属致命问题。
			if errors.Is(err, ErrBaseMissing) {
				img.baseMissing = true
			} else {
				return nil, err
			}
		} else {
			if !base.readOnly {
				// 底包必须只读，否则链上层的"未分配项去底包找"没有崩溃安全保证。
				return nil, corruptf("底包 %s 必须以只读方式打开", img.basePath)
			}
			img.base = base
		}
	}

	// 脏镜像：上次未正常关闭，先全盘重算引用计数。
	if img.dirty {
		rep, err := img.RecomputeRefCounts()
		if err != nil {
			return nil, err
		}
		img.dirtyReport = rep
		if !readOnly {
			img.dirty = false
			if err := img.commitCurrentHeader(); err != nil {
				return nil, err
			}
		}
	}
	img.buildFreeSet()
	return img, nil
}
