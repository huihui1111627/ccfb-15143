package vdisk

import (
	"fmt"
	"sort"
)

// IssueKind 是检查发现的问题类别。
type IssueKind string

const (
	// IssueCorruption 结构损坏：CRC、对齐、保留位、标志位组合、角色冲突等。
	IssueCorruption IssueKind = "corruption"
	// IssueLeak 泄漏：簇存在但没有任何引用（存储计数为 0，应为空闲）。
	IssueLeak IssueKind = "leak"
	// IssueRefUnder 引用计数偏小：实际引用数大于存储值。
	IssueRefUnder IssueKind = "refcount-under"
	// IssueRefOver 引用计数偏大：存储值大于实际引用数（含指向不存在引用的计数）。
	IssueRefOver IssueKind = "refcount-over"
)

// Issue 是一条检查发现。
type Issue struct {
	Kind    IssueKind `json:"kind"`
	Cluster int64     `json:"cluster,omitempty"`
	Detail  string    `json:"detail"`
}

// Report 是一次检查或修复的结果。
type Report struct {
	Path            string  `json:"path"`
	VirtualClusters int64   `json:"virtualClusters"`
	MaxClusters     int64   `json:"maxClusters"`
	HighWater       int64   `json:"highWater"`
	FileBytes       int64   `json:"fileBytes"`
	Issues          []Issue `json:"issues"`
	// 修复模式下记录每个动作。
	Actions []Issue `json:"actions,omitempty"`
}

// Clean 报告是否没有任何问题。
func (r *Report) Clean() bool { return len(r.Issues) == 0 }

func (r *Report) count(k IssueKind) int {
	n := 0
	for _, is := range r.Issues {
		if is.Kind == k {
			n++
		}
	}
	return n
}

// newReport 生成基础报告骨架。
func (img *Image) newReport(path string) *Report {
	return &Report{
		Path:            path,
		VirtualClusters: img.virtualClusters,
		MaxClusters:     img.maxClusters,
		HighWater:       img.highWater,
		FileBytes:       img.blk.Size(),
	}
}

// scanActualRefs 扫描整棵映射树，重算每个物理簇的真实引用数。
// 同时做完整的逐项合法性检查，发现的损坏写入 issues。
func (img *Image) scanActualRefs(issues *[]Issue) map[int64]int64 {
	actual := map[int64]int64{}
	// 根表簇（L1、refL1）由头部直接引用，各计 1。
	for c := img.l1Start; c < img.l1Start+img.l1Clusters(); c++ {
		actual[c]++
	}
	for c := img.refL1Start; c < img.refL1Start+img.refL1Clusters(); c++ {
		actual[c]++
	}
	addIssue := func(k IssueKind, c int64, format string, args ...any) {
		*issues = append(*issues, Issue{Kind: k, Cluster: c, Detail: fmt.Sprintf(format, args...)})
	}
	l1Groups := (img.virtualClusters + img.entriesPer - 1) / img.entriesPer
	for g := int64(0); g < l1Groups; g++ {
		l2c := img.l1[g]
		if l2c == 0 {
			continue
		}
		actual[l2c]++ // L1 -> L2
		buf, err := img.readCluster(l2c)
		if err != nil {
			addIssue(IssueCorruption, l2c, "读取 L2 簇失败: %v", err)
			continue
		}
		groupBase := g * img.entriesPer
		for j := int64(0); j < img.entriesPer; j++ {
			tvc := groupBase + j
			raw := readEntry(buf, j)
			if tvc >= img.virtualClusters {
				if raw != 0 {
					addIssue(IssueCorruption, l2c, "L2 尾部越界项非零: %#x", raw)
				}
				continue
			}
			target, isZero, err := img.validateMapEntry(raw, "L2")
			if err != nil {
				addIssue(IssueCorruption, l2c, "vc=%d: %v", tvc, err)
				continue
			}
			if target != 0 {
				actual[target]++ // L2 -> 数据簇
			}
			_ = isZero
		}
	}
	// refL1 -> refL2，以及 refL2 内每项的合法性。
	for pc := int64(0); pc < img.maxClusters; pc++ {
		refL2 := img.refL1[pc]
		if refL2 == 0 {
			continue
		}
		actual[refL2]++
		buf, err := img.readCluster(refL2)
		if err != nil {
			addIssue(IssueCorruption, refL2, "读取 refL2 簇失败: %v", err)
			continue
		}
		raw := readEntry(buf, pc%img.entriesPer)
		if _, err := validateRefEntry(raw, "refL2"); err != nil {
			addIssue(IssueCorruption, refL2, "pc=%d: %v", pc, err)
		}
	}
	return actual
}

// Check 只读扫描镜像，重算引用计数并生成报告，不修改任何数据。
func (img *Image) Check(path string) (*Report, error) {
	img.mu.RLock()
	defer img.mu.RUnlock()
	rep := img.newReport(path)
	var issues []Issue
	actual := img.scanActualRefs(&issues)

	// 对比存储计数与真实计数；同时找出泄漏簇。
	for c := int64(firstDataCluster); c < img.highWater; c++ {
		stored := img.refCount[c]
		real := actual[c]
		switch {
		case stored < real:
			issues = append(issues, Issue{
				Kind: IssueRefUnder, Cluster: c,
				Detail: fmt.Sprintf("簇 %d 引用计数偏小: 存 %d 实际 %d", c, stored, real),
			})
		case stored > real:
			issues = append(issues, Issue{
				Kind: IssueRefOver, Cluster: c,
				Detail: fmt.Sprintf("簇 %d 引用计数偏大: 存 %d 实际 %d", c, stored, real),
			})
		}
		if real == 0 && stored == 0 && !img.isRootCluster(c) {
			// 高水位内、映射树不可达、计数为 0 的非根簇。
			// 已纳入受管空闲池 freeSet 的是合法可复用空间，不算问题；
			// 游离在 freeSet 之外的（崩溃孤儿）才报泄漏。
			// 需要把所有占用空间的不可达簇（含空闲簇）都列出时用 CheckLeaks。
			if _, free := img.freeSet[c]; !free {
				issues = append(issues, Issue{
					Kind: IssueLeak, Cluster: c,
					Detail: fmt.Sprintf("簇 %d 无任何引用，属于泄漏簇", c),
				})
			}
		}
	}
	// 高水位之外不允许有计数记录。
	for c := range img.refCount {
		if c >= img.highWater && img.refCount[c] != 0 {
			issues = append(issues, Issue{
				Kind: IssueCorruption, Cluster: c,
				Detail: fmt.Sprintf("簇 %d 超出文件高水位却有计数 %d", c, img.refCount[c]),
			})
		}
	}
	sortIssues(issues)
	rep.Issues = issues
	return rep, nil
}

// RecomputeRefCounts 重算并把正确计数写回磁盘（脏镜像打开时自动调用，
// 也可由检查工具手动调用）。返回的报告只描述计数偏差，不做泄漏回收。
func (img *Image) RecomputeRefCounts() (*Report, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	rep := img.newReport("")
	var issues []Issue
	actual := img.scanActualRefs(&issues)
	rep.Issues = issues
	// 按簇号顺序写回，保证确定性，也便于检查递归分配。
	keys := make([]int64, 0, len(actual))
	for c := range actual {
		keys = append(keys, c)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, c := range keys {
		if err := img.storeRefCount(c, actual[c]); err != nil {
			return nil, err
		}
	}
	// 对存储里有、实际为 0 的簇也写回 0（修复偏大计数）。
	for c, v := range img.refCount {
		if v == 0 {
			continue
		}
		if _, ok := actual[c]; !ok {
			if err := img.storeRefCount(c, 0); err != nil {
				return nil, err
			}
		}
	}
	if err := img.blk.Sync(); err != nil {
		return nil, err
	}
	return rep, nil
}

// Repair 扫描并修复镜像：
//   - 计数偏小/偏大：按真实引用数重写；
//   - 泄漏簇：计数写 0 并收进空闲集合，尾部连续空闲簇截断还空间；
//   - 结构损坏：无法安全自动修复，保留在报告里并返回错误。
// 修复结束后立即再跑一遍 Check，确认报告干净。
func (img *Image) Repair(path string) (*Report, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.readOnly {
		return nil, ErrReadOnly
	}
	rep := img.newReport(path)
	var issues []Issue
	actual := img.scanActualRefs(&issues)

	// 先判断是否存在不可自动修复的损坏。
	var corrupt []Issue
	for _, is := range issues {
		if is.Kind == IssueCorruption {
			corrupt = append(corrupt, is)
		}
	}
	if len(corrupt) > 0 {
		rep.Issues = issues
		return rep, fmt.Errorf("%w: 存在 %d 处结构损坏，需人工介入", ErrCorruption, len(corrupt))
	}

	// 1) 修正所有计数偏差。
	for c := int64(firstDataCluster); c < img.maxClusters; c++ {
		real := actual[c]
		stored := img.refCount[c]
		if stored == real {
			continue
		}
		if err := img.storeRefCount(c, real); err != nil {
			return nil, err
		}
		rep.Actions = append(rep.Actions, Issue{
			Cluster: c,
			Detail:  fmt.Sprintf("簇 %d 引用计数 %d -> %d", c, stored, real),
		})
	}

	// 2) 回收泄漏簇：实际无引用且不是根表簇。
	var leaks []int64
	for c := int64(firstDataCluster); c < img.highWater; c++ {
		if actual[c] == 0 && !img.isRootCluster(c) {
			leaks = append(leaks, c)
		}
	}
	for _, c := range leaks {
		if err := img.storeRefCount(c, 0); err != nil {
			return nil, err
		}
		img.freeSet[c] = struct{}{}
		rep.Actions = append(rep.Actions, Issue{
			Kind: IssueLeak, Cluster: c, Detail: fmt.Sprintf("回收泄漏簇 %d", c),
		})
	}

	// 3) 截断尾部连续空闲簇，把空间还给文件系统。
	for img.highWater > firstDataCluster {
		tail := img.highWater - 1
		if img.isRootCluster(tail) {
			break
		}
		if _, free := img.freeSet[tail]; !free && img.refCount[tail] != 0 {
			break
		}
		delete(img.freeSet, tail)
		delete(img.refCount, tail)
		img.highWater--
	}
	newSize := img.highWater * int64(img.clusterSize)
	if img.blk.Size() != newSize {
		if err := img.blk.Truncate(newSize); err != nil {
			return nil, err
		}
	}
	if err := img.blk.Sync(); err != nil {
		return nil, err
	}
	// 干净头部重新发布。
	img.dirty = false
	if err := img.commitCurrentHeader(); err != nil {
		return nil, err
	}

	// 4) 复检：必须干净。
	verify, err := img.checkLocked(path)
	if err != nil {
		return nil, err
	}
	if !verify.Clean() {
		return verify, corruptf("修复后复检仍有 %d 个问题", len(verify.Issues))
	}
	rep.Issues = nil
	rep.FileBytes = img.blk.Size()
	rep.HighWater = img.highWater
	return rep, nil
}

// checkLocked 是 Check 的不加锁版本，供 Repair 在持锁时复检。
func (img *Image) checkLocked(path string) (*Report, error) {
	rep := img.newReport(path)
	var issues []Issue
	actual := img.scanActualRefs(&issues)
	for c := int64(firstDataCluster); c < img.highWater; c++ {
		stored := img.refCount[c]
		real := actual[c]
		switch {
		case stored < real:
			issues = append(issues, Issue{Kind: IssueRefUnder, Cluster: c,
				Detail: fmt.Sprintf("簇 %d 引用计数偏小: 存 %d 实际 %d", c, stored, real)})
		case stored > real:
			issues = append(issues, Issue{Kind: IssueRefOver, Cluster: c,
				Detail: fmt.Sprintf("簇 %d 引用计数偏大: 存 %d 实际 %d", c, stored, real)})
		case real == 0 && stored == 0 && !img.isRootCluster(c):
			// 已登记进空闲集合的簇是合法可复用空间，不算泄漏；
			// 只有"文件里存在、映射树不可达、又没被登记为空闲"的才是崩溃孤儿。
			if _, free := img.freeSet[c]; !free {
				issues = append(issues, Issue{Kind: IssueLeak, Cluster: c,
					Detail: fmt.Sprintf("簇 %d 无任何引用，属于泄漏簇", c)})
			}
		}
	}
	sortIssues(issues)
	rep.Issues = issues
	return rep, nil
}

func sortIssues(issues []Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Kind != issues[j].Kind {
			return issues[i].Kind < issues[j].Kind
		}
		return issues[i].Cluster < issues[j].Cluster
	})
}
