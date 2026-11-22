package vdisk

import (
	"bytes"
	"testing"
)

// 同一 L2 组内：先给 vc2 打零标记，再对 vc5 做部分写，
// 验证两个组内表项互不干扰。簇 512B，E=64，vc2/vc5 同属组 0。
func TestL2GroupEntryIsolation(t *testing.T) {
	cs := 512
	img, err := Create(NewMemBlock(0), Options{ClusterShift: 9, VirtualSize: 8 * int64(cs)})
	if err != nil {
		t.Fatal(err)
	}
	// 先给 vc2 整簇标零。
	if err := img.ZeroAt(2*int64(cs), int64(cs)); err != nil {
		t.Fatal(err)
	}
	// 再对 vc5 做不对齐部分写。
	part := bytes.Repeat([]byte{0x5A}, 100)
	if _, err := img.WriteAt(part, 5*int64(cs)+30); err != nil {
		t.Fatal(err)
	}
	// vc2 仍应为零。
	got := make([]byte, cs)
	if _, err := img.ReadAt(got, 2*int64(cs)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, cs)) {
		t.Fatal("vc2 零标记被 vc5 的写破坏")
	}
	// vc5 写入区域正确，其余为零。
	if _, err := img.ReadAt(got, 5*int64(cs)); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, cs)
	copy(want[30:130], part)
	if !bytes.Equal(got, want) {
		t.Fatalf("vc5 内容不符 got[30:34]=%x", got[30:34])
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
}

// 组内多个簇依次部分写（走 COW 新簇分配），验证 L2 槽位不串。
func TestL2GroupMultiPartialWrite(t *testing.T) {
	cs := 512
	img, err := Create(NewMemBlock(0), Options{ClusterShift: 9, VirtualSize: 8 * int64(cs)})
	if err != nil {
		t.Fatal(err)
	}
	for vc := int64(0); vc < 6; vc++ {
		part := []byte{byte(vc + 1), byte(vc + 2), byte(vc + 3)}
		if _, err := img.WriteAt(part, vc*int64(cs)+10); err != nil {
			t.Fatal(err)
		}
	}
	got := make([]byte, cs)
	for vc := int64(0); vc < 6; vc++ {
		if _, err := img.ReadAt(got, vc*int64(cs)); err != nil {
			t.Fatal(err)
		}
		if got[10] != byte(vc+1) || got[11] != byte(vc+2) || got[12] != byte(vc+3) {
			t.Fatalf("vc%d 表项串位 got[10:13]=%x", vc, got[10:13])
		}
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
}
