package vdisk

import (
	"errors"
	"io"
)

// VirtualSize 返回虚拟盘字节大小（簇对齐）。
func (img *Image) VirtualSize() int64 {
	return img.virtualClusters * int64(img.clusterSize)
}

// ClusterSize 返回簇字节大小。
func (img *Image) ClusterSize() int { return img.clusterSize }

// ReadAt 实现 io.ReaderAt。未映射的虚拟簇沿镜像链向下找底包，
// 整条链都没有则读出全零。
func (img *Image) ReadAt(p []byte, off int64) (int, error) {
	img.mu.RLock()
	defer img.mu.RUnlock()
	if off < 0 {
		return 0, corruptf("负读偏移 %d", off)
	}
	total := img.VirtualSize()
	n := len(p)
	if off >= total {
		// 与标准 ReaderAt 语义一致：盘外读零字节并给 EOF。
		return 0, io.EOF
	}
	if off+int64(n) > total {
		n = int(total - off)
		p = p[:n]
	}
	img.logicalR += int64(n)

	cs := int64(img.clusterSize)
	done := 0
	for done < n {
		pos := off + int64(done)
		vc := pos / cs
		inOff := pos % cs
		take := int(cs - inOff)
		if take > n-done {
			take = n - done
		}
		got, err := img.readVirtual(vc, inOff, p[done:done+take])
		if err != nil {
			return done, err
		}
		done += got
	}
	if done < len(p) {
		return done, io.EOF
	}
	return done, nil
}

// readVirtual 从本层或底包链读取一个虚拟簇的一段。
func (img *Image) readVirtual(vc, inOff int64, dst []byte) (int, error) {
	cs := int64(img.clusterSize)
	l2c := img.l1[img.l2Group(vc)]
	if l2c != 0 {
		raw, err := img.readL2Entry(l2c, img.l2EntryIndex(vc))
		if err != nil {
			return 0, err
		}
		target, isZero, err := img.validateMapEntry(raw, "L2")
		if err != nil {
			return 0, err
		}
		switch {
		case isZero:
			// 全零标记：明确不去底包找。
			zeroFill(dst)
			return len(dst), nil
		case target != 0:
			buf, err := img.readCluster(target)
			if err != nil {
				return 0, err
			}
			copy(dst, buf[inOff:inOff+int64(len(dst))])
			return len(dst), nil
		}
	}
	// 本层未分配：下沉到底包；没有底包就是全零。
	if img.base != nil {
		return img.base.ReadAt(dst, vc*cs+inOff)
	}
	zeroFill(dst)
	return len(dst), nil
}

func zeroFill(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// WriteAt 实现 io.WriterAt。支持跨簇、不对齐写。
// 元数据发布严格晚于数据落盘，崩溃后最多泄漏簇。
func (img *Image) WriteAt(p []byte, off int64) (int, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.readOnly {
		return 0, ErrReadOnly
	}
	if off < 0 {
		return 0, corruptf("负写偏移 %d", off)
	}
	total := img.VirtualSize()
	if off+int64(len(p)) > total {
		return 0, corruptf("写范围 [%d,%d) 超出虚拟盘大小 %d", off, off+int64(len(p)), total)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := img.markDirtyLocked(); err != nil {
		return 0, err
	}
	img.logicalW += int64(len(p))

	cs := int64(img.clusterSize)
	done := 0
	for done < len(p) {
		pos := off + int64(done)
		vc := pos / cs
		inOff := pos % cs
		take := int(cs - inOff)
		if take > len(p)-done {
			take = len(p) - done
		}
		if err := img.writeVirtualRange(vc, inOff, p[done:done+take]); err != nil {
			return done, err
		}
		done += take
	}
	if err := img.blk.Sync(); err != nil {
		return done, err
	}
	return len(p), nil
}

// writeVirtualRange 写一个虚拟簇内的连续一段（长度不跨簇边界）。
func (img *Image) writeVirtualRange(vc, inOff int64, data []byte) error {
	cs := int64(img.clusterSize)
	entry, err := img.currentL2Entry(vc)
	if err != nil {
		return err
	}
	// 整簇写且当前没有有效数据簇：可以直接换新簇，省一次读旧簇。
	fullCluster := inOff == 0 && len(data) == int(cs)
	if fullCluster && entry.target == 0 {
		return img.attachNewDataCluster(vc, data, entry.isZero)
	}

	var cluster []byte
	switch {
	case entry.target != 0:
		// 已有本层数据簇：读-改-写（原地更新，引用计数不变）。
		cluster, err = img.readCluster(entry.target)
		if err != nil {
			return err
		}
	case entry.isZero:
		// 零簇标记上的部分写：零簇本就是全零，直接开新簇并先填零。
		cluster = make([]byte, cs)
	default:
		// 未映射：先从底包链把旧内容捞上来做 COW。
		cluster = make([]byte, cs)
		if img.base != nil {
			if _, err := img.base.ReadAt(cluster, vc*cs); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		}
	}

	if entry.target == 0 {
		// 零簇标记或未映射的部分写：分配新簇，把合并后的整簇内容挂上。
		return img.attachNewDataCluster(vc, mergeInto(cluster, inOff, data), entry.isZero)
	}
	copy(cluster[inOff:], data)
	if err := img.writeCluster(entry.target, cluster); err != nil {
		return err
	}
	return nil
}

// attachNewDataCluster 分配一个数据簇、写好内容并发布 L2 表项。
// 若该虚拟簇此前是零簇标记，调用方已把数据合并进 content。
func (img *Image) attachNewDataCluster(vc int64, content []byte, wasZero bool) error {
	if len(content) != img.clusterSize {
		return corruptf("挂载新数据簇内容长度 %d 不是整簇", len(content))
	}
	c, err := img.allocCluster()
	if err != nil {
		return err
	}
	// 阶段 1：数据先落盘。
	if err := img.writeCluster(c, content); err != nil {
		return err
	}
	if err := img.blk.Sync(); err != nil {
		return err
	}
	// 阶段 2：先加引用计数（崩溃只会让新簇泄漏），再发布 L2 指针。
	if err := img.addRef(c); err != nil {
		return err
	}
	l2, entryIdx, err := img.ensureL2(vc)
	if err != nil {
		return err
	}
	if err := img.writeL2Entry(l2, entryIdx, makeValidEntry(c, img.clusterSize)); err != nil {
		return err
	}
	_ = wasZero
	return nil
}

// ZeroAt 把 [off, off+length) 标成全零。只改零簇标记，不写数据簇，
// 也不会再下沉到底包查找。
func (img *Image) ZeroAt(off, length int64) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.readOnly {
		return ErrReadOnly
	}
	if off < 0 || length < 0 || off+length > img.VirtualSize() {
		return corruptf("ZeroAt 范围 [%d,%d) 非法（盘大小 %d）", off, off+length, img.VirtualSize())
	}
	if length == 0 {
		return nil
	}
	if err := img.markDirtyLocked(); err != nil {
		return err
	}
	img.logicalW += length
	cs := int64(img.clusterSize)
	end := off + length
	vcStart := off / cs
	vcEnd := (end - 1) / cs
	for vc := vcStart; vc <= vcEnd; vc++ {
		vcLo := vc * cs
		vcHi := vcLo + cs
		// 与零区间完全不相交的簇（理论上循环边界已排除，防御性处理）。
		if vcHi <= off || vcLo >= end {
			continue
		}
		whole := off <= vcLo && end >= vcHi
		entry, err := img.currentL2Entry(vc)
		if err != nil {
			return err
		}
		if whole {
			if err := img.markWholeZero(vc, entry); err != nil {
				return err
			}
			continue
		}
		// 部分零写：退化为普通写，数据内容是零。
		lo := max64(off, vcLo) - vcLo
		hi := min64(end, vcHi) - vcLo
		zero := make([]byte, hi-lo)
		if err := img.writeVirtualRange(vc, lo, zero); err != nil {
			return err
		}
	}
	return img.blk.Sync()
}

// markWholeZero 给整个虚拟簇打零标记，回收它原来引用的数据簇。
func (img *Image) markWholeZero(vc int64, entry l2EntryView) error {
	l2, entryIdx, err := img.ensureL2(vc)
	if err != nil {
		return err
	}
	old := entry.target
	if err := img.writeL2Entry(l2, entryIdx, entryZero); err != nil {
		return err
	}
	if err := img.blk.Sync(); err != nil {
		return err
	}
	if old != 0 {
		// 标记已发布后再降引用；崩溃窗口内旧簇最多变成泄漏，由检查工具回收。
		if err := img.dropRef(old); err != nil {
			return err
		}
	}
	return nil
}

type l2EntryView struct {
	target int64
	isZero bool
}

// currentL2Entry 返回某虚拟簇当前 L2 项的解析视图。
func (img *Image) currentL2Entry(vc int64) (l2EntryView, error) {
	l2c := img.l1[img.l2Group(vc)]
	if l2c == 0 {
		return l2EntryView{}, nil
	}
	raw, err := img.readL2Entry(l2c, img.l2EntryIndex(vc))
	if err != nil {
		return l2EntryView{}, err
	}
	target, isZero, err := img.validateMapEntry(raw, "L2")
	if err != nil {
		return l2EntryView{}, err
	}
	return l2EntryView{target: target, isZero: isZero}, nil
}

func mergeInto(cluster []byte, inOff int64, data []byte) []byte {
	copy(cluster[inOff:], data)
	return cluster
}
