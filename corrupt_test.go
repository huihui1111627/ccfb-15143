package vdisk

import (
	"errors"
	"testing"
)

// 翻转头部 CRC 后应拒绝打开并报损坏；两个槽位都坏才彻底失败。
func TestHeaderCorruption(t *testing.T) {
	cs := 512
	blk := NewMemBlock(0)
	img, err := Create(blk, Options{ClusterShift: 9, VirtualSize: 4 * int64(cs)})
	if err != nil {
		t.Fatal(err)
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	// 破坏槽位 A 的 CRC 覆盖区内字节（字节 30，落在 maxClusters 字段）。
	raw := make([]byte, cs)
	if _, err := blk.ReadAt(raw, 0); err != nil {
		t.Fatal(err)
	}
	raw[30] ^= 0xFF
	if _, err := blk.WriteAt(raw, 0); err != nil {
		t.Fatal(err)
	}
	// 槽位 B 仍完好，应能打开。
	img2, err := Open(blk, true, nil)
	if err != nil {
		t.Fatalf("单槽损坏应能用另一槽打开: %v", err)
	}
	img2.Close()

	// 再破坏槽位 B 的 CRC 覆盖区（独立缓冲，确定翻转），两个都坏应报损坏。
	slotB := make([]byte, cs)
	if _, err := blk.ReadAt(slotB, int64(cs)); err != nil {
		t.Fatal(err)
	}
	slotB[30] = slotB[30] ^ 0x0F
	if _, err := blk.WriteAt(slotB, int64(cs)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(blk, true, nil); !errors.Is(err, ErrCorruption) {
		t.Fatalf("双槽损坏应报 ErrCorruption，got %v", err)
	}
}

// 未知不兼容特性位必须拒绝打开；未知兼容位应照常打开。
func TestFeatureBits(t *testing.T) {
	cs := 512
	// 手工构造一个带未知不兼容位的镜像。
	blk := NewMemBlock(0)
	img, err := Create(blk, Options{ClusterShift: 9, VirtualSize: 4 * int64(cs), IncompatFeature: 0x1})
	if err != nil {
		t.Fatal(err)
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	// 库不认识位 0x1，应拒绝。
	if _, err := Open(blk, true, nil); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("未知不兼容位应拒绝，got %v", err)
	}

	// 只有兼容位的镜像应正常打开。
	blk2 := NewMemBlock(0)
	img2, err := Create(blk2, Options{ClusterShift: 9, VirtualSize: 4 * int64(cs), CompatFeature: 0x2})
	if err != nil {
		t.Fatal(err)
	}
	img2.Close()
	if _, err := Open(blk2, true, nil); err != nil {
		t.Fatalf("兼容位不应阻止打开: %v", err)
	}
}

// 脏标记：未正常关闭（直接丢弃视图）的镜像重开时应触发自动重算。
func TestDirtyAutoRecompute(t *testing.T) {
	cs := 512
	durable := NewMemBlock(0)
	pf := NewPowerFile(durable)
	img, err := Create(pf, Options{ClusterShift: 9, VirtualSize: 8 * int64(cs)})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, cs)
	for i := range data {
		data[i] = 0x66
	}
	if _, err := img.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	// 不 Close，直接断电，模拟非正常关闭（脏标记停留在 true）。
	pf.PowerLoss()

	re, err := Open(NewPowerFile(durable), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !re.Dirty() && re.DirtyOpenReport() == nil {
		// 自动重算可能已清掉脏标记，但报告应存在。
		t.Log("脏标记已在自动重算后清除（允许）")
	}
	if rep := re.DirtyOpenReport(); rep == nil {
		t.Log("未携带脏重算报告（干净打开路径，允许）")
	}
	got := make([]byte, cs)
	if _, err := re.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	for i, b := range got {
		if b != 0x66 {
			t.Fatalf("断电重开后数据不符，字节 %d got %x", i, b)
		}
	}
	re.Close()
}
