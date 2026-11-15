package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Report is the machine readable result of a run (-json). It is stable enough
// to be diffed in CI: every collection is sorted.
type Report struct {
	Schema       int            `json:"schema"`
	Tool         string         `json:"tool"`
	Generated    string         `json:"generated_at"`
	DurationMS   int64          `json:"duration_ms"`
	Config       ReportConfig   `json:"config"`
	Declarations []ReportDecl   `json:"declarations"`
	Files        []ReportFile   `json:"files"`
	Verify       []ReportVerify `json:"verify"`
	Stages       []ReportStage  `json:"stages"`
	Warnings     []string       `json:"warnings"`
	Notes        []string       `json:"notes"`
	Missing      []string       `json:"missing"`
}

type ReportConfig struct {
	Dir         string   `json:"dir"`
	Package     string   `json:"package"`
	Names       []string `json:"names"`
	Targets     []string `json:"targets"`
	Jobs        int      `json:"jobs"`
	Split       string   `json:"split"`
	Follow      []string `json:"follow"`
	FollowDepth int      `json:"follow_depth"`
	Conflict    string   `json:"conflict"`
	Out         string   `json:"out"`
	ShortNames  bool     `json:"short_names,omitempty"`
	Verify      bool     `json:"verify"`
}

type ReportDecl struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Output string `json:"output,omitempty"`
	Origin string `json:"origin"`
	Target string `json:"target"`
}

type ReportFile struct {
	Name       string   `json:"name"`
	Path       string   `json:"path,omitempty"`
	Package    string   `json:"package"`
	Constraint string   `json:"constraint,omitempty"`
	Targets    []string `json:"targets"`
	Bytes      int      `json:"bytes"`
	SHA256     string   `json:"sha256"`
}

type ReportVerify struct {
	Target  string `json:"target"`
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Output  string `json:"output,omitempty"`
	Millis  int64  `json:"millis"`
}

type ReportStage struct {
	Name   string `json:"name"`
	Millis int64  `json:"millis"`
	Error  string `json:"error,omitempty"`
}

func buildReport(st *State, cfg *Config) *Report {
	rep := &Report{
		Schema:     1,
		Tool:       "vexport " + version,
		Generated:  time.Now().UTC().Format(time.RFC3339),
		DurationMS: st.Duration.Milliseconds(),
		Notes:      append([]string{}, st.Notes...),
		Warnings:   append([]string{}, st.Warnings...),
		Missing:    append([]string{}, st.Missing...),
		Config: ReportConfig{
			Dir:         cfg.Dir,
			Package:     cfg.Pattern,
			Names:       cfg.Names,
			Jobs:        cfg.loadWorkers(len(cfg.Targets)),
			Split:       string(cfg.Split),
			Follow:      cfg.Follow,
			FollowDepth: cfg.FollowDepth,
			Conflict:    string(cfg.Conflict),
			Out:         cfg.Out,
			ShortNames:  cfg.ShortNames,
			Verify:      cfg.Verify.Enabled,
		},
	}
	for _, t := range cfg.Targets {
		rep.Config.Targets = append(rep.Config.Targets, t.String())
	}
	for _, sel := range st.Selections {
		rep.Declarations = append(rep.Declarations, selDecls(sel)...)
	}
	sort.SliceStable(rep.Declarations, func(i, j int) bool {
		a, b := rep.Declarations[i], rep.Declarations[j]
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	for _, f := range st.Files {
		sum := sha256.Sum256(f.Content)
		rf := ReportFile{
			Name:       f.Name,
			Path:       f.Path,
			Package:    f.PkgName,
			Constraint: f.Constraint,
			Bytes:      len(f.Content),
			SHA256:     hex.EncodeToString(sum[:]),
		}
		for _, t := range f.Targets {
			rf.Targets = append(rf.Targets, t.String())
		}
		rep.Files = append(rep.Files, rf)
	}
	sort.SliceStable(rep.Files, func(i, j int) bool { return rep.Files[i].Name < rep.Files[j].Name })
	for _, v := range st.Verifies {
		rep.Verify = append(rep.Verify, ReportVerify{
			Target:  v.Target.String(),
			Command: v.Command,
			OK:      v.OK,
			Skipped: v.Skipped,
			Output:  v.Output,
			Millis:  v.Elapsed.Milliseconds(),
		})
	}
	for _, s := range st.Stages {
		rs := ReportStage{Name: s.Name, Millis: s.Elapsed.Milliseconds()}
		if s.Err != nil {
			rs.Error = s.Err.Error()
		}
		rep.Stages = append(rep.Stages, rs)
	}
	return rep
}

func selDecls(sel *Selection) []ReportDecl {
	var out []ReportDecl
	for _, d := range sel.sortedDecls() {
		origin := d.pkg().Path
		if d.pkg() == sel.Prog.Root {
			origin = "(target)"
		}
		rd := ReportDecl{
			Kind:   d.Kind,
			Name:   d.Name,
			Origin: origin,
			Target: sel.Target.String(),
		}
		if d.OutName != d.Name && d.Kind != "method" {
			rd.Output = d.OutName
		}
		out = append(out, rd)
	}
	return out
}

func writeJSONReport(w io.Writer, rep *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// writeHumanReport prints the summary a user expects from a CLI run.
func writeHumanReport(w io.Writer, st *State, cfg *Config, wrote bool) {
	for _, sel := range st.Selections {
		fmt.Fprintf(w, "target %s: %d declaration(s) from %d package(s)\n",
			sel.Target, len(sel.Decls), len(sel.packages()))
	}
	for _, f := range st.Files {
		line := fmt.Sprintf("file %s (%d bytes", f.Name, len(f.Content))
		if f.Constraint != "" {
			line += ", //go:build " + f.Constraint
		}
		line += ")"
		if f.Path != "" {
			if f.Path == "-" {
				line += " -> stdout"
			} else {
				line += " -> " + f.Path
			}
		}
		fmt.Fprintln(w, line)
	}
	for _, v := range st.Verifies {
		status := "ok"
		switch {
		case v.Skipped:
			status = "skipped"
		case !v.OK:
			status = "FAILED"
		}
		fmt.Fprintf(w, "verify %s: %s (%s, %s)\n", v.Target, status, v.Command,
			v.Elapsed.Round(time.Millisecond))
		if !v.OK && v.Output != "" {
			fmt.Fprintln(w, indent(v.Output, "    "))
		}
	}
	if !wrote && !cfg.DryRun {
		return
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// packages lists the packages a selection draws from.
func (s *Selection) packages() []string {
	set := map[string]bool{}
	for _, d := range s.Decls {
		set[d.pkg().Path] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
