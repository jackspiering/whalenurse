// Package notify sends the domain events people want to hear about to Shoutrrr URLs (ntfy, Gotify, Discord, Telegram,
// a webhook ...). It is one more subscriber of the engine's event stream, next to the log and the store, so what is sent
// can never disagree with what is recorded.
//
// Three rules shape it:
//
//   - Healing never waits for a notification. The subscriber only queues the event; one dispatcher renders and
//     deduplicates, and one worker per URL delivers with a timeout, so a dead or slow URL delays nothing, not even another
//     URL. A failure is logged and counted, never raised.
//   - The URLs are secrets. They are never logged, never part of a status, and every error is redacted before it is shown
//     (see Redactor). A URL is known by its position and its scheme only: "#2 discord".
//   - Say it once. The same event for the same container is sent once per incident, and each URL has a rate limit with a
//     reserve for the important messages.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackspiering/whalenurse/internal/clock"
	"github.com/jackspiering/whalenurse/internal/engine"
)

// Sender delivers a message to one URL.
type Sender interface {
	// Send delivers m, or fails. The context carries the delivery timeout. An error may name the URL's parts: the
	// Notifier redacts it before it is logged or kept.
	Send(ctx context.Context, m Message) error
	Close() error
}

// Target is one configured URL, as the Notifier knows it: a label that is safe to print and a Sender. Sender is nil when
// the URL was invalid (Err says why, redacted); such a target is listed in the status and sends nothing.
type Target struct {
	Label  string // "#2 discord"
	Sender Sender
	Err    string
}

// Ledger persists the dedupe, so a restart of whalenurse does not say the same thing twice. internal/store implements it.
// It is called from the dispatcher goroutine only, never from the engine's.
type Ledger interface {
	// LoadNotified returns the identity of every message sent within ttl, and when; older ones are dropped.
	LoadNotified(now time.Time, ttl time.Duration) (map[string]time.Time, error)
	SaveNotified(id, typ, key string, at time.Time)
	// ForgetNotified drops the rows of one event type for one key.
	ForgetNotified(typ, key string)
}

// Options configure a Notifier.
type Options struct {
	Host   string
	DryRun bool
	Clock  clock.Clock
	Log    *slog.Logger
	Redact *Redactor

	Targets   []Target
	Timeout   time.Duration // one delivery attempt; default 10s
	RateLimit int           // messages per hour per URL; the second half is reserved for critical ones; default 20
	QueueSize int           // events waiting for the dispatcher, and messages waiting per URL; default 64
	// Backoff is the wait before each retry of a failed delivery (so len(Backoff)+1 attempts); nil means 2s and 10s.
	Backoff []time.Duration
	// DedupeFor is how long a sent message suppresses its twin when there is no incident to tell them apart; default 24h.
	DedupeFor time.Duration
	// Ledger, when set, keeps the dedupe across restarts (nil in a dry run, which stores nothing).
	Ledger Ledger
}

// DefaultDedupeFor is how long a sent message is remembered (and how long the ledger keeps a row).
const DefaultDedupeFor = 24 * time.Hour

// Notifier is the subscriber. Create it with New, subscribe Handle to the bus, and call Shutdown when the daemon stops.
type Notifier struct {
	o   Options
	clk clock.Clock
	log *slog.Logger

	mu     sync.RWMutex // guards closed against Handle sending on a closed channel
	closed bool
	in     chan engine.Event

	// dispatcher-only state
	sent    map[string]time.Time // dedupe identity -> when it was sent
	reasons map[string]string    // container key -> why its running action was started
	// foldNext: container key -> the incident of a DriftBlocked just routed. The Suspended(drift) that follows it says
	// nothing new and is folded into it; any other Suspended(drift) is news (plan v3.13 item 19).
	foldNext map[string]string

	targets []*worker
	wg      sync.WaitGroup
	cancel  context.CancelFunc // aborts deliveries in flight when Shutdown runs out of time
	sendCtx context.Context

	droppedEvents atomic.Int64
}

type worker struct {
	t  Target
	q  chan Message
	nt *Notifier

	mu          sync.Mutex
	stamps      []time.Time          // when this URL's messages were sent, for the hourly limit
	throttled   map[string]time.Time // message class -> when the last one of it went out (a class has a minimum spacing)
	throttledN  int64
	suppressed  int // messages the limit held back since the last one that went out
	sentN       int64
	failedN     int64
	limitedN    int64
	droppedN    int64
	lastErr     string
	lastErrAt   time.Time
	lastOK      time.Time
	attemptsRun int64
}

// New starts the dispatcher and one worker per usable target.
func New(o Options) *Notifier {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.RateLimit <= 0 {
		o.RateLimit = 20
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 64
	}
	if o.Backoff == nil {
		o.Backoff = []time.Duration{2 * time.Second, 10 * time.Second}
	}
	if o.DedupeFor <= 0 {
		o.DedupeFor = DefaultDedupeFor
	}
	n := &Notifier{
		o: o, clk: o.Clock, log: o.Log,
		in:       make(chan engine.Event, o.QueueSize),
		sent:     map[string]time.Time{},
		reasons:  map[string]string{},
		foldNext: map[string]string{},
	}
	n.sendCtx, n.cancel = context.WithCancel(context.Background())
	if o.Ledger != nil {
		if sent, err := o.Ledger.LoadNotified(n.clk.Now(), o.DedupeFor); err != nil {
			n.log.Error("cannot read the notification ledger; dedupe starts empty", "error", err.Error())
		} else {
			n.sent = sent
		}
	}
	for _, t := range o.Targets {
		w := &worker{t: t, q: make(chan Message, o.QueueSize), nt: n, throttled: map[string]time.Time{}}
		n.targets = append(n.targets, w)
		if t.Sender != nil {
			n.wg.Add(1)
			go w.run()
		}
	}
	n.wg.Add(1)
	go n.dispatch()
	return n
}

// Handle is the bus subscriber. The engine calls it while it holds its lock, so it only queues: no rendering, no I/O.
// If the queue is full the event is dropped and counted.
func (n *Notifier) Handle(e engine.Event) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.closed {
		return
	}
	select {
	case n.in <- e:
	default:
		n.droppedEvents.Add(1)
		n.log.Warn("notification queue full, event dropped", "type", string(e.Type), "key", e.Key)
	}
}

// Shutdown stops taking events, delivers what is queued for up to grace, and then aborts what is still in flight. The
// last messages before a stop are often the ones that matter (a SUSPENDED the host is about to lose to a reboot).
func (n *Notifier) Shutdown(grace time.Duration) {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	close(n.in)
	n.mu.Unlock()

	done := make(chan struct{})
	go func() { n.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		n.cancel()
		<-done
	}
	n.cancel()
	for _, w := range n.targets {
		if w.t.Sender != nil {
			_ = w.t.Sender.Close()
		}
	}
}

// dispatch renders, deduplicates and fans out. It runs off the engine's goroutine, so it may take its time.
func (n *Notifier) dispatch() {
	defer n.wg.Done()
	defer func() {
		for _, w := range n.targets {
			close(w.q)
		}
	}()
	for e := range n.in {
		n.routeSafely(e)
	}
}

// routeSafely keeps one bad event (a template bug) from ending the dispatcher.
func (n *Notifier) routeSafely(e engine.Event) {
	defer func() {
		if r := recover(); r != nil {
			n.droppedEvents.Add(1)
			n.log.Error("notification dropped: panic while handling an event", "type", string(e.Type), "key", e.Key, "panic", n.o.Redact.Redact(fmt.Sprint(r)))
		}
	}()
	n.route(e)
}

func (n *Notifier) route(e engine.Event) {
	e = n.redacted(e)
	switch e.Type {
	case engine.ActionStarted:
		// Only remembered: the message goes out when the action ends, and says why it was started.
		n.reasons[e.Key] = afterPrefix(e.Message, e.Attrs["action"]+": ")
		return
	}
	switch {
	case e.Type == engine.DriftBlocked:
		n.foldNext[e.Key] = e.Attrs[engine.AttrIncident]
	case e.Type == engine.Suspended && e.Attrs["reason"] == engine.ReasonDrift:
		inc, fold := n.foldNext[e.Key]
		delete(n.foldNext, e.Key)
		if fold && inc == e.Attrs[engine.AttrIncident] {
			return // the DriftBlocked just before it said all of this: one message, not two
		}
	case e.Type == engine.Unsuspended:
		delete(n.foldNext, e.Key)
	}
	c := rctx{host: n.o.Host, dryRun: n.o.DryRun}
	if e.Type == engine.ActionResult {
		c.reason = n.reasons[e.Key]
		delete(n.reasons, e.Key)
	}
	m, id, ok := render(e, c)
	if !ok {
		return
	}
	if id != "" && n.seen(id, e) {
		return
	}
	// Suspended and unsuspended alternate within an incident; each is news again after the other, or the last word the
	// user heard would be wrong. Only once per incident (plan v3.13 item 18): the first Unsuspended after a Suspended and
	// the first Suspended after that are sent; a healthcheck that keeps flipping does not use up the hourly budget.
	switch e.Type {
	case engine.Unsuspended, engine.Suspended:
		if inc := e.Attrs[engine.AttrIncident]; !n.rearmed(e.Key, inc) {
			if e.Type == engine.Unsuspended {
				n.markRearmed(e.Key, inc)
				n.forget(engine.Suspended, e.Key)
			} else {
				n.forget(engine.Unsuspended, e.Key)
			}
		}
	}
	for _, w := range n.targets {
		w.offer(m)
	}
}

// redacted is the event the templates see: its free text redacted, so no body carries a secret that an error quoted (and
// URL #1's token is not pushed to URL #2's service). The rollback commands are pasted into a shell, so they are sent as
// they were built or not at all: when the redactor would change either, both are withheld and the message says so.
func (n *Notifier) redacted(e engine.Event) engine.Event {
	tag, up := e.Attrs["rollback_tag"], e.Attrs["rollback_up"]
	r := n.o.Redact.Event(e)
	if tag != "" || up != "" {
		if r.Attrs["rollback_tag"] == tag && r.Attrs["rollback_up"] == up {
			return r
		}
		delete(r.Attrs, "rollback_tag")
		delete(r.Attrs, "rollback_up")
		r.Attrs[attrRollbackWithheld] = "true"
	}
	return r
}

// attrRollbackWithheld marks, inside the notifier only, an event whose rollback commands were withheld.
const attrRollbackWithheld = "rollback_withheld"

// rearmType is the ledger type of the "made each other news again" mark of an incident. It is its own type, so forgetting
// the Suspended or Unsuspended rows of a key never drops it.
const rearmType = "rearmed"

func rearmID(key, incident string) string { return rearmType + "|" + key + "|" + incident }

// rearmed reports whether an Unsuspended already made Suspended news again in this incident of the key.
func (n *Notifier) rearmed(key, incident string) bool {
	at, ok := n.sent[rearmID(key, incident)]
	return ok && n.clk.Now().Sub(at) < n.o.DedupeFor
}

func (n *Notifier) markRearmed(key, incident string) {
	now := n.clk.Now()
	n.sent[rearmID(key, incident)] = now
	if n.o.Ledger != nil {
		n.o.Ledger.SaveNotified(rearmID(key, incident), rearmType, key, now)
	}
}

// forget drops the remembered messages of one type for one key.
func (n *Notifier) forget(typ engine.EventType, key string) {
	prefix := string(typ) + "|" + key + "|"
	for id := range n.sent {
		if strings.HasPrefix(id, prefix) {
			delete(n.sent, id)
		}
	}
	if n.o.Ledger != nil {
		n.o.Ledger.ForgetNotified(string(typ), key)
	}
}

// seen records the identity and reports whether it was already sent within DedupeFor.
func (n *Notifier) seen(id string, e engine.Event) bool {
	now := n.clk.Now()
	if at, ok := n.sent[id]; ok && now.Sub(at) < n.o.DedupeFor {
		return true
	}
	n.sent[id] = now
	if n.o.Ledger != nil {
		n.o.Ledger.SaveNotified(id, string(e.Type), e.Key, now)
	}
	if len(n.sent) > 4096 {
		for k, at := range n.sent {
			if now.Sub(at) >= n.o.DedupeFor {
				delete(n.sent, k)
			}
		}
	}
	return false
}

// offer applies the URL's rate limit and queues the message for its worker.
func (w *worker) offer(m Message) {
	if w.t.Sender == nil {
		return
	}
	w.mu.Lock()
	now := w.nt.clk.Now()
	if m.Class != "" && m.MinSpacing > 0 {
		if last, ok := w.throttled[m.Class]; ok && now.Sub(last) < m.MinSpacing {
			w.throttledN++
			w.mu.Unlock()
			return
		}
	}
	cut := now.Add(-time.Hour)
	kept := w.stamps[:0]
	for _, s := range w.stamps {
		if s.After(cut) {
			kept = append(kept, s)
		}
	}
	w.stamps = kept
	// Routine messages may use the first half of the hour's budget, critical ones all of it, so a storm of routine ones
	// cannot swallow the one that says a container was given up on.
	limit := w.nt.o.RateLimit
	if !m.Critical {
		limit = (limit + 1) / 2
	}
	if len(w.stamps) >= limit {
		w.suppressed++
		w.limitedN++
		w.mu.Unlock()
		w.nt.log.Warn("notification held back by the rate limit", "url", w.t.Label, "limit_per_hour", w.nt.o.RateLimit)
		return
	}
	w.stamps = append(w.stamps, now)
	if m.Class != "" && m.MinSpacing > 0 {
		w.throttled[m.Class] = now
	}
	if w.suppressed > 0 {
		m.Body = withPinnedNote(m, fmt.Sprintf("(%d earlier messages were held back by the rate limit)", w.suppressed))
		w.suppressed = 0
	}
	w.mu.Unlock()
	select {
	case w.q <- m:
	default:
		w.mu.Lock()
		w.droppedN++
		w.mu.Unlock()
		w.nt.log.Warn("notification queue full for this URL, message dropped", "url", w.t.Label)
	}
}

// withNote appends the note as a last line and keeps the body within max runes by cutting the details before it (the key
// line comes first and stays).
func withNote(body, note string, max int) string {
	room := max - utf8.RuneCountInString(note) - 1
	return fit(strings.Split(body, "\n"), room) + "\n" + note
}

// withPinnedNote adds the note to a message's body. A message with pinned lines (the rollback commands) gets it before
// them: the note and the details share the room the commands leave, and the commands stay whole and last.
func withPinnedNote(m Message, note string) string {
	if m.pinned == "" || !strings.HasSuffix(m.Body, m.pinned) {
		return withNote(m.Body, note, MaxBody)
	}
	head := strings.TrimSuffix(m.Body, m.pinned)
	return withNote(head, note, MaxBodyPinned-utf8.RuneCountInString(m.pinned)) + m.pinned
}

// run delivers one URL's messages in order.
func (w *worker) run() {
	defer w.nt.wg.Done()
	for m := range w.q {
		w.deliver(m)
	}
}

func (w *worker) deliver(m Message) {
	o := w.nt.o
	var err error
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(w.nt.sendCtx, o.Timeout)
		err = w.send(ctx, m)
		cancel()
		w.mu.Lock()
		w.attemptsRun++
		w.mu.Unlock()
		if err == nil || attempt >= len(o.Backoff) || w.nt.sendCtx.Err() != nil {
			break
		}
		select {
		case <-time.After(o.Backoff[attempt]):
		case <-w.nt.sendCtx.Done():
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err == nil {
		w.sentN++
		w.lastOK = w.nt.clk.Now()
		return
	}
	w.failedN++
	w.lastErr, w.lastErrAt = o.Redact.Err(err), w.nt.clk.Now()
	w.nt.log.Warn("notification failed", "url", w.t.Label, "error", w.lastErr, "title", m.Title)
}

// send calls the Sender and turns a panic into a failed delivery: nothing a notification service does may take down the
// healer.
func (w *worker) send(ctx context.Context, m Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return w.t.Sender.Send(ctx, m)
}

// TargetStatus is one URL in the status: its label and counts, never the URL.
type TargetStatus struct {
	URL         string    `json:"url"` // "#2 discord"
	Sent        int64     `json:"sent"`
	Failed      int64     `json:"failed"`
	RateLimited int64     `json:"rate_limited"`
	Throttled   int64     `json:"throttled,omitempty"` // messages of a class that has a minimum spacing, skipped inside it
	Dropped     int64     `json:"dropped"`
	LastError   string    `json:"last_error,omitempty"` // redacted
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
	LastOK      time.Time `json:"last_ok,omitzero"`
	Invalid     string    `json:"invalid,omitempty"` // why the URL cannot be used, redacted
}

// Status is the notification part of `whalenurse status`.
type Status struct {
	Enabled       bool           `json:"enabled"`
	DryRun        bool           `json:"dry_run,omitempty"`
	Targets       []TargetStatus `json:"targets,omitempty"`
	DroppedEvents int64          `json:"dropped_events,omitempty"`
	Ping          *PingStatus    `json:"uptime_ping,omitempty"`
}

// Status snapshots the counters.
func (n *Notifier) Status() Status {
	s := Status{Enabled: len(n.targets) > 0, DryRun: n.o.DryRun, DroppedEvents: n.droppedEvents.Load()}
	for _, w := range n.targets {
		w.mu.Lock()
		s.Targets = append(s.Targets, TargetStatus{
			URL: w.t.Label, Sent: w.sentN, Failed: w.failedN, RateLimited: w.limitedN, Throttled: w.throttledN, Dropped: w.droppedN,
			LastError: w.lastErr, LastErrorAt: w.lastErrAt, LastOK: w.lastOK, Invalid: w.t.Err,
		})
		w.mu.Unlock()
	}
	return s
}

// Label is the printable name of the i-th URL (0-based): "#1 ntfy".
func Label(i int, rawURL string) string { return fmt.Sprintf("#%d %s", i+1, schemeOf(rawURL)) }

// Summary is one line for the CLI and the log: how many URLs, never which.
func (s Status) Summary() string {
	if !s.Enabled {
		return "none configured (WHALENURSE_NOTIFY_URLS)"
	}
	var parts []string
	for _, t := range s.Targets {
		switch {
		case t.Invalid != "":
			parts = append(parts, fmt.Sprintf("%s INVALID (%s)", t.URL, t.Invalid))
		default:
			p := fmt.Sprintf("%s: %d sent", t.URL, t.Sent)
			if t.Failed > 0 {
				p += fmt.Sprintf(", %d failed (last: %s)", t.Failed, t.LastError)
			}
			if t.RateLimited > 0 {
				p += fmt.Sprintf(", %d held back by the rate limit", t.RateLimited)
			}
			if t.Dropped > 0 {
				p += fmt.Sprintf(", %d dropped", t.Dropped)
			}
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "; ")
}
