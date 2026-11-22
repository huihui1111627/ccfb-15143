package vdisk

import (
	"bytes"
	"testing"
)

// 反复 写 -> 关闭 -> 重开 -> 整盘比对，模拟压力测试的重开节奏。
func TestReopenLoop(t *testing.T) {
	cs := 512
	nClusters := int64(8)
	blk := NewMemBlock(0)
	brute := make([]byte, nClusters*int64(cs))

	img, err := Create(blk, Options{ClusterShift: 9, VirtualSize: nClusters * int64(cs)})
	if err != nil {
		t.Fatal(err)
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}

	rng := newRng(1)
	for iter := 0; iter < 50; iter++ {
		img, err = Open(blk, false, nil)
		if err != nil {
			t.Fatalf("iter %d open: %v", iter, err)
		}
		off := rng.intn(int(nClusters) * cs)
		length := 1 + rng.intn(int(nClusters)*cs-off)
		data := make([]byte, length)
		rng.fill(data)
		if _, err := img.WriteAt(data, int64(off)); err != nil {
			t.Fatalf("iter %d write: %v", iter, err)
		}
		copy(brute[off:off+length], data)
		if err := img.Close(); err != nil {
			t.Fatal(err)
		}

		// 重开只读比对。
		ro, err := Open(blk, true, nil)
		if err != nil {
			t.Fatalf("iter %d reopen: %v", iter, err)
		}
		got := make([]byte, len(brute))
		if _, err := ro.ReadAt(got, 0); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, brute) {
			for i := range brute {
				if got[i] != brute[i] {
					t.Fatalf("iter %d 字节 %d 不符 got %x want %x", iter, i, got[i], brute[i])
				}
			}
		}
		if err := ro.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
