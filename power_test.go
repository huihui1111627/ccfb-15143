package vdisk

import (
	"bytes"
	"testing"
)

// 用 PowerFile 模拟：写 -> Sync(Close) -> 断电 -> 用残留块重开 -> 比对。
func TestPowerCycle(t *testing.T) {
	cs := 512
	nClusters := int64(8)
	durable := NewMemBlock(0)
	brute := make([]byte, nClusters*int64(cs))

	pf := NewPowerFile(durable)
	img, err := Create(pf, Options{ClusterShift: 9, VirtualSize: nClusters * int64(cs)})
	if err != nil {
		t.Fatal(err)
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}

	rng := newRng(7)
	for iter := 0; iter < 40; iter++ {
		// 每次都从磁盘本体新建一个 PowerFile 视图。
		pf := NewPowerFile(durable)
		img, err = Open(pf, false, nil)
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
		// 不调 Close，直接断电：写路径结尾已 Sync。
		pf.PowerLoss()

		// 用残留磁盘本体重开。
		ro, err := Open(NewPowerFile(durable), true, nil)
		if err != nil {
			t.Fatalf("iter %d reopen after loss: %v", iter, err)
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
