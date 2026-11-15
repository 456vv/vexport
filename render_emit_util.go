package main

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
)

// shortHash keeps generated file names stable across runs and independent of
// map iteration order.
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// hasFilenameTagSuffix reports whether a complete base name (without .go)
// ends in "_GOOS" or "_GOARCH". Go treats such a suffix as an implicit build
// constraint, so generated names must avoid it unless it is intentional.
func hasFilenameTagSuffix(base string) bool {
	i := strings.LastIndex(base, "_")
	if i < 0 || i == len(base)-1 {
		return false
	}
	last := base[i+1:]
	return knownOS[last] || knownArch[last]
}

// slugEndsWithFilenameTag is the slug variant: the prefix is joined to the
// slug with an underscore, so even a bare "linux" slug would become "_linux"
// in the final file name.
func slugEndsWithFilenameTag(slug string) bool {
	if !strings.Contains(slug, "_") {
		return knownOS[slug] || knownArch[slug]
	}
	return hasFilenameTagSuffix(slug)
}

// ensureUniqueFileNames is the last line of defense against silent overwrites.
// fileWriter writes files in order and treats generated files as replaceable,
// so duplicate names would keep only the last content.
func ensureUniqueFileNames(files []File) error {
	seen := map[string]File{}
	for _, f := range files {
		if prev, ok := seen[f.Name]; ok {
			return fmt.Errorf("duplicate output file %q for targets %s and %s; use -suffix or a different -split mode",
				f.Name, targetNames(prev.Targets), targetNames(f.Targets))
		}
		seen[f.Name] = f
	}
	return nil
}

func targetNames(targets []Target) string {
	if len(targets) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.String())
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// declKeySetDiff compares the declarations of every variant. emitSingle can
// only merge variants whose declaration sets are identical; otherwise one
// //go:build line cannot describe both sides of the union.
func declKeySetDiff(variants []*variant) string {
	if len(variants) < 2 {
		return ""
	}
	first := map[string]bool{}
	for _, d := range variants[0].decls {
		first[d.key()] = true
	}
	var diffs []string
	for _, v := range variants[1:] {
		set := map[string]bool{}
		for _, d := range v.decls {
			set[d.key()] = true
		}
		for key := range first {
			if !set[key] {
				diffs = append(diffs, "missing "+declKeyName(key))
			}
		}
		for key := range set {
			if !first[key] {
				diffs = append(diffs, "extra "+declKeyName(key))
			}
		}
	}
	return strings.Join(uniqueSorted(diffs), ", ")
}

func declKeyName(key string) string {
	if i := strings.LastIndex(key, "::"); i >= 0 {
		return key[i+2:]
	}
	return key
}

// mergeVariantDecls builds the union for emitSingle. It must not use
// sortDecls: across target loads token.Pos values come from different
// FileSets, and the name fallback can reorder constants incorrectly.
func mergeVariantDecls(variants []*variant) []*Decl {
	seen := map[string]bool{}
	var out []*Decl
	for _, v := range variants {
		for _, d := range v.decls {
			if seen[d.key()] {
				continue
			}
			seen[d.key()] = true
			out = append(out, d)
		}
	}
	// stable rank order preserves source order within each kind and keeps
	// constants from later variants after the constants already present.
	sort.SliceStable(out, func(i, j int) bool {
		return rank(out[i].Kind) < rank(out[j].Kind)
	})
	return out
}

func constraintsAllEmpty(in []string) bool {
	for _, c := range in {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}
