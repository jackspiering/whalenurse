package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/engine"
)

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sampleKey() engine.KeyRecord {
	return engine.KeyRecord{
		Key: "h/p/web/1", Name: "p-web-1", MarkerAt: t0.Add(-time.Minute),
		Loop:  engine.LoopRecord{Have: true, LastCount: 4, LastObs: t0, N: 3, First: t0.Add(-6 * time.Minute), Last: t0.Add(-time.Minute)},
		Flaps: []time.Time{t0.Add(-2 * time.Hour), t0},
		Incident: &engine.IncidentRecord{
			Opened: t0, Class: "unhealthy", Rung: 2, CooldownUntil: t0.Add(time.Minute), Spent: true, SpentReason: "ladder",
			Suspended: true, SuspendedAt: t0.Add(30 * time.Second), Recovering: true, RecoverySince: t0.Add(time.Hour), RecoveryCount: 7,
		},
	}
}

func TestStateSurvivesReopeningTheDatabase(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	k := sampleKey()
	s.SaveKey(k)
	s.SavePause(engine.Pause{Scope: "p/web", Until: t0.Add(2 * time.Hour), Created: t0})
	s.SavePause(engine.Pause{Scope: "q", Created: t0}) // indefinite
	s.SaveAction(t0.Add(-10 * time.Minute))
	s.SaveAction(t0.Add(-2 * time.Hour)) // outside the rate-limit hour
	s.SetMeta("boot_id", "abc")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, dir)
	defer s.Close()
	snap, err := s.Load(t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Keys) != 1 || !reflect.DeepEqual(snap.Keys[0], k) {
		t.Fatalf("key did not round-trip:\n got %+v\nwant %+v", snap.Keys, k)
	}
	if len(snap.Pauses) != 2 || snap.Pauses[0].Scope != "p/web" || !snap.Pauses[0].Until.Equal(t0.Add(2*time.Hour)) || !snap.Pauses[1].Until.IsZero() {
		t.Fatalf("%+v", snap.Pauses)
	}
	if len(snap.Actions) != 1 || !snap.Actions[0].Equal(t0.Add(-10*time.Minute)) {
		t.Fatalf("only the last hour of actions counts: %+v", snap.Actions)
	}
	if v, ok := s.Meta("boot_id"); !ok || v != "abc" {
		t.Fatalf("%q %v", v, ok)
	}
	if _, ok := s.Meta("nope"); ok {
		t.Fatal("missing meta reported present")
	}
}

func TestKeyWithoutAnIncidentAndAClearedMarker(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	s.SaveKey(sampleKey())
	k := sampleKey()
	k.Incident, k.MarkerAt = nil, time.Time{} // everything cleared
	s.SaveKey(k)
	snap, _ := s.Load(t0)
	if len(snap.Keys) != 1 || snap.Keys[0].Incident != nil || !snap.Keys[0].MarkerAt.IsZero() {
		t.Fatalf("%+v", snap.Keys)
	}
	hist, _ := s.History("")
	if len(hist) != 1 || hist[0].Outcome != "cleared" {
		t.Fatalf("an incident the engine dropped without saying why is history too: %+v", hist)
	}
}

func TestClosedIncidentsBecomeHistoryAndRetentionPrunesThem(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	s.SaveKey(sampleKey())
	s.CloseIncident(engine.ClosedIncident{Key: "h/p/web/1", Opened: t0, Closed: t0.Add(time.Hour), Class: "unhealthy", Outcome: "recovered"})
	old := engine.ClosedIncident{Key: "h/p/old/1", Opened: t0.Add(-40 * 24 * time.Hour), Closed: t0.Add(-39 * 24 * time.Hour), Class: "crash-loop", Outcome: "recovered"}
	s.CloseIncident(old)

	snap, _ := s.Load(t0)
	if snap.Keys[0].Incident != nil {
		t.Fatalf("the closed incident is still loaded as open: %+v", snap.Keys[0].Incident)
	}
	hist, err := s.History("")
	if err != nil || len(hist) != 2 || hist[0].Key != "h/p/web/1" {
		t.Fatalf("%+v %v", hist, err)
	}
	n, err := s.Prune(t0.Add(2*time.Hour), 30*24*time.Hour, 24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	hist, _ = s.History("")
	if len(hist) != 1 || hist[0].Key != "h/p/web/1" {
		t.Fatalf("%+v", hist)
	}
	if h, _ := s.History("h/p/old/1"); len(h) != 0 {
		t.Fatalf("%+v", h)
	}
}

func TestPruneDropsOldFlapAndActionRows(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	s.SaveAction(t0.Add(-2 * time.Hour))
	if _, err := s.db.Exec(`INSERT INTO flaps (key, at) VALUES ('x', ?), ('x', ?)`, tn(t0.Add(-30*time.Hour)), tn(t0.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(t0, 30*24*time.Hour, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var flaps, actions int
	s.db.QueryRow(`SELECT count(*) FROM flaps`).Scan(&flaps)
	s.db.QueryRow(`SELECT count(*) FROM actions`).Scan(&actions)
	if flaps != 1 || actions != 0 {
		t.Fatalf("flaps=%d actions=%d", flaps, actions)
	}
}

func TestDeleteKeyForgetsItButKeepsHistory(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	s.SaveKey(sampleKey())
	s.DeleteKey("h/p/web/1")
	snap, _ := s.Load(t0)
	if len(snap.Keys) != 0 {
		t.Fatalf("%+v", snap.Keys)
	}
	if hist, _ := s.History(""); len(hist) != 1 || hist[0].Outcome != "gone" {
		t.Fatalf("%+v", hist)
	}
}

func TestOnlyOneOpenIncidentPerKey(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	a := sampleKey()
	s.SaveKey(a)
	b := sampleKey()
	b.Incident = &engine.IncidentRecord{Opened: t0.Add(time.Hour), Class: "crash-loop"}
	s.SaveKey(b) // a newer incident: the stale open row is closed, not left to violate the unique index
	var open int
	s.db.QueryRow(`SELECT count(*) FROM incidents WHERE closed_at IS NULL`).Scan(&open)
	if open != 1 || s.WriteErrors() != 0 {
		t.Fatalf("open=%d write errors=%d", open, s.WriteErrors())
	}
}

func TestRefusesADatabaseFromTheFuture(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.db.Exec("PRAGMA user_version = 99")
	s.Close()
	if _, err := Open(dir, nil); err == nil {
		t.Fatal("opened a schema newer than this build understands")
	}
}

func TestOpenCreatesTheDirectoryAndIsReopenable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	for i := 0; i < 2; i++ {
		s, err := Open(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version %d", v)
	}
}

func TestAWriteToAClosedStoreIsCountedNotFatal(t *testing.T) {
	s := open(t, t.TempDir())
	s.Close()
	s.SaveKey(sampleKey())
	if s.WriteErrors() == 0 {
		t.Fatal("a failed write must be counted")
	}
}

func TestNotificationLedgerSurvivesReopeningAndExpires(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.SaveNotified("Suspended|h/p/web/1|ladder|i1", "Suspended", "h/p/web/1", t0.Add(-time.Hour))
	s.SaveNotified("NotifyOnly|h/p/db/1|c/|", "NotifyOnly", "h/p/db/1", t0.Add(-30*time.Hour))
	s.SaveNotified("Suspended|h/p/web/1|ladder|i1", "Suspended", "h/p/web/1", t0) // saving again only moves the time
	s.Close()

	s = open(t, dir)
	defer s.Close()
	got, err := s.LoadNotified(t0, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["Suspended|h/p/web/1|ladder|i1"].Equal(t0) {
		t.Fatalf("the 30 h old row must be gone and the other kept: %v", got)
	}
	s.ForgetNotified("Suspended", "h/p/web/1")
	if got, _ := s.LoadNotified(t0, 24*time.Hour); len(got) != 0 {
		t.Fatalf("forgotten rows came back: %v", got)
	}
	s.SaveNotified("a", "T", "k", t0.Add(-2*time.Hour))
	if err := s.PruneNotified(t0, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LoadNotified(t0, 24*time.Hour); len(got) != 0 {
		t.Fatalf("PruneNotified left %v", got)
	}
	if s.WriteErrors() != 0 {
		t.Fatalf("%d write errors", s.WriteErrors())
	}
}

// A database written by M2 (schema 1) opens, keeps its rows, and gets the ledger table.
func TestSchemaOneDatabaseIsMigrated(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.SaveKey(sampleKey())
	s.db.Exec(`DROP TABLE notified`)
	s.db.Exec(`DROP TABLE events`)
	s.db.Exec("PRAGMA user_version = 1")
	s.Close()

	s = open(t, dir)
	defer s.Close()
	snap, err := s.Load(t0)
	if err != nil || len(snap.Keys) != 1 {
		t.Fatalf("%v %v", snap.Keys, err)
	}
	s.SaveNotified("x", "T", "k", t0)
	if got, err := s.LoadNotified(t0, time.Hour); err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
}

// A schema-2 database (M3/M4) gets the events table and keeps its rows.
func TestSchemaTwoDatabaseIsMigratedToEvents(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.SaveKey(sampleKey())
	s.SaveNotified("x", "T", "k", t0)
	s.db.Exec(`DROP TABLE events`)
	s.db.Exec("PRAGMA user_version = 2")
	s.Close()

	s = open(t, dir)
	defer s.Close()
	if got, err := s.LoadNotified(t0, time.Hour); err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	s.RecordEvent(engine.Event{ID: 1, Time: t0, Host: "h", Key: "h/p/web/1", Type: engine.IncidentOpened, Message: "m"})
	if got, err := s.Query(engine.EventQuery{}); err != nil || len(got) != 1 || s.LastID() != 1 {
		t.Fatalf("%v %v %d", got, err, s.LastID())
	}
}

func ev(id uint64, at time.Time, key string, typ engine.EventType, attrs map[string]string) engine.Event {
	return engine.Event{ID: id, Time: at, Host: "h", Key: key, Container: "c", Type: typ, Message: "m" + string(typ), Attrs: attrs}
}

func TestEventsRoundTripAndFilter(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	s.RecordEvent(ev(1, t0, "h/p/web/1", engine.IncidentOpened, map[string]string{"class": "unhealthy"}))
	s.RecordEvent(ev(2, t0.Add(time.Second), "h/p/db/1", engine.ActionResult, nil))
	s.RecordEvent(ev(3, t0.Add(2*time.Second), "h/p/web/1", engine.Suspended, map[string]string{"reason": "ladder"}))
	s.RecordEvent(ev(3, t0, "h/p/web/1", engine.Suspended, nil)) // the same ID twice: the first one stays

	all, err := s.Query(engine.EventQuery{})
	if err != nil || len(all) != 3 || all[0].ID != 1 || all[2].Attrs["reason"] != "ladder" || !all[0].Time.Equal(t0) || all[0].Time.Location() != time.UTC {
		t.Fatalf("%+v %v", all, err)
	}
	if got, _ := s.Query(engine.EventQuery{After: 1}); len(got) != 2 || got[0].ID != 2 {
		t.Fatalf("after: %+v", got)
	}
	if got, _ := s.Query(engine.EventQuery{Key: "h/p/web/1", Newest: true}); len(got) != 2 || got[0].ID != 3 {
		t.Fatalf("key, newest: %+v", got)
	}
	if got, _ := s.Query(engine.EventQuery{Types: []engine.EventType{engine.ActionResult, engine.Suspended}, Limit: 1}); len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("types, limit: %+v", got)
	}
	if got, _ := s.Query(engine.EventQuery{Before: 3}); len(got) != 2 {
		t.Fatalf("before: %+v", got)
	}
	if got, _ := s.Query(engine.EventQuery{ExcludeTypes: []engine.EventType{engine.ActionResult, engine.Suspended}}); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("exclude: %+v", got)
	}
}

// Monotonic IDs: the counter survives a reopen and retention deleting every row.
func TestEventIDsStayMonotonicAcrossRestartAndRetention(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	for i := uint64(1); i <= 5; i++ {
		s.RecordEvent(ev(i, t0.Add(-48*time.Hour), "h/p/web/1", engine.IncidentOpened, nil))
	}
	if s.LastID() != 5 {
		t.Fatal(s.LastID())
	}
	if _, err := s.Prune(t0, 24*time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Query(engine.EventQuery{}); len(got) != 0 {
		t.Fatalf("retention kept %d old events", len(got))
	}
	s.Close()

	s = open(t, dir)
	defer s.Close()
	if s.LastID() != 5 {
		t.Fatalf("the counter went back to %d after everything was pruned", s.LastID())
	}
	var bus engine.Bus
	bus.Seed(s.LastID())
	if e := bus.Publish(engine.Event{Time: t0}); e.ID != 6 {
		t.Fatalf("the next ID is %d", e.ID)
	}
}

func TestEventRowsAreCapped(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	tx, _ := s.db.Begin()
	for i := 1; i <= MaxEvents+50; i++ {
		tx.Exec(`INSERT INTO events (id, at, host, key, container, type, message, attrs) VALUES (?, ?, 'h', 'k', '', 'T', '', '{}')`, i, tn(t0))
	}
	tx.Commit()
	if err := s.pruneEvents(t0, time.Hour); err != nil {
		t.Fatal(err)
	}
	var n int
	var first uint64
	s.db.QueryRow(`SELECT COUNT(*), MIN(id) FROM events`).Scan(&n, &first)
	if n != MaxEvents || first != 51 {
		t.Fatalf("%d rows, first %d", n, first)
	}
}

// The API reads through its own connection: it sees what the writer committed, and it cannot write.
func TestReadsUseASeparateReadOnlyConnection(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	if s.rdb == s.db {
		t.Fatal("no separate read connection")
	}
	s.RecordEvent(ev(1, t0, "h/p/web/1", engine.IncidentOpened, nil))
	s.SaveKey(sampleKey())
	if got, err := s.Query(engine.EventQuery{}); err != nil || len(got) != 1 || s.LastID() != 1 {
		t.Fatalf("%v %v", got, err)
	}
	s.CloseIncident(engine.ClosedIncident{Key: "h/p/web/1", Opened: t0, Closed: t0.Add(time.Minute), Class: "c", Outcome: "recovered"})
	if h, err := s.History(""); err != nil || len(h) != 1 {
		t.Fatalf("%v %v", h, err)
	}
	if _, err := s.rdb.Exec(`DELETE FROM events`); err == nil {
		t.Fatal("the read connection can write")
	}
	// A busy writer does not stop a read.
	tx, _ := s.db.Begin()
	tx.Exec(`INSERT INTO events (id, at, host, key, container, type, message, attrs) VALUES (9, 0, 'h', 'k', '', 'T', '', '{}')`)
	done := make(chan struct{})
	go func() { s.Query(engine.EventQuery{}); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a read waited for the writer's open transaction")
	}
	tx.Rollback()
}

// Two whalenurse processes on one data directory would both heal, each from its own copy of the state: the second Open
// is refused while the first store is open, and allowed once it is closed. (flock locks belong to the open file, so two
// Opens in one process conflict just as two processes do.)
func TestASecondOpenOfTheSameDirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := Open(dir, nil); !errors.Is(err, ErrLocked) {
		if b != nil {
			b.Close()
		}
		t.Fatalf("second Open: %v, want ErrLocked", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	b.Close()
}
