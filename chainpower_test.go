package vdisk

import (
	"bytes"
	"testing"
)

// 两层链 + 各自断电重开，验证上层 COW 与底包下沉在断电后仍正确。
func TestTwoLayerPowerCycle(t *testing.T) {
	cs := 512
	nClusters := int64(8)
	vsize := nClusters * int64(cs)

	baseDurable := NewMemBlock(0)
	topDurable := NewMemBlock(0)

	// 建底层并写一些数据。
	basePF := NewPowerFile(baseDurable)
	base, err := Create(basePF, Options{ClusterShift: 9, VirtualSize: vsize})
	if err != nil {
		t.Fatal(err)
	}
	baseData := bytes.Repeat([]byte{0x33}, cs)
	if _, err := base.WriteAt(baseData, 2*int64(cs)); err != nil {
		t.Fatal(err)
	}
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}

	loader := func(_ string) (*Image, error) {
		return Open(NewPowerFile(baseDurable), true, nil)
	}

	// 建上层。
	topPF := NewPowerFile(topDurable)
	top, err := Create(topPF, Options{ClusterShift: 9, VirtualSize: vsize, BasePath: "base"})
	if err != nil {
		t.Fatal(err)
	}
	if err := top.Close(); err != nil {
		t.Fatal(err)
	}

	// 上层做若干 COW 写，每轮断电重开。
	brute := make([]byte, vsize)
	copy(brute[2*int64(cs):3*int64(cs)], baseData) // 上层视角初始：vc2 来自底包
	rng := newRng(11)
	for iter := 0; iter < 30; iter++ {
		pf := NewPowerFile(topDurable)
		img, err := Open(pf, false, loader)
		if err != nil {
			t.Fatalf("iter %d open: %v", iter, err)
		}
		off := rng.intn(int(vsize))
		length := 1 + rng.intn(int(vsize)-off)
		data := make([]byte, length)
		rng.fill(data)
		if _, err := img.WriteAt(data, int64(off)); err != nil {
			t.Fatalf("iter %d write: %v", iter, err)
		}
		copy(brute[off:off+length], data)
		pf.PowerLoss()

		ro, err := Open(NewPowerFile(topDurable), true, loader)
		if err != nil {
			t.Fatalf("iter %d reopen: %v", iter, err)
		}
		got := make([]byte, vsize)
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
