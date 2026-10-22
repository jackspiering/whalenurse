// Package web is the read-only dashboard: html/template pages and two embedded assets, no Node build, no CDN, no
// inline script or style (the Content-Security-Policy forbids both). It renders the documents of internal/api and
// nothing else: it imports no engine, store or daemon code, so a page can only show what the API says, and the API
// is where the output rules (no env vars, no full inspect, allow-listed labels, redaction) are enforced.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackspiering/whalenurse/internal/api"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Handler serves the dashboard: "/", "/containers", "/containers/{key...}", "/history" and "/static/". Mount it on the
// TCP listener behind api.Secure, which adds the headers, refuses every method but GET and HEAD and checks the token.
func Handler(svc *api.Service) http.Handler {
	pages := map[string]*template.Template{}
	for _, name := range []string{"overview", "containers", "container", "history", "error"} {
		pages[name] = template.Must(template.New(name).Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/"+name+".html"))
	}
	h := &handler{svc: svc, pages: pages}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.overview)
	mux.HandleFunc("GET /containers", h.containers)
	mux.HandleFunc("GET /containers/{key...}", h.container)
	mux.HandleFunc("GET /history", h.history)
	fileServer := http.StripPrefix("/static/", http.FileServerFS(static))
	mux.HandleFunc("GET /static/{file...}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, max-age=300")
		fileServer.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.fail(w, http.StatusNotFound, "Not found", "There is no such page.")
	})
	return mux
}

type handler struct {
	svc   *api.Service
	pages map[string]*template.Template
}

// page is the data every template gets; the page fills the rest.
type page struct {
	Title  string
	Page   string
	Host   string
	Status api.StatusDoc

	// overview
	Attention []api.ContainerDoc
	Events    eventTable

	// containers
	All         bool
	ScopeFilter string // ?scope=: only the containers under a pause scope
	Containers  []api.ContainerDoc

	// container
	Detail   *api.ContainerDetailDoc
	Scope    string // project/service, as `whalenurse unsuspend` takes it
	Timeline eventTable
	Debug    bool          // ?debug=1: the events the dashboard hides by default are shown
	Banner   template.HTML // the SUSPENDED banner's sentence

	// history
	Incidents  incidentTable
	Filters    []filter
	Key        string
	Retention  string
	Persistent bool

	// error
	Heading string
	Message string
}

type filter struct {
	Label  string
	Href   string
	Active bool
}

func (h *handler) base(name, title string) page {
	return page{Title: title, Page: name, Host: h.svc.Host, Status: h.svc.Status()}
}

func (h *handler) render(w http.ResponseWriter, name string, code int, p page) {
	var buf bytes.Buffer
	if err := h.pages[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		http.Error(w, "page error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = buf.WriteTo(w)
}

func (h *handler) fail(w http.ResponseWriter, code int, heading, msg string) {
	p := h.base("error", heading)
	p.Heading, p.Message = heading, msg
	h.render(w, "error", code, p)
}

// eventRow is an event with what the page needs to show its container: a link only where the page it points to exists
// (a container that is gone, or a pause scope, must never lead to a 404).
type eventRow struct {
	api.EventDoc
	Label string        // what to show in the Container column
	Link  string        // empty: no link
	Scope bool          // Label is a pause scope
	Text  template.HTML // the What column
}

type eventTable struct {
	Rows          []eventRow
	ShowContainer bool
}

type incidentRow struct {
	api.IncidentDoc
	Link string
}

type incidentTable struct {
	Rows          []incidentRow
	ShowContainer bool
}

// known is the set of container keys the daemon sees now: the only keys with a page.
func (h *handler) known() map[string]bool {
	m := map[string]bool{}
	for _, c := range h.svc.Containers(true) {
		m[c.Key] = true
	}
	return m
}

func eventRows(evs []api.EventDoc, known map[string]bool) []eventRow {
	rows := make([]eventRow, 0, len(evs))
	for _, e := range evs {
		r := eventRow{EventDoc: e, Text: eventText(e.Type, e.Message)}
		switch scope := e.Attrs["scope"]; {
		case scope != "" && (e.Type == "Paused" || e.Type == "Resumed"):
			// A pause covers a project or a service, so it links to the list of the containers under it.
			r.Label, r.Scope, r.Link = scope, true, "/containers?scope="+url.QueryEscape(scope)
		case e.Key == "":
			r.Label = ""
		default:
			r.Label = e.Container
			if r.Label == "" {
				r.Label = e.Key
			}
			if known[e.Key] {
				r.Link = containerPath(e.Key)
			}
		}
		rows = append(rows, r)
	}
	return rows
}

func incidentRows(in []api.IncidentDoc, known map[string]bool) []incidentRow {
	rows := make([]incidentRow, 0, len(in))
	for _, i := range in {
		r := incidentRow{IncidentDoc: i}
		if known[i.Key] {
			r.Link = containerPath(i.Key)
		}
		rows = append(rows, r)
	}
	return rows
}

// inScope mirrors the engine's rule for a pause scope: <project> or <project>/<service> against host/project/service/n.
func inScope(scope, key string) bool {
	_, rest, ok := strings.Cut(key, "/")
	if !ok {
		return false
	}
	have, want := strings.Split(rest, "/"), strings.Split(scope, "/")
	if len(want) > len(have) {
		return false
	}
	for i := range want {
		if have[i] != want[i] {
			return false
		}
	}
	return true
}

func (h *handler) overview(w http.ResponseWriter, _ *http.Request) {
	p := h.base("overview", "Overview")
	for _, c := range h.svc.Containers(false) {
		if c.Healer.State != "ok" || c.Healer.Paused != nil {
			p.Attention = append(p.Attention, c)
		}
	}
	p.Events = eventTable{ShowContainer: true,
		Rows: eventRows(h.svc.EventsPage(api.EventsQuery{Newest: true, Limit: 10, HideNoise: true}).Events, h.known())}
	h.render(w, "overview", http.StatusOK, p)
}

func (h *handler) containers(w http.ResponseWriter, r *http.Request) {
	p := h.base("containers", "Containers")
	p.All = r.URL.Query().Get("all") == "1"
	p.ScopeFilter = strings.Trim(r.URL.Query().Get("scope"), "/")
	for _, c := range h.svc.Containers(p.All) {
		if p.ScopeFilter == "" || inScope(p.ScopeFilter, c.Key) {
			p.Containers = append(p.Containers, c)
		}
	}
	h.render(w, "containers", http.StatusOK, p)
}

func (h *handler) container(w http.ResponseWriter, r *http.Request) {
	debug := r.URL.Query().Get("debug") == "1"
	d, ok := h.svc.ContainerOpts(r.PathValue("key"), debug)
	if !ok {
		h.fail(w, http.StatusNotFound, "No such container", "whalenurse has not seen a container with that key since it started.")
		return
	}
	p := h.base("container", d.Name)
	p.Detail, p.Debug = &d, debug
	if sp := d.Healer.Suspended; sp != nil {
		restarted := len(d.Ladder) > 0 && d.Ladder[0] == "restart" && d.Healer.RungsRun >= 1
		p.Banner = suspendedHTML(sp.Cause, restarted)
	}
	known := h.known()
	p.Timeline = eventTable{Rows: eventRows(d.Timeline, known)}
	p.Incidents = incidentTable{Rows: incidentRows(d.Incidents, known)}
	if d.Project != "" {
		p.Scope = d.Project + "/" + d.Service
	} else {
		p.Scope = strings.TrimPrefix(d.Key, d.Host+"/")
	}
	h.render(w, "container", http.StatusOK, p)
}

// eventGroups are the filter chips of the history page. "All" hides the noise events (api.NoiseTypes); the last chip,
// "Debug (everything)", shows them.
var eventGroups = []struct {
	label string
	types []string
	debug bool
}{
	{"All", nil, false},
	{"Actions", []string{"ActionResult", "ActionStarted", "ActionInterrupted"}, false},
	{"Giving up", []string{"Suspended", "Unsuspended"}, false},
	{"Recoveries", []string{"Recovered", "IncidentClosed"}, false},
	{"Drift", []string{"DriftBlocked", "DriftWarned"}, false},
	{"Upgrades", []string{"TagMoved", "Upgraded"}, false},
	{"Pauses", []string{"Paused", "Resumed"}, false},
	{"Debug (everything)", nil, true},
}

func (h *handler) history(w http.ResponseWriter, r *http.Request) {
	p := h.base("history", "History")
	q, err := api.ParseEventsQuery(r)
	if err != nil {
		h.fail(w, http.StatusBadRequest, "Bad request", err.Error())
		return
	}
	debug := r.URL.Query().Get("debug") == "1"
	q.Newest, q.Limit, q.Since, q.HideNoise = true, 200, 0, !debug
	p.Key, p.Debug = q.Key, debug
	known := h.known()
	p.Events = eventTable{ShowContainer: true, Rows: eventRows(h.svc.EventsPage(q).Events, known)}
	p.Incidents = incidentTable{ShowContainer: true, Rows: incidentRows(h.svc.Incidents(q.Key, 100).Incidents, known)}
	p.Persistent = p.Status.Persistent
	if s := p.Status.HistoryRetention; s > 0 {
		p.Retention = (time.Duration(s) * time.Second).Round(time.Hour).String()
		if days := s / 86400; days > 0 && s%86400 == 0 {
			p.Retention = fmt.Sprintf("%d days", days)
		}
	}
	active := strings.Join(q.Types, ",")
	for _, g := range eventGroups {
		v := url.Values{}
		if q.Key != "" {
			v.Set("key", q.Key)
		}
		if len(g.types) > 0 {
			v.Set("type", strings.Join(g.types, ","))
		}
		if g.debug {
			v.Set("debug", "1")
		}
		href := "/history"
		if enc := v.Encode(); enc != "" {
			href += "?" + enc
		}
		p.Filters = append(p.Filters, filter{Label: g.label, Href: href, Active: active == strings.Join(g.types, ",") && g.debug == debug})
	}
	h.render(w, "history", http.StatusOK, p)
}
