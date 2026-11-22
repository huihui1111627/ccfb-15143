package vdisk

import (
	"bytes"
	"testing"
)

// 三层链，在不同层交替写并反复重开。
// 期望模型与压力测试一致：簇级 COW 遮蔽。每层维护 brute 内容与簇级 touched，
// 写前对新拥有的簇从"下层视角"快照整簇；校验时取每个簇的最高拥有层内容。
func TestThreeLayerAlternating(t *testing.T) {
	cs := 512
	n := int64(16)
	vsize := n * int64(cs)

	blocks := []Block{NewMemBlock(0), NewMemBlock(0), NewMemBlock(0)}
	for li := 0; li < 3; li++ {
		base := ""
		if li > 0 {
			base = "x"
		}
		img, err := Create(blocks[li], Options{ClusterShift: 9, VirtualSize: vsize, BasePath: base})
		if err != nil {
			t.Fatal(err)
		}
		img.Close()
	}
	var makeLoader func(li int) BaseLoader
	makeLoader = func(li int) BaseLoader {
		if li == 0 {
			return nil
		}
		return func(_ string) (*Image, error) {
			return Open(blocks[li-1], true, makeLoader(li-1))
		}
	}

	rng := newRng(20261123)
	brute := make([][]byte, 3)
	clusterTouched := make([][]bool, 3) // 簇级拥有标记
	for li := range brute {
		brute[li] = make([]byte, vsize)
		clusterTouched[li] = make([]bool, n)
	}
	// effectiveView：站在 layer top 往下看，每簇取最高拥有层。
	effectiveView := func(top int64) []byte {
		out := make([]byte, vsize)
		for vc := int64(0); vc < n; vc++ {
			owner := -1
			for k := int64(0); k <= top; k++ {
				if clusterTouched[k][vc] {
					owner = int(k)
				}
			}
			if owner >= 0 {
				copy(out[vc*int64(cs):(vc+1)*int64(cs)], brute[owner][vc*int64(cs):(vc+1)*int64(cs)])
			}
		}
		return out
	}

	for iter := 0; iter < 100; iter++ {
		li := int64(rng.intn(3))
		off := int64(rng.intn(int(vsize)))
		length := int64(1 + rng.intn(int(vsize)-int(off)))
		data := make([]byte, length)
		rng.fill(data)

		img, err := Open(blocks[li], false, makeLoader(int(li)))
		if err != nil {
			t.Fatalf("iter %d open: %v", iter, err)
		}
		if _, err := img.WriteAt(data, off); err != nil {
			t.Fatalf("iter %d write: %v", iter, err)
		}
		img.Close()

		// 更新期望：对涉及簇，若本层首次拥有则从下层视角快照整簇。
		vcStart := off / int64(cs)
		vcEnd := (off + length - 1) / int64(cs)
		for vc := vcStart; vc <= vcEnd; vc++ {
			if !clusterTouched[li][vc] {
				var below []byte
				if li > 0 {
					below = effectiveView(li - 1)
				} else {
					below = make([]byte, vsize)
				}
				copy(brute[li][vc*int64(cs):(vc+1)*int64(cs)], below[vc*int64(cs):(vc+1)*int64(cs)])
				clusterTouched[li][vc] = true
			}
		}
		pos := int64(0)
		for pos < length {
			abs := off + pos
			vc := abs / int64(cs)
			in := abs % int64(cs)
			take := int64(cs) - in
			if take > length-pos {
				take = length - pos
			}
			copy(brute[li][in+vc*int64(cs):], data[pos:pos+take])
			pos += take
		}

		// 逐层只读校验。
		for chk := int64(0); chk < 3; chk++ {
			ro, err := Open(blocks[chk], true, makeLoader(int(chk)))
			if err != nil {
				t.Fatalf("iter %d reopen: %v", iter, err)
			}
			got := make([]byte, vsize)
			if _, err := ro.ReadAt(got, 0); err != nil {
				t.Fatal(err)
			}
			want := effectiveView(chk)
			if !bytes.Equal(got, want) {
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("iter %d 写层%d 校验层%d 字节%d(vc%d) got=%x want=%x",
							iter, li, chk, i, i/cs, got[i], want[i])
					}
				}
			}
			ro.Close()
		}
	}
}
