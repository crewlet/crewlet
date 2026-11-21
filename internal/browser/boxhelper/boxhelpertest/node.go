package boxhelpertest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// The words of a case that lacks a node or an npm: what makes each present. A
// one-package loop names the floor's build as the make targets do, the
// substitution its own statement, so a recipe that failed is never an empty
// variable.
const (
	nodeRemedy = "install a current node, as ci.yml does"
	npmRemedy  = "install a current node with the npm it bundles, as ci.yml does; a distribution's separate npm " +
		"package may be older than every npm a supported Node bundles"
	floorRemedy = "`make node` fetches it and `scripts/node.sh floor` prints its path, which the make " +
		"targets that run the whole suite name here; for the box helper's packages, as two statements: " +
		"`floor=$(scripts/node.sh floor) && " + floorEnv + "=\"$floor\" go test ./internal/browser/boxhelper/...`"
)

// Node is the machine's node, found on PATH: an executable at or above
// boxhelper.MinNodeMajor.
func Node(t testing.TB) string {
	t.Helper()
	node := prereqtest.OnPath(t, "node", "a case of the box helper runs under it beside the floor's pinned build",
		nodeRemedy)
	if v := versionOf(t, nodeAnswer, "the node on PATH, "+node, nodeRemedy, node); v.major < boxhelper.MinNodeMajor {
		t.Fatalf("the node on PATH, %s, is Node %s, below boxhelper.MinNodeMajor %d, the oldest a box may run the "+
			"helper on: %s", node, v.version, boxhelper.MinNodeMajor, nodeRemedy)
	}
	return node
}

// NPM is the machine's npm, found on PATH: an executable at or above
// boxhelper.MinNPMMajor, which runs under whichever node PATH finds, as a
// box's own npm does.
func NPM(t testing.TB) string {
	t.Helper()
	npm := prereqtest.OnPath(t, "npm", "a case of the box helper runs it beside the floor's own npm", npmRemedy)
	if v := versionOf(t, npmAnswer, "the npm on PATH, "+npm, npmRemedy, npm); v.major < boxhelper.MinNPMMajor {
		t.Fatalf("the npm on PATH, %s, is npm %s, below boxhelper.MinNPMMajor %d, the npm the oldest Node a box "+
			"may run bundles: %s", npm, v.version, boxhelper.MinNPMMajor, npmRemedy)
	}
	return npm
}

// Floor is the floor's pinned build, the newest release of Node
// boxhelper.MinNodeMajor: an executable of that major exactly. Named as
// chromiumtest.Floor is, never NodeFloor, which is gittest's floor for a
// Gitlet node's git.
func Floor(t testing.TB) string {
	t.Helper()
	node := prereqtest.Named(t, floorEnv, "the floor's pinned Node, the oldest the box helper supports, which a "+
		"case of the helper runs under beside the machine's own", floorRemedy)
	if v := versionOf(t, nodeAnswer, floorEnv+"="+node, floorRemedy, node); v.major != boxhelper.MinNodeMajor {
		t.Fatalf("%s=%s is Node %s, and the floor is Node %d (boxhelper.MinNodeMajor): a floor nobody is testing; %s",
			floorEnv, node, v.version, boxhelper.MinNodeMajor, floorRemedy)
	}
	return node
}

// FloorNPM is the npm the floor's pinned build bundles, which `make node`
// keeps beside its node: the path of its npm-cli.js, which a case runs with
// [Floor]'s node. It is of boxhelper.MinNPMMajor exactly, the npm every
// release of the floor's major bundles.
func FloorNPM(t testing.TB) string {
	t.Helper()
	node := Floor(t)
	npm := filepath.Join(filepath.Dir(filepath.Dir(node)), "lib", "node_modules", "npm", "bin", "npm-cli.js")
	if _, err := os.Stat(npm); err != nil {
		t.Fatalf("%s=%s has no npm beside it, which `make node` keeps (%v): %s", floorEnv, node, err, floorRemedy)
	}
	if v := versionOf(t, npmAnswer, "the floor's npm, "+npm, floorRemedy, node, npm); v.major != boxhelper.MinNPMMajor {
		t.Fatalf("the floor's npm, %s, is npm %s, and boxhelper.MinNPMMajor is %d: MinNPMMajor is the npm Node %d's "+
			"releases bundle, so it is %d, and it moves with MinNodeMajor", npm, v.version, boxhelper.MinNPMMajor,
			boxhelper.MinNodeMajor, v.major)
	}
	return npm
}

// answer is what a program said to --version: its version, and that
// version's major.
type answer struct {
	version string
	major   int
}

// grammar is a program's --version line, whole: its first group the version
// and its second the major, and name what the program is called.
type grammar struct {
	name string
	line *regexp.Regexp
}

// What Node and npm answer --version with: v and three numbers, and the three
// numbers alone, either followed by a prerelease's tag (Node's -alpha.1, -rc.2
// and nightlies, npm's -pre.0). A prerelease is a version of its major:
// ci.yml's `latest` installs whichever Node is newest, a prerelease included.
var (
	nodeAnswer = grammar{"Node", regexp.MustCompile(`^v(([0-9]+)\.[0-9]+\.[0-9]+` + prerelease + `)$`)}
	npmAnswer  = grammar{"npm", regexp.MustCompile(`^(([0-9]+)\.[0-9]+\.[0-9]+` + prerelease + `)$`)}
)

// prerelease is a version's optional prerelease tag, as semver.org spells
// one: a hyphen, then identifiers of letters, digits and hyphens, joined by
// dots.
const prerelease = `(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?`

// versionTimeout bounds one --version: a node or an npm answers in well under
// a second, so a minute is a hang, never a slow machine.
const versionTimeout = time.Minute

// versionStderr is how much of what a program that could not answer printed a
// failure carries: the end of it, where a loader or Node says why in a line or
// two (a library it lacks, an option it refuses), which 1 KiB holds several
// times over without carrying a whole crash's trace into the failure.
const versionStderr = 1 << 10

// npmSettings begins the name of every variable npm reads a setting from, in
// any case.
const npmSettings = "npm_config_"

// versions is each command's --version, asked once per test binary.
var versions sync.Map // the command, its words joined by NUL → func() (answer, error)

// versionOf is the version the command argv says it is, read by g, or t fails
// naming what, what the command printed if it could not answer, and remedy.
// It is asked in the caller's environment without NODE_OPTIONS or npm's
// settings (the package doc says why).
func versionOf(t testing.TB, g grammar, what, remedy string, argv ...string) answer {
	t.Helper()
	ask, _ := versions.LoadOrStore(strings.Join(argv, "\x00"), sync.OnceValues(func() (answer, error) {
		ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], slices.Concat(argv[1:], []string{"--version"})...)
		cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
			name, _, _ := strings.Cut(kv, "=")
			return name == "NODE_OPTIONS" || strings.HasPrefix(strings.ToLower(name), npmSettings)
		})
		out, err := cmd.Output()
		if err != nil {
			return answer{}, cannotStart(err)
		}
		said := strings.TrimSpace(string(out))
		m := g.line.FindStringSubmatch(said)
		if m == nil {
			return answer{}, fmt.Errorf("it answered --version with %q, which names no %s version", said, g.name)
		}
		major, err := strconv.Atoi(m[2])
		if err != nil {
			return answer{}, err
		}
		return answer{version: m[1], major: major}, nil
	}))
	v, err := ask.(func() (answer, error))()
	if err != nil {
		t.Fatalf("%s: %v; %s", what, err, remedy)
	}
	return v
}

// cannotStart is a program's failure to answer --version, with what it
// printed last.
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
