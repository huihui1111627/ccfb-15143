package vdisk

import (
	"bytes"
	"testing"
)

// 带底包：底层多个簇有内容，上层做零标记 + 跨簇部分写，
// 验证上层未触碰的簇仍能正确透传底包内容。
func TestBaseCOWIsolation(t *testing.T) {
	cs := 512
	n := int64(24)
	vsize := n * int64(cs)
	baseBlk := NewMemBlock(0)
	base, err := Create(baseBlk, Options{ClusterShift: 9, VirtualSize: vsize})
	if err != nil {
		t.Fatal(err)
	}
	// 底层给 vc0..vc23 每簇写不同的可辨识内容。
	for vc := int64(0); vc < n; vc++ {
		d := bytes.Repeat([]byte{byte(vc)}, cs)
		if _, err := base.WriteAt(d, vc*int64(cs)); err != nil {
			t.Fatal(err)
		}
	}
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}
	loader := func(_ string) (*Image, error) { return Open(baseBlk, true, nil) }

	topBlk := NewMemBlock(0)
	top, err := Create(topBlk, Options{ClusterShift: 9, VirtualSize: vsize, BasePath: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := top.Close(); err != nil {
		t.Fatal(err)
	}
	top, err = Open(topBlk, false, loader)
	if err != nil {
		t.Fatal(err)
	}

	// 上层给 vc11 打零标记。
	if err := top.ZeroAt(11*int64(cs), int64(cs)); err != nil {
		t.Fatal(err)
	}
	// 对 vc12 做跨簇部分写（模拟 op9 的 target!=0 读改写）。
	part := bytes.Repeat([]byte{0x56}, 200)
	if _, err := top.WriteAt(part, 12*int64(cs)+300); err != nil {
		t.Fatal(err)
	}

	// 全盘比对：未触碰簇应透传底包（vc 内容=byte(vc)）。
	got := make([]byte, cs)
	for vc := int64(0); vc < n; vc++ {
		if _, err := top.ReadAt(got, vc*int64(cs)); err != nil {
			t.Fatal(err)
		}
		switch vc {
		case 11:
			if !bytes.Equal(got, make([]byte, cs)) {
				t.Fatalf("vc11 应为零， got[:4]=%x", got[:4])
			}
		case 12:
			if !bytes.Equal(got[300:500], part) {
				t.Fatalf("vc12 写入区不符")
			}
			// 其余字节应来自底包 byte(12)
			if got[0] != 12 {
				t.Fatalf("vc12 COW 未保留底包内容 got[0]=%x", got[0])
			}
		default:
			if got[0] != byte(vc) {
				t.Fatalf("vc%d 底包透传被破坏 got[0]=%x want %x", vc, got[0], byte(vc))
			}
		}
	}
	top.Close()
}
