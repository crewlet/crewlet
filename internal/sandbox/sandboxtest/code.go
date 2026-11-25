package sandboxtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RELAUNCH UNIONS THE TURN'S CODE HOLDS, never replaces them. What a turn has
// named Gitlet's expiry must not take — its scratch repositories — is
// on its row once the turn has parked, and a relaunch is the same turn: an
// earlier job's repository, or one the turn named before a later launch of
// it, is still something it may come back to. Every other
// launch-scoped field a reset clears; this one only grows, each key once in
// the order first taken, and a union past the bound is refused with nothing
// written.
//
// Mutation: replace the set on a reset — the first launch's holds are gone, red.
func testBeginLaunchUnionsCodeHolds(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	first := run("t1")
	first.CodeHolds = []string{"repo-a", "repo-b", "repo-a"}
	mustBeginLaunch(t, s, first)
	if got := mustGet(t, s, "t1").CodeHolds; !slices.Equal(got, []string{"repo-a", "repo-b"}) {
		t.Fatalf("a create holds %v, want [repo-a repo-b], each key once", got)
	}
	second := run("t1")
	second.CodeHolds = []string{"repo-c", "repo-a"}
	if _, err := s.BeginLaunch(ctx, second, sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if got := mustGet(t, s, "t1").CodeHolds; !slices.Equal(got, []string{"repo-a", "repo-b", "repo-c"}) {
		t.Fatalf("a relaunch holds %v, want the union [repo-a repo-b repo-c]", got)
	}
	// AND A RELAUNCH HANDED NOTHING KEEPS EVERYTHING: a native turn that
	// named no new repository still depends on what it named before.
	if _, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch with no holds: %v", err)
	}
	if got := mustGet(t, s, "t1").CodeHolds; len(got) != 3 {
		t.Fatalf("a relaunch handed no holds left %v, want the three it held", got)
	}
	// PAST THE BOUND, refused and nothing written, on a create and a reset.
	over := run("t2")
	for i := range sandbox.MaxCodeHolds + 1 {
		over.CodeHolds = append(over.CodeHolds, fmt.Sprintf("repo-%04d", i))
	}
	if _, err := s.BeginLaunch(ctx, over, sandbox.Fence{}); !errors.Is(err, sandbox.ErrCodeHoldsFull) {
		t.Fatalf("a create past the bound = %v, want ErrCodeHoldsFull", err)
	}
	if _, found, err := s.Get(ctx, "t2"); err != nil || found {
		t.Fatalf("a create refused past the bound left a row: found=%v err=%v", found, err)
	}
	over.TurnID = "t1"
	if _, err := s.BeginLaunch(ctx, over, sandbox.Fence{}); !errors.Is(err, sandbox.ErrCodeHoldsFull) {
		t.Fatalf("a relaunch past the bound = %v, want ErrCodeHoldsFull", err)
	}
	if got := mustGet(t, s, "t1").CodeHolds; len(got) != 3 {
		t.Fatalf("a relaunch refused past the bound left %v, want the three it held", got)
	}
}

// A HOLD IS ADDED TO A LIVE ROW ONLY, AND NEVER LOST TO A RACE. A turn with a
// row takes its hold on a repository through this write BEFORE the request
// that names it, so the write's answer decides whether the request is sent:
// no row, or a row whose ending is decided, holds nothing new (false); a
// write past the bound is refused with nothing written; a key the row holds
// already is held (true, nothing written); and two turns' — a bridged call's
// and a worker's — adds racing on one row both land.
//
// Mutation: write the keys blind rather than through the row's version — one
// of the racing adds is lost, red.
func testAddCodeHoldsAddsToALiveRowOnly(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	if ok, err := s.AddCodeHolds(ctx, "nobody", []string{"repo-a"}); err != nil || ok {
		t.Fatalf("a hold on no row = %v, %v, want false and no error", ok, err)
	}
	if _, err := s.AddCodeHolds(ctx, "nobody", nil); err == nil {
		t.Fatal("a hold naming no key was taken")
	}
	mustLaunched(t, s, run("t1"))
	if ok, err := s.AddCodeHolds(ctx, "t1", []string{"repo-a"}); err != nil || !ok {
		t.Fatalf("a hold on a live row = %v, %v, want it taken", ok, err)
	}
	if ok, err := s.AddCodeHolds(ctx, "t1", []string{"repo-a"}); err != nil || !ok {
		t.Fatalf("a hold the row already carries = %v, %v, want it held", ok, err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if ok, err := s.AddCodeHolds(ctx, "t1", []string{fmt.Sprintf("racer-%d", i)}); err != nil || !ok {
				t.Errorf("racing hold %d = %v, %v", i, ok, err)
			}
		})
	}
	wg.Wait()
	got := mustGet(t, s, "t1").CodeHolds
	if len(got) != 9 || got[0] != "repo-a" {
		t.Fatalf("the row holds %v, want repo-a and all eight racers", got)
	}
	over := make([]string, 0, sandbox.MaxCodeHolds)
	for i := range sandbox.MaxCodeHolds {
		over = append(over, fmt.Sprintf("more-%04d", i))
	}
	if _, err := s.AddCodeHolds(ctx, "t1", over); !errors.Is(err, sandbox.ErrCodeHoldsFull) {
		t.Fatalf("a hold past the bound = %v, want ErrCodeHoldsFull", err)
	}
	if got := mustGet(t, s, "t1").CodeHolds; len(got) != 9 {
		t.Fatalf("a hold refused past the bound left %d keys, want the 9 it held", len(got))
	}
	// AN ENDING DECIDED HOLDS NOTHING NEW: the run is going, and its turn's
	// request is refused before it is sent.
	decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active))
	if ok, err := s.AddCodeHolds(ctx, "t1", []string{"repo-late"}); err != nil || ok {
		t.Fatalf("a hold on a run whose ending is decided = %v, %v, want false and no error", ok, err)
	}
}

// EVERY RUN IS LISTED, WHATEVER ITS STATUS. A run is unended exactly while it
// has a record, so Gitlet's expiry asks this listing which scratch
// repositories parked runs hold — and a row a newer build wrote with a status
// this build does not know is a run whose turn holds them still. Filtered by
// this build's own list, it would drop out mid-upgrade and its repositories
// would be expired under it.
//
// Mutation: filter by sandbox.Active — the newer build's row is missing, red.
func testListRunsListsEveryStatus(t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns) {
	mustLaunched(t, s, run("t1"))
	newer := run("t2")
	newer.Status = "rehearsing"
	newer.CodeHolds = []string{"repo-n"}
	body, err := json.Marshal(newer)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := runs.CreateSandboxRun(t.Context(), "t2", body, nil); err != nil || !created { //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		t.Fatalf("seed the newer build's row: created=%v err=%v", created, err)
	}
	listed, err := s.ListRuns(t.Context())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var ids []string
	for _, r := range listed {
		ids = append(ids, r.TurnID)
		if r.TurnID == "t2" && !slices.Equal(r.CodeHolds, []string{"repo-n"}) {
			t.Errorf("the newer build's row holds %v, want [repo-n]", r.CodeHolds)
		}
	}
	if !slices.Contains(ids, "t1") || !slices.Contains(ids, "t2") {
		t.Fatalf("ListRuns listed %v, want both runs — the one whose status this build does not "+
			"know among them", ids)
	}
}

// runRepo is a repository a run is given, valid by [sandbox.RunRepo.ValidateFor]
// an executor ([sandbox.CloneCoding]).
func runRepo(name, id string) sandbox.RunRepo {
	return sandbox.RunRepo{
		Repo: name, RepoID: id, Base: "0123456789abcdef0123456789abcdef01234567",
		Branch: "seats/swe/ENG-7", Access: sandbox.RepoWrite, Paths: []string{"src"},
	}
}

// The ids of the repositories the cases give a run.
const (
	webID = "0190d8f0-0000-7000-8000-00000000000a"
	apiID = "0190d8f0-0000-7000-8000-00000000000b"
)

// futureGrant is a key no build has declared: the stand-in for whatever a newer
// build adds beside a run's grants next.
const futureGrant = "code_scope"

// A RUN'S ROW CARRIES ITS GRANTS AND ITS CODE FLOOR, the launch's own: a create
// writes the repositories, the floor and the colleague's ceiling it is handed,
// and the launch floor from that same floor whatever the caller put there; a
// relaunch REPLACES all four — the previous job's repositories, and whom its
// turn worked for, are not this job's — where the turn's holds are unioned.
// And every write a build makes to the row hands back a key of a newer build's
// beside them, so a fleet mid-upgrade loses neither.
//
// Mutation: keep the previous job's repositories on a reset — the relaunch
// reads the first job's grant, red; take the launch floor from the caller —
// the create reads the caller's, red; keep the previous job's ceiling on a
// reset — the relaunch reads the first job's colleague, red.
func testARunRowCarriesItsRepositoriesAndFloor(t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns) {
	ctx := t.Context()
	first := run("t1")
	first.Repos = []sandbox.RunRepo{runRepo("web", webID)}
	first.CodeFloor = "code@1:40"
	first.CodeLaunch = "code@1:1"
	first.CodeCeiling = "designer"
	mustBeginLaunch(t, s, first)
	got := mustGet(t, s, "t1")
	if !reflect.DeepEqual(got.Repos, first.Repos) || got.CodeFloor != "code@1:40" ||
		got.CodeLaunch != "code@1:40" || got.CodeCeiling != "designer" {
		t.Fatalf("a create carries repos %+v, floor %q, launch floor %q and ceiling %q; want the run's "+
			"repositories, code@1:40, the launch floor equal to it and designer", got.Repos, got.CodeFloor,
			got.CodeLaunch, got.CodeCeiling)
	}

	// A KEY A NEWER BUILD WROTE beside the grants, put on the stored row as
	// that build would have: every write below must hand it back.
	if _, ok, err := runs.MutateSandboxRun(ctx, "t1", func(current coord.Record) (coord.SandboxRunChange, error) {
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(current.Value, &fields); err != nil {
			return coord.SandboxRunChange{}, err
		}
		fields[futureGrant] = json.RawMessage(`{"web":["src/**"]}`)
		body, err := json.Marshal(fields)
		return coord.SandboxRunChange{Put: body}, err
	}); err != nil || !ok {
		t.Fatalf("write the newer build's key: ok=%v err=%v", ok, err)
	}
	kept := func(step string) {
		t.Helper()
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		record, found, err := runs.SandboxRun(ctx, "t1")
		if err != nil || !found {
			t.Fatalf("%s: read the raw row: found=%v err=%v", step, found, err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(record.Value, &raw); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if string(raw[futureGrant]) != `{"web":["src/**"]}` {
			t.Errorf("%s dropped a key this build does not know: %s = %s", step, futureGrant, raw[futureGrant])
		}
	}

	launch := mustGet(t, s, "t1").LaunchID
	raised, err := s.RaiseCodeFloor(ctx, "t1", launch, func(string) (string, bool) { return "code@1:41", true })
	if err != nil || !raised {
		t.Fatalf("raise: %v, %v", raised, err)
	}
	kept("the raise")
	if got = mustGet(t, s, "t1"); got.CodeFloor != "code@1:41" || got.CodeLaunch != "code@1:40" {
		t.Fatalf("after a raise the floor is %q and the launch floor %q; want code@1:41 and code@1:40, "+
			"the launch floor never raised", got.CodeFloor, got.CodeLaunch)
	}

	second := run("t1")
	second.Repos = []sandbox.RunRepo{runRepo("api", apiID)}
	second.CodeFloor = "code@1:90"
	second.CodeHolds = []string{"repo-a"}
	if _, err := s.BeginLaunch(ctx, second, sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	kept("the relaunch")
	got = mustGet(t, s, "t1")
	if !reflect.DeepEqual(got.Repos, second.Repos) || got.CodeFloor != "code@1:90" ||
		got.CodeLaunch != "code@1:90" || got.CodeCeiling != "" {
		t.Fatalf("a relaunch carries repos %+v, floor %q, launch floor %q and ceiling %q; want the second "+
			"job's repositories alone, code@1:90 for both floors and no ceiling", got.Repos, got.CodeFloor,
			got.CodeLaunch, got.CodeCeiling)
	}

	// A RELAUNCH GIVEN NO REPOSITORIES has none: a job's grants are its own.
	if _, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch with no repositories: %v", err)
	}
	if got := mustGet(t, s, "t1"); len(got.Repos) != 0 || got.CodeFloor != "" || got.CodeLaunch != "" ||
		got.CodeCeiling != "" {
		t.Fatalf("a relaunch given no repositories carries %+v, floor %q, launch floor %q, ceiling %q; "+
			"want none", got.Repos, got.CodeFloor, got.CodeLaunch, got.CodeCeiling)
	}
	kept("the relaunch with no repositories")
}

// higherFloor is a raise to want, judged as Gitlet's caller judges one:
// written only when want is past the floor as the row holds it — here, by the
// sequence after the colon of a test's `code@1:<n>` tokens.
func higherFloor(want string) func(string) (string, bool) {
	seq := func(token string) int {
		_, n, _ := strings.Cut(token, ":")
		v, err := strconv.Atoi(n)
		if err != nil {
			return -1
		}
		return v
	}
	return func(current string) (string, bool) {
		if seq(want) <= seq(current) {
			return "", false
		}
		return want, true
	}
}

// A FLOOR IS RAISED ON THE LAUNCH THAT HOLDS THE ROW, AND ONLY UPWARD. The raise
// is the caller's judgement of which floor is higher, asked against the row as
// the write reads it, so a lower floor writes nothing and two raises racing
// land the higher whichever lands first; a raise naming a job the row no longer
// holds, a row whose ending is decided and a row that is gone are each no write
// and no error — the box that was shown something is over.
//
// Mutation: write without the launch check — the superseded job's raise moves
// the relaunched job's floor, red.
func testRaiseCodeFloorRaisesTheCurrentLaunchOnly(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	if ok, err := s.RaiseCodeFloor(ctx, "nobody", "launch-1", higherFloor("code@1:5")); err != nil || ok {
		t.Fatalf("a raise on no row = %v, %v, want false and no error", ok, err)
	}
	r := run("t1")
	r.Repos, r.CodeFloor = []sandbox.RunRepo{runRepo("web", webID)}, "code@1:10"
	first := mustLaunched(t, s, r)
	if _, err := s.RaiseCodeFloor(ctx, "t1", "", higherFloor("code@1:11")); err == nil {
		t.Fatal("a raise naming no launch was taken")
	}
	if ok, err := s.RaiseCodeFloor(ctx, "t1", first, higherFloor("code@1:9")); err != nil || ok {
		t.Fatalf("a raise below the floor = %v, %v, want no write", ok, err)
	}
	if ok, err := s.RaiseCodeFloor(ctx, "t1", first, higherFloor("code@1:10")); err != nil || ok {
		t.Fatalf("a raise to the floor itself = %v, %v, want no write", ok, err)
	}
	if got := mustGet(t, s, "t1").CodeFloor; got != "code@1:10" {
		t.Fatalf("the floor is %q after raises that were not higher, want code@1:10", got)
	}

	// TWO RAISES RACING LAND THE HIGHER, whichever the store takes first.
	for round := range 8 {
		low, high := 20+4*round, 22+4*round
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, to := range []int{high, low} {
			wg.Go(func() {
				<-start
				if _, err := s.RaiseCodeFloor(ctx, "t1", first, higherFloor(fmt.Sprintf("code@1:%d", to))); err != nil {
					t.Errorf("racing raise to %d: %v", to, err)
				}
			})
		}
		close(start)
		wg.Wait()
		if got, want := mustGet(t, s, "t1").CodeFloor, fmt.Sprintf("code@1:%d", high); got != want {
			t.Fatalf("round %d: two raises racing left the floor at %q, want %q, the higher", round, got, want)
		}
	}

	// A RAISE OF A JOB THE ROW NO LONGER HOLDS is no write: the relaunch's
	// floor is the relaunch's.
	relaunched := run("t1")
	relaunched.Repos, relaunched.CodeFloor = []sandbox.RunRepo{runRepo("web", webID)}, "code@1:200"
	second := mustBeginLaunch(t, s, relaunched)
	if ok, err := s.RaiseCodeFloor(ctx, "t1", first, higherFloor("code@1:900")); err != nil || ok {
		t.Fatalf("a superseded job's raise = %v, %v, want no write and no error", ok, err)
	}
	if got := mustGet(t, s, "t1").CodeFloor; got != "code@1:200" {
		t.Fatalf("a superseded job's raise left the floor at %q, want the relaunch's code@1:200", got)
	}
	if ok, err := s.RaiseCodeFloor(ctx, "t1", second, higherFloor("code@1:201")); err != nil || !ok {
		t.Fatalf("the current job's raise = %v, %v, want it written", ok, err)
	}

	// A RUN WHOSE ENDING IS DECIDED takes no raise, and says so without an
	// error: the ending's own steps are the only writes it takes.
	decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active))
	if ok, err := s.RaiseCodeFloor(ctx, "t1", second, higherFloor("code@1:300")); err != nil || ok {
		t.Fatalf("a raise on a run whose ending is decided = %v, %v, want false and no error", ok, err)
	}
	if got := mustGet(t, s, "t1").CodeFloor; got != "code@1:201" {
		t.Fatalf("a run whose ending is decided has floor %q, want code@1:201", got)
	}
}

// grantOf is a checkout of repo as the engine hands it to the store: read at
// floor, raised over the row's floor only where it is higher, bounded by
// ceiling.
func grantOf(repo sandbox.RunRepo, floor, ceiling string) sandbox.RepoGrant {
	return sandbox.RepoGrant{Repo: repo, Floor: floor, Raise: higherFloor(floor), Ceiling: ceiling}
}

// A RUN IS GRANTED ITS REPOSITORIES ONE CHECKOUT AT A TIME (ADR-0093), and
// only while the row holds the job that asked: a launch opens the row with
// none, the first checkout sets the launch floor once, a later one raises the
// code floor and never lowers it, a repository asked again on its branch and
// access is answered as held with nothing written, one asked on another
// branch, access or requester is a conflict, and a row that holds another
// job, or none, or an ending, takes nothing.
func testACheckoutIsGrantedToTheRunningJobOnly(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	web, api := runRepo("web", webID), runRepo("api", apiID)
	if _, outcome, err := s.GrantRepo(ctx, "nobody", "launch-1", grantOf(web, "code@1:5", "")); err != nil ||
		outcome != sandbox.GrantNotRunning {
		t.Fatalf("a checkout on no row = %q, %v, want not_running and no error", outcome, err)
	}
	first := mustLaunched(t, s, run("t1"))
	if got := mustGet(t, s, "t1"); len(got.Repos) != 0 || got.CodeFloor != "" || got.CodeLaunch != "" {
		t.Fatalf("a launch opened the row with %+v at floor %q, launch floor %q; want no grant before a "+
			"checkout", got.Repos, got.CodeFloor, got.CodeLaunch)
	}
	if _, _, err := s.GrantRepo(ctx, "t1", first, sandbox.RepoGrant{Repo: web}); err == nil {
		t.Fatal("a checkout read at no position was taken")
	}
	if _, outcome, err := s.GrantRepo(ctx, "t1", "launch-other", grantOf(web, "code@1:5", "")); err != nil ||
		outcome != sandbox.GrantNotRunning {
		t.Fatalf("a checkout for another job = %q, %v, want not_running", outcome, err)
	}

	granted, outcome, err := s.GrantRepo(ctx, "t1", first, grantOf(web, "code@1:10", ""))
	if err != nil || outcome != sandbox.GrantAdded || granted.RepoID != webID {
		t.Fatalf("the first checkout = %+v, %q, %v, want web added", granted, outcome, err)
	}
	got := mustGet(t, s, "t1")
	if len(got.Repos) != 1 || got.CodeFloor != "code@1:10" || got.CodeLaunch != "code@1:10" {
		t.Fatalf("after the first checkout the row holds %+v at floor %q, launch floor %q; want web at "+
			"code@1:10 for both", got.Repos, got.CodeFloor, got.CodeLaunch)
	}

	// THE SAME REPOSITORY ON ITS BRANCH AND ACCESS IS HELD: nothing written.
	if _, outcome, err := s.GrantRepo(ctx, "t1", first, grantOf(web, "code@1:30", "")); err != nil ||
		outcome != sandbox.GrantHeld {
		t.Fatalf("web checked out again = %q, %v, want held", outcome, err)
	}
	if got := mustGet(t, s, "t1").CodeFloor; got != "code@1:10" {
		t.Fatalf("a held checkout moved the floor to %q, want code@1:10", got)
	}
	// ON ANOTHER BRANCH, OR WITH ANOTHER ACCESS, IT CONFLICTS.
	other := web
	other.Branch = "seats/swe/other"
	if held, outcome, err := s.GrantRepo(ctx, "t1", first, grantOf(other, "code@1:30", "")); err != nil ||
		outcome != sandbox.GrantConflict || held.Branch != web.Branch {
		t.Fatalf("web on another branch = %+v, %q, %v, want a conflict answering the branch held", held,
			outcome, err)
	}
	read := web
	read.Access = sandbox.RepoRead
	if _, outcome, err := s.GrantRepo(ctx, "t1", first, grantOf(read, "code@1:30", "")); err != nil ||
		outcome != sandbox.GrantConflict {
		t.Fatalf("web to read = %q, %v, want a conflict", outcome, err)
	}
	// FOR ANOTHER REQUESTER, IT CONFLICTS: one run works for one.
	if _, outcome, err := s.GrantRepo(ctx, "t1", first, grantOf(api, "code@1:30", "ceo")); err != nil ||
		outcome != sandbox.GrantConflict {
		t.Fatalf("api for another requester = %q, %v, want a conflict", outcome, err)
	}

	// A LATER CHECKOUT RAISES THE CODE FLOOR, never the launch floor; a
	// lower one leaves the floor where it is.
	if _, outcome, err := s.GrantRepo(ctx, "t1", first, grantOf(api, "code@1:30", "")); err != nil ||
		outcome != sandbox.GrantAdded {
		t.Fatalf("api checked out = %q, %v, want added", outcome, err)
	}
	got = mustGet(t, s, "t1")
	if len(got.Repos) != 2 || got.CodeFloor != "code@1:30" || got.CodeLaunch != "code@1:10" {
		t.Fatalf("after two checkouts the row holds %d repositories at floor %q, launch floor %q; want 2 at "+
			"code@1:30, launched at code@1:10", len(got.Repos), got.CodeFloor, got.CodeLaunch)
	}

	// THE ROW HOLDS AT MOST MaxRunRepos.
	for k := len(got.Repos); k < sandbox.MaxRunRepos; k++ {
		more := runRepo(fmt.Sprintf("repo-%d", k), fmt.Sprintf("0190d8f0-0000-7000-8000-0000000001%02d", k))
		if _, outcome, err := s.GrantRepo(ctx, "t1", first, grantOf(more, "code@1:31", "")); err != nil ||
			outcome != sandbox.GrantAdded {
			t.Fatalf("checkout %d = %q, %v, want added", k, outcome, err)
		}
	}
	past := runRepo("one-too-many", "0190d8f0-0000-7000-8000-000000000199")
	if _, outcome, err := s.GrantRepo(ctx, "t1", first, grantOf(past, "code@1:31", "")); err != nil ||
		outcome != sandbox.GrantFull {
		t.Fatalf("a checkout past %d = %q, %v, want full", sandbox.MaxRunRepos, outcome, err)
	}

	// A RUN WHOSE ENDING IS DECIDED takes no checkout, and says so without an
	// error.
	decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active))
	if _, outcome, err := s.GrantRepo(ctx, "t1", first, grantOf(past, "code@1:40", "")); err != nil ||
		outcome != sandbox.GrantNotRunning {
		t.Fatalf("a checkout on a run whose ending is decided = %q, %v, want not_running", outcome, err)
	}
}

// A RUN WORKS FOR ONE REQUESTER (ADR-0109): whom its turn works for goes on its
// row with its first checkout (PendingRun.CodeWorkingFor), and the box's pushes
// and the change its collection opens carry it, so a later checkout for anybody
// else (another credential, a seat, nobody) is a conflict that writes nothing;
// one for the same credential holding fewer apps narrows the row's to those,
// and one holding more never widens it. A launch that carries a round on
// carries whom its job worked for with its grants, one given no repositories
// nobody. NOBODY IS ONE REQUESTER TOO: a run whose grants were taken for nobody
// (or carried on with no carrier) is never used for somebody, so a checkout for
// somebody under the row's own ceiling is a conflict. Mutations: GrantRepo
// admitting a second requester; the narrowing left out; a reset keeping the
// previous job's; a nobody-row adopting whom a later checkout is for.
func testARunWorksForOneRequester(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	web, api := runRepo("web", webID), runRepo("api", apiID)
	ci := func(apps ...iam.Grant) *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci", Apps: apps}
	}
	madeFor := func(repo sandbox.RunRepo, floor string, w *types.WorkingFor) sandbox.RepoGrant {
		g := grantOf(repo, floor, "")
		g.WorkingFor = w
		return g
	}
	first := mustLaunched(t, s, run("t1"))
	all := ci(iam.GrantWorklet, iam.GrantDoclet, iam.GrantGitlet)
	if _, outcome, err := s.GrantRepo(ctx, "t1", first, madeFor(web, "code@1:10", all)); err != nil ||
		outcome != sandbox.GrantAdded {
		t.Fatalf("the first checkout for token:ci = %q, %v, want added", outcome, err)
	}
	if got := mustGet(t, s, "t1").CodeWorkingFor; !reflect.DeepEqual(got, all) {
		t.Fatalf("the first checkout left the run working for %+v, want %+v", got, all)
	}

	// ANYBODY ELSE CONFLICTS, and the row is as it was.
	for _, other := range []struct {
		name string
		w    *types.WorkingFor
	}{
		{"another credential", &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:deploy",
			Apps: []iam.Grant{iam.GrantGitlet}}},
		{"a seat", &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ceo"}},
		{"nobody", nil},
	} {
		if _, outcome, err := s.GrantRepo(ctx, "t1", first, madeFor(api, "code@1:20", other.w)); err != nil ||
			outcome != sandbox.GrantConflict {
			t.Errorf("a checkout of api for %s = %q, %v, want a conflict", other.name, outcome, err)
		}
	}
	if got := mustGet(t, s, "t1"); len(got.Repos) != 1 || !reflect.DeepEqual(got.CodeWorkingFor, all) {
		t.Fatalf("after the conflicts the row holds %d repositories working for %+v, want web alone, for %+v",
			len(got.Repos), got.CodeWorkingFor, all)
	}

	// THE SAME CREDENTIAL HOLDING FEWER APPS NARROWS IT, a checkout held
	// again among them; HOLDING MORE NEVER WIDENS IT.
	narrow := ci(iam.GrantWorklet, iam.GrantGitlet)
	if _, outcome, err := s.GrantRepo(ctx, "t1", first, madeFor(web, "code@1:20", narrow)); err != nil ||
		outcome != sandbox.GrantHeld {
		t.Fatalf("web checked out again for token:ci holding fewer apps = %q, %v, want held", outcome, err)
	}
	if got := mustGet(t, s, "t1").CodeWorkingFor; !reflect.DeepEqual(got, narrow) {
		t.Fatalf("a checkout for token:ci holding fewer apps left the run working for %+v, want %+v", got, narrow)
	}
	wider := ci(iam.GrantWorklet, iam.GrantDoclet, iam.GrantGitlet, iam.GrantAtlas)
	if _, outcome, err := s.GrantRepo(ctx, "t1", first, madeFor(api, "code@1:20", wider)); err != nil ||
		outcome != sandbox.GrantAdded {
		t.Fatalf("api checked out for token:ci holding more apps = %q, %v, want added", outcome, err)
	}
	if got := mustGet(t, s, "t1").CodeWorkingFor; !reflect.DeepEqual(got, narrow) {
		t.Fatalf("a checkout for token:ci holding more apps left the run working for %+v, want %+v, never "+
			"widened", got, narrow)
	}

	// A ROUND CARRIED ON carries whom its job worked for with its grants;
	// a relaunch given no repositories carries nobody.
	carried := run("t1")
	carried.Repos, carried.CodeFloor, carried.CodeWorkingFor = []sandbox.RunRepo{web}, "code@1:30", narrow
	mustBeginLaunch(t, s, carried)
	if got := mustGet(t, s, "t1").CodeWorkingFor; !reflect.DeepEqual(got, narrow) {
		t.Fatalf("a round carried on works for %+v, want %+v", got, narrow)
	}
	mustBeginLaunch(t, s, run("t1"))
	if got := mustGet(t, s, "t1").CodeWorkingFor; got != nil {
		t.Fatalf("a relaunch given no repositories works for %+v, want nobody", got)
	}

	// A SEAT IS ONE REQUESTER TOO: nobody after it conflicts.
	second := mustLaunched(t, s, run("t2"))
	ceo := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ceo"}
	if _, outcome, err := s.GrantRepo(ctx, "t2", second, madeFor(web, "code@1:40", ceo)); err != nil ||
		outcome != sandbox.GrantAdded {
		t.Fatalf("a checkout for ceo = %q, %v, want added", outcome, err)
	}
	if _, outcome, err := s.GrantRepo(ctx, "t2", second, madeFor(api, "code@1:40", nil)); err != nil ||
		outcome != sandbox.GrantConflict {
		t.Fatalf("a checkout for nobody on a run working for ceo = %q, %v, want a conflict", outcome, err)
	}
	if got := mustGet(t, s, "t2").CodeWorkingFor; !reflect.DeepEqual(got, ceo) {
		t.Fatalf("the run checked out for ceo works for %+v, want %+v", got, ceo)
	}

	// A RUN GRANTED FOR NOBODY IS NOT USED FOR SOMEBODY, whichever build
	// wrote its row.
	ownSeats := mustLaunched(t, s, run("t3"))
	if _, outcome, err := s.GrantRepo(ctx, "t3", ownSeats, madeFor(web, "code@1:50", nil)); err != nil ||
		outcome != sandbox.GrantAdded {
		t.Fatalf("a checkout for nobody = %q, %v, want added", outcome, err)
	}
	older := run("t4")
	older.Repos, older.CodeFloor, older.CodeCeiling = []sandbox.RunRepo{web}, "code@1:50", "designer"
	carriedOn := mustBeginLaunch(t, s, older)
	person := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"}
	designer := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "designer"}
	for _, granted := range []struct {
		name, turn, launch, ceiling string
		// somebody is whom a later checkout under ceiling is made for.
		somebody []*types.WorkingFor
	}{
		{"by its first checkout, made for nobody", "t3", ownSeats, "", []*types.WorkingFor{person, all}},
		{"by a build before the carrier, under a colleague's ceiling", "t4", carriedOn, "designer",
			[]*types.WorkingFor{designer}},
	} {
		for _, w := range granted.somebody {
			for _, repo := range []sandbox.RunRepo{web, api} {
				g := grantOf(repo, "code@1:60", granted.ceiling)
				g.WorkingFor = w
				if _, outcome, err := s.GrantRepo(ctx, granted.turn, granted.launch, g); err != nil ||
					outcome != sandbox.GrantConflict {
					t.Errorf("a run granted for nobody %s checked %s out for %+v: %q, %v, want a conflict",
						granted.name, repo.Repo, w, outcome, err)
				}
			}
		}
		if got := mustGet(t, s, granted.turn); !reflect.DeepEqual(got.Repos, []sandbox.RunRepo{web}) ||
			got.CodeWorkingFor != nil || got.CodeCeiling != granted.ceiling || got.CodeFloor != "code@1:50" {
			t.Errorf("the run granted for nobody %s holds %+v at floor %q under ceiling %q, working for %+v; "+
				"want web alone at code@1:50 under %q, for nobody, as it was", granted.name, got.Repos,
				got.CodeFloor, got.CodeCeiling, got.CodeWorkingFor, granted.ceiling)
		}
	}
}
