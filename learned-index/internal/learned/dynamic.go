// 动态学习型索引：支持插入。
//
// 物理布局把叶子层每个段对应到一个独立的有序 block，并预留空隙：
// block 容量 = 拟合点数 * (1+gapRatio)，插入优先落到 block 的空位里，
// 保持有序（用 memmove 腾位，代价与 block 大小成正比）。
// block 放不下时进入该段的溢出缓冲区；缓冲区满（overflowCap）后只对
// 这一段做重新 PLA 拟合——可能仍是一段（更新模型），也可能分裂成多段。
// 段数变化时重建上层路由模型。
package learned

import (
	"sort"

	"learnedindex/internal/pla"
)

// DynSegment 是动态索引的叶子段。
type DynSegment struct {
	firstKey, lastKey int64
	slope, intercept  float64
	block             []int64 // 有序，尾部 nil 表示空位
	used              int
	overflow          []int64 // 有序溢出缓冲
	slack             int     // 相对当前模型的最大漂移，控制查找窗口
}

func (s *DynSegment) predict(x int64) float64 {
	return s.slope*float64(x) + s.intercept
}

// DynamicIndex 支持插入的学习型索引。
//
// 上层路由就是各段首键组成的有序数组 + 二分（段数通常几百到几千，
// O(log 段数) 足够快），因此单段 refit/分裂后只需重建这个小切片，
// 不必重建整棵多层模型。
type DynamicIndex struct {
	segs     []*DynSegment
	firsts   []int64 // 各段首键，与 segs 等长、有序
	eps      int
	ovfCap   int
	gapRatio float64

	Inserts        int64
	Refits         int64
	Splits         int64
	RouterRebuilds int64
}

// NewDynamic 在一批初始有序键上构建动态索引。
func NewDynamic(keys []int64, eps int, overflowCap int, gapRatio float64) *DynamicIndex {
	d := &DynamicIndex{eps: eps, ovfCap: overflowCap, gapRatio: gapRatio}
	base := pla.Fit(keys, eps)
	for _, sg := range base {
		part := keys[sg.StartPos : sg.EndPos+1]
		capHint := int(float64(len(part))*(1+gapRatio)) + 1
		block := make([]int64, capHint)
		copy(block, part)
		// 段内局部模型：把 block 内位置作为预测目标，截距平移到局部坐标。
		d.segs = append(d.segs, &DynSegment{
			firstKey:  sg.First,
			lastKey:   sg.Last,
			slope:     sg.Slope,
			intercept: sg.Intercept - float64(sg.StartPos),
			block:     block,
			used:      len(part),
		})
	}
	d.buildRouter()
	return d
}

func (d *DynamicIndex) buildRouter() {
	firsts := make([]int64, len(d.segs))
	for i, s := range d.segs {
		firsts[i] = s.firstKey
	}
	d.firsts = firsts
	d.RouterRebuilds++
}

// findSeg 找最后一个 firstKey <= key 的段；key 小于所有段首键时归到第 0 段。
func (d *DynamicIndex) findSeg(key int64) int {
	lo, hi := 0, len(d.segs)-1
	res := -1
	for lo <= hi {
		m := int(uint(lo+hi) >> 1)
		if d.segs[m].firstKey <= key {
			res = m
			lo = m + 1
		} else {
			hi = m - 1
		}
	}
	if res < 0 {
		res = 0
	}
	return res
}

// Insert 插入一个键（允许重复）。
//
// 段范围始终保持不交且有序：路由命中的段若容纳不下该键（落在相邻段
// 范围内），转交相邻段。只有第 0 段可向下扩展、最后一段可向上扩展，
// 此时路由首键集合变化，需要重建上层路由模型。
func (d *DynamicIndex) Insert(key int64) {
	d.Inserts++
	si := d.findSeg(key)
	routerDirty := false

	// 越过左边界：交给前一段；第 0 段没有前驱，只能自己扩展并重建路由。
	if key < d.segs[si].firstKey {
		if si > 0 {
			si--
		} else {
			d.segs[0].firstKey = key
			routerDirty = true
		}
	}
	// 越过右边界：交给后一段；最后一段自己扩展。
	if si < len(d.segs)-1 && key >= d.segs[si+1].firstKey {
		si++
	} else if si == len(d.segs)-1 && key > d.segs[si].lastKey {
		routerDirty = true
	}
	s := d.segs[si]

	if s.used < len(s.block) {
		pos := sort.Search(s.used, func(i int) bool { return s.block[i] >= key })
		// 在预留空隙内腾位：len==cap，不会触发底层扩容。
		copy(s.block[pos+1:s.used+1], s.block[pos:s.used])
		s.block[pos] = key
		s.used++
	} else {
		pos := sort.Search(len(s.overflow), func(i int) bool { return s.overflow[i] >= key })
		s.overflow = append(s.overflow, 0)
		copy(s.overflow[pos+1:], s.overflow[pos:])
		s.overflow[pos] = key
	}
	if key < s.firstKey {
		s.firstKey = key
	}
	if key > s.lastKey {
		s.lastKey = key
	}
	s.slack++
	if routerDirty {
		d.buildRouter()
	}
	if len(s.overflow) >= d.ovfCap || s.slack > 4*d.eps+4 {
		d.refit(si)
	}
}

// refit 只重新拟合第 idx 段；分裂时原地替换为多个新段并重建上层路由。
func (d *DynamicIndex) refit(idx int) {
	old := d.segs[idx]
	merged := make([]int64, 0, old.used+len(old.overflow))
	merged = append(merged, old.block[:old.used]...)
	merged = append(merged, old.overflow...)
	sort.Slice(merged, func(i, j int) bool { return merged[i] < merged[j] })

	newSegs := pla.Fit(merged, d.eps)
	built := make([]*DynSegment, 0, len(newSegs))
	for _, sg := range newSegs {
		part := merged[sg.StartPos : sg.EndPos+1]
		capHint := int(float64(len(part))*(1+d.gapRatio)) + 1
		block := make([]int64, capHint)
		copy(block, part)
		built = append(built, &DynSegment{
			firstKey:  sg.First,
			lastKey:   sg.Last,
			slope:     sg.Slope,
			intercept: sg.Intercept - float64(sg.StartPos),
			block:     block,
			used:      len(part),
		})
	}
	d.Refits++
	if len(built) > 1 {
		d.Splits++
	}
	// 原地替换段数组。
	tail := append([]*DynSegment{}, d.segs[idx+1:]...)
	d.segs = append(append(d.segs[:idx], built...), tail...)
	d.buildRouter()
}

// DynLookupResult 动态点查诊断。
type DynLookupResult struct {
	Found       bool
	BlockExam   int
	OvfExam     int
	RouteExam   int
	RefitNeeded bool
}

// PointLookup 点查：先路由到段，再在 block + overflow 的预测窗口内二分。
func (d *DynamicIndex) PointLookup(key int64) DynLookupResult {
	if len(d.segs) == 0 {
		return DynLookupResult{}
	}
	// 路由与 Insert 使用同一个“最后一个 firstKey<=key”选择器，
	// 保证键所在段与插入时归属的段一致。
	si := d.findSeg(key)
	s := d.segs[si]
	// 路由是首键数组上的二分，检查量按 log2(段数) 估算。
	res := DynLookupResult{RouteExam: bitLen(len(d.firsts))}

	window := d.eps + s.slack + 1
	pred := s.predict(key)
	lo := int(pred) - window
	hi := int(pred) + window
	if lo < 0 {
		lo = 0
	}
	if hi >= s.used {
		hi = s.used - 1
	}
	if lo <= hi {
		res.BlockExam = hi - lo + 1
		pos := sort.Search(hi-lo+1, func(i int) bool { return s.block[lo+i] >= key })
		if p := lo + pos; p < s.used && s.block[p] == key {
			res.Found = true
			return res
		}
	}
	// 溢出缓冲通常很小，直接有序二分。
	res.OvfExam = len(s.overflow)
	pos := sort.Search(len(s.overflow), func(i int) bool { return s.overflow[i] >= key })
	if pos < len(s.overflow) && s.overflow[pos] == key {
		res.Found = true
	}
	return res
}

// Count 返回当前键总数（含重复）。
func (d *DynamicIndex) Count() int64 {
	var n int64
	for _, s := range d.segs {
		n += int64(s.used + len(s.overflow))
	}
	return n
}

// SortedSnapshot 返回全量有序快照，用于与基准核对正确性。
func (d *DynamicIndex) SortedSnapshot() []int64 {
	var all []int64
	for _, s := range d.segs {
		all = append(all, s.block[:s.used]...)
		all = append(all, s.overflow...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	return all
}

// IndexSizeBytes 动态索引自身占用（block 中的空位也算预留空间成本）。
func (d *DynamicIndex) IndexSizeBytes() int64 {
	var b int64
	for _, s := range d.segs {
		b += int64(cap(s.block))*8 + int64(cap(s.overflow))*8
		b += 8 * 6 // 段头部字段估算
	}
	b += int64(cap(d.firsts)) * 8 // 上层路由首键数组
	return b
}

// bitLen 返回 n 的二进制位数（即二分查找的比较次数上界）。
func bitLen(n int) int {
	b := 0
	for n > 0 {
		n >>= 1
		b++
	}
	if b == 0 {
		return 1
	}
	return b
}

// SegmentCount 当前叶子段数。
func (d *DynamicIndex) SegmentCount() int { return len(d.segs) }

// Stats 返回维护操作计数。
func (d *DynamicIndex) Stats() (inserts, refits, splits, rebuilds int64) {
	return d.Inserts, d.Refits, d.Splits, d.RouterRebuilds
}
