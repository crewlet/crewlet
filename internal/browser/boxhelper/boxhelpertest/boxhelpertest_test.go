package boxhelpertest

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE FLOOR NODE IS THE MAJOR IT STANDS FOR: scripts/node-pin.env names
// exactly the floor's release and a SHA-256 for each of the four platforms the
// engine is released for, the release is of boxhelper.MinNodeMajor's major,
// the build CREWLET_TEST_NODE_FLOOR names answers to be that release exactly,
// and the npm it keeps is of boxhelper.MinNPMMajor's major, so the suite's
// floor is the Node and npm a box is held to. Raising MinNodeMajor moves the
// pin and MinNPMMajor in the same change.
//
// Mutations, each red: MinNodeMajor = 24 (the pin is of 22); a build of
// another patch named; a named build without lib/node_modules/npm;
// MinNPMMajor = 11 (the floor's npm is 10).
func TestTheFloorNodeIsTheMajorItStandsFor(t *testing.T) {
	t.Parallel()
	pins := prereqtest.ReadPins(t, filepath.Join(sourcetree.Root(t), "scripts", "node-pin.env"))
	names := []string{"NODE_FLOOR_VERSION"}
	for _, platform := range []string{"LINUX_X64", "LINUX_ARM64", "DARWIN_X64", "DARWIN_ARM64"} {
		names = append(names, "NODE_FLOOR_SHA256_"+platform)
	}
	if got := slices.Sorted(maps.Keys(pins)); !slices.Equal(got, slices.Sorted(slices.Values(names))) {
		t.Fatalf("scripts/node-pin.env names %q, want exactly %q", got, names)
	}
	digest := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, name := range names[1:] {
		if !digest.MatchString(pins[name]) {
			t.Errorf("%s=%q is not a SHA-256", name, pins[name])
		}
	}
	release := pins["NODE_FLOOR_VERSION"]
	m := regexp.MustCompile(`^([0-9]+)\.[0-9]+\.[0-9]+$`).FindStringSubmatch(release)
	if m == nil {
		t.Fatalf("NODE_FLOOR_VERSION=%q is not a Node release (three numbers)", release)
	}
	if major, _ := strconv.Atoi(m[1]); major != boxhelper.MinNodeMajor {
		t.Fatalf("the floor pin is Node %s, of major %d, and boxhelper.MinNodeMajor is %d: a floor nobody is "+
			"testing; move the pin to the newest release of Node %d (scripts/node-pin-check.sh prints it) with "+
			"MinNodeMajor", release, major, boxhelper.MinNodeMajor, boxhelper.MinNodeMajor)
	}
	node := Floor(t)
	if got := versionOf(t, nodeAnswer, floorEnv+"="+node, floorRemedy, node).version; got != release {
		t.Errorf("%s=%s is Node %s, and scripts/node-pin.env pins %s: %s", floorEnv, node, got, release, floorRemedy)
	}
	FloorNPM(t)
}

// script is an executable shell script of body, named name, in a directory of
// its own.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// refuseNodeOptions is how a stand-in starts: as a Node refuses a NODE_OPTIONS
// naming an option it predates, it refuses any.
const refuseNodeOptions = "[ -z \"${NODE_OPTIONS-}\" ] || { echo \"node: $NODE_OPTIONS is not allowed in NODE_OPTIONS\" >&2; exit 9; }\n"

// refuseNPMSettings is how a stand-in npm starts after that: as npm 10 and 11
// end, printing nothing, under a loglevel they do not know, it ends under the
// npm_config_loglevel cases spell.
const refuseNPMSettings = "[ -z \"${npm_config_loglevel-}${NPM_CONFIG_LOGLEVEL-}${Npm_Config_LogLevel-}\" ] || exit 1\n"

// nodeStub is a node that answers --version with said.
func nodeStub(t *testing.T, said string) string {
	t.Helper()
	return script(t, t.TempDir(), "node", refuseNodeOptions+"[ \"$1\" = --version ] && echo "+said+" && exit 0\nexit 3\n")
}

// npmStub is an npm that answers --version with said.
func npmStub(t *testing.T, said string) string {
	t.Helper()
	return script(t, t.TempDir(), "npm", refuseNodeOptions+refuseNPMSettings+
		"[ \"$1\" = --version ] && echo "+said+" && exit 0\nexit 3\n")
}

// floorBuild is a build laid out as scripts/node.sh lays one out: a bin/node
// answering --version with node, which runs a script it is handed as Node
// runs npm-cli.js, beside lib/node_modules/npm/bin/npm-cli.js answering
// --version with npm, or no npm where npm is empty. It prints bin/node's path.
func floorBuild(t *testing.T, node, npm string) string {
	t.Helper()
	dir := t.TempDir()
	if npm != "" {
		script(t, filepath.Join(dir, "lib", "node_modules", "npm", "bin"), "npm-cli.js",
			refuseNPMSettings+"[ \"$1\" = --version ] && echo "+npm+" && exit 0\nexit 3\n")
	}
	return script(t, filepath.Join(dir, "bin"), "node", refuseNodeOptions+
		"[ \"$1\" = --version ] && echo "+node+" && exit 0\ncase $1 in *.js) exec /bin/sh \"$@\" ;; esac\nexit 3\n")
}

// npmOf is the npm floorBuild lays out beside node.
func npmOf(node string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(node)), "lib", "node_modules", "npm", "bin", "npm-cli.js")
}

// A NODE OR AN NPM IS FOUND, REQUIRED OR SKIPPED, and held to the floor. Unset,
// each is a missing prerequisite (internal/prereqtest): it fails under the
// requirement the make targets set, naming what makes it present, and skips
// otherwise. Found, its --version, asked without NODE_OPTIONS or npm's
// settings, decides: the machine's at or above the floor, the floor's of it
// exactly. Not parallel: it sets the variables the helpers read.
//
// Mutations, each red: drop an exact-major check (a newer major stands for the
// floor), a floor check (an older one runs the cases), what a program that
// cannot answer printed, the NODE_OPTIONS drop, or the npm settings' drop in a
// case npm reads.
func TestANodeOrNPMIsFoundRequiredOrSkippedAndHeldToTheFloor(t *testing.T) {
	floor, npmFloor := boxhelper.MinNodeMajor, boxhelper.MinNPMMajor
	at := func(major int) string { return fmt.Sprintf("v%d.4.1", major) }
	npmAt := func(major int) string { return fmt.Sprintf("%d.9.9", major) }
	const loader = "node: error while loading shared libraries: libstdc++.so.6: cannot open shared object file"
	nodeCannotStart := script(t, t.TempDir(), "node", "echo '"+loader+"' >&2\nexit 127\n")
	npmCannotStart := script(t, t.TempDir(), "npm", "echo '"+loader+"' >&2\nexit 127\n")
	nodeAt, nodeCurrent, nodeUnderOptions := nodeStub(t, at(floor)), nodeStub(t, at(floor+4)), nodeStub(t, at(floor+4))
	npmAtFloor, npmCurrent := npmStub(t, npmAt(npmFloor)), npmStub(t, npmAt(npmFloor+2))
	npmUnderOptions := npmStub(t, npmAt(npmFloor+2))
	floorAt, floorUnderOptions := floorBuild(t, at(floor), npmAt(npmFloor)), floorBuild(t, at(floor), npmAt(npmFloor))
	floorNodeUnderOptions := nodeStub(t, at(floor))
	type row struct {
		name, require, path, floor, nodeOptions string
		env                                     map[string]string
		find                                    func(testing.TB) string
		fatal, skip                             string // a fragment of each; neither: the program back
		says                                    []string
		want                                    string // the program handed back
	}
	rows := []row{
		{name: "no node on PATH, required", require: "1", path: t.TempDir(), find: Node,
			fatal: "node is not on PATH", says: []string{nodeRemedy}},
		{name: "no node on PATH", path: t.TempDir(), find: Node, skip: nodeRemedy},
		{name: "a node below the floor", require: "1", path: filepath.Dir(nodeStub(t, at(floor-2))), find: Node,
			fatal: fmt.Sprintf("below boxhelper.MinNodeMajor %d", floor)},
		{name: "a node of no Node version", require: "1", path: filepath.Dir(nodeStub(t, "22.4.1")), find: Node,
			fatal: "names no Node version"},
		{name: "a node that cannot start", require: "1", path: filepath.Dir(nodeCannotStart), find: Node,
			fatal: "exit status 127", says: []string{loader}},
		{name: "a node at the floor", require: "1", path: filepath.Dir(nodeAt), find: Node, want: nodeAt},
		{name: "a current node", require: "1", path: filepath.Dir(nodeCurrent), find: Node, want: nodeCurrent},
		{name: "a node under a NODE_OPTIONS it predates", require: "1", path: filepath.Dir(nodeUnderOptions),
			nodeOptions: "--an-option-it-predates", find: Node, want: nodeUnderOptions},
		{name: "no npm on PATH, required", require: "1", path: t.TempDir(), find: NPM,
			fatal: "npm is not on PATH", says: []string{npmRemedy}},
		{name: "no npm on PATH", path: t.TempDir(), find: NPM, skip: npmRemedy},
		{name: "an npm below the floor", require: "1", path: filepath.Dir(npmStub(t, "9.2.0")), find: NPM,
			fatal: fmt.Sprintf("is npm 9.2.0, below boxhelper.MinNPMMajor %d", npmFloor), says: []string{npmRemedy}},
		{name: "an npm of no npm version", require: "1", path: filepath.Dir(npmStub(t, "v10.9.9")), find: NPM,
			fatal: "names no npm version"},
		{name: "an npm that cannot start", require: "1", path: filepath.Dir(npmCannotStart), find: NPM,
			fatal: "exit status 127", says: []string{loader}},
		{name: "an npm at the floor", require: "1", path: filepath.Dir(npmAtFloor), find: NPM, want: npmAtFloor},
		{name: "a current npm", require: "1", path: filepath.Dir(npmCurrent), find: NPM, want: npmCurrent},
		{name: "an npm under a NODE_OPTIONS it predates", require: "1", path: filepath.Dir(npmUnderOptions),
			nodeOptions: "--an-option-it-predates", find: NPM, want: npmUnderOptions},
		{name: "the floor unset and required", require: "1", find: Floor, fatal: floorEnv + " is unset",
			says: []string{"`make node` fetches it"}},
		{name: "the floor unset", find: Floor, skip: "`make node` fetches it"},
		{name: "the floor relative", require: "1", floor: "bin/node", find: Floor, fatal: "not an absolute path"},
		{name: "the floor missing", require: "1", floor: filepath.Join(t.TempDir(), "node"), find: Floor,
			fatal: "names nothing this case can reach"},
		{name: "a floor of the next even major", require: "1", floor: nodeStub(t, at(floor+2)), find: Floor,
			fatal: "a floor nobody is testing"},
		{name: "a floor below the floor", require: "1", floor: nodeStub(t, at(floor-2)), find: Floor,
			fatal: "a floor nobody is testing"},
		{name: "the floor's build", require: "1", floor: nodeAt, find: Floor, want: nodeAt},
		{name: "the floor's build under a NODE_OPTIONS it predates", require: "1", floor: floorNodeUnderOptions,
			nodeOptions: "--an-option-it-predates", find: Floor, want: floorNodeUnderOptions},
		{name: "the floor's npm with the floor unset and required", require: "1", find: FloorNPM,
			fatal: floorEnv + " is unset", says: []string{"`make node` fetches it"}},
		{name: "the floor's npm with the floor unset", find: FloorNPM, skip: "`make node` fetches it"},
		{name: "a floor build without its npm", require: "1", floor: floorBuild(t, at(floor), ""), find: FloorNPM,
			fatal: "has no npm beside it, which `make node` keeps"},
		{name: "a floor's npm of the next major", require: "1", floor: floorBuild(t, at(floor), npmAt(npmFloor+1)),
			find: FloorNPM, fatal: fmt.Sprintf("boxhelper.MinNPMMajor is %d", npmFloor)},
		{name: "a floor's npm below the floor", require: "1", floor: floorBuild(t, at(floor), "9.2.0"),
			find: FloorNPM, fatal: fmt.Sprintf("boxhelper.MinNPMMajor is %d", npmFloor)},
		{name: "a floor's npm with a Node of another major", require: "1",
			floor: floorBuild(t, at(floor+2), npmAt(npmFloor)), find: FloorNPM, fatal: "a floor nobody is testing"},
		{name: "the floor's npm", require: "1", floor: floorAt, find: FloorNPM, want: npmOf(floorAt)},
		{name: "the floor's npm under a NODE_OPTIONS and npm settings it refuses", require: "1",
			floor: floorUnderOptions, nodeOptions: "--an-option-it-predates",
			env: map[string]string{"NPM_CONFIG_LOGLEVEL": "an-unknown-level"}, find: FloorNPM,
			want: npmOf(floorUnderOptions)},
	}
	for _, name := range []string{"npm_config_loglevel", "NPM_CONFIG_LOGLEVEL", "Npm_Config_LogLevel"} {
		npm := npmStub(t, npmAt(npmFloor+1))
		rows = append(rows, row{name: "an npm under a " + name + " it does not know", require: "1",
			path: filepath.Dir(npm), env: map[string]string{name: "an-unknown-level"}, find: NPM, want: npm})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(prereqtest.RequireEnv, tc.require)
			t.Setenv(floorEnv, tc.floor)
			t.Setenv("NODE_OPTIONS", tc.nodeOptions)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			if tc.path != "" {
				t.Setenv("PATH", tc.path)
			}
			var got string
			ended := prereqtest.Run(t, func(tb testing.TB) { got = tc.find(tb) })
			switch {
			case tc.fatal != "":
				if !strings.Contains(ended.Fatal, tc.fatal) || ended.Skip != "" {
					t.Errorf("failed %q, skipped %q; want a failure saying %q", ended.Fatal, ended.Skip, tc.fatal)
				}
				for _, s := range tc.says {
					if !strings.Contains(ended.Fatal, s) {
						t.Errorf("the failure %q does not say %q", ended.Fatal, s)
					}
				}
			case tc.skip != "":
				if !strings.Contains(ended.Skip, tc.skip) || ended.Fatal != "" {
					t.Errorf("failed %q, skipped %q; want a skip saying %q", ended.Fatal, ended.Skip, tc.skip)
				}
			default:
				if ended != (prereqtest.Ended{}) || got != tc.want {
					t.Errorf("found %q (ended %+v), want %s", got, ended, tc.want)
				}
			}
		})
	}
}
