package code_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S MERGE, decided by the shipped committer from one snapshot on a
// real embedded broker, applied by the shipped applier as one record and read
// back from the rows it wrote (ADR-0066).

// seatRef is dev's own branch, under the seats' namespace: what a seat's
// change is opened from, and what its merge deletes by default (Q4).
const seatRef = "refs/heads/seats/dev/fix"

// mergeGate is main's gate on a merge case's repository: one counted
// approval, no check — so a change merges into main directly, and main's
// green row moves with every merge.
func mergeGate() *code.MergeGate {
	return &code.MergeGate{MergeBy: code.MergeByWriters, Approvals: code.ApprovalsOne,
		Landing: code.Landing{Depth: 4, Batch: 4}, DeleteSource: code.DeleteSeats}
}

// mergeRepo is a fresh repository named name — the one Engineering owns, its
// main protected by rule (taking direct pushes as direct says) with gate —
// holding main at A and dev's own branch at B, and dev's change from that
// branch into main on it.
func mergeRepo(t *testing.T, n *node, c *code.Committer, name string, direct code.DirectPush,
	gate *code.MergeGate) (repo, change string) {

	t.Helper()
	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].DirectPush, s.Protections[0].Gate = direct, gate
	repo = n.ensure(name, s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
		set(seatRef, code.ZeroOID, oidOf("B"))))
	opened, err := c.ProposeChange(t.Context(), opening(repo, seatRef, mainRef, dev))
	requireChanged(t, opened, err)
	return repo, opened.ChangeID
}

// approved makes change's revision 1 — head B — meet a gate asking one
// approval: its facts against main recorded, and plat-lead's approval.
func approved(t *testing.T, n *node, w *code.ChangeWriter, repo, change string) {
	t.Helper()
	converse(t, w, n.revising(repo, change, 1, oidOf("B"), mainRef, nil))
	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil))
}

// merging is asker's merge of change into main, method built onto base from
// head as commit, under an operation id of its own — carrying the pack its
// build stored, but for a fast-forward, which builds none.
func merging(repo, change string, asker code.Principal, method code.MergeMethod, base, head,
	commit string) code.MergeProposal {

	opID := statelog.NewOpID(time.Now(), "code_merge_change")
	m := code.Merge{RepoID: repo, MergeID: code.WritePushID(opID), ChangeID: change, Method: method,
		TargetRef: mainRef, BaseOID: base, HeadOID: head, CommitOID: commit, Node: "git-a", ReceivedAt: harnessAt}
	if method != code.MergeMethodFastForward {
		m.Pack = &code.PackRef{PackID: m.MergeID, Object: objstore.KeyAt(harnessAt),
			Hash: objstore.HashOf([]byte(m.MergeID)), Size: 32 + code.IndexBytesFor(1) + code.ReverseBytesFor(1),
			IndexBytes: code.IndexBytesFor(1), ReverseBytes: code.ReverseBytesFor(1),
			GitChecksum: oidOf("pack" + m.MergeID), Objects: 1}
	}
	return code.MergeProposal{OpID: opID, Asker: asker, Merge: m}
}

// requireMerged fails t unless res is a merge applied at a position, its
// target moved to commit.
func requireMerged(t *testing.T, res code.MergeResult, err error, commit string) {
	t.Helper()
	if err != nil {
		t.Fatalf("ProposeMerge: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() || res.Refusal != "" ||
		res.MergeOID != commit {
		t.Fatalf("the merge answered %+v, want applied at a position, merged at %s", res, commit[:7])
	}
}

// requireMergeRefused fails t unless res is refused under refusal with a
// sentence holding says, and nothing was appended to n's log since before.
func requireMergeRefused(t *testing.T, n *node, before uint64, res code.MergeResult, err error,
	refusal code.MergeRefusal, says string) {

	t.Helper()
	if err != nil {
		t.Fatalf("ProposeMerge: %v", err)
	}
	if res.Refusal != refusal || res.Outcome != "" || !strings.Contains(res.Detail, says) {
		t.Fatalf("the merge answered %+v, want refused %s saying %q", res, refusal, says)
	}
	if after := n.end(); after != before {
		t.Fatalf("a refused merge appended: the log moved from %d to %d", before, after)
	}
}

// mergedChange is what a change's row says of its merge.
type mergedChange struct {
	state, via, mergeOID      string
	by, byKind, operator, why string
	position                  int64
	closed                    bool
}

// merged is change id's merge as n's rows hold it.
func (n *node) mergedChange(id string) mergedChange {
	n.t.Helper()
	var m mergedChange
	var closed sql.NullInt64
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT state, merged_via, merge_oid, merged_by, merged_by_kind,
			merged_operator_id, override_reason, position, closed_at FROM code_changes WHERE id = ?`, id).Scan(
			&m.state, &m.via, &m.mergeOID, &m.by, &m.byKind, &m.operator, &m.why, &m.position, &closed)
	}); err != nil {
		n.t.Fatalf("read change %s: %v", id, err)
	}
	m.closed = closed.Valid
	return m
}

// mergeStream is a committer on n over its own publisher, announcing on an
// event stream of its own.
func mergeStream(t *testing.T, n *node) (*code.Committer, *announced) {
	t.Helper()
	stream := &announced{}
	c, _ := committerOn(t, n, n.pub, func(o *code.CommitterOptions) { o.Events, o.AgentIDs = stream, agentIDs })
	return c, stream
}

// A MERGE MOVES ITS TARGET AND DELETES ITS SOURCE IN ONE RECORD: dev's change
// from its own branch, its gate met, merges as ONE record — the log one record
// longer — which marks the change MERGED (via direct, at the merge's commit,
// by dev), never closed though its source branch is deleted in the same
// record; moves main from its base onto the merge's commit; deletes dev's
// branch, as main's delete_source seats asks; lands that as a push `via merge`
// naming dev and the commit it introduced, with its pack's row of kind merge;
// and writes main's landing row, green, and moves main's green row to the
// commit — every row at the one position. The node that landed it announces
// one change_merged, naming the change, the commit and dev.
//
// Mutation: land the push before marking the change merged — the source's
// deletion closes the change (M4's close on a deleted source), and it reads
// closed, red.
func TestAMergeMovesTargetDeletesSourceInOneRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, stream := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo, change := mergeRepo(t, n, c, "web", code.DirectPersons, mergeGate())
	approved(t, n, w, repo, change)

	before := n.end()
	p := merging(repo, change, dev, code.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	p.Merge.DeleteSource = true
	res, err := c.ProposeMerge(t.Context(), p)
	requireMerged(t, res, err, oidOf("M"))
	if after := n.end(); after != before+1 {
		t.Fatalf("the merge appended %d records, want one", after-before)
	}
	at := res.Position.Packed()

	got := n.mergedChange(change)
	if want := (mergedChange{state: "merged", via: "direct", mergeOID: oidOf("M"), by: "dev", byKind: "agent",
		position: at}); got != want {
		t.Fatalf("the merged change's row is %+v, want %+v: merged by its own record, never closed by its "+
			"source's deletion", got, want)
	}
	if main, branch := n.target(repo, mainRef), n.target(repo, seatRef); main != oidOf("M") || branch != "" {
		t.Fatalf("main is at %q and dev's branch at %q, want main at the merge's commit and the branch deleted",
			main, branch)
	}
	if pushes := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ? AND repo_id = ? AND position = ?
			AND via = 'merge' AND actor = 'dev' AND actor_kind = 'agent' AND introduced = ? AND introduced_count = 1`,
		p.Merge.MergeID, repo, at, oidOf("M")); pushes != 1 {
		t.Fatalf("%d push rows record the merge's push at its position, via merge by dev introducing its commit, "+
			"want one", pushes)
	}
	if packs := n.count(`SELECT COUNT(*) FROM code_packs WHERE repo_id = ? AND pack_id = ? AND kind = 'merge'
			AND state = 'live' AND added_position = ?`, repo, p.Merge.MergeID, at); packs != 1 {
		t.Fatalf("%d live pack rows of kind merge hold the merge's pack, want one", packs)
	}
	if landings := n.count(`SELECT COUNT(*) FROM code_landings WHERE repo_id = ? AND target_ref = ?
			AND position = ? AND ordinal = 1 AND kind = 'direct' AND change_id = ? AND base_oid = ?
			AND commit_oid = ? AND merged_by = 'dev' AND green = 1 AND broken = 0`, repo, mainRef, at, change,
		oidOf("A"), oidOf("M")); landings != 1 {
		t.Fatalf("%d landing rows record the merge on main, green, want one", landings)
	}
	if greens := n.count(`SELECT COUNT(*) FROM code_green WHERE repo_id = ? AND target_ref = ? AND commit_oid = ?
			AND position = ?`, repo, mainRef, oidOf("M"), at); greens != 1 {
		t.Fatalf("%d green rows hold main at the merge's commit, want one: a gate requiring no check is green as "+
			"its merge lands", greens)
	}

	waitAnnouncedOf(t, stream, "change_merged", 1)
	ev := stream.of("change_merged")[0]
	payload, ok := ev.Data.(*types.ChangeMerged)
	if !ok || ev.Type != "change_merged" || payload.ChangeID != change || payload.Number != 1 ||
		payload.Commit != oidOf("M") || payload.MergedBy != "dev" || payload.Method != "merge" ||
		payload.RepoName != "web" || payload.Override != "" || ev.Source != n.id {
		t.Fatalf("the merge was announced as %s from %q %+v, want web!1 merged at its commit by dev, from %s",
			ev.Type, ev.Source, ev.Data, n.id)
	}

	// THE SAME OPERATION ASKED AGAIN is the same merge: collapsed onto the
	// record that landed, announced once.
	again, err := c.ProposeMerge(t.Context(), p)
	requireMerged(t, again, err, oidOf("M"))
	if again.Position != res.Position || n.end() != before+1 {
		t.Fatalf("the merge asked again answered %+v, the log at %d; want the first record's answer and no "+
			"second record", again, n.end())
	}
	time.Sleep(20 * time.Millisecond)
	if got := len(stream.of("change_merged")); got != 1 {
		t.Fatalf("the merge was announced %d times, want once", got)
	}
}

// A MERGE IS DECIDED FROM ONE SNAPSHOT: a merge built onto main at A, asked
// after a push moved main to A2, is refused target_moved — read off the
// snapshot's refs, never off the proposal, which still says A — and appends
// nothing: main stays at A2 and the change open. Built again onto A2, it
// merges. And a merge built from a head the change has moved from is refused
// head_moved, as one into a target the change no longer has is stale.
//
// Mutation: decide from the proposal's base rather than the snapshot's tip —
// the stale merge is appended, its apply skips it, and the answer is no
// target_moved refusal, red.
func TestAMergeIsDecidedFromOneSnapshot(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo, change := mergeRepo(t, n, c, "web", code.DirectPersons, mergeGate())
	approved(t, n, w, repo, change)

	built := merging(repo, change, dev, code.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	// THE TARGET MOVES between the build and the decide: a person's push.
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("A2"))))
	before := n.end()
	res, err := c.ProposeMerge(t.Context(), built)
	requireMergeRefused(t, n, before, res, err, code.MergeRefusalTargetMoved, "build it again onto the tip")
	if main, state := n.target(repo, mainRef), n.mergedChange(change).state; main != oidOf("A2") ||
		state != "open" {
		t.Fatalf("main is at %q and the change %s, want main at A2 and the change open", main, state)
	}

	moved := merging(repo, change, dev, code.MergeMethodMerge, oidOf("A2"), oidOf("C"), oidOf("M2"))
	res, err = c.ProposeMerge(t.Context(), moved)
	requireMergeRefused(t, n, before, res, err, code.MergeRefusalHeadMoved, "read it again")
	elsewhere := merging(repo, change, dev, code.MergeMethodMerge, oidOf("A2"), oidOf("B"), oidOf("M2"))
	elsewhere.Merge.TargetRef = releaseRef
	res, err = c.ProposeMerge(t.Context(), elsewhere)
	requireMergeRefused(t, n, before, res, err, code.MergeRefusalStale, "merges into refs/heads/main")

	rebuilt := merging(repo, change, dev, code.MergeMethodMerge, oidOf("A2"), oidOf("B"), oidOf("M2"))
	res, err = c.ProposeMerge(t.Context(), rebuilt)
	requireMerged(t, res, err, oidOf("M2"))
	if main := n.target(repo, mainRef); main != oidOf("M2") {
		t.Fatalf("main is at %q after the merge built onto its tip, want M2", main)
	}
}

// AN OVERRIDE IS A PERSON'S: a change that meets no gate yet — no approval —
// is refused gate_unmet when dev merges it, naming the approval it waits for;
// dev's override is refused not_person, and so is a token bound to nobody's;
// a person's override merges it, the reason recorded on the change's row with
// the person's human seat and the credential they merged with, and announced.
// A target whose gate requires a check merges through its queue: a merge
// asked there directly is refused queue_target — a person's too, unless they
// override — and a person's override merges a QUEUED change directly, its
// entry dequeued as merged, its landing not green: the gate requires checks.
//
// Mutation: take a seat's override as a person's — dev's override merges the
// unapproved change, red.
func TestAnOverrideIsAPersons(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, stream := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo, change := mergeRepo(t, n, c, "web", code.DirectPersons, mergeGate())
	converse(t, w, n.revising(repo, change, 1, oidOf("B"), mainRef, nil))

	before := n.end()
	res, err := c.ProposeMerge(t.Context(), merging(repo, change, dev, code.MergeMethodSquash, oidOf("A"),
		oidOf("B"), oidOf("S")))
	requireMergeRefused(t, n, before, res, err, code.MergeRefusalGateUnmet, "does not meet")
	if len(res.Unmet) != 1 || res.Unmet[0].Kind != code.PreconditionApproval {
		t.Fatalf("the refusal names %+v, want the approval alone", res.Unmet)
	}
	for _, asker := range []code.Principal{dev, unbound} {
		overridden := merging(repo, change, asker, code.MergeMethodSquash, oidOf("A"), oidOf("B"), oidOf("S"))
		overridden.Merge.Override = "the release cannot wait"
		res, err = c.ProposeMerge(t.Context(), overridden)
		requireMergeRefused(t, n, before, res, err, code.MergeRefusalNotPerson, "only a person merges past")
	}

	byPerson := merging(repo, change, ada, code.MergeMethodSquash, oidOf("A"), oidOf("B"), oidOf("S"))
	byPerson.Merge.Override = "the release cannot wait"
	res, err = c.ProposeMerge(t.Context(), byPerson)
	requireMerged(t, res, err, oidOf("S"))
	if got := n.mergedChange(change); got.state != "merged" || got.by != "founder" || got.byKind != "person" ||
		got.operator != "pat:ada" || got.why != "the release cannot wait" {
		t.Fatalf("the overridden change's row is %+v, want merged by founder through pat:ada, the reason kept", got)
	}
	waitAnnouncedOf(t, stream, "change_merged", 1)
	merged := stream.of("change_merged")[0]
	if payload, ok := merged.Data.(*types.ChangeMerged); !ok || payload.Override != "the release cannot wait" ||
		payload.MergedBy != "founder" || payload.MergedByKind != "person" {
		t.Fatalf("the override was announced as %+v, want founder's merge naming its reason", merged.Data)
	}

	// A QUEUE TARGET: main's gate requires unit.
	queued, qchange := queueRepo(t, n, c, "api")
	ready(t, n, w, queued, qchange)
	enqueue := enqueueing(queued, qchange, ada, oidOf("B"))
	entered, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, entered, err, code.QueueWaiting, 1)
	before = n.end()
	for _, asker := range []code.Principal{dev, ada} {
		direct := merging(queued, qchange, asker, code.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("Q"))
		res, err = c.ProposeMerge(t.Context(), direct)
		requireMergeRefused(t, n, before, res, err, code.MergeRefusalQueueTarget, "through its queue")
	}
	overridden := merging(queued, qchange, ada, code.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("Q"))
	overridden.Merge.Override = "the queue's runner is down"
	res, err = c.ProposeMerge(t.Context(), overridden)
	requireMerged(t, res, err, oidOf("Q"))
	if state, reason := n.entryState(queued, enqueue.Enqueue.EntryID); state != code.QueueDequeued ||
		reason != string(code.EndMerged) {
		t.Fatalf("the merged change's entry is %s (%s), want dequeued, merged", state, reason)
	}
	if landings := n.count(`SELECT COUNT(*) FROM code_landings WHERE repo_id = ? AND kind = 'direct'
			AND green = 0`, queued); landings != 1 {
		t.Fatalf("%d landing rows record the override, not green, want one: its gate requires checks", landings)
	}
	if greens := n.count(`SELECT COUNT(*) FROM code_green WHERE repo_id = ?`, queued); greens != 0 {
		t.Fatalf("an override past required checks moved the green row (%d rows), want none", greens)
	}
}

// A PROTECTED REF MOVES ONLY BY A GATED RECORD: main is protected with
// direct_push none and a gate asking one approval. A person's push to it and
// a seat's are refused protected_direct; a merge of a change that does not
// meet the gate is refused gate_unmet; and main moves only once the change
// meets its gate, by the merge's record — a fast-forward here, its push row
// introducing nothing the change's own pushes had not. Under direct_push
// persons a person's push lands — that person's own override, recorded on
// its push row under their credential and human seat — and a seat's is still
// refused.
//
// Mutation: decide a merge without its gate — the unapproved change's merge
// moves main, red.
func TestAProtectedRefMovesOnlyByAGatedRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo, change := mergeRepo(t, n, c, "web", code.DirectNone, mergeGate())
	converse(t, w, n.revising(repo, change, 1, oidOf("B"), mainRef, nil))

	before := n.end()
	byPerson := proposal(repo, update(mainRef, oidOf("A"), oidOf("X")))
	bySeat := proposal(repo, update(mainRef, oidOf("A"), oidOf("X")))
	bySeat.Principal = code.Principal{Kind: code.PrincipalAgent, Seat: "dev",
		Run: code.RunProvenance{TurnID: "turn-1"}}
	bySeat.Via = code.ViaEngine
	for name, p := range map[string]code.Proposal{"a person's push": byPerson, "a seat's push": bySeat} {
		got, err := c.Propose(t.Context(), p)
		if err != nil || len(got.Answer.Commands) != 1 || got.Answer.Commands[0].Reason != code.RefusalProtectedDirect {
			t.Fatalf("%s to main answered %+v (%v), want refused protected_direct", name, got.Answer, err)
		}
	}
	res, err := c.ProposeMerge(t.Context(), merging(repo, change, dev, code.MergeMethodFastForward, oidOf("A"),
		oidOf("B"), oidOf("B")))
	requireMergeRefused(t, n, before, res, err, code.MergeRefusalGateUnmet, "does not meet")
	if main := n.target(repo, mainRef); main != oidOf("A") {
		t.Fatalf("main is at %q, want A: nothing but a gated record moves it", main)
	}

	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil))
	ff := merging(repo, change, dev, code.MergeMethodFastForward, oidOf("A"), oidOf("B"), oidOf("B"))
	res, err = c.ProposeMerge(t.Context(), ff)
	requireMerged(t, res, err, oidOf("B"))
	if main := n.target(repo, mainRef); main != oidOf("B") {
		t.Fatalf("main is at %q after its gated merge, want the change's head", main)
	}
	if pushes := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ? AND via = 'merge'
			AND introduced = '' AND introduced_count = 0 AND pack_id = ''`, ff.Merge.MergeID); pushes != 1 {
		t.Fatalf("%d push rows record the fast-forward, introducing nothing and storing no pack, want one", pushes)
	}

	// UNDER DIRECT_PUSH PERSONS, a person's push is their recorded override.
	persons, _ := mergeRepo(t, n, c, "api", code.DirectPersons, mergeGate())
	override := proposal(persons, update(mainRef, oidOf("A"), oidOf("Y")))
	got, err := c.Propose(t.Context(), override)
	if err != nil || len(got.Answer.Commands) != 1 || got.Answer.Commands[0].Result != code.ResultOK {
		t.Fatalf("a person's push to main under direct_push persons answered %+v (%v), want it landed",
			got.Answer, err)
	}
	if pushes := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ? AND via = 'git'
			AND actor = 'op-1' AND actor_kind = 'operator' AND actor_seat = 'ada'`, override.PushID); pushes != 1 {
		t.Fatalf("%d push rows record the person's override under their credential and human seat, want one",
			pushes)
	}
	seat := bySeat
	seat.PushID, seat.RepoID, seat.Commands = uuid.Must(uuid.NewV7()).String(), persons,
		[]code.Command{update(mainRef, oidOf("Y"), oidOf("Z"))}
	refused, err := c.Propose(t.Context(), seat)
	if err != nil || len(refused.Answer.Commands) != 1 ||
		refused.Answer.Commands[0].Reason != code.RefusalProtectedDirect {
		t.Fatalf("a seat's push to main under direct_push persons answered %+v (%v), want refused "+
			"protected_direct", refused.Answer, err)
	}
}

// A MERGE ASKED TO DELETE A SOURCE ITS MERGER MAY NOT DELETE is refused
// source_refused, appending nothing: dev's merge of a change from main's
// sibling release — a branch outside the seats' namespace, whose deletion
// takes a principal maintaining the repository — names the push refusal it
// would meet; asked keeping its source, it merges.
//
// Mutation: judge every source as a seat's own branch — dev deletes release,
// red.
func TestAMergeDeletesOnlyASourceItsMergerMay(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	opened, err := c.ProposeChange(t.Context(), opening(repo, releaseRef, mainRef, ada))
	requireChanged(t, opened, err)
	converse(t, w, n.revising(repo, opened.ChangeID, 1, oidOf("R"), mainRef, nil))
	converse(t, w, reviewing(repo, opened.ChangeID, platLead, 1, code.VerdictApprove, nil))

	before := n.end()
	deleting := merging(repo, opened.ChangeID, dev, code.MergeMethodMerge, oidOf("A"), oidOf("R"), oidOf("M"))
	deleting.Merge.DeleteSource = true
	res, err := c.ProposeMerge(t.Context(), deleting)
	requireMergeRefused(t, n, before, res, err, code.MergeRefusalSourceRefused, "refused not_permitted")
	if branch := n.target(repo, releaseRef); branch != oidOf("R") {
		t.Fatalf("release is at %q, want R: a refused merge deletes nothing", branch)
	}
	keeping := merging(repo, opened.ChangeID, dev, code.MergeMethodMerge, oidOf("A"), oidOf("R"), oidOf("M"))
	res, err = c.ProposeMerge(t.Context(), keeping)
	requireMerged(t, res, err, oidOf("M"))
	if branch := n.target(repo, releaseRef); branch != oidOf("R") {
		t.Fatalf("release is at %q after a merge keeping its source, want R", branch)
	}
}

// A MERGE PROPOSAL IS REFUSED BEFORE IT IS QUEUED where no committer could
// decide it: a fast-forward to anything but the change's head, or carrying a
// pack; a merge or a squash carrying none; a merge id that is not its
// operation's push id; a field the committer stamps sent by the asker; and an
// asker naming a coding run.
func TestAMergeProposalIsHeldToItsShape(t *testing.T) {
	t.Parallel()
	ok := merging("019a0c2e-0000-7000-8000-0000000000aa", "019a0c2e-0000-7000-8000-0000000000bb", dev,
		code.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	if err := ok.Validate(); err != nil {
		t.Fatalf("a merge of its shape is refused: %v", err)
	}
	ff := merging(ok.Merge.RepoID, ok.Merge.ChangeID, dev, code.MergeMethodFastForward, oidOf("A"), oidOf("B"),
		oidOf("B"))
	if err := ff.Validate(); err != nil {
		t.Fatalf("a fast-forward of its shape is refused: %v", err)
	}
	for name, mutate := range map[string]func(p *code.MergeProposal){
		"a fast-forward past the head": func(p *code.MergeProposal) {
			p.Merge.Method, p.Merge.Pack = code.MergeMethodFastForward, nil
		},
		"a fast-forward with a pack": func(p *code.MergeProposal) {
			p.Merge.Method, p.Merge.CommitOID = code.MergeMethodFastForward, oidOf("B")
		},
		"a merge with no pack": func(p *code.MergeProposal) { p.Merge.Pack = nil },
		"a squash of the head": func(p *code.MergeProposal) {
			p.Merge.Method, p.Merge.CommitOID = code.MergeMethodSquash, oidOf("B")
		},
		"a commit that is the base": func(p *code.MergeProposal) { p.Merge.CommitOID = oidOf("A") },
		"another push id":           func(p *code.MergeProposal) { p.Merge.MergeID = uuid.Must(uuid.NewV7()).String() },
		"a stamped principal":       func(p *code.MergeProposal) { p.Merge.Principal = dev },
		"a stamped source":          func(p *code.MergeProposal) { p.Merge.SourceRef = seatRef },
		"a stamped landing":         func(p *code.MergeProposal) { p.Merge.Protected = true },
		"a coding run": func(p *code.MergeProposal) {
			p.Asker.Run = code.RunProvenance{TurnID: "turn-1", RunID: "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b",
				CodingAgent: "claude"}
		},
		"an override past its bound": func(p *code.MergeProposal) {
			p.Merge.Override = strings.Repeat("x", code.MaxOverrideReasonBytes+1)
		},
	} {
		p := ok
		mutate(&p)
		if err := p.Validate(); !errors.Is(err, code.ErrInvalidProposal) {
			t.Errorf("%s: Validate answered %v, want code.ErrInvalidProposal", name, err)
		}
	}
}

// A MERGE RECORD FITS ITS ROWS: the most one merge record writes before its
// horizons — its own rows, its push's, and every change its push moves — is
// a record's row bound with the horizons' room, so a merge's decide counts no
// change of its push's as a push batch's must.
func TestAMergeRecordFitsItsRows(t *testing.T) {
	t.Parallel()
	if got := code.RecordRows(code.MergeRecordRows); got > code.MaxRecordRows {
		t.Fatalf("a merge record writes up to %d rows with its horizons' room, past MaxRecordRows (%d)", got,
			code.MaxRecordRows)
	}
}

// A MERGE THAT DELETES ITS SOURCE RECORDS THE DROP (M6-D2): the head its
// source branch held is an object no ref names once the merge's record deletes
// the branch — a squash's commit does not reach it — so the record writes the
// ref drop an expunge keeps for its grace, as any deletion's: one row naming
// the source and the head, at the record's position. The target's move onto
// the merge's commit, whose old tip the commit still reaches, leaves none.
//
// Mutation: hand the merge's push its source deletion with no old object, or
// leave a deletion out of what leaves an object, and no drop names the head.
func TestAMergeThatDeletesItsSourceRecordsTheDrop(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo, change := mergeRepo(t, n, c, "web", code.DirectPersons, mergeGate())
	approved(t, n, w, repo, change)

	p := merging(repo, change, dev, code.MergeMethodSquash, oidOf("A"), oidOf("B"), oidOf("S"))
	p.Merge.DeleteSource = true
	res, err := c.ProposeMerge(t.Context(), p)
	requireMerged(t, res, err, oidOf("S"))
	if got := n.count(`SELECT COUNT(*) FROM code_ref_drops WHERE repo_id = ? AND position = ? AND ref = ?
			AND old = ?`, repo, res.Position.Packed(), seatRef, oidOf("B")); got != 1 {
		t.Fatalf("%d ref drops name the source %s the merge deleted at its head B, want one", got, seatRef)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_ref_drops WHERE repo_id = ?`, repo); got != 1 {
		t.Fatalf("the merge left %d ref drops, want the source's alone: the target's move leaves nothing", got)
	}
}

// A MERGE BUILT BEFORE A BEGIN IS REFUSED EXPUNGING (M6-D4): once an expunge's
// begin has moved its repository's drop epoch, a merge whose builder's copy
// held the epoch before — its objects read while the begin's dropping ones
// were live — is refused expunging, before any rule of its own, its sentence
// naming both epochs and its builder, and nothing is appended; built again at
// the begin's epoch, it merges. A drop epoch below zero is no proposal.
//
// Mutation: drop the decide's comparison of the epochs, and the merge built
// before the begin lands a target the expunge's commit could leave reaching
// an object it retired.
func TestAMergeBuiltBeforeABeginIsRefusedExpunging(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo, change := mergeRepo(t, n, c, "web", code.DirectPersons, mergeGate())
	approved(t, n, w, repo, change)
	b := beginFor(n, repo, 1, n.livePacks(repo), 1, 0)
	began, err := c.ProposeExpunge(t.Context(), code.ExpungeProposal{Begin: &b})
	requireExpunged(t, began, err)

	p := merging(repo, change, dev, code.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	before := n.end()
	res, err := c.ProposeMerge(t.Context(), p)
	requireMergeRefused(t, n, before, res, err, code.MergeRefusalExpunging,
		code.ExpungingBuiltDetail("web", "merge", "its builder", 0, 1))
	if !strings.Contains(res.Detail, "(drop epoch 0, now 1)") {
		t.Fatalf("the refusal says %q, want both epochs named", res.Detail)
	}
	if main := n.target(repo, mainRef); main != oidOf("A") {
		t.Fatalf("main is at %q after a refused merge, want A", main)
	}

	p.DropEpoch = 1
	res, err = c.ProposeMerge(t.Context(), p)
	requireMerged(t, res, err, oidOf("M"))

	p.DropEpoch = -1
	if err := p.Validate(); !errors.Is(err, code.ErrInvalidProposal) {
		t.Fatalf("a merge at drop epoch -1 validates: %v, want ErrInvalidProposal", err)
	}
}
