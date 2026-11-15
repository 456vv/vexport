package main

import (
	"fmt"
	"go/build"
	"go/build/constraint"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// discoverReport is the outcome of -targets auto.
type discoverReport struct {
	Targets    []Target // minimal set covering every scannable file
	CustomTags []string // tags that are neither GOOS nor GOARCH; user must opt in
	Scanned    int      // .go files considered
	WithBuild  int      // files carrying //go:build or a filename suffix
	Skipped    []string // files no candidate target matched (e.g. //go:build ignore)
}

// discoverTargets scans the root package directory and derives a minimal
// Target set that covers every build-constrained file.
//
// It only "discovers": the OS and GOARCH tags found in the constraints are
// cross-multiplied, each candidate is validated with build.Context.MatchFile,
// and a greedy set cover picks the smallest subset. Tags that are not
// GOOS/GOARCH (integration, premium, ...) cannot be inferred safely, so they
// are reported back instead of being enabled.
//
// Cross-package -follow dependencies may add new OS/GOARCH constraints that
// are invisible here (nothing is loaded yet). That is intentional: cover the
// root package, warn the user, never pretend the result is exhaustive.
func discoverTargets(cfg *Config) (*discoverReport, error) {
	dir, err := resolvePackageDir(cfg)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("discover: %w", err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("discover: no .go files in %s", dir)
	}

	// Split every file into required tags and forbidden tags.
	positive := map[string]bool{}
	negative := map[string]bool{}
	withBuild := 0
	for _, f := range files {
		pos, neg := fileConstraintAtoms(f)
		if len(pos) > 0 || len(neg) > 0 {
			withBuild++
		}
		for _, tag := range pos {
			positive[tag] = true
		}
		for _, tag := range neg {
			negative[tag] = true
		}
	}

	var osTags, archTags, customTags []string
	for tag := range positive {
		switch {
		case knownOS[tag]:
			osTags = append(osTags, tag)
		case knownArch[tag]:
			archTags = append(archTags, tag)
		case tag == "unix", tag == "cgo", tag == "gc", tag == "gccgo":
			// Derived / toolchain tags, go/build handles them itself.
		case strings.HasPrefix(tag, "go1."):
			// Version tags are orthogonal to the build matrix.
		default:
			customTags = append(customTags, tag)
		}
	}
	var negOS, negArch []string
	for tag := range negative {
		switch {
		case knownOS[tag]:
			negOS = append(negOS, tag)
		case knownArch[tag]:
			negArch = append(negArch, tag)
		}
	}
	sort.Strings(osTags)
	sort.Strings(archTags)
	sort.Strings(customTags)
	sort.Strings(negOS)
	sort.Strings(negArch)

	host := hostTarget()
	if len(osTags) == 0 {
		osTags = []string{host.GOOS}
	}
	if len(archTags) == 0 {
		archTags = []string{host.GOARCH}
	}

	// A negated GOOS/GOARCH like "!windows" can only be satisfied by a target
	// that is not windows. If the positive set has no such target, add a
	// representative alternative. Without this, //go:build !windows files are
	// invisible to -targets auto and never emitted.
	for _, neg := range negOS {
		if hasOtherOS(osTags, neg) {
			continue
		}
		if alt := pickAlternativeOS(neg, osTags); alt != "" {
			osTags = append(osTags, alt)
		}
	}
	for _, neg := range negArch {
		if hasOtherArch(archTags, neg) {
			continue
		}
		if alt := pickAlternativeArch(neg, archTags); alt != "" {
			archTags = append(archTags, alt)
		}
	}
	sort.Strings(osTags)
	sort.Strings(archTags)

	type candidate struct {
		t      Target
		covers map[int]bool
	}
	var cands []candidate
	for _, goos := range osTags {
		for _, goarch := range archTags {
			t := Target{GOOS: goos, GOARCH: goarch}
			covers := map[int]bool{}
			for i, f := range files {
				if matchFileFor(t, f) {
					covers[i] = true
				}
			}
			if len(covers) > 0 {
				cands = append(cands, candidate{t: t, covers: covers})
			}
		}
	}

	// Greedy set cover.
	uncovered := map[int]bool{}
	for i := range files {
		uncovered[i] = true
	}
	var skipped []string
	for i, f := range files {
		matched := false
		for _, c := range cands {
			if c.covers[i] {
				matched = true
				break
			}
		}
		if !matched {
			skipped = append(skipped, f)
			delete(uncovered, i)
		}
	}

	var chosen []Target
	for len(uncovered) > 0 {
		best, bestGain := -1, 0
		for i, c := range cands {
			gain := 0
			for fi := range c.covers {
				if uncovered[fi] {
					gain++
				}
			}
			if gain > bestGain {
				best, bestGain = i, gain
			}
		}
		if best < 0 {
			break
		}
		chosen = append(chosen, cands[best].t)
		for fi := range cands[best].covers {
			delete(uncovered, fi)
		}
	}
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].String() < chosen[j].String() })
	if len(chosen) == 0 {
		chosen = []Target{host}
	}

	return &discoverReport{
		Targets:    chosen,
		CustomTags: customTags,
		Scanned:    len(files),
		WithBuild:  withBuild,
		Skipped:    skipped,
	}, nil
}

// resolvePackageDir locates the source directory of cfg.Pattern.
// Only relative patterns are supported here: an import path would require
// either module resolution (go list) or go/build.Import, which is unreliable
// in module mode. Refusing early is better than silently scanning the wrong
// directory.
func resolvePackageDir(cfg *Config) (string, error) {
	p := cfg.Pattern
	if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || p == "." {
		return filepath.Join(cfg.Dir, p), nil
	}
	return "", fmt.Errorf("discover: -targets auto needs -pkg to be a relative path (got %q); pass an explicit -targets list instead", p)
}

// matchFileFor asks go/build whether a file participates in a target. Unlike
// the expression walk above, this respects unix, cgo, release tags and the
// GOOS extension rules (android→linux, ios→darwin, illumos→solaris).
func matchFileFor(t Target, path string) bool {
	ctx := build.Default // value copy, no global state touched
	ctx.GOOS = t.GOOS
	ctx.GOARCH = t.GOARCH
	ctx.BuildTags = t.Tags
	// forcing CgoEnabled=false made every //go:build cgo file
	// invisible to -targets auto. Respect the host toolchain's cgo setting;
	// an explicit -targets list can still opt into a cgo-free target.
	ctx.CgoEnabled = build.Default.CgoEnabled
	ok, err := ctx.MatchFile(filepath.Dir(path), filepath.Base(path))
	return err == nil && ok
}

// fileConstraintAtoms returns the atomic tags of one file, split into the
// ones the //go:build expression requires (positive) and the ones it forbids
// (negative). Filename suffixes always count as positive requirements.
// 只有 // +build、没有 //go:build 的旧文件也要计入，否则 -targets auto 看不到它们。
func fileConstraintAtoms(path string) (positive, negative []string) {
	positive = append(positive, filenameTags(filepath.Base(path))...)
	b, err := os.ReadFile(path)
	if err != nil {
		return uniqueSorted(positive), negative
	}
	var legacy []constraint.Expr
	sawGoBuild := false
header:
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case constraint.IsGoBuild(line):
			if expr, err := constraint.Parse(line); err == nil {
				collectConstraintAtoms(expr, &positive, &negative)
			}
			sawGoBuild = true
			// 出现 //go:build 后 // +build 全部作废，不必再扫文件头。
			break header
		case constraint.IsPlusBuild(line):
			if expr, err := constraint.Parse(line); err == nil {
				legacy = append(legacy, expr)
			}
		case strings.HasPrefix(line, "//"):
			continue
		default:
			break header
		}
	}
	if !sawGoBuild {
		if expr := andExprs(legacy); expr != nil {
			collectConstraintAtoms(expr, &positive, &negative)
		}
	}
	return uniqueSorted(positive), uniqueSorted(negative)
}

// collectConstraintAtoms splits an expression into positive atoms and simple
// "!tag" negations. A complex negation like !(linux && amd64) contributes
// nothing: candidate validation (matchFileFor) will still reject wrong
// targets, and pretending to know its atoms would only mislead the cover.
func collectConstraintAtoms(expr constraint.Expr, positive, negative *[]string) {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		*positive = append(*positive, e.Tag)
	case *constraint.NotExpr:
		if tag, ok := e.X.(*constraint.TagExpr); ok {
			*negative = append(*negative, tag.Tag)
			return
		}
		// Complex negation: ignore, validation catches it.
	case *constraint.AndExpr:
		collectConstraintAtoms(e.X, positive, negative)
		collectConstraintAtoms(e.Y, positive, negative)
	case *constraint.OrExpr:
		collectConstraintAtoms(e.X, positive, negative)
		collectConstraintAtoms(e.Y, positive, negative)
	}
}

// hasOtherOS reports whether osTags already contains a value other than the
// negated one, i.e. a target that satisfies "!negated".
func hasOtherOS(osTags []string, negated string) bool {
	for _, t := range osTags {
		if t != negated {
			return true
		}
	}
	return false
}

func hasOtherArch(archTags []string, negated string) bool {
	for _, t := range archTags {
		if t != negated {
			return true
		}
	}
	return false
}

// pickAlternativeOS chooses a GOOS that is not negated and not already in the
// positive set. The host GOOS wins when it qualifies; otherwise a common
// unix-like GOOS is used.
func pickAlternativeOS(negated string, positive []string) string {
	host := hostTarget()
	if host.GOOS != negated && !containsName(positive, host.GOOS) {
		return host.GOOS
	}
	for _, cand := range []string{"linux", "darwin", "windows", "freebsd", "openbsd", "netbsd"} {
		if cand != negated && !containsName(positive, cand) {
			return cand
		}
	}
	return ""
}

func pickAlternativeArch(negated string, positive []string) string {
	host := hostTarget()
	if host.GOARCH != negated && !containsName(positive, host.GOARCH) {
		return host.GOARCH
	}
	for _, cand := range []string{"amd64", "arm64", "386", "arm"} {
		if cand != negated && !containsName(positive, cand) {
			return cand
		}
	}
	return ""
}
