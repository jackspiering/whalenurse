package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Moby implements Client on top of the moby client. API version negotiation is
// the client default.
type Moby struct {
	c *client.Client
}

// NewMoby connects using the standard DOCKER_HOST environment.
func NewMoby() (*Moby, error) {
	c, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &Moby{c: c}, nil
}

func (m *Moby) Close() error { return m.c.Close() }

func (m *Moby) Ping(ctx context.Context) error {
	_, err := m.c.Ping(ctx, client.PingOptions{})
	return err
}

func (m *Moby) List(ctx context.Context) ([]Summary, error) {
	res, err := m.c.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(res.Items))
	for _, it := range res.Items {
		s := Summary{
			ID:          it.ID,
			Labels:      it.Labels,
			State:       string(it.State),
			NetworkMode: it.HostConfig.NetworkMode,
			Image:       it.Image,
			Created:     time.Unix(it.Created, 0),
		}
		if len(it.Names) > 0 {
			s.Name = strings.TrimPrefix(it.Names[0], "/")
		}
		if it.Health != nil && it.Health.Status != container.NoHealthcheck {
			s.Health = string(it.Health.Status)
		}
		out = append(out, s)
	}
	return out, nil
}

func (m *Moby) Inspect(ctx context.Context, ref string) (Container, error) {
	res, err := m.c.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Container{}, fmt.Errorf("%w: %s", ErrNotFound, ref)
		}
		return Container{}, err
	}
	return fromInspect(res.Container), nil
}

func (m *Moby) ServerVersion(ctx context.Context) (ServerInfo, error) {
	v, err := m.c.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return ServerInfo{}, err
	}
	return ServerInfo{Version: v.Version, APIVersion: v.APIVersion, Arch: v.Arch, OS: v.Os}, nil
}

// ImageID is `docker image inspect --platform linux/<arch> --format '{{.Id}}' <ref>` (needs API 1.49, Engine 28.1). With
// an empty arch it is the plain `docker image inspect --format '{{.Id}}' <ref>`: the index digest on the containerd store.
func (m *Moby) ImageID(ctx context.Context, ref, arch string) (string, error) {
	var opts []client.ImageInspectOption
	if arch != "" {
		opts = append(opts, client.ImageInspectWithPlatform(&ocispec.Platform{OS: "linux", Architecture: arch}))
	}
	res, err := m.c.ImageInspect(ctx, ref, opts...)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return "", fmt.Errorf("%w: %s (linux/%s)", ErrImageNotFound, ref, arch)
		}
		return "", err
	}
	return res.ID, nil
}

func (m *Moby) Restart(ctx context.Context, id string) error {
	_, err := m.c.ContainerRestart(ctx, id, client.ContainerRestartOptions{})
	return err
}

func (m *Moby) Start(ctx context.Context, id string) error {
	_, err := m.c.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (m *Moby) Events(ctx context.Context, since time.Time) Stream {
	opts := client.EventsListOptions{Filters: client.Filters{}.Add("type", string(events.ContainerEventType))}
	if !since.IsZero() {
		opts.Since = fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond())
	}
	res := m.c.Events(ctx, opts)
	out := make(chan Event)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		errs <- forward(ctx, res, out) // before out closes, as Stream promises
	}()
	return Stream{Events: out, Err: errs}
}

// forward copies the client's messages until the stream ends and returns why.
// The moby client (v0.6.2) reports the end on Err only and never closes
// Messages, so both are watched; a Messages that does close is read as an end
// too.
func forward(ctx context.Context, res client.EventsResult, out chan<- Event) error {
	ended := func(err error, ok bool) error {
		if !ok || err == nil {
			return io.EOF
		}
		return err
	}
	for {
		select {
		case msg, ok := <-res.Messages:
			if !ok {
				select {
				case err, ok := <-res.Err:
					return ended(err, ok)
				default:
					return io.EOF
				}
			}
			select {
			case out <- fromMessage(msg):
			case <-ctx.Done():
				return ctx.Err()
			}
		case err, ok := <-res.Err:
			return ended(err, ok)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func fromMessage(msg events.Message) Event {
	ev := Event{
		ID:     msg.Actor.ID,
		Labels: msg.Actor.Attributes,
		Name:   msg.Actor.Attributes["name"],
		Signal: msg.Actor.Attributes["signal"],
	}
	ev.Action, ev.Detail, _ = strings.Cut(string(msg.Action), ": ")
	if v, ok := msg.Actor.Attributes["exitCode"]; ok {
		if _, err := fmt.Sscanf(v, "%d", &ev.ExitCode); err == nil {
			ev.HasExit = true
		}
	}
	switch {
	case msg.TimeNano != 0:
		ev.Time = time.Unix(0, msg.TimeNano)
	case msg.Time != 0:
		ev.Time = time.Unix(msg.Time, 0)
	}
	return ev
}

func fromInspect(r container.InspectResponse) Container {
	c := Container{ImageID: r.Image, ID: r.ID, Name: strings.TrimPrefix(r.Name, "/"), RestartCount: r.RestartCount, Created: parseTime(r.Created)}
	if r.Config != nil {
		c.Labels = r.Config.Labels
		c.Image = r.Config.Image
	}
	if s := r.State; s != nil {
		c.Status = string(s.Status)
		c.Running, c.Paused, c.Restarting, c.OOMKilled = s.Running, s.Paused, s.Restarting, s.OOMKilled
		c.ExitCode, c.Error = s.ExitCode, s.Error
		c.StartedAt, c.FinishedAt = parseTime(s.StartedAt), parseTime(s.FinishedAt)
		if s.Health != nil {
			c.Health = string(s.Health.Status)
		}
	}
	if h := r.HostConfig; h != nil {
		c.RestartPolicy = string(h.RestartPolicy.Name)
		c.MaxRetries = h.RestartPolicy.MaximumRetryCount
		c.NetworkMode, c.IpcMode, c.PidMode = string(h.NetworkMode), string(h.IpcMode), string(h.PidMode)
		c.VolumesFrom = append([]string(nil), h.VolumesFrom...)
	}
	return c
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}
	return t
}

// ParseInspect decodes `docker inspect` output (an array, or a single object)
// through the real moby types. Used for fixtures.
func ParseInspect(raw []byte) (Container, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '[' {
		var arr []container.InspectResponse
		if err := json.Unmarshal(raw, &arr); err != nil {
			return Container{}, err
		}
		if len(arr) != 1 {
			return Container{}, fmt.Errorf("expected one container, got %d", len(arr))
		}
		return fromInspect(arr[0]), nil
	}
	var one container.InspectResponse
	if err := json.Unmarshal(raw, &one); err != nil {
		return Container{}, err
	}
	return fromInspect(one), nil
}

// ParseEvent decodes one line of `docker events --format '{{json .}}'`.
func ParseEvent(line []byte) (Event, error) {
	var msg events.Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return Event{}, err
	}
	if msg.Type != events.ContainerEventType {
		return Event{}, errors.New("not a container event")
	}
	return fromMessage(msg), nil
}
