package main

import (
	"context"
	"fmt"
	"time"
)

// ============================== pipeline contract ==============================
//
// The extractor is a five stage pipeline. Every stage is an interface, so a
// caller can replace any of them, wrap them with middleware, or stop after a
// given stage (this is how the "plan" sub command works).
//
//	load    -> []*Program      one loaded + indexed package graph per target
//	resolve -> []*Selection    seeds + recursive dependency closure
//	emit    -> []File          rendered files grouped by build constraint
//	verify  -> []VerifyResult  proof that the result compiles
//	write   -> []File          files on disk (or stdout), with safety checks
//
// A programmatic user writes:
//
//	cfg := planb.DefaultConfig()
//	cfg.Pattern, cfg.Names = "./user", []string{"User"}
//	p := planb.New(cfg, planb.WithVerifier(myVerifier))
//	st, err := p.Run(context.Background())
//
// Middleware can enforce policy on any stage without forking the pipeline:
//
//	p.Use(func(next planb.StageFunc) planb.StageFunc {
//		return func(ctx context.Context, st *planb.State) error {
//			if err := next(ctx, st); err != nil {
//				return err
//			}
//			if len(st.Files) > 3 {
//				return fmt.Errorf("refusing to emit %d files", len(st.Files))
//			}
//			return nil
//		}
//	})

// Loader turns a Config into one loaded package graph per build target.
type Loader interface {
	Load(ctx context.Context, cfg *Config) ([]*Program, error)
}

// Resolver turns one loaded program into a selection.
type Resolver interface {
	Resolve(ctx context.Context, prog *Program, cfg *Config) (*Selection, error)
}

// Emitter renders selections into output files.
type Emitter interface {
	Emit(ctx context.Context, sels []*Selection, cfg *Config) ([]File, error)
}

// Verifier proves that the rendered files build.
type Verifier interface {
	Verify(ctx context.Context, files []File, cfg *Config) ([]VerifyResult, error)
}

// Writer persists the files.
type Writer interface {
	Write(ctx context.Context, files []File, cfg *Config) error
}

// StageFunc is the uniform shape of a pipeline step.
type StageFunc func(ctx context.Context, st *State) error

// Middleware wraps a stage: logging, retries, policy checks.
type Middleware func(next StageFunc) StageFunc

// State is the data flowing through the pipeline.
type State struct {
	Config     *Config
	Programs   []*Program
	Selections []*Selection
	Files      []File
	Verifies   []VerifyResult
	Warnings   []string
	Notes      []string
	Missing    []string
	Started    time.Time
	Duration   time.Duration
	Stages     []StageTiming
}

type StageTiming struct {
	Name    string
	Elapsed time.Duration
	Err     error
}

// ============================== pipeline ==============================

type Pipeline struct {
	loader   Loader
	resolver Resolver
	emitter  Emitter
	verifier Verifier
	writer   Writer

	cfg *Config
	mw  []Middleware

	// StopAfter ends the pipeline after the named stage (empty = run all).
	StopAfter string
	// OnStage is called after every stage (used by -v).
	OnStage func(name string, elapsed time.Duration, err error)
}

type Option func(*Pipeline)

func WithLoader(l Loader) Option     { return func(p *Pipeline) { p.loader = l } }
func WithResolver(r Resolver) Option { return func(p *Pipeline) { p.resolver = r } }
func WithEmitter(e Emitter) Option   { return func(p *Pipeline) { p.emitter = e } }
func WithVerifier(v Verifier) Option { return func(p *Pipeline) { p.verifier = v } }
func WithWriter(w Writer) Option     { return func(p *Pipeline) { p.writer = w } }

// New builds a pipeline with the default stages, then applies the options.
func New(cfg *Config, opts ...Option) *Pipeline {
	p := &Pipeline{
		loader:   packagesLoader{},
		resolver: typeResolver{},
		emitter:  taggedEmitter{},
		verifier: buildVerifier{},
		writer:   fileWriter{},
		cfg:      cfg,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (p *Pipeline) Use(mw ...Middleware) { p.mw = append(p.mw, mw...) }

// StageNames lists the stages that will actually run.
func (p *Pipeline) StageNames() []string {
	names := []string{"load", "resolve", "emit", "verify", "write"}
	if !p.cfg.Verify.Enabled {
		names = append(names[:3], "write")
	}
	if p.StopAfter != "" {
		var out []string
		for _, n := range names {
			out = append(out, n)
			if n == p.StopAfter {
				break
			}
		}
		names = out
	}
	return names
}

// Run executes the pipeline. A non nil error means the state is partial and
// nothing has been written to disk unless the failing stage is "verify".
func (p *Pipeline) Run(ctx context.Context) (*State, error) {
	st := &State{Config: p.cfg, Started: time.Now()}
	defer func() { st.Duration = time.Since(st.Started) }()

	steps := []struct {
		name string
		fn   StageFunc
	}{
		{"load", p.stageLoad},
		{"resolve", p.stageResolve},
		{"emit", p.stageEmit},
		{"verify", p.stageVerify},
		{"write", p.stageWrite},
	}
	for _, step := range steps {
		if !p.activeStage(step.name) {
			continue
		}
		fn := step.fn
		for i := len(p.mw) - 1; i >= 0; i-- {
			fn = p.mw[i](fn)
		}
		begin := time.Now()
		err := fn(ctx, st)
		elapsed := time.Since(begin)
		st.Stages = append(st.Stages, StageTiming{Name: step.name, Elapsed: elapsed, Err: err})
		if p.OnStage != nil {
			p.OnStage(step.name, elapsed, err)
		}
		if err != nil {
			return st, fmt.Errorf("%s: %w", step.name, err)
		}
		if p.StopAfter != "" && step.name == p.StopAfter {
			return st, nil
		}
	}
	return st, nil
}

func (p *Pipeline) activeStage(name string) bool {
	if name == "verify" && !p.cfg.Verify.Enabled {
		return false
	}
	return true
}

// ============================== stages ==============================

func (p *Pipeline) stageLoad(ctx context.Context, st *State) error {
	progs, err := p.loader.Load(ctx, p.cfg)
	if err != nil {
		return err
	}
	st.Programs = progs
	for _, prog := range progs {
		if prog.Root != nil {
			st.Notes = append(st.Notes, fmt.Sprintf("loaded %s (%s) for %s",
				prog.Root.Path, prog.Root.Name, prog.Target))
		}
		st.Warnings = append(st.Warnings, prog.Warnings...)
	}
	return nil
}

func (p *Pipeline) stageResolve(ctx context.Context, st *State) error {
	for _, prog := range st.Programs {
		sel, err := p.resolver.Resolve(ctx, prog, p.cfg)
		if err != nil {
			return err
		}
		st.Selections = append(st.Selections, sel)
		st.Warnings = append(st.Warnings, sel.Warnings...)
		st.Notes = append(st.Notes, sel.Notes...)
		st.Missing = append(st.Missing, sel.Missing...)
	}
	// the same missing declaration can be reported by several
	// targets. Count and print it once.
	st.Missing = uniqueSorted(st.Missing)
	if len(st.Missing) > 0 && !p.cfg.AllowPartial {
		return &MissingError{Names: st.Missing}
	}
	return nil
}

// MissingError reports -names that do not exist in the package. It maps to
// exit code 2 and, unlike a load error, is not a sign of broken input.
type MissingError struct{ Names []string }

func (e *MissingError) Error() string {
	return fmt.Sprintf("%d declaration(s) not found: %s (use -allow-partial to continue)",
		len(e.Names), joinStrings(e.Names, ", "))
}

func (p *Pipeline) stageEmit(ctx context.Context, st *State) error {
	files, err := p.emitter.Emit(ctx, st.Selections, p.cfg)
	if err != nil {
		return err
	}
	st.Files = files
	st.Notes = append(st.Notes, coverageNotes(files)...)
	// emitter diagnostics belong to State, so -json includes
	// them. The emitter itself must not write directly to stderr.
	for _, f := range files {
		st.Warnings = append(st.Warnings, f.Warnings...)
	}
	return nil
}

func (p *Pipeline) stageVerify(ctx context.Context, st *State) error {
	if !p.cfg.Verify.Enabled {
		return nil
	}
	res, err := p.verifier.Verify(ctx, st.Files, p.cfg)
	st.Verifies = res
	if err != nil {
		return err
	}
	for _, r := range res {
		if !r.OK {
			return &VerifyError{Results: res}
		}
	}
	return nil
}

func (p *Pipeline) stageWrite(ctx context.Context, st *State) error {
	if p.cfg.DryRun {
		return nil
	}
	return p.writer.Write(ctx, st.Files, p.cfg)
}

// VerifyError reports every failed build in one place.
type VerifyError struct{ Results []VerifyResult }

func (e *VerifyError) Error() string {
	var n int
	for _, r := range e.Results {
		if !r.OK {
			n++
		}
	}
	return fmt.Sprintf("generated code does not build for %d target(s)", n)
}

func joinStrings(in []string, sep string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
