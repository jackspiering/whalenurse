//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/engine"
)

// M3: notifications reach a local webhook receiver through Shoutrrr's generic service, for real heals of real containers.
// The receiver listens on loopback and the healer runs in this process, so nothing leaves the machine.

type webhook struct {
	mu      sync.Mutex
	msgs    []hookMsg
	srv     *httptest.Server
	project string // only messages about this compose project count: other tests' leftovers on the host talk too
}

type hookMsg struct {
	Title   string `json:"title"`
	Message string `json:"message"`
}

func newWebhook(t *testing.T, project string) *webhook {
	t.Helper()
	w := &webhook{project: project}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m hookMsg
		if err := json.Unmarshal(b, &m); err != nil {
			t.Errorf("the receiver got something that is not JSON: %q", b)
		}
		w.mu.Lock()
		w.msgs = append(w.msgs, m)
		w.mu.Unlock()
	}))
	t.Cleanup(w.srv.Close)
	return w
}

// url is the Shoutrrr URL: the generic service, JSON template, plain http. Its host is "localhost", not 127.0.0.1: the
// parts of a notification URL are secrets to the redactor, and the test registry's references start with 127.0.0.1, so a
// receiver on that address would mask them (and withhold the rollback) in every message and log line.
func (w *webhook) url() string {
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(w.srv.URL, "http://"))
	return "generic://localhost:" + port + "/hook?template=json&disabletls=yes"
}

func (w *webhook) all() []hookMsg {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []hookMsg
	for _, m := range w.msgs {
		if strings.Contains(m.Message, w.project) {
			out = append(out, m)
		}
	}
	return out
}

// matching returns the messages whose title contains every one of parts.
func (w *webhook) matching(parts ...string) []hookMsg {
	var out []hookMsg
next:
	for _, m := range w.all() {
		for _, p := range parts {
			if !strings.Contains(m.Title, p) {
				continue next
			}
		}
		out = append(out, m)
	}
	return out
}

func titles(ms []hookMsg) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Title)
	}
	return out
}

// deadURL is a webhook nobody listens on: connection refused.
func deadURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	return "generic://localhost:" + port + "/hook-dead-token-4242?template=json&disabletls=yes" // localhost: see webhook.url

}

// unhealthy -> restart -> recreate -> SUSPENDED: the receiver hears the two actions and exactly one "giving up" with the
// cause, each naming the host and the container key. A dead URL next to the working one changes nothing for the heal or
// for the working URL.
func TestNotificationsReachTheReceiverWhenTheLadderEndsInSuspended(t *testing.T) {
	g, cli := requireIntegration(t)
	s := stuckStack(t, g, cli, "notify-ladder")
	hook := newWebhook(t, s.project)
	dead := deadURL(t)

	r := startDaemonOpts(t, g, cli, daemonOpts{NotifyURLs: []string{dead, hook.url()}})
	waitFor(t, "the app to be SUSPENDED", 150*time.Second, func() bool { return countFor(r.Rec, engine.Suspended, s.key("app")) == 1 })
	waitFor(t, "three messages at the receiver", 30*time.Second, func() bool { return len(hook.all()) >= 3 })

	key := s.key("app")
	all := hook.all()
	for _, m := range all {
		if !strings.Contains(m.Title, testHost) || !strings.Contains(m.Message, key) {
			t.Errorf("a message does not carry the host and the key %q: %+v", key, m)
		}
		if len(m.Title) > 100 || len(m.Message) > 500 {
			t.Errorf("a message is too long for a phone push: %d / %d", len(m.Title), len(m.Message))
		}
	}
	if got := hook.matching("restarted"); len(got) != 1 {
		t.Errorf("want one restart message: %v", titles(all))
	}
	if got := hook.matching("recreated"); len(got) != 1 {
		t.Errorf("want one recreate message: %v", titles(all))
	}
	giving := hook.matching("giving up, SUSPENDED")
	if len(giving) != 1 {
		t.Fatalf("want exactly one giving-up message: %v", titles(all))
	}
	if !strings.Contains(giving[0].Message, "cause:") || !strings.Contains(giving[0].Message, "unhealthy") ||
		!strings.Contains(giving[0].Message, "whalenurse unsuspend "+s.project+"/app") {
		t.Errorf("the giving-up message has no cause or next step: %q", giving[0].Message)
	}

	// SUSPENDED means no more attempts and no more words.
	n := len(hook.all())
	time.Sleep(15 * time.Second)
	if len(hook.all()) != n {
		t.Errorf("more messages after SUSPENDED: %v", titles(hook.all()))
	}
	// The dead URL's failures are counted, not raised, and its token is not in the test log (the redactor's job; the
	// unit tests grep the logs, status and test output for it).
	st := r.D.Status()
	if js, _ := json.Marshal(st); strings.Contains(string(js), "hook-dead-token-4242") {
		t.Errorf("the status shows a URL: %s", js)
	}
}

// A restart does not repair it, a recreate does: restart, recreate, then recovered. The receiver hears all three and no
// "giving up".
func TestNotificationsReachTheReceiverOnRecovery(t *testing.T) {
	g, cli := requireIntegration(t)
	s := newStack(t, g, cli, "notify-repair", "services:"+brokenService("app", ""))
	s.up()
	waitFor(t, "app to be healthy", 60*time.Second, func() bool { return s.container("app").Health == "healthy" })
	before := s.container("app")
	breakWritableLayer(t, g, s, "app")
	hook := newWebhook(t, s.project)

	r := startDaemonOpts(t, g, cli, daemonOpts{NotifyURLs: []string{hook.url()}})
	waitFor(t, "the app to be recreated and healthy", 150*time.Second, func() bool {
		c := s.container("app")
		return c.ID != before.ID && c.Health == "healthy"
	})
	waitFor(t, "the Recovered event", 60*time.Second, func() bool { return countFor(r.Rec, engine.Recovered, s.key("app")) == 1 })
	waitFor(t, "the recovery message", 30*time.Second, func() bool { return len(hook.matching("recovered")) == 1 })

	if len(hook.matching("restarted")) != 1 || len(hook.matching("recreated")) != 1 || len(hook.matching("SUSPENDED")) != 0 {
		t.Fatalf("restarted, recreated, recovered; not SUSPENDED: %v", titles(hook.all()))
	}
	if got := hook.matching("recovered")[0]; !strings.Contains(got.Message, s.key("app")) {
		t.Errorf("%+v", got)
	}
}

// A dry run notifies too, marked, and changes nothing.
func TestADryRunNotifiesAndMarksTheMessages(t *testing.T) {
	g, cli := requireIntegration(t)
	s := appStack(t, g, cli, "notify-dry")
	before := s.container("app")
	breakApp(t, g, s)
	hook := newWebhook(t, s.project)

	r := startDaemonOpts(t, g, cli, daemonOpts{Dry: true, NotifyURLs: []string{hook.url()}})
	waitFor(t, "the dry-run restart", 90*time.Second, func() bool { return countFor(r.Rec, engine.ActionResult, s.key("app")) >= 1 })
	waitFor(t, "a message at the receiver", 30*time.Second, func() bool { return len(hook.all()) >= 1 })
	for _, m := range hook.all() {
		if !strings.HasPrefix(m.Title, "[dry-run]") || !strings.HasPrefix(m.Message, "[dry-run] ") {
			t.Errorf("a dry-run message is not marked: %+v", m)
		}
	}
	if len(hook.matching("would restart")) == 0 {
		t.Errorf("%v", titles(hook.all()))
	}
	if after := s.container("app"); after.ID != before.ID || after.StartedAt != before.StartedAt {
		t.Fatal("a dry run changed the container")
	}
}
