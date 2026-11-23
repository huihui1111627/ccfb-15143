// benchmark 命令：在四种仿真分布上对比学习型索引与 B+ 树。
//
// 用法：
//
//	go run ./cmd/benchmark -n 2000000 -eps 8,32,128 -queries 100000
//	go run ./cmd/benchmark -n 100000000 -profile timestamps   # 大内存机器
//
// 十亿级（n=1e9）仅 8 字节键就需 8GB 常驻内存，应在大内存机器运行或分片；
// 算法复杂度与数据量无关，结论可线性外推（见 README）。
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"learnedindex/internal/bench"
	"learnedindex/internal/data"
)

func main() {
	n := flag.Int("n", 2_000_000, "每种分布的键数量")
	epsFlag := flag.String("eps", "8,32,128", "逗号分隔的误差上限列表，展示内存/时延取舍")
	queries := flag.Int("queries", 100_000, "点查次数（范围查为其 1/4）")
	profile := flag.String("profile", "", "只跑某一种分布，默认全部")
	inserts := flag.Int("inserts", 100_000, "插入吞吐测试的插入键数")
	csvOut := flag.String("csv", "", "可选：把结果写入 CSV")
	seed := flag.Int64("seed", 20261123, "随机种子")
	flag.Parse()

	epsList := parseEps(*epsFlag)
	profiles := data.Profiles()
	if *profile != "" {
		profiles = []data.Profile{data.Profile(*profile)}
	}

	var csvRows [][]string
	if *csvOut != "" {
		csvRows = append(csvRows,
			[]string{"profile", "index", "index_bytes", "layers", "leaf_segments", "build_ms",
				"point_avg_ns", "point_p50_ns", "point_p99_ns", "range_avg_ns",
				"window_mean", "window_p99", "route_mean", "fallbacks", "correct_errors"})
	}

	for _, p := range profiles {
		fmt.Printf("\n==================== 分布: %s  n=%d ====================\n", p, *n)
		keys := data.Generate(p, *n, *seed)
		wl := data.GenWorkload(p, keys, *queries, *seed)
		fmt.Printf("不同键数: %d  重复占比: %.2f%%\n",
			countUnique(keys), 100*(1-float64(countUnique(keys))/float64(len(keys))))

		// B+ 树与数组二分与 eps 无关，每分布只构建/评估一次。
		btRep := bench.TreeReport(keys, wl)
		arrRep := bench.ArrayReport(keys, wl)
		printReport(p, btRep)
		printReport(p, arrRep)
		if *csvOut != "" {
			csvRows = append(csvRows, reportToCSV(p, btRep), reportToCSV(p, arrRep))
		}

		for _, eps := range epsList {
			r := bench.LearnedReport(keys, eps, wl)
			printReport(p, r)
			if *csvOut != "" {
				csvRows = append(csvRows, reportToCSV(p, r))
			}
		}

		eps := epsList[len(epsList)/2]
		base := keys[:len(keys)-*inserts]
		insKeys := data.Generate(p, *inserts, *seed+7)
		lrep, brep := bench.RunInserts(p, base, insKeys, eps, 64, 0.15)
		fmt.Println("---- 插入吞吐 ----")
		printInsert(p, lrep)
		printInsert(p, brep)
		if *csvOut != "" {
			csvRows = append(csvRows, insertToCSV(p, lrep), insertToCSV(p, brep))
		}
	}

	if *csvOut != "" {
		f, err := os.Create(*csvOut)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		w := csv.NewWriter(f)
		_ = w.WriteAll(csvRows)
		w.Flush()
		fmt.Printf("\nCSV 已写入 %s\n", *csvOut)
	}
}

func parseEps(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && v >= 0 {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		out = []int{32}
	}
	return out
}

func countUnique(keys []int64) int {
	if len(keys) == 0 {
		return 0
	}
	u := 1
	for i := 1; i < len(keys); i++ {
		if keys[i] != keys[i-1] {
			u++
		}
	}
	return u
}

func mb(b int64) float64 { return float64(b) / 1024 / 1024 }

func printReport(p data.Profile, r *bench.IndexReport) {
	fmt.Printf("[%-10s] %-18s 索引=%8.2fMB 层=%d 叶段=%d 构建=%.1fms\n",
		p, r.Name, mb(r.IndexBytes), r.Layers, r.LeafSegments, r.BuildMS)
	fmt.Printf("             点查 avg=%7.1fns p50=%7.1fns p99=%7.1fns | 范围查 avg=%8.1fns\n",
		r.PointAvgNS, r.PointP50NS, r.PointP99NS, r.RangeAvgNS)
	fmt.Printf("             搜索窗口 mean=%6.2f p99=%d 上层检查mean=%6.2f 回退=%d 正确性错误=%d\n\n",
		r.WindowMean, r.WindowP99, r.RouteMean, r.Fallbacks, r.CorrectErrors)
}

func printInsert(p data.Profile, r *bench.InsertReport) {
	fmt.Printf("[%-10s] %-22s 吞吐=%9.1f K/s 索引=%8.2fMB 段=%d 重拟合=%d 分裂=%d 路由重建=%d 错误=%d 总数=%d\n",
		p, r.Name, r.InsertKPS, mb(r.IndexBytes), r.Segments, r.Refits, r.Splits,
		r.RouterRebuilds, r.LookupErrors, r.FinalCount)
}

func reportToCSV(p data.Profile, r *bench.IndexReport) []string {
	return []string{
		string(p), r.Name, strconv.FormatInt(r.IndexBytes, 10),
		strconv.Itoa(r.Layers), strconv.Itoa(r.LeafSegments),
		fmt.Sprintf("%.3f", r.BuildMS),
		fmt.Sprintf("%.2f", r.PointAvgNS), fmt.Sprintf("%.2f", r.PointP50NS),
		fmt.Sprintf("%.2f", r.PointP99NS), fmt.Sprintf("%.2f", r.RangeAvgNS),
		fmt.Sprintf("%.3f", r.WindowMean), strconv.Itoa(r.WindowP99),
		fmt.Sprintf("%.3f", r.RouteMean), strconv.FormatInt(r.Fallbacks, 10),
		strconv.Itoa(r.CorrectErrors),
	}
}

func insertToCSV(p data.Profile, r *bench.InsertReport) []string {
	return []string{
		string(p), r.Name, strconv.FormatInt(r.IndexBytes, 10),
		"", strconv.Itoa(r.Segments), "",
		fmt.Sprintf("%.2f", r.InsertKPS), "", "", "", "", "", "",
		strconv.FormatInt(r.Refits, 10), strconv.Itoa(r.LookupErrors),
	}
}
