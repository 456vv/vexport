// Command vexport extracts declarations from a Go package using a full
// go/types analysis, pulls their dependencies in recursively (optionally across
// package boundaries), rewrites import paths, splits the result along
// //go:build constraints and can verify the generated code with go build.
//
// Usage:
//
//	vexport run  -dir . -pkg ./internal/user -names User,ParseUser -o out/
//	vexport plan -dir . -pkg ./internal/user -all
//	vexport run  -dir . -pkg ./internal/user -names User \
//	               -targets linux/amd64,windows/amd64,darwin/arm64 -o gen -verify
//	vexport run  -dir . -pkg ./user -all -follow module -conflict prefix -o gen
//
// Targets are loaded in parallel (-jobs); renaming an inlined declaration also
// rewrites the call sites that use it.
//
// Exit codes: 0 ok, 1 load/type error, 2 target not found, 3 output error,
// 4 verification failed, 5 usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

const version = "2.0.0"

const (
	exitOK      = 0
	exitLoad    = 1
	exitMissing = 2
	exitOutput  = 3
	exitVerify  = 4
	exitUsage   = 5
)

func main() {
	os.Exit(cliMain(os.Args[1:], os.Stdout, os.Stderr))
}

func cliMain(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "version", "-version", "--version":
			fmt.Fprintf(stdout, "vexport %s\n", version)
			return exitOK
		}
	}

	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "run", "plan":
			cmd, args = args[0], args[1:]
		case "help":
			usage(stdout)
			return exitOK
		default:
			fmt.Fprintf(stderr, "vexport: unknown command %q\n\n", args[0])
			usage(stderr)
			return exitUsage
		}
	}

	cfg := DefaultConfig()
	fs := flag.NewFlagSet("vexport "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }

	nv := bindFlags(fs, cfg)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "vexport: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return exitUsage
	}
	if err := nv.apply(cfg); err != nil {
		fmt.Fprintf(stderr, "vexport: %v\n", err)
		return exitUsage
	}
	if cfg.AutoTargets {
		rep, err := discoverTargets(cfg)
		if err != nil {
			fmt.Fprintf(stderr, "vexport: %v\n", err)
			return exitLoad
		}
		cfg.Targets = rep.Targets
		if !cfg.Quiet {
			names := make([]string, 0, len(rep.Targets))
			for _, t := range rep.Targets {
				names = append(names, t.String())
			}
			fmt.Fprintf(stderr, "vexport: note: -targets auto discovered %d target(s) from %d file(s): %s\n",
				len(rep.Targets), rep.Scanned, strings.Join(names, ", "))
			if len(rep.CustomTags) > 0 {
				fmt.Fprintf(stderr, "vexport: warning: custom build tags need explicit opt-in via "+
					"-targets os/arch+tag: %s\n", strings.Join(rep.CustomTags, ", "))
			}
			if len(rep.Skipped) > 0 {
				fmt.Fprintf(stderr, "vexport: warning: %d file(s) matched no candidate target "+
					"(likely //go:build ignore): %s\n", len(rep.Skipped), strings.Join(rep.Skipped, ", "))
			}
		}
	}
	if err := cfg.Normalize(); err != nil {
		fmt.Fprintf(stderr, "vexport: %v\n", err)
		return exitUsage
	}
	// FIX(P1-MN-02): -dry-run never writes source to stdout, so the JSON
	// report can safely use the default output.
	if cfg.JSON && (cfg.Out == "" || cfg.Out == "-") && cmd != "plan" && !cfg.DryRun {
		fmt.Fprintln(stderr, "vexport: -json and stdout output collide; use -o DIR or -dry-run")
		return exitUsage
	}

	p := New(cfg)
	if cmd == "plan" {
		p.StopAfter = "resolve"
	}
	if cfg.Verbose {
		p.OnStage = func(name string, d time.Duration, err error) {
			status := "ok"
			if err != nil {
				status = err.Error()
			}
			fmt.Fprintf(stderr, "vexport: stage %-8s %8s  %s\n", name,
				d.Round(time.Millisecond), status)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	st, err := p.Run(ctx)
	code := classify(st, err)

	if cfg.JSON {
		if jerr := writeJSONReport(stdout, buildReport(st, cfg)); jerr != nil {
			fmt.Fprintf(stderr, "vexport: %v\n", jerr)
			return exitOutput
		}
	} else {
		reportOut := stdout
		// 源码已经写到 stdout。摘要若再写 stdout，管道里就不是合法 Go 文件。
		if wroteStdout(st) {
			reportOut = stderr
		}
		if cmd == "plan" {
			writePlan(stdout, st)
		} else {
			writeHumanReport(reportOut, st, cfg, err == nil)
		}
	}

	if !cfg.Quiet {
		for _, w := range st.Warnings {
			fmt.Fprintf(stderr, "vexport: warning: %s\n", w)
		}
		for _, n := range st.Notes {
			fmt.Fprintf(stderr, "vexport: note: %s\n", n)
		}
		// FIX(P1-MN-04): -allow-partial used to hide missing names in the
		// human-readable report; only the JSON/plan output showed them.
		for _, name := range st.Missing {
			fmt.Fprintf(stderr, "vexport: missing: %s\n", name)
		}
		for _, sel := range st.Selections {
			for _, dep := range sel.ExternalDeps() {
				fmt.Fprintf(stderr, "vexport: note: result keeps an import of %s\n", dep)
			}
		}
	}

	if err != nil {
		fmt.Fprintf(stderr, "vexport: %v\n", err)
		var ve *VerifyError
		if errors.As(err, &ve) && !cfg.JSON {
			for _, r := range ve.Results {
				if r.OK {
					continue
				}
				fmt.Fprintf(stderr, "vexport: %s (%s):\n%s\n", r.Target, r.Command, indent(r.Output, "    "))
			}
		}
	}
	if cfg.Strict && code == exitOK {
		if n := externalCount(st); n > 0 {
			fmt.Fprintf(stderr, "vexport: -strict: %d external import(s) remain\n", n)
			code = exitLoad
		}
	}
	return code
}

// wroteStdout 报告本次是否把生成的 Go 写进了 stdout。
func wroteStdout(st *State) bool {
	if st == nil {
		return false
	}
	for _, f := range st.Files {
		if f.Path == "-" {
			return true
		}
	}
	return false
}

// classify maps a pipeline result to the documented exit codes.
func classify(st *State, err error) int {
	if err == nil {
		return exitOK
	}
	var miss *MissingError
	if errors.As(err, &miss) {
		return exitMissing
	}
	var verr *VerifyError
	if errors.As(err, &verr) {
		return exitVerify
	}
	if st != nil && len(st.Stages) > 0 {
		switch st.Stages[len(st.Stages)-1].Name {
		case "load", "resolve":
			return exitLoad
		case "emit", "write":
			return exitOutput
		case "verify":
			return exitVerify
		}
	}
	return exitLoad
}

func externalCount(st *State) int {
	set := map[string]bool{}
	for _, sel := range st.Selections {
		for _, dep := range sel.ExternalDeps() {
			set[dep] = true
		}
	}
	return len(set)
}

// ============================== flags ==============================

type flagValues struct {
	names    string
	follow   string
	rewrite  string
	targets  string
	split    string
	conflict string
}

func bindFlags(fs *flag.FlagSet, cfg *Config) *flagValues {
	nv := &flagValues{}
	fs.StringVar(&cfg.Dir, "dir", cfg.Dir, "模块或包目录")
	fs.StringVar(&cfg.Pattern, "pkg", "", "要加载的包（./internal/user 或导入路径）")
	fs.IntVar(&cfg.Jobs, "jobs", cfg.Jobs, "并行加载的 target 数（0 = 自动，1 = 串行）")
	fs.StringVar(&nv.names, "names", "", "逗号分隔的声明（允许 Type.Method，* = 所有导出声明）")
	fs.BoolVar(&cfg.All, "all", false, "-names * 的简写")

	fs.StringVar(&nv.targets, "targets", "", "构建矩阵：linux/amd64,windows/amd64,darwin/arm64+tag，或 auto 自动扫描")

	fs.BoolVar(&cfg.IncludeMethods, "methods", cfg.IncludeMethods, "同时复制所选类型的方法")
	fs.BoolVar(&cfg.IncludeCtors, "ctors", cfg.IncludeCtors, "同时复制所选类型的构造函数")
	fs.StringVar(&nv.follow, "follow", "", "递归内联依赖：module、all、std、!exclude、github.com/x/y、github.com/x/**")
	fs.IntVar(&cfg.FollowDepth, "follow-depth", cfg.FollowDepth, "跟随时跨包最大跳数（-1 = 不限，0 = 关闭）")
	fs.StringVar(&nv.rewrite, "rewrite", "", "导入路径重写：old=new,example.com/old/=example.com/new/")
	fs.StringVar(&nv.conflict, "conflict", string(cfg.Conflict), "名称冲突策略：keep（默认，如果冲突保留import） | prefix（加前缀名） | error（错误退出）")
	fs.IntVar(&cfg.MaxDecls, "max-decls", cfg.MaxDecls, "内联声明的硬限制（0 = 不限）")
	fs.BoolVar(&cfg.Strict, "strict", false, "生成结果含import 第三方包（当前 Go module 和标准库之外）时失败（文件已写入，退出码 1）")

	fs.StringVar(&cfg.Out, "o", "", "输出文件、目录，或 - 表示 stdout")
	fs.StringVar(&nv.split, "split", string(cfg.Split), "文件布局：auto|none|target")
	fs.StringVar(&cfg.Suffix, "suffix", cfg.Suffix, "生成文件的文件名后缀")
	fs.BoolVar(&cfg.ShortNames, "short-names", cfg.ShortNames, "文件名省略全局唯一的 GOOS 的 arch 后缀（_linux.go 而非 _linux_amd64.go）")
	fs.BoolVar(&cfg.SplitByPackage, "split-by-package", cfg.SplitByPackage, "按来源包拆分输出文件；内联包以包名作文件名前缀")
	fs.BoolVar(&cfg.KeepTags, "keep-tags", false, "始终写入 //go:build 行，即使一个文件覆盖所有目标")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "渲染并验证，但不写入")
	fs.BoolVar(&cfg.Force, "force", false, "覆盖非 vexport 生成的文件")
	fs.BoolVar(&cfg.AllowPartial, "allow-partial", false, "某些 -names 失败不存在的名字被跳过，其余名字照常输出")

	fs.BoolVar(&cfg.Verify.Enabled, "verify", false, "使用 go 工具链构建生成的文件")
	fs.StringVar(&cfg.Verify.Command, "verify-cmd", cfg.Verify.Command, "验证命令：build|vet")
	fs.BoolVar(&cfg.Verify.AllTargets, "verify-targets", cfg.Verify.AllTargets, "验证每个目标，而不只是宿主机")
	fs.BoolVar(&cfg.Verify.Keep, "verify-keep", false, "保留临时验证包")
	fs.DurationVar(&cfg.Verify.Timeout, "verify-timeout", cfg.Verify.Timeout, "单次验证构建的超时时间")

	fs.BoolVar(&cfg.JSON, "json", false, "在 stdout 输出机器可读报告")
	fs.BoolVar(&cfg.Quiet, "q", false, "抑制提示和警告")
	fs.BoolVar(&cfg.Verbose, "v", false, "记录每个阶段")
	return nv
}

func (nv *flagValues) apply(cfg *Config) error {
	cfg.Names = append(cfg.Names, splitList(nv.names)...)
	cfg.Follow = append(cfg.Follow, splitList(nv.follow)...)
	cfg.Rewrites = append(cfg.Rewrites, splitList(nv.rewrite)...)

	if nv.targets != "" {
		if strings.EqualFold(strings.TrimSpace(nv.targets), "auto") {
			cfg.AutoTargets = true
		} else {
			ts, err := ParseTargets([]string{nv.targets})
			if err != nil {
				return err
			}
			cfg.Targets = ts
		}
	}
	if nv.split != "" {
		m, err := ParseSplit(nv.split)
		if err != nil {
			return err
		}
		cfg.Split = m
	}
	if nv.conflict != "" {
		m, err := ParseConflict(nv.conflict)
		if err != nil {
			return err
		}
		cfg.Conflict = m
	}
	return nil
}

// ============================== plan output ==============================

func writePlan(w io.Writer, st *State) {
	for _, sel := range st.Selections {
		fmt.Fprintf(w, "plan for %s (package %s)\n", sel.Target, sel.PkgName)
		fmt.Fprintf(w, "  %d declaration(s) from %s\n", len(sel.Decls),
			strings.Join(sel.packages(), ", "))
		for _, line := range sel.Origin() {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	if len(st.Missing) > 0 {
		fmt.Fprintf(w, "missing: %s\n", strings.Join(st.Missing, ", "))
	}
}

// ============================== usage ==============================

func usage(w io.Writer) {
	name := filepath.Base(os.Args[0])
	fmt.Fprintf(w, `vexport %s - 类型感知的声明提取器

用法：
  %[2]s run  [flags]     提取、渲染、验证并写入
  %[2]s plan [flags]     仅加载并解析，打印将要复制的内容
  %[2]s version          打印版本
  %[2]s help             打印本帮助文本

输入：
  -dir DIR               模块或包目录（默认 "."）
  -pkg PATTERN           要加载的包，例如 ./internal/user
  -names LIST            逗号分隔的声明，允许 Type.Method，* = 所有导出声明
  -all                   -names * 的简写
  -jobs N                并行加载的 target 数（0 = 自动 = CPU 数，1 = 串行）

构建矩阵（每种不同渲染结果生成一个输出文件）：
   -targets LIST         linux/amd64,windows/amd64,darwin/arm64+tag,tag,auto
                         或 auto：扫描 -pkg 目录，推导最小覆盖集

选择：
  -methods               复制所选类型的方法（默认 true）
  -ctors                 复制所选类型的构造函数（默认 true）
  -follow LIST           递归内联依赖：module、all、std、!exclude、github.com/x/y、github.com/x/**
  -follow-depth N        跟随时跨包跳数（-1 不限，0 关闭，默认 8）
  -rewrite LIST          导入路径重写：old=new 或 example.com/old/=example.com/new/
  -conflict MODE         名称冲突策略：keep（默认，如果冲突保留import） | prefix（加前缀名） | error（错误退出）
  -max-decls N           内联声明的硬限制（默认 5000，0 = 不限）
  -strict                生成结果含 import 第三方包（当前 Go module 和标准库之外）时失败（文件已写入，退出码 1）

输出：
  -o PATH                输出文件、目录，或 - 表示 stdout（默认 stdout）
  -split MODE            auto（默认） | none | target
  -suffix S              文件名后缀
  -short-names           文件名省略全局唯一的 GOOS 的 arch 后缀
                         （name_linux.go 而非 name_linux_amd64.go）
  -split-by-package      按来源包拆分输出文件；内联包以包名作文件名前缀
  -keep-tags             始终写入 //go:build 行
  -force                 覆盖非 vexport 生成的文件
  -dry-run               渲染并验证，但不写入
  -allow-partial         某些 -names 失败不存在的名字被跳过，其余名字照常输出

验证：
  -verify                使用 go 工具链构建生成的文件
  -verify-cmd CMD        build（默认） | vet
  -verify-targets        验证每个目标（默认 true）
  -verify-keep           保留临时验证包
  -verify-timeout D      每次构建超时时间（默认 3m）

报告：
  -json                  在 stdout 输出机器可读报告
  -q                     抑制提示和警告
  -v                     记录每个阶段

退出码：0 成功，1 加载/类型错误，2 目标未找到，3 输出错误，
        4 验证失败，5 用法错误。
`, version, name)
}
