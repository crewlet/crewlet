package code_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/google/uuid"
)

// A REPOSITORY'S CHECKS THROUGH THEIR WRITER: every write handed to the checks
// writer beside the committer, decided in one snapshot on a real embedded
// broker as a record on the repository's checks subject, applied by the
// shipped applier and answered from the attempt's row.

// appendCounter is a code log's write authority that counts the records the
// publishes through it appended — a refusal before the append is none.
type appendCounter struct {
	inner    code.Publisher
	appended atomic.Int64
}

func (c *appendCounter) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	res, err := c.inner.Publish(ctx, req)
	if err == nil && !res.Position.IsZero() && !res.Collapsed {
		c.appended.Add(1)
	}
	return res, err
}

// announced is a node's event stream as the checks writer announces on it:
// every event it was handed, in order.
type announced struct {
	mu     sync.Mutex
	events []*events.Event
}

func (a *announced) Publish(_ context.Context, _ string, ev *events.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}

func (a *announced) all() []*events.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.events)
}

// of is every event of one wire type the stream was handed, in order: a
// committer announces merges and opened changes on one stream.
func (a *announced) of(eventType string) []*events.Event {
	return slices.DeleteFunc(a.all(), func(ev *events.Event) bool { return ev.Type != eventType })
}

// waitAnnouncedOf waits for the stream to hold n events of eventType.
func waitAnnouncedOf(t *testing.T, stream *announced, eventType string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(stream.of(eventType)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the stream holds %d %s announcements, want %d", len(stream.of(eventType)), eventType, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// checksWriterOn is a checks writer on n over a counter of what it appends,
// announcing on an event stream of its own, with a recorder of its own.
func checksWriterOn(t testing.TB, n *node) (*code.ChecksWriter, *appendCounter, *announced, *metrics.Recorder) {
	t.Helper()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	counter, stream := &appendCounter{inner: n.pub}, &announced{}
	w, err := code.NewChecksWriter(code.ChecksWriterOptions{
		Publisher: counter, DB: n.db.Reader(), NodeID: n.id,
		Retention: func() time.Duration { return code.DefaultDeletedRetention },
		SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{Roster: testChart(), Zone: time.UTC}, true },
		Events:    stream, AgentIDs: agentIDs, Now: func() time.Time { return harnessAt }, Metrics: rec,
	})
	if err != nil {
		t.Fatalf("NewChecksWriter: %v", err)
	}
	t.Cleanup(func() { w.Stop(context.WithoutCancel(t.Context())) })
	return w, counter, stream, rec
}

// checkOn is attempt of check unit of the commit oidOf("B") — a changeRepo's
// feature head — on repo.
func checkOn(repo string, attempt int) code.CheckKey {
	return code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: attempt}
}

// defOf is the definition every case queues an attempt of the check named
// name under: given ten minutes to run.
func defOf(name string) *code.CheckDef {
	return &code.CheckDef{Name: name, Run: "make " + name, TimeoutSeconds: 600}
}

// queueing asks for attempt key queued for reason, under an operation id of
// its own.
func queueing(key code.CheckKey, reason code.QueueReason) code.ChecksRequest {
	return code.ChecksRequest{OpID: statelog.NewOpID(time.Now(), "code_check_queue"),
		Queued: &code.CheckQueued{CheckKey: key, Purpose: code.PurposeTip, Def: defOf(key.Name),
			Reason: reason, QueuedAt: harnessAt}}
}

// claiming asks for key claimed by runner — naming box where it is set — at
// an instant with nanoseconds in it, as a runner's clock gives one and the
// store keeps to the microsecond: a holder's records are its own claim's
// whatever its clock carried past what the row holds.
func claiming(key code.CheckKey, runner, box string) code.ChecksRequest {
	return code.ChecksRequest{OpID: statelog.NewOpID(time.Now(), "code_check_claim"),
		Started: &code.CheckStarted{CheckKey: key, Runner: runner, Placement: code.PlacementContainer, Box: box,
			StartedAt: harnessAt.Add(time.Second + 123*time.Nanosecond)}}
}

// ending asks for key ended in state for reason.
func ending(key code.CheckKey, state code.CheckState, reason code.CheckReason) code.ChecksRequest {
	f := &code.CheckFinished{CheckKey: key, State: state, Reason: reason, DurationMs: 1_000,
		FinishedAt: harnessAt.Add(time.Minute)}
	if reason == code.CheckReasonExit {
		exit := 0
		if state == code.CheckStateFailed {
			exit = 1
		}
		f.ExitCode = &exit
	}
	return code.ChecksRequest{OpID: statelog.NewOpID(time.Now(), "code_check_end"), Finished: f}
}

// requireChecked fails t unless res is a write applied at a position, with
// a row.
func requireChecked(t *testing.T, what string, res code.ChecksResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() || res.Refusal != "" || res.Unchanged ||
		res.Row == nil {
		t.Fatalf("%s answered %+v, want applied at a position with its row", what, res)
	}
}

// requireCheckRefused fails t unless res is refused code, with no record.
func requireCheckRefused(t *testing.T, what string, res code.ChecksResult, err error, refusal code.CheckRefusal) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if res.Refusal != refusal || res.Outcome != "" || !res.Position.IsZero() {
		t.Fatalf("%s answered %+v, want refused %s with no record", what, res, refusal)
	}
}

// A SECOND CLAIM IS REFUSED BEFORE IT APPENDS: two git nodes claiming one
// attempt through the repository's one checks writer meet in its single
// flight — the second decides from the rows the first's record left, and is
// refused `claimed`, naming the first as the holder, with nothing appended, so
// it starts no box; the holder's own claim asked again is unchanged, and its
// record naming the box it made sets the box — each judged its claim's by the
// instant the row holds, to the microsecond, whatever nanoseconds the
// runner's clock gave it.
//
// Mutations: drop the decide's refusal of another runner's claim, and the
// second claim is not refused `claimed` — red; compare a claim's instant to
// the nanosecond, and the holder's own claim and box are refused `claimed` —
// red.
func TestASecondClaimIsRefusedBeforeItAppends(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := changeRepo(t, n, "web")
	w, counter, _, rec := checksWriterOn(t, n)
	key := checkOn(repo, 1)
	res, err := w.Publish(t.Context(), queueing(key, code.QueueDispatch))
	requireChecked(t, "the dispatch", res, err)

	res, err = w.Publish(t.Context(), claiming(key, "git-a", ""))
	requireChecked(t, "git-a's claim", res, err)
	if res.Holder != "git-a" || res.Row.State != code.CheckStateRunning {
		t.Fatalf("git-a's claim answered holder %q in state %s, want git-a running", res.Holder, res.Row.State)
	}
	appended := counter.appended.Load()
	res, err = w.Publish(t.Context(), claiming(key, "git-b", ""))
	requireCheckRefused(t, "git-b's claim", res, err, code.CheckRefusalClaimed)
	if res.Holder != "git-a" {
		t.Fatalf("git-b's claim was refused naming holder %q, want git-a", res.Holder)
	}
	if got := counter.appended.Load(); got != appended {
		t.Fatalf("git-b's refused claim appended %d records, want none", got-appended)
	}

	// THE HOLDER'S OWN: its claim asked again changes nothing, and its record
	// naming its box sets the box.
	again := claiming(key, "git-a", "")
	res, err = w.Publish(t.Context(), again)
	if err != nil || !res.Unchanged || res.Holder != "git-a" {
		t.Fatalf("git-a's claim asked again answered (%+v, %v), want unchanged, held by git-a", res, err)
	}
	res, err = w.Publish(t.Context(), claiming(key, "git-a", "box-1"))
	requireChecked(t, "git-a's box", res, err)
	if res.Row.Box != "box-1" {
		t.Fatalf("the holder's box record left the row's box %q, want box-1", res.Row.Box)
	}
	if got := counter.appended.Load(); got != appended+1 {
		t.Fatalf("the holder's claim again and its box appended %d records, want the box's one",
			got-appended)
	}
	for outcome, want := range map[string]uint64{"won": 1, "claimed": 1, "unchanged": 1} {
		if got := readingOf(rec, metrics.CodeChecksClaims, metrics.Attrs{"outcome": outcome}).Total; got != want {
			t.Errorf("crewlet.code.checks.claims{outcome=%s} = %v, want %v", outcome, got, want)
		}
	}
}

// A BURST OF ONE REPOSITORY'S CHECK WRITES PAST ITS BOUND IS REFUSED AT ONCE —
// the change writer's rule, one implementation (flight.go): while one write of
// a repository publishes and another waits its turn, a third of the same
// repository is refused busy, undecided, the moment it arrives, naming the
// bound, and appends nothing; another repository's write is still taken; and
// the two taken land once the held publish goes through.
//
// Mutation: drop the per-key count, and the third write waits its turn and
// lands.
func TestABurstOfOneRepositorysCheckWritesIsRefusedAtOnce(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, other := changeRepo(t, n, "web"), changeRepo(t, n, "api")
	gate := newGate(n.pub)
	w, err := code.NewChecksWriter(code.ChecksWriterOptions{Publisher: gate, DB: n.db.Reader(), NodeID: n.id,
		Retention: func() time.Duration { return code.DefaultDeletedRetention },
		SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{Roster: testChart(), Zone: time.UTC}, true },
		Now:       func() time.Time { return harnessAt }})
	if err != nil {
		t.Fatalf("NewChecksWriter: %v", err)
	}
	t.Cleanup(func() {
		gate.open()
		w.Stop(context.WithoutCancel(t.Context()))
	})
	code.SetChecksWriterBounds(w, 2, 8, code.ChecksWriterNodeBytes)
	type answer struct {
		res code.ChecksResult
		err error
	}
	publish := func(r code.ChecksRequest) <-chan answer {
		out := make(chan answer, 1)
		go func() {
			res, err := w.Publish(t.Context(), r)
			out <- answer{res, err}
		}()
		return out
	}
	first := publish(queueing(checkOn(repo, 1), code.QueueDispatch))
	eventually(t, "the first write reaching the publisher", func() bool { return gate.calls.Load() == 1 })
	second := publish(queueing(code.CheckKey{RepoID: repo, OID: oidOf("A"), Target: mainRef, Name: "unit",
		Attempt: 1}, code.QueueDispatch))
	// THE SECOND IS WAITING ITS TURN before the third arrives, or the third
	// could take its place and the second be the one refused.
	eventually(t, "the second write waiting its turn", func() bool { return code.ChecksWriterHolders(w, repo) == 2 })
	select {
	case a := <-publish(queueing(code.CheckKey{RepoID: repo, OID: oidOf("R"), Target: mainRef, Name: "unit",
		Attempt: 1}, code.QueueDispatch)):
		requireCheckRefused(t, "a third write of the repository", a.res, a.err, code.CheckRefusalBusy)
		if want := "2 check writes of this repository hold or wait for its turn on this node, the most one " +
			"repository takes at once (MaxCheckWritesWaiting)"; !strings.Contains(a.res.Detail, want) {
			t.Fatalf("the third write was refused %q, want it saying %q", a.res.Detail, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a third write of the repository waited rather than being refused at once")
	}
	elsewhere := publish(queueing(checkOn(other, 1), code.QueueDispatch))
	eventually(t, "the other repository's write reaching the publisher", func() bool {
		return gate.calls.Load() == 2
	})
	gate.open()
	for name, ch := range map[string]<-chan answer{"first": first, "second": second, "elsewhere": elsewhere} {
		select {
		case a := <-ch:
			requireChecked(t, "the "+name+" write", a.res, a.err)
		case <-time.After(30 * time.Second):
			t.Fatalf("the %s write was not answered in 30 s", name)
		}
	}
}

// AN ATTEMPT FOLLOWS THE ONE BEFORE IT, ENDED: a dispatch is a commit's first
// attempt, answered from its row when it is queued already; the next is queued
// only once the one before it ended, the home's requeue only after its
// infrastructure ended it and while that attempt is below
// AutoRequeueAttempts, and the home's dispatch only after its own dispatch
// errored it for a checks file that did not run; and no attempt passes
// MaxCheckAttempts.
//
// Mutation: let a dispatch follow any ending — the dispatch after a failure
// is queued, red; refuse a dispatch past the first attempt — the mended
// file's is refused, red.
func TestAnAttemptFollowsTheOneBeforeIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := changeRepo(t, n, "web")
	w, counter, _, _ := checksWriterOn(t, n)
	ctx := t.Context()

	res, err := w.Publish(ctx, queueing(checkOn(repo, 1), code.QueueDispatch))
	requireChecked(t, "the dispatch", res, err)
	appended := counter.appended.Load()
	res, err = w.Publish(ctx, queueing(checkOn(repo, 1), code.QueueDispatch))
	if err != nil || !res.Unchanged || res.Row == nil || res.Row.State != code.CheckStateQueued {
		t.Fatalf("a second dispatch of the head answered (%+v, %v), want unchanged from its queued row", res, err)
	}
	if counter.appended.Load() != appended {
		t.Fatal("a second dispatch of the head appended a record")
	}
	res, err = w.Publish(ctx, queueing(checkOn(repo, 2), code.QueueRequeue))
	requireCheckRefused(t, "a requeue of a queued attempt", res, err, code.CheckRefusalNotFinished)

	// THE INFRASTRUCTURE'S ENDINGS, requeued to AutoRequeueAttempts.
	for attempt := 1; attempt <= code.AutoRequeueAttempts; attempt++ {
		key := checkOn(repo, attempt)
		if attempt > 1 {
			res, err = w.Publish(ctx, queueing(key, code.QueueRequeue))
			requireChecked(t, "the requeue", res, err)
		}
		res, err = w.Publish(ctx, claiming(key, "git-a", ""))
		requireChecked(t, "the claim", res, err)
		res, err = w.Publish(ctx, ending(key, code.CheckStateErrored, code.CheckReasonRunnerLost))
		requireChecked(t, "the lost runner's ending", res, err)
	}
	res, err = w.Publish(ctx, queueing(checkOn(repo, code.AutoRequeueAttempts+1), code.QueueRequeue))
	requireCheckRefused(t, "a requeue past AutoRequeueAttempts", res, err, code.CheckRefusalAttemptsExhausted)
	res, err = w.Publish(ctx, queueing(checkOn(repo, code.AutoRequeueAttempts+2), code.QueueRequeue))
	requireCheckRefused(t, "an attempt that follows none", res, err, code.CheckRefusalNoSuchCheck)

	// A MEASUREMENT IS NO REQUEUE'S: another commit's attempt that failed.
	other := code.CheckKey{RepoID: repo, OID: oidOf("A"), Target: mainRef, Name: "unit", Attempt: 1}
	for _, req := range []code.ChecksRequest{queueing(other, code.QueueDispatch), claiming(other, "git-a", ""),
		ending(other, code.CheckStateFailed, code.CheckReasonExit)} {
		res, err = w.Publish(ctx, req)
		requireChecked(t, "the failing attempt", res, err)
	}
	other.Attempt = 2
	res, err = w.Publish(ctx, queueing(other, code.QueueRequeue))
	requireCheckRefused(t, "a requeue of a failure", res, err, code.CheckRefusalRerunGenuine)
	res, err = w.Publish(ctx, queueing(other, code.QueueDispatch))
	requireCheckRefused(t, "a dispatch after a failure", res, err, code.CheckRefusalRerunGenuine)

	// A DISPATCH FOLLOWS ITS OWN ERROR: a head's required check errored at
	// its dispatch for a checks file that did not run is queued again by the
	// head's dispatch under a file that runs, as the next attempt.
	broken := code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "lint", Attempt: 1}
	headDispatch := func(key code.CheckKey) code.ChecksRequest {
		req := queueing(key, code.QueueDispatch)
		req.Queued.Purpose, req.Queued.ChangeID, req.Queued.Revision = code.PurposeHead,
			"0190f3c4-0000-7000-8000-0000000000d1", 1
		req.Queued.Required = true
		return req
	}
	errored := headDispatch(broken)
	errored.Queued.Def, errored.Queued.Invalid = nil, "the checks file is not YAML"
	res, err = w.Publish(ctx, errored)
	requireChecked(t, "lint errored at its dispatch", res, err)
	mended := broken
	mended.Attempt = 2
	res, err = w.Publish(ctx, headDispatch(mended))
	requireChecked(t, "lint's dispatch under the mended file", res, err)
	if res.Row.State != code.CheckStateQueued || res.Row.Attempt != 2 {
		t.Fatalf("lint's dispatch under the mended file is %+v, want attempt 2 queued", res.Row)
	}

	// NO ATTEMPT PASSES THE LAST: one held at MaxCheckAttempts, which no
	// writer of this build reaches but by reruns, is followed by none.
	last := code.CheckKey{RepoID: repo, OID: oidOf("R"), Target: mainRef, Name: "unit",
		Attempt: code.MaxCheckAttempts}
	n.exec(`INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose, change_id,
		revision, candidate_id, entry_id, base_oid, defs_oid, required, run_in, timeout_seconds, reason,
		runner_node, placement, box_handle, borrowed, reporter, queued_at, started_at, finished_at,
		duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, overdue_at,
		url, summary, def, failures, tail)
		VALUES (?, ?, ?, ?, ?, 'errored', 'tip', '', 0, '', '', '', '', 0, '', 600,
			'runner_lost', 'git-a', 'container', '', 0, '', ?, ?, ?, 0, NULL, 0, 1, 2, 0, ?, '', '', '', '', '')`,
		repo, last.OID, last.Target, last.Name, last.Attempt, store.EncodeTime(harnessAt),
		store.EncodeTime(harnessAt), store.EncodeTime(harnessAt), store.EncodeTime(harnessAt.Add(600*time.Second)))
	past := last
	past.Attempt++
	res, err = w.Publish(ctx, queueing(past, code.QueueRequeue))
	requireCheckRefused(t, "an attempt past MaxCheckAttempts", res, err, code.CheckRefusalAttemptsExhausted)
	if !strings.Contains(res.Detail, "code.MaxCheckAttempts") {
		t.Fatalf("an attempt past the last was refused %q, want the bound named", res.Detail)
	}
}

// THE ENGINE'S ARMS ARE THE ENGINE'S: a queue naming a principal's rerun and
// a claim naming a person's CI are a principal's writes, asked only as a
// rerun and a report (checkasks.go), so through the engine's own arms each is
// no request the writer decides — refused invalid before anything is read —
// and an ending of an attempt no runner claimed, which no runner forms,
// likewise.
func TestAChecksWriterDecidesOnlyTheEnginesOwnRecords(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := changeRepo(t, n, "web")
	w, counter, _, _ := checksWriterOn(t, n)
	key := checkOn(repo, 1)
	rerun := queueing(key, code.QueueRerun)
	rerun.Queued.By = &dev
	report := code.ChecksRequest{OpID: statelog.NewOpID(time.Now(), "code_check_report"),
		Started: &code.CheckStarted{CheckKey: key, Placement: code.PlacementExternal, StartedAt: harnessAt}}
	for name, req := range map[string]code.ChecksRequest{"a rerun": rerun, "a report": report} {
		if _, err := w.Publish(t.Context(), req); !errors.Is(err, code.ErrInvalidProposal) {
			t.Errorf("%s answered %v, want code.ErrInvalidProposal", name, err)
		}
	}
	res, err := w.Publish(t.Context(), queueing(key, code.QueueDispatch))
	requireChecked(t, "the dispatch", res, err)
	if _, err := w.Publish(t.Context(), ending(key, code.CheckStatePassed, code.CheckReasonExit)); !errors.Is(err,
		code.ErrInvalidProposal) {
		t.Errorf("an exit of an attempt nobody claimed answered %v, want code.ErrInvalidProposal", err)
	}
	if got := counter.appended.Load(); got != 1 {
		t.Fatalf("the writer appended %d records, want the dispatch's alone", got)
	}
}

// AN ENDING IS ANNOUNCED ONCE, BY ITS WRITER: an ending whose record applied
// and which its attempt's row keeps is published as one code_check_finished
// event on the writing node's stream; one the row does not keep — an
// infrastructure ending after a measurement — appends nothing and announces
// nothing; and a retry under the same operation id, collapsed onto the record
// that landed, announces nothing again — nor counts the attempt it queued or
// the ending it set a second time. A measurement that REPLACES an
// infrastructure ending the row kept — a slow runner's exit after its attempt
// was errored runner_lost — is announced naming the ending it replaced, and
// counted under it rather than as the attempt's first ending, so a day files
// it as a correction and the attempt is one ending.
//
// Mutations: count a collapsed retry's record as the call's own, and the
// dispatch asked again counts its attempt twice — red; announce a
// replacement as the attempt's first ending, and lint's pass names nothing
// replaced and is counted a second ending — red.
func TestAnEndingIsAnnouncedOnceByItsWriter(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := changeRepo(t, n, "web")
	w, counter, stream, rec := checksWriterOn(t, n)
	key := checkOn(repo, 1)
	dispatch := queueing(key, code.QueueDispatch)
	for _, req := range []code.ChecksRequest{dispatch, claiming(key, "git-a", "")} {
		res, err := w.Publish(t.Context(), req)
		requireChecked(t, "the attempt", res, err)
	}
	failed := ending(key, code.CheckStateFailed, code.CheckReasonExit)
	res, err := w.Publish(t.Context(), failed)
	requireChecked(t, "the failure", res, err)
	waitAnnounced(t, stream, 1)
	got := stream.all()[0]
	payload, ok := got.Data.(*types.CodeCheckFinished)
	if !ok || got.Type != "code_check_finished" || payload.State != "failed" || payload.Name != "unit" ||
		payload.RepoName != "web" || payload.Attempt != 1 || payload.Placement != "container" ||
		got.Source != n.id {
		t.Fatalf("the ending was announced as %s from %q %+v, want web's unit attempt 1 failed in a container, "+
			"from %s", got.Type, got.Source, got.Data, n.id)
	}
	appended := counter.appended.Load()
	res, err = w.Publish(t.Context(), ending(key, code.CheckStateErrored, code.CheckReasonRunnerLost))
	if err != nil || !res.Unchanged || res.Row.State != code.CheckStateFailed {
		t.Fatalf("a lost runner's ending after the failure answered (%+v, %v), want unchanged, failed", res, err)
	}
	for name, again := range map[string]code.ChecksRequest{"the failure": failed, "the dispatch": dispatch} {
		if res, err = w.Publish(t.Context(), again); err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("%s asked again under its own operation id answered (%+v, %v), want applied", name, res, err)
		}
	}
	if counter.appended.Load() != appended {
		t.Fatal("an ending the row does not keep, or a write asked again, appended a record")
	}

	// A MEASUREMENT AFTER AN INFRASTRUCTURE ENDING replaces it — a runner
	// that was only slow, ending its attempt after the home's sweep errored
	// it runner_lost — and is announced as a correction of it, naming the
	// ending it replaced, never as the attempt's second ending.
	lint := code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "lint", Attempt: 1}
	for _, req := range []code.ChecksRequest{queueing(lint, code.QueueDispatch), claiming(lint, "git-a", "")} {
		res, err := w.Publish(t.Context(), req)
		requireChecked(t, "lint's attempt", res, err)
	}
	res, err = w.Publish(t.Context(), ending(lint, code.CheckStateErrored, code.CheckReasonRunnerLost))
	requireChecked(t, "lint's lost runner", res, err)
	res, err = w.Publish(t.Context(), ending(lint, code.CheckStatePassed, code.CheckReasonExit))
	requireChecked(t, "lint's exit after it", res, err)
	waitAnnounced(t, stream, 3)

	// THE STOP WAITS FOR EVERY ANNOUNCEMENT IN FLIGHT, so what the stream
	// holds after it is all the writer ever announced.
	w.Stop(t.Context())
	all := stream.all()
	if len(all) != 3 {
		t.Fatalf("the writer announced %d endings, want unit's failure, lint's lost runner and lint's pass", len(all))
	}
	// EACH ANNOUNCEMENT NAMES WHAT IT REPLACED, read by its state: an
	// announcement is published after its write's answer, so two writes'
	// may reach the stream in either order.
	replaces := map[string]string{}
	for _, ev := range all {
		e, ok := ev.Data.(*types.CodeCheckFinished)
		if !ok {
			t.Fatalf("the writer announced a %T", ev.Data)
		}
		replaces[e.Name+" "+e.State] = e.Replaces
	}
	want := map[string]string{"unit failed": "", "lint errored": "", "lint passed": "errored"}
	if !maps.Equal(replaces, want) {
		t.Fatalf("the endings were announced replacing %v, want %v: lint's pass a correction of its lost "+
			"runner, and every first ending replacing none", replaces, want)
	}
	for _, c := range []struct {
		attrs metrics.Attrs
		want  uint64
	}{
		{metrics.Attrs{"state": "failed", "placement": "container", "replaced": "none"}, 1},
		{metrics.Attrs{"state": "errored", "placement": "container", "replaced": "none"}, 1},
		{metrics.Attrs{"state": "passed", "placement": "container", "replaced": "errored"}, 1},
		{metrics.Attrs{"state": "passed", "placement": "container", "replaced": "none"}, 0},
	} {
		if got := readingOf(rec, metrics.CodeChecksFinished, c.attrs).Total; got != c.want {
			t.Errorf("crewlet.code.checks.finished%v = %v, want %v", c.attrs, got, c.want)
		}
	}
	if got := readingOf(rec, metrics.CodeChecksQueued, metrics.Attrs{"purpose": "tip"}).Total; got != 2 {
		t.Fatalf("crewlet.code.checks.queued{purpose=tip} = %v after two dispatches and a retry, want 2", got)
	}
}

// waitAnnounced waits for stream to hold n events: an announcement is
// published after the write's answer is handed back.
func waitAnnounced(t *testing.T, stream *announced, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(stream.all()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the stream holds %d announcements, want %d", len(stream.all()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// THE HOME'S DUE CHECK WORK IS THE ENGINE'S OWN: a checks_due read names no
// asker and no seat it reads for — a principal's read of the home's listing
// would be held to nothing — and lists a live change whose head is not
// dispatched, until its dispatch is recorded.
//
// Mutation: let the read take an asker, and a person's read of the listing is
// answered — red.
func TestChecksDueRefusesAnAsker(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	due := &code.ChecksDueQuery{RefreshBefore: harnessAt}
	for name, q := range map[string]code.ReadQuery{
		"a person's": {Kind: code.ReadChecksDue, Asker: ada, ChecksDue: due, Fresh: fresh},
		"a seat's":   {Kind: code.ReadChecksDue, Asker: dev, ChecksDue: due, Fresh: fresh},
		"one made for a seat": {Kind: code.ReadChecksDue, CeilingSeat: "cto",
			ChecksDue: &code.ChecksDueQuery{RepoID: repo, RefreshBefore: harnessAt}, Fresh: fresh},
		"one naming no refresh": {Kind: code.ReadChecksDue, ChecksDue: &code.ChecksDueQuery{RepoID: repo},
			Fresh: fresh},
		"one asking nothing": {Kind: code.ReadChecksDue, Fresh: fresh},
	} {
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("%s read of the due check work answered %v, want code.ErrInvalid", name, err)
		}
	}
	repos, heads, _ := checksDue(t, n, repo, harnessAt)
	if !slices.Equal(repos, []string{repo}) || len(heads) != 1 || heads[0].ID != change ||
		heads[0].Head != oidOf("B") || heads[0].Tip != oidOf("A") {
		t.Fatalf("the due check work is %v and %+v, want web and its change's head B against main's A", repos,
			heads)
	}
}

// checksDue is the home's due check work n's rows hold: the repositories
// holding some, and repo's due heads and targets, a target's reading due again
// once it was recorded at or before refresh.
func checksDue(t *testing.T, n *node, repo string, refresh time.Time) ([]string, []code.ChangeDue,
	[]code.CheckTargetDue) {

	t.Helper()
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	repos, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadChecksDue,
		ChecksDue: &code.ChecksDueQuery{RefreshBefore: refresh}, Fresh: fresh})
	if err != nil {
		t.Fatalf("the due repositories: %v", err)
	}
	work, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadChecksDue,
		ChecksDue: &code.ChecksDueQuery{RepoID: repo, RefreshBefore: refresh}, Fresh: fresh})
	if err != nil {
		t.Fatalf("the due work of %s: %v", repo, err)
	}
	return repos.DueRepos, work.DueChanges, work.DueTargets
}

// dispatching is the home's record of target main's checks file defs, read at
// the tip at position tip, with heads dispatched under it, under an operation
// id of its own.
func dispatching(repo, tipOID string, tip statelog.Position, defs string,
	heads ...code.DispatchedHead) code.ChecksRequest {

	return dispatchingFor(repo, mainRef, tipOID, tip, defs, heads...)
}

// dispatchingFor is [dispatching] for target.
func dispatchingFor(repo, target, tipOID string, tip statelog.Position, defs string,
	heads ...code.DispatchedHead) code.ChecksRequest {

	return code.ChecksRequest{OpID: statelog.NewOpID(time.Now(), "code_check_dispatch"),
		Dispatched: &code.CheckDispatched{RepoID: repo, Target: target, Tip: tipOID, TipPosition: tip,
			DefsOID: defs, Heads: heads, DispatchedAt: harnessAt}}
}

// TWO CHANGES SHARING A HEAD ARE EACH CHECKED FOR THEIR OWN TARGET: web's
// feature into main, and a backport branch at the same commit into release.
// What a check of a commit measures is the definition its TARGET's checks
// file gives it, and its target's protection is what requires it, so the head
// is two sets of checks, one a branch:
//
//   - it is due its dispatch for each target apart: main's recorded leaves
//     the backport due, release's too leaves nothing due;
//   - release's unit queued at it after main's unit passed there is an attempt
//     of its own — queued under release's definition, never answered from
//     main's row — and main's keeps its pass;
//   - each change's checks are its own target's: the feature's main's three,
//     the backport's release's one.
//
// Mutation: read the attempt a write decides by its commit, check and number
// alone (the row read ignoring its target) — release's unit is answered
// unchanged from main's passed row, and never runs — red.
func TestTwoChangesSharingAHeadAreEachCheckedForTheirOwnTarget(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, feature := discussed(t, n, "web")
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/backport", code.ZeroOID, oidOf("B"))))
	c, _ := committerOn(t, n, n.pub)
	opened, err := c.ProposeChange(t.Context(), opening(repo, "refs/heads/backport", releaseRef, dev))
	requireChanged(t, opened, err)
	backport := opened.ChangeID
	w, _, _, _ := checksWriterOn(t, n)
	ctx := t.Context()

	_, heads, _ := checksDue(t, n, repo, harnessAt)
	tips := map[string]code.ChangeDue{}
	for _, h := range heads {
		tips[h.TargetRef] = h
	}
	if len(heads) != 2 || tips[mainRef].ID != feature || tips[releaseRef].ID != backport ||
		tips[mainRef].Head != oidOf("B") || tips[releaseRef].Head != oidOf("B") {
		t.Fatalf("web's due heads are %+v, want its two changes at B, one for main and one for release", heads)
	}
	res, err := w.Publish(ctx, dispatchingFor(repo, mainRef, tips[mainRef].Tip, tips[mainRef].TipPosition,
		oidOf("main-checks"), code.DispatchedHead{OID: oidOf("B"), ChangeID: feature, Revision: 1}))
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Unchanged {
		t.Fatalf("the head's dispatch for main answered (%+v, %v), want applied", res, err)
	}
	if _, heads, _ := checksDue(t, n, repo, harnessAt); len(heads) != 1 || heads[0].ID != backport {
		t.Fatalf("with the head dispatched for main alone, the due heads are %+v, want the backport's for "+
			"release", heads)
	}

	// MAIN'S UNIT RUNS AND PASSES AT THE HEAD.
	forMain := code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: 1}
	req := queueing(forMain, code.QueueDispatch)
	req.Queued.Purpose, req.Queued.ChangeID, req.Queued.Revision = code.PurposeHead, feature, 1
	req.Queued.DefsOID, req.Queued.Def = oidOf("main-checks"), &code.CheckDef{Name: "unit", Run: "make test",
		TimeoutSeconds: 600}
	res, err = w.Publish(ctx, req)
	requireChecked(t, "main's unit at the head", res, err)
	for _, req := range []code.ChecksRequest{claiming(forMain, "git-a", ""),
		ending(forMain, code.CheckStatePassed, code.CheckReasonExit)} {
		res, err := w.Publish(ctx, req)
		requireChecked(t, "main's unit run", res, err)
	}

	// RELEASE'S UNIT AT THE SAME HEAD IS ITS OWN ATTEMPT, under its own file.
	forRelease := forMain
	forRelease.Target = releaseRef
	req = queueing(forRelease, code.QueueDispatch)
	req.Queued.Purpose, req.Queued.ChangeID, req.Queued.Revision = code.PurposeHead, backport, 1
	req.Queued.DefsOID, req.Queued.Def = oidOf("release-checks"), &code.CheckDef{Name: "unit",
		Run: "make test-release", TimeoutSeconds: 1_800}
	res, err = w.Publish(ctx, req)
	requireChecked(t, "release's unit at the head main's passed at", res, err)
	if res.Row.State != code.CheckStateQueued || res.Row.Target != releaseRef || res.Row.ChangeID != backport ||
		res.Row.DefsOID != oidOf("release-checks") {
		t.Fatalf("release's unit at the shared head is %+v, want the backport's own attempt, queued under "+
			"release's checks file", res.Row)
	}
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	for target, want := range map[string]struct {
		state code.CheckState
		run   string
	}{mainRef: {code.CheckStatePassed, "make test"}, releaseRef: {code.CheckStateQueued, "make test-release"}} {
		key := forMain
		key.Target = target
		one, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadCheck, Asker: ada, Repo: "web", Fresh: fresh,
			Checks: &code.CheckQuery{OID: key.OID, Target: key.Target, Name: key.Name, Attempt: key.Attempt}})
		if err != nil || one.Check == nil || one.Check.State != want.state || one.Check.Def == nil ||
			one.Check.Def.Run != want.run {
			t.Fatalf("unit at the head for %s read (%+v, %v), want %s running %q", target, one.Check, err,
				want.state, want.run)
		}
	}
	for number, want := range map[int64]string{1: mainRef, 2: releaseRef} {
		listed, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web",
			Fresh: fresh, Checks: &code.CheckQuery{Change: number}})
		if err != nil || len(listed.Checks) != 1 || listed.Checks[0].Target != want {
			t.Fatalf("change %d's checks are (%+v, %v), want its head's unit for %s alone", number,
				listed.Checks, err, want)
		}
	}

	res, err = w.Publish(ctx, dispatchingFor(repo, releaseRef, tips[releaseRef].Tip,
		tips[releaseRef].TipPosition, oidOf("release-checks"),
		code.DispatchedHead{OID: oidOf("B"), ChangeID: backport, Revision: 1}))
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Unchanged {
		t.Fatalf("the head's dispatch for release answered (%+v, %v), want applied", res, err)
	}
	if repos, heads, targets := checksDue(t, n, repo, harnessAt); len(repos)+len(heads)+len(targets) != 0 {
		t.Fatalf("with the head dispatched for both its targets, the due check work is %v, %+v and %+v, "+
			"want none", repos, heads, targets)
	}
}

// A HEAD LEAVES THE DUE LISTING ONCE ITS DISPATCH IS RECORDED, whatever its
// target's checks file defines: web's change into main, whose tip holds no
// checks file, is due until its home records the dispatch naming none — and
// then not, though no attempt of any check was ever queued at its head: the
// listing converges on what the home decided, never on attempts, which a
// target with no checks file — every repository that has not adopted checks —
// never has. The same dispatch asked again is answered unchanged. A checks
// file the target's reading comes to name makes the head due again, and its
// dispatch under that file takes it off again.
//
// Mutation: list a head while its commit holds no attempt of any check, as
// the listing once did — web's head, whose target defines nothing, is listed
// for ever — red.
func TestAHeadLeavesTheDueListingOnceItsDispatchIsRecorded(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, counter, _, _ := checksWriterOn(t, n)
	_, heads, _ := checksDue(t, n, repo, harnessAt)
	if len(heads) != 1 {
		t.Fatalf("web's due heads are %+v, want its change's", heads)
	}
	tip, head := heads[0].TipPosition, code.DispatchedHead{OID: oidOf("B"), ChangeID: change, Revision: 1}
	applied := func(what string, res code.ChecksResult, err error) {
		t.Helper()
		if err != nil || res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() || res.Unchanged {
			t.Fatalf("%s answered (%+v, %v), want applied at a position", what, res, err)
		}
	}
	none := func(when string) {
		t.Helper()
		if repos, heads, targets := checksDue(t, n, repo, harnessAt); len(repos)+len(heads)+len(targets) != 0 {
			t.Fatalf("%s the due check work is %v, %+v and %+v, want none", when, repos, heads, targets)
		}
	}

	res, err := w.Publish(t.Context(), dispatching(repo, oidOf("A"), tip, "", head))
	applied("the head's dispatch under no checks file", res, err)
	none("once its dispatch under no checks file is recorded,")
	appended := counter.appended.Load()
	res, err = w.Publish(t.Context(), dispatching(repo, oidOf("A"), tip, "", head))
	if err != nil || !res.Unchanged || counter.appended.Load() != appended {
		t.Fatalf("the dispatch asked again answered (%+v, %v), want unchanged with no record", res, err)
	}

	defs := oidOf("checks-file")
	res, err = w.Publish(t.Context(), dispatching(repo, oidOf("A"), tip, defs))
	applied("the target's reading naming a checks file", res, err)
	if _, heads, _ := checksDue(t, n, repo, harnessAt); len(heads) != 1 || heads[0].ID != change {
		t.Fatalf("with its target naming a checks file its dispatch did not, the due heads are %+v, want "+
			"web's change's again", heads)
	}
	res, err = w.Publish(t.Context(), dispatching(repo, oidOf("A"), tip, defs, head))
	applied("the head's dispatch under the checks file", res, err)
	none("once its dispatch under the target's checks file is recorded,")
}

// A DISPATCH QUEUES ITS ATTEMPTS IN ITS OWN RECORD: a head's dispatch
// carrying the attempts it queues is ONE record — the head taken off the
// due listing and its attempts' rows written by the same apply, each as a
// check_queued would write it: unit queued to run, docs recorded skipped and
// announced as an ending — counted and announced by the writer as the single
// queues were. Asked again it is answered unchanged, appending nothing; and
// a dispatch carrying an attempt the rows already hold and one they do not is
// appended with the new one alone, the held one's row untouched. A dispatch
// carrying an attempt it cannot — one queued twice, one for another target,
// one under another checks file, one at a head it does not name, past its
// rows — is no write at all.
//
// Mutations: keep an attempt the rows already hold in the record — the
// dispatch asked again appends, red; apply the dispatch without its attempts
// — no row is queued, red; announce none of the attempts a dispatch ended —
// docs' skip is never announced, red.
func TestADispatchQueuesItsAttemptsInItsOwnRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, counter, stream, rec := checksWriterOn(t, n)
	ctx := t.Context()
	_, heads, _ := checksDue(t, n, repo, harnessAt)
	if len(heads) != 1 {
		t.Fatalf("web's due heads are %+v, want its change's", heads)
	}
	defs := oidOf("checks-file")
	attempt := func(name string, skipped bool) code.CheckQueued {
		return code.CheckQueued{CheckKey: code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef,
			Name: name, Attempt: 1}, Purpose: code.PurposeHead, ChangeID: change, Revision: 1, DefsOID: defs,
			Def: defOf(name), Reason: code.QueueDispatch, Skipped: skipped, QueuedAt: harnessAt}
	}
	head := code.DispatchedHead{OID: oidOf("B"), ChangeID: change, Revision: 1}
	dispatch := func(attempts ...code.CheckQueued) code.ChecksRequest {
		req := dispatching(repo, heads[0].Tip, heads[0].TipPosition, defs, head)
		req.Dispatched.Attempts = attempts
		return req
	}
	rowOf := func(name string) *code.CheckRow {
		t.Helper()
		one, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadCheck, Asker: ada, Repo: "web",
			Fresh: statelog.Freshness{Level: statelog.ReadStale}, Checks: &code.CheckQuery{OID: oidOf("B"),
				Target: mainRef, Name: name, Attempt: 1}})
		if errors.Is(err, code.ErrCheckNotFound) {
			return nil
		}
		if err != nil {
			t.Fatalf("read %s at the head: %v", name, err)
		}
		if one.Check == nil {
			return nil
		}
		return &one.Check.CheckRow
	}

	before := counter.appended.Load()
	res, err := w.Publish(ctx, dispatch(attempt("unit", false), attempt("docs", true)))
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Unchanged ||
		counter.appended.Load() != before+1 {
		t.Fatalf("the head's dispatch with its attempts answered (%+v, %v) appending %d records, want applied "+
			"in one", res, err, counter.appended.Load()-before)
	}
	unit, docs := rowOf("unit"), rowOf("docs")
	if unit == nil || unit.State != code.CheckStateQueued || unit.Queued.Packed() != res.Position.Packed() ||
		docs == nil || docs.State != code.CheckStateSkipped || docs.Queued.Packed() != res.Position.Packed() {
		t.Fatalf("after the dispatch unit is %+v and docs %+v, want unit queued and docs skipped by its record",
			unit, docs)
	}
	if _, due, _ := checksDue(t, n, repo, harnessAt); len(due) != 0 {
		t.Fatalf("a head whose dispatch landed with its attempts is still due: %+v", due)
	}
	waitAnnouncedOf(t, stream, "code_check_finished", 1)
	if e, ok := stream.of("code_check_finished")[0].Data.(*types.CodeCheckFinished); !ok || e.Name != "docs" ||
		e.State != string(code.CheckStateSkipped) {
		t.Fatalf("the dispatch announced %+v, want docs' skip", stream.of("code_check_finished")[0].Data)
	}
	if got := readingOf(rec, metrics.CodeChecksQueued, metrics.Attrs{"purpose": "head"}).Total; got != 1 {
		t.Fatalf("crewlet.code.checks.queued{purpose=head} = %v after a dispatch queuing unit, want 1", got)
	}

	appended := counter.appended.Load()
	res, err = w.Publish(ctx, dispatch(attempt("unit", false), attempt("docs", true)))
	if err != nil || !res.Unchanged || counter.appended.Load() != appended {
		t.Fatalf("the dispatch asked again answered (%+v, %v), want unchanged with no record", res, err)
	}
	res, err = w.Publish(ctx, dispatch(attempt("unit", false), attempt("lint", false)))
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Unchanged ||
		counter.appended.Load() != appended+1 {
		t.Fatalf("a dispatch adding lint beside the held unit answered (%+v, %v), want applied in one record",
			res, err)
	}
	if lint, again := rowOf("lint"), rowOf("unit"); lint == nil || lint.Queued.Packed() != res.Position.Packed() ||
		again == nil || again.Queued.Packed() != unit.Queued.Packed() {
		t.Fatalf("after the second dispatch lint is %+v and unit %+v, want lint queued by it and unit's queue "+
			"the first's", lint, again)
	}

	other := attempt("e2e", false)
	other.Target = releaseRef
	underAnother := attempt("e2e", false)
	underAnother.DefsOID = oidOf("another-file")
	unnamed := attempt("e2e", false)
	unnamed.OID = oidOf("C")
	crowd := make([]code.CheckQueued, 0, code.MaxDispatchRows)
	for i := range code.MaxDispatchRows {
		crowd = append(crowd, attempt(fmt.Sprintf("c%03d", i), false))
	}
	for what, attempts := range map[string][]code.CheckQueued{
		"an attempt queued twice":           {attempt("e2e", false), attempt("e2e", true)},
		"an attempt for another target":     {other},
		"an attempt under another file":     {underAnother},
		"an attempt at a head it names not": {unnamed},
		"more attempts than a record rows":  crowd,
	} {
		if _, err := w.Publish(ctx, dispatch(attempts...)); !errors.Is(err, code.ErrInvalidProposal) {
			t.Errorf("a dispatch carrying %s answered %v, want code.ErrInvalidProposal", what, err)
		}
	}
}

// A TARGET THAT MOVES IS READ AGAIN AT THE HOME'S PACE: once web's head is
// dispatched at main's tip A, a push moving main to C makes no head due — the
// head's dispatch names the file the target's reading holds — and makes main
// due a reading only once that reading is older than the home's refresh: a
// target pushed to on every landing is read at the home's pace, never at its
// own. Read again at C, it is due nothing.
//
// Mutation: list a target due a reading as soon as its branch moves, whatever
// its reading's age — main is due at once — red.
func TestAMovedTargetIsDueAReadingAtTheHomesPace(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _, _, _ := checksWriterOn(t, n)
	_, heads, _ := checksDue(t, n, repo, harnessAt)
	if len(heads) != 1 {
		t.Fatalf("web's due heads are %+v, want its change's", heads)
	}
	res, err := w.Publish(t.Context(), dispatching(repo, oidOf("A"), heads[0].TipPosition, "",
		code.DispatchedHead{OID: oidOf("B"), ChangeID: change, Revision: 1}))
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the head's dispatch answered (%+v, %v), want applied", res, err)
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("C"))))

	// THE READING WAS RECORDED A MOMENT AGO, after an hour ago.
	if repos, heads, targets := checksDue(t, n, repo, time.Now().Add(-time.Hour)); len(repos)+len(heads)+
		len(targets) != 0 {
		t.Fatalf("with main's reading recorded inside the home's refresh, the due check work is %v, %+v and "+
			"%+v, want none", repos, heads, targets)
	}
	repos, heads, targets := checksDue(t, n, repo, time.Now().Add(time.Hour))
	if !slices.Equal(repos, []string{repo}) || len(heads) != 0 || len(targets) != 1 ||
		targets[0].TargetRef != mainRef || targets[0].Tip != oidOf("C") {
		t.Fatalf("with main's reading past the home's refresh, the due check work is %v, %+v and %+v, want "+
			"web for main's reading at C alone", repos, heads, targets)
	}
	res, err = w.Publish(t.Context(), dispatching(repo, oidOf("C"), targets[0].TipPosition, ""))
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("main's reading at C answered (%+v, %v), want applied", res, err)
	}
	if repos, heads, targets := checksDue(t, n, repo, time.Now().Add(time.Hour)); len(repos)+len(heads)+
		len(targets) != 0 {
		t.Fatalf("with main read again at C, the due check work is %v, %+v and %+v, want none", repos, heads,
			targets)
	}
}

// A CHECK'S BOX'S READ OF ITS ATTEMPT IS THE ENGINE'S OWN: a check_box read
// names no asker and no seat it reads for — a check's box is no principal,
// and judged as one it would be refused as one nobody named — and its attempt
// by its key whole, nothing else; it answers the attempt's row, held to no
// grant (the attempt's repository is `visibility: unit` and the read names
// nobody it admits), and none for an attempt the rows do not hold, which the
// box's admission reads again through the barrier before it refuses. A
// principal's read naming the key is refused, so one that forgot its asker is
// never answered ungated.
//
// Mutations: let the read take an asker — a person's is answered, red; answer
// an attempt not held as an error — the admission's re-read never happens,
// red.
func TestACheckBoxReadIsTheEnginesOwn(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	unit := suiteSettings()
	unit.Visibility = code.VisibilityUnit
	repo := n.ensure("web", unit, t1).RepoID
	key := checkOn(repo, 1)
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	for name, q := range map[string]code.ReadQuery{
		"a person's":      {Kind: code.ReadCheckBox, Asker: ada, CheckBox: &key, Fresh: fresh},
		"a seat's":        {Kind: code.ReadCheckBox, Asker: dev, CheckBox: &key, Fresh: fresh},
		"one for a seat":  {Kind: code.ReadCheckBox, CeilingSeat: "cto", CheckBox: &key, Fresh: fresh},
		"one naming none": {Kind: code.ReadCheckBox, Fresh: fresh},
		"one naming a repository": {Kind: code.ReadCheckBox, CheckBox: &key, Repo: "web",
			Fresh: fresh},
		"a principal's naming a key": {Kind: code.ReadCheck, Asker: ada, Repo: "web", CheckBox: &key,
			Checks: &code.CheckQuery{OID: key.OID, Target: key.Target, Name: key.Name, Attempt: 1}, Fresh: fresh},
	} {
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("%s check_box read answered %v, want code.ErrInvalid", name, err)
		}
	}
	bad := key
	bad.OID = "not-a-commit"
	if _, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadCheckBox, CheckBox: &bad,
		Fresh: fresh}); !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "check_box.oid") {
		t.Errorf("a check_box read of a key naming no commit answered %v, want code.ErrInvalid naming "+
			"check_box.oid", err)
	}
	read := func() []code.CheckRow {
		t.Helper()
		a, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadCheckBox, CheckBox: &key, Fresh: fresh})
		if err != nil {
			t.Fatalf("the check_box read: %v", err)
		}
		return a.Checks
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("an attempt never queued was read as %+v, want none", got)
	}
	w, _, _, _ := checksWriterOn(t, n)
	res, err := w.Publish(t.Context(), queueing(key, code.QueueDispatch))
	requireChecked(t, "the dispatch", res, err)
	got := read()
	if len(got) != 1 || got[0].CheckKey != key || got[0].State != code.CheckStateQueued || got[0].Queued.IsZero() {
		t.Fatalf("a queued attempt was read as %+v, want its row, queued at its record", got)
	}
}

// checksShape is one shape of a `checks` read over [checksOfEveryShape]'s
// rows — a page of at most its query's limit — and the names of the attempts
// it lists, in its order.
type checksShape struct {
	q    code.ReadQuery
	want []string
}

// checksOfEveryShape is a repository web whose head B is checked for two
// targets — web's feature into main (change 1: main's lint and unit failed a
// minute apart, its vet running) and a backport branch at the same commit into
// release (change 2: release's own unit, queued) — with every shape a `checks`
// read takes of them: the node, the backport's change, and the shapes.
func checksOfEveryShape(t *testing.T) (*node, code.ChangeResult, map[string]checksShape) {
	t.Helper()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/backport", code.ZeroOID, oidOf("B"))))
	c, _ := committerOn(t, n, n.pub)
	backport, err := c.ProposeChange(t.Context(), opening(repo, "refs/heads/backport", releaseRef, dev))
	requireChanged(t, backport, err)
	w, _, _, _ := checksWriterOn(t, n)
	ctx := t.Context()
	release := code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: releaseRef, Name: "unit", Attempt: 1}
	req := queueing(release, code.QueueDispatch)
	req.Queued.Purpose, req.Queued.ChangeID, req.Queued.Revision = code.PurposeHead, backport.ChangeID, 1
	res, err := w.Publish(ctx, req)
	requireChecked(t, "release's unit at the shared head", res, err)
	for i, name := range []string{"lint", "unit", "vet"} {
		key := code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: name, Attempt: 1}
		req := queueing(key, code.QueueDispatch)
		req.Queued.Def = &code.CheckDef{Name: name, Run: "make " + name, TimeoutSeconds: 60}
		req.Queued.Purpose, req.Queued.ChangeID, req.Queued.Revision = code.PurposeHead, change, 1
		for _, r := range []code.ChecksRequest{req, claiming(key, "git-a", "")} {
			res, err := w.Publish(ctx, r)
			requireChecked(t, "the attempt "+name, res, err)
		}
		if i == 2 {
			continue // vet stays running
		}
		end := ending(key, code.CheckStateFailed, code.CheckReasonExit)
		end.Finished.FinishedAt = harnessAt.Add(time.Duration(i+1) * time.Minute)
		end.Finished.Tail = "--- FAIL"
		end.Finished.Failures = []code.CheckFailure{{Path: "a_test.go", Line: 3, TestID: "TestA", Message: "no"}}
		res, err := w.Publish(ctx, end)
		requireChecked(t, "the ending of "+name, res, err)
	}
	stale := statelog.Freshness{Level: statelog.ReadStale}
	return n, backport, map[string]checksShape{
		"a commit's, for every target": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web", Limit: 1,
			Fresh: stale, Checks: &code.CheckQuery{OID: oidOf("B")}}, []string{"lint", "unit", "vet", "unit"}},
		"a change's": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web", Limit: 2, Fresh: stale,
			Checks: &code.CheckQuery{Change: 1}}, []string{"lint", "unit", "vet"}},
		"a change's at its head": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web", Limit: 1,
			Fresh: stale, Checks: &code.CheckQuery{Change: 1, OID: oidOf("B")}}, []string{"lint", "unit", "vet"}},
		"the backport's, at the same head": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web",
			Limit: 1, Fresh: stale, Checks: &code.CheckQuery{Change: 2}}, []string{"unit"}},
		"a commit's, for one target": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web", Limit: 1,
			Fresh: stale, Checks: &code.CheckQuery{OID: oidOf("B"), Target: mainRef}}, []string{"lint", "unit",
			"vet"}},
		"a commit's, for the other target": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web",
			Limit: 1, Fresh: stale, Checks: &code.CheckQuery{OID: oidOf("B"), Target: releaseRef}},
			[]string{"unit"}},
		"a change's latest": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web", Limit: 1,
			Fresh: stale, Checks: &code.CheckQuery{Change: 1, Latest: true}}, []string{"lint", "unit", "vet"}},
		"a commit's latest, for every target": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web",
			Limit: 1, Fresh: stale, Checks: &code.CheckQuery{OID: oidOf("B"), Latest: true}},
			[]string{"lint", "unit", "vet", "unit"}},
		"the repository's, newest ended first": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web",
			Limit: 1, Fresh: stale}, []string{"unit", "lint", "unit", "vet"}},
		"the window's": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Limit: 1, Fresh: stale,
			Checks: &code.CheckQuery{Since: harnessAt}}, []string{"unit", "lint"}},
		"the window past the first ending": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Fresh: stale,
			Checks: &code.CheckQuery{Since: harnessAt.Add(90 * time.Second)}}, []string{"unit"}},
		"the window ending before the second ending": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada,
			Limit: 1, Fresh: stale, Checks: &code.CheckQuery{Since: harnessAt,
				Until: harnessAt.Add(90 * time.Second)}}, []string{"lint"}},
	}
}

// servedChecks is every attempt q lists, a page at a time from q's own cursor.
func servedChecks(t *testing.T, n *node, q code.ReadQuery) []code.CheckRow {
	t.Helper()
	var all []code.CheckRow
	for pages := 1; pages <= 100; pages++ {
		got, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("read %+v: %v", q.Checks, err)
		}
		all = append(all, got.Checks...)
		if got.Next == "" {
			return all
		}
		q.Cursor = got.Next
	}
	t.Fatalf("the listing %+v did not end in a hundred pages", q.Checks)
	return nil
}

// checkNames is the names of rows, in their order.
func checkNames(rows []code.CheckRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

// A REPOSITORY'S CHECKS ARE READ BY EVERY SHAPE: a commit's attempts for
// every target and for the one it names, a change's — its head's, checked
// for ITS target — each also at its checks' latest attempts, the
// repository's newest ended first and the window's, with an end or none —
// each a keyset that resumes where its page ended, a window with an end
// served from inside it, its first page of one row the newest ending before
// the end — and one attempt whole, with the failures its
// ending named and its repository's name; and a reader the repository's grant
// shuts out reads none of them.
//
// TWO CHANGES SHARE A HEAD, into two branches ([checksOfEveryShape]): each is
// checked for its own target — main's lint, unit and vet, release's own unit
// — as two sets of attempts at one commit, and each change reads its own
// target's alone.
//
// Mutations: read a change's attempts at its head for every target — the
// backport's change reads main's three beside its own unit — red; read a
// commit's for the target it names by every target (the reader's Target case
// dropped) → release's unit among main's, red; seek the window with no end
// (checksWindowSQL's `finished_at < ?2` dropped) → its first page is unit,
// which ended after the end, red.
func TestARepositorysChecksAreReadByEveryShape(t *testing.T) {
	t.Parallel()
	n, backport, shapes := checksOfEveryShape(t)
	ctx := t.Context()
	stale := statelog.Freshness{Level: statelog.ReadStale}
	for name, tc := range shapes {
		if got := checkNames(servedChecks(t, n, tc.q)); !slices.Equal(got, tc.want) {
			t.Errorf("%s checks read %v, want %v", name, got, tc.want)
		}
	}
	one, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadCheck, Asker: ada, Repo: "web", Fresh: stale,
		Checks: &code.CheckQuery{OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: 1}})
	if err != nil {
		t.Fatalf("one attempt: %v", err)
	}
	if c := one.Check; c == nil || c.Repo != "web" || c.State != code.CheckStateFailed || c.Def == nil ||
		c.Def.Run != "make unit" ||
		c.Tail != "--- FAIL" || len(c.Failures) != 1 || c.Failures[0].TestID != "TestA" {
		t.Fatalf("one attempt read %+v, want main's unit's failure whole", one.Check)
	}
	other, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadCheck, Asker: ada, Repo: "web", Fresh: stale,
		Checks: &code.CheckQuery{OID: oidOf("B"), Target: releaseRef, Name: "unit", Attempt: 1}})
	if err != nil || other.Check == nil || other.Check.State != code.CheckStateQueued ||
		other.Check.ChangeID != backport.ChangeID {
		t.Fatalf("release's unit at the shared head read (%+v, %v), want the backport's own attempt, queued",
			other.Check, err)
	}
	if _, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadCheck, Asker: ada, Repo: "web", Fresh: stale,
		Checks: &code.CheckQuery{OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: 2}}); !errors.Is(err,
		code.ErrCheckNotFound) {
		t.Fatalf("an attempt never queued read %v, want code.ErrCheckNotFound", err)
	}
	n.chart = testChart()
	if _, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadChecks, Asker: agent("outsider"), Repo: "web",
		Fresh: stale}); !errors.Is(err, code.ErrNotPermitted) {
		t.Fatalf("a seat the grant shuts out read the repository's checks: %v", err)
	}
	window, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadChecks, Asker: agent("outsider"),
		Fresh: stale})
	if err != nil || len(window.Checks) != 0 {
		t.Fatalf("a seat the grant shuts out read the window (%v, %v), want none of web's", window.Checks, err)
	}
}

// insertAttempt writes attempt of check name of commit B checked for target
// on repo straight into code_checks, ended in state at the attempt's own
// minute: a history of reruns no test reaches through the writer but by a
// rerun a principal asks for each.
func insertAttempt(n *node, repo, target, name string, attempt int, state code.CheckState) {
	at := harnessAt.Add(time.Duration(attempt) * time.Minute)
	n.exec(`INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose, change_id,
		revision, candidate_id, entry_id, base_oid, defs_oid, required, run_in, timeout_seconds, reason,
		runner_node, placement, box_handle, borrowed, reporter, queued_at, started_at, finished_at,
		duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, overdue_at,
		url, summary, def, failures, tail)
		VALUES (?, ?, ?, ?, ?, ?, 'head', '', 1, '', '', '', '', 1, '', 600,
			'exit', 'git-a', 'container', '', 0, '', ?, ?, ?, 1000, NULL, 0, 1, 2, 0, 0, '', '', '', '', '')`,
		repo, oidOf("B"), target, name, attempt, string(state), store.EncodeTime(at), store.EncodeTime(at),
		store.EncodeTime(at.Add(time.Second)))
}

// A LATEST LISTING ANSWERS WHAT EACH CHECK STANDS AT, IN ONE PAGE. Change 1's
// head B is checked for main by sixty checks, six of them rerun — the
// last-named among them, its first attempt failed and its second passed — so
// main holds sixty-six attempts at B, past a listing's default page of fifty,
// and release one check's three beside them. A change's listing at its
// latest answers sixty rows, one a check, each at its latest attempt — the
// last-named passed — on ONE page with no next; a commit's for main answers
// the same; a commit's for every branch adds release's third attempt; and
// each, read seven rows at a time, walks to the same rows.
//
// Mutations: keep every attempt (latestOnly matching nothing) → sixty-six
// rows, red; take the listing's default page (latestCheckLimit at
// defaultCheckLimit) → fifty rows and a next, red.
func TestALatestListingAnswersWhatEachCheckStandsAtInOnePage(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, _ := discussed(t, n, "web")
	const checks, reruns = 60, 6
	var main []string
	for i := range checks {
		name := fmt.Sprintf("c%02d", i)
		insertAttempt(n, repo, mainRef, name, 1, code.CheckStateFailed)
		latest := name + "/1"
		if i >= checks-reruns {
			insertAttempt(n, repo, mainRef, name, 2, code.CheckStatePassed)
			latest = name + "/2"
		}
		main = append(main, latest)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		insertAttempt(n, repo, releaseRef, "c00", attempt, code.CheckStateErrored)
	}
	stale := statelog.Freshness{Level: statelog.ReadStale}
	attempts := func(rows []code.CheckRow) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Name+"/"+strconv.Itoa(r.Attempt))
		}
		return out
	}
	for name, tc := range map[string]struct {
		c    code.CheckQuery
		want []string
	}{
		"a change's":          {code.CheckQuery{Change: 1, Latest: true}, main},
		"a commit's for main": {code.CheckQuery{OID: oidOf("B"), Target: mainRef, Latest: true}, main},
		"a commit's for every branch": {code.CheckQuery{OID: oidOf("B"), Latest: true},
			append(slices.Clone(main), "c00/3")},
	} {
		q := code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web", Fresh: stale, Checks: &tc.c}
		page, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("%s at its latest: %v", name, err)
		}
		if got := attempts(page.Checks); !slices.Equal(got, tc.want) || page.Next != "" {
			t.Errorf("%s at its latest answered %v (next %q), want %v on one page", name, got, page.Next, tc.want)
			continue
		}
		if last := page.Checks[checks-1]; last.Name != "c59" || last.State != code.CheckStatePassed {
			t.Errorf("%s at its latest stands c59 at %+v, want its rerun, passed", name, last)
		}
		q.Limit = 7
		if got := attempts(servedChecks(t, n, q)); !slices.Equal(got, tc.want) {
			t.Errorf("%s at its latest, seven a page, walked %v, want %v", name, got, tc.want)
		}
	}
}

// A CHECKS LISTING REFUSES WHAT ITS WALK CANNOT ANSWER, naming the field: a
// latest listing of a repository's attempts or of the window, which walk the
// endings in time and hold a check's attempts anywhere in them; a branch
// beside no commit, beside a change — whose own target its listing reads —
// or that is no branch; an end beside a repository, or at its window's
// start; a cursor of the window at or past the window's end, which no page
// of it names; and a latest `check` read and one with an end, which name
// their attempt.
//
// Mutation: drop the refusal of a latest listing beside no change and no
// commit → the repository's latest answers every attempt, red.
func TestAChecksListingRefusesWhatItsWalkCannotAnswer(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	discussed(t, n, "web")
	stale := statelog.Freshness{Level: statelog.ReadStale}
	for name, q := range map[string]code.ReadQuery{
		"a repository's at its latest": {Kind: code.ReadChecks, Repo: "web",
			Checks: &code.CheckQuery{Latest: true}},
		"the window at its latest": {Kind: code.ReadChecks, Checks: &code.CheckQuery{Latest: true}},
		"a branch beside no commit": {Kind: code.ReadChecks, Repo: "web",
			Checks: &code.CheckQuery{Target: mainRef}},
		"a branch beside a change": {Kind: code.ReadChecks, Repo: "web",
			Checks: &code.CheckQuery{Change: 1, OID: oidOf("B"), Target: mainRef}},
		"a branch that is no branch": {Kind: code.ReadChecks, Repo: "web",
			Checks: &code.CheckQuery{OID: oidOf("B"), Target: "refs/tags/v1"}},
		"one attempt at its latest": {Kind: code.ReadCheck, Repo: "web", Checks: &code.CheckQuery{
			OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: 1, Latest: true}},
		"a repository's with an end": {Kind: code.ReadChecks, Repo: "web",
			Checks: &code.CheckQuery{Until: harnessAt}},
		"a window ending as it starts": {Kind: code.ReadChecks,
			Checks: &code.CheckQuery{Since: harnessAt, Until: harnessAt}},
		"one attempt with an end": {Kind: code.ReadCheck, Repo: "web", Checks: &code.CheckQuery{
			OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: 1, Until: harnessAt}},
	} {
		q.Asker, q.Fresh = ada, stale
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("%s answered %v, want code.ErrInvalid", name, err)
		}
	}
	past := strconv.FormatInt(store.EncodeTime(harnessAt.Add(time.Hour)), 10) + "/" +
		"0190f3c4-0000-7000-8000-0000000000aa/" + oidOf("B") + "/" +
		base64.RawURLEncoding.EncodeToString([]byte(mainRef)) + "/unit/1"
	if _, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Fresh: stale,
		Cursor: past, Checks: &code.CheckQuery{Since: harnessAt, Until: harnessAt.Add(time.Minute)}}); !errors.Is(
		err, code.ErrBadCursor) {
		t.Errorf("a cursor past the window's end answered %v, want code.ErrBadCursor", err)
	}
}

// THE CHECKS LISTING IS HELD TO THE GRANT, NARROWED OR NOT. Of three
// repositories — sales-private and plat-private at `visibility: unit`,
// platform at `company` — each holding a failed `unit` and a passed `lint`,
// a seat's window lists exactly the attempts of the repositories its grant
// reads, newest ended first, and a narrowing by state or by a check's name
// keeps fewer of those, never one of another repository; a person's lists
// every repository's. A page is the rows it READ: one the grant and the
// narrowing kept nothing of still names where the next starts, so a listing a
// row at a time reaches every kept row. A repository's own listing narrowed
// by state keeps that state's, and one of a repository the grant shuts out is
// refused not_permitted, however narrow — and a state that is no check's is
// refused before anything is read. Every row listed names its repository by
// name, the window's as a repository's own, since a reader links an attempt
// to its repository, change and commit by name.
//
// Mutations: leave the window's rows unnamed (checksWindow not filling Repo)
// → every window row names "", red; keep a window's row by the narrowing alone (checksWindow
// dropping `listed`) → vp-sales lists plat-private's unit, red; ignore the
// narrowing on a repository's listing (pageChecks kept everything there) →
// sales-private's passed listing holds its failed unit, red; name the next
// page after the last KEPT row → the one-row listing stops at its first empty
// page, before sales-private's unit, red.
func TestTheChecksListingIsHeldToTheGrant(t *testing.T) {
	t.Parallel()
	n, ids := grantedChanges(t)
	w, _, _, _ := checksWriterOn(t, n)
	ctx := t.Context()
	repoOf := map[string]string{}
	minute := 0
	for _, repo := range []string{"sales-private", "plat-private", "platform"} {
		repoOf[ids[repo]] = repo
		for _, c := range []struct {
			name  string
			state code.CheckState
		}{{"unit", code.CheckStateFailed}, {"lint", code.CheckStatePassed}} {
			key := code.CheckKey{RepoID: ids[repo], OID: oidOf("B"), Target: mainRef, Name: c.name, Attempt: 1}
			minute++
			end := ending(key, c.state, code.CheckReasonExit)
			end.Finished.FinishedAt = harnessAt.Add(time.Duration(minute) * time.Minute)
			for _, r := range []code.ChecksRequest{queueing(key, code.QueueDispatch), claiming(key, "git-a", ""),
				end} {
				res, err := w.Publish(ctx, r)
				requireChecked(t, repo+"'s "+c.name, res, err)
			}
		}
	}
	stale := statelog.Freshness{Level: statelog.ReadStale}
	list := func(q code.ReadQuery) ([]string, error) {
		t.Helper()
		q.Kind, q.Fresh = code.ReadChecks, stale
		var all []string
		for {
			got, err := n.reader.Read(ctx, q)
			if err != nil {
				return all, err
			}
			for _, row := range got.Checks {
				if row.Repo != repoOf[row.RepoID] {
					t.Errorf("%s's %s was listed naming repository %q, want %q", row.RepoID, row.Name, row.Repo,
						repoOf[row.RepoID])
				}
				all = append(all, repoOf[row.RepoID]+"/"+row.Name)
			}
			if got.Next == "" {
				return all, nil
			}
			q.Cursor = got.Next
		}
	}
	window := func(c code.CheckQuery) *code.CheckQuery { c.Since = harnessAt; return &c }
	for _, tc := range []struct {
		name string
		q    code.ReadQuery
		want []string
	}{
		{"vp-sales's failed window", code.ReadQuery{Asker: agent("vp-sales"),
			Checks: window(code.CheckQuery{State: code.CheckStateFailed})},
			[]string{"platform/unit", "sales-private/unit"}},
		{"vp-sales's unit window, a row a page", code.ReadQuery{Asker: agent("vp-sales"), Limit: 1,
			Checks: window(code.CheckQuery{Name: "unit"})}, []string{"platform/unit", "sales-private/unit"}},
		{"dev's lint window", code.ReadQuery{Asker: agent("dev"), Checks: window(code.CheckQuery{Name: "lint"})},
			[]string{"platform/lint", "plat-private/lint"}},
		{"vp-sales's whole window", code.ReadQuery{Asker: agent("vp-sales"), Checks: window(code.CheckQuery{})},
			[]string{"platform/lint", "platform/unit", "sales-private/lint", "sales-private/unit"}},
		{"a person's failed window", code.ReadQuery{Asker: ada,
			Checks: window(code.CheckQuery{State: code.CheckStateFailed})},
			[]string{"platform/unit", "plat-private/unit", "sales-private/unit"}},
		{"vp-sales's passed in sales-private", code.ReadQuery{Asker: agent("vp-sales"), Repo: "sales-private",
			Checks: &code.CheckQuery{State: code.CheckStatePassed}}, []string{"sales-private/lint"}},
	} {
		if got, err := list(tc.q); err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s listed (%v, %v), want %v", tc.name, got, err, tc.want)
		}
	}
	if got, err := list(code.ReadQuery{Asker: agent("vp-sales"), Repo: "plat-private",
		Checks: &code.CheckQuery{State: code.CheckStateFailed}}); !errors.Is(err, code.ErrNotPermitted) {
		t.Errorf("vp-sales's failed listing of plat-private = (%v, %v), want code.ErrNotPermitted", got, err)
	}
	if _, err := list(code.ReadQuery{Asker: ada, Checks: window(code.CheckQuery{State: "green"})}); !errors.Is(err,
		code.ErrInvalid) {
		t.Errorf("a window narrowed by a state that is no check's = %v, want code.ErrInvalid", err)
	}
}

// A CHECKS PAGE KEPT SHORTER THAN IT WAS SERVED RESUMES AFTER ITS LAST ROW:
// for every shape a `checks` read takes — a change's attempts, a commit's, a
// repository's newest ended first and the window's — the cursor a caller asks
// for after any row it was served (code.ReadQuery.ChecksCursor) reads every
// attempt after that row once, in order, as code_get_change's `checks` part
// does when a page of wide summaries is past one tool answer; and a page's own
// cursor is the one its last row names.
//
// Mutation: ChecksCursor spelled by another walk than the reader's (a
// change's attempts by the commit's walk) → red.
func TestAChecksPageResumesAfterAnyRowItServed(t *testing.T) {
	t.Parallel()
	n, _, shapes := checksOfEveryShape(t)
	for name, tc := range shapes {
		served := servedChecks(t, n, tc.q)
		if len(served) < 2 && name != "the window past the first ending" &&
			name != "the backport's, at the same head" {
			t.Fatalf("%s lists %v; the case needs two attempts to resume between", name, checkNames(served))
		}
		for i, row := range served {
			resumed := tc.q
			resumed.Cursor = tc.q.ChecksCursor(row)
			if got, want := checkNames(servedChecks(t, n, resumed)), checkNames(served[i+1:]); !slices.Equal(got,
				want) {
				t.Errorf("%s resumed after %s/%d (cursor %q) listed %v, want %v", name, row.Name, row.Attempt,
					resumed.Cursor, got, want)
			}
		}
		first, err := n.reader.Read(t.Context(), tc.q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if k := len(first.Checks); first.Next != "" && first.Next != tc.q.ChecksCursor(first.Checks[k-1]) {
			t.Errorf("%s's first page resumes at %q, and its last row names %q", name, first.Next,
				tc.q.ChecksCursor(first.Checks[k-1]))
		}
	}
}

// THE BACKLOG COUNTS EACH QUEUED CHECK AT ITS OWN TIMEOUT: of three attempts
// queued at one instant — a check given a minute to run, one given ten, and
// one given a minute that a runner has claimed since — the reading two
// minutes on counts the first alone, naming it by its repository's name: the
// second is inside its own time, and the third is running, queued no more.
// Thirty seconds on, none is behind.
//
// Mutation: judge every attempt at one threshold — any one — and the first
// two, queued together, are counted together or not at all — red.
func TestTheBacklogCountsEachCheckAtItsOwnTimeout(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := changeRepo(t, n, "web")
	w, _, _, _ := checksWriterOn(t, n)
	for _, c := range []struct {
		name    string
		timeout int
	}{{"lint", 60}, {"unit", 600}, {"vet", 60}} {
		req := queueing(code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: c.name, Attempt: 1},
			code.QueueDispatch)
		req.Queued.Def = &code.CheckDef{Name: c.name, Run: "make " + c.name, TimeoutSeconds: c.timeout}
		res, err := w.Publish(t.Context(), req)
		requireChecked(t, "the attempt of "+c.name, res, err)
	}
	res, err := w.Publish(t.Context(), claiming(code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef,
		Name: "vet", Attempt: 1}, "git-a", ""))
	requireChecked(t, "vet's claim", res, err)

	got, err := n.reader.ChecksBacklog(t.Context(), harnessAt.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("the backlog: %v", err)
	}
	want := "web@" + oidOf("B")[:12] + " for main: lint (attempt 1)"
	if got.Behind != 1 || got.Check != want || got.Oldest != 2*time.Minute {
		t.Fatalf("two minutes on the backlog is %+v, want lint's one attempt, 2m past its queueing, as %q", got,
			want)
	}
	if got, err := n.reader.ChecksBacklog(t.Context(), harnessAt.Add(30*time.Second)); err != nil ||
		got != (code.ChecksBacklog{}) {
		t.Fatalf("thirty seconds on the backlog is (%+v, %v), want none behind", got, err)
	}
}

// A CHECK'S WRITE IS DECIDED ONLY FROM ROWS ITS NODE HOLDS CURRENT: a check's
// record writes only its repository's checks, and its decide reads the
// repository's row on every arm and the change a head's write is about, so a
// node holding a record it cannot decode that covers either answers a check
// queued at a head, a seat's rerun (its grant and the change's authors), a
// dispatch, and a failing head's ending and a person's CI's failing report of
// it (each routed to the change's authors) unavailable — the framework's own
// `deferred`, ErrNotServing to the router,
// which asks a node whose rows are current — and appends nothing. A record
// about the repository covers every write of its checks; one about a change
// alone covers the writes that read that change, a change found only in the
// decide's snapshot (a rerun's, a failed head's) included, and leaves a check
// of the repository's tip to be decided. And the records name what their
// decides read: a head's queue names the change it was asked about, and a
// failed head's ending the change its decide routed it to.
//
// Mutations: scope a check's record by its checks alone ({ChecksPath}) — every
// write over the repository's record, the queue and the dispatch over the
// change's, are decided and appended, red; probe no change a decide finds in
// its snapshot — the seat's rerun, the failing ending and the CI's failing
// report over the change's record are decided, red; name no such change on
// the record — the failed
// head's ending names its repository and checks alone, red.
func TestAChecksWriteIsUnavailableWhereARecordItReadsIsDeferred(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		subject func(repo, change string) (subject, kind, id string)
		path    func(repo, change string) string
		// tipDecided is whether a check of the repository's tip, which
		// reads no change, is decided beside the deferred record.
		tipDecided bool
	}{
		"a record about the repository": {
			subject: func(repo, _ string) (string, string, string) {
				return "crewlet.code.log.repo." + repo, "repo", repo
			},
			path: func(repo, _ string) string { return code.RepoPath(repo) },
		},
		"a record about one change alone": {
			subject: func(_, change string) (string, string, string) {
				return "crewlet.code.log.change." + change, "change", change
			},
			path:       func(_, change string) string { return code.ChangePath(change) },
			tipDecided: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			n := newNode(t)
			repo, change := discussed(t, n, "web")
			w, counter, _, _ := checksWriterOn(t, n)
			head := func(name string) code.CheckKey {
				return code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: name, Attempt: 1}
			}
			queuedAtHead := func(key code.CheckKey) code.ChecksRequest {
				req := queueing(key, code.QueueDispatch)
				req.Queued.Purpose, req.Queued.ChangeID, req.Queued.Revision = code.PurposeHead, change, 1
				return req
			}
			scopeOf := func(what string, at statelog.Position, want ...string) {
				t.Helper()
				if got := []string(n.recordAt(at).Scope); !slices.Equal(got, want) {
					t.Fatalf("%s's record is scoped %v, want %v: what its decide read", what, got, want)
				}
			}
			repoAndChange := []string{code.RepoPath(repo), code.ChecksPath(repo), code.ChangePath(change)}

			// lint, a head check its box failed: a seat's rerun reads the
			// change's authors.
			lint := head("lint")
			res, err := w.Publish(ctx, queuedAtHead(lint))
			requireChecked(t, "lint queued at the head", res, err)
			scopeOf("lint's queue at the head", res.Position, repoAndChange...)
			res, err = w.Publish(ctx, claiming(lint, "git-1", ""))
			requireChecked(t, "lint claimed", res, err)
			res, err = w.Publish(ctx, ending(lint, code.CheckStateErrored, code.CheckReasonBoxFailed))
			requireChecked(t, "lint errored", res, err)
			// vet failed at the head before anything was deferred: its
			// ending is routed to the change its decide found.
			vet := head("vet")
			res, err = w.Publish(ctx, queuedAtHead(vet))
			requireChecked(t, "vet queued at the head", res, err)
			res, err = w.Publish(ctx, claiming(vet, "git-1", ""))
			requireChecked(t, "vet claimed", res, err)
			res, err = w.Publish(ctx, ending(vet, code.CheckStateFailed, code.CheckReasonExit))
			requireChecked(t, "vet failed", res, err)
			scopeOf("vet's failure at the head", res.Position, repoAndChange...)
			// e2e runs at the head: its failure is routed to the change.
			e2e := head("e2e")
			res, err = w.Publish(ctx, queuedAtHead(e2e))
			requireChecked(t, "e2e queued at the head", res, err)
			res, err = w.Publish(ctx, claiming(e2e, "git-1", ""))
			requireChecked(t, "e2e claimed", res, err)

			subject, kind, id := tc.subject(repo, change)
			n.exec(`INSERT INTO code_log_deferred (position, subject, subject_kind, subject_id,
				version, payload, stored_at) VALUES (999999, ?, ?, ?, 9, x'7b7d', 0)`, subject, kind, id)
			n.exec(`INSERT INTO code_log_deferred_scope (position, path) VALUES (999999, ?)`,
				tc.path(repo, change))

			tipOID, tipAt := n.tip(repo, mainRef)
			appended, end := counter.appended.Load(), n.end()
			for what, req := range map[string]code.ChecksRequest{
				"a check queued at the change's head":  queuedAtHead(head("docs")),
				"a seat's rerun of the change's check": rerunning(lint, dev),
				"a dispatch of the change's head": dispatching(repo, tipOID, tipAt, oidOf("checks-file"),
					code.DispatchedHead{OID: oidOf("B"), ChangeID: change, Revision: 1}),
				"a failing ending of the change's check": ending(e2e, code.CheckStateFailed, code.CheckReasonExit),
				"a person's CI failing the change's head": reporting(head("ci"), code.CheckStateFailed,
					"https://ci.example.com/1"),
			} {
				res, err := w.Publish(ctx, req)
				var refusal *statelog.Unavailable
				if !errors.Is(err, code.ErrNotServing) || !errors.As(err, &refusal) ||
					refusal.Reason != statelog.ReasonDeferred || refusal.Position.IsZero() {
					t.Errorf("%s beside the deferred record answered (%+v, %v), want ErrNotServing wrapping "+
						"the log's deferred refusal, naming its position", what, res, err)
				}
			}
			if got, at := counter.appended.Load(), n.end(); got != appended || at != end {
				t.Fatalf("the refused writes appended %d records and moved the log from %d to %d, want none",
					got-appended, end, at)
			}

			res, err = w.Publish(ctx, queueing(checkOn(repo, 1), code.QueueDispatch))
			switch {
			case tc.tipDecided:
				requireChecked(t, "a check of the tip, about no change", res, err)
				scopeOf("the tip's queue", res.Position, code.RepoPath(repo), code.ChecksPath(repo))
			case !errors.Is(err, code.ErrNotServing):
				t.Fatalf("a check of the tip beside a deferred record about its repository answered (%+v, %v), "+
					"want ErrNotServing", res, err)
			}
		})
	}
}
