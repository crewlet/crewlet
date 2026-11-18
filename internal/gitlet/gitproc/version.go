package gitproc

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/textcut"
)

// ClientMinVersion is the oldest git a CLIENT of Gitlet may run: a person's,
// a CI runner's, the operator CLI's (`crewlet gitlet import`, `export`, and
// every git its credential helper answers) and gittest's client. Its newest
// need is merge-tree --write-tree --merge-base (2.40). The operator does not
// choose a client's git, so this stays at what the distributions people run
// still ship (package doc, "Two floors, one pin (ADR-0087)").
var ClientMinVersion = Version{2, 40, 0}

// NodeMinVersion is the oldest git a GITLET NODE runs (every pool [New]
// builds). Its newest need is `multi-pack-index write --incremental
// --base=<checksum> --no-write-chain-file` (2.55): a bitmapped layer over a
// NAMED base, how a maintainer publishes the layers every node installs. From
// 2.48 either pack.allowPackReuse value sends overlapping packs whole (2.44 to
// 2.47's `multi` did not), so nothing picks it per binary (cache.ServeConfig).
//
// THE FLOOR FOLLOWS THE COMMANDS, never the reverse (both held on the floor's
// own build by TestTheNodeFloorWritesALayerOverANamedBase and
// TestEveryPackReuseModeSendsOverlappingPacksWhole). It is chosen once, before
// a tag ships, so no later need raises it under a deployed node; the engine
// image's newer git (scripts/git-pin.env) is not it.
var NodeMinVersion = Version{2, 55, 0}

// Floor is which of the two floors a git is held to: a closed set, so a
// value nothing set is refused rather than read as either.
type Floor string

const (
	// FloorNode is NodeMinVersion: every pool New builds.
	FloorNode Floor = "node"
	// FloorClient is ClientMinVersion: gittest's client and the operator
	// CLI's git.
	FloorClient Floor = "client"
)

// Valid reports whether f is one of the two floors.
func (f Floor) Valid() bool { return f == FloorNode || f == FloorClient }

// Min is the floor's version: the zero Version for a floor that is not one.
func (f Floor) Min() Version {
	switch f {
	case FloorNode:
		return NodeMinVersion
	case FloorClient:
		return ClientMinVersion
	}
	return Version{}
}

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
// are read: a vendor's or a build's suffix (" (Apple Git-145)", ".rc1",
// ".GIT") says nothing about the binary's features.
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

// ParseRelease is the exact inverse of [Version.String] ("2.56.0"): three
// decimal numbers joined by dots, no sign, no leading zero, nine digits at
// most. A node reads a published layer's release with it
// (gitlet.LayerRef.Git, held to the same spelling without importing this
// package), so it knows a layer
// written by a newer git before fetching it. Any other spelling ("2.56",
// "v2.56.0", a vendor's suffix, a version line) is refused: String never
// wrote it.
func ParseRelease(s string) (Version, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("gitproc: %q is not a release as Version.String spells one (2.56.0)",
			textcut.Ellipsis(s, 120))
	}
	var v Version
	for i, field := range []*int{&v.Major, &v.Minor, &v.Patch} {
		n := parts[i]
		// NINE DIGITS AT MOST: fits an int on every platform the engine
		// builds for; gitlet.LayerRef.Check holds the same bound.
		if n == "" || len(n) > 9 || (len(n) > 1 && n[0] == '0') {
			return Version{}, fmt.Errorf("gitproc: %q is not a release as Version.String spells one (2.56.0)",
				textcut.Ellipsis(s, 120))
		}
		u, err := strconv.ParseUint(n, 10, 32)
		if err != nil {
			return Version{}, fmt.Errorf("gitproc: %q is not a release as Version.String spells one (2.56.0): %w",
				textcut.Ellipsis(s, 120), err)
		}
		*field = int(u)
	}
	return v, nil
}

// ErrTooOld is the refusal of a git below its floor. Its message never names
// the engine image's git version, which would be a second copy of
// scripts/git-pin.env. The version wanted is derived ([ErrTooOld.Want]), not
// a field that could disagree with Floor.
type ErrTooOld struct {
	Path  string
	Found Version
	Floor Floor
}

// Want is the version the floor needs: the zero Version for an ErrTooOld held
// to no floor.
func (e *ErrTooOld) Want() Version { return e.Floor.Min() }

// Error names the binary, the version found, the floor and what to install.
// An ErrTooOld held to no floor is a call site's bug and says so rather than
// guess the floor.
func (e *ErrTooOld) Error() string {
	switch e.Floor {
	case FloorNode:
		return fmt.Sprintf("gitproc: %s is git %s, and a Gitlet node needs git %s or newer "+
			"(gitproc.NodeMinVersion, the node floor): install a newer git on this node's PATH — "+
			"the engine image builds the newest git release — or on macOS Homebrew's",
			e.Path, e.Found, NodeMinVersion)
	case FloorClient:
		return fmt.Sprintf("gitproc: %s is git %s, and a client of Gitlet needs git %s or newer "+
			"(gitproc.ClientMinVersion, the client floor): install a newer git — on macOS, Homebrew's: "+
			"Apple's bundled git is 2.39", e.Path, e.Found, ClientMinVersion)
	}
	return fmt.Sprintf("gitproc: %s is git %s, held to no floor (ErrTooOld.Floor %q)", e.Path, e.Found, string(e.Floor))
}
