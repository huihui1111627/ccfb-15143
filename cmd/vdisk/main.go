// vdisk 是 mvdisk 镜像的命令行工具：创建、检查、修复、检视页面、读写与零标记。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"mvdisk"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// reorderArgs 把位置参数挪到所有标志之后，使标准 flag 包能接受
// "vdisk check <路径> --base-dir x" 这种路径在前、标志在后的自然写法。
// 带值标志（-flag value）与布尔标志（-flag）都能正确识别。
func reorderArgs(args []string, boolFlags map[string]bool) []string {
	var positional, flags []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 0 && a[0] == '-' {
			name := a
			if idx := indexByte(a, '='); idx >= 0 {
				name = a[:idx]
			}
			flags = append(flags, a)
			// 形如 -flag value 且不是布尔标志、也没用等号赋值：把值一并收走。
			if indexByte(a, '=') < 0 && !boolFlags[trimDash(name)] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			positional = append(positional, a)
		}
	}
	return append(flags, positional...)
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func trimDash(s string) string {
	for len(s) > 0 && s[0] == '-' {
		s = s[1:]
	}
	return s
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("缺少子命令")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "create":
		return cmdCreate(rest)
	case "check":
		return cmdCheck(rest, false)
	case "repair":
		return cmdCheck(rest, true)
	case "serve":
		return cmdServe(rest)
	case "read":
		return cmdRead(rest)
	case "write":
		return cmdWrite(rest)
	case "zero":
		return cmdZero(rest)
	case "set-base":
		return cmdSetBase(rest)
	case "stats":
		return cmdStats(rest)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("未知子命令 %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mvdisk — 叠层虚拟磁盘镜像工具

用法:
  vdisk create <镜像路径> --size <字节> [--cluster-shift 9..20] [--base <底包路径>]
  vdisk check  <镜像路径> [--base-dir <目录>]
  vdisk repair <镜像路径> [--base-dir <目录>]
  vdisk serve  <镜像路径> --addr :8080 [--base-dir <目录>]
  vdisk read   <镜像路径> --off <偏移> --len <字节> [--out 文件]
  vdisk write  <镜像路径> --off <偏移> --in 文件
  vdisk zero   <镜像路径> --off <偏移> --len <字节>
  vdisk set-base <镜像路径> --base <新底包路径>
  vdisk stats  <镜像路径>
`)
}

// openChain 按头部扩展区的底包路径逐层只读打开；顶层按 writable 打开。
func openChain(topPath string, writable bool, baseDir string) (*vdisk.Image, []vdisk.ChainNode, error) {
	loader := func(p string) (*vdisk.Image, error) {
		bp := joinPath(baseDir, p)
		b, err := vdisk.OpenFile(bp, true)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// 底包缺失：降级为无底包模式，让顶层镜像仍能打开。
				return nil, errors.Join(vdisk.ErrBaseMissing, err)
			}
			return nil, err
		}
		return vdisk.Open(b, true, nil)
	}
	topBlock, err := vdisk.OpenFile(topPath, !writable)
	if err != nil {
		return nil, nil, err
	}
	img, err := vdisk.Open(topBlock, !writable, loader)
	if err != nil {
		return nil, nil, err
	}
	chain := []vdisk.ChainNode{{
		Path: topPath, BasePath: img.BasePath(),
		ReadOnly: img.ReadOnly(), Dirty: img.Dirty(),
	}}
	return img, chain, nil
}

func joinPath(dir, p string) string {
	if dir == "" {
		return p
	}
	return dir + string(os.PathSeparator) + p
}

func cmdCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	size := fs.Int64("size", 0, "虚拟盘字节大小")
	shift := fs.Uint("cluster-shift", 0, "簇位移（9=512B .. 20=1MiB），默认 12=4KiB")
	base := fs.String("base", "", "底包路径（写入头部扩展区）")
	if err := fs.Parse(reorderArgs(args, map[string]bool{})); err != nil {
		return err
	}
	if fs.NArg() != 1 || *size <= 0 {
		return errors.New("用法: vdisk create <路径> --size <字节>")
	}
	blk, err := vdisk.CreateFile(fs.Arg(0))
	if err != nil {
		return err
	}
	img, err := vdisk.Create(blk, vdisk.Options{
		ClusterShift: *shift, VirtualSize: *size, BasePath: *base,
	})
	if err != nil {
		return err
	}
	return img.Close()
}

func cmdCheck(args []string, repair bool) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	baseDir := fs.String("base-dir", "", "底包搜索目录")
	if err := fs.Parse(reorderArgs(args, map[string]bool{})); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("用法: vdisk check|repair <镜像路径>")
	}
	path := fs.Arg(0)
	img, _, err := openChain(path, repair, *baseDir)
	if err != nil {
		return err
	}
	defer img.Close()
	var rep *vdisk.Report
	if repair {
		rep, err = img.Repair(path)
	} else {
		rep, err = img.Check(path)
	}
	if err != nil {
		printReport(rep)
		return err
	}
	printReport(rep)
	if rep.Clean() {
		fmt.Println("结果: 干净")
	} else {
		fmt.Printf("结果: %d 个问题\n", len(rep.Issues))
	}
	return nil
}

func printReport(rep *vdisk.Report) {
	if rep == nil {
		return
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "监听地址")
	baseDir := fs.String("base-dir", "", "底包搜索目录")
	if err := fs.Parse(reorderArgs(args, map[string]bool{})); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("用法: vdisk serve <镜像路径> --addr :8080")
	}
	path := fs.Arg(0)
	img, chain, err := openChain(path, true, *baseDir)
	if err != nil {
		return err
	}
	// 补上底包链节点（库未暴露 base 指针，这里按扩展区路径补展示行）。
	if bp := img.BasePath(); bp != "" {
		chain = append([]vdisk.ChainNode{{Path: joinPath(*baseDir, bp), BasePath: "", ReadOnly: true}}, chain...)
	}
	srv := vdisk.NewServer(img, path, chain)
	fmt.Printf("检视页面: http://localhost%s/\n", *addr)
	return srv.ListenAndServe(*addr)
}

func cmdRead(args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	off := fs.Int64("off", 0, "起始偏移")
	length := fs.Int64("len", 0, "读取字节数")
	out := fs.String("out", "", "输出文件（默认 stdout）")
	baseDir := fs.String("base-dir", "", "底包搜索目录")
	if err := fs.Parse(reorderArgs(args, map[string]bool{})); err != nil {
		return err
	}
	if fs.NArg() != 1 || *length <= 0 {
		return errors.New("用法: vdisk read <路径> --off N --len N [--out 文件]")
	}
	img, _, err := openChain(fs.Arg(0), false, *baseDir)
	if err != nil {
		return err
	}
	defer img.Close()
	buf := make([]byte, *length)
	n, err := img.ReadAt(buf, *off)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if *out == "" {
		_, err = os.Stdout.Write(buf[:n])
		return err
	}
	return os.WriteFile(*out, buf[:n], 0o644)
}

func cmdWrite(args []string) error {
	fs := flag.NewFlagSet("write", flag.ContinueOnError)
	off := fs.Int64("off", 0, "起始偏移")
	in := fs.String("in", "", "写入内容文件")
	if err := fs.Parse(reorderArgs(args, map[string]bool{})); err != nil {
		return err
	}
	if fs.NArg() != 1 || *in == "" {
		return errors.New("用法: vdisk write <路径> --off N --in 文件")
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	img, _, err := openChain(fs.Arg(0), true, "")
	if err != nil {
		return err
	}
	defer img.Close()
	if _, err := img.WriteAt(data, *off); err != nil {
		return err
	}
	fmt.Printf("已写入 %d 字节到偏移 %d\n", len(data), *off)
	return nil
}

func cmdZero(args []string) error {
	fs := flag.NewFlagSet("zero", flag.ContinueOnError)
	off := fs.Int64("off", 0, "起始偏移")
	length := fs.Int64("len", 0, "长度")
	if err := fs.Parse(reorderArgs(args, map[string]bool{})); err != nil {
		return err
	}
	if fs.NArg() != 1 || *length <= 0 {
		return errors.New("用法: vdisk zero <路径> --off N --len N")
	}
	img, _, err := openChain(fs.Arg(0), true, "")
	if err != nil {
		return err
	}
	defer img.Close()
	if err := img.ZeroAt(*off, *length); err != nil {
		return err
	}
	fmt.Printf("已把 [%d,%d) 标为全零\n", *off, *off+*length)
	return nil
}

func cmdSetBase(args []string) error {
	fs := flag.NewFlagSet("set-base", flag.ContinueOnError)
	base := fs.String("base", "", "新底包路径")
	if err := fs.Parse(reorderArgs(args, map[string]bool{})); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("用法: vdisk set-base <路径> --base <新路径>")
	}
	img, _, err := openChain(fs.Arg(0), true, "")
	if err != nil {
		return err
	}
	defer img.Close()
	return img.SetBasePath(*base)
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	baseDir := fs.String("base-dir", "", "底包搜索目录")
	if err := fs.Parse(reorderArgs(args, map[string]bool{})); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("用法: vdisk stats <路径>")
	}
	img, _, err := openChain(fs.Arg(0), false, *baseDir)
	if err != nil {
		return err
	}
	defer img.Close()
	// 触发一次全量读以便统计读放大。
	full := make([]byte, img.ClusterSize())
	for off := int64(0); off < img.VirtualSize(); off += int64(img.ClusterSize()) {
		if _, err := img.ReadAt(full, off); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	st := img.Stats()
	b, _ := json.MarshalIndent(map[string]any{
		"logicalReadBytes":  st.LogicalReadBytes,
		"logicalWriteBytes": st.LogicalWriteBytes,
		"physicalReadBytes": st.PhysicalReadBytes,
		"physicalWriteBytes": st.PhysicalWriteBytes,
		"syncCount":         st.SyncCount,
		"readAmp":           st.ReadAmp(),
		"writeAmp":          st.WriteAmp(),
	}, "", "  ")
	fmt.Println(string(b))
	return nil
}
