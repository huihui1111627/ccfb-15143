package vdisk

import (
	"errors"
	"testing"
)

// 直接篡改一个已用 L2 表项，制造未按簇对齐的偏移，打开必须报损坏。
func TestMisalignedEntryRejected(t *testing.T) {
	cs := 512
	blk := NewMemBlock(0)
	img, err := Create(blk, Options{ClusterShift: 9, VirtualSize: 8 * int64(cs)})
	if err != nil {
		t.Fatal(err)
	}
	// 写一个簇，产生有效 L2 表项。
	data := make([]byte, cs)
	for i := range data {
		data[i] = 0x77
	}
	if _, err := img.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}

	// 找到 L2 表簇（L1 组 0 指向它），把第一项的偏移低位改成非对齐。
	// 先读 L1 根簇（簇 l1Start）第一项拿 L2 物理簇号。
	l1Raw := make([]byte, cs)
	if _, err := blk.ReadAt(l1Raw, img.l1Start*int64(cs)); err != nil {
		t.Fatal(err)
	}
	l2Off := be64(l1Raw[:8]) & entryOffsetMask
	l2Cluster := int64(l2Off) / int64(cs)

	l2Raw := make([]byte, cs)
	if _, err := blk.ReadAt(l2Raw, l2Cluster*int64(cs)); err != nil {
		t.Fatal(err)
	}
	// 把第一项改成有效位 + 一个未对齐偏移（加 7）。
	putBE64(l2Raw[:8], entryValid|7)
	if _, err := blk.WriteAt(l2Raw, l2Cluster*int64(cs)); err != nil {
		t.Fatal(err)
	}
	// 打开阶段的 L2 角色扫描会逐项校验，应报损坏。
	if _, err := Open(blk, true, nil); !errors.Is(err, ErrCorruption) {
		t.Fatalf("未对齐表项应报 ErrCorruption，got %v", err)
	}
}

// 篡改引用计数项的保留位（高 8 位），打开必须报损坏。
func TestRefReservedBitRejected(t *testing.T) {
	cs := 512
	blk := NewMemBlock(0)
	img, err := Create(blk, Options{ClusterShift: 9, VirtualSize: 8 * int64(cs)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := img.WriteAt(make([]byte, cs), 0); err != nil {
		t.Fatal(err)
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	// refL2 表簇起点：组 0 的 refL2 = refL2Start。改写它的第一个计数项保留位。
	refL2 := img.refL2Start
	buf := make([]byte, cs)
	if _, err := blk.ReadAt(buf, refL2*int64(cs)); err != nil {
		t.Fatal(err)
	}
	putBE64(buf[:8], uint64(1)<<60) // 落在保留位区间
	if _, err := blk.WriteAt(buf, refL2*int64(cs)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(blk, true, nil); !errors.Is(err, ErrCorruption) {
		t.Fatalf("引用计数保留位非零应报 ErrCorruption，got %v", err)
	}
}

// 只读镜像写入应返回 ErrReadOnly。
func TestReadOnlyRejectsWrite(t *testing.T) {
	cs := 512
	blk := NewMemBlock(0)
	img, err := Create(blk, Options{ClusterShift: 9, VirtualSize: 4 * int64(cs)})
	if err != nil {
		t.Fatal(err)
	}
	img.Close()
	ro, err := Open(blk, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.WriteAt([]byte{1}, 0); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("只读写应报 ErrReadOnly，got %v", err)
	}
	if err := ro.ZeroAt(0, 1); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("只读 ZeroAt 应报 ErrReadOnly，got %v", err)
	}
}
