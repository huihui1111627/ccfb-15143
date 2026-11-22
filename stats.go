package vdisk

import "sync/atomic"

// Stats 是一个镜像层的读写放大与占用统计。
type Stats struct {
	// 虚拟盘逻辑读写字节数（用户视角）。
	LogicalReadBytes  int64
	LogicalWriteBytes int64
	// 底层块实际读写字节数（含元数据、COW、整簇写等）。
	PhysicalReadBytes  int64
	PhysicalWriteBytes int64
	// Sync 次数（每次写操作结尾一次，另含头部提交等）。
	SyncCount int64
}

// ReadAmp = 物理读 / 逻辑读；WriteAmp = 物理写 / 逻辑写。
func (s *Stats) ReadAmp() float64 {
	if s.LogicalReadBytes == 0 {
		return 0
	}
	return float64(s.PhysicalReadBytes) / float64(s.LogicalReadBytes)
}

func (s *Stats) WriteAmp() float64 {
	if s.LogicalWriteBytes == 0 {
		return 0
	}
	return float64(s.PhysicalWriteBytes) / float64(s.LogicalWriteBytes)
}

// statsBlock 包装 Block 计数物理 IO。
type statsBlock struct {
	b         Block
	readBytes  *atomic.Int64
	writeBytes *atomic.Int64
	syncs      *atomic.Int64
}

func (s *statsBlock) ReadAt(p []byte, off int64) (int, error) {
	n, err := s.b.ReadAt(p, off)
	s.readBytes.Add(int64(n))
	return n, err
}

func (s *statsBlock) WriteAt(p []byte, off int64) (int, error) {
	n, err := s.b.WriteAt(p, off)
	s.writeBytes.Add(int64(n))
	return n, err
}

func (s *statsBlock) Size() int64              { return s.b.Size() }
func (s *statsBlock) Truncate(n int64) error  { return s.b.Truncate(n) }
func (s *statsBlock) Sync() error {
	s.syncs.Add(1)
	return s.b.Sync()
}
func (s *statsBlock) Close() error { return s.b.Close() }

// wrapStats 给镜像的底层块套上物理 IO 计数。
func (img *Image) wrapStats(blk Block) {
	var pr, pw, ps atomic.Int64
	img.stats = &Stats{
		PhysicalReadBytes:  0,
		PhysicalWriteBytes: 0,
	}
	sb := &statsBlock{b: blk, readBytes: &pr, writeBytes: &pw, syncs: &ps}
	img.blk = sb
	// Stats 结构里直接放 atomic 的读取入口，Stats() 时快照。
	img.sb = sb
}
