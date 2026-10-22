package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/engine"
)

func get(t *testing.T, h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Host = "127.0.0.1:8484" // httptest's default, example.com, is not a name a tokenless listener serves
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type okChecker struct{}

func (okChecker) Healthy() (bool, string) { return true, "ok" }

func handler(t *testing.T, token string) http.Handler {
	t.Helper()
	return Handler(Options{Health: okChecker{}, Service: fixtureService(), Token: token})
}

// The golden files are the hub contract: any change to a document shows up here.
func TestGoldenJSON(t *testing.T) {
	h := handler(t, "")
	for _, c := range []struct{ name, path string }{
		{"status.json", "/api/v1/status"},
		{"containers.json", "/api/v1/containers"},
		{"containers-all.json", "/api/v1/containers?all=1"},
		{"container-vpn.json", "/api/v1/containers/nas/media/vpn/1"},
		{"container-jellyfin.json", "/api/v1/containers/nas/media/jellyfin/1"},
		{"events.json", "/api/v1/events"},
		{"events-since-3.json", "/api/v1/events?since=3&limit=2"},
		{"events-filtered.json", "/api/v1/events?key=nas/media/vpn/1&type=DriftBlocked,Suspended"},
		{"events-reset.json", "/api/v1/events?since=999"},
		{"incidents.json", "/api/v1/incidents"},
	} {
		rec := get(t, h, c.path)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %d %s", c.path, rec.Code, rec.Header().Get("Content-Type"))
		}
		golden(t, c.name, rec.Body.Bytes())
	}
}

func TestEveryRecordCarriesTheHostAndUTCTimes(t *testing.T) {
	h := handler(t, "")
	for _, p := range []string{"/api/v1/status", "/api/v1/containers", "/api/v1/containers/nas/media/vpn/1", "/api/v1/events", "/api/v1/incidents"} {
		body := get(t, h, p).Body.String()
		var v any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		walk(t, p, v, false)
	}
}

// timeKeys are the fields of the documents that hold a time.
var timeKeys = map[string]bool{
	"time": true, "started_at": true, "restore_window_until": true, "next_slot": true, "created": true, "until": true, "since": true,
	"opened": true, "closed": true, "at": true, "incident_opened": true, "cooldown_until": true, "recovery_since": true,
	"finished_at": true, "moved_at": true,
}

// walk checks that every record has a host and every time is UTC.
func walk(t *testing.T, where string, v any, _ bool) {
	switch x := v.(type) {
	case map[string]any:
		if _, isRecord := x["key"]; isRecord && (x["host"] == nil || x["host"] == "") {
			t.Errorf("%s: record without host: %v", where, x["key"])
		}
		for k, c := range x {
			if s, ok := c.(string); ok && timeKeys[k] && !strings.HasSuffix(s, "Z") {
				t.Errorf("%s: %s=%q is not UTC", where, k, s)
			}
			walk(t, where, c, false)
		}
	case []any:
		for _, c := range x {
			walk(t, where, c, true)
		}
	}
}

func TestEventCursorIsMonotonicAndResets(t *testing.T) {
	s := fixtureService()
	d := s.EventsPage(EventsQuery{})
	if len(d.Events) != 7 || d.Next != 7 || d.LastID != 7 || d.Reset {
		t.Fatalf("%+v", d)
	}
	for i := 1; i < len(d.Events); i++ {
		if d.Events[i].ID <= d.Events[i-1].ID {
			t.Fatal("IDs are not increasing")
		}
	}
	// Following the cursor returns nothing new and keeps the cursor.
	d = s.EventsPage(EventsQuery{Since: 7})
	if len(d.Events) != 0 || d.Next != 7 {
		t.Fatalf("%+v", d)
	}
	// A cursor from the future is a reset: start over.
	d = s.EventsPage(EventsQuery{Since: 50})
	if !d.Reset || len(d.Events) != 7 || d.Next != 7 {
		t.Fatalf("%+v", d)
	}
	if d := get(t, handler(t, ""), "/api/v1/events?since=abc"); d.Code != 400 {
		t.Fatalf("bad cursor: %d", d.Code)
	}
	if d := get(t, handler(t, ""), "/api/v1/events?limit=0"); d.Code != 400 {
		t.Fatalf("bad limit: %d", d.Code)
	}
	if d := get(t, handler(t, ""), "/api/v1/containers/nas/nothing/1"); d.Code != 404 {
		t.Fatalf("unknown container: %d", d.Code)
	}
	if d := get(t, handler(t, ""), "/api/v1/nothing"); d.Code != 404 {
		t.Fatalf("unknown endpoint: %d", d.Code)
	}
}

func TestContainerKeyWithEscapedSlashesAndSpaces(t *testing.T) {
	h := handler(t, "")
	for _, p := range []string{"/api/v1/containers/nas%2Fmedia%2Fvpn%2F1", "/api/v1/containers/nas/media/vpn/1"} {
		if rec := get(t, h, p); rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

func TestDetailHasDriftUpgradeAndSuspendCause(t *testing.T) {
	d, ok := fixtureService().Container("nas/media/vpn/1")
	if !ok {
		t.Fatal("no detail")
	}
	if d.Drift.Status != "blocked" || !d.Drift.Blocking || d.Drift.MovedBy != "nas/media/other/1" || d.Drift.MovedAt.IsZero() {
		t.Errorf("drift: %+v", d.Drift)
	}
	u := d.LastUpgrade
	if u == nil || u.Kind != "TagMoved" || !strings.HasPrefix(u.RollbackTag, "docker image tag") || !strings.Contains(u.RollbackUp, "up -d") || len(u.Sidecars) != 1 {
		t.Errorf("upgrade: %+v", u)
	}
	if d.Healer.Suspended.Cause == "" || d.Timeline[0].ID < d.Timeline[len(d.Timeline)-1].ID {
		t.Errorf("cause %q, timeline order %v", d.Healer.Suspended.Cause, d.Timeline)
	}
	if len(d.Incidents) < 2 || !d.Incidents[0].Open {
		t.Errorf("incidents: %+v", d.Incidents)
	}
	if j, _ := fixtureService().Container("nas/media/jellyfin/1"); j.Drift.Status != "none-recorded" || j.LastUpgrade != nil {
		t.Errorf("a container with no drift events: %+v", j.Drift)
	}
}

// ---- output rules -------------------------------------------------------------------------------------

func TestOnlyComposeAndWhalenurseLabelsAndKnownAttrsLeave(t *testing.T) {
	s := fixtureService()
	for _, c := range s.Containers(true) {
		for k := range c.Labels {
			if !labelAllowed(k) {
				t.Errorf("%s: label %q came out", c.Key, k)
			}
		}
	}
	e := s.EventDoc(engine.Event{ID: 1, Time: t0, Type: engine.ActionResult, Message: "m", Attrs: map[string]string{
		"action": "restart", "labels": "password=hunter2", "env": "TOKEN=abc", "com.example.password": "x",
	}})
	if len(e.Attrs) != 1 || e.Attrs["action"] != "restart" {
		t.Errorf("attrs: %v", e.Attrs)
	}
}

func TestRedactionAndLengthCaps(t *testing.T) {
	s := fixtureService()
	s.Redact = func(v string) string { return strings.ReplaceAll(v, "SECRET-TOKEN-1234", "***") }
	long := strings.Repeat("x", 5000)
	e := s.EventDoc(engine.Event{ID: 1, Time: t0, Message: "failed: SECRET-TOKEN-1234 " + long, Attrs: map[string]string{"error": "SECRET-TOKEN-1234"}})
	if strings.Contains(e.Message, "SECRET") || e.Attrs["error"] != "***" || len([]rune(e.Message)) > maxText {
		t.Errorf("%q %v", e.Message[:60], e.Attrs)
	}
	c := s.clean(ContainerDoc{StartError: "bind SECRET-TOKEN-1234 " + long})
	if strings.Contains(c.StartError, "SECRET") || len([]rune(c.StartError)) > maxErrorText {
		t.Errorf("start error not cleaned")
	}
}

// ---- security -------------------------------------------------------------------------------------------

var allRoutes = []string{
	"/", "/healthz", "/metrics", "/containers", "/history", "/static/app.js",
	"/api/v1/status", "/api/v1/containers", "/api/v1/containers/nas/media/vpn/1", "/api/v1/events", "/api/v1/incidents", "/api/v1/stream",
	"/v1/pause", "/v1/resume", "/v1/unsuspend", "/v1/status", // the control socket's paths: they must not exist here
}

func TestEveryNonGETRequestIsRefused(t *testing.T) {
	h := Handler(Options{Health: okChecker{}, Service: fixtureService(), Token: "tok"})
	for _, path := range allRoutes {
		for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT", "PROPFIND"} {
			// With and without credentials: the refusal does not depend on them.
			for _, auth := range []string{"", "Bearer tok"} {
				req := httptest.NewRequest(m, path, strings.NewReader(`{"scope":"p"}`))
				if auth != "" {
					req.Header.Set("Authorization", auth)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s %s (auth %q): %d", m, path, auth, rec.Code)
				}
				if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Allow-Methods") != "" {
					t.Errorf("%s %s: CORS headers", m, path)
				}
				if a := rec.Header().Get("Allow"); a != "GET, HEAD" {
					t.Errorf("%s %s: Allow %q", m, path, a)
				}
			}
		}
	}
}

func TestControlSocketPathsDoNotExistOnTCP(t *testing.T) {
	h := Handler(Options{Health: okChecker{}, Service: fixtureService()})
	for _, p := range []string{"/v1/pause", "/v1/resume", "/v1/unsuspend", "/v1/status"} {
		if rec := get(t, h, p); rec.Code != 404 {
			t.Errorf("GET %s: %d", p, rec.Code)
		}
	}
}

func TestHEADWorksAndSendsNoBody(t *testing.T) {
	srv := httptest.NewServer(handler(t, ""))
	defer srv.Close()
	resp, err := http.Head(srv.URL + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(b) != 0 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q %v", resp.StatusCode, b, resp.Header)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := handler(t, "tok")
	for _, c := range []struct {
		path string
		auth bool
	}{{"/healthz", false}, {"/api/v1/status", true}, {"/api/v1/status", false}, {"/nothing", true}} {
		var rec *httptest.ResponseRecorder
		if c.auth {
			rec = get(t, h, c.path, "Authorization", "Bearer tok")
		} else {
			rec = get(t, h, c.path)
		}
		hd := rec.Header()
		for k, want := range map[string]string{
			"Content-Security-Policy": CSP, "X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
		} {
			if hd.Get(k) != want {
				t.Errorf("%s %s: %s = %q", c.path, rec.Result().Status, k, hd.Get(k))
			}
		}
		if !strings.Contains(CSP, "frame-ancestors 'none'") || !strings.Contains(CSP, "script-src 'self'") || strings.Contains(CSP, "unsafe-inline") {
			t.Fatal("CSP: " + CSP)
		}
	}
}

func TestTokenIsRequiredExceptForHealthz(t *testing.T) {
	h := handler(t, "s3cret-token-value")
	basic := func(user, pass string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	}
	for _, c := range []struct {
		name string
		hdr  []string
		path string
		want int
	}{
		{"no credentials", nil, "/api/v1/status", 401},
		{"healthz needs none", nil, "/healthz", 200},
		{"bearer", []string{"Authorization", "Bearer s3cret-token-value"}, "/api/v1/status", 200},
		{"bearer lowercase scheme", []string{"Authorization", "bearer s3cret-token-value"}, "/api/v1/status", 200},
		{"wrong bearer", []string{"Authorization", "Bearer s3cret-token-valuX"}, "/api/v1/status", 401},
		{"prefix of the token", []string{"Authorization", "Bearer s3cret"}, "/api/v1/status", 401},
		{"token plus suffix", []string{"Authorization", "Bearer s3cret-token-value-and-more"}, "/api/v1/status", 401},
		{"empty bearer", []string{"Authorization", "Bearer "}, "/api/v1/status", 401},
		{"basic with the token as password", []string{"Authorization", basic("anyone", "s3cret-token-value")}, "/api/v1/status", 200},
		{"basic with the token as user only", []string{"Authorization", basic("s3cret-token-value", "")}, "/api/v1/status", 401},
		{"basic wrong password", []string{"Authorization", basic("a", "nope")}, "/api/v1/status", 401},
		{"basic garbage", []string{"Authorization", "Basic !!!"}, "/api/v1/status", 401},
		{"other scheme", []string{"Authorization", "Token s3cret-token-value"}, "/api/v1/status", 401},
		{"token in the query is not accepted", nil, "/api/v1/status?token=s3cret-token-value", 401},
		{"static needs it", nil, "/static/app.js", 401},
		{"metrics need it", nil, "/metrics", 401},
	} {
		rec := get(t, h, c.path, c.hdr...)
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.want)
		}
		if rec.Code == 401 {
			if len(rec.Header().Values("WWW-Authenticate")) != 2 || strings.Contains(rec.Body.String(), "s3cret") {
				t.Errorf("%s: challenge %v, body %q", c.name, rec.Header().Values("WWW-Authenticate"), rec.Body.String())
			}
		}
	}
}

func TestNoTokenMeansOpenButStillReadOnly(t *testing.T) {
	h := handler(t, "")
	if rec := get(t, h, "/api/v1/status"); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
}

// ---- SSE ------------------------------------------------------------------------------------------------

type sseClient struct {
	r      *bufio.Reader
	cancel context.CancelFunc
	resp   *http.Response
}

func connect(t *testing.T, url string, hdr ...string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return &sseClient{r: bufio.NewReader(resp.Body), cancel: cancel, resp: resp}
}

func (c *sseClient) close() { c.cancel(); c.resp.Body.Close() }

// next reads one message (up to the blank line), skipping comments.
func (c *sseClient) next(t *testing.T) map[string]string {
	t.Helper()
	got := make(chan map[string]string, 1)
	go func() {
		m := map[string]string{}
		for {
			line, err := c.r.ReadString('\n')
			if err != nil {
				got <- nil
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case line == "":
				if len(m) > 0 {
					got <- m
					return
				}
			case strings.HasPrefix(line, ":"), strings.HasPrefix(line, "retry:"):
			default:
				k, v, _ := strings.Cut(line, ": ")
				m[k] = v
			}
		}
	}()
	select {
	case m := <-got:
		if m == nil {
			t.Fatal("stream ended")
		}
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no message")
		return nil
	}
}

func TestStreamReplaysFromLastEventIDThenFollowsLive(t *testing.T) {
	svc := fixtureService()
	ring := svc.Events.(*engine.Ring)
	srv := httptest.NewServer(Handler(Options{Health: okChecker{}, Service: svc}))
	defer srv.Close()

	c := connect(t, srv.URL+"/api/v1/stream", "Last-Event-ID", "5")
	defer c.close()
	if ct := c.resp.Header.Get("Content-Type"); ct != "text/event-stream" || c.resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("headers: %v", c.resp.Header)
	}
	for _, want := range []string{"6", "7"} {
		m := c.next(t)
		if m["id"] != want {
			t.Fatalf("replay: %v, want id %s", m, want)
		}
	}
	// A live event: the bus stores it first, then wakes the hub.
	e := engine.Event{ID: 8, Time: t0.Add(time.Hour), Host: "nas", Key: "nas/media/vpn/1", Type: engine.Recovered, Message: "recovered"}
	ring.Record(e)
	svc.Hub.Notify(e)
	m := c.next(t)
	var doc EventDoc
	if m["id"] != "8" || m["event"] != "Recovered" || json.Unmarshal([]byte(m["data"]), &doc) != nil || doc.Host != "nas" || doc.Message != "recovered" {
		t.Fatalf("live: %v", m)
	}
}

func TestStreamWithoutACursorStartsNowAndCanBeNarrowedToAKey(t *testing.T) {
	svc := fixtureService()
	ring := svc.Events.(*engine.Ring)
	srv := httptest.NewServer(Handler(Options{Health: okChecker{}, Service: svc}))
	defer srv.Close()
	c := connect(t, srv.URL+"/api/v1/stream?key=nas/web/proxy/1")
	defer c.close()
	for i, key := range []string{"nas/media/vpn/1", "nas/web/proxy/1"} {
		e := engine.Event{ID: uint64(8 + i), Time: t0, Host: "nas", Key: key, Type: engine.ActionResult, Message: "m"}
		ring.Record(e)
		svc.Hub.Notify(e)
	}
	if m := c.next(t); m["id"] != "9" {
		t.Fatalf("expected only the proxy's event (9), not the history or the vpn's: %v", m)
	}
}

func TestStreamResetsWhenTheCursorIsAheadAndEndsOnShutdown(t *testing.T) {
	svc := fixtureService()
	srv := httptest.NewServer(Handler(Options{Health: okChecker{}, Service: svc}))
	defer srv.Close()
	c := connect(t, srv.URL+"/api/v1/stream", "Last-Event-ID", "900")
	defer c.close()
	if m := c.next(t); m["event"] != "reset" {
		t.Fatalf("%v", m)
	}
	if m := c.next(t); m["id"] != "1" {
		t.Fatalf("after a reset the stream starts over: %v", m)
	}
	svc.Hub.Close()
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, c.resp.Body); close(done) }()
	// Drain the replay, then the body must end.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not end on shutdown")
	}
}

func TestStreamsAreBounded(t *testing.T) {
	svc := fixtureService()
	srv := httptest.NewServer(Handler(Options{Health: okChecker{}, Service: svc}))
	defer srv.Close()
	var cs []*sseClient
	for i := 0; i < MaxStreams; i++ {
		c := connect(t, srv.URL+"/api/v1/stream")
		cs = append(cs, c)
	}
	defer func() {
		for _, c := range cs {
			c.close()
		}
	}()
	resp, err := http.Get(srv.URL + "/api/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("the %dth stream: %d", MaxStreams+1, resp.StatusCode)
	}
}

func TestHubDoesNotBlockPublishers(t *testing.T) {
	h := NewHub()
	_, cancel, _ := h.Subscribe() // a client that never reads
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10000; i++ {
			h.Notify(engine.Event{Type: engine.ActionResult})
		}
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Notify blocked on a client that does not read")
	}
	if h.Counts()["ActionResult"] != 10000 {
		t.Fatal(h.Counts())
	}
}

// ---- metrics --------------------------------------------------------------------------------------------

func TestMetrics(t *testing.T) {
	svc := fixtureService()
	svc.Hub.Notify(engine.Event{Type: engine.Suspended})
	rec := get(t, Handler(Options{Health: okChecker{}, Service: svc}), "/metrics")
	body := rec.Body.String()
	for _, want := range []string{
		`whalenurse_info{host="nas",version="v-test",dry_run="false"} 1`,
		"whalenurse_healthy 1", "whalenurse_actions_last_hour 3", "whalenurse_action_rate_limit 10",
		`whalenurse_containers{state="suspended"} 1`, `whalenurse_container_suspended{key="nas/media/vpn/1"} 1`,
		`whalenurse_container_suspended{key="nas/media/jellyfin/1"} 0`, `whalenurse_events_total{type="Suspended"} 1`,
		"# TYPE whalenurse_events_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, "other-1") {
		t.Error("an unwatched container is in /metrics")
	}
	off := fixtureService()
	off.Metrics = false
	if rec := get(t, Handler(Options{Service: off}), "/metrics"); rec.Code != 404 {
		t.Errorf("metrics off: %d", rec.Code)
	}
	if got := labelValue("a\"b\\c\nd"); got != `a\"b\\c\nd` {
		t.Errorf("label escaping: %s", got)
	}
}

// main serves with a ReadTimeout. net/http keeps that deadline on the connection while a handler runs, and its
// background read then cancels the request: without the stream clearing it, every stream would end after the timeout.
func TestStreamSurvivesTheServersReadTimeout(t *testing.T) {
	svc := fixtureService()
	ring := svc.Events.(*engine.Ring)
	srv := httptest.NewUnstartedServer(Handler(Options{Health: okChecker{}, Service: svc}))
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()
	c := connect(t, srv.URL+"/api/v1/stream")
	defer c.close()
	time.Sleep(1200 * time.Millisecond)
	e := engine.Event{ID: 8, Time: t0, Host: "nas", Key: "nas/web/proxy/1", Type: engine.Recovered, Message: "late"}
	ring.Record(e)
	svc.Hub.Notify(e)
	if m := c.next(t); m["id"] != "8" {
		t.Fatalf("the stream did not survive the read timeout: %v", m)
	}
}

// The rollback commands are pasted into a shell: they leave whalenurse exactly as they were built, or not at all. A
// redactor fragment in one of them (an ntfy topic "docker", a receiver on the registry's host) withholds both, in the
// container document and in every event.
func TestRollbackIsShownExactlyOrNotAtAll(t *testing.T) {
	const tag = "docker image tag sha256:aaaaaaaaaaaaaaaa qmcgaw/gluetun:latest"
	const up = "docker compose -p media --project-directory /opt/stacks/media -f compose.yaml up -d --no-deps --pull never vpn"
	for name, redact := range map[string]func(string) string{
		"a fragment in both":        func(v string) string { return strings.ReplaceAll(v, "docker", "***") },
		"a fragment in the up only": func(v string) string { return strings.ReplaceAll(v, "/opt/stacks", "***") },
	} {
		t.Run(name, func(t *testing.T) {
			s := fixtureService()
			s.Redact = redact
			d, ok := s.Container("nas/media/vpn/1")
			if !ok || d.LastUpgrade == nil {
				t.Fatal("no last upgrade")
			}
			if u := d.LastUpgrade; u.RollbackTag != "" || u.RollbackUp != "" || u.OldImage == "" {
				t.Fatalf("a changed rollback must be withheld, both lines, and the old image kept: %+v", u)
			}
			for _, e := range s.EventsPage(EventsQuery{}).Events {
				if _, has := e.Attrs["rollback_tag"]; has {
					t.Fatalf("event %d still carries the rollback: %+v", e.ID, e.Attrs)
				}
				if _, has := e.Attrs["rollback_up"]; has {
					t.Fatalf("event %d still carries the rollback: %+v", e.ID, e.Attrs)
				}
			}
		})
	}
	// Untouched by the redactor: both, byte for byte.
	s := fixtureService()
	s.Redact = func(v string) string { return strings.ReplaceAll(v, "hunter2", "***") }
	d, _ := s.Container("nas/media/vpn/1")
	if u := d.LastUpgrade; u.RollbackTag != tag || u.RollbackUp != up {
		t.Fatalf("%+v", u)
	}
}
