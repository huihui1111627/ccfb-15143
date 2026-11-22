package vdisk

import (
	"bytes"
	"testing"
)

func TestBasicWriteRead(t *testing.T) {
	cs := 512
	img, err := Create(NewMemBlock(0), Options{ClusterShift: 9, VirtualSize: int64(8 * cs)})
	if err != nil {
		t.Fatal(err)
	}
	// 写第 5 个簇（vc=5），验证组语义（E=64，全在组 0）。
	data := bytes.Repeat([]byte{0xAB}, cs)
	if _, err := img.WriteAt(data, 5*int64(cs)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, cs)
	if _, err := img.ReadAt(got, 5*int64(cs)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("vc5 读回不一致")
	}
	// 其它簇应为零。
	z := make([]byte, cs)
	if _, err := img.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, z) {
		t.Fatalf("vc0 应为零， got %x...", got[:4])
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCrossClusterUnaligned(t *testing.T) {
	cs := 512
	img, err := Create(NewMemBlock(0), Options{ClusterShift: 9, VirtualSize: int64(8 * cs)})
	if err != nil {
		t.Fatal(err)
	}
	// 不对齐、跨簇写：从簇 1 偏移 500 写 100 字节（跨到簇 2）。
	off := int64(cs + 500)
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i + 1)
	}
	if _, err := img.WriteAt(data, off); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 100)
	if _, err := img.ReadAt(got, off); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("跨簇不对齐写读回不一致")
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestZeroMark(t *testing.T) {
	cs := 512
	img, err := Create(NewMemBlock(0), Options{ClusterShift: 9, VirtualSize: int64(4 * cs)})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0x77}, cs)
	if _, err := img.WriteAt(data, 2*int64(cs)); err != nil {
		t.Fatal(err)
	}
	if err := img.ZeroAt(2*int64(cs), int64(cs)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, cs)
	if _, err := img.ReadAt(got, 2*int64(cs)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, cs)) {
		t.Fatal("零标记后应读全零")
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
}
