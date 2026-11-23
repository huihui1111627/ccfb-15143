// Package tree 是一个内存 B+ 树实现，作为学习型索引的对照基线。
//
// 叶子节点按键有序并用链表串起来支持范围扫描；内部节点保存路由键。
// 与学习型索引一样统计每次查找实际检查的键数，便于公平比较。
package tree

// maxKeys 为单个节点最多保存的键数（内部节点最多 maxKeys+1 个孩子）。
const maxKeys = 16

type leaf struct {
	keys []int64
	next *leaf
	prev *leaf
}

type node struct {
	keys     []int64
	children []*node // 内部节点专用
	leaf     *leaf   // 叶子层级直接挂 leaf，简化实现
	internal bool
}

// BPlusTree 内存 B+ 树。
type BPlusTree struct {
	root *node
	size int
}

// New 返回空树。
func New() *BPlusTree {
	// 根初始为叶子层空节点。
	return &BPlusTree{root: &node{leaf: &leaf{}}}
}

// Insert 插入键（重复键也存，叶子内允许重复）。
func (t *BPlusTree) Insert(key int64) {
	t.size++
	// 根到叶路径，记录节点与进入的孩子下标。
	pathNodes := []*node{}
	pathIdx := []int{}
	cur := t.root
	for cur.internal {
		idx := upperBound(cur.keys, key)
		pathNodes = append(pathNodes, cur)
		pathIdx = append(pathIdx, idx)
		cur = cur.children[idx]
	}
	lf := cur.leaf
	pos := upperBound(lf.keys, key)
	lf.keys = insertAt(lf.keys, pos, key)
	if len(lf.keys) <= maxKeys {
		return
	}
	// 分裂叶子。重复键允许溢出节点容量，但分裂点必须落在不同键的边界上，
	// 否则路由键无法区分左右叶，范围/点查会漏掉跨叶的重复值。
	mid := len(lf.keys) / 2
	for mid > 0 && lf.keys[mid-1] == lf.keys[mid] {
		mid--
	}
	if mid == 0 {
		// 整叶同一个键，无法在不破坏键序的前提下分裂：允许该叶超容，
		// 等后续异键插入产生边界后再分裂。
		return
	}
	rightLeaf := &leaf{
		keys: append([]int64(nil), lf.keys[mid:]...),
		next: lf.next,
		prev: lf,
	}
	if lf.next != nil {
		lf.next.prev = rightLeaf
	}
	lf.keys = append([]int64(nil), lf.keys[:mid]...)
	lf.next = rightLeaf
	splitKey := rightLeaf.keys[0]

	// left/right 是当前待上推的一对节点；先把叶子包成节点。
	left, right := cur, &node{leaf: rightLeaf}
	for level := len(pathNodes) - 1; level >= 0; level-- {
		p := pathNodes[level]
		at := pathIdx[level]
		p.keys = insertAt(p.keys, at, splitKey)
		p.children = insertChild(p.children, at+1, right)
		if len(p.keys) <= maxKeys {
			return
		}
		// 分裂内部节点：升上的键取中间键，自身不保留它。
		m := len(p.keys) / 2
		upKey := p.keys[m]
		rn := &node{
			internal: true,
			keys:     append([]int64(nil), p.keys[m+1:]...),
			children: append([]*node(nil), p.children[m+1:]...),
		}
		p.keys = append([]int64(nil), p.keys[:m]...)
		p.children = append([]*node(nil), p.children[:m+1]...)
		left, right, splitKey = p, rn, upKey
	}
	// 新根。
	t.root = &node{
		internal: true,
		keys:     []int64{splitKey},
		children: []*node{left, right},
	}
}

// LookupResult 点查结果与检查量。
type LookupResult struct {
	Found    bool
	Examined int // 所有层级检查的键数总和
	Height   int
}

// PointLookup 点查。
func (t *BPlusTree) PointLookup(key int64) LookupResult {
	res := LookupResult{}
	cur := t.root
	for cur.internal {
		res.Height++
		res.Examined += len(cur.keys)
		idx := upperBound(cur.keys, key)
		cur = cur.children[idx]
	}
	lf := cur.leaf
	res.Examined += len(lf.keys)
	// 重复键可能在分裂时跨叶：若当前叶首键 > key，回退到前驱叶。
	for lf.prev != nil && len(lf.keys) > 0 && lf.keys[0] > key {
		lf = lf.prev
		res.Examined += len(lf.keys)
	}
	pos := lowerBound(lf.keys, key)
	res.Found = pos < len(lf.keys) && lf.keys[pos] == key
	return res
}

// RangeResult 范围查 [lo,hi) 的键数与检查量（含沿叶链扫描的键）。
type RangeResult struct {
	Count    int
	Examined int
}

// RangeLookup 范围查询。
func (t *BPlusTree) RangeLookup(lo, hi int64) RangeResult {
	if lo >= hi {
		return RangeResult{}
	}
	cur := t.root
	exam := 0
	for cur.internal {
		exam += len(cur.keys)
		cur = cur.children[upperBound(cur.keys, lo)]
	}
	lf := cur.leaf
	exam += len(lf.keys)
	// 重复键跨叶时沿前驱回退到真正的起始叶。
	for lf.prev != nil && len(lf.keys) > 0 && lf.keys[0] > lo {
		lf = lf.prev
		exam += len(lf.keys)
	}
	pos := lowerBound(lf.keys, lo)
	count := 0
	// 从定位叶开始沿链扫描，直到键 >= hi。
	for lf != nil {
		for i := pos; i < len(lf.keys); i++ {
			exam++
			if lf.keys[i] >= hi {
				return RangeResult{Count: count, Examined: exam}
			}
			if lf.keys[i] >= lo {
				count++
			}
		}
		lf = lf.next
		pos = 0
	}
	return RangeResult{Count: count, Examined: exam}
}

// Size 返回键数量。
func (t *BPlusTree) Size() int { return t.size }

// SizeBytes 估算树占用的堆内存。
func (t *BPlusTree) SizeBytes() int64 {
	var bytes int64
	var walk func(n *node)
	walk = func(n *node) {
		if n.internal {
			bytes += int64(cap(n.keys))*8 + int64(cap(n.children))*8 + 24
			for _, c := range n.children {
				walk(c)
			}
		} else {
			lf := n.leaf
			bytes += int64(cap(lf.keys))*8 + 24
		}
	}
	walk(t.root)
	return bytes
}

func lowerBound(keys []int64, key int64) int {
	lo, hi := 0, len(keys)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if keys[m] < key {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

func upperBound(keys []int64, key int64) int {
	lo, hi := 0, len(keys)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if keys[m] <= key {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

func insertAt(keys []int64, pos int, key int64) []int64 {
	keys = append(keys, 0)
	copy(keys[pos+1:], keys[pos:])
	keys[pos] = key
	return keys
}

func insertChild(ch []*node, pos int, c *node) []*node {
	ch = append(ch, nil)
	copy(ch[pos+1:], ch[pos:])
	ch[pos] = c
	return ch
}
