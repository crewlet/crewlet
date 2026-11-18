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
// and the build CREWLET_TEST_NODE_FLOOR names answers to be that release
// exactly, so the suite's floor is the Node a box is held to. Raising
// MinNodeMajor moves the pin in the same change.
//
// Mutation: MinNodeMajor = 24 -> the pin is of major 22 (red).
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
			"testing — move the pin to the newest release of Node %d (scripts/node-pin-check.sh prints it) with "+
			"MinNodeMajor", release, major, boxhelper.MinNodeMajor, boxhelper.MinNodeMajor)
	}
	node := Floor(t)
	if got := versionOf(t, floorEnv+"="+node, node, floorRemedy).version; got != release {
		t.Errorf("%s=%s is Node %s, and scripts/node-pin.env pins %s: %s", floorEnv, node, got, release, floorRemedy)
	}
}

// stub is an executable answering --version as a Node of version does, which
// refuses to start under a NODE_OPTIONS, as a Node refuses one naming an
// option it predates; body, where given, is the whole script instead.
func stub(t *testing.T, version, body string) string {
	t.Helper()
	dir := t.TempDir()
	if body == "" {
		body = "[ -z \"${NODE_OPTIONS-}\" ] || { echo \"node: $NODE_OPTIONS is not allowed in NODE_OPTIONS\" >&2; " +
			"exit 9; }\n[ \"$1\" = --version ] && echo " + version + " && exit 0\nexit 3\n"
	}
	path := filepath.Join(dir, "node")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A NODE IS FOUND, REQUIRED OR SKIPPED — and held to the floor. Unset, each is
// a missing prerequisite (internal/prereqtest): it fails under the
// requirement the make targets set, naming what is missing and what makes it
// present, and skips otherwise. Found, its --version, asked without the
// caller's NODE_OPTIONS, decides: the machine's at or above
// boxhelper.MinNodeMajor, the floor's of it exactly. Not parallel: it sets the
// variables the helpers read.
//
// Mutations, each red: drop the floor's exact-major check (a newer major
// stands for it), the machine's floor (an older one runs the cases), what a
// node that cannot answer printed from its failure, or the NODE_OPTIONS drop.
func TestANodeIsFoundRequiredOrSkippedAndHeldToTheFloor(t *testing.T) {
	floor := boxhelper.MinNodeMajor
	at := func(major int) string { return fmt.Sprintf("v%d.4.1", major) }
	const loader = "node: error while loading shared libraries: libstdc++.so.6: cannot open shared object file"
	cannotStart := stub(t, "", "echo '"+loader+"' >&2\nexit 127\n")
	for _, tc := range []struct {
		name, require, path, floorEnv, nodeOptions string
		find                                       func(testing.TB) string
		fatal, skip                                string // a fragment of each; neither: the node back
		says                                       []string
		want                                       string // the node handed back
	}{
		{name: "no node on PATH, required", require: "1", path: t.TempDir(), find: Node,
			fatal: "node is not on PATH", says: []string{nodeRemedy}},
		{name: "no node on PATH", path: t.TempDir(), find: Node, skip: nodeRemedy},
		{name: "a node below the floor", require: "1", path: stub(t, at(floor-2), ""), find: Node,
			fatal: fmt.Sprintf("below boxhelper.MinNodeMajor %d", floor)},
		{name: "a node of no Node version", require: "1", path: stub(t, "22.4.1", ""), find: Node,
			fatal: "names no Node version"},
		{name: "a node that cannot start", require: "1", path: cannotStart, find: Node,
			fatal: "exit status 127", says: []string{loader}},
		{name: "a node at the floor", require: "1", path: stub(t, at(floor), ""), find: Node},
		{name: "a current node", require: "1", path: stub(t, at(floor+4), ""), find: Node},
		{name: "a node under a NODE_OPTIONS it predates", require: "1", path: stub(t, at(floor+4), ""),
			nodeOptions: "--an-option-it-predates", find: Node},
		{name: "the floor unset and required", require: "1", find: Floor, fatal: floorEnv + " is unset",
			says: []string{"`make node` fetches it"}},
		{name: "the floor unset", find: Floor, skip: "`make node` fetches it"},
		{name: "the floor relative", require: "1", floorEnv: "bin/node", find: Floor,
			fatal: "not an absolute path"},
		{name: "the floor missing", require: "1", floorEnv: filepath.Join(t.TempDir(), "node"), find: Floor,
			fatal: "names nothing this case can run"},
		{name: "a floor of the next even major", require: "1", floorEnv: stub(t, at(floor+2), ""), find: Floor,
			fatal: "a floor nobody is testing"},
		{name: "a floor below the floor", require: "1", floorEnv: stub(t, at(floor-2), ""), find: Floor,
			fatal: "a floor nobody is testing"},
		{name: "the floor's build", require: "1", floorEnv: stub(t, at(floor), ""), find: Floor},
		{name: "the floor's build under a NODE_OPTIONS it predates", require: "1", floorEnv: stub(t, at(floor), ""),
			nodeOptions: "--an-option-it-predates", find: Floor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(prereqtest.RequireEnv, tc.require)
			t.Setenv(floorEnv, tc.floorEnv)
			t.Setenv("NODE_OPTIONS", tc.nodeOptions)
			want := tc.floorEnv
			if tc.path != "" {
				dir := tc.path
				if strings.HasSuffix(dir, string(filepath.Separator)+"node") {
					dir, want = filepath.Dir(dir), tc.path
				}
				t.Setenv("PATH", dir)
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
				if ended != (prereqtest.Ended{}) || got != want {
					t.Errorf("found %q (ended %+v), want %s", got, ended, want)
				}
			}
		})
	}
}
