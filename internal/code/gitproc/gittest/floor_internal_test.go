package gittest

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
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
// the floor's arm fails where CREWLET_REQUIRE_GIT_FLOOR is 1, which every make
// target sets, naming the variable and the recipe that builds the git, and is
// skipped otherwise naming `make git`; a value of CREWLET_REQUIRE_GIT_FLOOR
// that is neither is refused rather than read as either. Named, it must be an
// absolute path to an executable of the floor's series, or the variable is
// named; the floor's own build is handed back.
//
// Not parallel: it sets the variables every floor's arm reads.
//
// Mutation: read CREWLET_REQUIRE_GIT_FLOOR as set whenever it is non-empty,
// and "true" requires the floors — red.
func TestAFloorsGitIsNamedRequiredOrSkipped(t *testing.T) {
	floorBuild := Fake(t, "git version 2.55.2", "exit 0")
	nextSeries := Fake(t, "git version 2.56.0", "exit 0")
	for _, tc := range []struct {
		name, require, git string
		fatal, skip        string // a fragment of the failure or the skip; neither: the git back
	}{
		{name: "unset and required", require: "1", fatal: nodeFloorEnv + " is unset, and " + requireFloorEnv + "=1"},
		{name: "unset", skip: "`make git` builds it"},
		{name: "a requirement that is no setting", require: "true", fatal: requireFloorEnv + `="true"`},
		{name: "relative", require: "1", git: "git", fatal: "not an absolute path"},
		{name: "missing", require: "1", git: filepath.Join(t.TempDir(), "git"), fatal: nodeFloorEnv},
		{name: "another series", require: "1", git: nextSeries, fatal: "not of the node floor's series"},
		{name: "the floor's build", require: "1", git: floorBuild},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(requireFloorEnv, tc.require)
			t.Setenv(nodeFloorEnv, tc.git)
			r := &outcome{TB: t}
			var got string
			done := make(chan struct{})
			go func() {
				defer close(done)
				got = NodeFloor(r)
			}()
			<-done
			switch {
			case tc.fatal != "":
				if !strings.Contains(r.fatal, tc.fatal) || r.skip != "" {
					t.Errorf("failed %q, skipped %q; want a failure saying %q", r.fatal, r.skip, tc.fatal)
				}
			case tc.skip != "":
				if !strings.Contains(r.skip, tc.skip) || r.fatal != "" {
					t.Errorf("failed %q, skipped %q; want a skip saying %q", r.fatal, r.skip, tc.skip)
				}
			default:
				if r.fatal != "" || r.skip != "" || got != tc.git {
					t.Errorf("NodeFloor = %q (failed %q, skipped %q), want %s", got, r.fatal, r.skip, tc.git)
				}
			}
		})
	}
}

// outcome is a testing.TB that records the first failure or skip it is
// handed and ends the goroutine calling it, as the real ones do.
type outcome struct {
	testing.TB
	fatal, skip string
}

func (o *outcome) Helper() {}

func (o *outcome) Fatal(args ...any) {
	o.fatal = fmt.Sprint(args...)
	runtime.Goexit()
}

func (o *outcome) Fatalf(format string, args ...any) {
	o.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (o *outcome) Skipf(format string, args ...any) {
	o.skip = fmt.Sprintf(format, args...)
	runtime.Goexit()
}
