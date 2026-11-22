package vdisk

// Close 正常关闭镜像：清脏标记、重发头部、刷盘后关闭底层块。
// 只读镜像不会重写头部。
func (img *Image) Close() error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.closed {
		return nil
	}
	img.closed = true
	if !img.readOnly {
		img.dirty = false
		if err := img.commitCurrentHeader(); err != nil {
			return err
		}
		if err := img.blk.Sync(); err != nil {
			return err
		}
	}
	return img.blk.Close()
}

// Sync 把当前所有写刷到底层块（写路径每次已自动 Sync，这里供显式调用）。
func (img *Image) Sync() error {
	img.mu.Lock()
	defer img.mu.Unlock()
	return img.blk.Sync()
}

// ReadOnly 返回镜像是否只读打开。
func (img *Image) ReadOnly() bool { return img.readOnly }

// BaseMissing 返回头部记录的底包当前是否缺失（已降级为无底包模式）。
func (img *Image) BaseMissing() bool {
	img.mu.RLock()
	defer img.mu.RUnlock()
	return img.baseMissing
}

// FileSize 返回底层镜像文件当前字节大小。
func (img *Image) FileSize() int64 {
	img.mu.RLock()
	defer img.mu.RUnlock()
	return img.blk.Size()
}

// Dirty 返回当前脏标记。
func (img *Image) Dirty() bool {
	img.mu.RLock()
	defer img.mu.RUnlock()
	return img.dirty
}

// DirtyOpenReport 返回脏镜像打开时自动重算引用计数的报告（若有）。
func (img *Image) DirtyOpenReport() *Report {
	img.mu.RLock()
	defer img.mu.RUnlock()
	return img.dirtyReport
}
