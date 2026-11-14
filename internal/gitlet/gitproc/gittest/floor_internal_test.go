package gittest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/prereqtest"
)

// THE FLOOR JUDGEMENT IS THE FLOORS': a git below a floor cannot stand for
// it, one at or above it can — and one NAMED as a floor's own build must be of
// that floor's series as well, so a newer git named in its place is a floor
// nobody is testing rather than a pass. A floor that is not one is refused.
//
// Mutation: drop the series check, and 2.41.0 stands for the client floor's
// build — red.
func TestTheFloorJudgementIsTheFloors(t *testing.T) {
	t.Parallel()
	v := func(major, minor, patch int) gitproc.Version {
		return gitproc.Version{Major: major, Minor: minor, Patch: patch}
	}
	for _, tc := range []struct {
		found  gitproc.Version
		floor  gitproc.Floor
		series bool
		want   string // a fragment of the problem; "" is none
	}{
		{v(2, 39, 5), gitproc.FloorClient, false, "below the client floor, 2.40.0"},
		{v(2, 40, 0), gitproc.FloorClient, false, ""},
		{v(2, 56, 0), gitproc.FloorClient, false, ""},
		{v(2, 54, 9), gitproc.FloorNode, false, "below the node floor, 2.55.0"},
		{v(2, 55, 0), gitproc.FloorNode, false, ""},
		{v(3, 0, 0), gitproc.FloorNode, false, ""},
		// A floor's own build: its series, at or above it.
		{v(2, 40, 4), gitproc.FloorClient, true, ""},
		{v(2, 41, 0), gitproc.FloorClient, true, "not of the client floor's series, 2.40"},
		{v(2, 39, 9), gitproc.FloorClient, true, "below the client floor"},
		{v(2, 55, 3), gitproc.FloorNode, true, ""},
		{v(2, 56, 0), gitproc.FloorNode, true, "not of the node floor's series, 2.55"},
		{v(3, 55, 0), gitproc.FloorNode, true, "not of the node floor's series"},
		{v(2, 56, 0), "", false, "held to no floor"},
	} {
		got := floorProblem(tc.found, tc.floor, tc.series)
		if (got == "") != (tc.want == "") || !strings.Contains(got, tc.want) {
			t.Errorf("floorProblem(%s, %q, series %v) = %q, want %q", tc.found, tc.floor, tc.series, got, tc.want)
		}
	}
}

// A FLOOR'S GIT IS NAMED, OR REQUIRED, OR SKIPPED — and never guessed. Unset,
// the floor's arm is a missing prerequisite by internal/prereqtest's one rule:
// it fails where CREWLET_REQUIRE_PREREQUISITES is 1, which the make targets
// that run the suite set, naming the variable and the recipe that builds the
// git, and is skipped otherwise naming `make git`; a value of the requirement
// that is neither is refused rather than read as either. Named, it must be an
// absolute path to an executable of the floor's series, or the variable is
// named; the floor's own build is handed back.
//
// Not parallel: it sets the variables every floor's arm reads.
//
// Mutation: read CREWLET_REQUIRE_PREREQUISITES as set whenever it is
// non-empty (prereqtest.Required), and "true" requires the floors — red.
func TestAFloorsGitIsNamedRequiredOrSkipped(t *testing.T) {
	floorBuild := Fake(t, "git version 2.55.2", "exit 0")
	nextSeries := Fake(t, "git version 2.56.0", "exit 0")
	unset := nodeFloorEnv + " is unset, and it names the node floor's own git"
	for _, tc := range []struct {
		name, require, git string
		fatal, skip        string // a fragment of the failure or the skip; neither: the git back
	}{
		{name: "unset and required", require: "1", fatal: unset},
		{name: "unset", skip: unset + ", which this arm runs: `make git` builds it"},
		{name: "a requirement that is no setting", require: "true", fatal: prereqtest.RequireEnv + `="true"`},
		{name: "relative", require: "1", git: "git", fatal: "not an absolute path"},
		{name: "missing", require: "1", git: filepath.Join(t.TempDir(), "git"), fatal: nodeFloorEnv},
		{name: "another series", require: "1", git: nextSeries, fatal: "not of the node floor's series"},
		{name: "the floor's build", require: "1", git: floorBuild},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(prereqtest.RequireEnv, tc.require)
			t.Setenv(nodeFloorEnv, tc.git)
			var got string
			r := prereqtest.Run(t, func(tb testing.TB) { got = NodeFloor(tb) })
			switch {
			case tc.fatal != "":
				if !strings.Contains(r.Fatal, tc.fatal) || r.Skip != "" {
					t.Errorf("failed %q, skipped %q; want a failure saying %q", r.Fatal, r.Skip, tc.fatal)
				}
			case tc.skip != "":
				if !strings.Contains(r.Skip, tc.skip) || r.Fatal != "" {
					t.Errorf("failed %q, skipped %q; want a skip saying %q", r.Fatal, r.Skip, tc.skip)
				}
			default:
				if r.Fatal != "" || r.Skip != "" || got != tc.git {
					t.Errorf("NodeFloor = %q (failed %q, skipped %q), want %s", got, r.Fatal, r.Skip, tc.git)
				}
			}
		})
	}
}
