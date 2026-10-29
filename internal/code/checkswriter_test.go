package code_test

import (
	"context"
	"errors"
	"slices"
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
		Events:    stream, Now: func() time.Time { return harnessAt }, Metrics: rec,
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
	return code.CheckKey{RepoID: repo, OID: oidOf("B"), Name: "unit", Attempt: attempt}
}

// unitDef is the definition every case queues its attempts under.
var unitDef = &code.CheckDef{Name: "unit", Run: "make test", TimeoutSeconds: 600}

// queueing asks for attempt key queued for reason, under an operation id of
// its own.
func queueing(key code.CheckKey, reason code.QueueReason) code.ChecksRequest {
	return code.ChecksRequest{OpID: statelog.NewOpID(time.Now(), "code_check_queue"),
		Queued: &code.CheckQueued{CheckKey: key, Purpose: code.PurposeTip, Target: mainRef, Def: unitDef,
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

// AN ATTEMPT FOLLOWS THE ONE BEFORE IT, ENDED: a dispatch is a commit's first
// attempt, answered from its row when it is queued already; the next is queued
// only once the one before it ended, and the home's requeue only after its
// infrastructure ended it and while that attempt is below
// AutoRequeueAttempts; and no attempt passes MaxCheckAttempts.
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
	other := code.CheckKey{RepoID: repo, OID: oidOf("A"), Name: "unit", Attempt: 1}
	for _, req := range []code.ChecksRequest{queueing(other, code.QueueDispatch), claiming(other, "git-a", ""),
		ending(other, code.CheckStateFailed, code.CheckReasonExit)} {
		res, err = w.Publish(ctx, req)
		requireChecked(t, "the failing attempt", res, err)
	}
	other.Attempt = 2
	res, err = w.Publish(ctx, queueing(other, code.QueueRequeue))
	requireCheckRefused(t, "a requeue of a failure", res, err, code.CheckRefusalRerunGenuine)

	// NO ATTEMPT PASSES THE LAST: one held at MaxCheckAttempts, which no
	// writer of this build reaches but by reruns, is followed by none.
	last := code.CheckKey{RepoID: repo, OID: oidOf("R"), Name: "unit", Attempt: code.MaxCheckAttempts}
	n.exec(`INSERT INTO code_checks (repo_id, commit_oid, name, attempt, state, purpose, change_id, revision,
		candidate_id, entry_id, target_ref, base_oid, defs_oid, required, run_in, timeout_seconds, reason,
		runner_node, placement, box_handle, borrowed, reporter, queued_at, started_at, finished_at,
		duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, url, summary,
		def, failures, tail)
		VALUES (?, ?, ?, ?, 'errored', 'tip', '', 0, '', '', 'refs/heads/main', '', '', 0, '', 600,
			'runner_lost', 'git-a', 'container', '', 0, '', ?, ?, ?, 0, NULL, 0, 1, 2, 0, '', '', '', '', '')`,
		repo, last.OID, last.Name, last.Attempt, store.EncodeTime(harnessAt), store.EncodeTime(harnessAt),
		store.EncodeTime(harnessAt))
	past := last
	past.Attempt++
	res, err = w.Publish(ctx, queueing(past, code.QueueRequeue))
	requireCheckRefused(t, "an attempt past MaxCheckAttempts", res, err, code.CheckRefusalAttemptsExhausted)
	if !strings.Contains(res.Detail, "code.MaxCheckAttempts") {
		t.Fatalf("an attempt past the last was refused %q, want the bound named", res.Detail)
	}
}

// A WRITER DECIDES THE ENGINE'S OWN RECORDS: a rerun and a report name a
// principal, whose grant this writer holds no chart to judge, so each is no
// request it decides — refused invalid before anything is read — and an
// ending of an attempt no runner claimed, which no runner forms, likewise.
func TestAChecksWriterDecidesOnlyTheEnginesOwnRecords(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := changeRepo(t, n, "web")
	w, counter, _, _ := checksWriterOn(t, n)
	key := checkOn(repo, 1)
	person := code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:ada", Seat: "founder"}
	rerun := queueing(key, code.QueueRerun)
	rerun.Queued.By = &dev
	report := queueing(key, code.QueueReport)
	report.Queued.By, report.Queued.Def = &person, nil
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
// the ending it set a second time.
//
// Mutation: count a collapsed retry's record as the call's own, and the
// dispatch asked again counts its attempt twice — red.
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
	// THE STOP WAITS FOR EVERY ANNOUNCEMENT IN FLIGHT, so what the stream
	// holds after it is all the writer ever announced.
	w.Stop(t.Context())
	if all := stream.all(); len(all) != 1 {
		t.Fatalf("the attempt's ending was announced %d times, want once", len(all))
	}
	if got := readingOf(rec, metrics.CodeChecksFinished, metrics.Attrs{"state": "failed",
		"placement": "container"}).Total; got != 1 {
		t.Fatalf("crewlet.code.checks.finished{state=failed,placement=container} = %v, want 1", got)
	}
	if got := readingOf(rec, metrics.CodeChecksQueued, metrics.Attrs{"purpose": "tip"}).Total; got != 1 {
		t.Fatalf("crewlet.code.checks.queued{purpose=tip} = %v after a dispatch and its retry, want 1", got)
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
// would be held to nothing — and lists a live change whose head no attempt of
// any check was queued at, until one is.
//
// Mutation: let the read take an asker, and a person's read of the listing is
// answered — red.
func TestChecksDueRefusesAnAsker(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	for name, q := range map[string]code.ReadQuery{
		"a person's": {Kind: code.ReadChecksDue, Asker: ada, Fresh: fresh},
		"a seat's":   {Kind: code.ReadChecksDue, Asker: dev, Fresh: fresh},
		"one made for a seat": {Kind: code.ReadChecksDue, CeilingSeat: "cto",
			ChecksDue: &code.ChecksDueQuery{RepoID: repo}, Fresh: fresh},
	} {
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("%s read of the due check work answered %v, want code.ErrInvalid", name, err)
		}
	}
	listed := func() ([]string, []code.ChangeDue) {
		t.Helper()
		repos, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadChecksDue, Fresh: fresh})
		if err != nil {
			t.Fatalf("the due repositories: %v", err)
		}
		heads, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadChecksDue,
			ChecksDue: &code.ChecksDueQuery{RepoID: repo}, Fresh: fresh})
		if err != nil {
			t.Fatalf("the due heads: %v", err)
		}
		return repos.DueRepos, heads.DueChanges
	}
	repos, heads := listed()
	if !slices.Equal(repos, []string{repo}) || len(heads) != 1 || heads[0].ID != change ||
		heads[0].Head != oidOf("B") || heads[0].Tip != oidOf("A") {
		t.Fatalf("the due check work is %v and %+v, want web and its change's head B against main's A", repos,
			heads)
	}
	w, _, _, _ := checksWriterOn(t, n)
	head := code.CheckKey{RepoID: repo, OID: oidOf("B"), Name: "unit", Attempt: 1}
	req := queueing(head, code.QueueDispatch)
	req.Queued.Purpose, req.Queued.ChangeID, req.Queued.Revision = code.PurposeHead, change, 1
	res, err := w.Publish(t.Context(), req)
	requireChecked(t, "the head's dispatch", res, err)
	if repos, heads := listed(); len(repos) != 0 || len(heads) != 0 {
		t.Fatalf("a head with a queued attempt is still due: %v, %+v", repos, heads)
	}
}

// A REPOSITORY'S CHECKS ARE READ BY EVERY SHAPE: a commit's attempts, a
// change's, the repository's newest ended first and the window's — each a
// keyset that resumes where its page ended — and one attempt whole, with the
// failures its ending named; and a reader the repository's grant shuts out
// reads none of them.
func TestARepositorysChecksAreReadByEveryShape(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _, _, _ := checksWriterOn(t, n)
	ctx := t.Context()
	for i, name := range []string{"lint", "unit", "vet"} {
		key := code.CheckKey{RepoID: repo, OID: oidOf("B"), Name: name, Attempt: 1}
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
	names := func(rows []code.CheckRow) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Name)
		}
		return out
	}
	page := func(q code.ReadQuery) []string {
		t.Helper()
		var all []string
		for {
			got, err := n.reader.Read(ctx, q)
			if err != nil {
				t.Fatalf("read %+v: %v", q.Checks, err)
			}
			all = append(all, names(got.Checks)...)
			if got.Next == "" {
				return all
			}
			q.Cursor = got.Next
		}
	}
	for name, tc := range map[string]struct {
		q    code.ReadQuery
		want []string
	}{
		"a commit's": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web", Limit: 1, Fresh: stale,
			Checks: &code.CheckQuery{OID: oidOf("B")}}, []string{"lint", "unit", "vet"}},
		"a change's": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web", Limit: 2, Fresh: stale,
			Checks: &code.CheckQuery{Change: 1}}, []string{"lint", "unit", "vet"}},
		"a change's at its head": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web", Limit: 1,
			Fresh: stale, Checks: &code.CheckQuery{Change: 1, OID: oidOf("B")}}, []string{"lint", "unit", "vet"}},
		"the repository's, newest ended first": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Repo: "web",
			Limit: 1, Fresh: stale}, []string{"unit", "lint", "vet"}},
		"the window's": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Limit: 1, Fresh: stale,
			Checks: &code.CheckQuery{Since: harnessAt}}, []string{"unit", "lint"}},
		"the window past the first ending": {code.ReadQuery{Kind: code.ReadChecks, Asker: ada, Fresh: stale,
			Checks: &code.CheckQuery{Since: harnessAt.Add(90 * time.Second)}}, []string{"unit"}},
	} {
		if got := page(tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("%s checks read %v, want %v", name, got, tc.want)
		}
	}
	one, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadCheck, Asker: ada, Repo: "web", Fresh: stale,
		Checks: &code.CheckQuery{OID: oidOf("B"), Name: "unit", Attempt: 1}})
	if err != nil {
		t.Fatalf("one attempt: %v", err)
	}
	if c := one.Check; c == nil || c.State != code.CheckStateFailed || c.Def == nil || c.Def.Run != "make unit" ||
		c.Tail != "--- FAIL" || len(c.Failures) != 1 || c.Failures[0].TestID != "TestA" {
		t.Fatalf("one attempt read %+v, want unit's failure whole", one.Check)
	}
	if _, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadCheck, Asker: ada, Repo: "web", Fresh: stale,
		Checks: &code.CheckQuery{OID: oidOf("B"), Name: "unit", Attempt: 2}}); !errors.Is(err,
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
		req := queueing(code.CheckKey{RepoID: repo, OID: oidOf("B"), Name: c.name, Attempt: 1}, code.QueueDispatch)
		req.Queued.Def = &code.CheckDef{Name: c.name, Run: "make " + c.name, TimeoutSeconds: c.timeout}
		res, err := w.Publish(t.Context(), req)
		requireChecked(t, "the attempt of "+c.name, res, err)
	}
	res, err := w.Publish(t.Context(), claiming(code.CheckKey{RepoID: repo, OID: oidOf("B"), Name: "vet", Attempt: 1},
		"git-a", ""))
	requireChecked(t, "vet's claim", res, err)

	got, err := n.reader.ChecksBacklog(t.Context(), harnessAt.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("the backlog: %v", err)
	}
	want := "web@" + oidOf("B")[:12] + ": lint (attempt 1)"
	if got.Behind != 1 || got.Check != want || got.Oldest != 2*time.Minute {
		t.Fatalf("two minutes on the backlog is %+v, want lint's one attempt, 2m past its queueing, as %q", got,
			want)
	}
	if got, err := n.reader.ChecksBacklog(t.Context(), harnessAt.Add(30*time.Second)); err != nil ||
		got != (code.ChecksBacklog{}) {
		t.Fatalf("thirty seconds on the backlog is (%+v, %v), want none behind", got, err)
	}
}
