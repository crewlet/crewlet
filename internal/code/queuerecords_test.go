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

// laneRepo is queueRepo with main landing post_land: a lane lands changes ahead
// of the checks its tip then runs.
func laneRepo(t *testing.T, n *node, c *code.Committer, name string) (repo, change string) {
	t.Helper()
	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	gate := queueGate()
	gate.Landing.Policy = code.LandingPostLand
	s.Protections[0].Gate = gate
	repo = n.ensure(name, s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
		set(featureRef, code.ZeroOID, oidOf("B")), set(releaseRef, code.ZeroOID, oidOf("R"))))
	opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	return repo, opened.ChangeID
}

// queuedPack is a pack named id, as a home's build of a train or a chain
// stores it.
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

// landing is a landing on main of entries — each change's entry and the commit
// it lands as, in chain order — onto base, under policy (a pre_land one of
// train), under an operation id of its own.
func landingOf(repo string, policy code.LandingPolicy, train, base string, entries ...code.LandEntry) code.QueueProposal {
	opID := statelog.NewOpID(time.Now(), "code.queue")
	l := &code.Land{RepoID: repo, LandID: code.QueueLandID(opID), TargetRef: mainRef, BaseOID: base,
		TipOID: entries[len(entries)-1].Commit, Policy: policy, Train: train, Entries: entries, Node: "git-a",
		ReceivedAt: harnessAt}
	if policy == code.LandingPostLand {
		l.Pack = queuedPack(l.LandID)
	}
	return code.QueueProposal{OpID: opID, Land: l}
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
// at its tip, the entry landed and the train's landed candidate gone.
//
// Mutation: skip the re-proof (decide a landing without judging each entry's
// gate) — the landing applies over the review asking for changes, red.
func TestALandingReprovesEveryEntrysGate(t *testing.T) {
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

	train := training(repo, oidOf("A"), entryID, oidOf("M"))
	res, err = c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	if state, _ := n.entryState(repo, entryID); state != code.QueueTesting {
		t.Fatalf("an entry its train tests is %s, want testing", state)
	}
	n.passed(repo, oidOf("M"), mainRef, "unit")

	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictRequestChanges, nil))
	land := landingOf(repo, code.LandingPreLand, train.Candidate.Train, oidOf("A"),
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
}

// A POST_LAND RECORD FITS ITS CAPS (M5-D14): a lane's whole chain — sixty-four
// changes, MaxLandEntries — is ONE land record, its payload within what one
// proposal may carry and its rows but the changes its pushes move within
// MaxRecordRows with the horizons' room, which the decide adds the moved
// changes to and holds whole; one entry more is refused before it is queued.
//
// Mutation: take one entry past MaxLandEntries in a land's validation — the
// 65th is taken, red.
func TestAPostLandRecordFitsItsCaps(t *testing.T) {
	t.Parallel()
	repo := uuid.Must(uuid.NewV7()).String()
	entries := make([]code.LandEntry, code.MaxLandEntries+1)
	for i := range entries {
		entries[i] = code.LandEntry{EntryID: uuid.Must(uuid.NewV7()).String(),
			ChangeID: uuid.Must(uuid.NewV7()).String(), Commit: oidOf(fmt.Sprint("commit", i))}
	}
	full := landingOf(repo, code.LandingPostLand, "", oidOf("A"), entries[:code.MaxLandEntries]...)
	if err := full.Validate(); err != nil {
		t.Fatalf("a lane of %d changes is refused: %v", code.MaxLandEntries, err)
	}
	if got := code.RecordRows(code.LandRecordRows(code.MaxLandEntries)); got > code.MaxRecordRows {
		t.Fatalf("a land of %d entries writes up to %d rows with its horizons' room, past MaxRecordRows (%d)",
			code.MaxLandEntries, got, code.MaxRecordRows)
	}
	over := landingOf(repo, code.LandingPostLand, "", oidOf("A"), entries...)
	if err := over.Validate(); err == nil || !strings.Contains(err.Error(), "MaxLandEntries") {
		t.Fatalf("a lane of %d changes validated (%v), want refused past MaxLandEntries", len(entries), err)
	}
}

// GREEN ADVANCES ONLY ON GREEN (M5-D16): dev's change lands ahead of its checks
// on a post_land main — its landing row not green, main's green row untouched.
// Its required check FAILS on the landed tip: a green naming that landing is
// refused not_green, appending nothing, and a broken marks the landing broken
// while main's green row stays where it was. The check run again PASSES: the
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
	repo, change := laneRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	land := landingOf(repo, code.LandingPostLand, "", oidOf("A"),
		code.LandEntry{EntryID: enqueue.Enqueue.EntryID, ChangeID: change, Commit: oidOf("M")})
	res, err = c.ProposeQueue(t.Context(), land)
	landed := requireRecorded(t, res, err)
	if got := n.landingsOf(repo, mainRef); len(got) != 1 || got[0].kind != string(code.LandedPostLand) ||
		got[0].green {
		t.Fatalf("main's landings read %+v, want one post_land landing, not green", got)
	}
	if green, _ := n.greenOf(repo, mainRef); green != "" {
		t.Fatalf("main's green row is at %q: a lane lands ahead of its checks", green)
	}

	n.checked(repo, oidOf("M"), mainRef, "unit", 1, code.CheckStateFailed)
	green := queueRecord(func(p *code.QueueProposal) {
		p.Green = &code.Green{RepoID: repo, TargetRef: mainRef, OID: oidOf("M"), Through: landed}
	})
	before := n.end()
	res, err = c.ProposeQueue(t.Context(), green)
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
	fits := landingOf(repo, code.LandingPostLand, "", oidOf("A"), entries[:code.MaxScopePaths-2]...)
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
	wide := landingOf(repo, code.LandingPostLand, "", oidOf("A"), entries...)
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
			delete_source, close_task, auto_revert, enqueued_by, enqueued_kind, enqueued_operator, state,
			enqueued_at, ended_at, ended_position, end_reason)
			VALUES (?, ?, 'refs/heads/main', ?, 1, ?, 'merge', 0, 0, 0, 'dev', 'agent', '', ?, ?, ?, 2, '')`,
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
		payload.RepoName != "web" || ev.Source != n.id {
		t.Fatalf("the ejection was announced as %s from %q %+v, want web!1 ejected for unit, from %s", ev.Type,
			ev.Source, ev.Data, n.id)
	}
	before := n.end()
	res, err = c.ProposeQueue(t.Context(), queueRecord(func(p *code.QueueProposal) {
		p.Eject = &code.Eject{RepoID: repo, Entries: []code.Ejection{{EntryID: entryID, Reason: code.EjectConflict}}}
	}))
	requireQueueRefused(t, n, before, res, err, code.MergeRefusalNotQueued, "is ejected")
}
