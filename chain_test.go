package vdisk

import (
	"bytes"
	"testing"
)

// 两层链：底层写一些内容，上层做 COW 与遮蔽，验证上层读取沿链下沉。
func TestTwoLayerChain(t *testing.T) {
	cs := 512
	baseBlk := NewMemBlock(0)
	base, err := Create(baseBlk, Options{ClusterShift: 9, VirtualSize: int64(8 * cs)})
	if err != nil {
		t.Fatal(err)
	}
	// 底层 vc3 写满 0x11。
	if _, err := base.WriteAt(bytes.Repeat([]byte{0x11}, cs), 3*int64(cs)); err != nil {
		t.Fatal(err)
	}
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}

	// 上层以底包路径 "base" 打开。
	topBlk := NewMemBlock(0)
	top, err := Create(topBlk, Options{ClusterShift: 9, VirtualSize: int64(8 * cs), BasePath: "base"})
	if err != nil {
		t.Fatal(err)
	}
	loader := func(_ string) (*Image, error) {
		return Open(NewMemBlock(0), true, nil) // 占位，下面替换
	}
	_ = loader
	if err := top.Close(); err != nil {
		t.Fatal(err)
	}

	// 用真实底包块重新打开上层。
	realLoader := func(_ string) (*Image, error) {
		return Open(baseBlk, true, nil)
	}
	top2, err := Open(topBlk, false, realLoader)
	if err != nil {
		t.Fatal(err)
	}
	// vc3 应从底包读到 0x11。
	got := make([]byte, cs)
	if _, err := top2.ReadAt(got, 3*int64(cs)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{0x11}, cs)) {
		t.Fatalf("上层未下沉到底包， got %x...", got[:4])
	}
	// 上层对 vc3 部分写（COW），应保留底包其余字节。
	part := []byte{0x22, 0x33, 0x44}
	if _, err := top2.WriteAt(part, 3*int64(cs)+10); err != nil {
		t.Fatal(err)
	}
	got2 := make([]byte, cs)
	if _, err := top2.ReadAt(got2, 3*int64(cs)); err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0x11}, cs)
	copy(want[10:13], part)
	if !bytes.Equal(got2, want) {
		t.Fatalf("COW 部分写后内容不符， got[10:13]=%x", got2[10:13])
	}
	if err := top2.Close(); err != nil {
		t.Fatal(err)
	}
}
