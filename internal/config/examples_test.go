package config

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// unitValues reads a systemd unit file into section -> key -> the space-separated values of every line of that key.
func unitValues(t *testing.T, path string) map[string]map[string][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]map[string][]string{}
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line[0] == '[':
			section = strings.Trim(line, "[]")
			out[section] = map[string][]string{}
		default:
			k, v, ok := strings.Cut(line, "=")
			if ok && section != "" {
				out[section][strings.TrimSpace(k)] = append(out[section][strings.TrimSpace(k)], strings.Fields(v)...)
			}
		}
	}
	return out
}

func has(vs []string, want string) bool {
	for _, v := range vs {
		if v == want {
			return true
		}
	}
	return false
}

// Requires=docker.service stops whalenurse when Docker is stopped, but nothing starts it again when Docker is started:
// `enable` links it into multi-user.target only. WantedBy=docker.service makes a start of Docker (by hand, or by socket
// activation) start whalenurse too. systemd-analyze verify does not read [Install], so it cannot catch this.
func TestSystemdUnitStartsAgainWithDocker(t *testing.T) {
	u := unitValues(t, "../../examples/systemd/whalenurse.service")
	if !has(u["Unit"]["Requires"], "docker.service") || !has(u["Unit"]["After"], "docker.service") {
		t.Fatalf("[Unit] must keep Requires= and After=docker.service (plan §3.3(d): whalenurse stops before Docker): %v", u["Unit"])
	}
	if !has(u["Install"]["WantedBy"], "docker.service") {
		t.Fatalf("[Install] WantedBy=%v: without docker.service, a stop and start of Docker leaves whalenurse stopped", u["Install"]["WantedBy"])
	}
	if !has(u["Install"]["WantedBy"], "multi-user.target") {
		t.Fatalf("[Install] WantedBy=%v: whalenurse must still start at boot", u["Install"]["WantedBy"])
	}
	if !has(u["Service"]["KillMode"], "mixed") {
		t.Fatalf("[Service] KillMode=%v: a stop must SIGTERM whalenurse only, so a compose command in flight can finish", u["Service"]["KillMode"])
	}
}
