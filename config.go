package main

import (
	"fmt"
	"path"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ============================== targets ==============================

// Target is one entry of the build matrix. Tags behave like -tags a,b.
type Target struct {
	GOOS   string
	GOARCH string
	Tags   []string
}

func (t Target) String() string {
	s := t.GOOS + "/" + t.GOARCH
	if len(t.Tags) > 0 {
		s += "+" + strings.Join(t.Tags, ",")
	}
	return s
}

// slug is the file name fragment used for this target.
func (t Target) slug() string {
	s := t.GOOS
	if t.GOARCH != "" {
		s += "_" + t.GOARCH
	}
	if len(t.Tags) > 0 {
		s += "_" + strings.Join(t.Tags, "_")
	}
	return s
}

// Constraint renders the target as a //go:build expression.
func (t Target) Constraint() string {
	var parts []string
	if t.GOOS != "" {
		parts = append(parts, t.GOOS)
	}
	if t.GOARCH != "" {
		parts = append(parts, t.GOARCH)
	}
	parts = append(parts, t.Tags...)
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return "(" + strings.Join(parts, " && ") + ")"
}

// Env returns the environment overrides for this target.
func (t Target) Env() []string {
	var env []string
	if t.GOOS != "" {
		env = append(env, "GOOS="+t.GOOS)
	}
	if t.GOARCH != "" {
		env = append(env, "GOARCH="+t.GOARCH)
	}
	if len(t.Tags) > 0 {
		env = append(env, "GOFLAGS=-tags="+strings.Join(t.Tags, ","))
	}
	return env
}

func hostTarget() Target { return Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH} }

// ParseTarget accepts "host", "linux", "linux/amd64", "linux/arm64+tag1,tag2".
func ParseTarget(s string) (Target, error) {
	raw := s
	s = strings.TrimSpace(s)

	t := Target{}
	if i := strings.Index(s, "+"); i >= 0 {
		t.Tags = uniqueSorted(splitList(s[i+1:]))
		s = strings.TrimSpace(s[:i])
	}

	if s == "" || strings.EqualFold(s, "host") {
		t.GOOS = runtime.GOOS
		t.GOARCH = runtime.GOARCH
	} else {
		osPart, archPart, hasArch := strings.Cut(s, "/")
		t.GOOS = strings.TrimSpace(osPart)
		if hasArch {
			t.GOARCH = strings.TrimSpace(archPart)
		}
		if t.GOOS == "" {
			t.GOOS = runtime.GOOS
		}
		if t.GOARCH == "" {
			t.GOARCH = runtime.GOARCH
		}
	}

	if !validIdent(t.GOOS) || !validIdent(t.GOARCH) {
		return Target{}, fmt.Errorf("bad target %q", raw)
	}
	for _, tag := range t.Tags {
		if !validIdent(tag) {
			return Target{}, fmt.Errorf("bad build tag %q in %q", tag, raw)
		}
	}
	return t, nil
}

func ParseTargets(list []string) ([]Target, error) {
	var out []Target
	seen := map[string]bool{}

	for _, raw := range list {
		var cur string
		flush := func() error {
			if cur == "" {
				return nil
			}
			t, err := ParseTarget(cur)
			if err != nil {
				return err
			}
			cur = ""
			if seen[t.String()] {
				return nil
			}
			seen[t.String()] = true
			out = append(out, t)
			return nil
		}
		for _, part := range splitList(raw) {
			if cur == "" {
				cur = part
				continue
			}
			if strings.ContainsAny(part, "/+") || isTargetShorthand(part) {
				if err := flush(); err != nil {
					return nil, err
				}
				cur = part
				continue
			}
			if strings.Contains(cur, "+") {
				cur += "," + part
				continue
			}
			if err := flush(); err != nil {
				return nil, err
			}
			cur = part
		}
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		out = []Target{hostTarget()}
	}
	return out, nil
}

// isTargetShorthand reports whether a bare comma-separated segment is more
// likely to be a target than a build tag. Custom tags that happen to equal a
// GOOS/GOARCH name should be written with an explicit '+' target.
func isTargetShorthand(s string) bool {
	return strings.EqualFold(s, "host") || knownOS[s] || knownArch[s]
}

// ============================== modes ==============================

type SplitMode string

const (
	// SplitAuto writes a single untagged file when every target renders
	// identically, and one tagged file per distinct rendering otherwise.
	SplitAuto SplitMode = "auto"
	// SplitNone always writes exactly one file, without build constraints.
	SplitNone SplitMode = "none"
	// SplitTarget always writes one file per distinct rendering.
	SplitTarget SplitMode = "target"
)

func ParseSplit(s string) (SplitMode, error) {
	switch SplitMode(strings.ToLower(strings.TrimSpace(s))) {
	case SplitAuto:
		return SplitAuto, nil
	case SplitNone:
		return SplitNone, nil
	case SplitTarget:
		return SplitTarget, nil
	}
	return "", fmt.Errorf("bad -split %q (want auto|none|target)", s)
}

type ConflictMode string

const (
	// ConflictKeep leaves a name clash alone: the declaration is not inlined
	// and the package stays an import.
	ConflictKeep ConflictMode = "keep"
	// ConflictPrefix renames the inlined declaration to PkgName+Name.
	ConflictPrefix ConflictMode = "prefix"
	// ConflictError aborts the pipeline.
	ConflictError ConflictMode = "error"
)

func ParseConflict(s string) (ConflictMode, error) {
	switch ConflictMode(strings.ToLower(strings.TrimSpace(s))) {
	case ConflictKeep:
		return ConflictKeep, nil
	case ConflictPrefix:
		return ConflictPrefix, nil
	case ConflictError:
		return ConflictError, nil
	}
	return "", fmt.Errorf("bad -conflict %q (want keep|prefix|error)", s)
}

// Rewrite is one import path rewrite rule. A From ending in "/" is a prefix
// rewrite, everything else is an exact replacement.
type Rewrite struct {
	From string
	To   string
}

func (r Rewrite) Apply(p string) (string, bool) {
	if strings.HasSuffix(r.From, "/") {
		if after, ok := strings.CutPrefix(p, r.From); ok {
			return r.To + after, true
		}
		return p, false
	}
	if p == r.From {
		return r.To, true
	}
	return p, false
}

// parseRewrites keeps the longest rule first so specific rules win.
func parseRewrites(list []string) ([]Rewrite, error) {
	var out []Rewrite
	for _, raw := range list {
		for _, item := range splitList(raw) {
			from, to, ok := strings.Cut(item, "=")
			if !ok || strings.TrimSpace(from) == "" {
				return nil, fmt.Errorf("bad -rewrite %q (want old=new)", item)
			}
			out = append(out, Rewrite{From: strings.TrimSpace(from), To: strings.TrimSpace(to)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].From) > len(out[j].From) })
	return out, nil
}

// ============================== config ==============================

type VerifyConfig struct {
	Enabled    bool
	Command    string // build|vet
	AllTargets bool
	Keep       bool
	Timeout    time.Duration
}

type Config struct {
	// input
	Dir     string
	Pattern string
	Names   []string
	All     bool
	Jobs    int // targets loaded concurrently; 0 = auto, 1 = sequential

	// build matrix
	Targets []Target

	AutoTargets bool

	// selection
	IncludeMethods bool
	IncludeCtors   bool
	Follow         []string
	FollowDepth    int // package hops; -1 = unlimited, 0 = no inlining
	FollowStd      bool
	Rewrites       []string
	Conflict       ConflictMode
	MaxDecls       int // hard stop against runaway inlining, 0 = unlimited
	Strict         bool

	// output
	Out            string
	Split          SplitMode
	Suffix         string // 文件名后缀
	KeepTags       bool
	ShortNames     bool // 当GOOS在输出中是唯一的时，省略arch后缀
	SplitByPackage bool // 按来源包拆分输出文件；内联包以包名作文件名前缀
	DryRun         bool
	Force          bool // 覆盖不是 vexport 生成的文件（-force）
	AllowPartial   bool // 尽管某些名称无法解析，但仍写入输出

	// verify
	Verify VerifyConfig

	// reporting
	JSON    bool
	Quiet   bool
	Verbose bool

	rewrites   []Rewrite
	normalized bool
}

func DefaultConfig() *Config {
	return &Config{
		Dir:            ".",
		IncludeMethods: true,
		IncludeCtors:   true,
		FollowDepth:    8,
		Conflict:       ConflictKeep,
		Split:          SplitAuto,
		MaxDecls:       5000,
		Verify: VerifyConfig{
			Command:    "build",
			AllTargets: true,
			Timeout:    180 * time.Second,
		},
	}
}

// loadWorkers is the number of targets loaded at the same time. Every target
// load runs a "go list" of its own, so the useful count is bounded by the
// number of CPUs; the caller can still force a value with -jobs.
func (c *Config) loadWorkers(targets int) int {
	if targets <= 1 {
		return 1
	}
	workers := c.Jobs
	switch {
	case workers == 0:
		workers = runtime.NumCPU()
	case workers < 0:
		workers = 1 // rejected by Normalize, kept defensive
	}
	if workers > targets {
		workers = targets
	}
	return workers
}

// Normalize validates the config and fills in defaults. It is idempotent.
func (c *Config) Normalize() error {
	if c.normalized {
		return nil
	}
	if c.Dir == "" {
		c.Dir = "."
	}
	if len(c.Targets) == 0 && !c.AutoTargets {
		c.Targets = []Target{hostTarget()}
	}
	for _, t := range c.Targets {
		if !validIdent(t.GOOS) || !validIdent(t.GOARCH) {
			return fmt.Errorf("bad target %s", t)
		}
	}
	if c.FollowDepth < -1 {
		return fmt.Errorf("-follow-depth must be -1 (unlimited) or >= 0, got %d", c.FollowDepth)
	}
	if c.Jobs < 0 {
		return fmt.Errorf("-jobs must be >= 0 (0 = auto), got %d", c.Jobs)
	}
	rw, err := parseRewrites(c.Rewrites)
	if err != nil {
		return err
	}
	c.rewrites = rw

	if c.All && !containsName(c.Names, "*") {
		c.Names = append(c.Names, "*")
	}
	if len(c.Names) == 0 {
		return fmt.Errorf("no -names given (use -names Type,Func or -all)")
	}
	if c.Pattern == "" {
		return fmt.Errorf("no -pkg given")
	}
	switch c.Verify.Command {
	case "":
		c.Verify.Command = "build"
	case "build", "vet":
	default:
		return fmt.Errorf("bad -verify-cmd %q (want build|vet)", c.Verify.Command)
	}
	if c.Verify.Timeout <= 0 {
		c.Verify.Timeout = 180 * time.Second
	}
	c.normalized = true
	return nil
}

// compileFollow builds the follow matcher for a module path.
func (c *Config) compileFollow(modulePath string) (*followSet, error) {
	f := &followSet{module: modulePath, maxDepth: c.FollowDepth, stdOK: c.FollowStd}
	for _, raw := range c.Follow {
		for _, item := range splitList(raw) {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			neg := false
			if strings.HasPrefix(item, "!") {
				neg = true
				item = strings.TrimSpace(item[1:])
			}
			switch item {
			case "all", "*":
				if neg {
					return nil, fmt.Errorf("-follow: cannot exclude %q", item)
				}
				f.all = true
				continue
			case "std":
				if neg {
					return nil, fmt.Errorf("-follow: cannot exclude %q", item)
				}
				f.std = true
				continue
			case "module":
				if neg {
					f.module = ""
					continue
				}
				f.moduleAll = true
				continue
			}
			if !strings.ContainsAny(item, "*?[") {
				item += "**"
			}
			if neg {
				f.exclude = append(f.exclude, item)
			} else {
				f.include = append(f.include, item)
			}
		}
	}
	return f, nil
}

// followSet decides whether an imported package is pulled in recursively.
type followSet struct {
	all       bool
	std       bool
	stdOK     bool
	moduleAll bool
	module    string
	include   []string
	exclude   []string
	maxDepth  int
}

func (f *followSet) empty() bool {
	return !f.all && !f.std && !f.moduleAll && len(f.include) == 0
}

func (f *followSet) wants(importPath string, isStd bool) bool {
	if f.maxDepth == 0 {
		return false
	}
	if isStd && (!f.stdOK || !f.std) {
		return false
	}
	for _, pat := range f.exclude {
		if matchImport(pat, importPath) {
			return false
		}
	}
	if f.all {
		return true
	}
	if isStd && f.std {
		return true
	}
	if f.moduleAll && f.module != "" && (importPath == f.module || strings.HasPrefix(importPath, f.module+"/")) {
		return true
	}
	for _, pat := range f.include {
		if matchImport(pat, importPath) {
			return true
		}
	}
	return false
}

// matchImport supports "example.com/x/*" (one level), "example.com/x/**"
// (the path itself and any depth) plus plain path.Match wildcards.
func matchImport(pattern, p string) bool {
	if before, ok := strings.CutSuffix(pattern, "**"); ok {
		base := strings.TrimSuffix(before, "/")
		if ok, _ := path.Match(base, p); ok {
			return true
		}
		return p == base || strings.HasPrefix(p, base+"/")
	}
	ok, err := path.Match(pattern, p)
	return err == nil && ok
}

// ============================== helpers ==============================

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containsName(list []string, want string) bool {
	return slices.Contains(list, want)
}

func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return !goKeywords[s]
}

var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] = r[0] - 'a' + 'A'
	}
	return string(r)
}

func itoa(i int) string { return strconv.Itoa(i) }
