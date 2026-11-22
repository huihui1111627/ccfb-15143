// stress 用固定随机种子生成大量镜像链与读写序列，
// 每一步都把虚拟磁盘整盘读出，和一个直接存内容的暴力数组逐字节比对；
// 每次写之后随机断电重开，并运行检查/修复工具。
//
// 用法:
//
//	go run ./cmd/stress -seed 20261123 -scenarios 40
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"

	"mvdisk"
)

type layer struct {
	durable vdisk.Block // 断电后仍残留的"磁盘本体"（内存块）
	path    string
}

// view 基于磁盘本体新建一个干净的易失缓存视图。
// 每个视图只在一次打开期间使用，断电即丢弃，绝不复用。
func (l *layer) view() *vdisk.PowerFile {
	return vdisk.NewPowerFile(l.durable)
}

// harness 持有一条镜像链和每层的暴力内容数组。
type harness struct {
	rng       *rand.Rand
	clusterSz int
	virtSz    int64
	layers    []*layer
	// brute[i] 是第 i 层虚拟盘的"应有内容"（视角为该层读到的最终内容）。
	brute [][]byte
	// touched[i][x] 标记层 i 的字节 x 是否被本层写/零标记覆盖过（簇级 COW 用）。
	touched [][]bool
}

func main() {
	seed := flag.Int64("seed", 20261123, "固定随机种子")
	n := flag.Int("scenarios", 40, "生成的镜像链场景数")
	flag.Parse()

	fmt.Printf("stress: seed=%d scenarios=%d\n", *seed, *n)
	total := summary{}
	for s := 0; s < *n; s++ {
		rng := rand.New(rand.NewSource(*seed + int64(s)*1000003))
		if err := runScenario(rng, s, &total); err != nil {
			fmt.Fprintf(os.Stderr, "场景 %d 失败: %v\n", s, err)
			os.Exit(1)
		}
	}
	total.print()
	fmt.Println("全部场景通过：逐字节比对一致，断电后检查只报泄漏，修复后复检干净。")
}

type summary struct {
	writes       int
	zeros        int
	baseSwitches int
	powerLosses  int
	crashes      int
	leaksFound   int
	corruptions  int
	logicalW     int64
	physicalW    int64
	fileBytes    int64
}

func (s *summary) print() {
	fmt.Println("-------- 汇总 --------")
	fmt.Printf("写操作 %d，零标记 %d，改底包 %d，冷启动重开 %d 次，提交点断电 %d 次\n",
		s.writes, s.zeros, s.baseSwitches, s.powerLosses, s.crashes)
	fmt.Printf("检查到泄漏簇 %d，结构损坏 %d（损坏必须为 0）\n", s.leaksFound, s.corruptions)
	if s.logicalW > 0 {
		fmt.Printf("总逻辑写 %d B，总物理写 %d B，平均写放大 %.2fx\n",
			s.logicalW, s.physicalW, float64(s.physicalW)/float64(s.logicalW))
	}
	fmt.Printf("收尾镜像文件总大小 %d B\n", s.fileBytes)
}

// runScenario 构造一条 1~4 层的镜像链并跑一串随机操作。
func runScenario(rng *rand.Rand, idx int, sum *summary) error {
	shifts := []uint{9, 10, 12, 13}
	shift := shifts[rng.Intn(len(shifts))]
	clusterSz := 1 << shift
	// 虚拟盘保持小而精：8~40 个簇，保证各种路径都能走到。
	vClusters := 8 + rng.Intn(33)
	virtSz := int64(vClusters * clusterSz)
	depth := 1 + rng.Intn(4)

	h := &harness{
		rng: rng, clusterSz: clusterSz, virtSz: virtSz,
	}
	// 建链：底层先建，上层依次把下一层路径写进扩展区。
	for li := 0; li < depth; li++ {
		mem := vdisk.NewMemBlock(0)
		basePath := ""
		if li > 0 {
			basePath = fmt.Sprintf("layer-%d.mv", li-1)
		}
		img, err := vdisk.Create(vdisk.NewPowerFile(mem), vdisk.Options{
			ClusterShift: shift, VirtualSize: virtSz, BasePath: basePath,
		})
		if err != nil {
			return fmt.Errorf("layer %d create: %w", li, err)
		}
		if err := img.Close(); err != nil {
			return err
		}
		h.layers = append(h.layers, &layer{durable: mem, path: fmt.Sprintf("layer-%d.mv", li)})
		h.brute = append(h.brute, make([]byte, virtSz))
		h.touched = append(h.touched, make([]bool, virtSz))
	}

	// 每轮在随机层上做随机操作。
	ops := 60 + rng.Intn(120)
	for op := 0; op < ops; op++ {
		li := rng.Intn(depth)
		kind := rng.Intn(100)
		// 约 20% 的写类操作在提交点注入断电，制造真实的泄漏孤儿。
		crash := rng.Intn(100) < 20
		var crashed bool
		switch {
		case kind <= 54:
			// 55% 随机写（含跨簇不对齐写）。
			res, err := h.randomWrite(li, crash, sum)
			if err != nil {
				return err
			}
			crashed = res.crashed
		case kind <= 69:
			// 15% 写零。
			res, err := h.randomZero(li, crash, sum)
			if err != nil {
				return err
			}
			crashed = res.crashed
		case kind <= 74:
			// 5% 整簇写（走新簇直挂快路径）。
			res, err := h.fullClusterWrite(li, crash, sum)
			if err != nil {
				return err
			}
			crashed = res.crashed
		case kind <= 84:
			// 10% 改底包路径。
			if err := h.switchBase(li, sum); err != nil {
				return err
			}
		default:
			// 15% 纯读校验。
			if err := h.verifyLayer(li); err != nil {
				return err
			}
		}

		if crashed {
			// 提交点断电：本次写未生效，从磁盘本体冷启动做完整恢复 + 检查 + 修复。
			if err := h.powerCycleAndCheck(li, sum); err != nil {
				return fmt.Errorf("op %d 断电恢复失败(层%d): %w", op, li, err)
			}
			continue
		}
		// 正常写后立即整盘逐字节比对。
		if kind <= 74 {
			if err := h.verifyLayer(li); err != nil {
				return fmt.Errorf("op %d 写后即时校验失败(层%d): %w", op, li, err)
			}
		}
		// 再以一定概率做一次"已落盘后"的冷启动重开校验。
		if kind <= 74 && rng.Intn(100) < 30 {
			if err := h.powerCycleAndCheck(li, sum); err != nil {
				return err
			}
		}
	}

	// 场景收尾：全部层正常关闭，整盘逐字节比对 + 检查必须干净。
	if err := h.finalVerify(sum); err != nil {
		return err
	}
	fmt.Printf("场景 %2d: 深度=%d 簇=%3d 簇大小=%5dB  ✓\n", idx, depth, vClusters, clusterSz)
	return nil
}
