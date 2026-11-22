package vdisk

import (
	"bytes"
	"testing"
)

// 带底包 + 每次操作后关闭重开，复现压力测试的重开节奏下的 COW 串位。
func TestBaseCOWReopen(t *testing.T) {
	cs := 512
	n := int64(24)
	vsize := n * int64(cs)
	baseBlk := NewMemBlock(0)
	base, err := Create(baseBlk, Options{ClusterShift: 9, VirtualSize: vsize})
	if err != nil {
		t.Fatal(err)
	}
	for vc := int64(0); vc < n; vc++ {
		if _, err := base.WriteAt(bytes.Repeat([]byte{byte(vc)}, cs), vc*int64(cs)); err != nil {
			t.Fatal(err)
		}
	}
	base.Close()

	loader := func(_ string) (*Image, error) { return Open(baseBlk, true, nil) }
	topBlk := NewMemBlock(0)
	top, err := Create(topBlk, Options{ClusterShift: 9, VirtualSize: vsize, BasePath: "b"})
	if err != nil {
		t.Fatal(err)
	}
	top.Close()

	rng := newRng(99)
	brute := make([]byte, vsize)
	for vc := int64(0); vc < n; vc++ {
		for j := 0; j < cs; j++ {
			brute[vc*int64(cs)+int64(j)] = byte(vc)
		}
	}
	touched := make([]bool, vsize) // 上层触碰标记

	for iter := 0; iter < 60; iter++ {
		top, err = Open(topBlk, false, loader)
		if err != nil {
			t.Fatalf("iter %d open: %v", iter, err)
		}
		off := rng.intn(int(vsize))
		length := 1 + rng.intn(int(vsize)-off)
		data := make([]byte, length)
		rng.fill(data)
		if _, err := top.WriteAt(data, int64(off)); err != nil {
			t.Fatalf("iter %d write: %v", iter, err)
		}
		copy(brute[off:off+length], data)
		for i := off; i < off+length; i++ {
			touched[i] = true
		}
		top.Close()

		// 重开只读全盘比对。
		ro, err := Open(topBlk, true, loader)
		if err != nil {
			t.Fatalf("iter %d reopen: %v", iter, err)
		}
		got := make([]byte, vsize)
		if _, err := ro.ReadAt(got, 0); err != nil {
			t.Fatal(err)
		}
		for i := range brute {
			want := byte(0)
			if touched[i] {
				want = brute[i]
			} else {
				want = byte(i / cs) // 底包内容
			}
			if got[i] != want {
				t.Fatalf("iter %d 字节 %d (vc%d) got=%x want=%x touched=%v",
					iter, i, i/cs, got[i], want, touched[i])
			}
		}
		ro.Close()
	}
}
