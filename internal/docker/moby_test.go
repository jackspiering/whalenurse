package docker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// fakeEngine is just enough of the Engine API for Moby.Events: the version
// negotiation ping and /events, which the handler answers.
func fakeEngine(t *testing.T, events http.HandlerFunc) *Moby {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("Api-Version", "1.51")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/events"):
			events(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &Moby{c: c}
}

func writeEvent(w http.ResponseWriter, action string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"Type":"container","Action":%q,"Actor":{"ID":"abc","Attributes":{"name":"web"}},"timeNano":%d}`+"\n", action, time.Now().UnixNano())
	w.(http.Flusher).Flush()
}

// The moby client reports the end of the stream on Err only and never closes
// Messages, so the adapter must end Events itself: otherwise a dropped stream
// is never noticed and whalenurse runs without events until it is restarted.
func TestMobyEventsEndsWhenTheDaemonDropsTheStream(t *testing.T) {
	m := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		writeEvent(w, "die")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := m.Events(ctx, time.Time{})
	select {
	case ev := <-s.Events:
		if ev.Action != "die" || ev.Name != "web" {
			t.Fatalf("event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
	select {
	case _, ok := <-s.Events:
		if ok {
			t.Fatal("a second event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Events not closed after the daemon ended the stream")
	}
	select {
	case err := <-s.Err:
		if err == nil {
			t.Fatal("no reason for the end of the stream")
		}
	default:
		t.Fatal("Events closed before Err carried the reason")
	}
}

func TestMobyEventsEndsWhenTheDaemonCannotBeReached(t *testing.T) {
	m := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "daemon shutting down", http.StatusServiceUnavailable)
	})
	s := m.Events(context.Background(), time.Time{})
	select {
	case _, ok := <-s.Events:
		if ok {
			t.Fatal("an event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Events not closed after a failed subscription")
	}
	if err := <-s.Err; err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("err = %v", err)
	}
}

func TestMobyEventsEndsWhenTheContextIsCancelled(t *testing.T) {
	m := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		writeEvent(w, "start")
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	s := m.Events(ctx, time.Time{})
	<-s.Events
	cancel()
	select {
	case _, ok := <-s.Events:
		if ok {
			t.Fatal("an event after the cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Events not closed after the context was cancelled")
	}
	if err := <-s.Err; err == nil {
		t.Fatal("no reason")
	}
}
