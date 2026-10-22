package compose

import (
	"context"
	"sync"
)

// Call records one call on a Fake.
type Call struct {
	Verb    string // dry-run | hash | policy | recreate | up | pull
	Project Project
}

// Fake is an in-memory Reader and Actor for tests: it runs nothing. Mutating calls are recorded in order; an OnRecreate
// or OnUp hook lets a test make the "recreate" change the fake Docker.
type Fake struct {
	mu    sync.Mutex
	calls []Call

	Bin        Binary
	OracleFn   func(p Project, containerName string) (Oracle, error) // default: an unchanged container
	HashFn     func(p Project) (string, error)
	PolicyFn   func(p Project) (string, error) // default: no pull_policy
	OnRecreate func(p Project) error
	OnUp       func(p Project) error
	OnPull     func(p Project) error
	// OnCtx, when set, sees the context of every mutating call (recreate, up, pull) before its hook runs.
	OnCtx func(verb string, ctx context.Context)
}

// NewFake returns a Fake that reports compose 5.6.0 from the host plugin.
func NewFake() *Fake {
	return &Fake{Bin: Binary{Path: "/host/docker-compose", Source: SourceHost, Version: Version{5, 6, 0}, Raw: "5.6.0"}}
}

func (f *Fake) record(verb string, p Project) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Verb: verb, Project: p})
}

// Calls returns a copy of the calls so far.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Count returns how many calls had the verb.
func (f *Fake) Count(verb string) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Verb == verb {
			n++
		}
	}
	return n
}

func (f *Fake) Binary() Binary { return f.Bin }

func (f *Fake) DryRunUp(_ context.Context, p Project, name string) (Oracle, error) {
	f.record("dry-run", p)
	if f.OracleFn != nil {
		return f.OracleFn(p, name)
	}
	return Oracle{Lines: []Line{{ID: "Container " + name, Status: "Done", Text: "Running"}}}, nil
}

func (f *Fake) ConfigHash(_ context.Context, p Project) (string, error) {
	f.record("hash", p)
	if f.HashFn != nil {
		return f.HashFn(p)
	}
	return "", ErrUnparseable
}

func (f *Fake) PullPolicy(_ context.Context, p Project) (string, error) {
	f.record("policy", p)
	if f.PolicyFn != nil {
		return f.PolicyFn(p)
	}
	return "", nil
}

func (f *Fake) Recreate(ctx context.Context, p Project) error {
	f.record("recreate", p)
	if f.OnCtx != nil {
		f.OnCtx("recreate", ctx)
	}
	if f.OnRecreate != nil {
		return f.OnRecreate(p)
	}
	return nil
}

func (f *Fake) Up(ctx context.Context, p Project) error {
	f.record("up", p)
	if f.OnCtx != nil {
		f.OnCtx("up", ctx)
	}
	if f.OnUp != nil {
		return f.OnUp(p)
	}
	return nil
}

func (f *Fake) Pull(ctx context.Context, p Project) error {
	f.record("pull", p)
	if f.OnCtx != nil {
		f.OnCtx("pull", ctx)
	}
	if f.OnPull != nil {
		return f.OnPull(p)
	}
	return nil
}

var _ Reader = (*Fake)(nil)
var _ Actor = (*Fake)(nil)

// Recreated is an Oracle for a container compose would recreate.
func Recreated(name string) Oracle {
	return Oracle{Lines: []Line{
		{ID: "Container " + name, Status: "Working", Text: "Recreate"},
		{ID: "Container " + name, Status: "Done", Text: "Recreated"},
	}}
}
