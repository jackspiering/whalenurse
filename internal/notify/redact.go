package notify

import (
	"net/url"
	"sort"
	"strings"

	"github.com/jackspiering/whalenurse/internal/engine"
)

// mask replaces every secret in a redacted string.
const mask = "***"

// minSecret is the shortest fragment the redactor replaces. Shorter ones ("a", "yes") would mangle ordinary text and are no
// token; a token that short would not protect anything either.
const minSecret = 4

// minPathSecret is the shortest path segment the redactor replaces. A path segment of Shoutrrr's own error text is
// ordinary words ("hook" in "generic webhook"), and real tokens in a path (Discord, Slack, Gotify, a private ntfy topic)
// are longer.
const minPathSecret = 6

// Redactor removes the pieces of the configured URLs from text. Notification URLs hold tokens (a Discord webhook id and
// token are the host and path, a Telegram bot token is the user name, ntfy and Gotify tokens are a path or a query
// value), and the errors of an HTTP client quote the URL it called. Shoutrrr redacts its own errors too (v0.22.0), but
// that is one layer, and whalenurse logs, counts and prints these errors in several places, so every one of them goes
// through here.
type Redactor struct {
	fragments []string // longest first, so a URL goes before the pieces inside it
}

// NewRedactor builds a Redactor for the given raw URLs (notification URLs, the uptime ping URL).
func NewRedactor(raw ...string) *Redactor { return NewRedactorWith(nil, raw...) }

// NewRedactorWith is NewRedactor plus literal secrets that are not URLs (the registry credentials of M7): each one is
// removed whole, and in its URL-escaped forms.
func NewRedactorWith(secrets []string, raw ...string) *Redactor {
	seen := map[string]bool{}
	add := func(s string) {
		if len(s) < minSecret || seen[s] {
			return
		}
		seen[s] = true
	}
	for _, r := range raw {
		for _, f := range fragmentsOf(r) {
			add(f)
		}
	}
	for _, s := range secrets {
		add(s)
		add(url.QueryEscape(s))
		add(url.PathEscape(s))
	}
	r := &Redactor{}
	for f := range seen {
		r.fragments = append(r.fragments, f)
	}
	sort.Slice(r.fragments, func(i, j int) bool {
		if len(r.fragments[i]) != len(r.fragments[j]) {
			return len(r.fragments[i]) > len(r.fragments[j])
		}
		return r.fragments[i] < r.fragments[j]
	})
	return r
}

// fragmentsOf lists everything in a URL that must not reach a log: the URL itself, its parts, and their escaped forms.
// The scheme is kept, it is what names the service.
func fragmentsOf(raw string) []string {
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		out = append(out, s, url.QueryEscape(s), url.PathEscape(s))
	}
	add(raw)
	// Everything after the scheme: Shoutrrr rewrites generic://host/path to http(s)://host/path in the errors of its
	// HTTP client, so the remainder is what shows up, behind another scheme.
	if _, rest, ok := strings.Cut(raw, "://"); ok {
		add(rest)
		for _, scheme := range []string{"http://", "https://"} {
			add(scheme + rest)
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		// A URL that does not even parse is still redacted whole, and by its pieces.
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return strings.ContainsRune("/?&=@:#", r) }) {
			add(part)
		}
		return out
	}
	if u.User != nil {
		add(u.User.Username())
		if pw, ok := u.User.Password(); ok {
			add(pw)
		}
	}
	add(u.Host)
	add(u.Hostname())
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		if len(seg) < minPathSecret {
			continue
		}
		if s, err := url.PathUnescape(seg); err == nil {
			add(s)
		}
		add(seg)
	}
	if p := strings.Trim(u.Path, "/"); len(p) >= minPathSecret {
		add(p)
	}
	for _, vals := range u.Query() {
		for _, v := range vals {
			add(v)
			// A value with commas (tags=a,b, chats=1,2) is leaked piece by piece as well.
			for _, piece := range strings.Split(v, ",") {
				add(piece)
			}
		}
	}
	add(u.RawQuery)
	return out
}

// Redact returns s with every fragment of the configured URLs replaced by ***.
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	for _, f := range r.fragments {
		if strings.Contains(s, f) {
			s = strings.ReplaceAll(s, f, mask)
		}
	}
	return s
}

// controlAttrs are the event attributes that steer the templates and the dedupe (an action, a rung, ok, a reason, a time).
// whalenurse writes them itself and no Docker or compose text reaches them, so Event leaves them alone: a short query value
// of a URL ("true", "json") must not turn ok=true into ok=***.
var controlAttrs = map[string]bool{
	"action": true, "at": true, "class": true, "count": true, "dry_run": true, "held": true, engine.AttrIncident: true,
	"moved_at": true, "not_applicable": true, "ok": true, "opened": true, "outcome": true, "reason": true, "rung": true,
	"rungs": true, "scope": true, "until": true,
}

// Event returns a copy of e with its free text redacted: the message and every attribute but the control ones. The
// identity (host, key, container) is kept, so a message still names what it is about. The log and the notifications
// use it; the API applies its own rules (api.Service).
func (r *Redactor) Event(e engine.Event) engine.Event {
	if r == nil {
		return e
	}
	e.Message = r.Redact(e.Message)
	if len(e.Attrs) > 0 {
		attrs := make(map[string]string, len(e.Attrs))
		for k, v := range e.Attrs {
			if !controlAttrs[k] {
				v = r.Redact(v)
			}
			attrs[k] = v
		}
		e.Attrs = attrs
	}
	return e
}

// Err returns the redacted text of err, or "" for nil.
func (r *Redactor) Err(err error) string {
	if err == nil {
		return ""
	}
	return r.Redact(err.Error())
}

// schemeOf names a URL's service for logs and status, and nothing else about it: "ntfy", "generic", "generic+https". A
// string without a scheme is "invalid".
func schemeOf(raw string) string {
	scheme, _, ok := strings.Cut(raw, "://")
	if !ok || scheme == "" || strings.ContainsAny(scheme, "/?@ ") {
		return "invalid"
	}
	return strings.ToLower(scheme)
}
