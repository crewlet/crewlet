package gitlet_test

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// checkWork is every change of repo the home owes check work, as the
// `checks_due` read of the repository answers, and whether the listing of
// repositories names repo.
func checkWork(t *testing.T, n *node, repo string) (map[string]gitlet.CheckWork, bool) {
	t.Helper()
	repos, heads, _ := checksDue(t, n, repo, harnessAt)
	out := map[string]gitlet.CheckWork{}
	for _, c := range heads {
		if c.Checks == nil {
			t.Fatalf("a checks_due read named change %s with no check work", c.ID)
		}
		out[c.ID] = *c.Checks
	}
	return out, slices.Contains(repos, repo)
}

// headCheck is a dispatch of change's head's check name for main: attempt 1
// at oidOf head, revision revision of the change, under the suite's
// definition of name.
func headCheck(repo, change string, revision int, head, name string) gitlet.ChecksRequest {
	key := gitlet.CheckKey{RepoID: repo, OID: oidOf(head), Target: mainRef, Name: name, Attempt: 1}
	req := queueing(key, gitlet.QueueDispatch)
	req.Queued.Purpose, req.Queued.ChangeID, req.Queued.Revision = gitlet.PurposeHead, change, revision
	return req
}

// dispatchHead records change's head, revision revision at oidOf head,
// dispatched for main at the tip its listing names, as the home records it
// once it queued what the head owes.
func dispatchHead(t *testing.T, n *node, w *gitlet.ChecksWriter, repo, change string, revision int, head string) {
	t.Helper()
	_, heads, _ := checksDue(t, n, repo, harnessAt)
	i := slices.IndexFunc(heads, func(c gitlet.ChangeDue) bool { return c.ID == change })
	if i < 0 {
		t.Fatalf("change %s is not due its dispatch: %+v", change, heads)
	}
	res, err := w.Publish(t.Context(), dispatching(repo, heads[i].Tip, heads[i].TipPosition, oidOf("checks-file"),
		gitlet.DispatchedHead{OID: oidOf(head), ChangeID: change, Revision: revision}))
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the head's dispatch answered (%+v, %v), want applied", res, err)
	}
}

// gatedRepo is a repository named name whose main's rule carries a gate
// requiring lint and unit, its refs the suite's, and a change of its feature
// branch into main opened on it.
func gatedRepo(t *testing.T, n *node, name string) (repo, change string) {
	t.Helper()
	repo = n.ensure(name, gatedSettings(aGate()), t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(featureRef, gitlet.ZeroOID, oidOf("B"))))
	c, _ := committerOn(t, n, n.pub)
	opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	return repo, opened.ChangeID
}

// A DUE HEAD SAYS WHAT ITS DISPATCH OWES: a change whose head is due its
// dispatch is listed with the names its target's gate requires — main's lint
// and unit — and with what its head already holds for main: a name queued is
// HELD, its dispatch queuing nothing of it again, and a name errored at its
// dispatch for a checks file that did not run is INVALID, as that attempt's
// key, which a dispatch under a file that runs follows with the next attempt.
// Its dispatch recorded, it owes nothing.
//
// Mutation: read an attempt errored at its dispatch as held — the mended
// file's dispatch never runs lint, red.
func TestADueHeadSaysWhatItsDispatchOwes(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := gatedRepo(t, n, "web")
	w, _, _, _ := checksWriterOn(t, n)
	work, listed := checkWork(t, n, repo)
	got, ok := work[change]
	if !ok || !listed || !got.Dispatch || !slices.Equal(got.Required, []string{"lint", "unit"}) ||
		len(got.Held)+len(got.Invalid)+len(got.Stale)+len(got.Requeue) != 0 {
		t.Fatalf("a head never dispatched owes %+v (listed %t), want its dispatch, lint and unit required", work,
			listed)
	}
	res, err := w.Publish(t.Context(), headCheck(repo, change, 1, "B", "unit"))
	requireChecked(t, "unit at the head", res, err)
	lint := headCheck(repo, change, 1, "B", "lint")
	lint.Queued.Def, lint.Queued.Required, lint.Queued.Invalid = nil, true, "the checks file is not YAML"
	res, err = w.Publish(t.Context(), lint)
	requireChecked(t, "lint errored at its dispatch", res, err)
	if res.Row.State != gitlet.CheckStateErrored || res.Row.Reason != gitlet.CheckReasonDefsInvalid ||
		res.Row.Summary != "the checks file is not YAML" {
		t.Fatalf("lint errored at its dispatch is %+v, want errored defs_invalid saying why", res.Row)
	}
	work, listed = checkWork(t, n, repo)
	got = work[change]
	want := gitlet.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "lint", Attempt: 1}
	if !listed || !got.Dispatch || !slices.Equal(got.Held, []string{"unit"}) ||
		!slices.Equal(got.Invalid, []gitlet.CheckKey{want}) {
		t.Fatalf("a head holding unit queued and lint errored at its dispatch owes %+v (listed %t), want unit "+
			"held and lint's attempt 1 invalid", work, listed)
	}
	dispatchHead(t, n, w, repo, change, 1, "B")
	if work, listed = checkWork(t, n, repo); len(work) != 0 || listed {
		t.Fatalf("a dispatched head still owes %+v (listed %t)", work, listed)
	}
}

// A DUE HEAD CARRIES ITS RECORDED FACTS: a head owing its dispatch is listed
// with no facts while its head revision holds none for its target — its
// dispatch waits — and, once the change work recorded them, with exactly what
// they say it touches, read in the listing's own snapshot: what a `paths`
// check is matched against, with no git read of the home's.
//
// Mutation: answer every head with no facts — the recorded revision is listed
// without them, and its dispatch waits for ever, red.
func TestADueHeadCarriesItsRecordedFacts(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := gatedRepo(t, n, "web")
	cw, _ := writerOn(t, n, n.pub)
	work, _ := checkWork(t, n, repo)
	if got, ok := work[change]; !ok || !got.Dispatch || got.Facts != nil {
		t.Fatalf("a head whose revision holds no facts owes %+v, want its dispatch owed and no facts", work)
	}
	converse(t, cw, n.revising(repo, change, 1, oidOf("B"), mainRef, nil, "cmd/main.go", "docs/a.md"))
	work, _ = checkWork(t, n, repo)
	got, ok := work[change]
	if !ok || !got.Dispatch || got.Facts == nil || got.Facts.TouchesAll ||
		!slices.Equal(got.Facts.Touched, []string{"cmd/main.go", "docs/a.md"}) {
		t.Fatalf("a head whose revision's facts are recorded owes %+v (facts %+v), want its dispatch owed "+
			"carrying cmd/main.go and docs/a.md", got, got.Facts)
	}
}

// AN INFRASTRUCTURE'S ERROR IS QUEUED AGAIN BELOW ITS BOUND: a dispatched
// head's attempt its runner lost lists its change owing the next attempt —
// for the same target, under the definition it ran under — until that
// attempt is queued; one its box failed at the second attempt likewise; and
// at gitlet.AutoRequeueAttempts the home queues it no more: the change owes
// nothing, and only a rerun runs it again.
//
// Mutation: drop the requeue index's attempt bound from the listing — the
// third attempt's error lists the change again, red.
func TestAnInfrastructuresErrorIsQueuedAgainBelowItsBound(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _, _, _ := checksWriterOn(t, n)
	res, err := w.Publish(t.Context(), headCheck(repo, change, 1, "B", "unit"))
	requireChecked(t, "the head's check", res, err)
	dispatchHead(t, n, w, repo, change, 1, "B")
	key := gitlet.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: 1}
	for _, reason := range []gitlet.CheckReason{gitlet.CheckReasonRunnerLost, gitlet.CheckReasonBoxFailed,
		gitlet.CheckReasonSetupFailed} {
		for _, req := range []gitlet.ChecksRequest{claiming(key, "git-a", ""),
			ending(key, gitlet.CheckStateErrored, reason)} {
			res, err := w.Publish(t.Context(), req)
			requireChecked(t, "attempt "+key.Name, res, err)
		}
		work, listed := checkWork(t, n, repo)
		if key.Attempt >= gitlet.AutoRequeueAttempts {
			if len(work) != 0 || listed {
				t.Fatalf("attempt %d errored %s owes %+v (listed %t), want nothing: the home's requeue ends at "+
					"attempt %d", key.Attempt, reason, work, listed, gitlet.AutoRequeueAttempts)
			}
			return
		}
		got := work[change]
		if !listed || got.Dispatch || len(got.Requeue) != 1 {
			t.Fatalf("attempt %d errored %s owes %+v (listed %t), want its next attempt alone", key.Attempt,
				reason, work, listed)
		}
		next := got.Requeue[0]
		if next.Attempt != key.Attempt+1 || next.Target != mainRef || next.Reason != gitlet.QueueRequeue ||
			next.Def == nil || next.Def.Run != "make unit" || next.ChangeID != change ||
			next.Purpose != gitlet.PurposeHead {
			t.Fatalf("attempt %d's requeue is %+v, want attempt %d of the same check for main, its definition "+
				"and its change's", key.Attempt, next, key.Attempt+1)
		}
		next.QueuedAt = harnessAt
		res, err := w.Publish(t.Context(), gitlet.ChecksRequest{OpID: statelog.NewOpID(harnessAt, "code_check_queue"),
			Queued: &next})
		requireChecked(t, "the requeue", res, err)
		if work, listed := checkWork(t, n, repo); len(work) != 0 || listed {
			t.Fatalf("a requeued attempt still owes %+v (listed %t)", work, listed)
		}
		key.Attempt++
	}
	t.Fatal("the home queued a check past gitlet.AutoRequeueAttempts")
}

// A CHANGE'S ATTEMPTS AT A HEAD IT MOVED ON FROM ARE STALE: once web's change
// moves from B to C, its dispatch owes the cancellation of its attempts still
// queued at B — but not of those at C, its head now — and once another live
// change into main heads B, none: that change shares them.
//
// Mutation: list a change's attempts at its own head as stale — C's unit is
// cancelled, red; leave out the live change sharing a commit — B's unit is
// cancelled under the change that heads it, red.
func TestAChangesAttemptsAtAHeadItMovedOnFromAreStale(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _, _, _ := checksWriterOn(t, n)
	res, err := w.Publish(t.Context(), headCheck(repo, change, 1, "B", "unit"))
	requireChecked(t, "unit at B", res, err)
	dispatchHead(t, n, w, repo, change, 1, "B")
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("B"), oidOf("C"))))
	res, err = w.Publish(t.Context(), headCheck(repo, change, 2, "C", "unit"))
	requireChecked(t, "unit at C", res, err)
	work, _ := checkWork(t, n, repo)
	atB := gitlet.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: 1}
	if got := work[change]; !got.Dispatch || !slices.Equal(got.Stale, []gitlet.CheckKey{atB}) {
		t.Fatalf("a change moved from B to C owes %+v, want its dispatch and its unit at B stale alone", got)
	}
	other := "refs/heads/other"
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(other, gitlet.ZeroOID, oidOf("B"))))
	c, _ := committerOn(t, n, n.pub)
	opened, err := c.ProposeChange(t.Context(), opening(repo, other, mainRef, dev))
	requireChanged(t, opened, err)
	work, _ = checkWork(t, n, repo)
	if got := work[change]; !got.Dispatch || len(got.Stale) != 0 {
		t.Fatalf("with another live change into main heading B, the moved change owes %+v, want its dispatch "+
			"and nothing stale", got)
	}
}

// A FINISHED HEAD CHECK MARKS THE CHANGE DUE: a change whose facts, merge and
// readiness are recorded owes its home nothing — an attempt at its head for
// ANOTHER branch moves nothing it reads — until an attempt of its head's
// checks for its own target is queued, and again once one ends: each moves
// the head's checks past the position its stored readiness was judged at, so
// the due listing names it owing a REFRESH, which is RECORDED above the
// stored row, stamping the new position — never refused superseded, as one
// judged at the inputs already is — and the change owes nothing more. A
// refresh asked again with nothing moved is superseded.
//
// Mutation: drop the check term from the due listing — the ending lists
// nothing, red; drop the target from it — release's attempt lists the change,
// red; drop the checks position from the refresh's guard — the refresh after
// the ending is refused superseded, red.
func TestAFinishedHeadCheckMarksTheChangeDue(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	checks, _, _, _ := checksWriterOn(t, n)
	due := func() []gitlet.ChangeDue {
		t.Helper()
		a, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadChangesDue, Fresh: stale,
			ChangesDue: &gitlet.ChangesDueQuery{RepoID: repo, RecheckBefore: harnessAt}})
		if err != nil {
			t.Fatalf("Read(changes_due): %v", err)
		}
		return a.DueChanges
	}
	stored := func() int64 {
		t.Helper()
		var at int64
		if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `SELECT checks_position FROM code_mergeability
				WHERE change_id = ?`, change).Scan(&at)
		}); err != nil {
			t.Fatalf("read %s's checks position: %v", change, err)
		}
		return at
	}
	refresh := func() gitlet.ConversationResult {
		t.Helper()
		return converse(t, w, gitlet.ConversationWrite{OpID: statelog.NewOpID(harnessAt, ""),
			Mergeability: &gitlet.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"),
				TargetRef: mainRef, Refresh: true, Node: n.id}})
	}
	converse(t, w, n.patched(repo, change, 1, "B", oidOf("the retry diff")))
	converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	if got := due(); len(got) != 0 || stored() != 0 {
		t.Fatalf("a change merged before any check owes %+v, its checks at %d; want nothing, at none", got,
			stored())
	}
	forRelease := queueing(gitlet.CheckKey{RepoID: repo, OID: oidOf("B"), Target: releaseRef, Name: "unit",
		Attempt: 1}, gitlet.QueueDispatch)
	res, err := checks.Publish(t.Context(), forRelease)
	requireChecked(t, "unit at the head for release", res, err)
	if got := due(); len(got) != 0 {
		t.Fatalf("an attempt at the head for release marks %+v due, want nothing: the change into main's "+
			"gate reads main's", got)
	}
	key := gitlet.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: 1}
	for _, step := range []struct {
		what string
		req  gitlet.ChecksRequest
	}{
		{"queued", headCheck(repo, change, 1, "B", "unit")},
		{"claimed", claiming(key, "git-a", "")},
		{"finished", ending(key, gitlet.CheckStatePassed, gitlet.CheckReasonExit)},
	} {
		res, err := checks.Publish(t.Context(), step.req)
		requireChecked(t, "the check "+step.what, res, err)
		got := due()
		if step.what == "claimed" {
			// A CLAIM MOVES NEITHER POSITION: running is computing, as
			// queued is, and no stored answer turns on it.
			if len(got) != 0 {
				t.Fatalf("a claimed check marks %+v due, want nothing", got)
			}
			continue
		}
		if len(got) != 1 || got[0].ID != change || !got[0].RefreshDue || got[0].FactsDue || got[0].MergeDue {
			t.Fatalf("after its head's check %s the due listing names %+v, want web!1 owing a refresh alone",
				step.what, got)
		}
		refreshed := refresh()
		if at := stored(); at != res.Position.Packed() || refreshed.Position.IsZero() {
			t.Fatalf("the refresh after the check %s stored its checks at %d, want the check's record's %d",
				step.what, at, res.Position.Packed())
		}
		if got := due(); len(got) != 0 {
			t.Fatalf("a refreshed change is still due %+v", got)
		}
	}
	before := n.end()
	res2, err := w.Converse(t.Context(), gitlet.ConversationWrite{OpID: statelog.NewOpID(harnessAt, ""),
		Mergeability: &gitlet.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"),
			TargetRef: mainRef, Refresh: true, Node: n.id}})
	requireConversationRefused(t, n, before, res2, err, gitlet.ConversationRefusalSuperseded, "already")
}
