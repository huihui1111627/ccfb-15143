package vdisk

import (
	"fmt"
	"sync"
)

// Image 是一个可读写的镜像层。多层镜像通过 base 串成链。
type Image struct {
	mu sync.RWMutex

	blk       Block // 本层块（可能包了统计/断电模拟）
	sb        *statsBlock
	stats     *Stats
	logicalR  int64
	logicalW  int64
	readOnly  bool

	shift       uint
	clusterSize int
	entriesPer  int64 // 每个表簇能放多少个 8 字节表项

	virtualClusters int64
	maxClusters     int64

	l1       []int64 // 一级映射表项：虚拟簇号 -> L2 表簇号（0 = 未分配）
	refL1    []int64 // 引用计数一级表：物理簇号 -> refL2 表簇号（0 = 未分配）
	refCount map[int64]int64 // 本层内存中的引用计数（物理簇号 -> 计数）

	// 根表物理簇起点（连续预留）。布局：L1(A) → refL1(B) → refL2(B)。
	l1Start    int64
	refL1Start int64
	refL2Start int64

	// 物理簇分配器：highWater 以下的簇要么被引用，要么在 freeSet 里。
	highWater int64
	freeSet   map[int64]struct{}

	base     *Image // 只读底包（可为 nil）
	basePath string
	// baseMissing 表示头部记录了底包路径，但当前打不开（缺失/未挂载），
	// 已降级为无底包模式：未映射区读全零，镜像仍可读写、可再改底包路径。
	baseMissing bool
	// headerExt 保留头部扩展区的全部 TLV（含未知类型），重写头部时原样带回。
	headerExt []extRecord
	// dirtyReport 记录脏镜像打开时自动重算引用计数的结果。
	dirtyReport *Report

	dirty    bool
	sequence uint64

	// 特性位随镜像持久化，重写头部时原样带回（库自身只识别 0 号版本的特性集合）。
	incompatFeatures uint32
	compatFeatures   uint32

	closed bool
}

// Options 是 Create 的参数。
type Options struct {
	ClusterShift    uint   // 簇大小 = 1 << ClusterShift，范围 [9,20]，0 表示默认 12（4KiB）
	VirtualSize     int64  // 虚拟磁盘字节大小（会向上取整到簇边界）
	BasePath        string // 底包路径（记录在头部扩展区，可为空）
	IncompatFeature uint32 // 不兼容特性位（调用方自己定义；库只认 0）
	CompatFeature   uint32 // 兼容特性位
}

// extRecord 是扩展区里的一个 TLV 项。
type extRecord struct {
	typ uint16
	buf []byte
}

func encodeExt(recs []extRecord) []byte {
	n := 0
	for _, r := range recs {
		n += 2 + 4 + len(r.buf)
	}
	out := make([]byte, n)
	pos := 0
	for _, r := range recs {
		putBE16(out[pos:], r.typ)
		putBE32(out[pos+2:], uint32(len(r.buf)))
		copy(out[pos+6:], r.buf)
		pos += 6 + len(r.buf)
	}
	return out
}

// parseExt 解析扩展区；长度自洽性由调用方保证，这里只做边界检查。
// 不认识的类型原样保留在返回值里。
func parseExt(b []byte) ([]extRecord, error) {
	var recs []extRecord
	pos := 0
	for pos < len(b) {
		if pos+6 > len(b) {
			return nil, corruptf("扩展区 TLV 头被截断（偏移 %d）", pos)
		}
		typ := be16(b[pos:])
		l := int(be32(b[pos+2:]))
		pos += 6
		if pos+l > len(b) {
			return nil, corruptf("扩展区 TLV(type=%d) 长度 %d 越界", typ, l)
		}
		recs = append(recs, extRecord{typ: typ, buf: append([]byte(nil), b[pos:pos+l]...)})
		pos += l
	}
	return recs, nil
}

func extGet(recs []extRecord, typ uint16) string {
	for _, r := range recs {
		if r.typ == typ {
			return string(r.buf)
		}
	}
	return ""
}

func extSet(recs []extRecord, typ uint16, value string) []extRecord {
	out := recs[:0:0]
	found := false
	for _, r := range recs {
		if r.typ == typ {
			out = append(out, extRecord{typ: typ, buf: []byte(value)})
			found = true
		} else {
			out = append(out, r)
		}
	}
	if !found {
		out = append(out, extRecord{typ: typ, buf: []byte(value)})
	}
	return out
}

// validateShift 校验簇大小位移。
func validateShift(shift uint) error {
	if shift < minClusterShift || shift > maxClusterShift {
		return fmt.Errorf("mvdisk: 簇位移 %d 超出范围 [%d,%d]", shift, minClusterShift, maxClusterShift)
	}
	return nil
}

// planCapacity 计算创建时需要约定的物理簇上限，以及 L1、refL1 各占多少簇。
//
// 记 V=虚拟簇数，E=每簇表项数，A=ceil(V/E)。最坏情况下：
//
//	头部 2 簇 + L1 根表 A 簇 + refL1 根表 B=ceil(P/E) 簇
//	+ L2 表至多 A 簇 + 数据簇至多 V 个 + refL2 表至多 B 簇
//
// 即 P = 2 + V + 2A + 2*ceil(P/E)。该式对 P 单调，从下界起迭代到不动点即可
// （E>=64，系数 2/E<1，必然收敛）。
func planCapacity(virtualClusters, entriesPer int64) (maxClusters, l1Clusters, refL1Clusters int64) {
	l1Clusters = (virtualClusters + entriesPer - 1) / entriesPer
	if l1Clusters == 0 {
		l1Clusters = 1
	}
	base := firstDataCluster + virtualClusters + 2*l1Clusters
	p := base // 下界：先不计两张 ref 根/二级表
	for {
		b := (p + entriesPer - 1) / entriesPer
		next := base + 2*b
		if next == p {
			refL1Clusters = b
			maxClusters = p
			return maxClusters, l1Clusters, refL1Clusters
		}
		p = next
	}
}
