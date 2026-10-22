package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nicholas-fedor/shoutrrr/pkg/types"

	"github.com/jackspiering/whalenurse/internal/clock"
	"github.com/jackspiering/whalenurse/internal/engine"
	"github.com/jackspiering/whalenurse/internal/store"
)

func suspended(incident string) engine.Event {
	return ev(engine.Suspended, webKey, "every rung ran or was unavailable: container is unhealthy",
		map[string]string{"reason": "ladder", engine.AttrIncident: incident})
}

func TestTheSameEventIsSentOncePerIncident(t *testing.T) {
	nt, s, _, _ := newTest(t, 2, nil)
	nt.Handle(suspended("inc-1"))
	nt.Handle(suspended("inc-1")) // the same thing, same incident: said once
	nt.Handle(suspended("inc-2")) // a new incident is news again
	nt.Handle(ev(engine.Recovered, webKey, "healthy", map[string]string{engine.AttrIncident: "inc-2"}))
	nt.Handle(ev(engine.Recovered, webKey, "healthy", map[string]string{engine.AttrIncident: "inc-2"}))
	flush(nt)
	for i, snd := range s {
		if got := len(snd.messages()); got != 3 {
			t.Errorf("url %d got %d messages, want 3 (suspended, suspended, recovered): %v", i+1, got, snd.titles())
		}
	}
}

func TestDedupeIsPerKey(t *testing.T) {
	nt, s, _, _ := newTest(t, 1, nil)
	nt.Handle(suspended("inc-1"))
	other := suspended("inc-1")
	other.Key, other.Container = "nas/stack/db/1", "stack-db-1"
	nt.Handle(other)
	flush(nt)
	if got := len(s[0].messages()); got != 2 {
		t.Fatalf("two containers, two messages; got %d", got)
	}
}

func TestADriftBlockedSuspensionIsExactlyOneMessage(t *testing.T) {
	nt, s, _, _ := newTest(t, 1, nil)
	nt.Handle(ev(engine.DriftBlocked, webKey, "recreate skipped by the drift guard: x", map[string]string{"findings": "the tag moved", engine.AttrIncident: "i"}))
	nt.Handle(ev(engine.Suspended, webKey, "recreate skipped by the drift guard (x): y", map[string]string{"reason": "drift", engine.AttrIncident: "i"}))
	flush(nt)
	if got := s[0].titles(); len(got) != 1 || !strings.Contains(got[0], "drift guard") {
		t.Fatalf("want one drift message, got %v", got)
	}
}

func TestAdvisoriesWithoutAnIncidentAreDedupedForADayOnly(t *testing.T) {
	nt, s, clk, _ := newTest(t, 1, nil)
	down := ev(engine.NotifyOnly, webKey, "left down", map[string]string{"class": "exited-host-stopped", "dependents": "stack-side-1"})
	nt.Handle(down)
	nt.Handle(down)
	settle(t, "first message", func() bool { return len(s[0].messages()) == 1 })
	clk.Advance(25 * time.Hour)
	nt.Handle(down)
	flush(nt)
	if got := len(s[0].messages()); got != 2 {
		t.Fatalf("a day later it is news again: got %d messages", got)
	}
}

func TestPauseAndResumeAreCommandsAndAreNeverDeduplicated(t *testing.T) {
	nt, s, _, _ := newTest(t, 1, nil)
	p := engine.Event{Host: testHost, Key: "nas/stack/web", Type: engine.Paused, Message: "paused"}
	nt.Handle(p)
	nt.Handle(p)
	flush(nt)
	if got := len(s[0].messages()); got != 2 {
		t.Fatalf("got %d, want 2", got)
	}
}

func TestVersionGateWarningIsSentOncePerProcess(t *testing.T) {
	nt, s, _, _ := newTest(t, 1, nil)
	g := engine.Event{Host: testHost, Type: engine.RungsDisabled, Message: "rungs 2 and 3 are disabled: Engine 27.0 is below 28.1"}
	nt.Handle(g)
	nt.Handle(g) // re-emitted after every reconnect
	flush(nt)
	if got := len(s[0].messages()); got != 1 {
		t.Fatalf("got %d", got)
	}
}

func TestAnActionResultSaysWhyItWasStarted(t *testing.T) {
	nt, s, _, _ := newTest(t, 1, nil)
	nt.Handle(ev(engine.ActionStarted, webKey, "restart: container is unhealthy (3 failed checks)", map[string]string{"action": "restart", "rung": "restart"}))
	nt.Handle(ev(engine.ActionResult, webKey, "restart ok", map[string]string{"action": "restart", "rung": "restart", "ok": "true"}))
	flush(nt)
	m := s[0].messages()
	if len(m) != 1 || !strings.Contains(m[0].Body, "why: container is unhealthy (3 failed checks)") {
		t.Fatalf("%v", m)
	}
}

func TestEveryURLGetsEveryMessage(t *testing.T) {
	nt, s, _, _ := newTest(t, 3, nil)
	nt.Handle(suspended("i"))
	flush(nt)
	for i, snd := range s {
		if len(snd.messages()) != 1 {
			t.Errorf("url %d: %d messages", i+1, len(snd.messages()))
		}
	}
}

func TestRateLimitPerURLWithAReserveForCriticalMessages(t *testing.T) {
	nt, s, clk, _ := newTest(t, 2, func(o *Options) { o.RateLimit = 4 })
	// Routine messages may use half the budget (2). A storm of them must not stop "giving up" from getting through.
	for i := 0; i < 6; i++ {
		e := ev(engine.ActionResult, "nas/stack/web/"+string(rune('1'+i)), "restart ok", map[string]string{"action": "restart", "rung": "restart", "ok": "true"})
		nt.Handle(e)
	}
	nt.Handle(suspended("inc"))
	other := suspended("inc")
	other.Key = "nas/stack/db/1"
	nt.Handle(other)
	nt.Handle(ev(engine.NotifyOnly, "nas/stack/x/1", "left down", map[string]string{"class": "c"})) // the budget is spent by now
	settle(t, "delivery", func() bool { return len(s[0].messages()) >= 4 })
	flush(nt)
	got := s[0].messages()
	if len(got) != 4 {
		t.Fatalf("limit 4 per hour: got %d messages: %v", len(got), s[0].titles())
	}
	routine, critical := 0, 0
	for _, m := range got {
		if m.Critical {
			critical++
		} else {
			routine++
		}
	}
	if routine != 2 || critical != 2 {
		t.Errorf("want 2 routine + 2 critical, got %d + %d", routine, critical)
	}
	// Each URL has a limit of its own.
	if len(s[1].messages()) != 4 {
		t.Errorf("the second URL has its own budget: %d", len(s[1].messages()))
	}
	st := nt.Status().Targets[0]
	if st.RateLimited != 5 || st.Sent != 4 {
		t.Errorf("counted sent=%d limited=%d, want 4 and 5", st.Sent, st.RateLimited)
	}
	_ = clk
}

func TestHeldBackMessagesAreSummarisedInTheNextOne(t *testing.T) {
	nt, s, clk, _ := newTest(t, 1, func(o *Options) { o.RateLimit = 2 })
	rec := func(n int) engine.Event {
		return ev(engine.Recovered, "nas/stack/web/"+string(rune('0'+n)), "healthy", nil)
	}
	nt.Handle(rec(1)) // routine budget is 1
	nt.Handle(rec(2)) // held back
	nt.Handle(rec(3)) // held back
	settle(t, "first", func() bool { return len(s[0].messages()) == 1 && nt.Status().Targets[0].RateLimited == 2 })
	clk.Advance(61 * time.Minute)
	nt.Handle(rec(4))
	flush(nt)
	got := s[0].messages()
	if len(got) != 2 || !strings.Contains(got[1].Body, "2 earlier messages were held back") {
		t.Fatalf("%v", got)
	}
}

func TestAFailingURLIsRetriedCountedAndLoggedRedacted(t *testing.T) {
	var s *recSender
	nt, senders, _, lb := newTest(t, 1, func(o *Options) {
		o.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
		o.Redact = NewRedactor("ntfy://user:hunter2-token@ntfy.example.com/secret-topic")
	})
	s = senders[0]
	s.err = errors.New(`Post "https://ntfy.example.com/secret-topic": 401 for user hunter2-token`)
	nt.Handle(suspended("i"))
	flush(nt)
	st := nt.Status().Targets[0]
	if st.Failed != 1 || st.Sent != 0 || s.calls != 3 {
		t.Fatalf("failed=%d sent=%d attempts=%d, want 1, 0, 3", st.Failed, st.Sent, s.calls)
	}
	for _, text := range []string{lb.String(), st.LastError} {
		if strings.Contains(text, "hunter2-token") || strings.Contains(text, "secret-topic") {
			t.Errorf("a token leaked: %s", text)
		}
	}
	if !strings.Contains(lb.String(), "notification failed") || !strings.Contains(lb.String(), `"url":"#1 ntfy"`) {
		t.Errorf("the failure is not logged by label: %s", lb.String())
	}
}

// The hard rule: a failing or slow URL never blocks or delays the engine, which calls Handle with its lock held.
func TestHandleNeverBlocksEvenWithAHungURLAndAFullQueue(t *testing.T) {
	block := make(chan struct{})
	nt, senders, _, _ := newTest(t, 2, func(o *Options) { o.QueueSize = 4; o.Timeout = time.Minute })
	senders[0].block = block // hangs on every message
	start := time.Now()
	for i := 0; i < 500; i++ {
		nt.Handle(ev(engine.Suspended, "nas/stack/svc"+string(rune('a'+i%26))+"/"+string(rune('a'+i/26)), "x: y", map[string]string{"reason": "ladder", engine.AttrIncident: "i"}))
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Handle took %s for 500 events with a hung URL", d)
	}
	close(block)
}

func TestAHungURLDoesNotDelayAnother(t *testing.T) {
	block := make(chan struct{})
	nt, senders, _, _ := newTest(t, 2, func(o *Options) { o.Timeout = time.Minute })
	senders[0].block = block
	nt.Handle(suspended("i"))
	settle(t, "the healthy URL to get its message while the other hangs", func() bool { return len(senders[1].messages()) == 1 })
	close(block)
}

func TestATimeoutIsEnforcedPerAttempt(t *testing.T) {
	nt, senders, _, _ := newTest(t, 1, func(o *Options) { o.Timeout = 30 * time.Millisecond })
	senders[0].delay = time.Minute
	nt.Handle(suspended("i"))
	settle(t, "the timeout to count as a failure", func() bool { return nt.Status().Targets[0].Failed == 1 })
}

func TestShutdownDeliversWhatIsQueuedAndGivesUpOnWhatHangs(t *testing.T) {
	nt, senders, _, _ := newTest(t, 2, nil)
	senders[1].block = make(chan struct{}) // never released
	nt.Handle(suspended("i"))
	start := time.Now()
	nt.Shutdown(100 * time.Millisecond)
	if time.Since(start) > 2*time.Second {
		t.Fatal("Shutdown waited for a hung URL")
	}
	if len(senders[0].messages()) != 1 {
		t.Fatal("the message queued before the stop was not delivered")
	}
	// After Shutdown, Handle is a harmless no-op (the engine may still publish while the daemon stops).
	nt.Handle(suspended("j"))
	nt.Shutdown(time.Second)
}

func TestAnInvalidURLIsListedAndSendsNothing(t *testing.T) {
	good := &recSender{}
	nt, _, _, _ := newTest(t, 0, func(o *Options) {
		o.Targets = []Target{{Label: "#1 bogus", Err: "unknown service"}, {Label: "#2 ntfy", Sender: good}}
	})
	nt.Handle(suspended("i"))
	flush(nt)
	st := nt.Status()
	if len(good.messages()) != 1 || st.Targets[0].Invalid != "unknown service" || !strings.Contains(st.Summary(), "#1 bogus INVALID") {
		t.Fatalf("%+v / %s", st, st.Summary())
	}
}

func TestSendTestReportsEachURLAndBypassesTheLimits(t *testing.T) {
	ok, bad := &recSender{}, &recSender{err: errors.New("boom for secret-token-123")}
	targets := []Target{{Label: "#1 ntfy", Sender: ok}, {Label: "#2 gotify", Sender: bad}, {Label: "#3 bogus", Err: "unknown service"}}
	red := NewRedactor("gotify://h/secret-token-123")
	res := SendTest(context.Background(), targets, red, time.Second, TestMessage("nas", true))
	if !res[0].OK || res[1].OK || res[2].OK {
		t.Fatalf("%+v", res)
	}
	var out strings.Builder
	failed := PrintTest(&out, res)
	if failed != 2 || strings.Contains(out.String(), "secret-token-123") || !strings.Contains(out.String(), "#2 gotify") || !strings.Contains(out.String(), "FAILED") {
		t.Fatalf("failed=%d\n%s", failed, out.String())
	}
	m := ok.messages()[0]
	if !strings.HasPrefix(m.Title, DryRunMarker) || !strings.Contains(m.Body, "host nas") {
		t.Errorf("test message: %v", m)
	}
}

// With the dispatcher stalled and the queue full, Handle still returns at once and counts what it dropped: the engine
// calls it with its lock held.
func TestHandleDropsInsteadOfBlockingWhenTheQueueIsFull(t *testing.T) {
	lb := &logBuf{}
	n := &Notifier{in: make(chan engine.Event, 1), log: lb.logger()} // no dispatcher: nothing ever drains it
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			n.Handle(suspended("i"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Handle blocked on a full queue")
	}
	if got := n.droppedEvents.Load(); got != 4 {
		t.Fatalf("dropped %d, want 4", got)
	}
	if n.Status().DroppedEvents != 4 {
		t.Error("the status does not show the dropped events")
	}
}

// A panic in a notification service must not take the healer down (Shoutrrr's router runs services in goroutines of its
// own with no recover, so whalenurse sends by calling the service itself, under a recover).
type panicSender struct{ recSender }

func (*panicSender) Send(context.Context, Message) error { panic("boom with token-in-panic-9999") }

func TestAPanickingSenderIsAFailedDeliveryAndOtherURLsStillGetTheirs(t *testing.T) {
	good := &recSender{}
	nt, _, _, lb := newTest(t, 0, func(o *Options) {
		o.Targets = []Target{{Label: "#1 bad", Sender: &panicSender{}}, {Label: "#2 good", Sender: good}}
		o.Redact = NewRedactor("ntfy://x/token-in-panic-9999")
	})
	nt.Handle(suspended("i"))
	nt.Handle(ev(engine.Recovered, webKey, "healthy", map[string]string{engine.AttrIncident: "i"}))
	flush(nt)
	st := nt.Status().Targets
	if st[0].Failed != 2 || st[0].Sent != 0 || len(good.messages()) != 2 {
		t.Fatalf("%+v / %d delivered to the good URL", st, len(good.messages()))
	}
	if strings.Contains(lb.String(), "token-in-panic-9999") || strings.Contains(st[0].LastError, "token-in-panic-9999") {
		t.Errorf("the panic value leaked a token: %s / %s", lb.String(), st[0].LastError)
	}
}

type fakeSvc struct {
	types.Service // never called: only the methods below are
	withCtx       bool
}

func (fakeSvc) GetID() string                    { return "fake" }
func (fakeSvc) Send(string, *types.Params) error { panic("plain send panics") }

type fakeCtxSvc struct{ fakeSvc }

func (fakeCtxSvc) SendContext(context.Context, string, *types.Params) error {
	panic("context send panics")
}

func TestShoutrrrSenderTurnsAServicePanicIntoAnError(t *testing.T) {
	for name, svc := range map[string]types.Service{"context sender": fakeCtxSvc{}, "plain sender": fakeSvc{}} {
		err := (&shoutrrrSender{svc: svc}).Send(context.Background(), Message{Title: "t", Body: "b"})
		if err == nil || !strings.Contains(err.Error(), "panic") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTheHeldBackNoteNeverPushesTheBodyOverTheLimit(t *testing.T) {
	long := strings.Repeat("a long cause that goes on and on; ", 20)
	e := ev(engine.Suspended, webKey, "every rung ran or was unavailable: "+long, map[string]string{"reason": "ladder", engine.AttrIncident: "i"})
	m, _, _ := render(e, rctx{})
	got := withNote(m.Body, "(12 earlier messages were held back by the rate limit)", MaxBody)
	if utf8.RuneCountInString(got) > MaxBody || !strings.HasPrefix(got, webKey+"\n") || !strings.HasSuffix(got, "held back by the rate limit)") {
		t.Fatalf("%d runes: %q", utf8.RuneCountInString(got), got)
	}
	short := withNote("nas/p/web/1\nrecovered", "(1 earlier messages were held back by the rate limit)", MaxBody)
	if short != "nas/p/web/1\nrecovered\n(1 earlier messages were held back by the rate limit)" {
		t.Fatalf("%q", short)
	}
}

// ---- v3.7 follow-ups: the persisted dedupe, RateLimited, Unsuspended ------------------------------------------

type memLedger struct {
	mu      sync.Mutex
	rows    map[string]time.Time
	forgot  []string
	loadErr error
}

func (l *memLedger) LoadNotified(now time.Time, ttl time.Duration) (map[string]time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]time.Time{}
	for k, v := range l.rows {
		if now.Sub(v) < ttl {
			out[k] = v
		}
	}
	return out, l.loadErr
}
func (l *memLedger) SaveNotified(id, typ, key string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rows == nil {
		l.rows = map[string]time.Time{}
	}
	l.rows[id] = at
}
func (l *memLedger) ForgetNotified(typ, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.forgot = append(l.forgot, typ+"|"+key)
	for id := range l.rows {
		if strings.HasPrefix(id, typ+"|"+key+"|") {
			delete(l.rows, id)
		}
	}
}

// A restart of whalenurse does not say the same thing twice: the first process's notifier writes the ledger, the second
// one's reads it.
func TestTheDedupeSurvivesARestartOfWhalenurse(t *testing.T) {
	dir := t.TempDir()
	run := func(events ...engine.Event) []string {
		st, err := store.Open(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		nt, senders, _, _ := newTest(t, 1, func(o *Options) { o.Ledger = st; o.Clock = clock.Real{} })
		for _, e := range events {
			nt.Handle(e)
		}
		flush(nt)
		return senders[0].titles()
	}
	down := ev(engine.NotifyOnly, webKey, "left down", map[string]string{"class": "exited-host-stopped", "dependents": "stack-side-1"})
	susp := suspended("inc-1")
	if got := run(down, susp); len(got) != 2 {
		t.Fatalf("first run: %v", got)
	}
	// The same things after the restart: the engine re-emits the advisory, and an incident's events repeat.
	if got := run(down, susp); len(got) != 0 {
		t.Fatalf("a restart repeated what was already said: %v", got)
	}
	// News is still news: a new incident, and a different container.
	other := down
	other.Key = "nas/stack/db/1"
	if got := run(other, suspended("inc-2")); len(got) != 2 {
		t.Fatalf("%v", got)
	}
}

func TestAnUnreadableLedgerStartsEmptyAndDoesNotStopTheNotifier(t *testing.T) {
	l := &memLedger{loadErr: errors.New("disk on fire")}
	nt, s, _, lb := newTest(t, 1, func(o *Options) { o.Ledger = l })
	nt.Handle(suspended("i"))
	flush(nt)
	if len(s[0].messages()) != 1 || !strings.Contains(lb.String(), "cannot read the notification ledger") {
		t.Fatalf("%v / %s", s[0].titles(), lb.String())
	}
}

func TestUnsuspendedIsRoutineAndSuspendingAgainInTheSameIncidentIsNewsAgain(t *testing.T) {
	l := &memLedger{}
	nt, s, _, _ := newTest(t, 1, func(o *Options) { o.Ledger = l })
	inc := map[string]string{engine.AttrIncident: "i"}
	susp := func() engine.Event {
		return ev(engine.Suspended, webKey, "every rung ran or was unavailable: unhealthy", map[string]string{"reason": "ladder", engine.AttrIncident: "i"})
	}
	back := ev(engine.Unsuspended, webKey, "container returned to healthy", map[string]string{"reason": "healthy", engine.AttrIncident: inc[engine.AttrIncident]})
	nt.Handle(susp())
	nt.Handle(susp()) // a repeat: said once
	nt.Handle(back)
	nt.Handle(susp()) // it failed again before the incident closed: news again, or the last word would be "unsuspended"
	nt.Handle(back)   // the second time it came back in this incident: alternating is news once per incident (v3.13 item 18)
	flush(nt)
	var got []string
	for _, m := range s[0].messages() {
		got = append(got, m.Title)
	}
	want := []string{"giving up", "unsuspended", "giving up"}
	if len(got) != 3 {
		t.Fatalf("%v", got)
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("message %d = %q, want %q", i+1, got[i], w)
		}
	}
	if s[0].messages()[1].Critical {
		t.Error("unsuspended is a routine message")
	}
	if len(l.forgot) == 0 {
		t.Error("the ledger was not told to forget the suspension")
	}
}

func TestAnUnsuspendByCommandIsNeverDeduplicated(t *testing.T) {
	nt, s, _, _ := newTest(t, 1, nil)
	cmd := ev(engine.Unsuspended, webKey, "unsuspended by command", map[string]string{"reason": "command"})
	nt.Handle(cmd)
	nt.Handle(cmd)
	flush(nt)
	if len(s[0].messages()) != 2 {
		t.Fatalf("%v", s[0].titles())
	}
}

// "Action rate limit reached" is host-wide: at most one an hour per URL, whichever container hit it.
func TestRateLimitedIsSentAtMostOncePerHourPerURL(t *testing.T) {
	nt, s, clk, _ := newTest(t, 2, nil)
	rl := func(key string) engine.Event {
		e := ev(engine.RateLimited, key, "rate limit of 10 actions/hour reached; will retry", map[string]string{engine.AttrIncident: key})
		e.Key = key
		return e
	}
	nt.Handle(rl("nas/p/a/1"))
	nt.Handle(rl("nas/p/b/1")) // another container, same hour: skipped
	settle(t, "the first message", func() bool {
		return len(s[0].messages()) == 1 && len(s[1].messages()) == 1 && nt.Status().Targets[0].Throttled == 1
	})
	clk.Advance(59 * time.Minute)
	nt.Handle(rl("nas/p/c/1")) // 59 minutes: still skipped
	settle(t, "the skip", func() bool { return nt.Status().Targets[0].Throttled == 2 })
	clk.Advance(2 * time.Minute)
	nt.Handle(rl("nas/p/d/1")) // more than an hour since the last one that went out
	flush(nt)
	for i, snd := range s {
		got := snd.messages()
		if len(got) != 2 || !strings.Contains(got[0].Body, "nas/p/a/1") || !strings.Contains(got[1].Body, "nas/p/d/1") {
			t.Errorf("url %d: %v", i+1, snd.titles())
		}
	}
	if st := nt.Status().Targets[0]; st.Throttled != 2 || st.RateLimited != 0 || st.Sent != 2 {
		t.Errorf("%+v", st)
	}
}

func TestAThrottledMessageDoesNotSpendTheURLsHourlyBudget(t *testing.T) {
	nt, s, _, _ := newTest(t, 1, func(o *Options) { o.RateLimit = 2 })
	for _, k := range []string{"a", "b", "c", "d"} {
		e := ev(engine.RateLimited, "nas/p/"+k+"/1", "rate limit reached", nil)
		nt.Handle(e)
	}
	nt.Handle(suspended("i")) // critical, budget of 2: the one RateLimited that went out used 1, this is the 2nd
	flush(nt)
	if got := len(s[0].messages()); got != 2 {
		t.Fatalf("%v", s[0].titles())
	}
}

// The held-back note goes before the rollback, never over it: the commands of an "upgraded during heal" message stay whole
// and last even when the rate limit held messages back before it (plan v3.8 item 6).
func TestHeldBackNoteNeverCutsTheRollbackCommands(t *testing.T) {
	nt, s, _, _ := newTest(t, 1, func(o *Options) { o.RateLimit = 2 }) // the routine half is 1 an hour
	nt.Handle(ev(engine.Recovered, "nas/stack/a/1", "healthy", nil))   // sent
	nt.Handle(ev(engine.Recovered, "nas/stack/b/1", "healthy", nil))   // held back
	settle(t, "the hold-back", func() bool { return nt.Status().Targets[0].RateLimited == 1 })
	a := movedAttrs(stackProject(), "ghcr.io/o/web:latest", true)
	nt.Handle(ev(engine.TagMoved, webKey, "x", a)) // critical: takes the second slot and carries the note
	flush(nt)
	msgs := s[0].messages()
	if len(msgs) != 2 {
		t.Fatalf("%d messages", len(msgs))
	}
	got := msgs[1].Body
	if !strings.HasSuffix(got, "\n"+a["rollback_tag"]+"\n"+a["rollback_up"]) {
		t.Errorf("the rollback commands must be the last two lines, whole:\n%s", got)
	}
	if !strings.Contains(got, "(1 earlier messages were held back by the rate limit)") || !strings.HasPrefix(got, webKey+"\n") {
		t.Errorf("the note or the key line is missing:\n%s", got)
	}
	if utf8.RuneCountInString(got) > MaxBodyPinned {
		t.Errorf("%d runes", utf8.RuneCountInString(got))
	}
}

// With the longest pinned message the note still fits within MaxBodyPinned, and neither the commands nor the key line
// give way to it.
func TestHeldBackNoteOnTheWorstCaseUpgradeMessage(t *testing.T) {
	m := renderOne(t, worstCaseUpgrade(t), rctx{dryRun: true})
	note := "(999 earlier messages were held back by the rate limit)"
	got := withPinnedNote(m, note)
	if utf8.RuneCountInString(got) > MaxBodyPinned || !strings.HasSuffix(got, m.pinned) || !strings.HasPrefix(got, "[dry-run] nas/") {
		t.Fatalf("%d runes:\n%s", utf8.RuneCountInString(got), got)
	}
}

// Event text that quotes a configured secret is redacted before it is rendered, so no message body carries it (URL #1's
// token is not pushed to URL #2's service). The key and the host stay.
func TestEventTextIsRedactedInTheMessage(t *testing.T) {
	red := NewRedactorWith([]string{"reg-PASS-82002"}, "ntfy://user:hunter2-token@ntfy.example.com/secret-topic")
	nt, s, _, _ := newTest(t, 1, func(o *Options) { o.Redact = red })
	nt.Handle(ev(engine.ActionStarted, webKey, "restart: unhealthy, see ntfy.example.com/secret-topic", map[string]string{"action": "restart"}))
	nt.Handle(ev(engine.ActionResult, webKey, `restart failed: Post "https://ntfy.example.com/secret-topic": 401 for user hunter2-token reg-PASS-82002`,
		map[string]string{"action": "restart", "rung": "restart", "ok": "false", engine.AttrIncident: "i"}))
	nt.Handle(ev(engine.DriftBlocked, webKey, "x", map[string]string{"findings": "config: hunter2-token would recreate", engine.AttrIncident: "i"}))
	flush(nt)
	msgs := s[0].messages()
	if len(msgs) != 2 {
		t.Fatalf("%d messages", len(msgs))
	}
	for _, m := range msgs {
		for _, secret := range []string{"hunter2-token", "secret-topic", "ntfy.example.com", "reg-PASS-82002"} {
			if strings.Contains(m.Title+m.Body, secret) {
				t.Errorf("%q leaks into %v", secret, m)
			}
		}
		if !strings.Contains(m.Body, "***") || !strings.HasPrefix(m.Body, webKey+"\n") || !strings.HasPrefix(m.Title, "nas: ") {
			t.Errorf("%v", m)
		}
	}
	if !strings.Contains(msgs[0].Title, "restart failed") {
		t.Errorf("the control attributes are not redacted: %v", msgs[0])
	}
}

// The rollback commands are sent exactly or not at all: when the redactor would change one of them (a topic named like a
// word of the command), both are withheld, the message says so, and it is an ordinary message within MaxBody. Without
// such a fragment they are sent byte for byte.
func TestARollbackTheRedactorWouldChangeIsWithheld(t *testing.T) {
	a := movedAttrs(stackProject(), "ghcr.io/o/web:latest", true)
	nt, s, _, _ := newTest(t, 1, func(o *Options) { o.Redact = NewRedactor("ntfy://ntfy.example.com/stacks") })
	nt.Handle(ev(engine.TagMoved, webKey, "x", a))
	flush(nt)
	m := s[0].messages()[0]
	if strings.Contains(m.Body, "docker image tag") || strings.Contains(m.Body, "roll back:") || !strings.Contains(m.Body, "rollback not shown") {
		t.Fatalf("%s", m.Body)
	}
	if utf8.RuneCountInString(m.Body) > MaxBody {
		t.Fatalf("%d runes", utf8.RuneCountInString(m.Body))
	}
	nt2, s2, _, _ := newTest(t, 1, func(o *Options) { o.Redact = NewRedactor("ntfy://ntfy.example.com/unrelated-topic") })
	nt2.Handle(ev(engine.TagMoved, webKey, "x", a))
	flush(nt2)
	if b := s2[0].messages()[0].Body; !strings.HasSuffix(b, "\n"+a["rollback_tag"]+"\n"+a["rollback_up"]) {
		t.Fatalf("%s", b)
	}
}

// The control attributes are not redacted, so a short query value of a URL cannot change what a message says.
func TestRedactingAnEventKeepsItsControlAttributes(t *testing.T) {
	red := NewRedactor("generic://h.example/hook-path?template=json&ok=true&k=restart")
	e := red.Event(ev(engine.ActionResult, webKey, "restart failed: true json", map[string]string{"action": "restart", "rung": "restart", "ok": "true", "error": "json true"}))
	if e.Attrs["ok"] != "true" || e.Attrs["action"] != "restart" || e.Attrs["rung"] != "restart" || e.Key != webKey || e.Host != testHost {
		t.Fatalf("%+v", e)
	}
	if e.Attrs["error"] != "*** ***" || e.Message != "*** failed: *** ***" {
		t.Fatalf("%+v", e)
	}
}

// The drift fold applies only to the Suspended right after its DriftBlocked (plan v3.13 item 19): a re-suspension later in
// the same incident, after an Unsuspended, is a giving-up message. The Unsuspended after that is not news again: the two
// make each other news only once per incident (item 18).
func TestASuspensionAgainAfterADriftBlockIsNews(t *testing.T) {
	nt, s, _, _ := newTest(t, 1, nil)
	nt.Handle(ev(engine.DriftBlocked, webKey, "recreate skipped by the drift guard: the tag moved", map[string]string{"findings": "the tag moved", engine.AttrIncident: "i"}))
	nt.Handle(ev(engine.Suspended, webKey, "recreate skipped by the drift guard (the tag moved): unhealthy", map[string]string{"reason": "drift", engine.AttrIncident: "i"}))
	nt.Handle(ev(engine.Unsuspended, webKey, "container returned to healthy", map[string]string{"reason": "healthy", engine.AttrIncident: "i"}))
	nt.Handle(ev(engine.Suspended, webKey, "failed again before the incident closed (drift): unhealthy", map[string]string{"reason": "drift", engine.AttrIncident: "i"}))
	nt.Handle(ev(engine.Unsuspended, webKey, "container returned to healthy", map[string]string{"reason": "healthy", engine.AttrIncident: "i"}))
	flush(nt)
	got := s[0].messages()
	var titles []string
	for _, m := range got {
		titles = append(titles, m.Title)
	}
	want := []string{"drift guard, SUSPENDED", "unsuspended", "SUSPENDED"}
	if len(got) != len(want) {
		t.Fatalf("%d messages, want %d: %q", len(got), len(want), titles)
	}
	for i, w := range want {
		if !strings.HasSuffix(got[i].Title, w) {
			t.Errorf("message %d: %q, want ...%q", i, got[i].Title, w)
		}
	}
	again := got[2]
	if !again.Critical || !strings.Contains(again.Body, "drift guard") || !strings.Contains(again.Body, "whalenurse unsuspend stack/web") || utf8.RuneCountInString(again.Body) > MaxBody {
		t.Errorf("the re-suspension is a giving-up message that keeps the drift context: %v", again)
	}
}

// Suspended and Unsuspended make each other news again only once per incident (plan v3.13 item 18): the first Unsuspended
// after a Suspended and the first Suspended after that are sent; a healthcheck that keeps flipping sends nothing more in
// that incident, also across a restart of whalenurse. A new incident is news again.
func TestSuspendedAndUnsuspendedAlternateOnlyOncePerIncident(t *testing.T) {
	l := &memLedger{}
	susp := func(inc string) engine.Event {
		return ev(engine.Suspended, webKey, "failed again before the incident closed (ladder): unhealthy", map[string]string{"reason": "ladder", engine.AttrIncident: inc})
	}
	back := func(inc string) engine.Event {
		return ev(engine.Unsuspended, webKey, "container returned to healthy", map[string]string{"reason": "healthy", engine.AttrIncident: inc})
	}
	nt, s, _, _ := newTest(t, 1, func(o *Options) { o.Ledger = l })
	for _, e := range []engine.Event{susp("i"), back("i"), susp("i"), back("i"), susp("i")} {
		nt.Handle(e)
	}
	flush(nt)
	if got := s[0].titles(); len(got) != 3 || !strings.Contains(got[0], "giving up") || !strings.Contains(got[1], "unsuspended") || !strings.Contains(got[2], "giving up") {
		t.Fatalf("want giving up, unsuspended, giving up: %q", got)
	}
	// A restart of whalenurse in the same incident: still nothing more.
	nt2, s2, _, _ := newTest(t, 1, func(o *Options) { o.Ledger = l })
	nt2.Handle(back("i"))
	nt2.Handle(susp("i"))
	// A new incident is news again.
	nt2.Handle(susp("j"))
	nt2.Handle(back("j"))
	nt2.Handle(susp("j"))
	flush(nt2)
	if got := s2[0].titles(); len(got) != 3 {
		t.Fatalf("after a restart nothing more in incident i, then a new incident's three: %q", got)
	}
	// A deliberate unsuspend is said every time.
	nt3, s3, _, _ := newTest(t, 1, func(o *Options) { o.Ledger = l })
	cmd := ev(engine.Unsuspended, webKey, "unsuspended by command", map[string]string{"reason": "command", engine.AttrIncident: "i"})
	nt3.Handle(cmd)
	nt3.Handle(cmd)
	flush(nt3)
	if got := s3[0].titles(); len(got) != 2 {
		t.Fatalf("%q", got)
	}
}

// A message that no URL could take is not recorded as sent: with no URL, or only invalid ones, nothing is written to the
// ledger, so the warning goes out once a working URL is added and whalenurse restarts. A URL that takes the message and
// then fails to deliver it still records it (the dedupe is written as messages are accepted).
func TestNothingIsRecordedAsSentWhenNoURLCanTakeIt(t *testing.T) {
	events := []engine.Event{
		ev(engine.RungsDisabled, "", "rungs 2 and 3 are disabled: Engine 27.0 is below 28.1", nil),
		ev(engine.NotifyOnly, webKey, "left down by policy", map[string]string{"class": "exited-host-stopped"}),
	}
	for name, targets := range map[string][]Target{"no URL": nil, "an invalid URL": {{Label: "#1 bogus", Err: "unknown service"}}} {
		t.Run(name, func(t *testing.T) {
			l := &memLedger{}
			nt, _, _, _ := newTest(t, 0, func(o *Options) { o.Ledger = l; o.Targets = targets })
			for _, e := range events {
				nt.Handle(e)
			}
			flush(nt)
			if len(l.rows) != 0 {
				t.Fatalf("recorded %d rows with no usable URL: %v", len(l.rows), l.rows)
			}
			nt2, s, _, _ := newTest(t, 1, func(o *Options) { o.Ledger = l }) // a URL is added, whalenurse restarts
			for _, e := range events {
				nt2.Handle(e)
			}
			flush(nt2)
			if got := len(s[0].messages()); got != len(events) {
				t.Fatalf("%d of %d messages after the URL was added", got, len(events))
			}
		})
	}
	l := &memLedger{}
	nt, s, _, _ := newTest(t, 1, func(o *Options) { o.Ledger = l })
	s[0].err = errors.New("service down")
	nt.Handle(events[0])
	flush(nt)
	if len(l.rows) != 1 {
		t.Fatalf("a usable URL that failed to deliver still records it: %v", l.rows)
	}
}
