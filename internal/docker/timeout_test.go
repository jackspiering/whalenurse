package docker

import (
	"context"
	"testing"
	"time"
)

type deadlineReader struct {
	Reader
	got []time.Duration // how far away each call's deadline was; 0 for none
}

func (r *deadlineReader) note(ctx context.Context) {
	var left time.Duration
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	r.got = append(r.got, left)
}

func (r *deadlineReader) List(ctx context.Context) ([]Summary, error) { r.note(ctx); return nil, nil }
func (r *deadlineReader) Inspect(ctx context.Context, _ string) (Container, error) {
	r.note(ctx)
	return Container{}, nil
}
func (r *deadlineReader) ImageID(ctx context.Context, _, _ string) (string, error) {
	r.note(ctx)
	return "", nil
}
func (r *deadlineReader) Events(ctx context.Context, _ time.Time) Stream {
	r.note(ctx)
	return Stream{}
}

func TestWithReadTimeoutBoundsTheReadsOnly(t *testing.T) {
	inner := &deadlineReader{}
	r := WithReadTimeout(inner, time.Minute)
	ctx := context.Background()
	_, _ = r.List(ctx)
	_, _ = r.Inspect(ctx, "x")
	_, _ = r.ImageID(ctx, "x", "amd64")
	r.Events(ctx, time.Time{})
	for i, left := range inner.got[:3] {
		if left <= 50*time.Second || left > time.Minute {
			t.Errorf("read %d: deadline %v away, want about a minute", i, left)
		}
	}
	if inner.got[3] != 0 {
		t.Errorf("the events subscription got a deadline (%v away)", inner.got[3])
	}

	// An earlier deadline is kept; a later one is shortened.
	inner.got = nil
	soon, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, _ = r.Inspect(soon, "x")
	later, cancel2 := context.WithTimeout(ctx, time.Hour)
	defer cancel2()
	_, _ = r.Inspect(later, "x")
	if inner.got[0] > time.Second || inner.got[1] > time.Minute {
		t.Fatalf("deadlines %v", inner.got)
	}
}
