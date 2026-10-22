package docker

import (
	"context"
	"time"
)

// WithReadTimeout bounds List, Inspect and ImageID: each runs under a timeout of d unless ctx already ends sooner. A
// read that never returns then fails like any other API error instead of holding its caller (plan v3.13 item 13).
// Events, Ping and ServerVersion are passed through: a subscription is long-lived, and the others bound themselves.
func WithReadTimeout(r Reader, d time.Duration) Reader { return timeoutReader{Reader: r, d: d} }

type timeoutReader struct {
	Reader
	d time.Duration
}

func (t timeoutReader) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= t.d {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, t.d)
}

func (t timeoutReader) List(ctx context.Context) ([]Summary, error) {
	ctx, cancel := t.bound(ctx)
	defer cancel()
	return t.Reader.List(ctx)
}

func (t timeoutReader) Inspect(ctx context.Context, ref string) (Container, error) {
	ctx, cancel := t.bound(ctx)
	defer cancel()
	return t.Reader.Inspect(ctx, ref)
}

func (t timeoutReader) ImageID(ctx context.Context, ref, arch string) (string, error) {
	ctx, cancel := t.bound(ctx)
	defer cancel()
	return t.Reader.ImageID(ctx, ref, arch)
}
