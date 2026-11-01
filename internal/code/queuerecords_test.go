package code_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A QUEUE'S OWN RECORDS — its trains, its ejections, its landings, its green
// and broken landings and its sweep — decided by the shipped committer from one
// snapshot on a real embedded broker and applied by the shipped applier.

// queuedPack is a pack named id, as a home's build of a train stores it.
func queuedPack(id string) *code.PackRef {
	return &code.PackRef{PackID: id, Object: objstore.KeyAt(harnessAt), Hash: objstore.HashOf([]byte(id)),
		Size: 32 + code.IndexBytesFor(1) + code.ReverseBytesFor(1), IndexBytes: code.IndexBytesFor(1),
		ReverseBytes: code.ReverseBytesFor(1), GitChecksum: oidOf("pack" + id), Objects: 1}
}

// training is a train of one candidate on main, built onto base: entry merged
// as commit — the queue's candidate of it, under an operation id of its own.
func training(repo, base, entry, commit string) code.QueueProposal {
	opID := statelog.NewOpID(time.Now(), "code.queue")
	train := code.QueueTrainID(opID)
	return code.QueueProposal{OpID: opID, Candidate: &code.Candidate{RepoID: repo, TargetRef: mainRef,
		Train: train, BaseOID: base, Pack: queuedPack(train), Candidates: []code.TrainCandidate{{
			CandidateID: code.QueueCandidateID(train, 1), Ordinal: 1, TipOID: commit,
			Entries: []code.CandidateEntry{{EntryID: entry, Commit: commit}}}}}}
}

// landingOf is a landing on main of entries — each change's entry and the
// commit it lands as, in the order main moves through them — onto base, a
// tested prefix of train, under an operation id of its own.
func landingOf(repo, train, base string, entries ...code.LandEntry) code.QueueProposal {
	opID := statelog.NewOpID(time.Now(), "code.queue")
	return code.QueueProposal{OpID: opID, Land: &code.Land{RepoID: repo, LandID: code.QueueLandID(opID),
		TargetRef: mainRef, BaseOID: base, TipOID: entries[len(entries)-1].Commit, Train: train,
		Entries: entries, Node: "git-a", ReceivedAt: harnessAt}}
}

// queueRecord is a record of the queue's own, under an operation id of its own.
func queueRecord(set func(*code.QueueProposal)) code.QueueProposal {
	p := code.QueueProposal{OpID: statelog.NewOpID(time.Now(), "code.queue")}
	set(&p)
	return p
}

// requireRecorded fails t unless res is a queue record applied at a position.
func requireRecorded(t *testing.T, res code.QueueResult, err error) statelog.Position {
	t.Helper()
	if err != nil {
		t.Fatalf("ProposeQueue: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() || res.Refusal != "" {
		t.Fatalf("the queue's record answered %+v, want applied at a position", res)
	}
	return res.Position
}

// checked writes attempt of check name on commit oid of repoID, checked for
// target, in state, straight into n's estate — an ending no runner of this
// build reports yet.
func (n *node) checked(repoID, oid, target, name string, attempt int, state code.CheckState) {
	n.t.Helper()
	at := store.EncodeTime(harnessAt)
	n.exec(`INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose, change_id,
		revision, candidate_id, entry_id, base_oid, defs_oid, required, run_in, timeout_seconds, reason,
		runner_node, placement, box_handle, borrowed, reporter, queued_at, started_at, finished_at,
		duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, overdue_at,
		url, summary, def, failures, tail)
		VALUES (?, ?, ?, ?, ?, ?, 'tip', '', 0, '', '', '', '', 1, '', 600, 'exit',
			'node-a', 'container', '', 0, '', ?, ?, ?, 1000, 0, 0, 1, 2, 4, ?, '', '', '', '', '')`,
		repoID, oid, target, name, attempt, string(state), at, at, at, store.EncodeTime(harnessAt.Add(600*time.Second)))
}

// landingRow is one of target's landing rows as n's rows hold it.
type landingRow struct {
	kind, change, entry, base, commit, by string
	green, broken                         bool
}

// landingsOf is every landing row of target on repo, in the order they landed.
func (n *node) landingsOf(repo, target string) []landingRow {
	n.t.Helper()
	var out []landingRow
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(), `SELECT kind, change_id, entry_id, base_oid, commit_oid,
			merged_by, green, broken FROM code_landings WHERE repo_id = ? AND target_ref = ?
			ORDER BY position, ordinal`, repo, target)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var l landingRow
			if err := rows.Scan(&l.kind, &l.change, &l.entry, &l.base, &l.commit, &l.by, &l.green,
				&l.broken); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	}); err != nil {
		n.t.Fatalf("read %s's landings: %v", target, err)
	}
	return out
}

// greenOf is target's green row on repo — its commit and position — and "" for
// none.
func (n *node) greenOf(repo, target string) (string, int64) {
	n.t.Helper()
	var (
		commit   string
		position int64
	)
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		err := tx.QueryRowContext(n.t.Context(), `SELECT commit_oid, position FROM code_green
			WHERE repo_id = ? AND target_ref = ?`, repo, target).Scan(&commit, &position)
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}); err != nil {
		n.t.Fatalf("read %s's green row: %v", target, err)
	}
	return commit, position
}

// A LANDING REPROVES EVERY ENTRY'S GATE (M5-D8): an enqueue is the gate, and
// the landing — the record that moves the branch — judges it again from its
// own snapshot with each entry's ENQUEUER as the merger. dev's change, queued
// by ada and tested green in its train, loses plat-lead's approval to a review
// asking for changes after it was enqueued: its landing is refused gate_unmet,
// naming the entry and the approval it no longer has, and appends nothing.
// Approved again, the same landing lands as one record: the change merged by
// the queue (merged_via queue, merged by ada, who queued it — the queue never
// acts on its own account), main moved to the train's tip, ONE PUSH ROW `via
// queue` naming ada's credential, the landing row green with main's green row
// at its tip, the entry landed and the train's landed candidate gone — and
// the stored readiness its home's merge stamped ready cleared, since a merged
// change is ready for nobody.
//
// Mutations: skip the re-proof (decide a landing without judging each entry's
// gate) — the landing applies over the review asking for changes, red; leave
// the stored readiness out of the landing's apply → it still reads ready, red.
func TestALandingReprovesEveryEntrysGate(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean))
	if stored := n.storedReadiness(change); !stored.ready {
		t.Fatalf("the home's merge stored %+v, want ready: approved, checked and merging cleanly", stored)
	}
	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	entryID := enqueue.Enqueue.EntryID

	train := training(repo, oidOf("A"), entryID, oidOf("M"))
	res, err = c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	if state, _ := n.entryState(repo, entryID); state != code.QueueTesting {
		t.Fatalf("an entry its train tests is %s, want testing", state)
	}
	n.passed(repo, oidOf("M"), mainRef, "unit")

	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictRequestChanges, nil))
	land := landingOf(repo, train.Candidate.Train, oidOf("A"),
		code.LandEntry{EntryID: entryID, ChangeID: change, Commit: oidOf("M")})
	before := n.end()
	res, err = c.ProposeQueue(t.Context(), land)
	requireQueueRefused(t, n, before, res, err, code.MergeRefusalGateUnmet, "would not land")
	if len(res.Refused) != 1 || res.Refused[0].EntryID != entryID || res.Refused[0].Refusal != code.MergeRefusalGateUnmet ||
		!slices.ContainsFunc(res.Refused[0].Unmet, func(p code.Precondition) bool {
			return p.Kind == code.PreconditionApproval
		}) {
		t.Fatalf("the landing's refusal names %+v, want entry %s refused gate_unmet for its approval", res.Refused,
			entryID)
	}
	if main := n.target(repo, mainRef); main != oidOf("A") {
		t.Fatalf("main is at %q, want A: a landing whose entry's gate no longer holds moves nothing", main)
	}

	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil))
	res, err = c.ProposeQueue(t.Context(), land)
	requireRecorded(t, res, err)
	if main := n.target(repo, mainRef); main != oidOf("M") {
		t.Fatalf("main is at %q after its landing, want the train's tip", main)
	}
	if m := n.mergedChange(change); m.state != string(code.ChangeStateMerged) || m.via != string(code.MergedViaQueue) ||
		m.mergeOID != oidOf("M") || m.by != "founder" || m.byKind != string(code.PartyPerson) ||
		m.operator != "pat:ada" {
		t.Fatalf("the landed change reads %+v, want merged via queue at M by ada, who queued it", m)
	}
	push := code.LandPushID(land.Land.LandID, entryID)
	if pushes := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ? AND via = 'queue'
			AND actor = 'pat:ada' AND actor_seat = 'founder' AND introduced = ?`, push, oidOf("M")); pushes != 1 {
		t.Fatalf("%d push rows record the landing as ada's push via queue introducing M, want one", pushes)
	}
	if got := n.landingsOf(repo, mainRef); len(got) != 1 || got[0].kind != string(code.LandedPreLand) ||
		got[0].entry != entryID || got[0].base != oidOf("A") || got[0].commit != oidOf("M") || !got[0].green {
		t.Fatalf("main's landings read %+v, want one green pre_land landing of the entry from A to M", got)
	}
	if green, _ := n.greenOf(repo, mainRef); green != oidOf("M") {
		t.Fatalf("main's green row is at %q, want the landing's tip", green)
	}
	if state, _ := n.entryState(repo, entryID); state != code.QueueLanded {
		t.Fatalf("the landed entry is %s, want landed", state)
	}
	if left := n.count(`SELECT COUNT(*) FROM code_candidates WHERE repo_id = ?`, repo); left != 0 {
		t.Fatalf("%d candidates outlive the train their landing landed whole", left)
	}
	if stored := n.storedReadiness(change); stored.ready || stored.waitsPerson {
		t.Fatalf("the landed change stores %+v, want ready for nobody and waiting on nobody", stored)
	}
}

// A LAND RECORD FITS ITS CAPS: a train's whole tested prefix — sixty-four
// changes, MaxLandEntries, as many as a train's candidates take — is ONE land
// record, its payload within what one proposal may carry and its rows but the
// changes its pushes move within MaxRecordRows with the horizons' room, which
// the decide adds the moved changes to and holds whole; one entry more is
// refused before it is queued.
//
// Mutation: take one entry past MaxLandEntries in a land's validation — the
// 65th is taken, red.
func TestALandRecordFitsItsCaps(t *testing.T) {
	t.Parallel()
	repo, train := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	entries := make([]code.LandEntry, code.MaxLandEntries+1)
	for i := range entries {
		entries[i] = code.LandEntry{EntryID: uuid.Must(uuid.NewV7()).String(),
			ChangeID: uuid.Must(uuid.NewV7()).String(), Commit: oidOf(fmt.Sprint("commit", i))}
	}
	if code.MaxCandidateEntries > code.MaxLandEntries {
		t.Fatalf("a train takes %d entries and a land record lands %d: its whole prefix would not land as one",
			code.MaxCandidateEntries, code.MaxLandEntries)
	}
	full := landingOf(repo, train, oidOf("A"), entries[:code.MaxLandEntries]...)
	if err := full.Validate(); err != nil {
		t.Fatalf("a landing of %d changes is refused: %v", code.MaxLandEntries, err)
	}
	if got := code.RecordRows(code.LandRecordRows(code.MaxLandEntries)); got > code.MaxRecordRows {
		t.Fatalf("a land of %d entries writes up to %d rows with its horizons' room, past MaxRecordRows (%d)",
			code.MaxLandEntries, got, code.MaxRecordRows)
	}
	over := landingOf(repo, train, oidOf("A"), entries...)
	if err := over.Validate(); err == nil || !strings.Contains(err.Error(), "MaxLandEntries") {
		t.Fatalf("a landing of %d changes validated (%v), want refused past MaxLandEntries", len(entries), err)
	}
}

// GREEN ADVANCES ONLY ON GREEN (M5-D16): dev's change lands ahead of its checks
// by a person's override merge past main's queue — its landing row not green,
// main's green row untouched. Its required check FAILS on the landed tip: a
// green naming that landing is refused not_green, appending nothing, and a
// broken marks the landing broken while main's green row stays where it was.
// The check run again PASSES: the
// green then moves main's green row onto the landing, at the landing's own
// position — so a read of main's landings above its green lists what came
// after it — and marks the landing green; asked again, it is refused
// no_landing, the row already there.
//
// Mutation: move the green row on a broken record — the green row reads M
// after the broken, red; judge a green's checks by their first attempt — the
// failed one, so the second green is refused, red.
func TestGreenAdvancesOnlyOnGreen(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	converse(t, w, n.revising(repo, change, 1, oidOf("B"), mainRef, nil))
	override := merging(repo, change, ada, code.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	override.Merge.Override = "the queue's runner is down"
	merged, err := c.ProposeMerge(t.Context(), override)
	requireMerged(t, merged, err, oidOf("M"))
	landed := merged.Position
	if got := n.landingsOf(repo, mainRef); len(got) != 1 || got[0].kind != string(code.LandedDirect) ||
		got[0].green {
		t.Fatalf("main's landings read %+v, want one direct landing, not green", got)
	}
	if green, _ := n.greenOf(repo, mainRef); green != "" {
		t.Fatalf("main's green row is at %q: an override lands ahead of its checks", green)
	}

	n.checked(repo, oidOf("M"), mainRef, "unit", 1, code.CheckStateFailed)
	green := queueRecord(func(p *code.QueueProposal) {
		p.Green = &code.Green{RepoID: repo, TargetRef: mainRef, OID: oidOf("M"), Through: landed}
	})
	before := n.end()
	res, err := c.ProposeQueue(t.Context(), green)
	requireQueueRefused(t, n, before, res, err, code.MergeRefusalNotGreen, "unit (failed, attempt 1)")
	broken := queueRecord(func(p *code.QueueProposal) {
		p.Broken = &code.Broken{RepoID: repo, TargetRef: mainRef, Position: landed, Ordinal: 1, OID: oidOf("M"),
			Checks: []string{"unit"}}
	})
	res, err = c.ProposeQueue(t.Context(), broken)
	requireRecorded(t, res, err)
	if got := n.landingsOf(repo, mainRef); len(got) != 1 || !got[0].broken || got[0].green {
		t.Fatalf("main's landings read %+v, want its landing broken and not green", got)
	}
	if at, _ := n.greenOf(repo, mainRef); at != "" {
		t.Fatalf("main's green row is at %q after a broken landing: it only ever advances on green", at)
	}

	n.checked(repo, oidOf("M"), mainRef, "unit", 2, code.CheckStatePassed)
	res, err = c.ProposeQueue(t.Context(), green)
	requireRecorded(t, res, err)
	if at, position := n.greenOf(repo, mainRef); at != oidOf("M") || position != landed.Packed() {
		t.Fatalf("main's green row is at %q, %d, want M at the landing's position %d", at, position,
			landed.Packed())
	}
	if got := n.landingsOf(repo, mainRef); len(got) != 1 || !got[0].green {
		t.Fatalf("main's landings read %+v, want its landing green", got)
	}
	before = n.end()
	res, err = c.ProposeQueue(t.Context(), queueRecord(func(p *code.QueueProposal) {
		p.Green = &code.Green{RepoID: repo, TargetRef: mainRef, OID: oidOf("M"), Through: landed}
	}))
	requireQueueRefused(t, n, before, res, err, code.MergeRefusalNoLanding, "at or past")
}

// A WIDE LANDING NAMES THE WHOLE DOMAIN (M5-D33): a landing's decide proves
// every entry's gate, which reads each change's conversation, so its record
// names each change's path beside the repository's and its checks' — while
// they fit MaxScopePaths, fourteen entries; past it the domain, which covers
// them all and which a node holding a deferred record of any of them never
// decides on. Narrower records name what they read: a green and a broken the
// repository's checks beside it, a train and an ejection the repository alone.
//
// Mutation: name every entry's change whatever their number — fifteen entries
// name seventeen paths, past MaxScopePaths, red.
func TestAWideLandingNamesTheWholeDomain(t *testing.T) {
	t.Parallel()
	repo := uuid.Must(uuid.NewV7()).String()
	entries := make([]code.LandEntry, code.MaxScopePaths-1)
	for i := range entries {
		entries[i] = code.LandEntry{EntryID: uuid.Must(uuid.NewV7()).String(),
			ChangeID: uuid.Must(uuid.NewV7()).String(), Commit: oidOf(strings.Repeat("c", i+1))}
	}
	train := uuid.Must(uuid.NewV7()).String()
	fits := landingOf(repo, train, oidOf("A"), entries[:code.MaxScopePaths-2]...)
	scope := code.QueueScope(fits)
	if len(scope) != code.MaxScopePaths || scope[0] != code.RepoPath(repo) || scope[1] != code.ChecksPath(repo) {
		t.Fatalf("a landing of %d entries names %v, want the repository, its checks and each change",
			len(fits.Land.Entries), scope)
	}
	for i, e := range fits.Land.Entries {
		if scope[i+2] != code.ChangePath(e.ChangeID) {
			t.Fatalf("a landing's scope names %q in place of entry %d's change", scope[i+2], i)
		}
	}
	wide := landingOf(repo, train, oidOf("A"), entries...)
	if scope := code.QueueScope(wide); !slices.Equal(scope, []string{code.DomainPath()}) {
		t.Fatalf("a landing of %d entries names %v, want the whole domain", len(entries), scope)
	}
	for name, tc := range map[string]struct {
		p    code.QueueProposal
		want []string
	}{
		"a green": {queueRecord(func(p *code.QueueProposal) {
			p.Green = &code.Green{RepoID: repo}
		}), []string{code.RepoPath(repo), code.ChecksPath(repo)}},
		"a broken": {queueRecord(func(p *code.QueueProposal) {
			p.Broken = &code.Broken{RepoID: repo}
		}), []string{code.RepoPath(repo), code.ChecksPath(repo)}},
		"a train": {queueRecord(func(p *code.QueueProposal) {
			p.Candidate = &code.Candidate{RepoID: repo}
		}), []string{code.RepoPath(repo)}},
		"an ejection": {queueRecord(func(p *code.QueueProposal) {
			p.Eject = &code.Eject{RepoID: repo}
		}), []string{code.RepoPath(repo)}},
	} {
		if got := code.QueueScope(tc.p); !slices.Equal(got, tc.want) {
			t.Errorf("%s names %v, want %v", name, got, tc.want)
		}
	}
}

// AN IDLE TARGET'S LANDINGS ARE SWEPT (M5-D34): a repository whose queue keeps
// working removes its own history with every train, ejection and landing; one
// whose queue stopped has nothing of its own that ever would — so the
// retention pass lists the repositories holding an ended entry, or a landing
// below its target's green row, from before its clock less QueueHistory less
// QueueSweepGrace, and publishes a queue_sweep on each one's subject removing
// every such row past the history. The landing AT the green row, and those
// above it — what its tip checks have yet to judge — stay, as do live
// entries; and a repository whose history is past the history but inside the
// grace is not listed.
//
// Mutation: leave the queue step out of the pass — the idle repository keeps
// its rows, red; sweep landings at or above the green row — the green landing
// goes, red.
func TestAnIdleTargetsLandingsAreSwept(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("idle", described("idle"), t1)
	n.ensure("recent", described("recent"), t1)
	idle, recent := n.mustID("idle"), n.mustID("recent")
	old := store.EncodeTime(harnessAt.Add(-code.QueueHistory - code.QueueSweepGrace - time.Hour))
	inGrace := store.EncodeTime(harnessAt.Add(-code.QueueHistory - time.Hour))
	seedEntry := func(repo, state string, ended int64) {
		n.exec(`INSERT INTO code_queue (repo_id, entry_id, target_ref, change_id, seq, head_oid, method,
			delete_source, close_task, enqueued_by, enqueued_kind, enqueued_operator, state, enqueued_at,
			ended_at, ended_position, end_reason)
			VALUES (?, ?, 'refs/heads/main', ?, 1, ?, 'merge', 0, 0, 'dev', 'agent', '', ?, ?, ?, 2, '')`,
			repo, uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String(), oidOf("B"), state, old, ended)
	}
	seedLanding := func(repo string, position int64, at int64) {
		n.exec(`INSERT INTO code_landings (repo_id, target_ref, position, ordinal, kind, base_oid, commit_oid,
			merged_by, at) VALUES (?, 'refs/heads/main', ?, 1, 'push', ?, ?, 'founder', ?)`,
			repo, position, oidOf("A"), oidOf("B"), at)
	}
	seedEntry(idle, "landed", old)
	seedEntry(idle, "waiting", 0)
	seedLanding(idle, 500, old)
	seedLanding(idle, 1000, old)
	seedLanding(idle, 1500, old)
	n.exec(`INSERT INTO code_green (repo_id, target_ref, commit_oid, position, at)
		VALUES (?, 'refs/heads/main', ?, 1000, ?)`, idle, oidOf("B"), old)
	seedLanding(recent, 500, inGrace)
	n.exec(`INSERT INTO code_green (repo_id, target_ref, commit_oid, position, at)
		VALUES (?, 'refs/heads/main', ?, 1000, ?)`, recent, oidOf("B"), inGrace)
	n.storedAt = func(uint64, time.Time) time.Time { return harnessAt }
	before := n.end()

	var p purged
	report, err := passOver(n.store, n.reader, &p).Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if report.QueueSweeps != 1 || report.QueueSweptRows != 2 || len(report.Failed) != 0 || report.Remain {
		t.Fatalf("the pass reported %+v; want one queue sweep of the idle repository's two rows", report)
	}
	want := []string{fmt.Sprintf("%s %s", code.OpQueueSweep, code.RepoSubject(idle))}
	if got := n.opsAfter(before); !slices.Equal(got, want) {
		t.Fatalf("the pass appended %v, want %v", got, want)
	}
	if left := n.count(`SELECT COUNT(*) FROM code_queue WHERE repo_id = ?`, idle); left != 1 {
		t.Fatalf("the idle repository keeps %d queue entries, want its live one alone", left)
	}
	if kept := n.count(`SELECT COUNT(*) FROM code_landings WHERE repo_id = ? AND position >= 1000`, idle); kept != 2 {
		t.Fatalf("the idle repository keeps %d of its landings at and above its green row, want both", kept)
	}
	if left := n.count(`SELECT COUNT(*) FROM code_landings WHERE repo_id = ? AND position < 1000`, idle); left != 0 {
		t.Fatalf("the idle repository keeps %d landings below its green row past the history", left)
	}
	if kept := n.count(`SELECT COUNT(*) FROM code_landings WHERE repo_id = ?`, recent); kept != 1 {
		t.Fatalf("the recent repository keeps %d of its 1 landing inside the grace", kept)
	}
	again, err := passOver(n.store, n.reader, &p).Pass(t.Context())
	if err != nil || again.QueueSweeps != 0 {
		t.Fatalf("a second pass swept again (%+v, %v): nothing is past the history by the grace now", again, err)
	}
}

// A PERSON'S PUSH LEAVES A LANDING ROW (M5-D17): every record that moves a
// protected branch leaves its landing — so a branch's tip checks judge, and
// its green row follows, every way it moved. ada's own push to main under
// direct_push persons, their recorded override, lands main's landing row of
// kind push naming their human seat, green where main's gate requires no check
// and main's green row moving to it; on a branch whose gate requires a check
// the landing is not green and the green row stays; and a push to a branch no
// rule protects leaves none.
//
// Mutation: stamp no landing on a push — main holds no landing row, red.
func TestAPersonsPushLeavesALandingRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		gate  *code.MergeGate
		green bool
	}{
		{"a gate requiring no check", mergeGate(), true},
		{"a gate requiring a check", queueGate(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			n.chart = testChart()
			c, _ := mergeStream(t, n)
			repo, _ := mergeRepo(t, n, c, "web", code.DirectPersons, tc.gate)
			push := proposal(repo, update(mainRef, oidOf("A"), oidOf("Y")), update(releaseRef, code.ZeroOID,
				oidOf("Z")))
			got, err := c.Propose(t.Context(), push)
			if err != nil || len(got.Answer.Commands) != 2 || got.Answer.Commands[0].Result != code.ResultOK {
				t.Fatalf("ada's push answered %+v (%v), want it landed", got.Answer, err)
			}
			landings := n.landingsOf(repo, mainRef)
			if len(landings) != 1 || landings[0].kind != string(code.LandedPush) || landings[0].change != "" ||
				landings[0].base != oidOf("A") || landings[0].commit != oidOf("Y") || landings[0].by != "ada" ||
				landings[0].green != tc.green {
				t.Fatalf("main's landings read %+v, want ada's push from A to Y, green %v", landings, tc.green)
			}
			want := ""
			if tc.green {
				want = oidOf("Y")
			}
			if green, _ := n.greenOf(repo, mainRef); green != want {
				t.Fatalf("main's green row is at %q, want %q", green, want)
			}
			if others := n.count(`SELECT COUNT(*) FROM code_landings WHERE repo_id = ? AND target_ref != ?`, repo,
				mainRef); others != 0 {
				t.Fatalf("%d landings on branches no rule protects", others)
			}
		})
	}
}

// A PERSON'S PUSH LANDING ON MORE PROTECTED BRANCHES THAN A RECORD HOLDS IS
// REFUSED WHOLE: every landing a push makes on a protected branch is two rows of
// its record — the landing row and the branch's green row — which the push's
// decide counts from the rules its snapshot holds, since the committer's
// batching bound cannot know which branches a rule protects. ada's push of
// MaxCommandsPerPush forced updates, each onto a branch a rule protects (force
// allowed, direct pushes a person's), would take even a record of its own past
// MaxRecordRows: it is refused too_many_changes, every command ng under the
// code in one sentence naming its landings, appending nothing. The same updates
// in two pushes land, every branch's landing row written, and no record's apply
// writes past code.MaxRecordRows.
//
// Mutation: count no landing among a decided entry's rows — the push is
// admitted, and its record's apply writes past the bound, red.
func TestAPushLandingOnMoreProtectedBranchesThanARecordHoldsIsRefusedWhole(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	var widest int
	n.applied = func(_ uint64, rows int) { widest = max(widest, rows) }
	s := suiteSettings()
	s.Protections = append(s.Protections, code.Protection{PushRule: code.PushRule{Pattern: "refs/heads/release/*",
		AllowForce: true, DirectPush: code.DirectPersons}})
	repo := n.ensure("web", s, t1).RepoID
	branches := code.MaxCommandsPerPush
	ref := func(i int) string { return fmt.Sprintf("refs/heads/release/%06d", i) }
	seeded := make([]code.Command, branches)
	for i := range seeded {
		seeded[i] = set(ref(i), code.ZeroOID, oidOf(fmt.Sprint("old", i)))
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), seeded...))
	c, _ := committerOn(t, n, n.pub)

	forced := make([]code.Command, branches)
	for i := range forced {
		forced[i] = code.Command{Ref: ref(i), Old: oidOf(fmt.Sprint("old", i)), New: oidOf(fmt.Sprint("new", i)),
			Forced: true}
	}
	before := n.end()
	res, err := c.Propose(t.Context(), proposal(repo, forced...))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if res.Answer.Refused != code.RefusalTooManyChanges || res.Outcome != "" || n.end() != before ||
		!strings.Contains(res.Answer.Detail, fmt.Sprintf("lands on %d protected branches", branches)) {
		t.Fatalf("a push landing on %d protected branches answered %+v (%q) and appended %d records; want "+
			"too_many_changes naming its landings, nothing appended", branches, res, res.Answer.Detail,
			n.end()-before)
	}
	for _, cmd := range res.Answer.Commands {
		if cmd.Result != code.ResultNG || cmd.Reason != code.RefusalTooManyChanges {
			t.Fatalf("%s answered %s %s, want ng under too_many_changes", cmd.Ref, cmd.Result, cmd.Reason)
		}
	}

	half := branches / 2
	for _, part := range [][]code.Command{forced[:half], forced[half:]} {
		res, err := c.Propose(t.Context(), proposal(repo, part...))
		requireOK(t, answer{res, err})
	}
	if landed := n.count(`SELECT COUNT(*) FROM code_landings WHERE repo_id = ? AND kind = 'push'`,
		repo); landed != branches {
		t.Fatalf("%d landing rows after the push in halves, want one for each of the %d branches", landed,
			branches)
	}
	if widest > code.MaxRecordRows {
		t.Fatalf("a record's apply wrote %d rows, past the %d one record may", widest, code.MaxRecordRows)
	}
}

// AN EJECTED ENTRY LEAVES ITS QUEUE AND ITS TRAIN: an eject puts a live entry
// out — ejected, why and the checks that failed kept on its row — abandons the
// train it was tested in, its candidates gone, and the node whose committer
// landed the record announces one code_queue_ejected naming the change, the
// branch and why. An eject of an entry no longer live is refused not_queued.
//
// Mutation: keep the train when its entry is ejected — its candidate outlives
// the ejection, red.
func TestAnEjectedEntryLeavesItsQueueAndItsTrain(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, stream := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	entryID := enqueue.Enqueue.EntryID
	res, err = c.ProposeQueue(t.Context(), training(repo, oidOf("A"), entryID, oidOf("M")))
	requireRecorded(t, res, err)

	eject := queueRecord(func(p *code.QueueProposal) {
		p.Eject = &code.Eject{RepoID: repo, Entries: []code.Ejection{{EntryID: entryID,
			Reason: code.EjectCheckFailed, Checks: []string{"unit"}}}}
	})
	res, err = c.ProposeQueue(t.Context(), eject)
	requireRecorded(t, res, err)
	if state, reason := n.entryState(repo, entryID); state != code.QueueEjected ||
		reason != string(code.EjectCheckFailed) {
		t.Fatalf("the ejected entry is %s (%s), want ejected, check_failed", state, reason)
	}
	if left := n.count(`SELECT COUNT(*) FROM code_candidates WHERE repo_id = ?`, repo); left != 0 {
		t.Fatalf("%d candidates outlive the train their entry was ejected from", left)
	}
	waitAnnouncedOf(t, stream, "code_queue_ejected", 1)
	ev := stream.of("code_queue_ejected")[0]
	payload, ok := ev.Data.(*types.CodeQueueEjected)
	if !ok || ev.Type != "code_queue_ejected" || payload.ChangeID != change || payload.Number != 1 ||
		payload.EntryID != entryID || payload.Reason != string(code.EjectCheckFailed) ||
		!slices.Equal(payload.Checks, []string{"unit"}) || payload.EnqueuedBy != "founder" ||
		payload.RepoName != "web" || ev.Source != n.id || payload.Owner != "dev" || payload.OwnerKind != "agent" ||
		payload.OwnerAgent != "agent-dev" {
		t.Fatalf("the ejection was announced as %s from %q %+v, want web!1 ejected for unit, owned by dev — "+
			"its day counting it — from %s", ev.Type, ev.Source, ev.Data, n.id)
	}
	before := n.end()
	res, err = c.ProposeQueue(t.Context(), queueRecord(func(p *code.QueueProposal) {
		p.Eject = &code.Eject{RepoID: repo, Entries: []code.Ejection{{EntryID: entryID, Reason: code.EjectConflict}}}
	}))
	requireQueueRefused(t, n, before, res, err, code.MergeRefusalNotQueued, "is ejected")
}

// A PUSH THAT EJECTS A QUEUED HEAD ANNOUNCES IT: a push moving a queued
// change's source branch ends its entry ejected head_moved in the push's own
// record, which no queue record names — so the committer that landed the push
// announces one code_queue_ejected for that entry, naming the change, why and
// its owner dev, whose day counts it (`ejections`), from the node that landed
// it.
//
// Mutation: never announce a push's ejections (drop commit's call of
// announcePushEjections) — no code_queue_ejected follows the push, red.
func TestAPushThatEjectsAQueuedHeadAnnouncesIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, stream := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	first := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), first)
	requireQueued(t, res, err, code.QueueWaiting, 1)

	push := func(old, new string) {
		t.Helper()
		pushed, err := c.Propose(t.Context(), proposal(repo, update(featureRef, oidOf(old), oidOf(new))))
		if err != nil || pushed.Position.IsZero() {
			t.Fatalf("the push of %s answered %+v, %v", new, pushed, err)
		}
	}
	push("B", "B2")
	waitAnnouncedOf(t, stream, "code_queue_ejected", 1)
	ev := stream.of("code_queue_ejected")[0]
	payload, ok := ev.Data.(*types.CodeQueueEjected)
	if !ok || payload.EntryID != first.Enqueue.EntryID || payload.ChangeID != change || payload.Number != 1 ||
		payload.Reason != string(code.EjectHeadMoved) || payload.RepoName != "web" || ev.Source != n.id ||
		payload.Owner != "dev" || payload.OwnerKind != "agent" || payload.OwnerAgent != "agent-dev" {
		t.Fatalf("the push's ejection was announced from %q as %+v, want web!1's entry %s ejected head_moved, "+
			"owned by dev — its day counting it — from %s", ev.Source, ev.Data, first.Enqueue.EntryID, n.id)
	}
}

// purgeTips is every object an `expunge_tips` read of repo names from since,
// every page of it, n's reader paging every pageRows rows.
func (n *node) purgeTips(repo string, since time.Time, pageRows int) []string {
	n.t.Helper()
	code.SetPageLimits(n.reader, pageRows, 1<<20)
	var tips []string
	q := code.ReadQuery{Kind: code.ReadExpungeTips, ExpungeTips: &code.ExpungeTipsQuery{RepoID: repo, Since: since},
		Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	for {
		a, err := n.reader.Read(n.t.Context(), q)
		if err != nil {
			n.t.Fatalf("the expunge's tips of %s: %v", repo, err)
		}
		tips = append(tips, a.Tips...)
		if a.Next == "" {
			return tips
		}
		q.Cursor = a.Next
	}
}

// A CANDIDATE'S TIP SURVIVES A PURGE (M6-D2): a candidate is rows and a pack,
// never a ref, so its tip — the commit a landing moves its target
// onto — is reached by no ref; a purge, which keeps no drop, keeps it all the
// same, because the read its scan walks from names what rows hold in flight
// whatever its instant: the candidate's tip, the live entry's head and the
// open change's revision head, and — once a landing moves main — main's green
// row. Read a row a page, every phase resumes after the place its cursor
// names and nothing is skipped; once the train lands, its candidate and its
// entry are history and name nothing.
//
// Mutation: drop the candidates' phase from the read, and a purge drops the
// tip a pending landing moves main onto — a landing onto a missing tip; drop
// the green phase, and the commit mirrors follow goes too; drop the queue's,
// and the live entry's row is missing from the pages.
func TestACandidateTipSurvivesAPurge(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	train := training(repo, oidOf("A"), enqueue.Enqueue.EntryID, oidOf("M"))
	res, err = c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	asked := purgeOf(repo)
	purged, err := c.ProposeExpunge(t.Context(), code.ExpungeProposal{Request: &asked})
	requireExpunged(t, purged, err)

	// ONE PAGE answers each object once; A ROW A PAGE answers each phase's
	// row on a page of its own, in the phases' order — the change's revision
	// at B, the live entry at B, the candidate at M — so a phase left out is
	// a row missing, though another phase names the same object.
	now := time.Now()
	if got := n.purgeTips(repo, now, code.MaxSnapshotItems); !slices.Equal(got, []string{oidOf("B"), oidOf("M")}) {
		t.Fatalf("a purge's tips in one page are %v; want the head B the change's revision and the entry "+
			"hold, then the candidate's tip M", got)
	}
	if got := n.purgeTips(repo, now, 1); !slices.Equal(got, []string{oidOf("B"), oidOf("B"), oidOf("M")}) {
		t.Fatalf("a purge's tips a row a page are %v; want the revision's head B, the live entry's B and the "+
			"candidate's tip M, in that order", got)
	}

	n.passed(repo, oidOf("M"), mainRef, "unit")
	land := landingOf(repo, train.Candidate.Train, oidOf("A"),
		code.LandEntry{EntryID: enqueue.Enqueue.EntryID, ChangeID: change, Commit: oidOf("M")})
	res, err = c.ProposeQueue(t.Context(), land)
	requireRecorded(t, res, err)
	if got := n.purgeTips(repo, now, code.MaxSnapshotItems); !slices.Equal(got, []string{oidOf("M")}) {
		t.Fatalf("after the landing a purge's tips are %v; want main's green row at M alone: the landed "+
			"candidate, its entry and the merged change's revisions are history", got)
	}
}

// A PURGE'S TIPS READ EVERY REVISION OF A CHANGE OPENED BETWEEN ITS PAGES
// (M6-D2: the tip set is exact): a branch heads one live change at a time, but
// not the same one from page to page — the change a revisions page stopped in
// closed, and another opened on its branch, before the next page is read. The
// cursor names the change it stopped in, so the next page reads the new
// change from its first revision, never from the number its predecessor
// reached.
//
// Mutation: resume a branch's change at the cursor's revision whichever change
// it is, and the new change's first revision, its head C, is never named.
func TestAPurgesTipsReadEveryRevisionOfAChangeOpenedBetweenItsPages(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	repo, first := queueRepo(t, n, c, "web")

	code.SetPageLimits(n.reader, 1, 1<<20)
	q := code.ReadQuery{Kind: code.ReadExpungeTips, ExpungeTips: &code.ExpungeTipsQuery{RepoID: repo,
		Since: time.Now()}, Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	a, err := n.reader.Read(t.Context(), q)
	if err != nil {
		t.Fatalf("the first page of the tips: %v", err)
	}
	if !slices.Equal(a.Tips, []string{oidOf("B")}) || a.Next == "" {
		t.Fatalf("the first page of the tips is %v (next %q); want the change's revision 1 at B, and more",
			a.Tips, a.Next)
	}

	// BETWEEN THE PAGES the change closes, feature moves to C, a second change
	// opens on feature at C — its revision 1 — and feature moves on to D, its
	// revision 2.
	closed, err := c.ProposeChange(t.Context(), closing(repo, first, dev, 1))
	requireChanged(t, closed, err)
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("B"), oidOf("C"))))
	opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("C"), oidOf("D"))))
	if got := n.revisions(opened.ChangeID); len(got) != 2 || got[0].head != oidOf("C") ||
		got[1].head != oidOf("D") {
		t.Fatalf("the premise: the second change's revisions are %+v, want C then D", got)
	}

	var rest []string
	for q.Cursor = a.Next; q.Cursor != ""; q.Cursor = a.Next {
		if a, err = n.reader.Read(t.Context(), q); err != nil {
			t.Fatalf("a later page of the tips: %v", err)
		}
		rest = append(rest, a.Tips...)
	}
	if !slices.Equal(rest, []string{oidOf("C"), oidOf("D")}) {
		t.Fatalf("the pages after the first name %v; want the second change's revisions C and D", rest)
	}
}

// A TRAIN OR A LANDING BUILT BEFORE A BEGIN IS REFUSED EXPUNGING (M6-D4): the
// queue's own records that move a ref onto what a build made carry the drop
// epoch their home's copy held, and once an expunge's begin has moved the
// repository's epoch, one built at the epoch before is refused expunging,
// nothing appended, its sentence naming its home; built again at the begin's
// epoch, it lands. No other queue record carries an epoch.
//
// Mutation: drop the queue decide's comparison of the epochs, and the train
// built before the begin is recorded; compare them for a candidate alone, and
// the landing built before the begin moves main.
func TestATrainBuiltBeforeABeginIsRefusedExpunging(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	b := beginFor(n, repo, 1, n.livePacks(repo), 1, 0)
	began, err := c.ProposeExpunge(t.Context(), code.ExpungeProposal{Begin: &b})
	requireExpunged(t, began, err)

	train := training(repo, oidOf("A"), enqueue.Enqueue.EntryID, oidOf("M"))
	before := n.end()
	res, err = c.ProposeQueue(t.Context(), train)
	requireQueueRefused(t, n, before, res, err, code.MergeRefusalExpunging,
		code.ExpungingBuiltDetail("web", "train", "its home", 0, 1))
	train.DropEpoch = 1
	res, err = c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)

	// THE LANDING its home built from a copy at the epoch before the begin is
	// refused too, main left where it was; built at the begin's epoch, it
	// moves main onto the train's tip.
	n.passed(repo, oidOf("M"), mainRef, "unit")
	land := landingOf(repo, train.Candidate.Train, oidOf("A"),
		code.LandEntry{EntryID: enqueue.Enqueue.EntryID, ChangeID: change, Commit: oidOf("M")})
	before = n.end()
	res, err = c.ProposeQueue(t.Context(), land)
	requireQueueRefused(t, n, before, res, err, code.MergeRefusalExpunging,
		code.ExpungingBuiltDetail("web", "landing", "its home", 0, 1))
	if main := n.target(repo, mainRef); main != oidOf("A") {
		t.Fatalf("main is at %q after a refused landing, want A", main)
	}
	land.DropEpoch = 1
	res, err = c.ProposeQueue(t.Context(), land)
	requireRecorded(t, res, err)
	if main := n.target(repo, mainRef); main != oidOf("M") {
		t.Fatalf("main is at %q after the landing at the begin's epoch, want the train's tip M", main)
	}

	dequeue := queueRecord(func(p *code.QueueProposal) {
		p.Asker, p.Dequeue = ada, &code.Dequeue{RepoID: repo, EntryID: enqueue.Enqueue.EntryID}
	})
	dequeue.DropEpoch = 1
	if err := dequeue.Validate(); err == nil || !strings.Contains(err.Error(), "drop_epoch") {
		t.Fatalf("a dequeue carrying a drop epoch validates: %v, want drop_epoch refused", err)
	}
}

// A QUEUE READ SAYS WHAT ITS QUEUES PUT OUT LATELY: a read of a repository
// answers, beside its live entries, the entry its queue ejected — the change by
// its number, who enqueued it, why, and the paths its merge conflicted on —
// and so does a read of that entry's target; a read of another target, a read
// of every repository and a read a day after the ejection answer none.
//
// Mutations: drop the ejections from the queue listing — the first read
// answers none, red; drop the target filter from the read — the read of
// another target answers the ejection, red; drop the horizon — the read a
// day later answers it, red.
func TestAQueueReadSaysWhatItsQueuesPutOutLately(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	entryID := enqueue.Enqueue.EntryID
	res, err = c.ProposeQueue(t.Context(), queueRecord(func(p *code.QueueProposal) {
		p.Eject = &code.Eject{RepoID: repo, Entries: []code.Ejection{{EntryID: entryID,
			Reason: code.EjectConflict, Paths: []string{"go.mod", "main.go"}}}}
	}))
	requireRecorded(t, res, err)
	var endedMicros int64
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT ended_at FROM code_queue WHERE repo_id = ? AND entry_id = ?`,
			repo, entryID).Scan(&endedMicros)
	}); err != nil {
		t.Fatalf("read the ejection's instant: %v", err)
	}
	ejectedAt := time.UnixMicro(endedMicros).UTC()
	soon := ejectedAt.Add(time.Hour)
	n.readAt.Store(&soon)

	read := func(q code.ReadQuery) []code.EjectedRow {
		t.Helper()
		q.Kind, q.Asker = code.ReadQueue, ada
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("read the queue %+v: %v", q, err)
		}
		return answer.Ejected
	}
	for _, q := range []code.ReadQuery{{Repo: "web"}, {Repo: "web", Target: "refs/heads/main"}} {
		got := read(q)
		if len(got) != 1 || got[0].EntryID != entryID || got[0].Number != 1 || got[0].Repo != "web" ||
			got[0].Reason != code.EjectConflict || !slices.Equal(got[0].Paths, []string{"go.mod", "main.go"}) ||
			got[0].EnqueuedBy.Handle != "founder" || !got[0].EjectedAt.Equal(ejectedAt) {
			t.Fatalf("a read of %+v answers the ejections %+v, want web!1's, conflicted on go.mod and main.go",
				q, got)
		}
	}
	if got := read(code.ReadQuery{Repo: "web", Target: releaseRef}); len(got) != 0 {
		t.Errorf("a read of %s answers main's ejection: %+v", releaseRef, got)
	}
	if got := read(code.ReadQuery{}); len(got) != 0 {
		t.Errorf("a read of every repository answers ejections %+v, want none: a named repository's alone", got)
	}
	later := ejectedAt.Add(code.QueueEjectionsShown + time.Minute)
	n.readAt.Store(&later)
	if got := read(code.ReadQuery{Repo: "web"}); len(got) != 0 {
		t.Errorf("a read a day after the ejection still answers it: %+v", got)
	}
}
