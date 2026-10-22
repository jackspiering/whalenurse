package docker

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

// selfRootRe matches the root field of Docker's /etc bind for a container: <data-root>/containers/<id>/<file>, or
// /containers/<id>/<file> when the data root is a filesystem of its own (the root is relative to that filesystem).
var selfRootRe = regexp.MustCompile(`(?:^|/)containers/([0-9a-f]{64})/(?:hostname|hosts|resolv\.conf)$`)

// SelfID finds whalenurse's own container ID in /proc/self/mountinfo. The
// hostname can be overridden, but Docker always bind-mounts /etc/hostname,
// /etc/hosts and /etc/resolv.conf from <data-root>/containers/<id>/. Only those
// three mount points count: on the host (under systemd) the mount table also
// lists the mounts dockerd makes for other containers (a shareable container's
// <data-root>/containers/<id>/mounts/shm), which are not whalenurse's. It
// returns "" outside a container.
func SelfID(mountinfo io.Reader) string {
	sc := bufio.NewScanner(mountinfo)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		// mountinfo(5): ID parent major:minor root mount-point options [optional...] - fstype source super-options;
		// a space in a path is written \040, so the fields split on spaces.
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		switch f[4] {
		case "/etc/hostname", "/etc/hosts", "/etc/resolv.conf":
		default:
			continue
		}
		if m := selfRootRe.FindStringSubmatch(f[3]); m != nil {
			return m[1]
		}
	}
	return ""
}
