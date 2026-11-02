package code_test

import (
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A CHANGE'S MERGEABILITY THROUGH THE CHANGE WRITER: what the repository's
// home git node found merging a change's head into its target's tip, handed
// to the change writer as the home's own write (no asker), decided in one
// snapshot on a real embedded broker as a record on the change's own subject,
// applied by the shipped applier — and the due listing that asks it of the
// home again once the target moves.

// merging is the home's write of change's merge — revision rev, its head
// head, into target at its tip as n's rows hold it — that state, naming
// conflicts.
func (n *node) merging(repo, change string, rev int64, head, target string, state code.MergeState,
	conflicts ...string) code.ConversationWrite {

	n.t.Helper()
	tip, at := n.tip(repo, target)
	return code.ConversationWrite{OpID: statelog.NewOpID(time.Now(), ""), Mergeability: &code.Mergeability{
		RepoID: repo, ChangeID: change, Revision: rev, HeadOID: head, TargetRef: target, TargetOID: tip,
		TargetPosition: at, State: state, Conflicts: conflicts, Node: n.id}}
}

// heldMerge is a change's mergeability as n's rows hold it.
type heldMerge struct {
	revision               int64
	target, tip            string
	targetAt, position, at int64
	state                  code.MergeState
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
	m.held, m.state = true, code.MergeState(state)
	if conflicts != "" {
		m.conflicts = strings.Split(conflicts, "\x00")
	}
	return m
}

// A MERGEABILITY RECORD IS MONOTONE: a change's mergeability is recorded only
// of its HEAD revision and head, into the branch it merges into now — one of a
// revision a push has moved past, of a head the change does not have, or into
// another branch is refused superseded, appending nothing — and, for that
// revision against that branch, only forward in the target's tip: a merge into
// an earlier tip than the row holds (a pass that read before the target moved,
// slower than the one that read after) is superseded, and so is the same tip
// merged again once the row says clean or conflict, since a merge of two
// commits is decided once. The one same tip taken is an UNKNOWN row's —
// decided at last, or tried again, the row's instant moving so the next try
// waits its home's refresh. A later tip, a new revision and a decided merge
// after an undecided one each replace the one row; its conflicted paths are
// weighed in the change's conversation's text and come back on the change's
// read. A closed change takes no merge; a mergeability naming an asker, or
// conflicts beside a clean merge, is no write at all.
//
// Mutations: drop the guard's position term, and the merge into the earlier
// tip replaces the later one's row; drop its decided term, and the same tip
// merged again appends a second record.
func TestAMergeabilityRecordIsMonotone(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()

	res, err := w.Converse(ctx, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean))
	requireConversed(t, res, err)
	if m := n.mergeability(change); !m.held || m.revision != 1 || m.target != mainRef || m.tip != oidOf("A") ||
		m.state != code.MergeClean || m.position != res.Position.Packed() {
		t.Fatalf("the change's mergeability is %+v, want revision 1 clean into main at A, at %s", m, res.Position)
	}
	before := n.end()
	res, err = w.Converse(ctx, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeConflict, "a.go"))
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalSuperseded, "decided once")

	n.revise(repo, "B", "B2")
	for name, tc := range map[string]struct {
		w    code.ConversationWrite
		says string
	}{
		"a merge of a revision the change has moved past": {
			n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean), "is at revision 2"},
		"a merge of a head the change does not have": {
			n.merging(repo, change, 2, oidOf("B3"), mainRef, code.MergeClean), "is at revision 2"},
		"a merge into a branch the change does not merge into": {
			n.merging(repo, change, 2, oidOf("B2"), releaseRef, code.MergeClean), "merges into"},
	} {
		before := n.end()
		res, err := w.Converse(ctx, tc.w)
		if err != nil || res.Refusal != code.ConversationRefusalSuperseded || !strings.Contains(res.Detail, tc.says) ||
			n.end() != before {
			t.Errorf("%s answered %+v, %v; want superseded saying %q, nothing appended", name, res, err, tc.says)
		}
	}

	// AN UNDECIDED MERGE, AND ITS SAME TIP TRIED AGAIN: taken each time, the
	// row's instant moving with its record.
	res, err = w.Converse(ctx, n.merging(repo, change, 2, oidOf("B2"), mainRef, code.MergeUnknown))
	requireConversed(t, res, err)
	undecided := n.mergeability(change)
	if undecided.revision != 2 || undecided.state != code.MergeUnknown {
		t.Fatalf("the new revision's undecided merge left %+v, want revision 2 unknown", undecided)
	}
	res, err = w.Converse(ctx, n.merging(repo, change, 2, oidOf("B2"), mainRef, code.MergeUnknown))
	requireConversed(t, res, err)
	// STILL UNDECIDED, the outcome the row holds: kept, so the row's instant
	// moves with the try and its position stays the record that decided it
	// (TestAReEvaluationThatKeepsTheOutcomeRecordsWhatItMoved).
	if again := n.mergeability(change); again.position != undecided.position || again.at <= undecided.at {
		t.Fatalf("the same tip tried again left %+v, want the instant past %+v's and its position", again,
			undecided)
	}
	res, err = w.Converse(ctx, n.merging(repo, change, 2, oidOf("B2"), mainRef, code.MergeConflict,
		"upload/retry.go", "docs/README.md"))
	requireConversed(t, res, err)
	conflicted := n.mergeability(change)
	weight := int64(len("upload/retry.go\x00docs/README.md"))
	if conflicted.state != code.MergeConflict || conflicted.count != 2 ||
		!slices.Equal(conflicted.conflicts, []string{"upload/retry.go", "docs/README.md"}) ||
		conflicted.bytes != weight || conflicted.conversationBytes != weight {
		t.Fatalf("the decided merge left %+v, want its two conflicted paths, weighing %d bytes in its "+
			"conversation's text", conflicted, weight)
	}
	before = n.end()
	res, err = w.Converse(ctx, n.merging(repo, change, 2, oidOf("B2"), mainRef, code.MergeClean))
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalSuperseded, "decided once")

	// A LATER TIP REPLACES THE ROW, its paths' weight with it; the earlier
	// tip's merge, arriving after it, is refused.
	earlier := n.merging(repo, change, 2, oidOf("B2"), mainRef, code.MergeConflict, "upload/retry.go")
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("A2"))))
	res, err = w.Converse(ctx, n.merging(repo, change, 2, oidOf("B2"), mainRef, code.MergeClean))
	requireConversed(t, res, err)
	if m := n.mergeability(change); m.tip != oidOf("A2") || m.state != code.MergeClean || len(m.conflicts) != 0 ||
		m.bytes != 0 || m.conversationBytes != 0 || m.targetAt <= conflicted.targetAt {
		t.Fatalf("the later tip's merge left %+v, want clean into A2 with no paths left weighed", m)
	}
	before = n.end()
	res, err = w.Converse(ctx, earlier)
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalSuperseded, "a later tip")
	if got := n.count(`SELECT COUNT(*) FROM code_mergeability WHERE change_id = ?`, change); got != 1 {
		t.Fatalf("the change holds %d mergeability rows, want its one", got)
	}

	// THE CHANGE'S READ carries what its home recorded.
	answer, err := readChange(t, n, ada, "", "web", 1, 0, "")
	if err != nil || answer.Change == nil || answer.Change.Mergeability == nil {
		t.Fatalf("the change read (%+v, %v) carries no mergeability", answer.Change, err)
	}
	if got := answer.Change.Mergeability; got.Revision != 2 || got.TargetRef != mainRef ||
		got.TargetOID != oidOf("A2") || got.State != code.MergeClean || got.Node != n.id || got.At.IsZero() {
		t.Fatalf("the change read carries %+v, want revision 2's clean merge into main at A2", got)
	}

	invalid := n.merging(repo, change, 2, oidOf("B2"), mainRef, code.MergeClean)
	invalid.Asker = dev
	if _, err := w.Converse(ctx, invalid); !errors.Is(err, code.ErrInvalidProposal) ||
		!strings.Contains(err.Error(), "asker") {
		t.Fatalf("a mergeability naming an asker answered %v, want ErrInvalidProposal naming the asker", err)
	}
	for name, tc := range map[string]struct {
		w     code.ConversationWrite
		field string
	}{
		"conflicts beside a clean merge": {
			n.merging(repo, change, 2, oidOf("B2"), mainRef, code.MergeClean, "a.go"), "mergeability.conflicts"},
		"a conflicted path named twice": {
			n.merging(repo, change, 2, oidOf("B2"), mainRef, code.MergeConflict, "a.go", "a.go"),
			"mergeability.conflicts[1]"},
		"a state no writer forms": {
			n.merging(repo, change, 2, oidOf("B2"), mainRef, "maybe"), "mergeability.state"},
	} {
		if _, err := w.Converse(ctx, tc.w); !errors.Is(err, code.ErrInvalidProposal) ||
			!strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s answered %v, want ErrInvalidProposal naming %s", name, err, tc.field)
		}
	}

	c, _ := committerOn(t, n, n.pub)
	closed, err := c.ProposeChange(ctx, closing(repo, change, dev, n.change(change).version))
	requireChanged(t, closed, err)
	before = n.end()
	res, err = w.Converse(ctx, n.merging(repo, change, 2, oidOf("B2"), mainRef, code.MergeClean))
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalChangeClosed, "closed")
}

// dueAt is one listing of repo's due change work at recheck — its changes owing
// facts or a merge, every page, and the rechecks its first page names, at most
// rechecks of them.
func (n *node) dueAt(repo string, recheck time.Time, rechecks int) (due, again []code.ChangeDue) {
	n.t.Helper()
	q := code.ReadQuery{Kind: code.ReadChangesDue, Fresh: stale,
		ChangesDue: &code.ChangesDueQuery{RepoID: repo, RecheckBefore: recheck, Rechecks: rechecks}}
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
	q := code.ReadQuery{Kind: code.ReadChangesDue, Fresh: stale,
		ChangesDue: &code.ChangesDueQuery{RecheckBefore: recheck}}
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
// PASSED: a change whose head revision holds a mergeability into its branch is
// due no merge while its target holds the tip it merged into — nor named among
// a repository's rechecks — and once the target moves past that tip it is
// named a RECHECK only where its mergeability was recorded at or before the
// instant the home's refresh asks, the oldest merge first and no more than the
// home asks; the listing of repositories names the repository exactly then. An
// undecided merge is a recheck at its own tip too, once old enough; a merge
// into a branch the change no longer targets, or of a revision it has moved
// past, owes a MERGE at once, whatever its age.
//
// Mutations: order the rechecks by change rather than by when they were
// merged, and the home's budget rechecks the same first ones every refresh;
// drop the instant's term, and a moved target's changes are merged again on
// every pass.
func TestAMovedTargetsMergesAreDueOldestFirst(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, first := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/second", code.ZeroOID, oidOf("C")),
		set("refs/heads/third", code.ZeroOID, oidOf("D"))))
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
		res, err := w.Converse(ctx, n.merging(repo, change, 1, heads[change], mainRef, code.MergeClean))
		requireConversed(t, res, err)
	}
	future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	due, again := n.dueAt(repo, future, code.MaxSnapshotItems)
	if len(due) != 0 || len(again) != 0 || slices.Contains(n.reposDueAt(future), repo) {
		t.Fatalf("a target holding the tip its changes merged into asks %+v and rechecks %+v", due, again)
	}

	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("A2"))))
	if _, again := n.dueAt(repo, past, code.MaxSnapshotItems); len(again) != 0 ||
		slices.Contains(n.reposDueAt(past), repo) {
		t.Fatalf("merges recorded after the refresh's instant are due again: %+v", again)
	}
	// THE ORDER THE ROWS SAY: by when each merge was recorded.
	byAge := []string{first, changes[0], changes[1]}
	sort.Slice(byAge, func(i, j int) bool {
		a, b := n.mergeability(byAge[i]), n.mergeability(byAge[j])
		return a.at < b.at || (a.at == b.at && byAge[i] < byAge[j])
	})
	_, again = n.dueAt(repo, future, code.MaxSnapshotItems)
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
	res, err := w.Converse(ctx, n.merging(repo, byAge[0], 1, heads[byAge[0]], mainRef, code.MergeClean))
	requireConversed(t, res, err)
	res, err = w.Converse(ctx, n.merging(repo, byAge[1], 1, heads[byAge[1]], mainRef, code.MergeUnknown))
	requireConversed(t, res, err)
	_, again = n.dueAt(repo, future, code.MaxSnapshotItems)
	named = named[:0]
	for _, d := range again {
		named = append(named, d.ID)
	}
	if want := []string{byAge[2], byAge[1]}; !slices.Equal(named, want) {
		t.Fatalf("after one change was merged again and one left undecided the rechecks are %v, want %v", named,
			want)
	}

	// A RETARGET owes a merge at once, never a recheck.
	moved, err := c.ProposeChange(ctx, writing(dev, n.change(byAge[2]).version, func(p *code.ChangeProposal) {
		p.Retarget = &code.ChangeRetarget{RepoID: repo, ChangeID: byAge[2], TargetRef: releaseRef}
	}))
	requireChanged(t, moved, err)
	due, again = n.dueAt(repo, past, code.MaxSnapshotItems)
	if !slices.ContainsFunc(due, func(d code.ChangeDue) bool {
		return d.ID == byAge[2] && d.MergeDue && d.TargetRef == releaseRef && d.Tip == oidOf("R")
	}) || len(again) != 0 {
		t.Fatalf("the retargeted change is due as %+v beside rechecks %+v, want its merge into release owed at "+
			"once", due, again)
	}
}

// A RE-EVALUATION THAT KEEPS THE OUTCOME RECORDS WHAT IT MOVED: a recheck of a
// moved target that merges as the row says, and a refresh whose readiness is
// the row's, each append a mergeability_kept naming only what moved — the tip
// and its position, or the versions the readiness was judged at — which moves
// those columns and nothing else: the recheck the row's instant too, the
// refresh never. Every transition is the full record: the first merge, a merge
// that turns into a conflict, and a conflict whose paths changed; a conflict
// rechecked with the same paths is kept, its paths never written again. A
// stale recheck — of a tip before the row's — is refused superseded, as the
// merge it replaces is. And a record that concerns nobody carries no routing
// and writes no notice, where one that turns a merge into a conflict carries
// its routing and tells the change's authors.
//
// Mutations: write the full record whatever the row holds → the recheck
// appends a mergeability, red; leave the conflicts out of the outcome compared
// → the changed conflict list is kept and the row keeps the old path, red;
// drop the tip from the kept apply → the row stays at A, red; drop the
// versions from it → the refresh moves no version, red; move the instant on a
// refresh → the row's instant moves, red; stamp a routing whatever the
// audience → the first merge carries one, red.
func TestAReEvaluationThatKeepsTheOutcomeRecordsWhatItMoved(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	recorded := func(p statelog.Position) (code.OpKind, string) {
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
	res := converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean))
	if op, body := recorded(res.Position); op != code.OpMergeability || strings.Contains(body, `"routing"`) {
		t.Fatalf("the first merge recorded %s %s; want a mergeability carrying no routing", op, body)
	}
	if got := noticesOf(t, n, res.Position); len(got) != 0 {
		t.Fatalf("the first merge, which concerns nobody, wrote the notices %v", got)
	}
	first := n.mergeability(change)
	staleTip, staleAt := n.tip(repo, mainRef)

	// A RECHECK OF A MOVED TARGET THAT MERGES CLEAN AGAIN: kept, by its tip.
	moved("A", "A2")
	res = converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean))
	tip, tipAt := n.tip(repo, mainRef)
	if op, body := recorded(res.Position); op != code.OpMergeabilityKept || !strings.Contains(body, tip) ||
		strings.Contains(body, `"state"`) || strings.Contains(body, `"routing"`) ||
		strings.Contains(body, `"change_version"`) {
		t.Fatalf("the recheck recorded %s %s; want a mergeability_kept naming the tip %s alone", op, body, tip)
	}
	rechecked := n.mergeability(change)
	if rechecked.tip != tip || rechecked.targetAt != tipAt.Packed() || rechecked.at <= first.at ||
		rechecked.position != first.position || rechecked.state != code.MergeClean {
		t.Fatalf("the kept recheck left the row %+v; want the tip %s at %s and a later instant, the rest the "+
			"first merge's %+v", rechecked, tip, tipAt, first)
	}

	// A STALE RECHECK, of the tip before the row's: superseded.
	before := n.end()
	res, err := w.Converse(ctx, code.ConversationWrite{OpID: statelog.NewOpID(time.Now(), ""),
		Mergeability: &code.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"),
			TargetRef: mainRef, TargetOID: staleTip, TargetPosition: staleAt, State: code.MergeClean, Node: n.id}})
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalSuperseded, "a later tip")

	// A REFRESH WHOSE READINESS IS THE ROW'S, after an ask: kept, by its
	// versions, the merge's instant left alone.
	judged := n.storedReadiness(change)
	converse(t, w, asking(repo, change, dev, "plat-lead"))
	res = converse(t, w, code.ConversationWrite{OpID: statelog.NewOpID(time.Now(), ""),
		Mergeability: &code.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"),
			TargetRef: mainRef, Refresh: true, Node: n.id}})
	if op, body := recorded(res.Position); op != code.OpMergeabilityKept || strings.Contains(body, `"target_oid"`) ||
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
	res = converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeConflict, "upload/retry.go"))
	if op, body := recorded(res.Position); op != code.OpMergeability || !strings.Contains(body, `"routing"`) {
		t.Fatalf("the merge that turned into a conflict recorded %s %s; want a routed mergeability", op, body)
	}
	if got := heard(code.Concerns(routingAt(t, n, res.Position))); len(got) == 0 ||
		!strings.HasSuffix(got[0], ":conflicted") {
		t.Fatalf("the merge that turned into a conflict tells %v; want the change's authors, conflicted", got)
	}

	// THE SAME CONFLICT RECHECKED: kept, its paths not written again.
	moved("A3", "A4")
	res = converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeConflict, "upload/retry.go"))
	if op, body := recorded(res.Position); op != code.OpMergeabilityKept || strings.Contains(body, "upload/") {
		t.Fatalf("the same conflict rechecked recorded %s %s; want a mergeability_kept naming no path", op, body)
	}
	if m := n.mergeability(change); !slices.Equal(m.conflicts, []string{"upload/retry.go"}) {
		t.Fatalf("the kept conflict left the row's paths %v", m.conflicts)
	}

	// A CONFLICT WHOSE PATHS CHANGED: the full record, which turns nothing a
	// routing names, so it carries none.
	moved("A4", "A5")
	res = converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeConflict, "upload/store.go"))
	if op, body := recorded(res.Position); op != code.OpMergeability || strings.Contains(body, `"routing"`) {
		t.Fatalf("the changed conflict recorded %s %s; want a mergeability carrying no routing", op, body)
	}
	if m := n.mergeability(change); !slices.Equal(m.conflicts, []string{"upload/store.go"}) {
		t.Fatalf("the changed conflict left the row's paths %v, want upload/store.go", m.conflicts)
	}
}
