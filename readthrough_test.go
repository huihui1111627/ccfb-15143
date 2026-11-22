package vdisk

import (
	"bytes"
	"testing"
)

// 最小三层读穿透：层1写一个簇，层2未映射，应透过层2读到层1的内容。
func TestThreeLayerReadThrough(t *testing.T) {
	cs := 512
	n := int64(8)
	vsize := n * int64(cs)
	b0 := NewMemBlock(0)
	b1 := NewMemBlock(0)
	b2 := NewMemBlock(0)
	for li, b := range []Block{b0, b1, b2} {
		base := ""
		if li > 0 {
			base = "x"
		}
		img, err := Create(b, Options{ClusterShift: 9, VirtualSize: vsize, BasePath: base})
		if err != nil {
			t.Fatal(err)
		}
		img.Close()
	}
	var loader func(int) BaseLoader
	loader = func(li int) BaseLoader {
		if li == 0 {
			return nil
		}
		return func(_ string) (*Image, error) { return Open(blocks(li, b0, b1, b2), true, loader(li-1)) }
	}

	// 层 1 写 vc5。
	i1, err := Open(b1, false, loader(1))
	if err != nil {
		t.Fatal(err)
	}
	d := bytes.Repeat([]byte{0x42}, cs)
	if _, err := i1.WriteAt(d, 5*int64(cs)); err != nil {
		t.Fatal(err)
	}
	i1.Close()

	// 层 2 只读打开，vc5 应透传层 1。
	i2, err := Open(b2, true, loader(2))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, cs)
	if _, err := i2.ReadAt(got, 5*int64(cs)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, d) {
		t.Fatalf("层2 未透传层1， got[:4]=%x", got[:4])
	}
	i2.Close()
}

func blocks(li int, all ...Block) Block { return all[li-1] }
