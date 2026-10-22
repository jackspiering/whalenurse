package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseInspectDecodesRealFixtures(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/inspect/*.json")
	if len(files) == 0 {
		t.Fatal("no fixtures found")
	}
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		c, err := ParseInspect(raw)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if c.ID == "" || c.Name == "" || strings.HasPrefix(c.Name, "/") || c.Status == "" || c.Labels[LabelProject] == "" {
			t.Errorf("%s: incomplete decode: %+v", f, c)
		}
	}
	raw, _ := os.ReadFile("../../testdata/inspect/e8-stuck-failed-restart.json")
	c, _ := ParseInspect(raw)
	if c.Status != "exited" || c.ExitCode != 1 || c.RestartCount != 1 || c.RestartPolicy != "unless-stopped" || !strings.Contains(c.Error, "failed to fulfil mount request") {
		t.Fatalf("%+v", c)
	}
	if !c.StartedAt.After(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || !c.FinishedAt.After(c.StartedAt) {
		t.Fatalf("times: %v %v", c.StartedAt, c.FinishedAt)
	}
	raw, _ = os.ReadFile("../../testdata/inspect/e1-dep-stale-after-parent-recreate.json")
	c, _ = ParseInspect(raw)
	if ContainerRef(c.NetworkMode) != "48d0a0133f45a83e707be12da7d90c65ef6e116524f09f4799d109b3471ec612" {
		t.Fatalf("network mode %q", c.NetworkMode)
	}
	raw, _ = os.ReadFile("../../testdata/inspect/e9-hc-unhealthy.json")
	c, _ = ParseInspect(raw)
	if c.Health != "unhealthy" {
		t.Fatalf("health %q", c.Health)
	}
	if c, _ := ParseInspect([]byte(`[{"Id":"x","Name":"/n","State":{"Status":"created","StartedAt":"0001-01-01T00:00:00Z"}}]`)); !c.StartedAt.IsZero() {
		t.Fatal("the zero time should stay zero")
	}
	if _, err := ParseInspect([]byte(`[]`)); err == nil {
		t.Fatal("empty array should be an error")
	}
}

func TestParseEventSplitsActions(t *testing.T) {
	ev, err := ParseEvent([]byte(`{"Type":"container","Action":"health_status: unhealthy","Actor":{"ID":"abc","Attributes":{"name":"c1","com.docker.compose.project":"p"}},"timeNano":1500000000}`))
	if err != nil || ev.Action != "health_status" || ev.Detail != "unhealthy" || ev.Name != "c1" || ev.ID != "abc" {
		t.Fatalf("%+v %v", ev, err)
	}
	ev, _ = ParseEvent([]byte(`{"Type":"container","Action":"die","Actor":{"ID":"abc","Attributes":{"name":"c1","exitCode":"137"}}}`))
	if !ev.HasExit || ev.ExitCode != 137 {
		t.Fatalf("%+v", ev)
	}
	if _, err := ParseEvent([]byte(`{"Type":"network","Action":"connect","Actor":{"ID":"n"}}`)); err == nil {
		t.Fatal("non-container events are rejected")
	}
}

// A `compose run` container shares its service's key (compose writes no container-number on it, plan §3.2 keeps the key),
// so the key index must never let it, or a swarm task, take the key from a container rule 0 does not skip (WN-004).
func TestPreferForKey(t *testing.T) {
	t0 := time.Unix(1000, 0)
	svc := Summary{ID: "a", Labels: map[string]string{LabelProject: "p", LabelService: "web", LabelNumber: "1", LabelOneOff: "False"}, Created: t0}
	newer := svc
	newer.ID, newer.Created = "b", t0.Add(time.Minute)
	run := Summary{ID: "r", Labels: map[string]string{LabelProject: "p", LabelService: "web", LabelOneOff: "True"}, Created: t0.Add(time.Hour)}
	lower := run
	lower.Labels = map[string]string{LabelProject: "p", LabelService: "web", LabelOneOff: "true"}
	task := Summary{ID: "t", Labels: map[string]string{LabelSwarmTaskID: "x"}, Created: t0.Add(time.Hour)}
	for _, tc := range []struct {
		name     string
		old, new Summary
		want     bool
	}{
		{"compose's rename: the newest wins", svc, newer, true},
		{"an older one does not", newer, svc, false},
		{"a newer one-off does not displace the service", svc, run, false},
		{"lowercase oneoff too", svc, lower, false},
		{"a newer swarm task does not either", svc, task, false},
		{"the service displaces a one-off", run, svc, true},
		{"between one-offs, the newest", run, Summary{ID: "r2", Labels: run.Labels, Created: run.Created.Add(time.Second)}, true},
	} {
		if got := PreferForKey(tc.old, tc.new); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	if !IsOneOff(map[string]string{LabelOneOff: "TRUE"}) || IsOneOff(map[string]string{LabelOneOff: "False"}) || IsOneOff(nil) {
		t.Fatal("IsOneOff")
	}
}

func TestStateKey(t *testing.T) {
	l := map[string]string{LabelProject: "media", LabelService: "web", LabelNumber: "2"}
	if got := StateKey("box", "media-web-2", l); got != "box/media/web/2" {
		t.Fatal(got)
	}
	// The container number defaults to 1, and the key never contains the ID.
	if got := StateKey("box", "x", map[string]string{LabelProject: "p", LabelService: "s"}); got != "box/p/s/1" {
		t.Fatal(got)
	}
	if got := StateKey("box", "/plain", nil); got != "box/plain" {
		t.Fatal(got)
	}
}

func TestMatchesRef(t *testing.T) {
	id := strings.Repeat("ab", 32)
	for ref, want := range map[string]bool{id: true, id[:12]: true, "web": true, "/web": true, "ab": false, "": false, "other": false, "cd" + id[2:]: false} {
		if got := MatchesRef(id, "web", ref); got != want {
			t.Errorf("ref %q: %v", ref, got)
		}
	}
}

func TestSelfID(t *testing.T) {
	id := strings.Repeat("1f", 32)
	mi := "36 25 0:32 / / rw - overlay overlay rw\n" +
		"412 36 8:1 /var/lib/docker/containers/" + id + "/resolv.conf /etc/resolv.conf rw - ext4 /dev/sda1 rw\n"
	if got := SelfID(strings.NewReader(mi)); got != id {
		t.Fatalf("got %q", got)
	}
	if got := SelfID(strings.NewReader("36 25 0:32 / / rw - overlay overlay rw\n")); got != "" {
		t.Fatalf("outside a container: %q", got)
	}
}

func TestContainerRef(t *testing.T) {
	if ContainerRef("container:abc") != "abc" || ContainerRef("host") != "" || ContainerRef("p_default") != "" {
		t.Fatal("ContainerRef")
	}
}

// The boundary of the dashboard's secrecy: an inspect response carries the environment, the command, the mounts and the
// network settings, and none of it enters the snapshot the rest of whalenurse (and the API) can see.
func TestInspectNeverCarriesTheEnvironmentTheCommandOrTheMounts(t *testing.T) {
	raw := []byte(`[{"Id":"abc","Name":"/web","Image":"sha256:img","RestartCount":1,
	  "Config":{"Image":"nginx:1","Env":["DB_PASSWORD=env-SECRET-1","API_TOKEN=env-SECRET-2"],"Cmd":["serve","--key=cmd-SECRET-3"],
	            "Entrypoint":["/bin/run","entry-SECRET-4"],"Labels":{"com.docker.compose.project":"p","com.docker.compose.service":"web"}},
	  "State":{"Status":"running","Running":true,"Health":{"Status":"healthy","Log":[{"Output":"log-SECRET-5"}]}},
	  "HostConfig":{"Binds":["/srv/bind-SECRET-6:/data"],"RestartPolicy":{"Name":"always"},"NetworkMode":"bridge"},
	  "Mounts":[{"Source":"/srv/mount-SECRET-7","Destination":"/m"}],
	  "NetworkSettings":{"Networks":{"net":{"IPAddress":"10.0.0.7","Aliases":["alias-SECRET-8"]}}}}]`)
	c, err := ParseInspect(raw)
	if err != nil || c.ID != "abc" || c.Image != "nginx:1" {
		t.Fatalf("%+v %v", c, err)
	}
	if dump := fmt.Sprintf("%+v %#v", c, c); strings.Contains(dump, "SECRET") {
		t.Fatalf("the snapshot carries something it should not: %s", dump)
	}
}

// Under systemd whalenurse sees the host's mount table, which lists the mounts dockerd makes for other containers: a
// shareable IPC donor's /dev/shm tmpfs is mounted at <data-root>/containers/<id>/mounts/shm (moby ShmResourcePath). That
// is not whalenurse's container, and taking it for its own made whalenurse skip it as "self" for ever.
func TestSelfIDIgnoresOtherContainersMountsOnTheHost(t *testing.T) {
	other := strings.Repeat("ab", 32)
	host := "22 1 254:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw\n" +
		"1234 22 0:123 / /var/lib/docker/containers/" + other +
		"/mounts/shm rw,nosuid,nodev,noexec,relatime shared:456 - tmpfs shm rw,size=65536k\n" +
		"1235 22 254:1 /var/lib/docker/containers/" + other + "/mounts/secrets /var/lib/docker/containers/" + other +
		"/mounts/secrets ro,relatime shared:457 - ext4 /dev/vda1 rw\n"
	if got := SelfID(strings.NewReader(host)); got != "" {
		t.Fatalf("host mountinfo: got %q, want \"\"", got)
	}

	// In a container, another container's mount listed first does not hide the own /etc binds.
	own := strings.Repeat("1f", 32)
	mi := "36 25 0:32 / / rw - overlay overlay rw\n" +
		"40 36 0:123 / /var/lib/docker/containers/" + other + "/mounts/shm rw - tmpfs shm rw\n" +
		"412 36 8:1 /var/lib/docker/containers/" + own + "/hostname /etc/hostname rw - ext4 /dev/sda1 rw\n"
	if got := SelfID(strings.NewReader(mi)); got != own {
		t.Fatalf("container with another container's mount first: got %q, want the own ID", got)
	}

	// A data root on a filesystem of its own: the root field is relative to that filesystem.
	mi = "36 25 0:32 / / rw - overlay overlay rw\n" +
		"413 36 8:17 /containers/" + own + "/hosts /etc/hosts rw - ext4 /dev/sdb1 rw\n"
	if got := SelfID(strings.NewReader(mi)); got != own {
		t.Fatalf("data root on its own filesystem: got %q", got)
	}

	// The own ID in a mount point other than /etc/{hostname,hosts,resolv.conf} does not count.
	mi = "36 25 0:32 / / rw - overlay overlay rw\n" +
		"414 36 8:1 /var/lib/docker/containers/" + other + "/hostname /mnt/x rw - ext4 /dev/sda1 rw\n"
	if got := SelfID(strings.NewReader(mi)); got != "" {
		t.Fatalf("a bind elsewhere: got %q", got)
	}
}
