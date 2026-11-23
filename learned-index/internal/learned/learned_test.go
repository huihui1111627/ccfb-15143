package learned

import (
	"math/rand"
	"sort"
	"testing"

	"learnedindex/internal/data"
)

// oraclePoint / oracleRange 用标准二分作为正确性基准。
func oraclePoint(keys []int64, key int64) bool {
	i := sort.Search(len(keys), func(i int) bool { return keys[i] >= key })
	return i < len(keys) && keys[i] == key
}

func oracleRange(keys []int64, lo, hi int64) int {
	l := sort.Search(len(keys), func(i int) bool { return keys[i] >= lo })
	r := sort.Search(len(keys), func(i int) bool { return keys[i] >= hi })
	return r - l
}

// TestStaticCorrectness 四种分布 × 多档 eps：全部点查/范围查与二分一致，零回退。
func TestStaticCorrectness(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for _, p := range data.Profiles() {
		keys := data.Generate(p, 20000, 99)
		domain := int64(1 << 40)
		for _, eps := range []int{1, 8, 32, 128} {
			idx := BuildStatic(keys, eps)
			for q := 0; q < 5000; q++ {
				var key int64
				if q%2 == 0 {
					key = keys[rng.Intn(len(keys))] // 命中（含重复键）
				} else {
					key = rng.Int63n(domain) // 多为不存在的键
				}
				r := idx.PointLookup(key)
				if r.Found != oraclePoint(keys, key) {
					t.Fatalf("%s eps=%d key=%d: 点查 found=%v 与二分不一致", p, eps, key, r.Found)
				}
				if r.FellBack {
					t.Fatalf("%s eps=%d key=%d: 触发了不应发生的全量回退", p, eps, key)
				}
			}
			// 范围查
			for q := 0; q < 1000; q++ {
				lo := rng.Int63n(domain)
				hi := lo + rng.Int63n(domain/50+1)
				st, en, _ := idx.RangeLookup(lo, hi)
				if en-st != oracleRange(keys, lo, hi) {
					t.Fatalf("%s eps=%d range[%d,%d): got %d want %d",
						p, eps, lo, hi, en-st, oracleRange(keys, lo, hi))
				}
			}
		}
	}
}

// TestWindowWithinBound 叶子模型预测窗口有确定上界。
// 模型二分窗口至多 2eps+4（含浮点安全余量）；examined 还可能包含重复键
// 回扫到首个副本的成本，因此这里用宽松上界，严格的误差保证由 TestStaticCorrectness 覆盖。
func TestWindowWithinBound(t *testing.T) {
	keys := data.Generate(data.Timestamps, 50000, 3)
	for _, eps := range []int{4, 16, 64} {
		idx := BuildStatic(keys, eps)
		rng := rand.New(rand.NewSource(5))
		for q := 0; q < 3000; q++ {
			key := keys[rng.Intn(len(keys))]
			r := idx.PointLookup(key)
			if r.Examined > 4*eps+16 {
				t.Fatalf("eps=%d 窗口(含重复回扫) %d 超出预期", eps, r.Examined)
			}
		}
	}
}

// TestDynamicInserts 插入后点查与最终快照的二分结果完全一致。
func TestDynamicInserts(t *testing.T) {
	for _, p := range data.Profiles() {
		base := data.Generate(p, 30000, 21)
		ins := data.Generate(p, 10000, 22)
		dyn := NewDynamic(base, 16, 32, 0.15)
		for _, k := range ins {
			dyn.Insert(k)
		}
		snap := dyn.SortedSnapshot()
		if int64(len(snap)) != dyn.Count() || len(snap) != len(base)+len(ins) {
			t.Fatalf("%s: 数量不符 %d vs %d", p, len(snap), len(base)+len(ins))
		}
		rng := rand.New(rand.NewSource(33))
		for q := 0; q < 4000; q++ {
			var key int64
			if q%2 == 0 {
				key = snap[rng.Intn(len(snap))]
			} else {
				key = rng.Int63n(1 << 40)
			}
			if dyn.PointLookup(key).Found != oraclePoint(snap, key) {
				t.Fatalf("%s: 动态点查 key=%d 与快照二分不一致", p, key)
			}
		}
		if dyn.Refits == 0 {
			t.Fatalf("%s: 1 万次插入未触发任何重拟合，缓冲逻辑可能未生效", p)
		}
	}
}

// TestDynamicHeavyDuplicates 大量重复插入不应破坏正确性。
func TestDynamicHeavyDuplicates(t *testing.T) {
	base := []int64{1, 2, 3, 4, 5}
	dyn := NewDynamic(base, 2, 8, 0.2)
	for i := 0; i < 2000; i++ {
		dyn.Insert(3) // 同一个键反复插入
	}
	snap := dyn.SortedSnapshot()
	if len(snap) != 2005 {
		t.Fatalf("got %d want 2005", len(snap))
	}
	if !dyn.PointLookup(3).Found || dyn.PointLookup(99).Found {
		t.Fatal("重复键点查错误")
	}
}
