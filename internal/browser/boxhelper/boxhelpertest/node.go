// Package boxhelpertest hands a case of the box helper the Nodes it runs
// under: the machine's own node ([Node]), at or above boxhelper.MinNodeMajor,
// and the floor's pinned build ([Floor]), of that major exactly, so what a case
// asserts holds on the oldest Node a box may carry as well as on a current one
// (boxhelper's package doc says why the floor is pinned).
//
// # Where each comes from
//
// The machine's node is found on PATH, as internal/prereqtest finds a program
// the suite needs: under the make targets that run the suite a missing one
// fails the case, and only a bare `go test` outside make skips it. Every make
// target that runs the whole suite (`test`, `test-solo`, `test-norace`) fetches
// the floor's build (`make node`, the one recipe scripts/node.sh) and names it
// in CREWLET_TEST_NODE_FLOOR, under the same rule. A node that is
// found is held by what it answers to --version, asked without NODE_OPTIONS,
// which a Node refuses to start under when it names an option that Node
// predates: the machine's at or above the floor, the floor's of its major
// exactly, since a newer Node in the floor's place is a floor nobody is
// testing.
//
// Test support (the name ends in `test`), so internal/fixedwait reads it as
// test code: it holds no fixed wait, and every bound in it is a context's.
package boxhelpertest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/prereqtest"
)

// floorEnv is the variable the make targets that run the whole suite name the
// floor's build in (the Makefile's NODE_FLOOR_ENV, from scripts/node.sh);
// nothing the engine runs reads it.
const floorEnv = "CREWLET_TEST_NODE_FLOOR"

// The words of a case that lacks a node: what makes each present. A one-package
// loop names the floor's build as the make targets do, the substitution its
// own statement, so a recipe that failed is never an empty variable.
const (
	nodeRemedy  = "install a current node, as ci.yml does"
	floorRemedy = "`make node` fetches it and `scripts/node.sh floor` prints its path, which the make " +
		"targets that run the whole suite name here; for one package, as two statements: " +
		"floor=$(scripts/node.sh floor) && " + floorEnv + "=\"$floor\" go test ./…"
)

// Node is the machine's node, found on PATH: an executable at or above
// boxhelper.MinNodeMajor.
func Node(t testing.TB) string {
	t.Helper()
	node := prereqtest.OnPath(t, "node", "a case of the box helper runs under it beside the floor's pinned build",
		nodeRemedy)
	if v := versionOf(t, "the node on PATH, "+node, node, nodeRemedy); v.major < boxhelper.MinNodeMajor {
		t.Fatalf("the node on PATH, %s, is Node %s, below boxhelper.MinNodeMajor %d, the oldest a box may run the "+
			"helper on: %s", node, v.version, boxhelper.MinNodeMajor, nodeRemedy)
	}
	return node
}

// Floor is the floor's pinned build, the newest release of Node
// boxhelper.MinNodeMajor: an executable of that major exactly. Named as
// chromiumtest.Floor is, never NodeFloor, which is gittest's floor for a
// Gitlet node's git.
func Floor(t testing.TB) string {
	t.Helper()
	node := prereqtest.Named(t, floorEnv, "the floor's pinned Node, the oldest the box helper supports, which a "+
		"case of the helper runs under beside the machine's own", floorRemedy)
	if v := versionOf(t, floorEnv+"="+node, node, floorRemedy); v.major != boxhelper.MinNodeMajor {
		t.Fatalf("%s=%s is Node %s, and the floor is Node %d (boxhelper.MinNodeMajor): a floor nobody is testing — %s",
			floorEnv, node, v.version, boxhelper.MinNodeMajor, floorRemedy)
	}
	return node
}

// version is what a node said to --version.
type version struct {
	version string
	major   int
}

// versionAnswer is a node's whole --version line: v and three numbers.
var versionAnswer = regexp.MustCompile(`^v(([0-9]+)\.[0-9]+\.[0-9]+)$`)

// versionTimeout bounds one --version: a node answers in well under a second,
// so a minute is a hang, never a slow machine.
const versionTimeout = time.Minute

// versionStderr is how much of what a node that could not answer printed a
// failure carries: the end of it, where a loader or Node says why in a line or
// two (a library it lacks, an option it refuses), which 1 KiB holds several
// times over without carrying a whole crash's trace into the failure.
const versionStderr = 1 << 10

// versions is each node's --version, asked once per test binary.
var versions sync.Map // path → func() (version, error)

// versionOf is the version node says it is, or t fails naming what, what the
// node printed if it could not answer, and remedy.
func versionOf(t testing.TB, what, node, remedy string) version {
	t.Helper()
	ask, _ := versions.LoadOrStore(node, sync.OnceValues(func() (version, error) {
		ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, node, "--version")
		cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "NODE_OPTIONS=") })
		out, err := cmd.Output()
		if err != nil {
			return version{}, cannotStart(err)
		}
		said := strings.TrimSpace(string(out))
		m := versionAnswer.FindStringSubmatch(said)
		if m == nil {
			return version{}, fmt.Errorf("it answered --version with %q, which names no Node version", said)
		}
		major, err := strconv.Atoi(m[2])
		if err != nil {
			return version{}, err
		}
		return version{version: m[1], major: major}, nil
	}))
	v, err := ask.(func() (version, error))()
	if err != nil {
		t.Fatalf("%s: %v; %s", what, err, remedy)
	}
	return v
}

// cannotStart is a node's failure to answer --version, with what it printed
// last.
func cannotStart(err error) error {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	said := strings.TrimSpace(string(exit.Stderr))
	if said == "" {
		return err
	}
	if len(said) > versionStderr {
		said = "..." + said[len(said)-versionStderr:]
	}
	return fmt.Errorf("%w, saying: %s", err, said)
}
