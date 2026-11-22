package main

import (
	"errors"
	"fmt"
	"io"

	"mvdisk"
)

// openWritable 以可写方式打开某层，并按扩展区路径把下面各层只读挂好。
// 返回镜像与本次打开使用的临时易失视图（调用方可在写之前安排故障注入）。
func (h *harness) openWritable(li int) (*vdisk.Image, *vdisk.PowerFile, error) {
	loader := h.makeLoader(li)
	pf := h.layers[li].view()
	img, err := vdisk.Open(pf, false, loader)
	return img, pf, err
}

// makeLoader 为第 li 层构造底包加载器：按相对层号向下找内存块。
func (h *harness) makeLoader(li int) vdisk.BaseLoader {
	return func(path string) (*vdisk.Image, error) {
		var k int
		if _, err := fmt.Sscanf(path, "layer-%d.mv", &k); err != nil {
			return nil, err
		}
		if k < 0 || k >= len(h.layers) || k >= li {
			return nil, fmt.Errorf("底包路径 %s 越界（当前层 %d）", path, li)
		}
		return vdisk.Open(h.layers[k].view(), true, h.makeLoader(k))
	}
}

// openReadOnly 只读打开一层（用于检查/比对）。
func (h *harness) openReadOnly(li int) (*vdisk.Image, error) {
	return vdisk.Open(h.layers[li].view(), true, h.makeLoader(li))
}

// crashResult 记录一次写操作是否在提交点断电。
type crashResult struct {
	crashed bool
}

// armCrash 在易失视图上安排一次提交点断电。
// 约 1/3 整笔丢弃；其余部分落盘，并在"保留高偏移(数据簇)/保留低偏移(元数据)"
// 两种崩溃点间随机，前者制造泄漏孤儿、后者验证悬挂引用永不出现。
func (h *harness) armCrash(pf *vdisk.PowerFile) {
	switch h.rng.Intn(3) {
	case 0:
		pf.FailNextSync(1)
	default:
		frac := 0.2 + 0.6*h.rng.Float64()
		// 多数部分落盘走 keep-high，确保能稳定产生可被检查工具发现的泄漏簇。
		pf.FailPartialSync(frac, h.rng.Intn(4) != 0)
	}
}

// randomWrite 生成一次随机写，约一半概率跨簇且起止不对齐。
// crash 为 true 时安排本次写在 Sync 提交点断电（写不生效，但只能产生泄漏孤儿）。
func (h *harness) randomWrite(li int, crash bool, sum *summary) (crashResult, error) {
	off := h.rng.Int63n(h.virtSz)
	maxLen := h.virtSz - off
	length := 1 + h.rng.Int63n(maxLen)
	// 偏向制造不对齐/跨簇：起点常常不在簇边界。
	if h.rng.Intn(2) == 0 && off+1 < h.virtSz {
		off++
		if length > 1 {
			length--
		}
	}
	data := make([]byte, length)
	_, _ = h.rng.Read(data)

	img, pf, err := h.openWritable(li)
	if err != nil {
		return crashResult{}, err
	}
	if crash {
		h.armCrash(pf)
	}
	_, werr := img.WriteAt(data, off)
	if crash {
		// 提交点断电：本次写丢弃，暴力数组保持上一次成功 Sync 的状态。
		sum.crashes++
		return crashResult{crashed: true}, h.discardAfterCrash(img, werr)
	}
	if werr != nil {
		return crashResult{}, werr
	}
	h.snapshotClusters(li, off, length)
	copy(h.brute[li][off:off+length], data)
	sum.writes++
	sum.logicalW += length
	return crashResult{}, h.closeAndCollect(img, sum)
}

// fullClusterWrite 整簇写。
func (h *harness) fullClusterWrite(li int, crash bool, sum *summary) (crashResult, error) {
	vc := h.rng.Int63n(h.virtSz / int64(h.clusterSz))
	data := make([]byte, h.clusterSz)
	_, _ = h.rng.Read(data)
	off := vc * int64(h.clusterSz)
	img, pf, err := h.openWritable(li)
	if err != nil {
		return crashResult{}, err
	}
	if crash {
		h.armCrash(pf)
	}
	_, werr := img.WriteAt(data, off)
	if crash {
		sum.crashes++
		return crashResult{crashed: true}, h.discardAfterCrash(img, werr)
	}
	if werr != nil {
		return crashResult{}, werr
	}
	h.snapshotClusters(li, off, int64(h.clusterSz))
	copy(h.brute[li][off:], data)
	sum.writes++
	sum.logicalW += int64(len(data))
	return crashResult{}, h.closeAndCollect(img, sum)
}

// randomZero 在随机区间打全零标记，同时更新暴力数组。
func (h *harness) randomZero(li int, crash bool, sum *summary) (crashResult, error) {
	off := h.rng.Int63n(h.virtSz)
	length := 1 + h.rng.Int63n(h.virtSz-off)
	img, pf, err := h.openWritable(li)
	if err != nil {
		return crashResult{}, err
	}
	if crash {
		h.armCrash(pf)
	}
	zerr := img.ZeroAt(off, length)
	if crash {
		sum.crashes++
		return crashResult{crashed: true}, h.discardAfterCrash(img, zerr)
	}
	if zerr != nil {
		return crashResult{}, zerr
	}
	h.snapshotClusters(li, off, length)
	for i := off; i < off+length; i++ {
		h.brute[li][i] = 0
	}
	sum.zeros++
	sum.logicalW += length
	return crashResult{}, h.closeAndCollect(img, sum)
}

// discardAfterCrash 在注入断电后收尾：写接口必须返回 ErrPowerLoss，
// 视图已死直接丢弃（不 Close，避免触发又一次 Sync）。
func (h *harness) discardAfterCrash(img *vdisk.Image, werr error) error {
	if !errors.Is(werr, vdisk.ErrPowerLoss) {
		return fmt.Errorf("故障注入未产生预期的断电错误，got %v", werr)
	}
	_ = img
	return nil
}

// switchBase 修改底包路径，改完再改回真实路径；
// 专门验证"整段重写头部、断电可开、未知 TLV 保留"。
func (h *harness) switchBase(li int, sum *summary) error {
	img, _, err := h.openWritable(li)
	if err != nil {
		return err
	}
	orig := img.BasePath()
	fake := "backup/renamed-layer.mv"
	if err := img.SetBasePath(fake); err != nil {
		return err
	}
	if err := img.SetBasePath(orig); err != nil {
		return err
	}
	sum.baseSwitches++
	return h.closeAndCollect(img, sum)
}

// closeAndCollect 关闭镜像并累计物理写。
func (h *harness) closeAndCollect(img *vdisk.Image, sum *summary) error {
	st := img.Stats()
	sum.physicalW += st.PhysicalWriteBytes
	return img.Close()
}

// verifyLayer 整盘读出与暴力数组逐字节比对。
func (h *harness) verifyLayer(li int) error {
	img, err := h.openReadOnly(li)
	if err != nil {
		return err
	}
	defer img.Close()
	got := make([]byte, h.virtSz)
	if _, err := img.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return h.compare(li, got)
}

// compare 逐字节比对，不一致时给出首个差异位置。
func (h *harness) compare(li int, got []byte) error {
	want := h.effectiveBrute(li)
	if len(got) != len(want) {
		return fmt.Errorf("层 %d 长度不一致: got %d want %d", li, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("层 %d 字节 %d 不一致: got %#x want %#x", li, i, got[i], want[i])
		}
	}
	return nil
}

// snapshotClusters 对 [off,off+length) 覆盖到的每个虚拟簇，若本层还没拥有它，
// 先把"站在本层往下看"的当前整簇内容快照进本层 brute 并标记整簇 touched。
// 与镜像的簇级 COW 语义一致：部分写会冻结写时刻的底包内容。
func (h *harness) snapshotClusters(li int, off, length int64) {
	cs := int64(h.clusterSz)
	vcStart := off / cs
	vcEnd := (off + length - 1) / cs
	for vc := vcStart; vc <= vcEnd; vc++ {
		lo, hi := vc*cs, (vc+1)*cs
		owned := false
		for x := lo; x < hi; x++ {
			if h.touched[li][x] {
				owned = true
				break
			}
		}
		if owned {
			continue
		}
		var below []byte
		if li > 0 {
			below = h.effectiveBrute(li - 1)
		} else {
			below = make([]byte, h.virtSz)
		}
		copy(h.brute[li][lo:hi], below[lo:hi])
		for x := lo; x < hi; x++ {
			h.touched[li][x] = true
		}
	}
}

// effectiveBrute 沿镜像链从底到顶，按簇级 COW 遮蔽合并出某层的最终视图。
// 每个虚拟簇取拥有它的最高层的整簇内容；无人拥有则为全零。
func (h *harness) effectiveBrute(li int) []byte {
	out := make([]byte, h.virtSz)
	cs := int64(h.clusterSz)
	for vc := int64(0); vc < h.virtSz/cs; vc++ {
		owner := -1
		for k := 0; k <= li; k++ {
			lo, hi := vc*cs, (vc+1)*cs
			for x := lo; x < hi; x++ {
				if h.touched[k][x] {
					owner = k
					break
				}
			}
		}
		if owner >= 0 {
			copy(out[vc*cs:(vc+1)*cs], h.brute[owner][vc*cs:(vc+1)*cs])
		}
	}
	return out
}
