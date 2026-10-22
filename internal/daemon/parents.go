package daemon

import (
	"slices"
	"strings"
	"time"

	"github.com/jackspiering/whalenurse/internal/compose"
	"github.com/jackspiering/whalenurse/internal/docker"
)

// metaParents is the prefix of the store keys that remember which services a dependent hangs on.
const metaParents = "parents:"

// MetaKeyed and MetaWithKey are the meta rows that hold a state key, which the store re-keys to the current host ID at
// startup (store.Rekey, plan v3.13 item 12): a prefix followed by the key, and a prefix whose JSON value has a "key".
// A new meta record keyed by a state key must be added here.
var (
	MetaKeyed   = []string{metaParents}
	MetaWithKey = []string{metaTagMoved}
)

// parentsGoneAfter is how long a key may be absent from the container list before what is remembered about it is dropped
// (a compose recreate by hand removes the container for a moment, and the parent mapping must survive that).
const parentsGoneAfter = time.Hour

// A dependent (network_mode: service:X, or an ipc or pid reference) is tied to its parent by the compose depends_on label
// once the parent's ID is gone (plan §3.5, v3.6 decision 9). Compose writes that label from the services it has in scope,
// so a dependent recreated with `up --no-deps` (which is how whalenurse does it) comes back with an empty label (compose
// pkg/compose/create.go builds com.docker.compose.depends_on from service.DependsOn; v5.6.0 up.go narrows the project with
// types.IgnoreDependencies for --no-deps). From then on the label cannot find the parent, so the dependent would not be
// repaired when the parent is recreated the next time. whalenurse therefore remembers, per key, the services a dependent
// depends on, learned while the label or a live container: reference still shows them (plan v3.11).

// parentServices returns the services that the dependent holding these labels depends on: the label while it has any,
// otherwise what was learned for the key.
func (d *Daemon) parentServices(key string, labels map[string]string) []string {
	if l := dependsOn(labels[compose.LabelDependsOn]); len(l) > 0 {
		return l
	}
	return d.knownParents(key)
}

func (d *Daemon) knownParents(key string) []string {
	if l, ok := d.parents[key]; ok {
		return l
	}
	if d.o.Meta == nil {
		return nil
	}
	v, ok := d.o.Meta.Meta(metaParents + key)
	if !ok || v == "" {
		return nil
	}
	l := strings.Split(v, ",")
	d.parents[key] = l
	return l
}

// learnParents records the services a dependent depends on. A non-empty label is authoritative and replaces what was
// stored; a live container: reference that resolves to a container of the same project adds that container's service, and
// never shrinks a set learned from the label (depends_on can name services besides the one it shares a namespace with).
func (d *Daemon) learnParents(key string, c docker.Container) {
	delete(d.parentsGone, key)
	known := d.knownParents(key)
	var next []string
	if l := dependsOn(c.Labels[compose.LabelDependsOn]); len(l) > 0 {
		next = slices.Clone(l)
	} else {
		next = slices.Clone(known)
		project := c.Labels[docker.LabelProject]
		for _, mode := range []string{c.NetworkMode, c.IpcMode, c.PidMode} {
			ref := docker.ContainerRef(mode)
			if ref == "" {
				continue
			}
			for _, s := range d.idx.all {
				svc := s.Labels[docker.LabelService]
				if svc == "" || s.Labels[docker.LabelProject] != project || docker.IsOneOff(s.Labels) || !docker.MatchesRef(s.ID, s.Name, ref) {
					continue
				}
				if !slices.Contains(next, svc) {
					next = append(next, svc)
				}
			}
		}
	}
	if len(next) == 0 || slices.Equal(next, known) {
		return
	}
	d.parents[key] = next
	if d.o.Meta != nil {
		d.o.Meta.SetMeta(metaParents+key, strings.Join(next, ","))
	}
}

// forgetParents drops what is remembered about keys that have been gone from the container list for a while.
func (d *Daemon) forgetParents(now time.Time) {
	for key, since := range d.parentsGone {
		if now.Sub(since) < parentsGoneAfter {
			continue
		}
		delete(d.parents, key)
		delete(d.parentsGone, key)
		if d.o.Meta != nil {
			d.o.Meta.SetMeta(metaParents+key, "")
		}
	}
}
