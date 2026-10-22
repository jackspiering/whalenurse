package daemon

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/api"
	"github.com/jackspiering/whalenurse/internal/compose"
	"github.com/jackspiering/whalenurse/internal/config"
	"github.com/jackspiering/whalenurse/internal/docker"
	"github.com/jackspiering/whalenurse/internal/engine"
	"github.com/jackspiering/whalenurse/internal/notify"
	"github.com/jackspiering/whalenurse/internal/store"
	"github.com/jackspiering/whalenurse/internal/web"
)

// Secrets planted where a leak could come from. None of them may appear in any page, any API response, the event
// stream or /metrics. Every one is a long, unique string, so a plain substring search proves the absence.
const (
	secLabelA   = "lbl-SECRET-apikey-71113"
	secLabelB   = "lbl-SECRET-traefik-71114"
	secEnv      = "env-SECRET-dbpass-71115"
	secUserinfo = "tok-USERINFO-71116"
	secPath     = "tok-PATH-71117"
	secQuery    = "tok-QUERY-71118"
	secPing     = "tok-PINGUUID-71119"
	secHTTP     = "tok-HTTP-BEARER-71120"
	secAttr     = "attr-SECRET-71121"

	// The pull rung's registry credentials (M7): the user, the password, an identity token and the "auth" value (base64 of
	// user:password) of a WHALENURSE_REGISTRY_AUTH_DIR file. The registry host is shown on purpose.
	secRegUser  = "reguser-71122" // a user name is not a secret (it is often the image namespace), but status must not show it
	secRegPass  = "regpass-SECRET-71123"
	secRegToken = "regtoken-SECRET-71124"
	regHost     = "registry.example.lan:5000"
)

var secRegAuth = base64.StdEncoding.EncodeToString([]byte(secRegUser + ":" + secRegPass))

var allSecrets = []string{secLabelA, secLabelB, secEnv, secUserinfo, secPath, secQuery, secPing, secHTTP, secAttr,
	secRegPass, secRegToken, secRegAuth}

// registryAuthDir writes a credentials directory the way the docs say, and loads it the way main does.
func registryAuthDir(t *testing.T) *compose.RegistryAuth {
	t.Helper()
	dir := t.TempDir()
	cfg := `{"auths": {"` + regHost + `": {"auth": "` + secRegAuth + `"}, "ghcr.io": {"identitytoken": "` + secRegToken + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o444); err != nil {
		t.Fatal(err)
	}
	a, err := compose.LoadRegistryAuth(dir)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// leakyActor fails every restart with an error that quotes a notification URL and the HTTP token, as an HTTP client's
// or a compose child's error text can.
type leakyActor struct{ docker.Actor }

// rigUnderTest lets the actor feed Docker events back while it acts, as a real restart does: those events fall in the
// action's window and become EventAttributed.
var rigUnderTest *dashboardRig

func (leakyActor) Restart(_ context.Context, id string) error {
	if r := rigUnderTest; r != nil && r.h != nil {
		if c, err := r.h.fake.Inspect(context.Background(), id); err == nil {
			r.h.event(c, "die", "")
			r.h.event(c, "start", "")
		}
	}
	return errors.New("restart failed: Post \"generic://u:" + secUserinfo + "@host/hook/" + secPath + "?k=" + secQuery + "\": boom " + secHTTP +
		"; pull: unauthorized: user " + secRegUser + " password " + secRegPass + " auth " + secRegAuth + " token " + secRegToken)
}

// dashboardRig is the whole read path: a daemon on a fake Docker and a fake clock, a real SQLite store behind the
// event log, the API service and the dashboard, started the way main starts them.
type dashboardRig struct {
	h    *harness
	srv  *httptest.Server
	svc  *api.Service
	hub  *api.Hub
	st   *store.Store
	urls []string
}

func newRig(t *testing.T, token string) *dashboardRig {
	t.Helper()
	notifyURL := "generic://u:" + secUserinfo + "@127.0.0.1:9/hook/" + secPath + "?k=" + secQuery
	ping := "http://127.0.0.1:9/ping/" + secPing
	t.Setenv("DB_PASSWORD", secEnv) // an environment variable of the process: nothing may dump it
	regAuth := registryAuthDir(t)
	red := notify.NewRedactorWith(regAuth.Secrets, notifyURL, ping, token)

	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hub := api.NewHub()
	rig := &dashboardRig{hub: hub, st: st, urls: []string{notifyURL, ping}}
	rigUnderTest = rig
	t.Cleanup(func() { rigUnderTest = nil })
	rig.h = newHarnessOpts(t, func(c *config.Global) {
		c.HostID = "nas"
		c.HTTPToken, c.NotifyURLs, c.PingURL, c.Metrics = token, []string{notifyURL}, ping, true
	}, func(f *docker.Fake) docker.Actor { return leakyActor{f} }, func(o *Options) {
		o.Persist, o.Meta, o.Version = st, st, "v-test"
		o.PullAuthHosts = regAuth.Hosts
		o.Bus.Seed(st.LastID())
		o.Bus.Subscribe(st.RecordEvent)
		o.Bus.Subscribe(hub.Notify)
		o.NotifyStatus = func() notify.Status {
			return notify.Status{Enabled: true, Targets: []notify.TargetStatus{{URL: "#1 generic", Sent: 2, Failed: 1}}}
		}
	})
	rig.h.planted = map[string]bool{"labels": true, "env": true, "password": true}
	rig.svc = NewAPI(rig.h.d, st, st, hub, red.Redact)
	rig.srv = httptest.NewServer(api.Handler(api.Options{Health: rig.h.d, Service: rig.svc, Token: token, Web: web.Handler(rig.svc)}))
	t.Cleanup(rig.srv.Close)
	return rig
}

// drive runs a real incident through the daemon: an unhealthy container whose restart fails with a leaky error, then
// no recreate (no compose), then SUSPENDED. A container with planted labels is not opted in. A sidecar hangs on the
// first. Docker events go through HandleEvent, the way main feeds them.
func (r *dashboardRig) drive() {
	h := r.h
	a := h.running(hexID("a1"), "p", "web")
	h.set(a.ID, func(c *docker.Container) {
		c.Image, c.RestartPolicy = "nginx:1", "unless-stopped"
		c.Labels[config.LabelActions] = "restart,recreate"
		c.Labels["com.example.apikey"] = secLabelA
		c.Labels["traefik.http.routers.x.rule"] = secLabelB
		c.Error = "failed to fulfil mount request: open /srv/data: no such file or directory"
		c.Labels["com.docker.compose.depends_on"] = "" // an empty label: the page leaves it out
	})
	b := h.running(hexID("b2"), "p", "side")
	h.set(b.ID, func(c *docker.Container) { c.NetworkMode = docker.Ref(a.ID); c.Image = "busybox" })
	o := h.running(hexID("c3"), "other", "thing")
	h.set(o.ID, func(c *docker.Container) {
		delete(c.Labels, config.LabelEnable)
		c.Labels["com.example.apikey"] = secLabelA
		c.Labels["whalenurse.note"] = "visible-whalenurse-label"
	})
	h.tick()
	c := h.set(a.ID, unhealthy)
	h.event(c, "health_status", "unhealthy")
	h.advance(45 * time.Second)
	// A planted attribute on an event that goes through the bus, and the rest of the ladder.
	h.d.Engine().Note(engine.Target{Key: "nas/p/web/1", Name: "p-web-1"}, engine.NeedsRecreate, "note "+secUserinfo,
		map[string]string{"labels": secLabelA, "env": secEnv, "password": secAttr, "reason": "x " + secPath})
	h.advance(10 * time.Minute)
	h.d.Engine().Pause("p/side", time.Hour)
	// An "upgraded during heal" record with its rollback commands, as the daemon's own Note writes it.
	h.d.Engine().Note(engine.Target{Key: "nas/p/web/1", Name: "p-web-1"}, engine.Upgraded, "nginx:1: aaaaaaaaaaaa -> bbbbbbbbbbbb; recreated", map[string]string{
		"ref": "nginx:1", "old": "sha256:" + strings.Repeat("a", 64), "new": "sha256:" + strings.Repeat("b", 64), "old_image": "sha256:" + strings.Repeat("a", 64), "ok": "true",
		"rollback_tag": "docker image tag sha256:" + strings.Repeat("a", 64) + " nginx:1",
		"rollback_up":  "docker compose -p p --project-directory /opt/stacks/p -f /opt/stacks/p/compose.yaml up -d --no-deps --pull never --no-build web",
	})
	// A compose temporary name, as an event of a recreate carries it.
	h.d.Engine().Note(engine.Target{Key: "nas/p/side/1", Name: "0123456789ab_p-side-1"}, engine.ActionResult, "recreate ok", map[string]string{"ok": "true"})
}

func (r *dashboardRig) fetch(t *testing.T, path, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", r.srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// keys are the containers the rig knows, as the dashboard links them.
func (r *dashboardRig) keys() []string {
	var out []string
	for _, c := range r.svc.Containers(true) {
		out = append(out, c.Key)
	}
	return out
}

var inlineScript = regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>`)
var scriptSrc = regexp.MustCompile(`(?is)<script\s[^>]*src=["']/static/app\.js["'][^>]*>\s*</script>`)
var handlerAttr = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
var styleAttr = regexp.MustCompile(`(?i)(\sstyle\s*=|<style[\s>])`)
var jsURL = regexp.MustCompile(`(?i)(href|src|action)\s*=\s*["']\s*javascript:`)

func TestNoSecretReachesAnyPageEndpointOrStream(t *testing.T) {
	r := newRig(t, secHTTP)
	r.drive()
	if len(r.h.rec.Types()) < 6 {
		t.Fatalf("the drive produced too little to prove anything: %v", r.h.rec.Types())
	}

	keys := r.keys()
	if len(keys) != 3 {
		t.Fatalf("keys: %v", keys)
	}
	paths := []string{
		"/api/v1/status", "/api/v1/containers", "/api/v1/containers?all=1", "/api/v1/events?limit=1000", "/api/v1/events?since=0",
		"/api/v1/incidents", "/metrics", "/", "/containers", "/containers?all=1", "/history", "/history?type=ActionResult,Suspended",
		"/history?key=" + url.QueryEscape(keys[0]), "/static/app.js", "/static/app.css",
	}
	for _, k := range keys {
		paths = append(paths, "/api/v1/containers/"+k, "/containers/"+k, "/history?key="+url.QueryEscape(k))
	}
	pages := map[string]string{}
	for _, p := range paths {
		code, body := r.fetch(t, p, secHTTP)
		if code != 200 {
			t.Errorf("GET %s: %d %.200s", p, code, body)
			continue
		}
		pages[p] = body
	}

	// The event stream, from the start: the replay carries every stored event.
	pages["SSE replay"] = r.readStream(t, "0", 400*time.Millisecond)

	for p, body := range pages {
		for _, s := range allSecrets {
			if strings.Contains(body, s) {
				t.Errorf("%s leaks %q", p, s)
			}
		}
		for _, u := range r.urls {
			if strings.Contains(body, u) {
				t.Errorf("%s holds a whole URL", p)
			}
		}
	}

	// A user name is not redacted from messages (it is often the image namespace), but the status documents never carry one.
	if strings.Contains(pages["/api/v1/status"], secRegUser) {
		t.Error("/api/v1/status shows the registry user name")
	}

	// The positive controls: the checks above are not vacuous. The secrets were fed through real paths, the redactor
	// masked them, and what is allowed is shown.
	all := strings.Join(mapValues(pages), "\n")
	for _, want := range []string{"p-web-1", "p-side-1", "restart failed", "***", "whalenurse.note", "visible-whalenurse-label", "com.docker.compose.project", "SUSPENDED", "PAUSED", "NeedsRecreate", "failed to fulfil mount request", regHost, "ghcr.io", "Pull credentials"} {
		if !strings.Contains(all, want) {
			t.Errorf("expected %q somewhere in the output", want)
		}
	}
	if strings.Contains(all, "com.example.apikey") || strings.Contains(all, "traefik.") {
		t.Error("a label outside com.docker.compose.* and whalenurse.* came out")
	}
	// The attribute the engine never emits was dropped; the allowed one ("reason") stayed, redacted.
	_, ev := r.fetch(t, "/api/v1/events?limit=1000", secHTTP)
	if strings.Contains(ev, `"labels"`) || strings.Contains(ev, `"env"`) || !strings.Contains(ev, `"reason": "x ***"`) {
		t.Errorf("event attributes: allow-list not applied:\n%s", ev)
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// readStream connects to the SSE stream with Last-Event-ID and returns what arrived within the time.
func (r *dashboardRig) readStream(t *testing.T, lastID string, wait time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", r.srv.URL+"/api/v1/stream", nil)
	req.Header.Set("Last-Event-ID", lastID)
	req.Header.Set("Authorization", "Bearer "+secHTTP)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		b.WriteString(sc.Text() + "\n")
	}
	return b.String()
}

func TestPagesRenderAndAreCleanHTML(t *testing.T) {
	r := newRig(t, "")
	r.drive()
	paths := []string{"/", "/containers", "/containers?all=1", "/history", "/history?type=Suspended,Unsuspended", "/history?key=" + url.QueryEscape("nas/p/web/1")}
	for _, k := range r.keys() {
		paths = append(paths, "/containers/"+k)
	}
	for _, p := range paths {
		code, body := r.fetch(t, p, "")
		if code != 200 {
			t.Errorf("GET %s: %d %.300s", p, code, body)
			continue
		}
		if !strings.Contains(body, "<!doctype html>") || !strings.Contains(body, `<meta name="viewport"`) {
			t.Errorf("%s: not a full page", p)
		}
		// No inline script, handler, style or javascript: URL: the CSP forbids them and the pages must not need them.
		if n := len(inlineScript.FindAllString(body, -1)); n != len(scriptSrc.FindAllString(body, -1)) {
			t.Errorf("%s: inline <script>", p)
		}
		if handlerAttr.MatchString(body) || styleAttr.MatchString(body) || jsURL.MatchString(body) {
			t.Errorf("%s: an inline handler, style or javascript: URL", p)
		}
		for _, bad := range []string{"http://", "https://", "//cdn", "@import"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s: references %q: no third-party origin may be needed", p, bad)
			}
		}
	}
	if code, _ := r.fetch(t, "/containers/nas/none/zzz/1", ""); code != 404 {
		t.Errorf("unknown container page: %d", code)
	}
	if code, _ := r.fetch(t, "/nothing", ""); code != 404 {
		t.Errorf("unknown page: %d", code)
	}
	if code, _ := r.fetch(t, "/history?since=x", ""); code != 400 {
		t.Errorf("bad filter: %d", code)
	}

	// The content: the overview, the badges with reason and expiry, the detail page.
	_, overview := r.fetch(t, "/", "")
	for _, want := range []string{"Healer", "Version gate", "Action budget", "Needs attention", "p-web-1"} {
		if !strings.Contains(overview, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	_, list := r.fetch(t, "/containers", "")
	if !strings.Contains(list, "SUSPENDED: ladder") || strings.Contains(list, "other-thing-1") {
		t.Errorf("containers page:\n%s", list)
	}
	_, detail := r.fetch(t, "/containers/nas/p/web/1", "")
	for _, want := range []string{"Inspect summary", "Drift guard", "Timeline", "unsuspend p/web", "Dependents", "p-side-1", "restart ✓"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
	// The recreate rung could not run (no compose here): the page says so instead of drawing a tick.
	if !strings.Contains(detail, "recreate skipped – not available") || strings.Contains(detail, "recreate ✓") || !strings.Contains(detail, "Skipped") {
		t.Errorf("a skipped rung must not look done:\n%s", regexp.MustCompile(`(?s).*(<dt>Ladder.*?</dd>).*`).ReplaceAllString(detail, "$1"))
	}
	if !strings.Contains(list, "1 skipped (not available)") {
		t.Errorf("the list lacks the skipped note: %s", regexp.MustCompile(`(?s).*(<td data-label="Incident">.*?</td>).*`).ReplaceAllString(list, "$1"))
	}
	_, side := r.fetch(t, "/containers/nas/p/side/1", "")
	if !strings.Contains(side, "Parent") || !strings.Contains(side, "p-web-1") {
		t.Errorf("sidecar page lacks its parent")
	}
}

func TestDryRunServesTheRingWithNoStore(t *testing.T) {
	h := newHarnessOpts(t, func(c *config.Global) { c.DryRun = true; c.HostID = "nas" }, nil, nil)
	ring := engine.NewRing(0)
	h.d.eng.Pause("x", time.Hour) // an event on the bus is not wired to the ring here; publish by hand below
	svc := NewAPI(h.d, ring, nil, api.NewHub(), nil)
	ring.Record(engine.Event{ID: 1, Time: time.Now(), Host: "nas", Key: "nas/x", Type: engine.Paused, Message: "paused x"})
	srv := httptest.NewServer(api.Handler(api.Options{Health: h.d, Service: svc, Web: web.Handler(svc)}))
	defer srv.Close()
	for _, p := range []string{"/api/v1/status", "/api/v1/events", "/api/v1/incidents", "/", "/history"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
		if p == "/" && !strings.Contains(string(b), "Dry run.") {
			t.Error("no dry-run banner")
		}
		if p == "/api/v1/status" && !strings.Contains(string(b), `"dry_run": true`) || p == "/api/v1/status" && !strings.Contains(string(b), `"persistent": false`) {
			t.Errorf("status: %s", b)
		}
	}
}

// IDs keep counting across a restart of the process: the bus continues after the store's highest ID.
func TestEventIDsContinueAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	run := func() (first, last uint64) {
		st, err := store.Open(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		var bus engine.Bus
		bus.Seed(st.LastID())
		bus.Subscribe(st.RecordEvent)
		first = bus.Publish(engine.Event{Time: time.Now(), Host: "nas", Type: engine.Paused}).ID
		last = bus.Publish(engine.Event{Time: time.Now(), Host: "nas", Type: engine.Resumed}).ID
		return
	}
	f1, l1 := run()
	f2, l2 := run()
	if f1 != 1 || l1 != 2 || f2 != 3 || l2 != 4 {
		t.Fatalf("IDs: %d %d then %d %d", f1, l1, f2, l2)
	}
}

func TestContainerDocsMergeListInspectAndEngine(t *testing.T) {
	r := newRig(t, "")
	r.drive()
	docs := map[string]api.ContainerDoc{}
	for _, c := range r.svc.Containers(true) {
		docs[c.Key] = c
	}
	web, side, other := docs["nas/p/web/1"], docs["nas/p/side/1"], docs["nas/other/thing/1"]
	if side.Healer.Paused == nil || side.Healer.Paused.Scope != "p/side" || side.Healer.Paused.Until.IsZero() {
		t.Errorf("a pause shows on the container it covers, with its expiry: %+v", side.Healer.Paused)
	}
	if !web.Watched || !web.OptedIn || web.Healer.State != "SUSPENDED" || web.Healer.Suspended == nil || web.Healer.Suspended.Reason != "ladder" {
		t.Errorf("web: %+v", web.Healer)
	}
	if web.Healer.RungsRun < 1 || web.Image != "nginx:1" || web.Policy != "unless-stopped" || len(web.Ladder) != 2 || len(web.Dependents) != 1 || web.Dependents[0].Key != "nas/p/side/1" {
		t.Errorf("web: %+v", web)
	}
	if side.Parent == nil || side.Parent.Key != "nas/p/web/1" || !side.Watched {
		t.Errorf("sidecar: parent %+v watched %v", side.Parent, side.Watched)
	}
	if other.Watched || other.Healer.Paused != nil {
		t.Errorf("other: %+v", other)
	}
	if other.Labels["whalenurse.note"] == "" {
		t.Error("whalenurse.* labels are shown")
	}
	st := r.svc.Status()
	if st.Counts.Containers != 3 || st.Counts.Watched != 2 || st.Counts.Suspended != 1 || st.Version != "v-test" || st.Host != "nas" || st.LastEventID == 0 || st.RateLimit.Used == 0 {
		t.Errorf("status: %+v", st)
	}
	if !st.Persistent || st.Notifications.URLs != 1 || st.Notifications.Failed != 1 {
		t.Errorf("status: %+v", st)
	}
	if got := fmt.Sprint(st.VersionGate.RecreateEnabled); got != "false" || st.VersionGate.Reason == "" {
		t.Errorf("no compose: the version gate says %+v", st.VersionGate)
	}
}

var hrefRE = regexp.MustCompile(`href="(/[^"]*)"`)

// section returns the HTML between a heading and the next one.
func section(html, heading string) string {
	i := strings.Index(html, "<h2>"+heading+"</h2>")
	if i < 0 {
		return ""
	}
	rest := html[i+len(heading)+9:]
	if j := strings.Index(rest, "<h2>"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// No link of any page leads to a 404: a pause row links to the containers under its scope, a container that is gone is
// plain text, and every other link points at a page that exists.
func TestEveryLinkOnEveryPageWorks(t *testing.T) {
	r := newRig(t, "")
	r.drive()
	r.h.fake.Remove(hexID("b2"))  // the sidecar is gone: its events and its incident stay, its page does not
	r.h.advance(70 * time.Second) // a poll later the list no longer has it

	pages := []string{"/", "/containers", "/containers?all=1", "/containers?scope=p%2Fside", "/history", "/history?debug=1", "/history?type=Paused,Resumed",
		"/history?key=" + url.QueryEscape("nas/p/web/1")}
	for _, k := range r.keys() {
		pages = append(pages, "/containers/"+k, "/containers/"+k+"?debug=1")
	}
	seen := map[string]bool{}
	checked := 0
	for len(pages) > 0 {
		p := pages[0]
		pages = pages[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		code, body := r.fetch(t, p, "")
		if code != 200 {
			t.Errorf("GET %s: %d", p, code)
			continue
		}
		checked++
		for _, m := range hrefRE.FindAllStringSubmatch(body, -1) {
			link := strings.ReplaceAll(m[1], "&amp;", "&")
			if strings.HasPrefix(link, "/static/") || strings.HasPrefix(link, "/api/") {
				continue
			}
			if !seen[link] && len(seen) < 80 {
				pages = append(pages, link)
			}
		}
	}
	if checked < 12 {
		t.Fatalf("the crawl reached only %d pages", checked)
	}
	_, hist := r.fetch(t, "/history?type=Paused,Resumed", "")
	if !strings.Contains(hist, `href="/containers?scope=p%2fside"`) && !strings.Contains(hist, `href="/containers?scope=p%2Fside"`) {
		t.Errorf("the Paused row should link to the containers under its scope:\n%s", section(hist, "Events"))
	}
	if !strings.Contains(hist, `data-label="Pause scope"`) {
		t.Errorf("the Paused row names a scope, not a container")
	}
	_, scoped := r.fetch(t, "/containers?scope=p%2Fside", "")
	if !strings.Contains(scoped, "p-side-1") && !strings.Contains(scoped, "No containers") {
		t.Errorf("scope page: %s", scoped)
	}
	if code, body := r.fetch(t, "/containers/nas/p/side/1", ""); code != 404 || strings.Contains(body, "page error") {
		t.Errorf("the gone sidecar's page: %d", code)
	}
}

// The EventAttributed events are hidden in "Latest events", the timelines and History, and shown by the debug filter;
// the JSON API still carries them.
func TestEventAttributedIsHiddenUnlessDebug(t *testing.T) {
	r := newRig(t, "")
	r.drive()
	row := `<span class="etype">EventAttributed</span>`
	_, js := r.fetch(t, "/api/v1/events?limit=1000", "")
	if !strings.Contains(js, `"type": "EventAttributed"`) {
		t.Fatal("the control: the API has the events")
	}
	if _, detailJSON := r.fetch(t, "/api/v1/containers/nas/p/web/1", ""); !strings.Contains(detailJSON, "EventAttributed") {
		t.Fatal("the control: the JSON timeline keeps them")
	}
	for _, p := range []string{"/", "/history", "/containers/nas/p/web/1"} {
		_, body := r.fetch(t, p, "")
		if strings.Contains(body, row) {
			t.Errorf("%s shows EventAttributed rows by default", p)
		}
		if !strings.Contains(body, "ActionResult") && p != "/" {
			t.Errorf("%s lost the other events", p)
		}
	}
	for _, p := range []string{"/history?debug=1", "/containers/nas/p/web/1?debug=1"} {
		_, body := r.fetch(t, p, "")
		if !strings.Contains(body, row) {
			t.Errorf("%s with debug lacks the EventAttributed rows", p)
		}
	}
	_, hist := r.fetch(t, "/history", "")
	if !strings.Contains(hist, "Debug (everything)") || !strings.Contains(hist, "debug=1") {
		t.Errorf("the History page has no debug filter")
	}
}

func TestDetailPageDetails(t *testing.T) {
	r := newRig(t, "")
	r.drive()
	_, web := r.fetch(t, "/containers/nas/p/web/1", "")
	// A running container has no "Finished"; the restart policy says where "no" comes from; an empty label is left out.
	if strings.Contains(web, "<dt>Finished</dt>") {
		t.Error("Finished is shown for a running container")
	}
	if !strings.Contains(web, "unless-stopped") {
		t.Error("restart policy")
	}
	if strings.Contains(web, "com.docker.compose.depends_on") {
		t.Error("an empty label is shown")
	}
	_, side := r.fetch(t, "/containers/nas/p/side/1", "")
	if !strings.Contains(side, "no <span class=\"muted\">(compose default)</span>") {
		t.Error("restart policy no is not explained")
	}
	// On its own page the timeline and the incidents have no container column.
	for _, h := range []string{"Timeline", "Incidents"} {
		if sec := section(web, h); strings.Contains(sec, "<th>Container</th>") || sec == "" {
			t.Errorf("%s section: %q", h, sec[:min(len(sec), 200)])
		}
	}
	if !strings.Contains(section(web, "Timeline"), "<th>Time</th>") {
		t.Error("the timeline lost its columns")
	}
	// The overview and History keep it.
	for _, p := range []string{"/", "/history"} {
		if _, b := r.fetch(t, p, ""); !strings.Contains(b, "<th>Container</th>") {
			t.Errorf("%s lost the Container column", p)
		}
	}
	// The parent's state is a badge under Related, and it says why the sidecar may be down.
	if !strings.Contains(side, `badge badge-bad">running, unhealthy · SUSPENDED</span>`) {
		t.Errorf("the parent's state badge:\n%s", section(side, "Related"))
	}
	_, parent := r.fetch(t, "/containers/nas/p/web/1", "")
	if !strings.Contains(parent, "Dependents") || !strings.Contains(parent, `badge-`) {
		t.Error("the dependents of the parent")
	}
}

func TestRollbackIsOneCommandPerLineWithACopyButton(t *testing.T) {
	r := newRig(t, "")
	r.drive()
	_, body := r.fetch(t, "/containers/nas/p/web/1", "")
	blocks := regexp.MustCompile(`(?s)<div class="cmd"><pre class="rollback"><code>(.*?)</code></pre><button type="button" class="copy" data-copy[^>]*>Copy</button></div>`).FindAllStringSubmatch(body, -1)
	if len(blocks) != 2 {
		t.Fatalf("want two command blocks with a copy button each, got %d:\n%s", len(blocks), section(body, "Last upgrade during heal"))
	}
	// Each block is the event's command as a whole, byte for byte.
	wantTag := "docker image tag sha256:" + strings.Repeat("a", 64) + " nginx:1"
	wantUp := "docker compose -p p --project-directory /opt/stacks/p -f /opt/stacks/p/compose.yaml up -d --no-deps --pull never --no-build web"
	if blocks[0][1] != wantTag || blocks[1][1] != wantUp {
		t.Errorf("each block is exactly one command: %q", blocks)
	}
	_, js := r.fetch(t, "/static/app.js", "")
	if !strings.Contains(js, "button[data-copy]") || !strings.Contains(js, "execCommand") {
		t.Error("app.js does not wire the copy buttons")
	}
	_, css := r.fetch(t, "/static/app.css", "")
	if !strings.Contains(css, "white-space: pre") || !strings.Contains(css, ".js .copy") {
		t.Error("the rollback block must not wrap, and the buttons only exist with JavaScript")
	}
}

func TestCompanionNamesAreFinal(t *testing.T) {
	r := newRig(t, "")
	r.drive()
	for _, p := range []string{"/api/v1/events?limit=1000", "/history?debug=1", "/"} {
		_, body := r.fetch(t, p, "")
		if strings.Contains(body, "0123456789ab_") {
			t.Errorf("%s shows compose's temporary name", p)
		}
	}
}

// driftRig is a container that the drift guard really blocked, through the compose fake: restart, then the recreate
// refused because compose says it would recreate, so it is SUSPENDED for drift.
func driftRig(t *testing.T) *dashboardRig {
	t.Helper()
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hub := api.NewHub()
	cf := compose.NewFake()
	cf.OracleFn = func(_ compose.Project, name string) (compose.Oracle, error) { return compose.Recreated(name), nil }
	h := newCompose(t, nil, cf, docker.ServerInfo{}, func(o *Options) {
		o.Persist, o.Meta, o.Version = st, st, "v-test"
		o.Bus.Seed(st.LastID())
		o.Bus.Subscribe(st.RecordEvent)
		o.Bus.Subscribe(hub.Notify)
	})
	web := h.svc(hexID("a1"), "p", "web")
	h.tick()
	h.set(web.ID, unhealthy)
	h.advance(6 * time.Minute)
	if h.count(engine.DriftBlocked) != 1 {
		t.Fatalf("the drift guard did not block: %v", h.rec.Types())
	}
	r := &dashboardRig{h: h.harness, hub: hub, st: st}
	r.svc = NewAPI(h.d, st, st, hub, nil)
	r.srv = httptest.NewServer(api.Handler(api.Options{Health: h.d, Service: r.svc, Web: web2(r.svc)}))
	t.Cleanup(r.srv.Close)
	return r
}

func web2(s *api.Service) http.Handler { return web.Handler(s) }

func TestADriftSuspensionReadsInPlainLanguage(t *testing.T) {
	r := driftRig(t)
	_, page := r.fetch(t, "/containers/h/p/web/1", "")

	// The banner: one plain sentence, the raw text under details.
	i := strings.Index(page, "<strong>SUSPENDED (")
	if i < 0 {
		t.Fatalf("no SUSPENDED banner:\n%s", page)
	}
	banner := page[i:]
	banner = strings.ReplaceAll(banner[:strings.Index(banner, "</div>")], "&#39;", "'")
	for _, want := range []string{"Unhealthy; a restart didn't help, and the recreate was blocked because the configuration or image changed since the container was created.",
		`<details class="raw"><summary>details</summary>`, "recreate skipped by the drift guard (config: compose would recreate this container (its dry-run says:"} {
		if !strings.Contains(banner, want) {
			t.Errorf("the banner lacks %q:\n%s", want, banner)
		}
	}
	if plain := banner[:strings.Index(banner, "<details")]; strings.Contains(plain, "dry-run says") || strings.Contains(plain, "recreate skipped by the drift guard (") {
		t.Errorf("the nested phrase is still in the sentence:\n%s", plain)
	}

	// The ladder marks the blocked rung, and the list says so.
	if !strings.Contains(page, "restart ✓ → recreate ✗ blocked (drift)") {
		t.Errorf("ladder:\n%s", regexp.MustCompile(`(?s).*(<dt>Ladder.*?</dd>).*`).ReplaceAllString(page, "$1"))
	}
	_, list := r.fetch(t, "/containers", "")
	if !strings.Contains(list, "recreate blocked (drift)") {
		t.Errorf("list lacks the blocked rung")
	}

	// The timeline: the Suspended row is short and does not repeat the drift explanation the DriftBlocked row carries.
	tl := section(page, "Timeline")
	rows := strings.Split(tl, "<tr>")
	var susp, blocked string
	for _, row := range rows {
		switch {
		case strings.Contains(row, `class="etype">Suspended<`):
			susp = row
		case strings.Contains(row, `class="etype">DriftBlocked<`):
			blocked = row
		}
	}
	if susp == "" || blocked == "" {
		t.Fatalf("timeline rows missing:\n%s", tl)
	}
	visible := func(row string) string {
		if i := strings.Index(row, "<details"); i >= 0 {
			return row[:i]
		}
		return row
	}
	if !strings.Contains(visible(susp), "the recreate was blocked by the drift guard, so the container is SUSPENDED.") || strings.Contains(visible(susp), "changed since") {
		t.Errorf("the Suspended row:\n%s", susp)
	}
	if !strings.Contains(visible(blocked), "compose would recreate this container: the configuration or the image changed since it was created") {
		t.Errorf("the DriftBlocked row:\n%s", blocked)
	}
}

// What a person reads in the pages, not the machine forms: a pause says "for 1h, until 19:05"; the upgrade card has no raw
// "...; recreated" line under its badge; a card lists no field that says "none".
func TestFriendlyTextsAndCards(t *testing.T) {
	r := newRig(t, "")
	r.drive()
	_, hist := r.fetch(t, "/history?type=Paused,Resumed", "")
	if !regexp.MustCompile(`paused p/side for 1h, until \d\d:\d\d`).MatchString(hist) && !regexp.MustCompile(`paused p/side for 1h, until \d+ \w{3} \d\d:\d\d`).MatchString(hist) {
		t.Errorf("pause row:\n%s", section(hist, "Events"))
	}
	if regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\)?<`).MatchString(strings.ReplaceAll(section(hist, "Events"), `datetime="`, `X`)) && strings.Contains(section(hist, "Events"), "(until 20") {
		t.Errorf("the pause row still shows an ISO timestamp")
	}
	if strings.Contains(hist, "for 1h0m0s") {
		t.Error("the Go duration is shown")
	}
	_, web := r.fetch(t, "/containers/nas/p/web/1", "")
	up := section(web, "Last upgrade during heal")
	if up == "" || strings.Contains(up, "; recreated") || !strings.Contains(up, "Upgraded") {
		t.Errorf("the upgrade card:\n%s", up)
	}
	// The cells that say "none" are marked, and the stylesheet hides them in a card.
	_, list := r.fetch(t, "/containers", "")
	if n := strings.Count(list, `class="none"`); n < 3 {
		t.Errorf("only %d cells marked none", n)
	}
	for _, row := range strings.Split(list, "<tr>")[1:] {
		for _, cell := range regexp.MustCompile(`(?s)<td data-label="(Health|Incident|Cooldown)"([^>]*)>(.*?)</td>`).FindAllStringSubmatch(row, -1) {
			if strings.Contains(cell[3], ">none<") && !strings.Contains(cell[2], `class="none"`) {
				t.Errorf("a %s cell says none and is not marked: %s", cell[1], cell[0])
			}
		}
	}
	_, css := r.fetch(t, "/static/app.css", "")
	if !regexp.MustCompile(`@media \(max-width: 56\.25rem\) \{[^@]*\.grid td\.none \{ display: none; \}`).MatchString(css) {
		t.Error("the stylesheet does not hide the none cells in cards")
	}
	// The copy button is beside the code, not on top of it.
	if !strings.Contains(css, ".cmd { display: flex;") || strings.Contains(css, ".copy { display: none; position: absolute") {
		t.Error("the copy button must sit beside the scroll area")
	}
}

// A rollback that the redactor would change is withheld on the page, with a sentence that says so, and no Copy button sits
// next to masked text. The rig's notification URL is on 127.0.0.1, so a test-registry reference holds a fragment.
func TestRollbackThatTheRedactorWouldChangeIsWithheld(t *testing.T) {
	r := newRig(t, "")
	r.drive()
	ref := "127.0.0.1:5000/app:1"
	old := "sha256:" + strings.Repeat("c", 64)
	r.h.d.Engine().Note(engine.Target{Key: "nas/p/web/1", Name: "p-web-1"}, engine.Upgraded, ref+": cccccccccccc -> dddddddddddd; recreated", map[string]string{
		"ref": ref, "old": old, "new": "sha256:" + strings.Repeat("d", 64), "old_image": old, "ok": "true",
		"rollback_tag": "docker image tag " + old + " " + ref,
		"rollback_up":  "docker compose -p p --project-directory /opt/stacks/p -f /opt/stacks/p/compose.yaml up -d --no-deps --pull never --no-build web",
	})
	_, body := r.fetch(t, "/containers/nas/p/web/1", "")
	up := section(body, "Last upgrade during heal")
	if strings.Contains(up, "data-copy") || strings.Contains(up, `class="rollback"`) {
		t.Fatalf("a withheld rollback has no command block or Copy button:\n%s", up)
	}
	if !strings.Contains(up, "The rollback commands are withheld") {
		t.Fatalf("the page must say the rollback is withheld:\n%s", up)
	}
	_, js := r.fetch(t, "/api/v1/containers/nas/p/web/1", "")
	if strings.Contains(js, "docker image tag "+old) || strings.Contains(js, "docker image tag sha256:"+strings.Repeat("c", 64)+" ***") {
		t.Fatalf("the API withholds it too:\n%s", js)
	}
}
