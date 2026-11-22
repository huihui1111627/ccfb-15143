package main

import (
	"fmt"
	"io"

	"mvdisk"
)

// powerCycleAndCheck 在某层写操作之后模拟断电：
//  1. 丢弃 PowerFile 未落盘的全部写；
//  2. 用残留块重新打开镜像（脏标记会触发自动重算引用计数）；
//  3. 整盘内容必须仍与暴力数组一致（未落盘的写本就不算数，
//     所以这里比对的是"断电前最近一次成功 Sync 的视图"——
//     为了让语义清晰，所有写操作在返回前都已 Sync）；
//  4. 检查工具报出的问题只允许是泄漏；修复后复检必须干净。
func (h *harness) powerCycleAndCheck(li int, sum *summary) error {
	// 写路径每次结尾都 Sync，因此当前视图里通常没有残留覆盖层。
	// 断电 = 丢弃这个临时视图（未落盘的写全部消失），磁盘本体保持不变；
	// 之后所有重开都基于本体新建干净视图，绝不复用已断电的视图。
	sum.powerLosses++

	img, err := h.openReadOnly(li)
	if err != nil {
		return fmt.Errorf("断电后重开层 %d 失败: %w", li, err)
	}
	// 脏打开应自动重算引用计数。
	if rep := img.DirtyOpenReport(); rep != nil {
		for _, is := range rep.Issues {
			if is.Kind == vdisk.IssueCorruption {
				return fmt.Errorf("层 %d 自动重算发现损坏: %s", li, is.Detail)
			}
		}
	}
	if err := h.verifyOpenImage(li, img); err != nil {
		return err
	}

	// 只读检查：问题类别只允许泄漏（断电可能留下未发布的孤儿簇）。
	rep, err := img.Check(h.layers[li].path)
	if err != nil {
		return err
	}
	for _, is := range rep.Issues {
		switch is.Kind {
		case vdisk.IssueLeak:
			sum.leaksFound++
		case vdisk.IssueCorruption:
			sum.corruptions++
			return fmt.Errorf("层 %d 检查到结构损坏: %s", li, is.Detail)
		case vdisk.IssueRefUnder, vdisk.IssueRefOver:
			// 自动重算后不应再有计数偏差；若有说明不变量被破坏。
			return fmt.Errorf("层 %d 断电后引用计数偏差: %s", li, is.Detail)
		}
	}
	if err := img.Close(); err != nil {
		return err
	}

	// 可写重开并修复；修复后复检必须干净，且内容仍与暴力数组一致。
	wimg, _, err := h.openWritable(li)
	if err != nil {
		return err
	}
	fixRep, err := wimg.Repair(h.layers[li].path)
	if err != nil {
		return fmt.Errorf("层 %d 修复失败: %w (报告: %+v)", li, err, fixRep)
	}
	if err := h.verifyOpenImage(li, wimg); err != nil {
		return err
	}
	post, err := wimg.Check(h.layers[li].path)
	if err != nil {
		return err
	}
	if !post.Clean() {
		return fmt.Errorf("层 %d 修复后复检仍有 %d 个问题: %+v", li, len(post.Issues), post.Issues)
	}
	return wimg.Close()
}

// verifyOpenImage 对一个已打开的镜像做整盘逐字节比对。
func (h *harness) verifyOpenImage(li int, img *vdisk.Image) error {
	got := make([]byte, h.virtSz)
	if _, err := img.ReadAt(got, 0); err != nil && err != io.EOF {
		return err
	}
	return h.compare(li, got)
}

// finalVerify 场景收尾：逐层逐字节比对；检查只允许出现泄漏（空闲簇），
// 不允许损坏或引用计数偏差；随后修复并复检必须干净，再统计修复后的文件大小。
func (h *harness) finalVerify(sum *summary) error {
	for li := 0; li < len(h.layers); li++ {
		img, err := h.openReadOnly(li)
		if err != nil {
			return err
		}
		if err := h.verifyOpenImage(li, img); err != nil {
			_ = img.Close()
			return err
		}
		rep, err := img.Check(h.layers[li].path)
		if err != nil {
			_ = img.Close()
			return err
		}
		for _, is := range rep.Issues {
			switch is.Kind {
			case vdisk.IssueLeak:
				sum.leaksFound++
			case vdisk.IssueCorruption, vdisk.IssueRefUnder, vdisk.IssueRefOver:
				_ = img.Close()
				return fmt.Errorf("层 %d 收尾出现非泄漏问题: %+v", li, is)
			}
		}
		if err := img.Close(); err != nil {
			return err
		}

		// 可写重开修复，复检必须干净，内容保持一致。
		wimg, _, err := h.openWritable(li)
		if err != nil {
			return err
		}
		if _, err := wimg.Repair(h.layers[li].path); err != nil {
			return fmt.Errorf("层 %d 收尾修复失败: %w", li, err)
		}
		if err := h.verifyOpenImage(li, wimg); err != nil {
			_ = wimg.Close()
			return err
		}
		post, err := wimg.Check(h.layers[li].path)
		if err != nil {
			_ = wimg.Close()
			return err
		}
		if !post.Clean() {
			_ = wimg.Close()
			return fmt.Errorf("层 %d 收尾修复后复检不干净: %+v", li, post.Issues)
		}
		sum.fileBytes += wimg.FileSize()
		if err := wimg.Close(); err != nil {
			return err
		}
	}
	return nil
}
