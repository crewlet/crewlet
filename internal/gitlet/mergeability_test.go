package gitlet_test

import (
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A CHANGE'S MERGEABILITY THROUGH THE CHANGE WRITER: the home's record of merging
// a change's head into its target's tip, written as the home itself (no asker) on
// the change's own subject over a real broker and the shipped applier, and the
// due listing that asks it again once the target moves.

// merging is the home's write of change's merge — revision rev, its head
// head, into target at its tip as n's rows hold it — that state, naming
// conflicts.
func (n *node) merging(repo, change string, rev int64, head, target string, state gitlet.MergeState,
	conflicts ...string) gitlet.ConversationWrite {

	n.t.Helper()
	tip, at := n.tip(repo, target)
	return gitlet.ConversationWrite{OpID: statelog.NewOpID(time.Now(), ""), Mergeability: &gitlet.Mergeability{
		RepoID: repo, ChangeID: change, Revision: rev, HeadOID: head, TargetRef: target, TargetOID: tip,
		TargetPosition: at, State: state, Conflicts: conflicts, Node: n.id}}
}

// heldMerge is a change's mergeability as n's rows hold it.
type heldMerge struct {
	revision               int64
	target, tip            string
	targetAt, position, at int64
	state                  gitlet.MergeState
	conflicts              []string
	count, omitted, bytes  int64
	held                   bool
	conversationBytes      int64
}

// mergeability is change's mergeability as n's rows hold it, held false where
// there is none, beside what its conversation's text weighs.
func (n *node) mergeability(change string) heldMerge {
	n.t.Helper()
	var m heldMerge
	var state, conflicts string
	err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(n.t.Context(), `SELECT revision, target_ref, target_oid, target_position,
			state, conflicts_count, conflicts_omitted, bytes, position, at, conflicts FROM code_mergeability
			WHERE change_id = ?`, change).Scan(&m.revision, &m.target, &m.tip, &m.targetAt, &state, &m.count,
			&m.omitted, &m.bytes, &m.position, &m.at, &conflicts); err != nil {
			return err
		}
		return tx.QueryRowContext(n.t.Context(), `SELECT text_bytes FROM code_conversations WHERE change_id = ?`,
			change).Scan(&m.conversationBytes)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return heldMerge{}
	case err != nil:
		n.t.Fatalf("read %s's mergeability: %v", change, err)
	}
	m.held, m.state = true, gitlet.MergeState(state)
	if conflicts != "" {
		m.conflicts = strings.Split(conflicts, "\x00")
	}
	return m
}

// A MERGEABILITY RECORD IS MONOTONE: taken only of the change's head revision
// and head into the branch it targets now (else superseded, nothing appended),
// and only forward in the target's tip: an earlier tip than the row's (a slower
// pass) is superseded, as is the same tip once the row is clean or conflict, a
// merge of two commits being decided once. Only an UNKNOWN row takes its tip
// again, its instant moving so the next try waits its home's refresh. A later
// tip, a new revision or a decision after an unknown replaces the one row, its
// conflicted paths weighed in the conversation's text and back on the change's read. A
// closed change takes no merge; an asker, or conflicts beside clean, no write.
//
// Mutations: drop the guard's position term → the earlier tip replaces the row;
// drop its decided term → the same tip merged again appends a second record.
func TestAMergeabilityRecordIsMonotone(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()

	res, err := w.Converse(ctx, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	requireConversed(t, res, err)
	if m := n.mergeability(change); !m.held || m.revision != 1 || m.target != mainRef || m.tip != oidOf("A") ||
		m.state != gitlet.MergeClean || m.position != res.Position.Packed() {
		t.Fatalf("the change's mergeability is %+v, want revision 1 clean into main at A, at %s", m, res.Position)
	}
	before := n.end()
	res, err = w.Converse(ctx, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeConflict, "a.go"))
	requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalSuperseded, "decided once")

	n.revise(repo, "B", "B2")
	for name, tc := range map[string]struct {
		w    gitlet.ConversationWrite
		says string
	}{
		"a merge of a revision the change has moved past": {
			n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean), "is at revision 2"},
		"a merge of a head the change does not have": {
			n.merging(repo, change, 2, oidOf("B3"), mainRef, gitlet.MergeClean), "is at revision 2"},
		"a merge into a branch the change does not merge into": {
			n.merging(repo, change, 2, oidOf("B2"), releaseRef, gitlet.MergeClean), "merges into"},
	} {
		before := n.end()
		res, err := w.Converse(ctx, tc.w)
		if err != nil || res.Refusal != gitlet.ConversationRefusalSuperseded || !strings.Contains(res.Detail, tc.says) ||
			n.end() != before {
			t.Errorf("%s answered %+v, %v; want superseded saying %q, nothing appended", name, res, err, tc.says)
		}
	}

	// AN UNDECIDED MERGE, AND ITS SAME TIP TRIED AGAIN: taken each time, the
	// row's instant moving with its record.
	res, err = w.Converse(ctx, n.merging(repo, change, 2, oidOf("B2"), mainRef, gitlet.MergeUnknown))
	requireConversed(t, res, err)
	undecided := n.mergeability(change)
	if undecided.revision != 2 || undecided.state != gitlet.MergeUnknown {
		t.Fatalf("the new revision's undecided merge left %+v, want revision 2 unknown", undecided)
	}
	res, err = w.Converse(ctx, n.merging(repo, change, 2, oidOf("B2"), mainRef, gitlet.MergeUnknown))
	requireConversed(t, res, err)
	// STILL UNDECIDED, the outcome the row holds: kept, so the row's instant
	// moves with the try and its position stays the record that decided it
	// (TestAReEvaluationThatKeepsTheOutcomeRecordsWhatItMoved).
	if again := n.mergeability(change); again.position != undecided.position || again.at <= undecided.at {
		t.Fatalf("the same tip tried again left %+v, want the instant past %+v's and its position", again,
			undecided)
	}
	res, err = w.Converse(ctx, n.merging(repo, change, 2, oidOf("B2"), mainRef, gitlet.MergeConflict,
		"upload/retry.go", "docs/README.md"))
	requireConversed(t, res, err)
	conflicted := n.mergeability(change)
	weight := int64(len("upload/retry.go\x00docs/README.md"))
	if conflicted.state != gitlet.MergeConflict || conflicted.count != 2 ||
		!slices.Equal(conflicted.conflicts, []string{"upload/retry.go", "docs/README.md"}) ||
		conflicted.bytes != weight || conflicted.conversationBytes != weight {
		t.Fatalf("the decided merge left %+v, want its two conflicted paths, weighing %d bytes in its "+
			"conversation's text", conflicted, weight)
	}
	before = n.end()
	res, err = w.Converse(ctx, n.merging(repo, change, 2, oidOf("B2"), mainRef, gitlet.MergeClean))
	requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalSuperseded, "decided once")

	// A LATER TIP REPLACES THE ROW, its paths' weight with it; the earlier
	// tip's merge, arriving after it, is refused.
	earlier := n.merging(repo, change, 2, oidOf("B2"), mainRef, gitlet.MergeConflict, "upload/retry.go")
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("A2"))))
	res, err = w.Converse(ctx, n.merging(repo, change, 2, oidOf("B2"), mainRef, gitlet.MergeClean))
	requireConversed(t, res, err)
	if m := n.mergeability(change); m.tip != oidOf("A2") || m.state != gitlet.MergeClean || len(m.conflicts) != 0 ||
		m.bytes != 0 || m.conversationBytes != 0 || m.targetAt <= conflicted.targetAt {
		t.Fatalf("the later tip's merge left %+v, want clean into A2 with no paths left weighed", m)
	}
	before = n.end()
	res, err = w.Converse(ctx, earlier)
	requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalSuperseded, "a later tip")
	if got := n.count(`SELECT COUNT(*) FROM code_mergeability WHERE change_id = ?`, change); got != 1 {
		t.Fatalf("the change holds %d mergeability rows, want its one", got)
	}

	// THE CHANGE'S READ carries what its home recorded.
	answer, err := readChange(t, n, ada, "", "web", 1, 0, "")
	if err != nil || answer.Change == nil || answer.Change.Mergeability == nil {
		t.Fatalf("the change read (%+v, %v) carries no mergeability", answer.Change, err)
	}
	if got := answer.Change.Mergeability; got.Revision != 2 || got.TargetRef != mainRef ||
		got.TargetOID != oidOf("A2") || got.State != gitlet.MergeClean || got.Node != n.id || got.At.IsZero() {
		t.Fatalf("the change read carries %+v, want revision 2's clean merge into main at A2", got)
	}

	invalid := n.merging(repo, change, 2, oidOf("B2"), mainRef, gitlet.MergeClean)
	invalid.Asker = dev
	if _, err := w.Converse(ctx, invalid); !errors.Is(err, gitlet.ErrInvalidProposal) ||
		!strings.Contains(err.Error(), "asker") {
		t.Fatalf("a mergeability naming an asker answered %v, want ErrInvalidProposal naming the asker", err)
	}
	for name, tc := range map[string]struct {
		w     gitlet.ConversationWrite
		field string
	}{
		"conflicts beside a clean merge": {
			n.merging(repo, change, 2, oidOf("B2"), mainRef, gitlet.MergeClean, "a.go"), "mergeability.conflicts"},
		"a conflicted path named twice": {
			n.merging(repo, change, 2, oidOf("B2"), mainRef, gitlet.MergeConflict, "a.go", "a.go"),
			"mergeability.conflicts[1]"},
		"a state no writer forms": {
			n.merging(repo, change, 2, oidOf("B2"), mainRef, "maybe"), "mergeability.state"},
	} {
		if _, err := w.Converse(ctx, tc.w); !errors.Is(err, gitlet.ErrInvalidProposal) ||
			!strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s answered %v, want ErrInvalidProposal naming %s", name, err, tc.field)
		}
	}

	c, _ := committerOn(t, n, n.pub)
	closed, err := c.ProposeChange(ctx, closing(repo, change, dev, n.change(change).version))
	requireChanged(t, closed, err)
	before = n.end()
	res, err = w.Converse(ctx, n.merging(repo, change, 2, oidOf("B2"), mainRef, gitlet.MergeClean))
	requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalChangeClosed, "closed")
}

// dueAt is one listing of repo's due change work at recheck — its changes owing
// facts or a merge, every page, and the rechecks its first page names, at most
// rechecks of them.
func (n *node) dueAt(repo string, recheck time.Time, rechecks int) (due, again []gitlet.ChangeDue) {
	n.t.Helper()
	q := gitlet.ReadQuery{Kind: gitlet.ReadChangesDue, Fresh: stale,
		ChangesDue: &gitlet.ChangesDueQuery{RepoID: repo, RecheckBefore: recheck, Rechecks: rechecks}}
	for {
		a, err := n.reader.Read(n.t.Context(), q)
		if err != nil {
			n.t.Fatalf("read %s's due changes: %v", repo, err)
		}
		if q.Cursor == "" {
			again = a.Rechecks
		} else if len(a.Rechecks) > 0 {
			n.t.Fatalf("a page after the first named rechecks %+v", a.Rechecks)
		}
		due = append(due, a.DueChanges...)
		if a.Next == "" {
			return due, again
		}
		q.Cursor = a.Next
	}
}

// reposDueAt is every repository the reader lists change work due in at
// recheck.
func (n *node) reposDueAt(recheck time.Time) []string {
	n.t.Helper()
	q := gitlet.ReadQuery{Kind: gitlet.ReadChangesDue, Fresh: stale,
		ChangesDue: &gitlet.ChangesDueQuery{RecheckBefore: recheck}}
	var out []string
	for {
		a, err := n.reader.Read(n.t.Context(), q)
		if err != nil {
			n.t.Fatalf("read the due repositories: %v", err)
		}
		out = append(out, a.DueRepos...)
		if a.Next == "" {
			return out
		}
		q.Cursor = a.Next
	}
}

// A MOVED TARGET'S MERGES ARE DUE AGAIN, OLDEST FIRST, ONCE THEIR REFRESH HAS
// PASSED: while the target holds the tip a change's head revision merged into,
// it owes no merge and is no recheck; once the target moves, it is a RECHECK
// only if its merge was recorded at or before the home's refresh instant, oldest
// first, at most as many as the home asks, and the repository listing names the
// repository exactly then. An undecided merge is a recheck at its own tip too,
// once old enough; a merge into a branch no longer targeted, or of a revision
// moved past, owes a MERGE at once, whatever its age.
//
// Mutations: order the rechecks by change, not merge time → the budget rechecks
// the same first ones every refresh; drop the instant's term → a moved target's
// changes are merged again every pass.
func TestAMovedTargetsMergesAreDueOldestFirst(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, first := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/second", gitlet.ZeroOID, oidOf("C")),
		set("refs/heads/third", gitlet.ZeroOID, oidOf("D"))))
	var changes []string
	for _, source := range []string{"refs/heads/second", "refs/heads/third"} {
		opened, err := c.ProposeChange(ctx, opening(repo, source, mainRef, dev))
		requireChanged(t, opened, err)
		changes = append(changes, opened.ChangeID)
	}
	heads := map[string]string{first: oidOf("B"), changes[0]: oidOf("C"), changes[1]: oidOf("D")}
	// THEIR FACTS RECORDED, so a merge is all any of them may owe; then
	// MERGED in this order, each its own record: third, first, second.
	for change, head := range heads {
		res, err := w.Converse(ctx, n.revising(repo, change, 1, head, mainRef, nil, "a.go"))
		requireConversed(t, res, err)
	}
	for _, change := range []string{changes[1], first, changes[0]} {
		res, err := w.Converse(ctx, n.merging(repo, change, 1, heads[change], mainRef, gitlet.MergeClean))
		requireConversed(t, res, err)
	}
	future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	due, again := n.dueAt(repo, future, gitlet.MaxSnapshotItems)
	if len(due) != 0 || len(again) != 0 || slices.Contains(n.reposDueAt(future), repo) {
		t.Fatalf("a target holding the tip its changes merged into asks %+v and rechecks %+v", due, again)
	}

	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("A2"))))
	if _, again := n.dueAt(repo, past, gitlet.MaxSnapshotItems); len(again) != 0 ||
		slices.Contains(n.reposDueAt(past), repo) {
		t.Fatalf("merges recorded after the refresh's instant are due again: %+v", again)
	}
	// THE ORDER THE ROWS SAY: by when each merge was recorded.
	byAge := []string{first, changes[0], changes[1]}
	sort.Slice(byAge, func(i, j int) bool {
		a, b := n.mergeability(byAge[i]), n.mergeability(byAge[j])
		return a.at < b.at || (a.at == b.at && byAge[i] < byAge[j])
	})
	_, again = n.dueAt(repo, future, gitlet.MaxSnapshotItems)
	var named []string
	for _, d := range again {
		named = append(named, d.ID)
		if d.Tip != oidOf("A2") || d.Head != heads[d.ID] || d.Checked.IsZero() ||
			store.EncodeTime(d.Checked) != n.mergeability(d.ID).at || d.FactsDue || d.MergeDue {
			t.Errorf("a recheck is %+v, want its head merged again into A2, checked when its merge was recorded",
				d)
		}
	}
	if !slices.Equal(named, byAge) || !slices.Contains(n.reposDueAt(future), repo) {
		t.Fatalf("the rechecks are %v, want every change of the moved target oldest merge first: %v", named, byAge)
	}
	if _, oldest := n.dueAt(repo, future, 1); len(oldest) != 1 || oldest[0].ID != byAge[0] {
		t.Fatalf("a budget of one recheck names %+v, want the oldest merge's change %s", oldest, byAge[0])
	}

	// MERGED AGAIN into the moved tip, a change leaves the rechecks; an
	// undecided merge there is one again once old enough.
	res, err := w.Converse(ctx, n.merging(repo, byAge[0], 1, heads[byAge[0]], mainRef, gitlet.MergeClean))
	requireConversed(t, res, err)
	res, err = w.Converse(ctx, n.merging(repo, byAge[1], 1, heads[byAge[1]], mainRef, gitlet.MergeUnknown))
	requireConversed(t, res, err)
	_, again = n.dueAt(repo, future, gitlet.MaxSnapshotItems)
	named = named[:0]
	for _, d := range again {
		named = append(named, d.ID)
	}
	if want := []string{byAge[2], byAge[1]}; !slices.Equal(named, want) {
		t.Fatalf("after one change was merged again and one left undecided the rechecks are %v, want %v", named,
			want)
	}

	// A RETARGET owes a merge at once, never a recheck.
	moved, err := c.ProposeChange(ctx, writing(dev, n.change(byAge[2]).version, func(p *gitlet.ChangeProposal) {
		p.Retarget = &gitlet.ChangeRetarget{RepoID: repo, ChangeID: byAge[2], TargetRef: releaseRef}
	}))
	requireChanged(t, moved, err)
	due, again = n.dueAt(repo, past, gitlet.MaxSnapshotItems)
	if !slices.ContainsFunc(due, func(d gitlet.ChangeDue) bool {
		return d.ID == byAge[2] && d.MergeDue && d.TargetRef == releaseRef && d.Tip == oidOf("R")
	}) || len(again) != 0 {
		t.Fatalf("the retargeted change is due as %+v beside rechecks %+v, want its merge into release owed at "+
			"once", due, again)
	}
}

// A RE-EVALUATION THAT KEEPS THE OUTCOME RECORDS WHAT IT MOVED: a recheck of a
// moved target merging as the row says, or a refresh whose readiness is the
// row's, appends a mergeability_kept naming only what moved (the tip and its
// position, or the readiness versions) and moves nothing else but the recheck's
// instant. A transition (the first merge, a merge turned conflict, a conflict's
// paths changed) is the full record; the same conflict rechecked is kept,
// paths unwritten; a stale recheck is superseded. Only a merge turned
// conflict carries a routing and tells the change's authors.
//
// Mutations, each red: the full record always; the conflicts left out of the
// outcome compared; the tip or the versions dropped from the kept apply; the
// instant moved on a refresh; a routing stamped whatever the audience.
func TestAReEvaluationThatKeepsTheOutcomeRecordsWhatItMoved(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	recorded := func(p statelog.Position) (gitlet.OpKind, string) {
		t.Helper()
		rec := n.recordAt(p)
		return rec.Op, string(rec.Mutation)
	}
	moved := func(from, to string) {
		t.Helper()
		n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf(from), oidOf(to))))
	}

	// THE FIRST MERGE: the full record, and one that concerns nobody — clean,
	// and not ready, since nobody approved — so it carries no routing and
	// writes no notice.
	res := converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	if op, body := recorded(res.Position); op != gitlet.OpMergeability || strings.Contains(body, `"routing"`) {
		t.Fatalf("the first merge recorded %s %s; want a mergeability carrying no routing", op, body)
	}
	if got := noticesOf(t, n, res.Position); len(got) != 0 {
		t.Fatalf("the first merge, which concerns nobody, wrote the notices %v", got)
	}
	first := n.mergeability(change)
	staleTip, staleAt := n.tip(repo, mainRef)

	// A RECHECK OF A MOVED TARGET THAT MERGES CLEAN AGAIN: kept, by its tip.
	moved("A", "A2")
	res = converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	tip, tipAt := n.tip(repo, mainRef)
	if op, body := recorded(res.Position); op != gitlet.OpMergeabilityKept || !strings.Contains(body, tip) ||
		strings.Contains(body, `"state"`) || strings.Contains(body, `"routing"`) ||
		strings.Contains(body, `"change_version"`) {
		t.Fatalf("the recheck recorded %s %s; want a mergeability_kept naming the tip %s alone", op, body, tip)
	}
	rechecked := n.mergeability(change)
	if rechecked.tip != tip || rechecked.targetAt != tipAt.Packed() || rechecked.at <= first.at ||
		rechecked.position != first.position || rechecked.state != gitlet.MergeClean {
		t.Fatalf("the kept recheck left the row %+v; want the tip %s at %s and a later instant, the rest the "+
			"first merge's %+v", rechecked, tip, tipAt, first)
	}

	// A STALE RECHECK, of the tip before the row's: superseded.
	before := n.end()
	res, err := w.Converse(ctx, gitlet.ConversationWrite{OpID: statelog.NewOpID(time.Now(), ""),
		Mergeability: &gitlet.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"),
			TargetRef: mainRef, TargetOID: staleTip, TargetPosition: staleAt, State: gitlet.MergeClean, Node: n.id}})
	requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalSuperseded, "a later tip")

	// A REFRESH WHOSE READINESS IS THE ROW'S, after an ask: kept, by its
	// versions, the merge's instant left alone.
	judged := n.storedReadiness(change)
	converse(t, w, asking(repo, change, dev, "plat-lead"))
	res = converse(t, w, gitlet.ConversationWrite{OpID: statelog.NewOpID(time.Now(), ""),
		Mergeability: &gitlet.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"),
			TargetRef: mainRef, Refresh: true, Node: n.id}})
	if op, body := recorded(res.Position); op != gitlet.OpMergeabilityKept || strings.Contains(body, `"target_oid"`) ||
		!strings.Contains(body, `"readiness_version"`) || strings.Contains(body, `"routing"`) {
		t.Fatalf("the refresh recorded %s %s; want a mergeability_kept naming its versions alone", op, body)
	}
	refreshed := n.storedReadiness(change)
	if refreshed.readinessVersion <= judged.readinessVersion || refreshed.ready {
		t.Fatalf("the kept refresh stored %+v after %+v; want a later readiness version, still not ready",
			refreshed, judged)
	}
	if after := n.mergeability(change); after.at != rechecked.at || after.tip != tip {
		t.Fatalf("the kept refresh moved the merge to %+v; want the recheck's instant and tip %+v", after,
			rechecked)
	}

	// A MERGE THAT TURNS INTO A CONFLICT: the full record, routed to the
	// change's authors.
	moved("A2", "A3")
	res = converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeConflict, "upload/retry.go"))
	if op, body := recorded(res.Position); op != gitlet.OpMergeability || !strings.Contains(body, `"routing"`) {
		t.Fatalf("the merge that turned into a conflict recorded %s %s; want a routed mergeability", op, body)
	}
	if got := heard(gitlet.Concerns(routingAt(t, n, res.Position))); len(got) == 0 ||
		!strings.HasSuffix(got[0], ":conflicted") {
		t.Fatalf("the merge that turned into a conflict tells %v; want the change's authors, conflicted", got)
	}

	// THE SAME CONFLICT RECHECKED: kept, its paths not written again.
	moved("A3", "A4")
	res = converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeConflict, "upload/retry.go"))
	if op, body := recorded(res.Position); op != gitlet.OpMergeabilityKept || strings.Contains(body, "upload/") {
		t.Fatalf("the same conflict rechecked recorded %s %s; want a mergeability_kept naming no path", op, body)
	}
	if m := n.mergeability(change); !slices.Equal(m.conflicts, []string{"upload/retry.go"}) {
		t.Fatalf("the kept conflict left the row's paths %v", m.conflicts)
	}

	// A CONFLICT WHOSE PATHS CHANGED: the full record, which turns nothing a
	// routing names, so it carries none.
	moved("A4", "A5")
	res = converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeConflict, "upload/store.go"))
	if op, body := recorded(res.Position); op != gitlet.OpMergeability || strings.Contains(body, `"routing"`) {
		t.Fatalf("the changed conflict recorded %s %s; want a mergeability carrying no routing", op, body)
	}
	if m := n.mergeability(change); !slices.Equal(m.conflicts, []string{"upload/store.go"}) {
		t.Fatalf("the changed conflict left the row's paths %v, want upload/store.go", m.conflicts)
	}
}

// A KEPT JUDGEMENT TAKES A CHECKS POSITION THE HISTORY LOWERED: a readiness is
// judged at the highest position of its head's check rows, and an ending's
// horizon removes rows ended more than CheckHistory before it, so a change open
// that long is judged at zero, below its row's position. A refresh after an ask,
// and a recheck of a moved target after another, then each keep the outcome as
// a mergeability_kept that APPLIES (the later versions, the recheck's tip and
// instant, the zero position) and leaves the due listing; applied as nothing,
// its writer would answer unknown and the change stay due every pass, for ever.
//
// Mutation: hold the kept apply's checks position forward (checks_position <= ?8
// in its guard) → the refresh after the history applies as nothing — red.
func TestAKeptJudgementTakesAChecksPositionTheHistoryLowered(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	// ended queues and ends one attempt of check name at commit oid for the
	// change's target, the drain handing the applier the broker instant at
	// from then on: an ending's horizon is its own instant less the history.
	ended := func(oid, name string, at time.Time) {
		t.Helper()
		n.storedAt = func(uint64, time.Time) time.Time { return at }
		key := gitlet.CheckKey{RepoID: repo, OID: oid, Target: mainRef, Name: name, Attempt: 1}
		exit := 0
		if _, err := n.publishCheck(repo, gitlet.OpCheckQueued, gitlet.CheckQueued{CheckKey: key,
			Purpose: gitlet.PurposeTip, Reason: gitlet.QueueDispatch,
			Def: &gitlet.CheckDef{Name: name, Run: "make test", TimeoutSeconds: 600}, QueuedAt: at}); err != nil {
			t.Fatalf("queue %s: %v", name, err)
		}
		if _, err := n.publishCheck(repo, gitlet.OpCheckFinished, gitlet.CheckFinished{CheckKey: key,
			State: gitlet.CheckStatePassed, Reason: gitlet.CheckReasonExit, ExitCode: &exit,
			FinishedAt: at}); err != nil {
			t.Fatalf("end %s: %v", name, err)
		}
	}
	headChecks := func() int {
		t.Helper()
		return n.count(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ? AND commit_oid = ?`, repo, oidOf("B"))
	}
	// dueNow reports whether the home's listing at at asks the change's
	// readiness refreshed or its merge rechecked — the work a kept record
	// applied as nothing would leave it owing at every pass. Its facts are
	// another matter: no push here records them.
	dueNow := func(at time.Time) bool {
		t.Helper()
		due, again := n.dueAt(repo, at, 64)
		return slices.ContainsFunc(due, func(d gitlet.ChangeDue) bool { return d.ID == change && d.RefreshDue }) ||
			slices.ContainsFunc(again, func(d gitlet.ChangeDue) bool { return d.ID == change })
	}
	refresh := func() gitlet.ConversationWrite {
		return gitlet.ConversationWrite{OpID: statelog.NewOpID(time.Now(), ""),
			Mergeability: &gitlet.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"),
				TargetRef: mainRef, Refresh: true, Node: n.id}}
	}
	// kept holds the record at p to a mergeability_kept.
	kept := func(what string, p statelog.Position) {
		t.Helper()
		if rec := n.recordAt(p); rec.Op != gitlet.OpMergeabilityKept {
			t.Fatalf("the premise: %s recorded %s %s; want a mergeability_kept", what, rec.Op, rec.Mutation)
		}
	}

	// THE FIRST MERGE, judged at the head's one ended check.
	ended(oidOf("B"), "unit", harnessAt)
	converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	judged := n.storedReadiness(change)
	if judged.checksPosition == 0 {
		t.Fatalf("the premise: the first merge stored %+v; want it judged at the head's ended check", judged)
	}

	// THE HISTORY PASSES: another commit's check ends a history and an hour
	// later, and its horizon removes the head's.
	swept := harnessAt.Add(gitlet.CheckHistory + time.Hour)
	ended(oidOf("X"), "unit", swept)
	if held := headChecks(); held != 0 {
		t.Fatalf("the premise: the head keeps %d check rows past the history", held)
	}

	// A REFRESH AFTER AN ASK: kept, at a checks position of zero, below the
	// row's — and applied.
	converse(t, w, asking(repo, change, dev, "plat-lead"))
	if !dueNow(swept) {
		t.Fatalf("the premise: the ask left the change due no refresh")
	}
	res := converse(t, w, refresh())
	kept("the refresh after the history", res.Position)
	refreshed := n.storedReadiness(change)
	if refreshed.readinessVersion <= judged.readinessVersion || refreshed.checksPosition != 0 {
		t.Fatalf("the refresh after the history stored %+v after %+v; want a later readiness version at a "+
			"checks position of zero", refreshed, judged)
	}
	if dueNow(swept) {
		t.Fatalf("the refreshed change is still due a refresh")
	}

	// A RECHECK AFTER ANOTHER ENDING AND ITS HISTORY, AND ANOTHER ASK: kept, by
	// its tip, its versions and a checks position of zero again — and applied,
	// its instant moving.
	ended(oidOf("B"), "lint", swept)
	converse(t, w, refresh())
	if again := n.storedReadiness(change); again.checksPosition == 0 {
		t.Fatalf("the premise: the refresh after the head's second check stored %+v; want it judged at it",
			again)
	}
	later := swept.Add(gitlet.CheckHistory + time.Hour)
	ended(oidOf("Y"), "unit", later)
	if held := headChecks(); held != 0 {
		t.Fatalf("the premise: the head keeps %d check rows past the second history", held)
	}
	converse(t, w, asking(repo, change, dev, "cto"))
	before := n.mergeability(change)
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("A2"))))
	res = converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	kept("the recheck after the history", res.Position)
	tip, _ := n.tip(repo, mainRef)
	rechecked, stored := n.mergeability(change), n.storedReadiness(change)
	if rechecked.tip != tip || rechecked.at <= before.at || stored.readinessVersion <= refreshed.readinessVersion ||
		stored.checksPosition != 0 {
		t.Fatalf("the recheck after the history left the merge %+v and stored %+v; want the tip %s, a later "+
			"instant and readiness version, at a checks position of zero", rechecked, stored, tip)
	}
	if dueNow(later) {
		t.Fatalf("the rechecked change is still due")
	}
}
