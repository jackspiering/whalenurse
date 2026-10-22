package daemon

import (
	"context"
	"log/slog"

	"github.com/jackspiering/whalenurse/internal/engine"
	"github.com/jackspiering/whalenurse/internal/notify"
)

// LogSink returns the domain-event subscriber that writes the structured log.
// It reads the same stream the store and the notifier (internal/notify) do, so the log cannot disagree with them.
// The message and the free-text attributes go through red first (an error can quote a notification URL, the token or a
// registry credential); the identity of the event and its control attributes are written as they are.
func LogSink(log *slog.Logger, red *notify.Redactor) func(engine.Event) {
	return func(e engine.Event) {
		e = red.Event(e)
		level := slog.LevelInfo
		switch e.Type {
		case engine.Suspended, engine.NeedsRecreate, engine.ActionRefused, engine.DriftBlocked, engine.DriftWarned,
			engine.RungsDisabled, engine.DependentPostCheck, engine.RateLimited, engine.NotifyOnly, engine.IncidentOpened:
			level = slog.LevelWarn
		case engine.EventAttributed, engine.RestoreEvent:
			level = slog.LevelDebug
		}
		attrs := []any{"event_id", e.ID, "type", string(e.Type), "host", e.Host, "key", e.Key, "container", e.Container}
		for k, v := range e.Attrs {
			attrs = append(attrs, "attr."+k, v)
		}
		log.Log(context.Background(), level, e.Message, attrs...)
	}
}
