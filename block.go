package vdisk

import (
	"io"
	"os"
)

// Block 是镜像文件的底层块接口。镜像库只通过这个接口读写字节，
// 这样既可以接真实文件，也可以接带断电模拟的内存块做测试。
type Block interface {
	// ReadAt 语义与 os.File.ReadAt 一致：读满 len(p)，越界部分返回零。
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Size() int64
	Truncate(n int64) error
	Sync() error
	Close() error
}

// fileBlock 包装一个真实 os.File。
type fileBlock struct {
	f *os.File
}

// CreateFile 创建一个新的镜像文件（必须不存在）。
func CreateFile(path string) (Block, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	return &fileBlock{f: f}, nil
}

// OpenFile 打开已有镜像文件。readOnly 为 true 时以只读方式打开。
func OpenFile(path string, readOnly bool) (Block, error) {
	flag := os.O_RDWR
	if readOnly {
		flag = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, err
	}
	return &fileBlock{f: f}, nil
}

func (b *fileBlock) ReadAt(p []byte, off int64) (int, error) {
	n, err := b.f.ReadAt(p, off)
	// 读到文件尾之后的洞/未写区域按全零处理，和普通镜像文件的稀疏区一致。
	if err == io.EOF && n < len(p) {
		for i := n; i < len(p); i++ {
			p[i] = 0
		}
		return len(p), nil
	}
	return n, err
}

func (b *fileBlock) WriteAt(p []byte, off int64) (int, error) {
	return b.f.WriteAt(p, off)
}

func (b *fileBlock) Size() int64 {
	st, err := b.f.Stat()
	if err != nil {
		return 0
	}
	return st.Size()
}

func (b *fileBlock) Truncate(n int64) error { return b.f.Truncate(n) }
func (b *fileBlock) Sync() error           { return b.f.Sync() }
func (b *fileBlock) Close() error          { return b.f.Close() }
