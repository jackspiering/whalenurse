// Package compose runs the `docker compose` plugin for rung 2 (recreate), the drift guard and the dependents step
// (plan §3.4 to §3.6). It is the only place that builds a compose command line, and it exposes typed methods only: there
// is no way to pass arguments through, so `down` (which removes the project network) cannot be expressed, and every `up`
// carries --no-deps --pull never --no-build (plan §3.1).
package compose

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a release number. The zero Version means "unknown".
type Version struct {
	Major, Minor, Patch int
	Pre                 string // the pre-release, "rc.1" of 28.1.0-rc.1; empty for a release
}

func (v Version) IsZero() bool { return v == Version{} }

func (v Version) String() string {
	if v.IsZero() {
		return "unknown"
	}
	if v.Pre != "" {
		return fmt.Sprintf("%d.%d.%d-%s", v.Major, v.Minor, v.Patch, v.Pre)
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Less reports whether v is an older release than o. A pre-release comes before its release (semver); two
// pre-releases of one release are not ordered, which the minimums (releases) never need.
func (v Version) Less(o Version) bool {
	switch {
	case v.Major != o.Major:
		return v.Major < o.Major
	case v.Minor != o.Minor:
		return v.Minor < o.Minor
	case v.Patch != o.Patch:
		return v.Patch < o.Patch
	}
	return v.Pre != "" && o.Pre == ""
}

// The minimums for rungs 2 and 3 and the drift guard (plan §3.6, *Minimum versions per rung*).
var (
	MinEngine  = Version{Major: 28, Minor: 1}
	MinCompose = Version{Major: 5, Minor: 2}
)

var versionRE = regexp.MustCompile(`v?(\d+)\.(\d+)(?:\.(\d+))?(?:-([0-9A-Za-z.-]+))?`)

// ParseVersion reads the first release number in s, leniently: "5.6.0", "v5.6.0", "Docker Compose version v5.6.0",
// "29.9.1", "28.1.0-rc.1", "28.1" (patch 0) all parse. A pre-release suffix ("-rc.1") is kept, so a pre-release of a
// minimum is below it; build metadata ("+dfsg1") is ignored. It reports false when s holds no number, so a caller can
// fail closed.
func ParseVersion(s string) (Version, bool) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, false
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		v.Patch, _ = strconv.Atoi(m[3])
	}
	v.Pre = m[4]
	return v, true
}

// SameRelease compares the version printed in a container's com.docker.compose.version label with a binary's.
func SameRelease(label string, v Version) bool {
	l, ok := ParseVersion(strings.TrimSpace(label))
	return ok && l == v
}

// Gate is the verdict of the startup version check (plan §3.6): whether rungs 2 and 3 and the drift guard may run.
type Gate struct {
	Recreate bool   // rungs 2 and 3, the drift guard, and every other use of compose
	Reason   string // why not, in words fit for one warning; empty when Recreate is true
}

// CheckVersions applies the minimums. engine is the Engine's version string; bin is the compose binary that was found,
// or nil when none was. An unparseable version, or no binary at all, disables the rungs: the gate fails closed.
func CheckVersions(engine string, bin *Binary) Gate {
	var why []string
	if ev, ok := ParseVersion(engine); !ok {
		why = append(why, fmt.Sprintf("the Docker Engine version %q cannot be read", engine))
	} else if ev.Less(MinEngine) {
		why = append(why, fmt.Sprintf("Docker Engine %s is older than %s (image inspect --platform, needed by the drift guard)", ev, MinEngine))
	}
	switch {
	case bin == nil:
		why = append(why, "no docker compose binary was found")
	case bin.Version.IsZero():
		why = append(why, fmt.Sprintf("the version of %s cannot be read (%q)", bin.Path, bin.Raw))
	case bin.Version.Less(MinCompose):
		why = append(why, fmt.Sprintf("Docker Compose %s (%s) is older than %s", bin.Version, bin.Path, MinCompose))
	}
	if len(why) == 0 {
		return Gate{Recreate: true}
	}
	return Gate{Reason: strings.Join(why, "; ")}
}
