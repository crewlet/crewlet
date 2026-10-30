package checkrun_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/checkrun"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// claim claims key for runner at at, in placement, naming box where it is
// set — as a runner's two records do.
func (f *fleet) claim(key code.CheckKey, runner string, placement code.CheckPlacement, box string, at time.Time) {
	f.t.Helper()
	records := []string{""}
	if box != "" {
		records = append(records, box)
	}
	for _, named := range records {
		res, err := f.d.Served().PublishChecks(f.t.Context(), code.ChecksRequest{OpID: newOpID("code_check_claim"),
			Started: &code.CheckStarted{CheckKey: key, Runner: runner, Placement: placement, Box: named,
				StartedAt: at}})
		if err != nil || res.Refusal != "" {
			f.t.Fatalf("claim %s for %s naming box %q: (%+v, %v)", key.Name, runner, named, res, err)
		}
	}
}

// sandboxSpec is a remote box's spec, as a lost runner made one.
func sandboxSpec() sandbox.Spec { return sandbox.Spec{Placement: sandbox.E2B} }

// sweeper is a home's sweep over the case's data node, its git order o and
// its clock now.
func (f *fleet) sweeper(o checkrun.Order, now func() time.Time) *checkrun.Sweeper {
	f.t.Helper()
	s, err := checkrun.NewSweeper(checkrun.SweeperOptions{Estate: estate{f.d}, Order: o,
		Boxes: func() (checkrun.Boxes, bool) { return f.manager, true }, Poll: checkrun.CheckPoll, Now: now})
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// THE HOME SWEEPS ONLY A RUNNER THAT IS GONE: a running attempt whose runner
// is in the repository's git order and whose claim is younger than its check's
// timeout, the setup's grace and the slack is left running; one whose runner
// is still in the order but whose claim has outlived all of that — a node that
// restarted mid-run, holding nothing — is ended `errored runner_lost`, as is
// one whose runner left the order, its remote box killed through the
// catalogue's e2b backend.
//
// Mutations: sweep every running attempt — the live runner's is ended, red;
// leave the remote box to its TTL — no kill, red.
func TestTheHomeSweepsOnlyARunnerThatIsGone(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	now := time.Now().UTC()
	live := f.queue(oidOf("1"), def("unit", 10*time.Minute))
	f.claim(live, "git-a", code.PlacementContainer, "", now)
	restarted := f.queue(oidOf("2"), def("unit", time.Minute))
	f.claim(restarted, "git-a", code.PlacementContainer, "", now.Add(-time.Minute-code.CheckBoxGrace-
		checkrun.SweepPolls*checkrun.CheckPoll-time.Second))
	left := f.queue(oidOf("3"), def("unit", 10*time.Minute))
	remote, err := f.e2b.Create(t.Context(), sandboxSpec())
	if err != nil {
		t.Fatal(err)
	}
	f.claim(left, "git-gone", code.PlacementE2B, remote.ID(), now)

	s := f.sweeper(&order{nodes: []string{"git-home", "git-a"}}, func() time.Time { return now })
	repos, _, err := s.Due(t.Context(), statelogZero, "")
	if err != nil || !slices.Contains(repos, f.repo) {
		t.Fatalf("the home's listing = (%v, %v), want web", repos, err)
	}
	if err := s.Work(t.Context(), f.repo, statelogZero); err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	if row, _ := f.row(live); row.State != code.CheckStateRunning {
		t.Errorf("a live runner's attempt was swept: %+v", row)
	}
	for name, key := range map[string]code.CheckKey{"restarted": restarted, "left": left} {
		row, _ := f.row(key)
		if row.State != code.CheckStateErrored || row.Reason != code.CheckReasonRunnerLost {
			t.Errorf("the %s runner's attempt = %+v, want errored runner_lost", name, row)
		}
	}
	if row, _ := f.row(left); !strings.Contains(row.Summary, "git-gone") {
		t.Errorf("the left runner's ending says %q, want the runner named", row.Summary)
	}
	if killed := f.e2b.KilledIDs(); !slices.Equal(killed, []string{remote.ID()}) {
		t.Errorf("the boxes killed = %v, want the gone runner's remote box %s alone", killed, remote.ID())
	}
	// SWEPT AGAIN, NOTHING MORE: the live attempt stays, and nothing is
	// killed twice.
	if err := s.Work(t.Context(), f.repo, statelogZero); err != nil {
		t.Fatalf("the second sweep: %v", err)
	}
	if killed := f.e2b.KilledIDs(); len(killed) != 1 {
		t.Errorf("a second sweep killed %v", killed)
	}
}
