package pla

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

// TestErrorBound 验证任意分布下每段预测误差都不超过 eps（含浮点容差）。
func TestErrorBound(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	cases := []struct {
		name string
		gen  func(n int) []int64
	}{
		{"linear", func(n int) []int64 {
			k := make([]int64, n)
			for i := range k {
				k[i] = int64(i * 3)
			}
			return k
		}},
		{"uniform", func(n int) []int64 {
			k := make([]int64, n)
			for i := range k {
				k[i] = rng.Int63n(1_000_000)
			}
			sort.Slice(k, func(i, j int) bool { return k[i] < k[j] })
			return k
		}},
		{"duplicates", func(n int) []int64 {
			k := make([]int64, n)
			for i := range k {
				k[i] = rng.Int63n(50) // 键域极小，重复极多
			}
			sort.Slice(k, func(i, j int) bool { return k[i] < k[j] })
			return k
		}},
		{"bursty", func(n int) []int64 {
			k := make([]int64, n)
			for i := range k {
				if i%7 == 0 {
					k[i] = 1000 + rng.Int63n(5) // 密集突发
				} else {
					k[i] = rng.Int63n(1_000_000)
				}
			}
			sort.Slice(k, func(i, j int) bool { return k[i] < k[j] })
			return k
		}},
	}
	for _, eps := range []int{0, 1, 4, 16, 64} {
		for _, c := range cases {
			keys := c.gen(5000)
			segs := Fit(keys, eps)
			if len(segs) == 0 {
				t.Fatalf("%s eps=%d: 无段输出", c.name, eps)
			}
			// 段必须有序且完整覆盖。
			if segs[0].StartPos != 0 || segs[len(segs)-1].EndPos != int64(len(keys))-1 {
				t.Fatalf("%s eps=%d: 段未覆盖全部数据", c.name, eps)
			}
			for si, s := range segs {
				if si > 0 && s.First < segs[si-1].Last {
					t.Fatalf("%s eps=%d: 段乱序", c.name, eps)
				}
				for j := s.StartPos; j <= s.EndPos; j++ {
					if eps == 0 {
						// eps=0：精确定点模型必须零误差。
						if !s.Exact {
							t.Fatalf("%s eps=0: 段 %d 缺少精确模型", c.name, si)
						}
						p := s.PredictFloor(keys[j])
						if p != j {
							t.Fatalf("%s eps=0: 段 %d 位置 %d 精确预测=%d 不匹配",
								c.name, si, j, p)
						}
						continue
					}
					err := math.Abs(s.Predict(keys[j]) - float64(j))
					if err > float64(eps)+1e-6 {
						t.Fatalf("%s eps=%d: 段 %d 位置 %d 误差 %f 超限",
							c.name, eps, si, j, err)
					}
				}
			}
		}
	}
}

// TestSingleSegmentLinear 完美线性数据应被压成 1 段（eps 足够时）。
func TestSingleSegmentLinear(t *testing.T) {
	keys := make([]int64, 10000)
	for i := range keys {
		keys[i] = int64(i)*7 + 3
	}
	for _, eps := range []int{0, 1} {
		segs := Fit(keys, eps)
		if len(segs) != 1 {
			t.Fatalf("eps=%d 完美线性数据得到 %d 段，期望 1", eps, len(segs))
		}
	}
}

// TestMonotoneSegments 增大 eps 时段数单调不增。
func TestMonotoneSegments(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	keys := make([]int64, 20000)
	for i := range keys {
		keys[i] = rng.Int63n(1_000_000)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	prev := math.MaxInt
	for _, eps := range []int{1, 2, 8, 32, 128, 512} {
		n := len(Fit(keys, eps))
		if n > prev {
			t.Fatalf("eps=%d 段数 %d 多于上一档 %d", eps, n, prev)
		}
		prev = n
	}
}

func TestFindSegment(t *testing.T) {
	segs := []Segment{{First: 0}, {First: 10}, {First: 20}}
	if got := FindSegment(segs, 15); got != 1 {
		t.Fatalf("got %d want 1", got)
	}
	if got := FindSegment(segs, 0); got != 0 {
		t.Fatalf("got %d want 0", got)
	}
	if got := FindSegment(segs, -5); got != -1 {
		t.Fatalf("got %d want -1", got)
	}
	if got := FindSegment(segs, 100); got != 2 {
		t.Fatalf("got %d want 2", got)
	}
}
