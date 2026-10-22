package engine

import (
	"sync"
	"time"
)

// EventType names a domain event. Store, notify and the API subscribe to one
// stream of these (plan §5), so history and notifications cannot disagree.
type EventType string

const (
	IncidentOpened      EventType = "IncidentOpened"
	IncidentClosed      EventType = "IncidentClosed" // closed without recovery: gone, stopped by hand, recreated
	ActionStarted       EventType = "ActionStarted"
	ActionResult        EventType = "ActionResult"
	ActionInterrupted   EventType = "ActionInterrupted" // whalenurse was stopped while the action ran and it was cut short: the rung is not spent
	RungUnavailable     EventType = "RungUnavailable"   // a configured rung cannot run: disabled by the version gate, no usable compose inputs, or not built yet (pull)
	Suspended           EventType = "Suspended"         // the ladder is spent, or the key is flapping; nothing more is tried
	Recovered           EventType = "Recovered"         // recovered for RECOVERY_PERIOD: the incident closes
	Unsuspended         EventType = "Unsuspended"
	Paused              EventType = "Paused"
	Resumed             EventType = "Resumed" // by the resume command, or because the pause expired
	RateLimited         EventType = "RateLimited"
	NeedsRecreate       EventType = "NeedsRecreate" // it needs a recreate and none is available (version gate, no usable compose labels, recreate not enabled)
	WaitingForParent    EventType = "WaitingForParent"
	NotifyOnly          EventType = "NotifyOnly"
	ActionRefused       EventType = "ActionRefused" // the pre-action check found a vanished parent
	EventAttributed     EventType = "EventAttributed"
	ManualStopMarked    EventType = "ManualStopMarked"
	ManualStopCleared   EventType = "ManualStopCleared"
	MarkersQuarantined  EventType = "MarkersQuarantined" // a stream loss or a new boot id discarded manual-stop markers
	RestoreWindowOpened EventType = "RestoreWindowOpened"
	RestoreEvent        EventType = "RestoreEvent" // a kill or start seen inside the restore window; interpreted as a restore
	DriftBlocked        EventType = "DriftBlocked" // the drift guard skipped a recreate; the container is SUSPENDED
	DriftWarned         EventType = "DriftWarned"  // WHALENURSE_DRIFT_GUARD=warn: the recreate went ahead despite what the guard found
	RungsDisabled       EventType = "RungsDisabled"
	DependentPostCheck  EventType = "DependentPostCheck" // a dependent was still stale after a non-forced up: recreating it with --force-recreate
	TagMoved            EventType = "TagMoved"           // rung 3 pulled a newer image: the tag now points elsewhere, for every stack that shares it
	Upgraded            EventType = "Upgraded"           // rung 2 recreated a container of a pull-enabled service onto an image that moved under it ("upgraded during heal")
	UpgradePostCheck    EventType = "UpgradePostCheck"   // after the pull's non-forced up the container was still the old one: recreating it with --force-recreate
)

// Event is one domain event. IDs are monotonic per host and times are UTC, so
// the stream can later serve as the hub's `since` cursor.
type Event struct {
	ID        uint64            `json:"id"`
	Time      time.Time         `json:"time"`
	Host      string            `json:"host"`
	Key       string            `json:"key"`
	Container string            `json:"container,omitempty"`
	Type      EventType         `json:"type"`
	Message   string            `json:"message"`
	Attrs     map[string]string `json:"attrs,omitempty"`
}

// Bus fans events out, in order, to subscribers. Subscribers run
// synchronously inside Publish and must not block.
type Bus struct {
	mu   sync.Mutex
	next uint64
	subs []func(Event)
}

// Seed makes the next event's ID n+1: the daemon seeds the bus with the highest ID the store ever stored, so that IDs
// stay monotonic per host across restarts (the API's `since` cursor). Call it before the first Publish.
func (b *Bus) Seed(n uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.next {
		b.next = n
	}
}

// Subscribe registers a subscriber for all later events.
func (b *Bus) Subscribe(fn func(Event)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, fn)
}

// Publish stamps the ID and delivers the event.
func (b *Bus) Publish(e Event) Event {
	b.mu.Lock()
	b.next++
	e.ID = b.next
	e.Time = e.Time.UTC()
	subs := append([]func(Event){}, b.subs...)
	b.mu.Unlock()
	for _, fn := range subs {
		fn(e)
	}
	return e
}

// Recorder is a subscriber that keeps every event, for tests and /status.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

// Record is the subscriber function.
func (r *Recorder) Record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// Events returns a copy of everything recorded.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// Types returns the recorded event types in order.
func (r *Recorder) Types() []EventType {
	var out []EventType
	for _, e := range r.Events() {
		out = append(out, e.Type)
	}
	return out
}

// Count returns how many events of the type were recorded.
func (r *Recorder) Count(t EventType) int {
	n := 0
	for _, e := range r.Events() {
		if e.Type == t {
			n++
		}
	}
	return n
}

// EventQuery selects events from an EventLog.
type EventQuery struct {
	After  uint64      // only IDs greater than this
	Before uint64      // only IDs less than this; 0 for no bound
	Key    string      // only this state key; empty for all
	Types  []EventType // only these types; empty for all
	// ExcludeTypes are never returned (the dashboard hides the EventAttributed noise by default).
	ExcludeTypes []EventType
	Limit        int  // at most this many; 0 means 100
	Newest       bool // newest first (descending IDs) instead of oldest first
}

// Matches reports whether the event satisfies the query's filters (not its limit or order).
func (q EventQuery) Matches(e Event) bool {
	if e.ID <= q.After || (q.Before != 0 && e.ID >= q.Before) || (q.Key != "" && e.Key != q.Key) {
		return false
	}
	for _, t := range q.ExcludeTypes {
		if t == e.Type {
			return false
		}
	}
	if len(q.Types) == 0 {
		return true
	}
	for _, t := range q.Types {
		if t == e.Type {
			return true
		}
	}
	return false
}

// EventLog is where the API reads events from: the store, or the in-memory Ring in a dry run.
type EventLog interface {
	Query(EventQuery) ([]Event, error)
	// LastID is the highest ID ever assigned, which stays valid after retention deleted the newest rows.
	LastID() uint64
}

// Ring keeps the last events in memory. A dry run stores nothing, so the dashboard reads this instead of the store;
// its IDs restart with the process, which the API reports as a reset.
type Ring struct {
	mu     sync.Mutex
	max    int
	events []Event
	last   uint64
}

// NewRing keeps up to max events (5000 when max is 0).
func NewRing(max int) *Ring {
	if max <= 0 {
		max = 5000
	}
	return &Ring{max: max}
}

// Record is the bus subscriber.
func (r *Ring) Record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = e.ID
	r.events = append(r.events, e)
	if over := len(r.events) - r.max; over > 0 {
		r.events = append(r.events[:0], r.events[over:]...)
	}
}

// LastID implements EventLog.
func (r *Ring) LastID() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Query implements EventLog.
func (r *Ring) Query(q EventQuery) ([]Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	var out []Event
	if q.Newest {
		for i := len(r.events) - 1; i >= 0 && len(out) < limit; i-- {
			if q.Matches(r.events[i]) {
				out = append(out, r.events[i])
			}
		}
		return out, nil
	}
	for _, e := range r.events {
		if len(out) >= limit {
			break
		}
		if q.Matches(e) {
			out = append(out, e)
		}
	}
	return out, nil
}
