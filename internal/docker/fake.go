package docker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Call records one mutating call made on the Fake.
type Call struct {
	Verb string // restart | start
	ID   string
	Name string
}

// Fake is an in-memory Client for tests. It never touches Docker. Mutating
// calls are recorded in order; FailMutations makes any of them fail, which
// proves a code path performs none.
type Fake struct {
	mu         sync.Mutex
	containers map[string]Container
	calls      []Call
	events     chan Event
	errs       chan error

	// Hooks and switches, set before use.
	FailMutations bool                                   // every Restart/Start returns an error and is still recorded
	OnMutate      func(f *Fake, verb, id string)         // runs inside the call, before it returns
	OnCtx         func(verb string, ctx context.Context) // sees the context of every Restart/Start, before OnMutate
	AfterInspect  func(f *Fake, ref string)              // runs after an Inspect call has read its result
	ListErr       error
	ServerErr     error // ServerVersion fails with this

	server ServerInfo
	images map[string]string // reference -> what ImageID returns for it
	plain  map[string]string // reference -> what ImageID returns with no platform (the index digest); defaults to images
	since  []time.Time       // the since argument of every Events subscription
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{
		containers: map[string]Container{}, events: make(chan Event, 256), errs: make(chan error, 1),
		server: ServerInfo{Version: "29.9.1", APIVersion: "1.56", Arch: "amd64", OS: "linux"}, images: map[string]string{}, plain: map[string]string{},
	}
}

// SetServer replaces what ServerVersion reports.
func (f *Fake) SetServer(s ServerInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.server = s
}

// SetImage makes ImageID(ref) return id; an empty id removes the reference.
func (f *Fake) SetImage(ref, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "" {
		delete(f.images, ref)
		return
	}
	f.images[ref] = id
}

// SetPlainImage makes the platform-less ImageID(ref, "") return id (the index digest on the containerd store).
func (f *Fake) SetPlainImage(ref, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plain[ref] = id
}

func (f *Fake) ServerVersion(context.Context) (ServerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ServerErr != nil {
		return ServerInfo{}, f.ServerErr
	}
	return f.server, nil
}

func (f *Fake) ImageID(_ context.Context, ref, arch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if arch == "" {
		if id, ok := f.plain[ref]; ok {
			return id, nil
		}
	}
	if id, ok := f.images[ref]; ok {
		return id, nil
	}
	return "", fmt.Errorf("%w: %s (linux/%s)", ErrImageNotFound, ref, arch)
}

// Put adds or replaces a container (keyed by ID).
func (f *Fake) Put(c Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[c.ID] = c
}

// Remove deletes a container.
func (f *Fake) Remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
}

// Update mutates a stored container.
func (f *Fake) Update(id string, fn func(*Container)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	fn(&c)
	f.containers[id] = c
}

// Calls returns a copy of the recorded mutating calls.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// CallsOn returns the recorded calls that targeted id.
func (f *Fake) CallsOn(id string) []Call {
	var out []Call
	for _, c := range f.Calls() {
		if c.ID == id {
			out = append(out, c)
		}
	}
	return out
}

// Emit queues an event for the current subscriber.
func (f *Fake) Emit(ev Event) { f.events <- ev }

// BreakStream ends the current subscription with err.
func (f *Fake) BreakStream(err error) { f.errs <- err }

func (f *Fake) Ping(context.Context) error { return nil }
func (f *Fake) Close() error               { return nil }

func (f *Fake) List(context.Context) ([]Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	out := make([]Summary, 0, len(f.containers))
	for _, c := range f.containers {
		out = append(out, Summary{ID: c.ID, Name: c.Name, Labels: c.Labels, State: c.Status, Health: c.Health, NetworkMode: c.NetworkMode, Image: c.Image, Created: c.Created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *Fake) Inspect(_ context.Context, ref string) (Container, error) {
	c, err := f.lookup(ref)
	f.mu.Lock()
	hook := f.AfterInspect
	f.mu.Unlock()
	if hook != nil {
		hook(f, ref)
	}
	return c, err
}

func (f *Fake) lookup(ref string) (Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[ref]; ok {
		return c, nil
	}
	for _, c := range f.containers {
		if MatchesRef(c.ID, c.Name, ref) {
			return c, nil
		}
	}
	return Container{}, fmt.Errorf("%w: %s", ErrNotFound, ref)
}

// Subscriptions returns the since argument of every Events call so far.
func (f *Fake) Subscriptions() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.since...)
}

func (f *Fake) Events(ctx context.Context, since time.Time) Stream {
	f.mu.Lock()
	f.since = append(f.since, since)
	f.mu.Unlock()
	out := make(chan Event)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		for {
			select {
			case ev := <-f.events:
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			case err := <-f.errs:
				errs <- err
				return
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
		}
	}()
	return Stream{Events: out, Err: errs}
}

func (f *Fake) mutate(ctx context.Context, verb, id string) error {
	if f.OnCtx != nil {
		f.OnCtx(verb, ctx)
	}
	f.mu.Lock()
	name := ""
	if c, ok := f.containers[id]; ok {
		name = c.Name
	}
	f.calls = append(f.calls, Call{Verb: verb, ID: id, Name: name})
	fail, hook := f.FailMutations, f.OnMutate
	f.mu.Unlock()
	if fail {
		return fmt.Errorf("fake: %s %s refused (FailMutations)", verb, id)
	}
	if hook != nil {
		hook(f, verb, id)
	}
	if _, err := f.lookup(id); err != nil {
		return fmt.Errorf("cannot %s %s: %w", verb, id, err)
	}
	return nil
}

func (f *Fake) Restart(ctx context.Context, id string) error { return f.mutate(ctx, "restart", id) }
func (f *Fake) Start(ctx context.Context, id string) error   { return f.mutate(ctx, "start", id) }

var _ Client = (*Fake)(nil)
var _ Client = (*Moby)(nil)

// Ref builds a "container:<id>" mode string.
func Ref(id string) string { return containerRefPrefix + id }

// ShortID is the 12-character form used in logs.
func ShortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
