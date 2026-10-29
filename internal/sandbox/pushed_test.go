package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
)

// pushSpy is a [RunPushes] answering what a case scripts and recording every
// run it was asked about.
type pushSpy struct {
	mu    sync.Mutex
	refs  []PushedRef
	more  int
	err   error
	asked []PendingRun
	// held, when set, holds every read until the context it runs under ends,
	// and is closed by the first read to arrive.
	held chan struct{}
}

func (s *pushSpy) Pushed(ctx context.Context, run PendingRun) ([]PushedRef, int, error) {
	s.mu.Lock()
	s.asked = append(s.asked, run)
	refs, more, err, held := s.refs, s.more, s.err, s.held
	s.held = nil
	s.mu.Unlock()
	if held != nil {
		// THE READ OUTLASTS ITS CALLER: answered only once the context the
		// collection runs under is ended.
		close(held)
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}
	return refs, more, err
}

// answer scripts what every later read answers.
func (s *pushSpy) answer(refs []PushedRef, more int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs, s.more, s.err = refs, more, err
}

// runs is every run the spy was asked about, in order.
func (s *pushSpy) runs() []PendingRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

// Object ids the cases' pushes move refs between.
var (
	oidA = strings.Repeat("a", 40)
	oidB = strings.Repeat("b", 40)
	oidC = strings.Repeat("c", 40)
	oidD = strings.Repeat("d", 40)
)

// withNative gives a rig run web and api to push to and docs to read, at a
// launch floor on the code log.
func withNative(r *PendingRun) {
	r.Repos = []RunRepo{
		{Repo: "web", RepoID: webRepoID, Branch: "seats/swe/ENG-1", Access: RepoWrite},
		{Repo: "api", RepoID: apiRepoID, Branch: "seats/swe/ENG-1", Access: RepoWrite, Paths: []string{"src"}},
		{Repo: "docs", RepoID: "0190d8f0-0000-7000-8000-00000000000c", Branch: "seats/swe/ENG-1",
			Access: RepoRead},
	}
	r.CodeFloor = "CREWLET_CODE_LOG@1:40"
}

// completeRun finishes the rig's run t1 with result and collects it, as the
// completion poll's signal does.
func completeRun(t *testing.T, rig *coordRig, result Result) error {
	t.Helper()
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(result)
	payload, ev := rig.completion("t1")
	return rig.coordinator.OnCompleted(t.Context(), payload, ev)
}

// A COLLECTED RUN IS TOLD WHAT IT PUSHED, by the push record rather than its
// own report: the refs the engine read under its launch — created, moved and
// deleted, each as it ended, with how many pushes moved it — follow the run's
// own `Delivered:` line in the resumed executor's text, and ride the resume
// and the run's own phase record whole. The record is read once a collection,
// for the run as its row stands.
//
// Mutations: leave the pushed lines out of resumeText — red; skip the read
// in collect — red; leave the refs off the run's phase record — red.
func TestACollectedRunIsToldWhatItPushed(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	launched := rig.launchWith("t1", withNative)
	rig.pushes.answer([]PushedRef{
		{Repo: "web", Ref: "refs/heads/seats/swe/ENG-1", Old: oidA, New: oidB, Pushes: 3},
		{Repo: "web", Ref: "refs/heads/seats/swe/spike", Old: gitref.ZeroOID, New: oidC, Pushes: 1},
		{Repo: "api", Ref: "refs/heads/seats/swe/old", Old: oidD, New: gitref.ZeroOID, Pushes: 1},
	}, 0, nil)

	if err := completeRun(t, rig, Result{Success: true, Text: "fixed the flake",
		DeliveredRefs: []string{"https://github.com/acme/site/pull/7"}}); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want once", len(calls))
	}
	want := "Delivered: https://github.com/acme/site/pull/7\n" +
		"Pushed to this company's code host (its push record):\n" +
		"- web refs/heads/seats/swe/ENG-1 aaaaaaaaaaaa → bbbbbbbbbbbb (3 pushes)\n" +
		"- web refs/heads/seats/swe/spike created at cccccccccccc\n" +
		"- api refs/heads/seats/swe/old deleted (it was dddddddddddd)\n"
	if !strings.Contains(calls[0].Answer, want) {
		t.Errorf("the resumed executor reads:\n%s\nwant it to carry:\n%s", calls[0].Answer, want)
	}
	if len(calls[0].Pushed) != 3 || calls[0].PushedElided != 0 || calls[0].PushedUnread != "" {
		t.Errorf("the resume carries %+v, %d more, unread %q; want the three refs", calls[0].Pushed,
			calls[0].PushedElided, calls[0].PushedUnread)
	}

	asked := rig.pushes.runs()
	if len(asked) != 1 || asked[0].LaunchID != launched.LaunchID || len(asked[0].Repos) != 3 ||
		asked[0].CodeLaunch != "CREWLET_CODE_LOG@1:40" {
		t.Fatalf("the record was read for %+v, want once for the collected run as its row stands", asked)
	}

	recs := rig.phases()
	if len(recs) != 1 {
		t.Fatalf("published %d phase records, want the run's own", len(recs))
	}
	wantRecs := []types.PushedRefRecord{
		{Repo: "web", Ref: "refs/heads/seats/swe/ENG-1", Old: oidA, New: oidB, Pushes: 3},
		{Repo: "web", Ref: "refs/heads/seats/swe/spike", Old: gitref.ZeroOID, New: oidC, Pushes: 1},
		{Repo: "api", Ref: "refs/heads/seats/swe/old", Old: oidD, New: gitref.ZeroOID, Pushes: 1},
	}
	if !slices.Equal(recs[0].PushedRefs, wantRecs) || recs[0].PushedRefsElided != 0 ||
		recs[0].PushedRefsUnread != "" {
		t.Errorf("the run's record lists %+v, want the whole ids of %+v", recs[0].PushedRefs, wantRecs)
	}
}

// A PUSH RECORD THAT CANNOT BE READ IS SAID, NOT HIDDEN: the run is still
// collected and resumed — a collection held until the code host answers would
// hold a finished run's seat for as long as the host is down — and the
// executor is told its pushes are UNKNOWN, why, and what to read instead, never
// a silence it would read as "nothing pushed". The run's record says so too,
// and the log names the run.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
//
// Mutations: omit the unknown sentence — red; fail the collection on the read's
// error — no resume, red.
func TestAnUnreadPushRecordIsSaidNotHidden(t *testing.T) {
	logs := captureLogs(t)
	rig := newCoordRig(t)
	rig.launchWith("t1", withNative)
	rig.pushes.answer(nil, 0, errors.New("code: this node is not serving the code host"))

	if err := completeRun(t, rig, Result{Success: true, Text: "done"}); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want the collection resumed whatever the record said", len(calls))
	}
	want := "Pushed to this company's code host: unknown — its push record could not be read (code: this " +
		"node is not serving the code host); read the branches it worked on (web: seats/swe/ENG-1; api: " +
		"seats/swe/ENG-1) with code_resolve before you report them."
	if !strings.Contains(calls[0].Answer, want) {
		t.Errorf("the resumed executor reads:\n%s\nwant:\n%s", calls[0].Answer, want)
	}
	if calls[0].PushedUnread == "" || len(calls[0].Pushed) != 0 {
		t.Errorf("the resume carries %+v, unread %q; want the reason and no refs", calls[0].Pushed,
			calls[0].PushedUnread)
	}
	if recs := rig.phases(); len(recs) != 1 || !strings.Contains(recs[0].PushedRefsUnread, "not serving") {
		t.Errorf("the run's record = %+v, want it to say why its pushes are unknown", recs)
	}
	if line := logs.String(); !strings.Contains(line, "sandbox_pushes_unread") || !strings.Contains(line, "t1") {
		t.Errorf("the log does not say the record was unread:\n%s", line)
	}
}

// A READ THIS NODE ITSELF CANCELLED IS THE COLLECTION INTERRUPTED: a drain or
// a moving seat says nothing about the record, so the claim is handed back
// uncounted and the completion collects again — rather than resuming the run
// with its pushes "unknown" and publishing its record, once, saying so.
//
// Mutation: say the cancelled read as unread — the run resumes, red.
func TestAPushReadThisNodeCancelledHandsTheCollectionBack(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launchWith("t1", withNative)
	reading := make(chan struct{})
	rig.pushes.mu.Lock()
	rig.pushes.held = reading
	rig.pushes.mu.Unlock()
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done"})
	payload, ev := rig.completion("t1")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- rig.coordinator.OnCompleted(ctx, payload, ev) }()
	// The collection reaches the read, which waits on its context.
	select {
	case <-reading:
	case err := <-done:
		t.Fatalf("the collection ended before it read the record: %v", err)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("an interrupted collection was acked")
	}
	if calls := rig.resumer.calls(); len(calls) != 0 {
		t.Fatalf("an interrupted collection resumed the run: %+v", calls)
	}
	if got := rig.get("t1"); got.Status != StatusRunning || got.Launch.CollectFailures != 0 {
		t.Fatalf("the run is %s with %d collection failures, want it running again and uncounted",
			got.Status, got.Launch.CollectFailures)
	}
	if recs := rig.phases(); len(recs) != 0 {
		t.Fatalf("an interrupted collection published %d records", len(recs))
	}
}

// A RUN THAT PUSHED NOTHING SAYS SO — and only a run that could have: one given
// a repository to push to and whose record names no push is told "nothing";
// one given its repositories only to read is told it could push none, with no
// read of a record that cannot hold any; one given no repository on the code
// host is told nothing of it.
//
// Mutations: drop the "nothing" sentence — red; read the record of a run given
// only repositories to read — red.
func TestARunThatPushedNothingSaysSo(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launchWith("t1", withNative)
	if err := completeRun(t, rig, Result{Success: true, Text: "looked around"}); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if calls := rig.resumer.calls(); len(calls) != 1 || !strings.Contains(calls[0].Answer,
		"Pushed to this company's code host: nothing — no push of this run was recorded.") {
		t.Errorf("a run that pushed nothing is told %+v", calls)
	}

	read := PendingRun{Repos: []RunRepo{{Repo: "docs", Access: RepoRead}}}
	if text := resumeText(read, Result{Success: true}); !strings.Contains(text,
		"Pushed to this company's code host: nothing — it was given its repositories there to read, not to "+
			"push to.") {
		t.Errorf("a run given repositories to read is told:\n%s", text)
	}
	reader := newCoordRig(t)
	reader.launchWith("t1", func(r *PendingRun) {
		withNative(r)
		r.Repos = r.Repos[2:]
	})
	if err := completeRun(t, reader, Result{Success: true}); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if asked := reader.pushes.runs(); len(asked) != 0 {
		t.Errorf("the record of a run given repositories only to read was read %d times", len(asked))
	}

	if text := resumeText(PendingRun{}, Result{Success: true}); strings.Contains(text, "Pushed to") {
		t.Errorf("a run given no repository on the code host is told of its pushes:\n%s", text)
	}
}

// THE PUSHED REFS SHARE THE REFS' ONE BOUND: what the run's own lines leave
// of MaxDeliveredRefBytes is what the push record's are listed in, counted by
// the lines they render to; the list stops at the first that does not fit,
// and the rest are counted with what the read had no room to hold and said in
// the executor's text. A ref's names are redacted before they are measured.
//
// Mutations: give the pushed refs a budget of their own — red; skip a long
// ref for a shorter one after it — red; drop the reader's count — red.
func TestPushedRefsShareTheRefsBound(t *testing.T) {
	t.Parallel()
	ref := func(i int) PushedRef {
		return PushedRef{Repo: "web", Ref: fmt.Sprintf("refs/heads/seats/swe/%04d", i), Old: oidA, New: oidB,
			Pushes: 1}
	}
	var pushed []PushedRef
	for i := range 400 {
		pushed = append(pushed, ref(i))
	}
	// A long ref among them, past what the first hundred leave: the list
	// stops there, whatever fits after it.
	long := PushedRef{Repo: "web", Ref: "refs/heads/" + strings.Repeat("x", 240), Old: oidA, New: oidB, Pushes: 1}
	pushed = append(pushed[:100], append([]PushedRef{long}, pushed[100:]...)...)
	left := 100*len(ref(0).line()) + len(long.line())/2
	delivered := []string{"https://h/" + strings.Repeat("d", MaxDeliveredRefBytes-left-len("https://h/"))}

	got := (&Coordinator{}).fitResult(t.Context(), PendingRun{}, Result{
		Success: true, DeliveredRefs: delivered, Pushed: pushed, PushedElided: 7,
	})
	size := len(delivered[0])
	for _, p := range got.Pushed {
		size += len(p.line())
	}
	if size > MaxDeliveredRefBytes {
		t.Errorf("the refs take %d bytes, past the %d they share", size, MaxDeliveredRefBytes)
	}
	if len(got.Pushed) != 100 || got.Pushed[99] != ref(99) {
		t.Fatalf("listed %d pushed refs, want the 100 before the one that does not fit", len(got.Pushed))
	}
	if got.PushedElided != 7+len(pushed)-100 {
		t.Errorf("counted %d more, want the reader's 7 and the %d not listed", got.PushedElided, len(pushed)-100)
	}
	run := PendingRun{Repos: []RunRepo{{Repo: "web", Access: RepoWrite}}}
	if text := resumeText(run, got); !strings.Contains(text,
		fmt.Sprintf("- and %d more ref(s) its push record names, not listed here", got.PushedElided)) {
		t.Errorf("the resumed executor is not told the list is short:\n%.400s", text)
	}

	// WITH NO OWN LINES, THE PUSH RECORD HAS THE WHOLE BOUND, and what it
	// lists is redacted.
	secret := "ghp_" + strings.Repeat("A", 36)
	alone := (&Coordinator{}).fitResult(t.Context(), PendingRun{}, Result{Pushed: []PushedRef{
		{Repo: "web", Ref: "refs/heads/" + secret, Old: gitref.ZeroOID, New: oidC, Pushes: 1},
	}})
	if len(alone.Pushed) != 1 || strings.Contains(alone.Pushed[0].Ref, secret) || alone.PushedElided != 0 {
		t.Errorf("a ref naming a credential is listed as %+v", alone.Pushed)
	}
}

// EVERY PUSHED LINE IS AT LEAST ITS FLOOR, at its shortest — a one-letter
// repository and the shortest ref the code host takes — in every shape a ref
// can end in, so no list of MaxDeliveredRefBytes holds more than
// MaxPushedRefs and a reader that holds that many holds every candidate.
func TestEveryPushedLineIsAtLeastItsFloor(t *testing.T) {
	t.Parallel()
	for _, p := range []PushedRef{
		{Old: gitref.ZeroOID, New: oidA},
		{Old: oidA, New: gitref.ZeroOID},
		{Old: gitref.ZeroOID, New: gitref.ZeroOID},
		{Old: oidA, New: oidA},
		{Old: oidA, New: oidB},
	} {
		p.Repo, p.Ref, p.Pushes = "a", "refs/x/y", 1
		if !gitref.ValidRef(p.Ref) {
			t.Fatalf("%q is no ref the code host takes, so it is not the shortest one", p.Ref)
		}
		if line := p.line(); len(line) < minPushedLineBytes {
			t.Errorf("%q is %d bytes, under the %d MaxPushedRefs is derived from", line, len(line),
				minPushedLineBytes)
		}
	}
}

// A RECLAIMED RUN WITH REPOSITORIES IS TOLD TO CLONE ITS BRANCHES AGAIN: its
// box is gone, so the answer's text gives the call that gives the fresh box
// each repository on the branch the run worked on — the one its work was
// pushed to — with a repository it was given to read kept to reading and its
// cone kept; an ask's branch that is none of those keeps a clause of its own.
// A run whose box is still up reuses its checkout, as before.
//
// Mutation: leave the repositories out of the call — red.
func TestAReclaimedRunWithReposIsToldToCloneItsBranchAgain(t *testing.T) {
	t.Parallel()
	run := PendingRun{Question: "which suite?"}
	withNative(&run)
	text := answerText(run, "the unit suite", "")
	call := `[{"repo":"web","branch":"seats/swe/ENG-1"},{"repo":"api","branch":"seats/swe/ENG-1",` +
		`"paths":["src"]},{"repo":"docs","branch":"seats/swe/ENG-1","access":"read"}]`
	if !strings.Contains(text, "call run_sandbox again with `repos` set to "+call) {
		t.Errorf("a reclaimed run's answer reads:\n%s\nwant it to give the call %s", text, call)
	}
	var value []map[string]any
	if err := json.Unmarshal([]byte(call), &value); err != nil || len(value) != 3 {
		t.Fatalf("the call is not the tool's argument: %v", err)
	}
	if strings.Contains(text, "check out the existing branch") {
		t.Errorf("a run whose ask named no branch elsewhere is told to check one out:\n%s", text)
	}

	run.Branch = "wip/suite"
	text = answerText(run, "the unit suite", "")
	if !strings.Contains(text, "also check out the existing branch `wip/suite` it pushed its work-in-progress to") {
		t.Errorf("an ask's branch on another host is not named:\n%s", text)
	}
	run.Branch = "seats/swe/ENG-1"
	text = answerText(run, "the unit suite", "")
	if strings.Contains(text, "check out the existing branch") {
		t.Errorf("an ask's branch the clone already holds is named again:\n%s", text)
	}

	run.SandboxID = "box-1"
	text = answerText(run, "the unit suite", "")
	if strings.Contains(text, "`repos`") || !strings.Contains(text, "it reuses the same checkout") {
		t.Errorf("a run whose box is up is told to clone again:\n%s", text)
	}
}

// A RUN IS TOLD WHICH OF ITS PUSHES ARE REPORTED FOR IT: the repositories it
// was given to push to on the code host reach its runner, which names them in
// the report instruction; one given to read is not among them, and a run given
// none is told of none.
//
// Mutation: hand the runner every repository the run was given — docs among
// them, red.
func TestARunIsToldWhichOfItsPushesAreReported(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	read := granted("docs", "0190d8f0-0000-7000-8000-00000000000c")
	read.Access = RepoRead
	req := withRepos(t, launchReq("t1"), granted("web", webRepoID), read, granted("api", apiRepoID))
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	started := rig.runner.Started()
	if len(started) != 1 || !slices.Equal(started[0].ReportedRepos, []string{"web", "api"}) {
		t.Fatalf("the runner was handed %+v, want web and api reported", started)
	}

	plain := newWaiterRig(t)
	if _, err := plain.launchVia(t.Context(), plain.manager, launchReq("t2")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if started := plain.runner.Started(); len(started) != 1 || started[0].ReportedRepos != nil {
		t.Fatalf("a run given no repository on the code host reports %+v", started)
	}
}
