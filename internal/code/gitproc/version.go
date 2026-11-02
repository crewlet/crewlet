package gitproc

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/textcut"
)

// ClientMinVersion is the oldest git a CLIENT of the code host may run: a
// person's and a CI runner's, the operator CLI's (`crewlet code import`,
// `export`, and every git its credential helper answers), and gittest's
// client in every suite. merge-tree --write-tree --merge-base (2.40) is the
// newest feature a client-side recipe uses; GIT_CONFIG_COUNT (2.31), protocol
// v2 by default (2.26) and partial clone (2.19) are older. A client's git is
// a person's laptop or a CI image the operator does not choose, so it is held
// to what the distributions people run still ship (ADR-0080).
var ClientMinVersion = Version{2, 40, 0}

// NodeMinVersion is the oldest git a GIT NODE runs — every pool [New] builds:
// an incremental multi-pack-index layer with its bitmap (2.50) written over a
// NAMED base with no chain file (`multi-pack-index write --incremental
// --base=<checksum> --no-write-chain-file`, 2.55: `write` offers neither
// option in v2.54.0's documentation and both in v2.55.0's), which is how a
// repository's maintainer publishes the layers every git node installs.
// Every git at the floor also sends a cache's overlapping packs whole under
// either pack.allowPackReuse value
// (TestEveryPackReuseModeSendsOverlappingPacksWhole, on the node pin and on
// the floor's own build), so nothing chooses the value per binary: a cache
// serves `single` (cache.ServeConfig says why). `multi` was fixed in 2.48.0
// (tb/multi-pack-reuse-dupfix, "Object reuse code based on multi-pack-index
// sent an unwanted copy of object"): 2.43 refuses it outright ("bad boolean
// config value 'multi'"), and 2.44 to 2.47 accept it and, reusing from
// several packs that hold one object — which a cache's are, since
// receive-pack completes each thin push with the bases it borrowed from the
// packs before it — sent that object twice or a delta without its base, so
// the client's index-pack refused the whole pack ("REF_DELTA … already
// resolved", "pack has N unresolved deltas") and every full clone of a
// repository the maintainer had written a bitmap for failed on 2.47.3,
// Debian trixie's git. Everything else a git node runs is older:
// proc-receive (2.29), index-pack --rev-index (2.31), object-type filters
// (2.32), multi-pack-index bitmaps (2.34) and merge-tree --write-tree
// --merge-base (2.40).
//
// THE FLOOR FOLLOWS THE COMMANDS, never the reverse:
// TestTheNodeFloorWritesALayerOverANamedBase runs the layer on the floor's
// own build, and a later part that needs more of git raises this rather than
// bend its commands to fit it. EACH FLOOR IS CHOSEN ONCE, so that no later
// capability raises it under a node an operator has already deployed: both
// moved before any tag shipped Go code. A git release newer than the floor is
// what the engine image builds (scripts/git-pin.env), never what the floor is.
var NodeMinVersion = Version{2, 55, 0}

// Floor is which of the two floors a git is held to — a closed set, so a
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

// ParseRelease reads a release as [Version.String] writes it — "2.56.0", its
// exact inverse — where [ParseVersion] reads what `git version` prints: three
// decimal numbers joined by dots, each with no sign and no leading zero, nine
// digits at most. It is how a git node reads the release a published layer's
// row names (code.LayerRef.Git, which the domain holds to the same spelling
// without importing this package), so a node knows a layer written by a newer
// git before it fetches a byte of it. Anything else — "2.56", "v2.56.0", a
// vendor's suffix, a version line — is refused: a row spells exactly what
// String wrote, and a spelling it did not write is not a release this reads.
func ParseRelease(s string) (Version, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("gitproc: %q is not a release as Version.String spells one (2.56.0)",
			textcut.Ellipsis(s, 120))
	}
	var v Version
	for i, field := range []*int{&v.Major, &v.Minor, &v.Patch} {
		n := parts[i]
		// NINE DIGITS AT MOST, so every number fits an int on every platform
		// the engine builds for — the bound code.LayerRef.Check holds too.
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

// ErrTooOld is the refusal of a git below its floor. Its message names the
// binary, what it is, the floor and its version and what to install, because
// the person reading it cannot go on until they do — and never the engine
// image's own git, whose version moves with the pin: a sentence naming it
// would be a second copy of scripts/git-pin.env.
//
// The version wanted is the floor's ([ErrTooOld.Want]) rather than a field
// beside it, which could disagree with it.
type ErrTooOld struct {
	Path  string
	Found Version
	Floor Floor
}

// Want is the version the floor needs: the zero Version for an ErrTooOld held
// to no floor.
func (e *ErrTooOld) Want() Version { return e.Floor.Min() }

// Error names the binary, the version found, the floor and what to install.
// An ErrTooOld held to no floor is a call site's bug, never an operator's, and
// says so rather than guess which floor was meant.
func (e *ErrTooOld) Error() string {
	switch e.Floor {
	case FloorNode:
		return fmt.Sprintf("gitproc: %s is git %s, and a git node needs git %s or newer "+
			"(gitproc.NodeMinVersion, the node floor): install a newer git on this node's PATH — "+
			"the engine image builds the newest git release — or on macOS Homebrew's",
			e.Path, e.Found, NodeMinVersion)
	case FloorClient:
		return fmt.Sprintf("gitproc: %s is git %s, and a client of the code host needs git %s or newer "+
			"(gitproc.ClientMinVersion, the client floor): install a newer git — on macOS, Homebrew's: "+
			"Apple's bundled git is 2.39", e.Path, e.Found, ClientMinVersion)
	}
	return fmt.Sprintf("gitproc: %s is git %s, held to no floor (ErrTooOld.Floor %q)", e.Path, e.Found, string(e.Floor))
}
