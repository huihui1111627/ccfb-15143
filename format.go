// Package vdisk 实现一个可叠层的虚拟磁盘镜像格式（mvdisk）。
//
// 镜像物理布局：
//
//	簇 0、簇 1：两个头部槽位（A/B 原子切换，任一槽位损坏都能用另一个打开）
//	簇 2 起   ：一级映射表(L1)、引用计数一级表(refL1)、以及动态分配的各数据/表簇
//
// 所有多字节整数均为大端定长编码。
package vdisk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc64"
)

// ---- 基础常量 ----

const (
	magic   = "MVDISK01"
	version = 1

	headerSlots     = 2 // 头部双槽位
	firstDataCluster = 2 // 簇 0/1 是头部

	minClusterShift = 9 // 512
	maxClusterShift = 20 // 1 MiB
	defaultShift    = 12 // 4 KiB

	// 每个表簇可容纳的表项数（表项统一 8 字节）在运行时由簇大小算出。
	entrySize = 8

	// 映射表项标志位，放在 64 位字的最高位。
	entryValid = uint64(1) << 63
	entryZero  = uint64(1) << 62
	// 映射表项低 62 位存放物理字节偏移（必须按簇对齐）。
	entryOffsetMask = uint64(1)<<62 - 1
	entryFlagsMask  = entryValid | entryZero

	// 引用计数项：最高 8 位为保留位（必须为零），低 56 位为计数。
	refReservedMask = uint64(0xFF00000000000000)
	refCountMask    = uint64(0x00FFFFFFFFFFFFFF)
	maxRefCount     = int64(refCountMask)
)

// 特性位。当前版本没有定义任何已实现的特性位：
// 不兼容位只要出现未知位就拒绝打开；兼容位出现未知位照常打开。
const (
	knownIncompatFeatures uint32 = 0
	knownCompatFeatures   uint32 = 0
)

// 扩展区 TLV 类型。
const (
	extBasePath = uint16(1) // 底包路径（UTF-8）
)

var crcTable = crc64.MakeTable(crc64.ISO)

// ---- 头部槽位定长布局（簇内偏移）----
const (
	hOffMagic          = 0  // 8 字节
	hOffVersion        = 8  // 1 字节
	hOffClusterShift   = 9  // 1 字节
	hOffDirty          = 10 // 1 字节
	hOffReserved       = 11 // 1 字节，必须为 0
	hOffIncompat       = 12 // 4 字节
	hOffCompat         = 16 // 4 字节
	hOffVirtualClusters = 20 // 8 字节
	hOffMaxClusters    = 28 // 8 字节
	hOffL1Offset       = 36 // 8 字节
	hOffL1Clusters     = 44 // 8 字节
	hOffRefL1Offset    = 52 // 8 字节
	hOffRefL1Clusters  = 60 // 8 字节
	hOffSequence       = 68 // 8 字节
	hOffCRC            = 76 // 8 字节，覆盖 [0,76)
	hOffExtLen         = 84 // 4 字节
	hOffExt            = 88 // 扩展区起点
)

// header 是解析后的镜像头。
type header struct {
	shift           uint
	dirty           bool
	incompat        uint32
	compat          uint32
	virtualClusters int64
	maxClusters     int64
	l1Offset        int64
	l1Clusters      int64
	refL1Offset     int64
	refL1Clusters   int64
	sequence        uint64
	ext             []byte // 原始 TLV 字节（含未知类型，原样保留）
}

func be64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }

func putBE64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }
func putBE32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func putBE16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }

// encodeSlot 把头部编码进一个整簇大小的缓冲区。
func (h *header) encodeSlot(clusterSize int) []byte {
	buf := make([]byte, clusterSize)
	copy(buf[hOffMagic:], magic)
	buf[hOffVersion] = version
	buf[hOffClusterShift] = byte(h.shift)
	if h.dirty {
		buf[hOffDirty] = 1
	}
	putBE32(buf[hOffIncompat:], h.incompat)
	putBE32(buf[hOffCompat:], h.compat)
	putBE64(buf[hOffVirtualClusters:], uint64(h.virtualClusters))
	putBE64(buf[hOffMaxClusters:], uint64(h.maxClusters))
	putBE64(buf[hOffL1Offset:], uint64(h.l1Offset))
	putBE64(buf[hOffL1Clusters:], uint64(h.l1Clusters))
	putBE64(buf[hOffRefL1Offset:], uint64(h.refL1Offset))
	putBE64(buf[hOffRefL1Clusters:], uint64(h.refL1Clusters))
	putBE64(buf[hOffSequence:], h.sequence)
	putBE32(buf[hOffExtLen:], uint32(len(h.ext)))
	copy(buf[hOffExt:], h.ext)
	crc := crc64.Checksum(buf[:hOffCRC], crcTable)
	putBE64(buf[hOffCRC:], crc)
	return buf
}

var (
	// ErrCorruption 表示镜像结构损坏（校验失败、保留位非零、偏移未对齐等）。
	ErrCorruption = errors.New("mvdisk: 镜像损坏")
	// ErrIncompatible 表示镜像带有所用软件不认识的不兼容特性位。
	ErrIncompatible = errors.New("mvdisk: 镜像含有不兼容特性，拒绝打开")
	// ErrReadOnly 表示对只读镜像（或只读底包）执行了写操作。
	ErrReadOnly = errors.New("mvdisk: 镜像以只读方式打开")
	// ErrNoSpace 表示物理簇数达到创建时约定的上限。
	ErrNoSpace = errors.New("mvdisk: 物理簇已达上限")
	// ErrPowerLoss 由断电模拟块在 Sync 时触发。
	ErrPowerLoss = errors.New("mvdisk: 模拟断电，未落盘的写全部丢失")
	// ErrBaseMissing 表示头部记录的底包路径当前无法打开（文件缺失/未挂载）。
	// Open 会把它降级为"无底包模式"继续打开，未映射区读全零，镜像本身仍可用，
	// 从而保证改底包路径后、底包尚未就位时镜像不会彻底打不开。
	ErrBaseMissing = errors.New("mvdisk: 底包缺失，按无底包模式打开")
)

func corruptf(format string, args ...any) error {
	return errors.Join(ErrCorruption, fmt.Errorf(format, args...))
}
