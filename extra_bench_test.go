package main

import (
	"errors"
	"testing"
)

func BenchmarkParseTarget(b *testing.B) {
	inputs := []string{"host", "linux/amd64", "linux/arm64+integration,dbg"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ParseTarget(inputs[i%len(inputs)])
	}
}

func BenchmarkParseTargets(b *testing.B) {
	in := []string{
		"linux/amd64,windows/amd64,darwin/arm64",
		"linux/amd64+x,linux/amd64+y,linux/arm64+integration",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ParseTargets(in)
	}
}

func BenchmarkMatchImport(b *testing.B) {
	cases := []struct {
		pattern, path string
	}{
		{"example.com/x/**", "example.com/x/a/b"},
		{"example.com/*", "example.com/x/y"},
		{"example.com/*", "example.com/x"},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := cases[i%len(cases)]
		_ = matchImport(c.pattern, c.path)
	}
}

func BenchmarkFollowSetWants(b *testing.B) {
	f := &followSet{
		module:    "example.com/m",
		moduleAll: true,
		include:   []string{"example.com/**"},
		exclude:   []string{"example.com/internal/**"},
		maxDepth:  8,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = f.wants("example.com/pkg/sub", false)
	}
}

func BenchmarkParseRewrites(b *testing.B) {
	in := []string{"a/=x/", "bb/=y/", "ccc/=z/", "longer/path/=long/"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = parseRewrites(in)
	}
}

func BenchmarkOrConstraints(b *testing.B) {
	in := []string{"linux && amd64", "windows && amd64", "darwin && arm64", "linux && arm64"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = orConstraints(in)
	}
}

func BenchmarkImportNameHint(b *testing.B) {
	paths := []string{
		"example.com/foo",
		"example.com/foo/v2",
		"golang.org/x/tools/go/packages",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = importNameHint(paths[i%len(paths)])
	}
}

func BenchmarkSanitizeFileName(b *testing.B) {
	in := "pkg_ext_linux_amd64-with tag.go"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sanitizeFileName(in)
	}
}

func BenchmarkMergeEnv(b *testing.B) {
	base := []string{"PATH=/usr/bin", "GOOS=linux", "GOARCH=amd64", "GOFLAGS="}
	overrides := []string{"GOOS=windows", "GOARCH=arm64", "GOFLAGS=-tags=a,b"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mergeEnv(base, overrides)
	}
}

func BenchmarkClassify(b *testing.B) {
	miss := &MissingError{Names: []string{"A", "B"}}
	verr := &VerifyError{Results: []VerifyResult{{OK: false}}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		switch i % 3 {
		case 0:
			_ = classify(nil, nil)
		case 1:
			_ = classify(nil, miss)
		default:
			_ = classify(nil, verr)
		}
	}
}

func BenchmarkClassifyGenericWithStages(b *testing.B) {
	st := &State{Stages: []StageTiming{{Name: "emit"}}}
	err := errors.New("boom")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = classify(st, err)
	}
}
