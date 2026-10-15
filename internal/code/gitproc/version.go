package gitproc

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/crewlet/crewlet/internal/textcut"
)

// MinVersion is the oldest git the engine runs: proc-receive (2.29),
// GIT_CONFIG_COUNT (2.31), index-pack --rev-index (2.31), object-type filters
// (2.32), multi-pack-index bitmaps (2.34), `git hook run` (2.36), and
// merge-tree --write-tree --merge-base (2.40) — the newest feature the code
// host is designed around.
//
// ONE MINIMUM FOR THE WHOLE DESIGN, chosen once, so that no later capability
// raises it under a node an operator has already deployed. Reftable (2.45),
// the one later feature the design once used, was dropped for packed-refs
// swapped by an atomic rename rather than raise this.
var MinVersion = Version{2, 40, 0}

// MultiPackReuse is the first git whose pack.allowPackReuse=multi sends a
// pack its client can read, so the value a cache serves with is chosen per
// binary ([Pool.PackReuse]). 2.43 refuses the value outright ("bad boolean
// config value 'multi'"); 2.44 to 2.47 accept it, and reusing from several
// packs that hold one object — which a cache's are, since receive-pack
// completes each thin push with the bases it borrowed from the packs before
// it — send that object twice or a delta without its base, and the client's
// index-pack refuses the whole pack: every full clone of a repository the
// maintainer had written a bitmap for failed on 2.47.3, the git the engine
// image installs ("REF_DELTA … already resolved", "pack has N unresolved
// deltas"). 2.48.0 is the release that fixed it (tb/multi-pack-reuse-dupfix,
// "Object reuse code based on multi-pack-index sent an unwanted copy of
// object"), measured against the same repositories. An older git reuses from
// the bitmap's preferred pack alone, which costs a clone CPU and never its
// pack.
var MultiPackReuse = Version{2, 48, 0}

// Version is a git release, compared on (Major, Minor, Patch).
type Version struct{ Major, Minor, Patch int }

// AtLeast reports whether v is o or newer.
func (v Version) AtLeast(o Version) bool {
	if v.Major != o.Major {
		return v.Major > o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor > o.Minor
	}
	return v.Patch >= o.Patch
}

// String is the release as git prints it: "2.43.0".
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// versionLine is the line `git version` prints. Only the first three numbers
// are read: what follows them is a vendor's (" (Apple Git-145)",
// ".windows.1") or a build's (".rc1", ".GIT"), and says nothing about which
// features the binary has.
var versionLine = regexp.MustCompile(`(?m)^git version ([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)

// ParseVersion reads `git version` output: the FIRST line beginning
// "git version ", then major.minor[.patch]; anything after is ignored. A
// missing patch is 0.
func ParseVersion(out string) (Version, error) {
	m := versionLine.FindStringSubmatch(out)
	if m == nil {
		return Version{}, fmt.Errorf("gitproc: %q is not what `git version` prints", textcut.Ellipsis(out, 120))
	}
	var v Version
	for i, field := range []*int{&v.Major, &v.Minor, &v.Patch} {
		if m[i+1] == "" {
			continue // the patch, absent: "git version 2.45.GIT"
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Version{}, fmt.Errorf("gitproc: %q is not what `git version` prints: %w", textcut.Ellipsis(out, 120), err)
		}
		*field = n
	}
	return v, nil
}

// ErrTooOld is the refusal of a git below [MinVersion]. Its message names the
// binary, what it is and what to install, because the person reading it is
// the operator of a node that cannot serve git until they do.
type ErrTooOld struct {
	Path  string
	Found Version
	Want  Version
}

// Error names the binary, the version found, the minimum and where to get one.
func (e *ErrTooOld) Error() string {
	return fmt.Sprintf("gitproc: %s is git %s, and the code host needs git %s or newer: "+
		"install a newer git on this node — the engine image ships git 2.47 (Debian trixie); "+
		"on macOS, Apple's bundled git is 2.39, so install Homebrew's", e.Path, e.Found, e.Want)
}
