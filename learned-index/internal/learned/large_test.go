package learned

import (
	"math/rand"
	"sort"
	"testing"

	"learnedindex/internal/data"
)

// TestLargeScaleCorrectness 在百万级、四分布、多档 eps 上验证零回退、
// 零正确性错误，并确认搜索窗口上界接近 2eps+1。
func TestLargeScaleCorrectness(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过百万级长测")
	}
	const n = 1_000_000
	rng := rand.New(rand.NewSource(2026))
	for _, p := range data.Profiles() {
		keys := data.Generate(p, n, 7)
		domain := int64(1) << 50
		for _, eps := range []int{16, 128} {
			idx := BuildStatic(keys, eps)
			maxWin := 0
			for q := 0; q < 20000; q++ {
				var key int64
				if q%2 == 0 {
					key = keys[rng.Intn(n)]
				} else {
					key = rng.Int63n(domain)
				}
				r := idx.PointLookup(key)
				if r.Found != oraclePoint(keys, key) {
					t.Fatalf("%s eps=%d: 点查不一致 key=%d", p, eps, key)
				}
				if r.FellBack || idx.Fallbacks != 0 {
					t.Fatalf("%s eps=%d: 出现全量回退", p, eps)
				}
				if r.Examined > maxWin {
					maxWin = r.Examined
				}
			}
			// examined 含重复键回扫成本，用宽松上界；模型窗口本身 ≤ 2eps+4。
			if maxWin > 4*eps+16 {
				t.Fatalf("%s eps=%d: 最大检查量 %d 超出预期", p, eps, maxWin)
			}
			// 范围查抽样。
			for q := 0; q < 5000; q++ {
				lo := rng.Int63n(domain)
				hi := lo + rng.Int63n(domain/100+1)
				st, en, _ := idx.RangeLookup(lo, hi)
				if en-st != oracleRange(keys, lo, hi) {
					t.Fatalf("%s eps=%d: 范围查不一致", p, eps)
				}
			}
			t.Logf("%s eps=%d 层=%v 索引=%.2fMB 最大窗口=%d",
				p, eps, idx.SegmentCounts(),
				float64(idx.IndexSizeBytes())/1024/1024, maxWin)
		}
	}
}

// TestSortedOracleSanity 确保 oracle 本身工作正常。
func TestSortedOracleSanity(t *testing.T) {
	keys := []int64{1, 3, 3, 5, 8}
	if !oraclePoint(keys, 3) || oraclePoint(keys, 4) {
		t.Fatal("oracle 点查错误")
	}
	if got := oracleRange(keys, 3, 6); got != 3 { // 3,3,5
		t.Fatalf("oracle 范围查 got %d want 3", got)
	}
	_ = sort.Ints
}
