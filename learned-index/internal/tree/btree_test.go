package tree

import (
	"math/rand"
	"sort"
	"testing"
)

func TestBTreePointAndRange(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	var keys []int64
	bt := New()
	const n = 50000
	domain := int64(100000)
	for i := 0; i < n; i++ {
		k := rng.Int63n(domain)
		keys = append(keys, k)
		bt.Insert(k)
	}
	sorted := append([]int64(nil), keys...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	// 点查：全部生成过的键必须命中；随机键与二分结果一致。
	for _, k := range keys {
		if !bt.PointLookup(k).Found {
			t.Fatalf("已插入的键 %d 未命中", k)
		}
	}
	for i := 0; i < 10000; i++ {
		k := rng.Int63n(domain)
		want := sort.Search(len(sorted), func(j int) bool { return sorted[j] >= k }) < len(sorted) &&
			sorted[sort.Search(len(sorted), func(j int) bool { return sorted[j] >= k })] == k
		if bt.PointLookup(k).Found != want {
			t.Fatalf("键 %d 点查不一致", k)
		}
	}
	// 范围查
	for i := 0; i < 2000; i++ {
		lo := rng.Int63n(domain)
		hi := lo + rng.Int63n(1000)
		l := sort.Search(len(sorted), func(j int) bool { return sorted[j] >= lo })
		r := sort.Search(len(sorted), func(j int) bool { return sorted[j] >= hi })
		want := r - l
		if got := bt.RangeLookup(lo, hi).Count; got != want {
			t.Fatalf("range[%d,%d) got %d want %d", lo, hi, got, want)
		}
	}
	if bt.Size() != n {
		t.Fatalf("size %d want %d", bt.Size(), n)
	}
}
