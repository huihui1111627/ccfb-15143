// Package pla 实现“分段线性近似”（Piecewise Linear Approximation）。
//
// 给定排好序的键 x[0..n-1]（下标 i 即键的“位置”），用尽量少的线段
// 拟合映射 x -> i，使每个点的预测位置与真实位置之差的绝对值不超过 eps。
//
// 一遍扫描贪心：维护当前段起点 first，逐个尝试把新点纳入当前段；每次用
// float64 候选斜率（水平 + 相邻异键点误差带角点斜率）配合残差跨度验证
// 可行性，不可行就在 i-1 关段、从 i 重开。eps=0 段额外保存整数定点模型，
// 查找时无浮点舍入。重复键由“同键 y 跨度 ≤ 2eps”的容量规则单独切分。
// 整体 O(n)。
package pla

// Segment 是一段线性模型：pred(x) = slope*x + intercept。
type Segment struct {
	Slope     float64
	Intercept float64
	First     int64
	Last      int64
	StartPos  int64
	EndPos    int64

	// 精确定点模型（eps=0）：pos = floor(SlopeNum*(x-BaseKey)/SlopeDen)+BasePos。
	SlopeNum int64
	SlopeDen int64
	BaseKey  int64
	BasePos  int64
	Exact    bool
}

// Predict 返回浮点预测位置（eps>0 与可视化用）。
func (s Segment) Predict(x int64) float64 {
	return s.Slope*float64(x) + s.Intercept
}

// PredictFloor 返回精确定点模型下的预测位置（向下取整），全程整数运算。
func (s Segment) PredictFloor(x int64) int64 {
	num := s.SlopeNum * (x - s.BaseKey)
	q := num / s.SlopeDen
	if num < 0 && num%s.SlopeDen != 0 {
		q--
	}
	return q + s.BasePos
}

// Fit 对有序键 keys 执行一遍扫描分段，保证每段误差不超过 eps。
//
// 逐点调用 buildFast 检验当前段可行性；buildFast 为 O(段长)，每个点只在
// 其所属段内被线性扫描，整体摊销 O(n)。eps=0 段用整数定点模型，查找零舍入。
func Fit(keys []int64, eps int) []Segment {
	n := len(keys)
	if n == 0 {
		return nil
	}
	if eps < 0 {
		eps = 0
	}
	e := int64(eps)

	var segs []Segment
	first := 0 // 当前段起点
	lo, hi := negInf, posInf

	// emit 在 [first..lastIdx] 上产出一个合规段，返回下一段起点 nextFirst。
	// 在线判据已保证常规情况一次成功；buildFast 失败时二分最长可行前缀。
	emit := func(lastIdx int) int {
		if s, ok := buildFast(keys, first, lastIdx, e, lo, hi); ok {
			segs = append(segs, s)
			return lastIdx + 1
		}
		l, r, best := first, lastIdx-1, first
		for l <= r {
			m := int(uint(l+r) >> 1)
			if _, ok := buildFast(keys, first, m, e, negInf, posInf); ok {
				best, l = m, m+1
			} else {
				r = m - 1
			}
		}
		s, _ := buildFast(keys, first, best, e, negInf, posInf)
		segs = append(segs, s)
		return best + 1
	}

	for first < n {
		lo, hi = negInf, posInf
		runStart := first
		i := first + 1
		for ; i < n; i++ {
			if keys[i] != keys[i-1] {
				runStart = i
			}
			runLen := i - runStart + 1
			dupOverflow := (runStart != first && int64(runLen-1) > 2*e) ||
				(runStart == first && int64(runLen) > 2*e+1)
			if dupOverflow || (e == 0 && keys[i] == keys[first]) {
				break
			}
			// O(1)/点在线斜率区间收紧。
			if dx := float64(keys[i] - keys[first]); dx != 0 {
				sLow := (float64(i) - float64(e) - float64(first)) / dx
				sHigh := (float64(i) + float64(e) - float64(first)) / dx
				if sLow > lo {
					lo = sLow
				}
				if sHigh < hi {
					hi = sHigh
				}
				if lo > hi {
					break
				}
			}
		}
		// 段覆盖 [first..i-1]；emit 可能因 buildFast 失败而缩短，nextFirst
		// 之后的点由外层下一轮重新扫描，保证不丢点、不重叠。
		first = emit(i - 1)
	}
	return segs
}

// singleSeg 构造单点段（必然可行）。
func singleSeg(keys []int64, idx int) Segment {
	return Segment{
		First: keys[idx], Last: keys[idx],
		StartPos: int64(idx), EndPos: int64(idx),
		Slope: 0, Intercept: float64(idx),
		SlopeNum: 0, SlopeDen: 1,
		BaseKey: keys[idx], BasePos: int64(idx), Exact: true,
	}
}

const (
	negInf = -1e300
	posInf = 1e300
)

// buildFast 用在线可行斜率边界 lo/hi 构造段。只试有限个候选斜率（水平、
// 首末连线、在线上下界），每个配合一次 O(段长) 残差扫描定截距并验证。
// 关段时调用一次，因此不破坏 Fit 的 O(n) 复杂度。
func buildFast(keys []int64, first, last int, e int64, lo, hi float64) (Segment, bool) {
	seg := Segment{
		First:    keys[first],
		Last:     keys[last],
		StartPos: int64(first),
		EndPos:   int64(last),
	}
	x0, y0 := keys[first], int64(first)

	if e == 0 {
		if last == first {
			seg.Slope, seg.Intercept = 0, float64(y0)
			seg.SlopeNum, seg.SlopeDen = 0, 1
			seg.BaseKey, seg.BasePos, seg.Exact = x0, y0, true
			return seg, true
		}
		if keys[last] != x0 {
			dx := keys[last] - x0
			dy := int64(last - first)
			slope := float64(dy) / float64(dx)
			if _, maxErr := fitIntercept(keys, first, last, slope); maxErr <= 1e-9 {
				seg.SlopeNum, seg.SlopeDen = dy, dx
				seg.Slope = slope
				seg.Intercept = float64(y0) - slope*float64(x0)
				seg.BaseKey, seg.BasePos, seg.Exact = x0, y0, true
				return seg, true
			}
		}
		return Segment{}, false
	}

	ef := float64(e)
	candidates := []float64{0}
	if keys[last] != keys[first] {
		candidates = append(candidates,
			float64(last-first)/float64(keys[last]-keys[first]))
	}
	if lo > negInf/2 {
		candidates = append(candidates, lo)
	}
	if hi < posInf/2 {
		candidates = append(candidates, hi)
	}
	for _, sl := range candidates {
		intc, maxErr := fitIntercept(keys, first, last, sl)
		if maxErr <= ef+1e-9 {
			seg.Slope, seg.Intercept = sl, intc
			return seg, true
		}
	}
	return Segment{}, false
}

// fitIntercept 给定斜率，返回使最大误差最小的截距（残差区间中心）与最大误差。
func fitIntercept(keys []int64, first, last int, slope float64) (intercept, maxErr float64) {
	minRes, maxRes := posInf, negInf
	for j := first; j <= last; j++ {
		r := float64(j) - slope*float64(keys[j])
		if r < minRes {
			minRes = r
		}
		if r > maxRes {
			maxRes = r
		}
	}
	intercept = (minRes + maxRes) / 2
	maxErr = (maxRes - minRes) / 2
	return intercept, maxErr
}

// FindSegment 在按 First 有序的段数组中找最后一个 First <= key 的段；
// 全部 First 都大于 key 时返回 -1。
func FindSegment(segs []Segment, key int64) int {
	lo, hi := 0, len(segs)-1
	res := -1
	for lo <= hi {
		mid := int(uint(lo+hi) >> 1)
		if segs[mid].First <= key {
			res = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return res
}
