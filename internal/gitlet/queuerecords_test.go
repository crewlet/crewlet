package gitlet_test

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A queue's own records (trains, ejections, landings, green and broken
// landings, the sweep), decided by the shipped committer on a real embedded
// broker and applied by the shipped applier.

// queuedPack is a pack named id, as a home's build of a train stores it.
func queuedPack(id string) *gitlet.PackRef {
	return &gitlet.PackRef{PackID: id, Object: objstore.KeyAt(harnessAt), Hash: objstore.HashOf([]byte(id)),
		Size: 32 + gitlet.IndexBytesFor(1) + gitlet.ReverseBytesFor(1), IndexBytes: gitlet.IndexBytesFor(1),
		ReverseBytes: gitlet.ReverseBytesFor(1), GitChecksum: oidOf("pack" + id), Objects: 1}
}

// training is a train of one candidate on main onto base: entry merged as
// commit, under an operation id of its own.
func training(repo, base, entry, commit string) gitlet.QueueProposal {
	return trainOf(repo, base, gitlet.CandidateEntry{EntryID: entry, Commit: commit})
}

// trainOf is a train on main onto base, a candidate an entry in order, each
// merged as its commit onto the candidate before, under an operation id of its
// own.
func trainOf(repo, base string, entries ...gitlet.CandidateEntry) gitlet.QueueProposal {
	opID := statelog.NewOpID(time.Now(), "code_queue")
	train := gitlet.QueueTrainID(opID)
	c := &gitlet.Candidate{RepoID: repo, TargetRef: mainRef, Train: train, BaseOID: base, Pack: queuedPack(train)}
	for i, e := range entries {
		c.Candidates = append(c.Candidates, gitlet.TrainCandidate{CandidateID: gitlet.QueueCandidateID(train, i+1),
			Ordinal: i + 1, TipOID: e.Commit, Entries: []gitlet.CandidateEntry{e}})
	}
	return gitlet.QueueProposal{OpID: opID, Candidate: c}
}

// landingOf is a landing on main onto base of entries, in the order main moves
// through them, a tested prefix of train, under an operation id of its own.
func landingOf(repo, train, base string, entries ...gitlet.LandEntry) gitlet.QueueProposal {
	opID := statelog.NewOpID(time.Now(), "code_queue")
	return gitlet.QueueProposal{OpID: opID, Land: &gitlet.Land{RepoID: repo, LandID: gitlet.QueueLandID(opID),
		TargetRef: mainRef, BaseOID: base, TipOID: entries[len(entries)-1].Commit, Train: train,
		Entries: entries, Node: "git-a", ReceivedAt: harnessAt}}
}

// queueRecord is a record of the queue's own, under an operation id of its own.
func queueRecord(set func(*gitlet.QueueProposal)) gitlet.QueueProposal {
	p := gitlet.QueueProposal{OpID: statelog.NewOpID(time.Now(), "code_queue")}
	set(&p)
	return p
}

// requireRecorded fails t unless res is a queue record applied at a position.
func requireRecorded(t *testing.T, res gitlet.QueueResult, err error) statelog.Position {
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
func (n *node) checked(repoID, oid, target, name string, attempt int, state gitlet.CheckState) {
	n.t.Helper()
	at := store.EncodeTime(harnessAt)
	n.exec(`INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose, change_id,
		revision, candidate_id, entry_id, base_oid, defs_oid, required, timeout_seconds, reason,
		runner_node, placement, box_handle, reporter, queued_at, started_at, finished_at,
		duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, overdue_at,
		url, summary, def, failures, tail)
		VALUES (?, ?, ?, ?, ?, ?, 'tip', '', 0, '', '', '', '', 1, 600, 'exit',
			'node-a', 'container', '', '', ?, ?, ?, 1000, 0, 0, 1, 2, 4, ?, '', '', '', '', '')`,
		repoID, oid, target, name, attempt, string(state), at, at, at, store.EncodeTime(harnessAt.Add(600*time.Second)))
}

// landingRow is one of target's landing rows as n's rows hold it.
type landingRow struct {
	kind, change, entry, base, commit, by string
	green, broken                         bool
}

// landingsOf is every landing row of target on repo, in the order they landed,
// each green where it lies at or below the target's green row.
func (n *node) landingsOf(repo, target string) []landingRow {
	n.t.Helper()
	var out []landingRow
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(), `SELECT l.kind, l.change_id, l.entry_id, l.base_oid,
			l.commit_oid, l.merged_by, l.position <= COALESCE((SELECT g.position FROM code_green AS g
				WHERE g.repo_id = l.repo_id AND g.target_ref = l.target_ref), -1), l.broken
			FROM code_landings AS l WHERE l.repo_id = ? AND l.target_ref = ?
			ORDER BY l.position, l.ordinal`, repo, target)
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

// greenOf is target's green row on repo, its commit and position; "" for none.
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

// A LANDING REPROVES EVERY ENTRY'S GATE (M5-D8), from its own snapshot with
// each entry's ENQUEUER as merger. dev's change, queued by ada and green in
// its train, loses plat-lead's approval to a changes-requested review after
// the enqueue: its landing is refused gate_unmet naming the entry and the
// approval, appending nothing. Approved again, it lands as one record: merged
// via queue by ada (the queue never acts on its own account), the landing
// green, main and its green row at the train's tip, ONE PUSH ROW via queue
// under ada's credential, the entry landed, its candidate gone, and the stored
// readiness cleared (a merged change is ready for nobody).
//
// Mutations: skip the re-proof — the landing applies over the review, red;
// leave the stored readiness out of the landing's apply — still ready, red.
func TestALandingReprovesEveryEntrysGate(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	if stored := n.storedReadiness(change); !stored.ready {
		t.Fatalf("the home's merge stored %+v, want ready: approved, checked and merging cleanly", stored)
	}
	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	entryID := enqueue.Enqueue.EntryID

	train := training(repo, oidOf("A"), entryID, oidOf("M"))
	res, err = c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	if state, _ := n.entryState(repo, entryID); state != gitlet.QueueTesting {
		t.Fatalf("an entry its train tests is %s, want testing", state)
	}
	n.passed(repo, oidOf("M"), mainRef, "unit")

	converse(t, w, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil))
	land := landingOf(repo, train.Candidate.Train, oidOf("A"),
		gitlet.LandEntry{EntryID: entryID, ChangeID: change, Commit: oidOf("M")})
	before := n.end()
	res, err = c.ProposeQueue(t.Context(), land)
	requireQueueRefused(t, n, before, res, err, gitlet.MergeRefusalGateUnmet, "would not land")
	if len(res.Refused) != 1 || res.Refused[0].EntryID != entryID || res.Refused[0].Refusal != gitlet.MergeRefusalGateUnmet ||
		!slices.ContainsFunc(res.Refused[0].Unmet, func(p gitlet.Precondition) bool {
			return p.Kind == gitlet.PreconditionApproval
		}) {
		t.Fatalf("the landing's refusal names %+v, want entry %s refused gate_unmet for its approval", res.Refused,
			entryID)
	}
	if main := n.target(repo, mainRef); main != oidOf("A") {
		t.Fatalf("main is at %q, want A: a landing whose entry's gate no longer holds moves nothing", main)
	}

	converse(t, w, reviewing(repo, change, platLead, 1, gitlet.VerdictApprove, nil))
	res, err = c.ProposeQueue(t.Context(), land)
	requireRecorded(t, res, err)
	if main := n.target(repo, mainRef); main != oidOf("M") {
		t.Fatalf("main is at %q after its landing, want the train's tip", main)
	}
	n.activityHolds()
	if m := n.mergedChange(change); m.state != string(gitlet.ChangeStateMerged) || m.via != string(gitlet.MergedViaQueue) ||
		m.mergeOID != oidOf("M") || m.by != "founder" || m.byKind != string(gitlet.PartyPerson) ||
		m.operator != "pat:ada" {
		t.Fatalf("the landed change reads %+v, want merged via queue at M by ada, who queued it", m)
	}
	push := gitlet.LandPushID(land.Land.LandID, entryID)
	if pushes := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ? AND via = 'queue'
			AND actor = 'pat:ada' AND actor_seat = 'founder' AND introduced = ?`, push, oidOf("M")); pushes != 1 {
		t.Fatalf("%d push rows record the landing as ada's push via queue introducing M, want one", pushes)
	}
	if got := n.landingsOf(repo, mainRef); len(got) != 1 || got[0].kind != string(gitlet.LandedQueue) ||
		got[0].entry != entryID || got[0].base != oidOf("A") || got[0].commit != oidOf("M") || !got[0].green {
		t.Fatalf("main's landings read %+v, want one green queue landing of the entry from A to M", got)
	}
	if green, _ := n.greenOf(repo, mainRef); green != oidOf("M") {
		t.Fatalf("main's green row is at %q, want the landing's tip", green)
	}
	if state, _ := n.entryState(repo, entryID); state != gitlet.QueueLanded {
		t.Fatalf("the landed entry is %s, want landed", state)
	}
	if left := n.count(`SELECT COUNT(*) FROM code_candidates WHERE repo_id = ?`, repo); left != 0 {
		t.Fatalf("%d candidates outlive the train their landing landed whole", left)
	}
	if stored := n.storedReadiness(change); stored.ready || stored.waitsPerson {
		t.Fatalf("the landed change stores %+v, want ready for nobody and waiting on nobody", stored)
	}
}

// A LAND RECORD FITS ITS CAPS: a train's whole tested prefix, MaxLandEntries
// (as many as a train's candidates take), is ONE land record within what one
// proposal may carry, its rows (less the changes its pushes move, which the
// decide adds and holds whole) within MaxRecordRows with the horizons' room;
// one entry more is refused before it is queued.
//
// Mutation: take one entry past MaxLandEntries in a land's validation — the
// 65th is taken, red.
func TestALandRecordFitsItsCaps(t *testing.T) {
	t.Parallel()
	repo, train := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	entries := make([]gitlet.LandEntry, gitlet.MaxLandEntries+1)
	for i := range entries {
		entries[i] = gitlet.LandEntry{EntryID: uuid.Must(uuid.NewV7()).String(),
			ChangeID: uuid.Must(uuid.NewV7()).String(), Commit: oidOf(fmt.Sprint("commit", i))}
	}
	if gitlet.MaxCandidateEntries > gitlet.MaxLandEntries {
		t.Fatalf("a train takes %d entries and a land record lands %d: its whole prefix would not land as one",
			gitlet.MaxCandidateEntries, gitlet.MaxLandEntries)
	}
	full := landingOf(repo, train, oidOf("A"), entries[:gitlet.MaxLandEntries]...)
	if err := full.Validate(); err != nil {
		t.Fatalf("a landing of %d changes is refused: %v", gitlet.MaxLandEntries, err)
	}
	if got := gitlet.RecordRows(gitlet.LandRecordRows(gitlet.MaxLandEntries)); got > gitlet.MaxRecordRows {
		t.Fatalf("a land of %d entries writes up to %d rows with its horizons' room, past MaxRecordRows (%d)",
			gitlet.MaxLandEntries, got, gitlet.MaxRecordRows)
	}
	over := landingOf(repo, train, oidOf("A"), entries...)
	if err := over.Validate(); err == nil || !strings.Contains(err.Error(), "MaxLandEntries") {
		t.Fatalf("a landing of %d changes validated (%v), want refused past MaxLandEntries", len(entries), err)
	}
}

// GREEN ADVANCES ONLY ON GREEN (M5-D16): an override merge lands dev's change
// ahead of its checks, its landing not green, main's green row untouched. Its
// required check FAILS on the tip: a green is refused not_green, appending
// nothing, and a broken marks the landing broken, the green row unmoved. Run
// again, the check PASSES: the green moves main's green row onto the landing
// at the landing's own position (so a read of landings above the green lists
// what came after it) and marks it green; asked again, it is refused
// no_landing.
//
// Mutations: move the green row on a broken record — it reads M after the
// broken, red; judge a green's checks by their first attempt — the second
// green is refused, red.
func TestGreenAdvancesOnlyOnGreen(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	converse(t, w, n.revising(repo, change, 1, oidOf("B"), mainRef, nil))
	override := merging(repo, change, ada, gitlet.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	override.Merge.Override = "the queue's runner is down"
	merged, err := c.ProposeMerge(t.Context(), override)
	requireMerged(t, merged, err, oidOf("M"))
	landed := merged.Position
	if got := n.landingsOf(repo, mainRef); len(got) != 1 || got[0].kind != string(gitlet.LandedDirect) ||
		got[0].green {
		t.Fatalf("main's landings read %+v, want one direct landing, not green", got)
	}
	if green, _ := n.greenOf(repo, mainRef); green != "" {
		t.Fatalf("main's green row is at %q: an override lands ahead of its checks", green)
	}

	n.checked(repo, oidOf("M"), mainRef, "unit", 1, gitlet.CheckStateFailed)
	green := queueRecord(func(p *gitlet.QueueProposal) {
		p.Green = &gitlet.Green{RepoID: repo, TargetRef: mainRef, OID: oidOf("M"), Through: landed}
	})
	before := n.end()
	res, err := c.ProposeQueue(t.Context(), green)
	requireQueueRefused(t, n, before, res, err, gitlet.MergeRefusalNotGreen, "unit (failed, attempt 1)")
	broken := queueRecord(func(p *gitlet.QueueProposal) {
		p.Broken = &gitlet.Broken{RepoID: repo, TargetRef: mainRef, Position: landed, Ordinal: 1, OID: oidOf("M"),
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

	n.checked(repo, oidOf("M"), mainRef, "unit", 2, gitlet.CheckStatePassed)
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
	res, err = c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
		p.Green = &gitlet.Green{RepoID: repo, TargetRef: mainRef, OID: oidOf("M"), Through: landed}
	}))
	requireQueueRefused(t, n, before, res, err, gitlet.MergeRefusalNoLanding, "at or past")
}

// THE WIDEST LANDING NAMES EACH ENTRY'S CHANGE, NEVER THE DOMAIN (M5-D33): its
// decide proves every entry's gate, reading each change's conversation, so its
// scope names each change's path beside the repository's and its checks' — all
// MaxLandEntries of them, a scope the writer's check passes. Named under the
// domain, it would meet every deferred record of any repository and, retained
// beside one, every later record (MaxScopePaths' doc). Narrower records name what they read: a green and a
// broken the repository and its checks, a train and an ejection the repository
// alone.
//
// Mutations: name the domain past sixteen paths — the landing of sixty-four
// names the domain, red; MaxScopePaths = 16 — its scope is refused, red.
func TestTheWidestLandingNamesEachEntrysChange(t *testing.T) {
	t.Parallel()
	repo := uuid.Must(uuid.NewV7()).String()
	entries := make([]gitlet.LandEntry, gitlet.MaxLandEntries)
	for i := range entries {
		entries[i] = gitlet.LandEntry{EntryID: uuid.Must(uuid.NewV7()).String(),
			ChangeID: uuid.Must(uuid.NewV7()).String(), Commit: oidOf(strings.Repeat("c", i+1))}
	}
	train := uuid.Must(uuid.NewV7()).String()
	widest := landingOf(repo, train, oidOf("A"), entries...)
	scope := gitlet.QueueScope(widest)
	if len(scope) != gitlet.MaxLandEntries+2 || scope[0] != gitlet.RepoPath(repo) || scope[1] != gitlet.ChecksPath(repo) {
		t.Fatalf("a landing of %d entries names %v, want the repository, its checks and each change",
			len(widest.Land.Entries), scope)
	}
	for i, e := range widest.Land.Entries {
		if scope[i+2] != gitlet.ChangePath(e.ChangeID) {
			t.Fatalf("a landing's scope names %q in place of entry %d's change", scope[i+2], i)
		}
	}
	if err := gitlet.ValidScope(scope); err != nil {
		t.Fatalf("the widest landing's scope of %d paths is refused: %v", len(scope), err)
	}
	for name, tc := range map[string]struct {
		p    gitlet.QueueProposal
		want []string
	}{
		"a green": {queueRecord(func(p *gitlet.QueueProposal) {
			p.Green = &gitlet.Green{RepoID: repo}
		}), []string{gitlet.RepoPath(repo), gitlet.ChecksPath(repo)}},
		"a broken": {queueRecord(func(p *gitlet.QueueProposal) {
			p.Broken = &gitlet.Broken{RepoID: repo}
		}), []string{gitlet.RepoPath(repo), gitlet.ChecksPath(repo)}},
		"a train": {queueRecord(func(p *gitlet.QueueProposal) {
			p.Candidate = &gitlet.Candidate{RepoID: repo}
		}), []string{gitlet.RepoPath(repo)}},
		"an ejection": {queueRecord(func(p *gitlet.QueueProposal) {
			p.Eject = &gitlet.Eject{RepoID: repo}
		}), []string{gitlet.RepoPath(repo)}},
	} {
		if got := gitlet.QueueScope(tc.p); !slices.Equal(got, tc.want) {
			t.Errorf("%s names %v, want %v", name, got, tc.want)
		}
	}
}

// AN IDLE TARGET'S LANDINGS ARE SWEPT (M5-D34): a working queue removes its own
// history with each train, ejection and landing; an idle one never would. So
// the retention pass lists repositories holding an ended entry, or a landing
// below its target's green row, older than its clock less QueueHistory less
// QueueSweepGrace, and publishes a queue_sweep on each one's subject removing
// every such row past the history. The landing AT the green row and those above
// it (its tip checks have yet to judge them) stay, as do live entries; a
// repository past the history but inside the grace is not listed.
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
	old := store.EncodeTime(harnessAt.Add(-gitlet.QueueHistory - gitlet.QueueSweepGrace - time.Hour))
	inGrace := store.EncodeTime(harnessAt.Add(-gitlet.QueueHistory - time.Hour))
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
	want := []string{fmt.Sprintf("%s %s", gitlet.OpQueueSweep, gitlet.RepoSubject(idle))}
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
// protected branch leaves its landing, so tip checks and the green row follow
// every way it moved. ada's own push to main under direct_push persons lands a
// push landing naming their human seat: green, main's green row on it, where
// main's gate requires no check; not green, the row unmoved, where it requires
// one. A push to a branch no rule protects leaves none.
//
// Mutation: stamp no landing on a push — main holds no landing row, red.
func TestAPersonsPushLeavesALandingRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		gate  *gitlet.MergeGate
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
			repo, _ := mergeRepo(t, n, c, "web", gitlet.DirectPersons, tc.gate)
			push := proposal(repo, update(mainRef, oidOf("A"), oidOf("Y")), update(releaseRef, gitlet.ZeroOID,
				oidOf("Z")))
			got, err := c.Propose(t.Context(), push)
			if err != nil || len(got.Answer.Commands) != 2 || got.Answer.Commands[0].Result != gitlet.ResultOK {
				t.Fatalf("ada's push answered %+v (%v), want it landed", got.Answer, err)
			}
			landings := n.landingsOf(repo, mainRef)
			if len(landings) != 1 || landings[0].kind != string(gitlet.LandedPush) || landings[0].change != "" ||
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
// REFUSED WHOLE: each landing is two rows (the landing and the branch's green
// row), counted by the push's decide from its snapshot's rules, since the
// committer's batching bound cannot know which branches a rule protects. ada's
// forced push to MaxCommandsPerPush protected branches would take a record of
// its own past MaxRecordRows: it is refused too_many_changes, every command ng
// under one sentence naming its landings, nothing appended. In two pushes the
// same updates land, every landing row written, no apply past MaxRecordRows.
//
// Mutation: count no landing among a decided entry's rows — the push is
// admitted and its apply writes past the bound, red.
func TestAPushLandingOnMoreProtectedBranchesThanARecordHoldsIsRefusedWhole(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	var widest int
	n.applied = func(_ uint64, rows int) { widest = max(widest, rows) }
	s := suiteSettings()
	s.Protections = append(s.Protections, gitlet.Protection{PushRule: gitlet.PushRule{Pattern: "refs/heads/release/*",
		AllowForce: true, DirectPush: gitlet.DirectPersons}})
	repo := n.ensure("web", s, t1).RepoID
	branches := gitlet.MaxCommandsPerPush
	ref := func(i int) string { return fmt.Sprintf("refs/heads/release/%06d", i) }
	seeded := make([]gitlet.Command, branches)
	for i := range seeded {
		seeded[i] = set(ref(i), gitlet.ZeroOID, oidOf(fmt.Sprint("old", i)))
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), seeded...))
	c, _ := committerOn(t, n, n.pub)

	forced := make([]gitlet.Command, branches)
	for i := range forced {
		forced[i] = gitlet.Command{Ref: ref(i), Old: oidOf(fmt.Sprint("old", i)), New: oidOf(fmt.Sprint("new", i)),
			Forced: true}
	}
	before := n.end()
	res, err := c.Propose(t.Context(), proposal(repo, forced...))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if res.Answer.Refused != gitlet.RefusalTooManyChanges || res.Outcome != "" || n.end() != before ||
		!strings.Contains(res.Answer.Detail, fmt.Sprintf("lands on %d protected branches", branches)) {
		t.Fatalf("a push landing on %d protected branches answered %+v (%q) and appended %d records; want "+
			"too_many_changes naming its landings, nothing appended", branches, res, res.Answer.Detail,
			n.end()-before)
	}
	for _, cmd := range res.Answer.Commands {
		if cmd.Result != gitlet.ResultNG || cmd.Reason != gitlet.RefusalTooManyChanges {
			t.Fatalf("%s answered %s %s, want ng under too_many_changes", cmd.Ref, cmd.Result, cmd.Reason)
		}
	}

	half := branches / 2
	for _, part := range [][]gitlet.Command{forced[:half], forced[half:]} {
		res, err := c.Propose(t.Context(), proposal(repo, part...))
		requireOK(t, answer{res, err})
	}
	if landed := n.count(`SELECT COUNT(*) FROM code_landings WHERE repo_id = ? AND kind = 'push'`,
		repo); landed != branches {
		t.Fatalf("%d landing rows after the push in halves, want one for each of the %d branches", landed,
			branches)
	}
	if widest > gitlet.MaxRecordRows {
		t.Fatalf("a record's apply wrote %d rows, past the %d one record may", widest, gitlet.MaxRecordRows)
	}
}

// AN EJECTED ENTRY LEAVES ITS QUEUE AND ITS TRAIN: an eject puts a live entry
// out (why and the failed checks kept on its row, weighed by their bytes for
// the sweep), abandons its train, the candidates gone, and the committer's node
// announces one code_queue_ejected naming the change, the branch and why. An
// eject of an entry no longer live is refused not_queued.
//
// Mutation: keep the train when its entry is ejected — its candidate outlives
// the ejection, red; write ejected_bytes 0 — red.
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
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	entryID := enqueue.Enqueue.EntryID
	res, err = c.ProposeQueue(t.Context(), training(repo, oidOf("A"), entryID, oidOf("M")))
	requireRecorded(t, res, err)

	eject := queueRecord(func(p *gitlet.QueueProposal) {
		p.Eject = &gitlet.Eject{RepoID: repo, Entries: []gitlet.Ejection{{EntryID: entryID,
			Reason: gitlet.EjectCheckFailed, Checks: []string{"unit"}}}}
	})
	res, err = c.ProposeQueue(t.Context(), eject)
	requireRecorded(t, res, err)
	if state, reason := n.entryState(repo, entryID); state != gitlet.QueueEjected ||
		reason != string(gitlet.EjectCheckFailed) {
		t.Fatalf("the ejected entry is %s (%s), want ejected, check_failed", state, reason)
	}
	if left := n.count(`SELECT COUNT(*) FROM code_candidates WHERE repo_id = ?`, repo); left != 0 {
		t.Fatalf("%d candidates outlive the train their entry was ejected from", left)
	}
	if weighed := n.count(`SELECT COUNT(*) FROM code_queue WHERE repo_id = ? AND entry_id = ?
		AND ejected_bytes = length(CAST(ejected AS BLOB)) AND ejected_bytes > 0`, repo, entryID); weighed != 1 {
		t.Fatalf("the ejected entry's row is not weighed by its ejection's bytes")
	}
	waitAnnouncedOf(t, stream, "code_queue_ejected", 1)
	ev := stream.of("code_queue_ejected")[0]
	payload, ok := ev.Data.(*types.CodeQueueEjected)
	if !ok || ev.Type != "code_queue_ejected" || payload.ChangeID != change || payload.Number != 1 ||
		payload.EntryID != entryID || payload.Reason != string(gitlet.EjectCheckFailed) ||
		!slices.Equal(payload.Checks, []string{"unit"}) || payload.EnqueuedBy != "founder" ||
		payload.RepoName != "web" || ev.Source != n.id || payload.Owner != "dev" || payload.OwnerKind != "agent" ||
		payload.OwnerAgent != "agent-dev" {
		t.Fatalf("the ejection was announced as %s from %q %+v, want web!1 ejected for unit, owned by dev — "+
			"its day counting it — from %s", ev.Type, ev.Source, ev.Data, n.id)
	}
	before := n.end()
	res, err = c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
		p.Eject = &gitlet.Eject{RepoID: repo, Entries: []gitlet.Ejection{{EntryID: entryID, Reason: gitlet.EjectConflict}}}
	}))
	requireQueueRefused(t, n, before, res, err, gitlet.MergeRefusalNotQueued, "is ejected")
}

// A CHANGE'S QUEUE ENTRIES ARE ITS PART, in queue order: each enqueue names who made it, the head,
// the target and, once ended, how and the record's position; an ejection names its failed checks,
// a dequeue its reason; a page resumes after its cursor's entry.
func TestAChangesQueueEntriesAreItsPart(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	first := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), first)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	ejected, err := c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
		p.Eject = &gitlet.Eject{RepoID: repo, Entries: []gitlet.Ejection{{EntryID: first.Enqueue.EntryID,
			Reason: gitlet.EjectCheckFailed, Checks: []string{"unit"}}}}
	}))
	ejectedAt := requireRecorded(t, ejected, err)
	second := enqueueing(repo, change, ada, oidOf("B"))
	res, err = c.ProposeQueue(t.Context(), second)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	dequeued, err := c.ProposeQueue(t.Context(), dequeueing(repo, second.Enqueue.EntryID, ada))
	if err != nil || dequeued.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the dequeue answered %+v, %v", dequeued, err)
	}

	read := func(limit int, cursor string) gitlet.ReadAnswer {
		t.Helper()
		answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: ada, Kind: gitlet.ReadChange, Repo: "web",
			Change: 1, Part: gitlet.PartQueue, Limit: limit, Cursor: cursor, Fresh: stale})
		if err != nil {
			t.Fatalf("read the change's queue entries after %q: %v", cursor, err)
		}
		return answer
	}
	var got []gitlet.ChangeQueueEntry
	cursor, pages := "", 0
	for {
		answer := read(1, cursor)
		pages++
		got = append(got, answer.QueueEntries...)
		if cursor = answer.Next; cursor == "" || pages > 3 {
			break
		}
	}
	founder := gitlet.Party{Handle: "founder", Kind: gitlet.PartyPerson}
	// ONE A PAGE: a full page carries a cursor, so the walk ends on the page past the second.
	if len(got) != 2 || pages < 2 {
		t.Fatalf("the change's queue entries, one a page, read in %d pages as %+v; want its two entries, a page "+
			"each", pages, got)
	}
	one, two := got[0], got[1]
	if one.EntryID != first.Enqueue.EntryID || one.State != gitlet.QueueEjected ||
		one.Reason != gitlet.EjectCheckFailed || !slices.Equal(one.Checks, []string{"unit"}) ||
		one.EnqueuedBy != founder || one.HeadOID != oidOf("B") || one.TargetRef != mainRef ||
		one.EnqueuedPosition.IsZero() || one.EndedPosition != ejectedAt || one.EndedAt.IsZero() {
		t.Errorf("the first entry is %+v, want founder's enqueue of B into main, ejected for unit at %v", one,
			ejectedAt)
	}
	if two.EntryID != second.Enqueue.EntryID || two.State != gitlet.QueueDequeued || two.Reason != gitlet.EndAsked ||
		two.EndedPosition != dequeued.Position || one.EndedPosition.Packed() >= two.EnqueuedPosition.Packed() {
		t.Errorf("the second entry is %+v, want founder's later enqueue, dequeued as asked at %v", two,
			dequeued.Position)
	}
}

// A PUSH THAT EJECTS A QUEUED HEAD ANNOUNCES IT: a push moving a queued
// change's source branch ejects its entry head_moved in the push's own record,
// which no queue record names, so the committer that landed the push announces
// one code_queue_ejected naming the change, why and its owner dev, whose day
// counts it (`ejections`).
//
// Mutation: drop commit's call of announcePushEjections — no
// code_queue_ejected follows the push, red.
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
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)

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
		payload.Reason != string(gitlet.EjectHeadMoved) || payload.RepoName != "web" || ev.Source != n.id ||
		payload.Owner != "dev" || payload.OwnerKind != "agent" || payload.OwnerAgent != "agent-dev" {
		t.Fatalf("the push's ejection was announced from %q as %+v, want web!1's entry %s ejected head_moved, "+
			"owned by dev — its day counting it — from %s", ev.Source, ev.Data, first.Enqueue.EntryID, n.id)
	}
}

// purgeTips is every object an `expunge_tips` read of repo names from since,
// over every page, n's reader paging every pageRows rows.
func (n *node) purgeTips(repo string, since time.Time, pageRows int) []string {
	n.t.Helper()
	gitlet.SetPageLimits(n.reader, pageRows, 1<<20)
	var tips []string
	q := gitlet.ReadQuery{Kind: gitlet.ReadExpungeTips, ExpungeTips: &gitlet.ExpungeTipsQuery{RepoID: repo, Since: since},
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
// never a ref, so no ref reaches its tip (what a landing moves the target onto);
// a purge, which keeps no drop, keeps it because the read its scan walks from
// names what rows hold in flight: the candidate's tip, the live entry's head,
// the open change's revision head and, once a landing moves main, main's green
// row. Read a row a page, every phase resumes after its cursor, skipping
// nothing; once the train lands, its candidate and entry name nothing.
//
// Mutations: drop the candidates' phase — a purge drops the tip a pending
// landing moves main onto; drop the green phase — the commit mirrors follow
// goes; drop the queue's — the live entry's row is missing from the pages.
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
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	train := training(repo, oidOf("A"), enqueue.Enqueue.EntryID, oidOf("M"))
	res, err = c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	asked := purgeOf(repo)
	purged, err := c.ProposeExpunge(t.Context(), gitlet.ExpungeProposal{Request: &asked})
	requireExpunged(t, purged, err)

	// ONE PAGE answers each object once; A ROW A PAGE answers each phase's row on
	// its own page, in phase order (the revision at B, the live entry at B, the
	// candidate at M), so a phase left out is a missing row even where another
	// phase names the same object.
	now := time.Now()
	if got := n.purgeTips(repo, now, gitlet.MaxSnapshotItems); !slices.Equal(got, []string{oidOf("B"), oidOf("M")}) {
		t.Fatalf("a purge's tips in one page are %v; want the head B the change's revision and the entry "+
			"hold, then the candidate's tip M", got)
	}
	if got := n.purgeTips(repo, now, 1); !slices.Equal(got, []string{oidOf("B"), oidOf("B"), oidOf("M")}) {
		t.Fatalf("a purge's tips a row a page are %v; want the revision's head B, the live entry's B and the "+
			"candidate's tip M, in that order", got)
	}

	n.passed(repo, oidOf("M"), mainRef, "unit")
	land := landingOf(repo, train.Candidate.Train, oidOf("A"),
		gitlet.LandEntry{EntryID: enqueue.Enqueue.EntryID, ChangeID: change, Commit: oidOf("M")})
	res, err = c.ProposeQueue(t.Context(), land)
	requireRecorded(t, res, err)
	if got := n.purgeTips(repo, now, gitlet.MaxSnapshotItems); !slices.Equal(got, []string{oidOf("M")}) {
		t.Fatalf("after the landing a purge's tips are %v; want main's green row at M alone: the landed "+
			"candidate, its entry and the merged change's revisions are history", got)
	}
}

// A PURGE'S TIPS READ EVERY REVISION OF A CHANGE OPENED BETWEEN ITS PAGES
// (M6-D2: the tip set is exact): the change a revisions page stopped in can
// close, and another open on its branch, before the next page. The cursor
// names the change it stopped in, so the next page reads a new change from its
// first revision, never from the number its predecessor reached.
//
// Mutation: resume a branch's change at the cursor's revision whichever change
// it is — the new change's first revision, head C, is never named.
func TestAPurgesTipsReadEveryRevisionOfAChangeOpenedBetweenItsPages(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	repo, first := queueRepo(t, n, c, "web")

	gitlet.SetPageLimits(n.reader, 1, 1<<20)
	q := gitlet.ReadQuery{Kind: gitlet.ReadExpungeTips, ExpungeTips: &gitlet.ExpungeTipsQuery{RepoID: repo,
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
	// opens there (revision 1 at C), and feature moves on to D (revision 2).
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

// A PURGE'S TIPS RESUME INSIDE EVERY PHASE (M6-D2: the tip set is exact): read
// a row a page, a page ends INSIDE each two-row phase (two live changes on two
// branches, two live entries on one target, two candidates of one train) and
// the next resumes after the row its cursor names, so every object is named in
// phase order and none skipped: heads B and R of each revision and each entry,
// then the candidates' tips M1 and M2, the one a whole-train landing moves
// main onto.
//
// Mutations: resume the live changes after a batch's last with none —
// release's R is skipped; resume a queue after its target alone, never an
// entry's seq — the second entry's R is skipped; resume a train after it
// alone, never a candidate's ordinal — M2 is skipped.
func TestAPurgesTipsResumeInsideEveryPhase(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, first := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, first)
	opened, err := c.ProposeChange(t.Context(), opening(repo, releaseRef, mainRef, dev))
	requireChanged(t, opened, err)
	second := opened.ChangeID
	converse(t, w, n.revising(repo, second, 1, oidOf("R"), mainRef, nil))
	converse(t, w, reviewing(repo, second, platLead, 1, gitlet.VerdictApprove, nil))
	n.passed(repo, oidOf("R"), mainRef, "unit")
	entries := make([]string, 0, 2)
	for i, q := range []gitlet.QueueProposal{enqueueing(repo, first, ada, oidOf("B")),
		enqueueing(repo, second, ada, oidOf("R"))} {
		res, err := c.ProposeQueue(t.Context(), q)
		requireQueued(t, res, err, gitlet.QueueWaiting, i+1)
		entries = append(entries, q.Enqueue.EntryID)
	}
	res, err := c.ProposeQueue(t.Context(), trainOf(repo, oidOf("A"),
		gitlet.CandidateEntry{EntryID: entries[0], Commit: oidOf("M1")},
		gitlet.CandidateEntry{EntryID: entries[1], Commit: oidOf("M2")}))
	requireRecorded(t, res, err)

	now := time.Now()
	if got, want := n.purgeTips(repo, now, gitlet.MaxSnapshotItems),
		[]string{oidOf("B"), oidOf("R"), oidOf("M1"), oidOf("M2")}; !slices.Equal(got, want) {
		t.Fatalf("a purge's tips in one page are %v; want %v — each object once, in its first phase's order",
			got, want)
	}
	if got, want := n.purgeTips(repo, now, 1),
		[]string{oidOf("B"), oidOf("R"), oidOf("B"), oidOf("R"), oidOf("M1"), oidOf("M2")}; !slices.Equal(got, want) {
		t.Fatalf("a purge's tips a row a page are %v; want %v — feature's and release's revisions, the "+
			"two entries, then the train's two candidates", got, want)
	}
}

// A TRAIN OR A LANDING BUILT BEFORE A BEGIN IS REFUSED EXPUNGING (M6-D4): the
// queue records that move a ref onto what a build made carry their home copy's
// drop epoch; once an expunge's begin moves the repository's epoch, one built
// at the epoch before is refused expunging, nothing appended, naming its home.
// Built again at the begin's epoch, it lands. No other queue record carries an
// epoch.
//
// Mutation: drop the queue decide's comparison of the epochs — the train built
// before the begin is recorded; compare them for a candidate alone — the
// landing built before the begin moves main.
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
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	b := beginFor(n, repo, 1, n.livePacks(repo), 1, 0)
	began, err := c.ProposeExpunge(t.Context(), gitlet.ExpungeProposal{Begin: &b})
	requireExpunged(t, began, err)

	train := training(repo, oidOf("A"), enqueue.Enqueue.EntryID, oidOf("M"))
	before := n.end()
	res, err = c.ProposeQueue(t.Context(), train)
	requireQueueRefused(t, n, before, res, err, gitlet.MergeRefusalExpunging,
		gitlet.ExpungingBuiltDetail("web", "train", "its home", 0, 1))
	train.DropEpoch = 1
	res, err = c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)

	// THE LANDING built at the epoch before the begin is refused too, main
	// unmoved; built at the begin's epoch, it moves main onto the train's tip.
	n.passed(repo, oidOf("M"), mainRef, "unit")
	land := landingOf(repo, train.Candidate.Train, oidOf("A"),
		gitlet.LandEntry{EntryID: enqueue.Enqueue.EntryID, ChangeID: change, Commit: oidOf("M")})
	before = n.end()
	res, err = c.ProposeQueue(t.Context(), land)
	requireQueueRefused(t, n, before, res, err, gitlet.MergeRefusalExpunging,
		gitlet.ExpungingBuiltDetail("web", "landing", "its home", 0, 1))
	if main := n.target(repo, mainRef); main != oidOf("A") {
		t.Fatalf("main is at %q after a refused landing, want A", main)
	}
	land.DropEpoch = 1
	res, err = c.ProposeQueue(t.Context(), land)
	requireRecorded(t, res, err)
	if main := n.target(repo, mainRef); main != oidOf("M") {
		t.Fatalf("main is at %q after the landing at the begin's epoch, want the train's tip M", main)
	}

	dequeue := queueRecord(func(p *gitlet.QueueProposal) {
		p.Asker, p.Dequeue = ada, &gitlet.Dequeue{RepoID: repo, EntryID: enqueue.Enqueue.EntryID}
	})
	dequeue.DropEpoch = 1
	if err := dequeue.Validate(); err == nil || !strings.Contains(err.Error(), "drop_epoch") {
		t.Fatalf("a dequeue carrying a drop epoch validates: %v, want drop_epoch refused", err)
	}
}

// A QUEUE READ SAYS WHAT ITS QUEUES PUT OUT LATELY: a read of a repository, or
// of the entry's target, answers beside its live entries the ejected entry —
// its change's number, enqueuer, why and the paths its merge conflicted on; a
// read of another target, of every repository, or a day later answers none.
//
// Mutations: drop the ejections from the listing — the first read answers
// none, red; drop the target filter — another target's read answers it, red;
// drop the horizon — the read a day later answers it, red.
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
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	entryID := enqueue.Enqueue.EntryID
	res, err = c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
		p.Eject = &gitlet.Eject{RepoID: repo, Entries: []gitlet.Ejection{{EntryID: entryID,
			Reason: gitlet.EjectConflict, Paths: []string{"go.mod", "main.go"}}}}
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

	read := func(q gitlet.ReadQuery) []gitlet.EjectedRow {
		t.Helper()
		q.Kind, q.Asker = gitlet.ReadQueue, ada
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("read the queue %+v: %v", q, err)
		}
		return answer.Ejected
	}
	for _, q := range []gitlet.ReadQuery{{Repo: "web"}, {Repo: "web", Target: "refs/heads/main"}} {
		got := read(q)
		if len(got) != 1 || got[0].EntryID != entryID || got[0].Number != 1 || got[0].Repo != "web" ||
			got[0].Reason != gitlet.EjectConflict || !slices.Equal(got[0].Paths, []string{"go.mod", "main.go"}) ||
			got[0].EnqueuedBy.Handle != "founder" || !got[0].EjectedAt.Equal(ejectedAt) {
			t.Fatalf("a read of %+v answers the ejections %+v, want web!1's, conflicted on go.mod and main.go",
				q, got)
		}
	}
	if got := read(gitlet.ReadQuery{Repo: "web", Target: releaseRef}); len(got) != 0 {
		t.Errorf("a read of %s answers main's ejection: %+v", releaseRef, got)
	}
	if got := read(gitlet.ReadQuery{}); len(got) != 0 {
		t.Errorf("a read of every repository answers ejections %+v, want none: a named repository's alone", got)
	}
	later := ejectedAt.Add(gitlet.QueueEjectionsShown + time.Minute)
	n.readAt.Store(&later)
	if got := read(gitlet.ReadQuery{Repo: "web"}); len(got) != 0 {
		t.Errorf("a read a day after the ejection still answers it: %+v", got)
	}
}

// queuedFor is a repository named name whose one change (dev's, feature into
// main, a queue target ([queueGate]), carrying item) by queued asking its work
// item closed, the enqueue made for f, and a train of it onto main's tip A,
// its tip M passed: the repository, the change, the entry and the train.
func queuedFor(t *testing.T, n *node, c *gitlet.Committer, w *gitlet.ChangeWriter, name string,
	item *types.WorkItem, by gitlet.Principal, f types.WrittenFor) (repo, change, entryID string,
	train gitlet.QueueProposal) {

	t.Helper()
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = queueGate()
	repo = n.ensure(name, s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(featureRef, gitlet.ZeroOID, oidOf("B"))))
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.WorkItem = item
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	change = opened.ChangeID
	ready(t, n, w, repo, change)
	enqueue := enqueueing(repo, change, by, oidOf("B"))
	enqueue.WrittenFor, enqueue.Enqueue.CloseTask = f, true
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	entryID = enqueue.Enqueue.EntryID
	train = training(repo, oidOf("A"), entryID, oidOf("M"))
	res, err = c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	n.passed(repo, oidOf("M"), mainRef, "unit")
	return repo, change, entryID, train
}

// landedOwingClose is a repository named name whose one change, carrying
// item, ada queued into main asking its work item closed, and its landing
// applied: the repository, the change, the entry and the land record.
func landedOwingClose(t *testing.T, n *node, c *gitlet.Committer, w *gitlet.ChangeWriter, name string,
	item *types.WorkItem) (repo, change, entryID string, land gitlet.QueueProposal) {

	t.Helper()
	repo, change, entryID, train := queuedFor(t, n, c, w, name, item, ada, types.ForNobody())
	land = landingOf(repo, train.Candidate.Train, oidOf("A"),
		gitlet.LandEntry{EntryID: entryID, ChangeID: change, Commit: oidOf("M")})
	res, err := c.ProposeQueue(t.Context(), land)
	requireRecorded(t, res, err)
	return repo, change, entryID, land
}

// A LANDED ENTRY OWES ITS WORK ITEM'S CLOSE UNTIL IT IS SETTLED (M5-D27): a
// queued merge asking its work item closed lands, and the apply leaves the
// close OWED on the entry's row under gitlet.CloseWorkItemOpID — what every
// home's queue work answers (the repository listed, its first page naming the
// entry, enqueuer and work item), whichever home asked the landing. A closed
// settle clears it; a second settle is refused stale, appending nothing. A
// not_closed settle tells the change's authors AND its enqueuer, under
// `merged`, the reason beside.
//
// Mutations: leave close_op out of the land's write (entryLanded) — nothing is
// owed, red; drop the settle's UPDATE — the close stays owed, red; route a
// not_closed settle to the authors alone — the enqueuer hears nothing, red.
func TestALandedEntryOwesItsWorkItemsCloseUntilItIsSettled(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	// WITHIN EVERY HORIZON: the broker stamps a record at the wall clock and the
	// harness applies it at its own instant, years on, past which the applier
	// writes no notice.
	storedBefore(n, time.Hour)
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	stale := statelog.Freshness{Level: statelog.ReadStale}
	land := func(name string, item *types.WorkItem) (repo, change, entryID string, land gitlet.QueueProposal) {
		return landedOwingClose(t, n, c, w, name, item)
	}
	owed := func(repo string) []gitlet.QueueWorkEntry {
		t.Helper()
		a, err := n.reader.QueueDue(t.Context(), gitlet.QueueDueQuery{RepoID: repo, Fresh: stale})
		if err != nil {
			t.Fatalf("read %s's queue work: %v", repo, err)
		}
		return a.QueueOwedCloses
	}
	listed := func(repo string) bool {
		t.Helper()
		a, err := n.reader.QueueDue(t.Context(), gitlet.QueueDueQuery{Fresh: stale})
		if err != nil {
			t.Fatalf("list the repositories owing queue work: %v", err)
		}
		return slices.Contains(a.DueRepos, repo)
	}
	settling := func(repo, entryID string, outcome gitlet.CloseOutcome, detail string) gitlet.QueueProposal {
		return queueRecord(func(p *gitlet.QueueProposal) {
			p.CloseSettled = &gitlet.CloseSettled{RepoID: repo,
				Settled: []gitlet.SettledClose{{EntryID: entryID, Outcome: outcome, Detail: detail}}}
		})
	}

	item := &types.WorkItem{Backend: types.WorkWorklet, ID: "task-7", Key: "ENG-7", Project: "ENG"}
	repo, _, entryID, landed := land("web", item)
	want := gitlet.CloseWorkItemOpID(landed.OpID, entryID)
	if got := owed(repo); len(got) != 1 || got[0].EntryID != entryID || got[0].CloseOp != want ||
		got[0].WorkItem == nil || got[0].WorkItem.ID != item.ID || got[0].EnqueuedBy.Seat != ada.Seat ||
		!got[0].CloseTask {
		t.Fatalf("the landed entry's owed closes read %+v, want entry %s owing ENG-7's close as %s under %s",
			got, entryID, ada.Seat, want)
	}
	if !listed(repo) {
		t.Fatalf("a repository owing a landed entry's close is not listed as owing queue work")
	}

	res, err := c.ProposeQueue(t.Context(), settling(repo, entryID, gitlet.CloseClosed, ""))
	requireRecorded(t, res, err)
	if got := owed(repo); len(got) != 0 {
		t.Fatalf("a settled close is still owed: %+v", got)
	}
	if listed(repo) {
		t.Fatalf("a repository whose every close is settled is still listed as owing queue work")
	}
	before := n.end()
	res, err = c.ProposeQueue(t.Context(), settling(repo, entryID, gitlet.CloseClosed, ""))
	requireQueueRefused(t, n, before, res, err, gitlet.MergeRefusalStale, "owes its work item a close")

	// NOT CLOSED: the authors and the enqueuer told, under merged.
	repo, change, entryID, _ := land("api", item)
	why := "ENG-7 is a jira work item, which is closed where it lives"
	res, err = c.ProposeQueue(t.Context(), settling(repo, entryID, gitlet.CloseNotClosed, why))
	requireRecorded(t, res, err)
	for _, who := range []string{"dev", ada.Seat} {
		if told := n.count(`SELECT COUNT(*) FROM code_notifications WHERE change_id = ? AND recipient = ?
				AND reason = ? AND excerpt = ?`, change, who, string(gitlet.ReasonMerged), why); told != 1 {
			t.Errorf("%s was told %d times that the merged change's work item was not closed, want once", who,
				told)
		}
	}
	if got := owed(repo); len(got) != 0 {
		t.Fatalf("a close settled not_closed is still owed: %+v", got)
	}
}

// landedTwoOwingCloses is a repository named name whose two changes, dev's from
// feature (head B, ENG-7) and from release (head R, ENG-8), ada queued into
// main asking their work items closed, one landing of both applied: the
// repository, and each change and its entry, in the landing's order.
func landedTwoOwingCloses(t *testing.T, n *node, c *gitlet.Committer, w *gitlet.ChangeWriter,
	name string) (repo string, changes, entries [2]string) {

	t.Helper()
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = queueGate()
	repo = n.ensure(name, s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(featureRef, gitlet.ZeroOID, oidOf("B")), set(releaseRef, gitlet.ZeroOID, oidOf("R"))))
	for i, source := range []struct{ ref, head, key string }{
		{featureRef, oidOf("B"), "ENG-7"}, {releaseRef, oidOf("R"), "ENG-8"},
	} {
		open := opening(repo, source.ref, mainRef, dev)
		open.Open.WorkItem = &types.WorkItem{Backend: types.WorkWorklet, ID: "task-" + source.key,
			Key: source.key, Project: "ENG"}
		opened, err := c.ProposeChange(t.Context(), open)
		requireChanged(t, opened, err)
		changes[i] = opened.ChangeID
		converse(t, w, n.revising(repo, changes[i], 1, source.head, mainRef, nil))
		converse(t, w, reviewing(repo, changes[i], platLead, 1, gitlet.VerdictApprove, nil))
		n.passed(repo, source.head, mainRef, "unit")
		enqueue := enqueueing(repo, changes[i], ada, source.head)
		enqueue.Enqueue.CloseTask = true
		res, err := c.ProposeQueue(t.Context(), enqueue)
		requireQueued(t, res, err, gitlet.QueueWaiting, i+1)
		entries[i] = enqueue.Enqueue.EntryID
	}
	train := trainOf(repo, oidOf("A"), gitlet.CandidateEntry{EntryID: entries[0], Commit: oidOf("M1")},
		gitlet.CandidateEntry{EntryID: entries[1], Commit: oidOf("M2")})
	res, err := c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	n.passed(repo, oidOf("M1"), mainRef, "unit")
	n.passed(repo, oidOf("M2"), mainRef, "unit")
	res, err = c.ProposeQueue(t.Context(), landingOf(repo, train.Candidate.Train, oidOf("A"),
		gitlet.LandEntry{EntryID: entries[0], ChangeID: changes[0], Commit: oidOf("M1")},
		gitlet.LandEntry{EntryID: entries[1], ChangeID: changes[1], Commit: oidOf("M2")}))
	requireRecorded(t, res, err)
	return repo, changes, entries
}

// ONE SETTLE RECORD SETTLES A LANDING'S CLOSES (M5-D27): one close_settled,
// never a record a change, and each not_closed close tells ITS change's
// authors and enqueuer ITS OWN sentence, under `merged`. A close already
// cleared is SKIPPED, the rest applied; a record whose every close is cleared
// is refused stale and appends nothing.
//
// Mutations: one excerpt for every notice (the first routing's) — the second
// change's authors read the first's sentence, red; refuse the record where one
// close owes nothing — the second stays owed, red; clear every named entry,
// owed or not, on the stale record — applied, not refused, red; leave a code
// wake's TriggerOrigin unset in inboundOf — dev's wakes name nothing, red.
func TestOneSettleRecordSettlesALandingsClosesEachToldItsOwn(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, time.Hour)
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	stale := statelog.Freshness{Level: statelog.ReadStale}
	owed := func(repo string) []string {
		t.Helper()
		a, err := n.reader.QueueDue(t.Context(), gitlet.QueueDueQuery{RepoID: repo, Fresh: stale})
		if err != nil {
			t.Fatalf("read %s's queue work: %v", repo, err)
		}
		var ids []string
		for _, e := range a.QueueOwedCloses {
			ids = append(ids, e.EntryID)
		}
		return ids
	}
	settle := func(repo string, closes ...gitlet.SettledClose) gitlet.QueueProposal {
		return queueRecord(func(p *gitlet.QueueProposal) {
			p.CloseSettled = &gitlet.CloseSettled{RepoID: repo, Settled: closes}
		})
	}
	told := func(change, who, why string) int {
		return n.count(`SELECT COUNT(*) FROM code_notifications WHERE change_id = ? AND recipient = ?
			AND reason = ? AND excerpt = ?`, change, who, string(gitlet.ReasonMerged), why)
	}

	// EACH ITS OWN SENTENCE, in one record.
	repo, changes, entries := landedTwoOwingCloses(t, n, c, w, "web")
	if got := owed(repo); !slices.Equal(got, entries[:]) {
		t.Fatalf("the landing of two left %v owing their closes, want both of %v", got, entries)
	}
	why := [2]string{"ENG-7 is a jira work item, which is closed where it lives",
		"ada may no longer update ENG-8"}
	res, err := c.ProposeQueue(t.Context(), settle(repo,
		gitlet.SettledClose{EntryID: entries[0], Outcome: gitlet.CloseNotClosed, Detail: why[0]},
		gitlet.SettledClose{EntryID: entries[1], Outcome: gitlet.CloseNotClosed, Detail: why[1]}))
	requireRecorded(t, res, err)
	for i, change := range changes {
		for _, who := range []string{"dev", ada.Seat} {
			if got := told(change, who, why[i]); got != 1 {
				t.Errorf("%s was told %d times why change %d's work item was not closed, want once", who, got,
					i+1)
			}
		}
	}
	// EACH WAKE NAMES WHAT STARTED ITS TURN: dev is woken about each change under
	// merged, its own sentence beside, each wake naming the change by its ref so
	// the executors board and the turn's history say which merge's close it
	// answers (ada, a person, hears by notice).
	_, _, routed := wakesOf(t, feedRecord(t, n, res.Position))
	if got := woken(routed); !slices.Equal(got, []string{"dev:merged", "dev:merged"}) {
		t.Fatalf("the settle of two not_closed closes wakes %v, want dev about each change", got)
	}
	refs := map[string]bool{}
	for _, r := range routed {
		i := slices.Index(changes[:], r.Metadata[gitlet.MetaChangeID])
		ref := r.Metadata[gitlet.MetaChangeRef]
		switch {
		case i < 0:
			t.Errorf("dev is woken about %q, which is neither of the landing's changes %v",
				r.Metadata[gitlet.MetaChangeID], changes)
		case r.Metadata[gitlet.MetaNotClosed] != why[i]:
			t.Errorf("dev's wake about change %d says %q, want its own sentence %q", i+1,
				r.Metadata[gitlet.MetaNotClosed], why[i])
		case !strings.HasPrefix(ref, "web!") || refs[ref]:
			t.Errorf("dev's wake about change %d names it %q, want its own ref in web", i+1, ref)
		case r.Origin != (types.TriggerOrigin{Kind: types.OriginCodeChange, ID: ref,
			Reason: string(gitlet.ReasonMerged)}):
			t.Errorf("dev's wake about change %d names %+v as what starts its turn, want the change %s, "+
				"merged", i+1, r.Origin, ref)
		}
		refs[ref] = true
	}
	if got := owed(repo); len(got) != 0 {
		t.Fatalf("both closes settled in one record, %v still owe one", got)
	}
	before := n.end()
	res, err = c.ProposeQueue(t.Context(), settle(repo,
		gitlet.SettledClose{EntryID: entries[0], Outcome: gitlet.CloseClosed},
		gitlet.SettledClose{EntryID: entries[1], Outcome: gitlet.CloseClosed}))
	requireQueueRefused(t, n, before, res, err, gitlet.MergeRefusalStale, "owes its work item a close")

	// ONE SETTLED ALREADY, the other settled beside it.
	repo, _, entries = landedTwoOwingCloses(t, n, c, w, "api")
	res, err = c.ProposeQueue(t.Context(), settle(repo,
		gitlet.SettledClose{EntryID: entries[0], Outcome: gitlet.CloseClosed}))
	requireRecorded(t, res, err)
	if got := owed(repo); !slices.Equal(got, entries[1:]) {
		t.Fatalf("one close settled, %v still owe one, want %v", got, entries[1:])
	}
	res, err = c.ProposeQueue(t.Context(), settle(repo,
		gitlet.SettledClose{EntryID: entries[0], Outcome: gitlet.CloseClosed},
		gitlet.SettledClose{EntryID: entries[1], Outcome: gitlet.CloseClosed}))
	requireRecorded(t, res, err)
	if got := owed(repo); len(got) != 0 {
		t.Fatalf("a settle naming a close settled already beside one owed left %v owing, want none", got)
	}
}

// enqueuedForCases are the enqueues the queue's records about an entry are
// read back for: cto's in a turn founder woke, cto's for nobody (the seat's own
// work), and ada's, a person's, who writes for nobody but herself.
func enqueuedForCases(founder *types.WorkingFor) []struct {
	name string
	by   gitlet.Principal
	f    types.WrittenFor
	want *types.WorkingFor
} {
	return []struct {
		name string
		by   gitlet.Principal
		f    types.WrittenFor
		want *types.WorkingFor
	}{
		{"cto's enqueue made for founder", cto, types.ForTurn(founder), founder},
		{"cto's enqueue made for nobody", cto, types.ForTurn(nil), nil},
		{"ada's enqueue", ada, types.ForNobody(), nil},
	}
}

// versionOf is the code record version a record carrying want is stamped at:
// the carrier's where it carries one, the base otherwise.
func versionOf(want *types.WorkingFor) int {
	if want != nil {
		return gitlet.WorkingForVersion
	}
	return 1
}

// A QUEUE'S LANDING WAKES FOR WHOM ITS ENTRY WAS ENQUEUED (ADR-0109): the
// landing names the enqueuer as merger, and the enqueue was that seat's write
// in a turn somebody woke, so the change's other authors are told it merged
// for that somebody, off the entry's row, as the seat's own merge would. cto
// queues dev's change in a turn founder woke: the landing carries founder
// beside cto at version 2, and dev is woken by cto for founder. cto's enqueue
// for nobody, and ada's, carry nobody at version 1, dev woken by the enqueuer
// alone.
//
// Mutation: hand the landing's routing no carrier — cto's entry made for
// founder carries nobody and dev is woken for nobody, red.
func TestAQueuesLandingWakesForWhomItsEntryWasEnqueued(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	founder := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"}
	item := &types.WorkItem{Backend: types.WorkWorklet, ID: "task-7", Key: "ENG-7", Project: "ENG"}
	for i, tc := range enqueuedForCases(founder) {
		t.Run(tc.name, func(t *testing.T) {
			repo, change, entryID, train := queuedFor(t, n, c, w, fmt.Sprintf("landed-%d", i), item, tc.by, tc.f)
			res, err := c.ProposeQueue(t.Context(), landingOf(repo, train.Candidate.Train, oidOf("A"),
				gitlet.LandEntry{EntryID: entryID, ChangeID: change, Commit: oidOf("M")}))
			at := requireRecorded(t, res, err)
			routings := routingAt(t, n, at)
			if len(routings) != 1 || !routings[0].Merged || routings[0].Actor != tc.by.Seat {
				t.Fatalf("the landing carries %+v, want the change merged by %s, who queued it", routings,
					tc.by.Seat)
			}
			if f, v := routings[0].WorkingFor, n.recordAt(at).V; !reflect.DeepEqual(f, tc.want) ||
				v != versionOf(tc.want) {
				t.Errorf("the landing's routing carries %+v at version %d, want %+v at %d", f, v, tc.want,
					versionOf(tc.want))
			}
			writer := types.NotificationWriter{Kind: iam.ActorAgent, Seat: "cto", WorkingFor: tc.want}
			if tc.by.Kind != gitlet.PrincipalAgent {
				writer = types.NotificationWriter{Kind: iam.ActorHuman, Seat: tc.by.Seat}
			}
			_, relayed, routed := wakesOf(t, feedRecord(t, n, at))
			switch {
			case !relayed || !slices.Equal(woken(routed), []string{"dev:merged"}):
				t.Fatalf("the landing wakes %v (relayed %v), want dev, its owner, told it merged", woken(routed),
					relayed)
			case routed[0].Writer == nil || !reflect.DeepEqual(*routed[0].Writer, writer):
				t.Errorf("dev's wake is written by %s, want %s", writtenBy(routed[0].Writer), writtenBy(&writer))
			}
		})
	}
}

// AN EJECTION AND AN UNMADE CLOSE WAKE FOR WHOM THEIR ENQUEUE WAS MADE
// (ADR-0109's amendment): the queue's own records about an entry name no actor
// (no principal's act made them) and tell its enqueuer and its change's
// authors about that enqueue, so each carries whom it was made for, off the
// entry's row: a turn worked for the told seat's own would requeue the change,
// or close the item its requester could not, with every app the seat holds.
// cto's entry queued for founder: its ejection and its not_closed settle wake
// dev and cto written by the engine for founder, at version 2; enqueued for
// nobody, or by ada, by the engine alone, at version 1.
//
// Mutations: no carrier on the ejection's, or the settle's, routing — dev and
// cto woken for nobody, red; writerOf passing no engine carrier — both, red.
func TestAnEjectionAndAnUnmadeCloseWakeForWhomTheirEnqueueWasMade(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	founder := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"}
	item := &types.WorkItem{Backend: types.WorkWorklet, ID: "task-7", Key: "ENG-7", Project: "ENG"}
	// told is what the record at at carries and wakes: its one routing
	// carrying want beside no actor, at its version, and the seats seats
	// woken, each written by the engine for want.
	told := func(t *testing.T, what string, at statelog.Position, want *types.WorkingFor, seats ...string) {
		t.Helper()
		routings := routingAt(t, n, at)
		if len(routings) != 1 || routings[0].Actor != "" {
			t.Fatalf("the %s carries %+v, want one routing naming no actor", what, routings)
		}
		if f, v := routings[0].WorkingFor, n.recordAt(at).V; !reflect.DeepEqual(f, want) || v != versionOf(want) {
			t.Errorf("the %s's routing carries %+v at version %d, want %+v at %d", what, f, v, want,
				versionOf(want))
		}
		_, relayed, routed := wakesOf(t, feedRecord(t, n, at))
		got := woken(routed)
		slices.Sort(got)
		if !relayed || !slices.Equal(got, seats) {
			t.Fatalf("the %s wakes %v (relayed %v), want %v", what, got, relayed, seats)
		}
		writer := types.NotificationWriter{Kind: iam.ActorSystem, WorkingFor: want}
		for _, r := range routed {
			if r.Writer == nil || !reflect.DeepEqual(*r.Writer, writer) {
				t.Errorf("the %s wakes %s written by %s, want %s", what, r.To.Handle, writtenBy(r.Writer),
					writtenBy(&writer))
			}
		}
	}
	for i, tc := range enqueuedForCases(founder) {
		t.Run(tc.name, func(t *testing.T) {
			// THE EJECTION: its entry put out of the queue, its train with it.
			repo, _, entryID, _ := queuedFor(t, n, c, w, fmt.Sprintf("ejected-%d", i), item, tc.by, tc.f)
			res, err := c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
				p.Eject = &gitlet.Eject{RepoID: repo, Entries: []gitlet.Ejection{{EntryID: entryID,
					Reason: gitlet.EjectCheckFailed, Checks: []string{"unit"}}}}
			}))
			seats := []string{"cto:dequeued", "dev:dequeued"}
			if tc.by.Kind != gitlet.PrincipalAgent {
				seats = []string{"dev:dequeued"}
			}
			told(t, "ejection", requireRecorded(t, res, err), tc.want, seats...)

			// THE UNMADE CLOSE: landed, its work item's close settled not
			// closed.
			repo, change, entryID, train := queuedFor(t, n, c, w, fmt.Sprintf("unclosed-%d", i), item, tc.by,
				tc.f)
			res, err = c.ProposeQueue(t.Context(), landingOf(repo, train.Candidate.Train, oidOf("A"),
				gitlet.LandEntry{EntryID: entryID, ChangeID: change, Commit: oidOf("M")}))
			requireRecorded(t, res, err)
			res, err = c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
				p.CloseSettled = &gitlet.CloseSettled{RepoID: repo, Settled: []gitlet.SettledClose{{
					EntryID: entryID, Outcome: gitlet.CloseNotClosed,
					Detail: "founder, for whom cto queued the change, does not use Worklet"}}}
			}))
			seats = []string{"cto:merged", "dev:merged"}
			if tc.by.Kind != gitlet.PrincipalAgent {
				seats = []string{"dev:merged"}
			}
			told(t, "unmade close", requireRecorded(t, res, err), tc.want, seats...)
		})
	}
}

// THE HOME'S QUEUE WORK SAYS WHOM EACH ENTRY WAS ENQUEUED FOR (ADR-0109): the
// one read answer carrying whom a code write was made for, since the home
// closes a landed change's work item for them, on both its paths: the target's
// live entries (QueueDue.Entries) and the closes a landing left owed
// (QueueOwedCloses). cto's entry enqueued in a turn founder woke answers
// founder on both; cto's for nobody, and ada's, nobody.
//
// Mutations: read the target's entries without the carrier (workEntriesOf) —
// cto's live entry answers nobody, red; the owed closes likewise
// (owedClosesIn) — cto's owed close answers nobody, red.
func TestTheHomesQueueWorkSaysWhomEachEntryWasEnqueuedFor(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	founder := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"}
	item := &types.WorkItem{Backend: types.WorkWorklet, ID: "task-7", Key: "ENG-7", Project: "ENG"}
	stale := statelog.Freshness{Level: statelog.ReadStale}
	for i, tc := range enqueuedForCases(founder) {
		t.Run(tc.name, func(t *testing.T) {
			repo, change, entryID, train := queuedFor(t, n, c, w, fmt.Sprintf("read-%d", i), item, tc.by, tc.f)
			a, err := n.reader.QueueDue(t.Context(), gitlet.QueueDueQuery{RepoID: repo, Fresh: stale})
			switch {
			case err != nil:
				t.Fatalf("read %s's queue work: %v", repo, err)
			case a.DueQueue == nil || len(a.DueQueue.Entries) != 1 || a.DueQueue.Entries[0].EntryID != entryID:
				t.Fatalf("%s's queue work answers %+v, want its one entry %s", repo, a.DueQueue, entryID)
			case !reflect.DeepEqual(a.DueQueue.Entries[0].WorkingFor, tc.want):
				t.Errorf("the live entry is answered enqueued for %+v, want %+v", a.DueQueue.Entries[0].WorkingFor,
					tc.want)
			}

			res, err := c.ProposeQueue(t.Context(), landingOf(repo, train.Candidate.Train, oidOf("A"),
				gitlet.LandEntry{EntryID: entryID, ChangeID: change, Commit: oidOf("M")}))
			requireRecorded(t, res, err)
			a, err = n.reader.QueueDue(t.Context(), gitlet.QueueDueQuery{RepoID: repo, Fresh: stale})
			switch {
			case err != nil:
				t.Fatalf("read %s's queue work: %v", repo, err)
			case len(a.QueueOwedCloses) != 1 || a.QueueOwedCloses[0].EntryID != entryID:
				t.Fatalf("%s's queue work owes %+v, want its landed entry %s's close", repo, a.QueueOwedCloses,
					entryID)
			case !reflect.DeepEqual(a.QueueOwedCloses[0].WorkingFor, tc.want):
				t.Errorf("the owed close is answered enqueued for %+v, want %+v", a.QueueOwedCloses[0].WorkingFor,
					tc.want)
			}
		})
	}
}

// queuedTwo is a repository named name, its main a queue target ([queueGate]) at A, holding
// dev's two ready changes into main, from feature at B and from release at R, each queued by
// ada: the repository and the two entries, in queue order.
func queuedTwo(t *testing.T, n *node, c *gitlet.Committer, w *gitlet.ChangeWriter,
	name string) (repo string, entries [2]string) {

	t.Helper()
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = queueGate()
	repo = n.ensure(name, s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(featureRef, gitlet.ZeroOID, oidOf("B")), set(releaseRef, gitlet.ZeroOID, oidOf("R"))))
	for i, source := range []struct{ ref, head string }{{featureRef, oidOf("B")}, {releaseRef, oidOf("R")}} {
		opened, err := c.ProposeChange(t.Context(), opening(repo, source.ref, mainRef, dev))
		requireChanged(t, opened, err)
		converse(t, w, n.revising(repo, opened.ChangeID, 1, source.head, mainRef, nil))
		converse(t, w, reviewing(repo, opened.ChangeID, platLead, 1, gitlet.VerdictApprove, nil))
		n.passed(repo, source.head, mainRef, "unit")
		enqueue := enqueueing(repo, opened.ChangeID, ada, source.head)
		res, err := c.ProposeQueue(t.Context(), enqueue)
		requireQueued(t, res, err, gitlet.QueueWaiting, i+1)
		entries[i] = enqueue.Enqueue.EntryID
	}
	return repo, entries
}

// batchOfTwo is a train on main onto A of ONE candidate merging both entries, the first as M1
// and the second as M2, its tip: the proposal and the candidate's id.
func batchOfTwo(repo string, entries [2]string) (gitlet.QueueProposal, string) {
	opID := statelog.NewOpID(time.Now(), "code_queue")
	train := gitlet.QueueTrainID(opID)
	candidate := gitlet.QueueCandidateID(train, 1)
	return gitlet.QueueProposal{OpID: opID, Candidate: &gitlet.Candidate{RepoID: repo, TargetRef: mainRef,
		Train: train, BaseOID: oidOf("A"), Pack: queuedPack(train), Candidates: []gitlet.TrainCandidate{{
			CandidateID: candidate, Ordinal: 1, TipOID: oidOf("M2"), Entries: []gitlet.CandidateEntry{
				{EntryID: entries[0], Commit: oidOf("M1")}, {EntryID: entries[1], Commit: oidOf("M2")}}}}}}, candidate
}

// failedOn is an eject of entry check_failed naming e2e and the train t.
func failedOn(repo, entry string, t *gitlet.EjectionTrain) gitlet.QueueProposal {
	return queueRecord(func(p *gitlet.QueueProposal) {
		p.Eject = &gitlet.Eject{RepoID: repo, Entries: []gitlet.Ejection{{EntryID: entry,
			Reason: gitlet.EjectCheckFailed, Checks: []string{"e2e"}, TestedIn: t}}}
	})
}

// AN EJECTION KEEPS THE TRAIN IT FAILED ON: two changes batched into one
// candidate onto A (M1, then M2 its tip); the first ejected check_failed naming
// the train, candidate, base A and M1, where e2e failed. The eject abandons the
// train, the second entry waiting in no candidate, and the first's row keeps
// the train, weighed by its bytes; the queue read and the change's card answer
// it, and the record is stamped [gitlet.TestedInVersion].
//
// Mutations: leave the train out of ejectedDetail — none named, red; weigh the
// row without the train — ejected_bytes is not its length, red.
func TestAnEjectionKeepsTheTrainItFailedOn(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, entries := queuedTwo(t, n, c, w, "web")
	train, candidate := batchOfTwo(repo, entries)
	res, err := c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)

	want := gitlet.EjectionTrain{CandidateID: candidate, BaseOID: oidOf("A"), CommitOID: oidOf("M1")}
	tested := want
	res, err = c.ProposeQueue(t.Context(), failedOn(repo, entries[0], &tested))
	at := requireRecorded(t, res, err)
	if v := n.recordAt(at).V; v != gitlet.TestedInVersion {
		t.Errorf("the eject naming its train is stamped %d, want %d (TestedInVersion)", v, gitlet.TestedInVersion)
	}
	if left := n.count(`SELECT COUNT(*) FROM code_candidates WHERE repo_id = ?`, repo); left != 0 {
		t.Fatalf("%d candidates outlive the train their entry was ejected from", left)
	}
	if state, _ := n.entryState(repo, entries[1]); state != gitlet.QueueWaiting ||
		n.count(`SELECT COUNT(*) FROM code_queue WHERE repo_id = ? AND entry_id = ? AND candidate_id = ''`,
			repo, entries[1]) != 1 {
		t.Fatalf("the entry batched behind the culprit is %s or still in a candidate, want waiting in none", state)
	}
	if kept := n.count(`SELECT COUNT(*) FROM code_queue WHERE repo_id = ? AND entry_id = ?
		AND state = 'ejected' AND ejected ->> '$.tested_in.candidate_id' = ?
		AND ejected ->> '$.tested_in.base_oid' = ? AND ejected ->> '$.tested_in.commit_oid' = ?
		AND ejected_bytes = length(CAST(ejected AS BLOB))`,
		repo, entries[0], want.CandidateID, want.BaseOID, want.CommitOID); kept != 1 {
		t.Fatalf("the ejected entry's row does not keep the train %+v weighed by its bytes", want)
	}

	var endedMicros int64
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT ended_at FROM code_queue WHERE repo_id = ? AND entry_id = ?`,
			repo, entries[0]).Scan(&endedMicros)
	}); err != nil {
		t.Fatalf("read the ejection's instant: %v", err)
	}
	soon := time.UnixMicro(endedMicros).UTC().Add(time.Hour)
	n.readAt.Store(&soon)
	answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadQueue, Asker: ada, Repo: "web"})
	if err != nil {
		t.Fatalf("read the queue: %v", err)
	}
	if len(answer.Ejected) != 1 || answer.Ejected[0].TestedIn == nil || *answer.Ejected[0].TestedIn != want {
		t.Fatalf("the queue read answers the ejections %+v, want the first entry's naming %+v", answer.Ejected, want)
	}
	if got := cardOf(t, n, 1).Ejected; got == nil || got.TestedIn == nil || *got.TestedIn != want {
		t.Fatalf("the ejected change's card carries %+v, want the train %+v", got, want)
	}
}

// AN EJECTION'S TRAIN IS HELD TO THE ROWS: one naming a train its entry is not
// testing in is refused stale, appending nothing — while the entry only waits,
// a candidate a newer train replaced, the second entry's commit for the first,
// another base; on a two-candidate train, the second entry naming the first
// candidate, or its own onto the first's base. The rows' train is recorded.
//
// Mutations: skip testedInStale in decideEject — each recorded, red; drop its
// check of the entry's candidate — the first candidate recorded, red; drop the
// candidate from its match — its own onto the first's base recorded, red.
func TestAnEjectionsTrainIsHeldToTheRows(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, entries := queuedTwo(t, n, c, w, "web")
	refused := func(what, entry string, tested gitlet.EjectionTrain) {
		t.Helper()
		before := n.end()
		res, err := c.ProposeQueue(t.Context(), failedOn(repo, entry, &tested))
		if res.Refusal != gitlet.MergeRefusalStale {
			t.Errorf("an ejection naming %s answered %+v, %v, want refused stale", what, res, err)
			return
		}
		requireQueueRefused(t, n, before, res, err, gitlet.MergeRefusalStale, "read its queue again")
	}

	first, replaced := batchOfTwo(repo, entries)
	refused("a train while its entry waits", entries[0], gitlet.EjectionTrain{CandidateID: replaced,
		BaseOID: oidOf("A"), CommitOID: oidOf("M1")})
	res, err := c.ProposeQueue(t.Context(), first)
	requireRecorded(t, res, err)
	second, candidate := batchOfTwo(repo, entries)
	res, err = c.ProposeQueue(t.Context(), second)
	requireRecorded(t, res, err)
	refused("the candidate a newer train replaced", entries[0], gitlet.EjectionTrain{CandidateID: replaced,
		BaseOID: oidOf("A"), CommitOID: oidOf("M1")})
	refused("the next entry's commit", entries[0], gitlet.EjectionTrain{CandidateID: candidate,
		BaseOID: oidOf("A"), CommitOID: oidOf("M2")})
	refused("another base", entries[0], gitlet.EjectionTrain{CandidateID: candidate, BaseOID: oidOf("Z"),
		CommitOID: oidOf("M1")})
	res, err = c.ProposeQueue(t.Context(), failedOn(repo, entries[0], &gitlet.EjectionTrain{
		CandidateID: candidate, BaseOID: oidOf("A"), CommitOID: oidOf("M1")}))
	requireRecorded(t, res, err)

	// Two candidates: the first merges entry one onto A as M1, the second entry two onto M1 as M2.
	repo, entries = queuedTwo(t, n, c, w, "api")
	pair := trainOf(repo, oidOf("A"), gitlet.CandidateEntry{EntryID: entries[0], Commit: oidOf("M1")},
		gitlet.CandidateEntry{EntryID: entries[1], Commit: oidOf("M2")})
	res, err = c.ProposeQueue(t.Context(), pair)
	requireRecorded(t, res, err)
	one, two := pair.Candidate.Candidates[0].CandidateID, pair.Candidate.Candidates[1].CandidateID
	refused("the candidate before its own, onto that one's base", entries[1], gitlet.EjectionTrain{
		CandidateID: one, BaseOID: oidOf("A"), CommitOID: oidOf("M2")})
	refused("its candidate onto the one before's base", entries[1], gitlet.EjectionTrain{CandidateID: two,
		BaseOID: oidOf("A"), CommitOID: oidOf("M2")})
	res, err = c.ProposeQueue(t.Context(), failedOn(repo, entries[1], &gitlet.EjectionTrain{
		CandidateID: two, BaseOID: oidOf("M1"), CommitOID: oidOf("M2")}))
	requireRecorded(t, res, err)
}

// ONLY A FAILED CHECK'S EJECTION NAMES ITS TRAIN, whole: a train beside a conflict, beside
// no checks, under a candidate that is no UUIDv7, onto a base that is no whole object id, or
// at the base itself is refused naming the ejection's tested_in; a whole one is valid.
//
// Mutation: drop any of Eject.validate's train rules (that case is valid, red).
func TestOnlyAFailedChecksEjectionNamesItsTrain(t *testing.T) {
	t.Parallel()
	const entryID = "0190f3c4-0000-7000-8000-0000000000e1"
	whole := gitlet.EjectionTrain{CandidateID: gitlet.QueueCandidateID(statelog.NewOpID(time.Now(), ""), 1),
		BaseOID: oidOf("A"), CommitOID: oidOf("M")}
	eject := func(x gitlet.Ejection) gitlet.QueueProposal {
		return queueRecord(func(p *gitlet.QueueProposal) {
			p.Eject = &gitlet.Eject{RepoID: "0190f3c4-0000-7000-8000-00000000c0de", Entries: []gitlet.Ejection{x}}
		})
	}
	with := func(set func(*gitlet.EjectionTrain)) *gitlet.EjectionTrain {
		tr := whole
		set(&tr)
		return &tr
	}
	if err := eject(gitlet.Ejection{EntryID: entryID, Reason: gitlet.EjectCheckFailed, Checks: []string{"e2e"},
		TestedIn: &whole}).Validate(); err != nil {
		t.Fatalf("a failed check's ejection naming its whole train is refused: %v", err)
	}
	for name, tc := range map[string]struct {
		x    gitlet.Ejection
		says string
	}{
		"a conflict's": {gitlet.Ejection{Reason: gitlet.EjectConflict, TestedIn: &whole},
			"only a failed check's ejection names the train"},
		"one naming no checks": {gitlet.Ejection{Reason: gitlet.EjectCheckFailed, TestedIn: &whole},
			"names the checks that failed on its train"},
		"a candidate that is no UUIDv7": {gitlet.Ejection{Reason: gitlet.EjectCheckFailed, Checks: []string{"e2e"},
			TestedIn: with(func(tr *gitlet.EjectionTrain) { tr.CandidateID = "candidate-1" })},
			"tested_in.candidate_id"},
		"a short base": {gitlet.Ejection{Reason: gitlet.EjectCheckFailed, Checks: []string{"e2e"},
			TestedIn: with(func(tr *gitlet.EjectionTrain) { tr.BaseOID = "abc123" })}, "tested_in.base_oid"},
		"a zero commit": {gitlet.Ejection{Reason: gitlet.EjectCheckFailed, Checks: []string{"e2e"},
			TestedIn: with(func(tr *gitlet.EjectionTrain) { tr.CommitOID = gitlet.ZeroOID })},
			"tested_in.commit_oid"},
		"the base as its commit": {gitlet.Ejection{Reason: gitlet.EjectCheckFailed, Checks: []string{"e2e"},
			TestedIn: with(func(tr *gitlet.EjectionTrain) { tr.CommitOID = tr.BaseOID })},
			"is not " + oidOf("A")[:7] + " itself"},
	} {
		tc.x.EntryID = entryID
		err := eject(tc.x).Validate()
		if !errors.Is(err, gitlet.ErrInvalidProposal) || !strings.Contains(err.Error(), "eject.entries[0].tested_in") ||
			!strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s train is answered %v, want it refused naming eject.entries[0].tested_in: %q", name, err,
				tc.says)
		}
	}
}

// ONLY AN EJECTION NAMING ITS TRAIN STAMPS ITS RECORD: of the ejects a queue lands, the one
// naming its train is stamped [gitlet.TestedInVersion]; a failed check's naming none and a
// conflict's are base format, 1.
//
// Mutations: drop the Ejection.TestedIn row (stamped 1, red); tag the field without
// omitempty (every eject stamped 7, red).
func TestOnlyAnEjectionNamingItsTrainStampsItsRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, entries := queuedTwo(t, n, c, w, "web")
	train, candidate := batchOfTwo(repo, entries)
	res, err := c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)

	stamped := func(what string, p gitlet.QueueProposal, want int) {
		t.Helper()
		got, proposeErr := c.ProposeQueue(t.Context(), p)
		if v := n.recordAt(requireRecorded(t, got, proposeErr)).V; v != want {
			t.Errorf("%s is stamped %d, want %d", what, v, want)
		}
	}
	stamped("a culprit's eject naming its train", failedOn(repo, entries[0], &gitlet.EjectionTrain{
		CandidateID: candidate, BaseOID: oidOf("A"), CommitOID: oidOf("M1")}), gitlet.TestedInVersion)
	stamped("a failed check's eject naming no train", failedOn(repo, entries[1], nil), 1)
	repo, entries = queuedTwo(t, n, c, w, "api")
	stamped("a conflict's eject", queueRecord(func(p *gitlet.QueueProposal) {
		p.Eject = &gitlet.Eject{RepoID: repo, Entries: []gitlet.Ejection{{EntryID: entries[0],
			Reason: gitlet.EjectConflict, Paths: []string{"go.mod"}}}}
	}), 1)
}
