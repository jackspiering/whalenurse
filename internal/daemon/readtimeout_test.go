package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/docker"
	"github.com/jackspiering/whalenurse/internal/engine"
)

// One Docker read that never returns must not stop the loop: it times out, the key is tried again at the next poll, and
// every other container is still healed (plan v3.13 item 13).
func TestAHungInspectDoesNotStopTheLoop(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.d.rd = docker.WithReadTimeout(h.fake, 50*time.Millisecond)
	a := h.running(hexID("a1"), "p", "a")
	b := h.running(hexID("b1"), "p", "b")
	h.set(a.ID, unhealthy)
	h.set(b.ID, unhealthy)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.fake.InspectWait = func(ctx context.Context, ref string) error {
		if ref != a.ID {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return context.Canceled
		}
	}
	t0 := h.clk.Now()
	done := make(chan struct{})
	go func() {
		h.tick()
		h.advance(h.cfg.Grace + 2*time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop is stuck on one container's inspect; b was never healed")
	}
	h.wantCalls("restart " + b.ID)
	ka := docker.StateKey(host, a.Name, a.Labels)
	// a's inspect in the startup sweep (the first tick) timed out: it is tried again one poll interval later.
	want := t0.Add(h.cfg.PollInterval)
	if p, ok := h.d.pend[ka]; !ok || p.due.Before(want.Add(-time.Second)) || p.due.After(want.Add(time.Second)) {
		t.Fatalf("a is not tried again at the next poll: due %v (ok %v), want %v", p.due, ok, want)
	}
	for _, e := range h.rec.Events() {
		if e.Key == ka && e.Type == engine.IncidentOpened {
			t.Fatal("a timed-out inspect opened an incident")
		}
	}
}

// With reads bounded the loop keeps ticking, so a Docker API that keeps failing must show in Healthy (so /healthz, the
// HEALTHCHECK and the uptime ping), also while an action is in flight.
func TestHealthyReportsADockerAPIThatKeepsFailing(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.d.setEvents(true)
	h.running(hexID("a1"), "p", "a")
	h.tick()
	h.fake.ListErr = context.DeadlineExceeded
	h.advance(h.cfg.PollInterval) // the poll's list fails: failing since now
	h.advance(h.cfg.PollInterval)
	if ok, why := h.d.Healthy(); !ok {
		t.Fatalf("failing for one poll interval is tolerated: %s", why)
	}
	h.advance(h.cfg.PollInterval + 2*time.Second)
	h.d.acting.Store(h.clk.Now().UnixNano())
	if ok, why := h.d.Healthy(); ok || !strings.Contains(why, "docker API failing for") {
		t.Fatalf("Healthy() = %v, %q; want the failing API reported, before the action tolerance", ok, why)
	}
	h.d.acting.Store(0)
	h.fake.ListErr = nil
	h.advance(h.cfg.PollInterval)
	if ok, why := h.d.Healthy(); !ok {
		t.Fatalf("the API answers again: %s", why)
	}
}

func TestTheDaemonBoundsItsReads(t *testing.T) {
	h := newHarness(t, nil, nil)
	c := h.running(hexID("a1"), "p", "a")
	var deadline time.Time
	h.fake.InspectWait = func(ctx context.Context, ref string) error {
		deadline, _ = ctx.Deadline()
		return nil
	}
	if _, err := h.d.rd.Inspect(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	if left := time.Until(deadline); left <= 0 || left > readTimeout {
		t.Fatalf("inspect deadline %v away, want within %v", left, readTimeout)
	}
}
