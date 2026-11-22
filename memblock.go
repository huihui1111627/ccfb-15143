package vdisk

import (
	"sync"
)

// MemBlock 是纯内存块，用于单元测试；行为与稀疏文件一致：
// 未写过的区域读出来全零，越界读也返回零。
type MemBlock struct {
	mu   sync.Mutex
	data []byte
}

// NewMemBlock 创建初始大小为 size 的内存块。
func NewMemBlock(size int64) *MemBlock {
	return &MemBlock{data: make([]byte, size)}
}

func (m *MemBlock) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range p {
		pos := off + int64(i)
		if pos >= 0 && pos < int64(len(m.data)) {
			p[i] = m.data[pos]
		} else {
			p[i] = 0
		}
	}
	return len(p), nil
}

func (m *MemBlock) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, corruptf("负偏移 %d", off)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	end := off + int64(len(p))
	if end > int64(len(m.data)) {
		grown := make([]byte, end)
		copy(grown, m.data)
		m.data = grown
	}
	copy(m.data[off:end], p)
	return len(p), nil
}

func (m *MemBlock) Size() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.data))
}

func (m *MemBlock) Truncate(n int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n < 0 {
		return corruptf("负长度 %d", n)
	}
	if int64(len(m.data)) == n {
		return nil
	}
	grown := make([]byte, n)
	copy(grown, m.data) // 缩小时尾部自然丢弃
	m.data = grown
	return nil
}

func (m *MemBlock) Sync() error  { return nil }
func (m *MemBlock) Close() error { return nil }
