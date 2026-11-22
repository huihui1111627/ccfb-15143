package vdisk

import (
	"sort"
	"sync"
)

// PowerFile 模拟带易失写回缓存的磁盘：
//
//   - WriteAt 只落到内存覆盖层，不修改"磁盘"本体；
//   - Sync 把覆盖层整体写回磁盘本体（模拟缓存刷盘 + barrier）；
//   - PowerLoss 直接丢弃覆盖层，所有还没 Sync 的写全部消失。
//
// 重开镜像时用 DurableBlock() 取断电后的"磁盘"内容新建块即可。
// 这与真实断电语义一致：Sync 之前的写可能完全不存在，但不会出现半扇区撕裂。
type PowerFile struct {
	mu       sync.Mutex
	durable  Block            // 已落盘的磁盘本体
	pending  map[int64][]byte // 偏移 -> 待刷盘的数据块（不同偏移互不重叠）
	size     int64            // 逻辑大小（含未落盘的 Truncate）
	pendingN int64            // 未落盘写字节数（统计用）
	syncedN  int64            // 已落盘写字节数（统计用）
	lost     bool
	// failNextSync>0 时，接下来这么多次 Sync 不落盘、丢弃全部待写并返回断电错误，
	// 用于模拟写事务在提交点之前崩溃（产生只泄漏、不悬挂的孤儿簇）。
	failNextSync int
	// partialFail>0 时，下一次 Sync 先把待写按偏移排序、只落一部分到磁盘本体，
	// 随后断电丢弃其余。模拟"数据簇已刷盘、元数据指针或引用计数尚未提交"的
	// 真实崩溃窗口，按本库的写顺序不变量，这只会留下泄漏孤儿，不会产生悬挂引用。
	partialFail int
	// partialFraction 是部分落盘故障时保留的字节比例。
	partialFraction float64
	// partialKeepHigh 为 true 时保留高偏移字节（数据簇方向），丢弃低偏移（元数据），
	// 这正是产生"已落盘数据簇但无任何元数据引用"泄漏孤儿的崩溃点。
	partialKeepHigh bool
}

// NewPowerFile 用一个已存在的块作为磁盘本体创建易失缓存视图。
// 调用方在 PowerLoss 后可再次用残留块调用本函数模拟重开。
func NewPowerFile(durable Block) *PowerFile {
	return &PowerFile{durable: durable, pending: map[int64][]byte{}, size: durable.Size()}
}

// FailNextSync 安排接下来 n 次 Sync 模拟断电（丢弃未落盘写并返回 ErrPowerLoss）。
func (p *PowerFile) FailNextSync(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failNextSync += n
}

// FailPartialSync 安排下一次 Sync 在部分落盘后断电。
// keepFraction 取 [0,1]，决定待写字节保留的比例；keepHigh 为 true 时保留高偏移
// （数据簇）丢弃低偏移（元数据），制造泄漏孤儿。
func (p *PowerFile) FailPartialSync(keepFraction float64, keepHigh bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.partialFraction = keepFraction
	p.partialKeepHigh = keepHigh
	p.partialFail++
}

func (p *PowerFile) ReadAt(buf []byte, off int64) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 先读磁盘本体，再用覆盖层修补。
	if _, err := p.durable.ReadAt(buf, off); err != nil {
		return 0, err
	}
	for po, pd := range p.pending {
		pe := po + int64(len(pd))
		lo := max64(off, po)
		hi := min64(off+int64(len(buf)), pe)
		if lo < hi {
			copy(buf[lo-off:hi-off], pd[lo-po:hi-po])
		}
	}
	return len(buf), nil
}

func (p *PowerFile) WriteAt(data []byte, off int64) (int, error) {
	if off < 0 {
		return 0, corruptf("负偏移 %d", off)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := append([]byte(nil), data...)
	// 合并到相交的既有覆盖块，保持每个覆盖块内部连续。
	p.mergePending(off, cp)
	p.pendingN += int64(len(data))
	if end := off + int64(len(data)); end > p.size {
		p.size = end
	}
	return len(data), nil
}

// mergePending 把新写合并进覆盖层。与新范围相交的旧块按字节拼起来。
func (p *PowerFile) mergePending(off int64, data []byte) {
	start := off
	end := off + int64(len(data))
	mergedStart := start
	mergedEnd := end
	// 找出所有相交的旧覆盖块，确定合并后的范围。
	for po, pd := range p.pending {
		pe := po + int64(len(pd))
		if pe <= start || po >= end {
			continue
		}
		if po < mergedStart {
			mergedStart = po
		}
		if pe > mergedEnd {
			mergedEnd = pe
		}
	}
	merged := make([]byte, mergedEnd-mergedStart)
	// 先铺相交的旧数据，并把它们从覆盖层移除。
	for po, pd := range p.pending {
		pe := po + int64(len(pd))
		if pe <= mergedStart || po >= mergedEnd {
			continue
		}
		copy(merged[po-mergedStart:], pd)
		delete(p.pending, po)
	}
	copy(merged[start-mergedStart:], data)
	p.pending[mergedStart] = merged
}

func (p *PowerFile) Truncate(n int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n < 0 {
		return corruptf("负长度 %d", n)
	}
	// 模拟文件截断：丢弃 n 之后的覆盖数据。
	for po, pd := range p.pending {
		pe := po + int64(len(pd))
		if po >= n {
			delete(p.pending, po)
			continue
		}
		if pe > n {
			p.pending[po] = pd[:n-po]
		}
	}
	p.size = n
	return nil
}

// Sync 把全部待写内容刷到磁盘本体并 fsync。
func (p *PowerFile) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lost {
		return ErrPowerLoss
	}
	// 故障注入：在提交点之前断电，待写全部丢弃，磁盘本体保持上一次 Sync 的状态。
	if p.failNextSync > 0 {
		p.failNextSync--
		p.pending = map[int64][]byte{}
		p.pendingN = 0
		p.size = p.durable.Size()
		p.lost = true
		return ErrPowerLoss
	}
	// 先扩文件，再写数据，最后 fsync——作为单个原子刷盘点。
	if err := p.durable.Truncate(p.size); err != nil {
		return err
	}
	// 偏移排序后写，写放大统计里也能看到真实落盘顺序。
	offs := make([]int64, 0, len(p.pending))
	for po := range p.pending {
		offs = append(offs, po)
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })

	// 部分落盘故障：只把一定比例的待写字节刷到本体，其余随断电丢弃。
	if p.partialFail > 0 {
		p.partialFail--
		var total int64
		for _, po := range offs {
			total += int64(len(p.pending[po]))
		}
		budget := int64(float64(total) * p.partialFraction)
		var flushed int64
		if p.partialKeepHigh {
			// 从高偏移往低偏移刷：数据簇落盘、低偏移元数据丢失 -> 泄漏孤儿。
			for i := len(offs) - 1; i >= 0 && flushed < budget; i-- {
				po := offs[i]
				full := p.pending[po]
				keep := int64(len(full))
				if flushed+keep > budget {
					keep = budget - flushed
				}
				start := int64(len(full)) - keep
				if _, err := p.durable.WriteAt(full[start:], po+start); err != nil {
					return err
				}
				flushed += keep
			}
		} else {
			// 从低偏移往高偏移刷。
			for _, po := range offs {
				if flushed >= budget {
					break
				}
				pd := p.pending[po]
				keep := int64(len(pd))
				if flushed+keep > budget && budget > flushed {
					keep = budget - flushed
				}
				if keep > 0 {
					if _, err := p.durable.WriteAt(pd[:keep], po); err != nil {
						return err
					}
					flushed += keep
				}
			}
		}
		_ = p.durable.Sync()
		p.pending = map[int64][]byte{}
		p.pendingN = 0
		p.lost = true
		return ErrPowerLoss
	}

	var n int64
	for _, po := range offs {
		pd := p.pending[po]
		if _, err := p.durable.WriteAt(pd, po); err != nil {
			return err
		}
		n += int64(len(pd))
	}
	if err := p.durable.Sync(); err != nil {
		return err
	}
	p.pending = map[int64][]byte{}
	p.syncedN += n
	p.pendingN = 0
	return nil
}

// PowerLoss 模拟突然断电：覆盖层直接丢弃。
func (p *PowerFile) PowerLoss() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = map[int64][]byte{}
	p.pendingN = 0
	p.size = p.durable.Size()
	p.lost = true
}

// DurableBlock 返回断电/关闭后残留的磁盘本体。
func (p *PowerFile) DurableBlock() Block {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.durable
}

// PendingBytes 返回尚未刷盘的写字节数。
func (p *PowerFile) PendingBytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pendingN
}

// SyncedBytes 返回累计落盘字节数。
func (p *PowerFile) SyncedBytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.syncedN
}

func (p *PowerFile) Size() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.size
}

func (p *PowerFile) Close() error {
	// 正常关闭等价于一次完整刷盘。
	if err := p.Sync(); err != nil {
		return err
	}
	return p.durable.Close()
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
