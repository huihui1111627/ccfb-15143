// Package bench 把学习型索引、B+ 树与全量有序数组二分放在同一负载下对比：
// 索引大小、点查/范围查时延、每次查找实际搜索范围、插入吞吐，
// 并用全量有序数组二分核对全部查询结果。
package bench

import (
	"fmt"
	"math/rand"
	"sort"
	"time"

	"learnedindex/internal/data"
	"learnedindex/internal/learned"
	"learnedindex/internal/tree"
)

// Oracle 是全量有序数组上的标准二分，作为正确性基准。
type Oracle struct{ Keys []int64 }

func (o *Oracle) Point(key int64) bool {
	i := sort.Search(len(o.Keys), func(i int) bool { return o.Keys[i] >= key })
	return i < len(o.Keys) && o.Keys[i] == key
}

func (o *Oracle) Range(lo, hi int64) int {
	l := sort.Search(len(o.Keys), func(i int) bool { return o.Keys[i] >= lo })
	r := sort.Search(len(o.Keys), func(i int) bool { return o.Keys[i] >= hi })
	return r - l
}

// IndexReport 是一种索引在一种分布上的结果。
type IndexReport struct {
	Name          string
	IndexBytes    int64
	Layers        int
	LeafSegments  int
	BuildMS       float64
	PointAvgNS    float64
	PointP50NS    float64
	PointP99NS    float64
	RangeAvgNS    float64
	WindowMean    float64
	WindowP99     int
	RouteMean     float64
	Fallbacks     int64
	CorrectErrors int
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(float64(len(sorted)-1)*q)]
}

func avg(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// LearnedReport 评估某一档 eps 的学习型索引（B+ 树等基准与 eps 无关，单独复用）。
func LearnedReport(keys []int64, eps int, w data.Workload) *IndexReport {
	orc := &Oracle{Keys: keys}
	t0 := time.Now()
	idx := learned.BuildStatic(keys, eps)
	rep := &IndexReport{
		Name:         fmt.Sprintf("learned(eps=%d)", eps),
		IndexBytes:   idx.IndexSizeBytes(),
		Layers:       idx.LayerCount(),
		LeafSegments: len(idx.LeafSegments()),
		BuildMS:      float64(time.Since(t0).Microseconds()) / 1000,
	}
	var winSum, routeSum int64
	times := make([]float64, 0, len(w.PointKeys))
	wins := make([]int, 0, len(w.PointKeys))
	for _, k := range w.PointKeys {
		s := time.Now()
		r := idx.PointLookup(k)
		times = append(times, float64(time.Since(s).Nanoseconds()))
		winSum += int64(r.Examined)
		routeSum += int64(r.RouteExamined)
		wins = append(wins, r.Examined)
		if r.Found != orc.Point(k) {
			rep.CorrectErrors++
		}
	}
	sort.Float64s(times)
	sort.Ints(wins)
	rep.PointAvgNS, rep.PointP50NS, rep.PointP99NS = avg(times), quantile(times, .5), quantile(times, .99)
	rep.WindowMean = float64(winSum) / float64(len(w.PointKeys))
	rep.WindowP99 = wins[int(float64(len(wins)-1)*.99)]
	rep.RouteMean = float64(routeSum) / float64(len(w.PointKeys))
	rep.Fallbacks = idx.Fallbacks

	rt := make([]float64, 0, len(w.RangeLos))
	for i := range w.RangeLos {
		s := time.Now()
		st, en, _ := idx.RangeLookup(w.RangeLos[i], w.RangeHis[i])
		rt = append(rt, float64(time.Since(s).Nanoseconds()))
		if en-st != orc.Range(w.RangeLos[i], w.RangeHis[i]) {
			rep.CorrectErrors++
		}
	}
	sort.Float64s(rt)
	rep.RangeAvgNS = avg(rt)
	return rep
}

// TreeReport 构建并评估 B+ 树（每分布只需调用一次，与 eps 无关）。
func TreeReport(keys []int64, w data.Workload) *IndexReport {
	orc := &Oracle{Keys: keys}
	t0 := time.Now()
	bt := tree.New()
	for _, k := range keys {
		bt.Insert(k)
	}
	rep := &IndexReport{
		Name:       "bplustree",
		IndexBytes: bt.SizeBytes(),
		BuildMS:    float64(time.Since(t0).Microseconds()) / 1000,
	}
	times := make([]float64, 0, len(w.PointKeys))
	for _, k := range w.PointKeys {
		s := time.Now()
		r := bt.PointLookup(k)
		times = append(times, float64(time.Since(s).Nanoseconds()))
		if r.Found != orc.Point(k) {
			rep.CorrectErrors++
		}
	}
	sort.Float64s(times)
	rep.PointAvgNS, rep.PointP50NS, rep.PointP99NS = avg(times), quantile(times, .5), quantile(times, .99)
	rt := make([]float64, 0, len(w.RangeLos))
	for i := range w.RangeLos {
		s := time.Now()
		r := bt.RangeLookup(w.RangeLos[i], w.RangeHis[i])
		rt = append(rt, float64(time.Since(s).Nanoseconds()))
		if r.Count != orc.Range(w.RangeLos[i], w.RangeHis[i]) {
			rep.CorrectErrors++
		}
	}
	sort.Float64s(rt)
	rep.RangeAvgNS = avg(rt)
	return rep
}

// ArrayReport 评估全量有序数组二分（零索引结构，纯时延基准）。
func ArrayReport(keys []int64, w data.Workload) *IndexReport {
	orc := &Oracle{Keys: keys}
	rep := &IndexReport{Name: "binary-search"}
	times := make([]float64, 0, len(w.PointKeys))
	for _, k := range w.PointKeys {
		s := time.Now()
		_ = orc.Point(k)
		times = append(times, float64(time.Since(s).Nanoseconds()))
	}
	sort.Float64s(times)
	rep.PointAvgNS, rep.PointP50NS, rep.PointP99NS = avg(times), quantile(times, .5), quantile(times, .99)
	rt := make([]float64, 0, len(w.RangeLos))
	for i := range w.RangeLos {
		s := time.Now()
		_ = orc.Range(w.RangeLos[i], w.RangeHis[i])
		rt = append(rt, float64(time.Since(s).Nanoseconds()))
	}
	sort.Float64s(rt)
	rep.RangeAvgNS = avg(rt)
	return rep
}

// InsertReport 动态插入对比。
type InsertReport struct {
	Name           string
	InsertKPS      float64
	IndexBytes     int64
	Segments       int
	Refits         int64
	Splits         int64
	RouterRebuilds int64
	LookupErrors   int
	FinalCount     int64
}

// RunInserts 对学习型动态索引与 B+ 树做插入吞吐对比，并核对最终数据正确性。
func RunInserts(p data.Profile, baseKeys, insertKeys []int64, eps, ovfCap int, gapRatio float64) (*InsertReport, *InsertReport) {
	t0 := time.Now()
	dyn := learned.NewDynamic(baseKeys, eps, ovfCap, gapRatio)
	for _, k := range insertKeys {
		dyn.Insert(k)
	}
	learnedDur := time.Since(t0)
	total := int64(len(baseKeys)) + int64(len(insertKeys))
	ins, ref, spl, rb := dyn.Stats()

	snap := dyn.SortedSnapshot()
	orc := &Oracle{Keys: snap}
	g := rand.New(rand.NewSource(42))
	errs := 0
	for i := 0; i < 2000; i++ {
		var k int64
		if i%2 == 0 {
			k = snap[g.Intn(len(snap))]
		} else {
			k = g.Int63()
		}
		if dyn.PointLookup(k).Found != orc.Point(k) {
			errs++
		}
	}
	lrep := &InsertReport{
		Name:           fmt.Sprintf("learned-dynamic(eps=%d)", eps),
		InsertKPS:      float64(total) / learnedDur.Seconds() / 1000,
		IndexBytes:     dyn.IndexSizeBytes(),
		Segments:       dyn.SegmentCount(),
		Refits:         ref,
		Splits:         spl,
		RouterRebuilds: rb,
		LookupErrors:   errs,
		FinalCount:     dyn.Count(),
	}
	_ = ins

	t0 = time.Now()
	bt := tree.New()
	for _, k := range baseKeys {
		bt.Insert(k)
	}
	for _, k := range insertKeys {
		bt.Insert(k)
	}
	btDur := time.Since(t0)
	return lrep, &InsertReport{
		Name:       "bplustree",
		InsertKPS:  float64(total) / btDur.Seconds() / 1000,
		IndexBytes: bt.SizeBytes(),
		FinalCount: int64(bt.Size()),
	}
}
