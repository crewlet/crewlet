package gittest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// The variables the floors' arm reads. Every make target sets all three, from
// scripts/git.sh (the Makefile's GIT_ENV); nothing the engine runs reads them.
const (
	// nodeFloorEnv names the node floor's build: the newest patch of
	// gitproc.NodeMinVersion's series (`scripts/git.sh node-floor`).
	nodeFloorEnv = "CREWLET_TEST_GIT_NODE_FLOOR"
	// clientFloorEnv names the client floor's build: the newest patch of
	// gitproc.ClientMinVersion's series (`scripts/git.sh client-floor`).
	clientFloorEnv = "CREWLET_TEST_GIT_CLIENT_FLOOR"
	// requireFloorEnv, 1, makes a floor's git that is not named a failure
	// rather than a skip.
	requireFloorEnv = "CREWLET_REQUIRE_GIT_FLOOR"
)

// onPath is the git on PATH and the version it says it is.
type onPath struct {
	git     string
	version gitproc.Version
}

// found is the git on PATH and its version, read once per test binary: the
// git every client and every pool of a test runs unless it names another.
var found = sync.OnceValues(func() (onPath, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return onPath{}, fmt.Errorf("no git on PATH (%w)", err)
	}
	v, err := readVersion(git)
	return onPath{git: git, version: v}, err
})

// Require is a CLIENT's git — what every test drives as a person, a CI
// runner or the author of a fixture: exec.LookPath("git"), read once per test
// binary, held to gitproc.ClientMinVersion, the client floor. Absent,
// unreadable or older, it FAILS t — never skips.
func Require(t testing.TB) string {
	t.Helper()
	return onFloor(t, gitproc.FloorClient)
}

// RequireNode is the git every POOL a test builds runs — the same PATH git,
// held to gitproc.NodeMinVersion, the node floor gitproc.New refuses below.
// It FAILS t, never skips, and a test calls it BEFORE it builds a pool or
// boots an engine with the git role, so a machine below the floor reads what
// to run rather than the refusal an operator would.
func RequireNode(t testing.TB) string {
	t.Helper()
	return onFloor(t, gitproc.FloorNode)
}

// onFloor is the git on PATH, failing t unless it meets floor.
func onFloor(t testing.TB, floor gitproc.Floor) string {
	t.Helper()
	p, err := found()
	if err != nil {
		t.Fatalf("%v cannot run this test: %s", err, remedy(floor))
	}
	if problem := floorProblem(p.version, floor, false); problem != "" {
		t.Fatalf("the git on PATH, %s, cannot run this test — %s: %s", p.git, problem, remedy(floor))
	}
	return p.git
}

// remedy is what a contributor runs when the git on PATH cannot stand for
// floor's — the suite's own gits, never the machine's (CONTRIBUTING.md,
// Development setup). Two statements for the one-package loop, because a
// failed recipe in `PATH="$(scripts/git.sh node):$PATH"` leaves the machine's
// git first and says nothing.
func remedy(floor gitproc.Floor) string {
	return fmt.Sprintf("the code host's tests run real git, and %s needs git %s or newer (the %s floor): "+
		"run the suite through `make test`, which builds the pinned gits (`make git`) and puts the node pin "+
		"first on PATH, or for one package `bin=$(scripts/git.sh node) && PATH=\"$bin:$PATH\" go test ./…` "+
		"(CONTRIBUTING.md, Development setup)", whose(floor), floor.Min(), floor)
}

// whose is what floor's git is, in a sentence.
func whose(floor gitproc.Floor) string {
	if floor == gitproc.FloorNode {
		return "every pool a test builds"
	}
	return "a client of the code host"
}

// floorProblem is why a git of version found cannot stand for floor's — ""
// when it can. series also holds it to the floor's own SERIES (the floor's
// major and minor): a floor's build is the newest patch of the series the
// floor names, so a git of a later series named as the floor's is a floor
// nothing is testing.
func floorProblem(found gitproc.Version, floor gitproc.Floor, series bool) string {
	low := floor.Min()
	switch {
	case !floor.Valid():
		return fmt.Sprintf("git %s is held to no floor (%q)", found, string(floor))
	case !found.AtLeast(low):
		return fmt.Sprintf("git %s is below the %s floor, %s", found, floor, low)
	case series && (found.Major != low.Major || found.Minor != low.Minor):
		return fmt.Sprintf("git %s is not of the %s floor's series, %d.%d", found, floor, low.Major, low.Minor)
	}
	return ""
}

// NodeFloor is the node floor's own git — the build CREWLET_TEST_GIT_NODE_FLOOR
// names (`scripts/git.sh node-floor`'s, the newest patch of
// gitproc.NodeMinVersion's series) — which the floors' arm of the code host's
// suite runs every pool on. See [ClientFloor] for what it holds the variable
// to.
func NodeFloor(t testing.TB) string {
	t.Helper()
	return floorGit(t, gitproc.FloorNode, nodeFloorEnv, "node-floor")
}

// ClientFloor is the client floor's own git — the build
// CREWLET_TEST_GIT_CLIENT_FLOOR names (`scripts/git.sh client-floor`'s, the
// newest patch of gitproc.ClientMinVersion's series) — which the floors' arm
// runs every person's git on.
//
// Named, the variable must be an absolute path to an executable file whose
// `git version` is of the floor's series and at or above the floor, or t
// FAILS naming the variable. UNSET, t fails when CREWLET_REQUIRE_GIT_FLOOR is
// 1 — every make target sets it beside the two floors — and is otherwise
// SKIPPED naming `make git`: the one environment skip of the suite's gits, so
// a bare `go test` of one package still runs everything else, and one
// internal/skipgate never judges, since every target it runs under requires
// the floors.
func ClientFloor(t testing.TB) string {
	t.Helper()
	return floorGit(t, gitproc.FloorClient, clientFloorEnv, "client-floor")
}

// floorGit is the git env names for floor, held to it; pin is the name
// scripts/git.sh builds it under.
func floorGit(t testing.TB, floor gitproc.Floor, env, pin string) string {
	t.Helper()
	required, err := floorRequired()
	if err != nil {
		t.Fatal(err)
	}
	git := os.Getenv(env)
	if git == "" {
		if required {
			t.Fatalf("%s is unset, and %s=1 requires the %s floor's git: the make targets set it to "+
				"`$(scripts/git.sh %s)/git` (the Makefile's GIT_ENV)", env, requireFloorEnv, floor, pin)
		}
		t.Skipf("%s is unset: this arm runs the %s floor's own git, which every make target names "+
			"(`make git` builds it, and `scripts/git.sh %s` prints its bin)", env, floor, pin)
	}
	if !filepath.IsAbs(git) {
		t.Fatalf("%s=%q is not an absolute path to a git", env, git)
	}
	info, err := os.Stat(git)
	if err != nil {
		t.Fatalf("%s=%s: %v", env, git, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%s=%s is not an executable file", env, git)
	}
	v, err := floorVersion(git)
	if err != nil {
		t.Fatalf("%s=%s: %v", env, git, err)
	}
	if problem := floorProblem(v, floor, true); problem != "" {
		t.Fatalf("%s=%s cannot stand for the %s floor: %s", env, git, floor, problem)
	}
	return git
}

// floorRequired reads CREWLET_REQUIRE_GIT_FLOOR: unset is false, 1 true, and
// anything else a mistake named rather than read as either.
func floorRequired() (bool, error) {
	switch v := os.Getenv(requireFloorEnv); v {
	case "":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("%s=%q: set it to 1 (the floors' gits are required) or leave it unset", requireFloorEnv, v)
	}
}

// floorVersions is each named git's version, read once per test binary.
var floorVersions sync.Map // path → func() (gitproc.Version, error)

// floorVersion is what the git at path says it is, asked once.
func floorVersion(git string) (gitproc.Version, error) {
	once, _ := floorVersions.LoadOrStore(git, sync.OnceValues(func() (gitproc.Version, error) {
		return readVersion(git)
	}))
	return once.(func() (gitproc.Version, error))()
}
