// Package learned 实现多层学习型有序索引（PGM-Index 风格）。
//
// 第 0 层是数据键上的 PLA 分段线性模型；段数仍然很多时，在各段起始键
// （First 数组，天然有序）上递归再拟合一层，直到顶层小到可以直接二分。
// 查找时自顶向下逐层“模型预测 + 误差窗口内二分”，叶子层再在数据数组的
// 窗口内二分，因此每层至多 O(log eps) 次比较，且不依赖树的多级指针跳转。
package learned

import (
	"math"

	"learnedindex/internal/pla"
)

// layer 是一层路由模型：segs 在该层的“下层键数组”上拟合，
// 预测值是下层数组的下标。firstKeys 缓存下层 First 数组，避免查找时分配。
type layer struct {
	segs      []pla.Segment
	firstKeys []int64
	eps       int
}

// StaticIndex 是只读的批量加载学习型索引。
type StaticIndex struct {
	keys   []int64 // 全量有序数据（共享所有权，不计入索引自身大小）
	layers []layer
	eps    int

	// 统计
	Fallbacks int64 // 预测窗口未命中、退化为全量二分的次数
}

// maxTopSegments 顶层段数低于该值时停止向上递归。
const maxTopSegments = 32

// BuildStatic 在有序 keys 上构建多层索引，所有层使用统一误差上限 eps。
func BuildStatic(keys []int64, eps int) *StaticIndex {
	idx := &StaticIndex{keys: keys, eps: eps}
	segs := pla.Fit(keys, eps)
	idx.layers = append(idx.layers, layer{segs: segs, eps: eps, firstKeys: firstKeysOf(segs)})

	for len(segs) > maxTopSegments {
		firsts := firstKeysOf(segs)
		segs = pla.Fit(firsts, eps)
		idx.layers = append(idx.layers, layer{segs: segs, eps: eps, firstKeys: firstKeysOf(segs)})
	}
	return idx
}

func firstKeysOf(segs []pla.Segment) []int64 {
	out := make([]int64, len(segs))
	for i, s := range segs {
		out[i] = s.First
	}
	return out
}

// Eps 返回误差上限。
func (idx *StaticIndex) Eps() int { return idx.eps }

// LayerCount 返回模型层数（含叶子层）。
func (idx *StaticIndex) LayerCount() int { return len(idx.layers) }

// SegmentCounts 返回每层段数，下标 0 为叶子层。
func (idx *StaticIndex) SegmentCounts() []int {
	out := make([]int, len(idx.layers))
	for i, l := range idx.layers {
		out[i] = len(l.segs)
	}
	return out
}

// LeafSegments 返回叶子层分段（可视化与动态索引复用）。
func (idx *StaticIndex) LeafSegments() []pla.Segment { return idx.layers[0].segs }

// IndexSizeBytes 估算索引自身内存（不含数据键数组），含各层缓存的 First 数组。
func (idx *StaticIndex) IndexSizeBytes() int64 {
	var b int64
	for _, l := range idx.layers {
		b += int64(cap(l.segs)) * int64(segSize)
		b += int64(cap(l.firstKeys)) * 8
	}
	return b
}

// segSize 与 pla.Segment 的实际布局一致：2*float64 + 3*int64 = 40B。
const segSize = 40

// clamp 把位置裁剪到数组合法区间。
func clamp(p, n int64) int64 {
	if p < 0 {
		return 0
	}
	if p >= n {
		return n - 1
	}
	return p
}

// lowerBound 在 keys[lo:hi+1] 窗口内返回第一个 >= key 的下标；
func lowerBound(keys []int64, lo, hi int, key int64) (pos, examined int) {
	if lo > hi {
		return lo, 0
	}
	examined = hi - lo + 1
	l, r := lo, hi+1
	for l < r {
		m := int(uint(l+r) >> 1)
		if keys[m] < key {
			l = m + 1
		} else {
			r = m
		}
	}
	return l, examined
}

// routeLayer 在第 layerIdx 层上路由，lowerFirsts 是该层下层的 First 数组。
func (idx *StaticIndex) routeLayer(layerIdx int, lowerFirsts []int64, key int64) (segIdx, examined int) {
	l := idx.layers[layerIdx]
	si := pla.FindSegment(l.segs, key)
	if si < 0 {
		si = 0
	}
	pred := l.segs[si].Predict(key)
	// 两侧 +1 吸收浮点舍入，保证真实下标落在窗口内。
	lo := clamp(int64(math.Floor(pred))-int64(l.eps)-1, int64(len(lowerFirsts)))
	hi := clamp(int64(math.Ceil(pred))+int64(l.eps)+1, int64(len(lowerFirsts)))
	// 右边界二分：最后一个 First <= key
	lb, rb := int(lo), int(hi)
	examined = rb - lb + 1
	res := si // 兜底：模型保证窗口内必中，正常情况下二分结果即答案
	for lb <= rb {
		m := int(uint(lb+rb) >> 1)
		if lowerFirsts[m] <= key {
			res = m
			lb = m + 1
		} else {
			rb = m - 1
		}
	}
	return res, examined
}

// LookupResult 是一次点查的诊断信息。
type LookupResult struct {
	Found         bool
	Position      int // 命中时为首个相等位置；未命中为 lowerBound 插入点
	Examined      int // 叶子层窗口大小（实际搜索范围）
	RouteExamined int // 上层路由累计检查元素数
	Layers        int
	FellBack      bool
}

// PointLookup 点查。预测窗口未命中时退化为全量二分并计入 Fallbacks。
func (idx *StaticIndex) PointLookup(key int64) LookupResult {
	n := len(idx.keys)
	if n == 0 {
		return LookupResult{Position: 0, Layers: len(idx.layers)}
	}
	routeExam := 0
	// 自顶向下逐层路由，最终定位到叶子段。单层时直接在叶子层找段。
	if len(idx.layers) > 1 {
		for li := len(idx.layers) - 1; li >= 1; li-- {
			// 第 li 层的下层是第 li-1 层，其 First 数组已在构建时缓存。
			_, ex := idx.routeLayer(li, idx.layers[li-1].firstKeys, key)
			routeExam += ex
		}
	}

	// 叶子层：找段后直接用“预测位置 ± eps”作为二分窗口。
	// PLA 误差保证使真实位置必然落在窗口内，无需再按段边界裁剪
	// （裁剪反而会切掉段边界处的合法位置）。重复键同理：同一键的所有
	// 副本位置连续，且距该键任一点的预测不超过 eps，窗口天然覆盖。
	leaf := idx.layers[0].segs
	si := pla.FindSegment(leaf, key)
	if si < 0 {
		si = 0
	}
	var lo, hi int64
	if leaf[si].Exact {
		// 精确定点模型：整数预测无浮点误差，eps=0 时窗口退化为单点。
		p := leaf[si].PredictFloor(key)
		lo = clamp(p-int64(idx.eps), int64(n))
		hi = clamp(p+int64(idx.eps), int64(n))
	} else {
		// 浮点模型两侧各留 1 的安全余量，吸收 float64 在误差边界的舍入
		// （数学误差仍为 eps，实际窗口至多 2eps+3，不影响复杂度）。
		pred := leaf[si].Predict(key)
		lo = clamp(int64(math.Floor(pred))-int64(idx.eps)-1, int64(n))
		hi = clamp(int64(math.Ceil(pred))+int64(idx.eps)+1, int64(n))
	}
	examined := int(hi-lo) + 1
	pos, _ := lowerBound(idx.keys, int(lo), int(hi), key)
	found := pos < n && idx.keys[pos] == key

	// 重复键可能横跨多个段，窗口内二分命中的未必是该键的首个副本。
	// 在 [0,pos] 上再做一次二分定位首个 >= key 的位置（即首个副本），
	// O(log n)，避免对大重复组（如 Zipf 热门键）线性回扫。
	if found && pos > 0 && idx.keys[pos-1] == key {
		l, r := 0, pos
		steps := 0
		for l < r {
			steps++
			m := int(uint(l+r) >> 1)
			if idx.keys[m] < key {
				l = m + 1
			} else {
				r = m
			}
		}
		examined += steps // 仅计二分比较次数，不计跳过的副本数
		pos = l
	}

	// 防御性回退：理论上不会发生，发生即说明误差保证被破坏（如浮点精度）。
	if !found {
		// 确认 key 是否真的不存在（全量二分，只做一次）。
		l, r := 0, n
		for l < r {
			m := int(uint(l+r) >> 1)
			if idx.keys[m] < key {
				l = m + 1
			} else {
				r = m
			}
		}
		if l < n && idx.keys[l] == key {
			idx.Fallbacks++
			pos, found, examined = l, true, n
		} else {
			pos = l
		}
	}
	return LookupResult{
		Found: found, Position: pos, Examined: examined,
		RouteExamined: routeExam, Layers: len(idx.layers),
		FellBack: found && examined == n,
	}
}

// RangeLookup 返回 [lo,hi) 内全部键的下标区间 [start,end)，并统计检查量。
// 两次点查分别定位下界（lowerBound）与上界（upperBound），复用点查的
// 误差窗口与回退保证，确保不存在的键、跨段重复键都正确。
func (idx *StaticIndex) RangeLookup(lo, hi int64) (start, end, examined int) {
	n := len(idx.keys)
	if n == 0 || lo >= hi {
		return 0, 0, 0
	}
	rLo := idx.PointLookup(lo) // Position 即 lowerBound
	start = rLo.Position
	// 上界 = 对 hi 的 lowerBound：直接复用点查路径。
	rHi := idx.PointLookup(hi)
	end = rHi.Position
	examined = rLo.Examined + rLo.RouteExamined + rHi.Examined + rHi.RouteExamined
	if end < start {
		end = start
	}
	return start, end, examined
}

// TraceStep 是可视化用的单层路由步骤。
type TraceStep struct {
	Layer        int     `json:"layer"`
	SegmentIndex int     `json:"segmentIndex"`
	Slope        float64 `json:"slope"`
	Intercept    float64 `json:"intercept"`
	Predicted    float64 `json:"predicted"`
	WindowLo     int     `json:"windowLo"`
	WindowHi     int     `json:"windowHi"`
	Chosen       int     `json:"chosen"`
}

// Trace 还原一次点查的完整过程，供页面展示。
func (idx *StaticIndex) Trace(key int64) ([]TraceStep, LookupResult) {
	var steps []TraceStep
	n := len(idx.keys)
	if n == 0 {
		return nil, LookupResult{}
	}
	for li := len(idx.layers) - 1; li >= 1; li-- {
		l := idx.layers[li]
		si := pla.FindSegment(l.segs, key)
		if si < 0 {
			si = 0
		}
		var lowerLen int
		if li == 0 {
			lowerLen = n
		} else {
			lowerLen = len(idx.layers[li-1].segs)
		}
		pred := l.segs[si].Predict(key)
		wlo := clamp(int64(math.Floor(pred))-int64(l.eps)-1, int64(lowerLen))
		whi := clamp(int64(math.Ceil(pred))+int64(l.eps)+1, int64(lowerLen))
		chosen, _ := idx.routeLayer(li, idx.layers[li-1].firstKeys, key)
		steps = append(steps, TraceStep{
			Layer: li, SegmentIndex: si,
			Slope: l.segs[si].Slope, Intercept: l.segs[si].Intercept,
			Predicted: pred, WindowLo: int(wlo), WindowHi: int(whi), Chosen: chosen,
		})
	}
	// 叶子层
	leaf := idx.layers[0].segs
	si := pla.FindSegment(leaf, key)
	if si < 0 {
		si = 0
	}
	seg := leaf[si]
	var pred float64
	var wlo, whi int64
	if seg.Exact {
		p := seg.PredictFloor(key)
		pred = float64(p)
		wlo = clamp(p-int64(idx.eps), int64(n))
		whi = clamp(p+int64(idx.eps), int64(n))
	} else {
		pred = seg.Predict(key)
		wlo = clamp(int64(math.Floor(pred))-int64(idx.eps)-1, int64(n))
		whi = clamp(int64(math.Ceil(pred))+int64(idx.eps)+1, int64(n))
	}
	pos, _ := lowerBound(idx.keys, int(wlo), int(whi), key)
	steps = append(steps, TraceStep{
		Layer: 0, SegmentIndex: si,
		Slope: seg.Slope, Intercept: seg.Intercept,
		Predicted: pred, WindowLo: int(wlo), WindowHi: int(whi), Chosen: pos,
	})
	res := idx.PointLookup(key)
	return steps, res
}
