package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// quiet keeps a benchmark's output to its figures: an estate logs at info, and
// benchstat cannot read an info line amid the figures (`make bench` runs this
// package).
func quiet(b *testing.B) {
	logging.SetVerbosity(slog.LevelWarn, logging.FormatConsole)
	b.Cleanup(func() { logging.SetVerbosity(slog.LevelInfo, logging.FormatConsole) })
}

// widestPushRow is the commands and introduced ids of the widest row a push
// stores: MaxCommandsPerPush commands at MaxRefNameBytes refs, each refused
// with the longest code a command records, and MaxIntroduced ids. About 470 KB,
// some fourteen times a body at MaxChangeBodyBytes: the domain's widest row.
func widestPushRow(b *testing.B) (commands, introduced string) {
	b.Helper()
	longest := Refusal("")
	for _, r := range Refusals() {
		if r.Recorded() && len(r) > len(longest) {
			longest = r
		}
	}
	cmds := make([]Command, MaxCommandsPerPush)
	for c := range cmds {
		ref := fmt.Sprintf("refs/heads/%08d", c)
		cmds[c] = Command{Ref: ref + strings.Repeat("x", MaxRefNameBytes-len(ref)),
			Old: oid(fmt.Sprint("old", c)), New: oid(fmt.Sprint("new", c)), Forced: true,
			Result: ResultNG, Reason: longest}
	}
	commands, err := recordedCommands(cmds)
	if err != nil {
		b.Fatal(err)
	}
	ids := make([]string, MaxIntroduced)
	for i := range ids {
		ids[i] = oid(fmt.Sprint("introduced", i))
	}
	return commands, strings.Join(ids, " ")
}

// seedWide writes n push rows of a repository at the widest a row can be,
// stamped at the epoch, so the history horizon may take them and a purge takes
// them first.
func (e *estate) seedWide(b *testing.B, id string, n int) {
	b.Helper()
	commands, introduced := widestPushRow(b)
	ctx := context.Background()
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, e.db.Caps().MaxVariables,
			`INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, via, commands,
				introduced, introduced_count, received_at, row_bytes) VALUES `,
			`(?, ?, ?, ?, 'seed', 'operator', 'git', ?, ?, ?, 0, ?)`, ``, n, func(i int) []any {
				return []any{fmt.Sprintf("%s-wide-%04d", id, i), id, i + 1, i, commands, introduced,
					MaxIntroduced, len(commands) + len(introduced)}
			})
		return err
	}); err != nil {
		b.Fatalf("seed %s's wide pushes: %v", id, err)
	}
	e.recountActivity(id)
}

// seedNarrow writes n push rows of a repository of about a kilobyte each,
// stamped at the epoch: pushes moving a few refs each.
func (e *estate) seedNarrow(id string, n int) {
	e.t.Helper()
	commands := `[` + strings.Repeat(`{"ref":"refs/heads/main","result":"ok"},`, 25) +
		`{"ref":"refs/heads/main","result":"ok"}]`
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, via, commands,
			received_at, row_bytes)
		SELECT printf('%s-narrow-%06d', ?, i), ?, i + 1, i, 'seed', 'operator', 'git', ?, 0, ? FROM k`,
		n, id, id, commands, len(commands))
	e.recountActivity(id)
}

// seedSpread writes n narrow push rows of a repository stamped at the epoch's days, the i-th on day
// i % days, each by a person of its own, and counts them: the activity's widest keys a row.
func (e *estate) seedSpread(id string, n, days int) {
	e.t.Helper()
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, commands,
			received_at, row_bytes)
		SELECT printf('%s-spread-%06d', ?, i), ?, i + 1, (i % ?) * ?, printf('pat:%06d', i), 'operator',
			printf('person-%06d', i), 'git', '[]', 0, 2 FROM k`, n, id, id, days, dayMicros)
	e.recountActivity(id)
}

// seedChangeRows writes changes of a repository, numbered from 1, each with
// revisions revisions, authors authors and, where body is not 0, a body of
// that many bytes its change weighs: the rows a purge part walks.
func (e *estate) seedChangeRows(id string, changes, revisions, authors, body int) {
	e.t.Helper()
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref, target_ref,
			head_oid, revision, owner, owner_kind, opened_at, opened_position, version, position)
		SELECT printf('%s-change-%06d', ?, i), ?, i + 1, 'a change', ?, 'open',
			printf('refs/heads/change-%06d', i), 'refs/heads/main', printf('%040d', i), ?, 'seat', 'agent',
			0, i + 1, 1, i + 1 FROM k`, changes, id, id, body, revisions)
	if body > 0 {
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_change_bodies (change_id, body)
			SELECT printf('%s-change-%06d', ?, i), ? FROM k`, changes, id, strings.Repeat("b", body))
	}
	if revisions > 0 {
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_revisions (change_id, n, head_oid, pusher, pusher_kind, position, at)
			SELECT printf('%s-change-%06d', ?, i / ?), i % ? + 1, printf('%040d', i), 'seat', 'agent', 1, 0
			FROM k`, changes*revisions, id, revisions, revisions)
	}
	if authors > 0 {
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_change_authors (change_id, author, author_kind, first_revision, position)
			SELECT printf('%s-change-%06d', ?, i / ?), printf('seat-%05d', i % ?), 'agent', 1, 1 FROM k`,
			changes*authors, id, authors, authors)
	}
	// THE COUNTS THE SEEDED ROWS IMPLY, as an upgraded node re-derives them.
	e.rederive()
}

// seedNotices writes perChange notices on each of a repository's first changes
// changes (seedChangeRows), each to a handle of its own with an excerpt at
// MaxNoticeExcerpt, the widest notice, and counts them on their change as the
// applier does (code_changes.notices).
func (e *estate) seedNotices(id string, changes, perChange int) {
	e.t.Helper()
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_notifications (record_id, recipient, recipient_kind, repo_id, change_id, change_ref,
			reason, addressed, fallback_only, fallback_rank, excerpt, created_at, log_seq, log_stream,
			log_generation)
		SELECT printf('%s-record-%06d', ?, i / ?), printf('reader-%05d', i % ?), 'person', ?,
			printf('%s-change-%06d', ?, i / ?), 'r!1', 'thread', 0, 0, 0, ?, 0, i + 1, 'code', 1 FROM k`,
		changes*perChange, id, perChange, perChange, id, id, perChange, strings.Repeat("e", MaxNoticeExcerpt))
	e.exec(`UPDATE code_changes SET notices = ? WHERE repo_id = ? AND number <= ?`, perChange, id, changes)
}

// seedLiveChanges writes n live changes of a repository at one revision each
// (seedChangeRows), each source branch (liveSource) live at its head
// (liveHead) and counted, and "op", whom every pushEntry and benchProposal
// names, as an author of each: so a push moving one costs its record the
// change's row and revision but no author, the most changes one record moves.
func (e *estate) seedLiveChanges(id string, n int) {
	e.t.Helper()
	e.seedChangeRows(id, n, 1, 0, 0)
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_refs (repo_id, ref, target, position, at)
		SELECT ?, printf('refs/heads/change-%06d', i), printf('%040d', i), 1, 0 FROM k`, n, id)
	e.exec(`UPDATE code_repos SET refs = refs + ?, branches = branches + ? WHERE id = ?`, n, n, id)
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_change_authors (change_id, author, author_kind, first_revision, position)
		SELECT printf('%s-change-%06d', ?, i), 'op', 'person', 1, 1 FROM k`, n, id)
}

// liveSource is the source branch of the i-th change seedLiveChanges seeds,
// counted from 0, and liveHead its head, the commit that branch is at.
func liveSource(i int) string { return fmt.Sprintf("refs/heads/change-%06d", i) }
func liveHead(i int) string   { return fmt.Sprintf("%040d", i) }

// rebasingChanges is the record that moves the most live changes: heads, a
// push of MaxCommandsPerPush fast-forwards of sources by their author (a ref,
// the change's row and its revision each), and closes, a push deleting as many
// more sources as the record's rows leave (a ref, the sweep's room for its
// tombstone and the object it left ([commandRows]), and the change's row
// each). Counted as the push decide counts; [estate.decidedRecord] holds it.
func rebasingChanges() (heads, closes int) {
	heads = MaxCommandsPerPush
	moving := PushEntry{Pack: &PackRef{}, Commands: make([]Command, heads)}
	for c := range moving.Commands {
		moving.Commands[c] = move(liveSource(c), liveHead(c), oid(fmt.Sprint("rebased", c)))
	}
	rows := entryRows(moving) + 2*heads
	deleting := PushEntry{Pack: &PackRef{}}
	for {
		deleting.Commands = append(deleting.Commands, move(liveSource(heads+closes), liveHead(heads+closes),
			ZeroOID))
		if recordRows(rows+entryRows(deleting)+len(deleting.Commands)) > MaxRecordRows {
			return heads, closes
		}
		closes++
	}
}

// decidedRecord is the record the push decide forms over e's rows of repo from
// pushes (undecided, each pack minted at the decide's instant), failing b
// unless every push is in it whole within MaxRecordRows: a benchmark applies
// only a record a committer forms ([estate.admitted]'s rule, for a push). It
// answers the entries and the change rows the decide counted ([changeBook]).
func (e *estate) decidedRecord(b *testing.B, repo string, pushes []PushEntry) ([]PushEntry, int) {
	b.Helper()
	batch := make([]Proposal, len(pushes))
	for i, p := range pushes {
		cmds := make([]Command, len(p.Commands))
		for c, cmd := range p.Commands {
			cmds[c] = Command{Ref: cmd.Ref, Old: cmd.Old, New: cmd.New, Forced: cmd.Forced}
		}
		batch[i] = withPack(Proposal{PushID: p.PushID, RepoID: repo, Principal: p.Principal, Via: p.Via,
			Node: p.Node, Commands: cmds, ReceivedAt: p.ReceivedAt}, p.Pack.Size, applyClock)
	}
	var (
		entries    []PushEntry
		answers    []decided
		changeRows int
	)
	ctx := context.Background()
	if err := e.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		var moves batchMoves
		entries, answers, moves, _, err = decideBatch(ctx, tx, repo, batch, DefaultLimits(),
			DefaultDeletedRetention, SeatRules{}, applyClock, &verdicts{})
		changeRows = moves.rows
		return err
	}); err != nil {
		b.Fatalf("decide the record: %v", err)
	}
	for i, a := range answers {
		if !a.inRecord || slices.ContainsFunc(a.commands, func(c Command) bool { return c.Result != ResultOK }) {
			b.Fatalf("push %d of %d is not in the record whole (carried %t, refused %q: %s)", i, len(batch),
				a.carried, a.refused, a.detail)
		}
	}
	rows := changeRows
	for _, entry := range entries {
		rows += entryRows(entry)
	}
	if recordRows(rows) > MaxRecordRows {
		b.Fatalf("the decide formed a record of %d rows, past MaxRecordRows (%d)", recordRows(rows), MaxRecordRows)
	}
	return entries, changeRows
}

// changeRowsOf counts the rows of a repository's changes (seedChangeRows) the
// estate still holds, revisions, authors and bodies found by their ids.
func (e *estate) changeRowsOf(id string) int64 {
	e.t.Helper()
	prefix := id + "-change-%"
	return e.num(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ?`, id) +
		e.num(`SELECT COUNT(*) FROM code_revisions WHERE change_id LIKE ?`, prefix) +
		e.num(`SELECT COUNT(*) FROM code_change_authors WHERE change_id LIKE ?`, prefix) +
		e.num(`SELECT COUNT(*) FROM code_change_bodies WHERE change_id LIKE ?`, prefix)
}

// seedTombstones writes n ref tombstones into a repository, deleted at, and
// counts them, as a sweep finds them.
func (e *estate) seedTombstones(id string, n int, at time.Time) {
	e.t.Helper()
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_refs (repo_id, ref, target, position, at)
		SELECT ?, printf('refs/heads/gone-%06d', i), '', 1, ? FROM k`, n, id, store.EncodeTime(at))
	e.exec(`UPDATE code_repos SET tombstones = tombstones + ? WHERE id = ?`, n, id)
}

// applyTimed times one record's Apply and commit, the time it holds the write
// lane, and holds the rows it reported to at least rows.
func (e *estate) applyTimed(b *testing.B, a *Applier, r built, rows int) {
	b.Helper()
	ctx := context.Background()
	env, err := Domain{}.Envelope(r.payload)
	if err != nil {
		b.Fatal(err)
	}
	record := statelog.Record{Envelope: env, Payload: r.payload, StoredAt: applyClock,
		Position: statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: e.next}}
	e.next++
	opts := statelog.ApplyOptions{StoredAt: applyClock, MaxVariables: e.db.Caps().MaxVariables}
	b.StartTimer()
	err = e.db.Tx(ctx, func(tx *sql.Tx) error {
		n, err := a.Apply(ctx, tx, record, opts)
		if err == nil && n < rows {
			err = fmt.Errorf("the record wrote %d rows, want at least %d", n, rows)
		}
		return err
	})
	b.StopTimer()
	if err != nil {
		b.Fatal(err)
	}
}

// fullest is the pushes of the record the committer forms with the most
// commands of one kind: pushes of MaxCommandsPerPush commands, each with its
// pack, and a last push of what the row bound leaves, held by [rowBound] and
// [recordRows] themselves to leave no room for another command. command is
// the c-th command of push p.
func fullest(tb testing.TB, prefix string, i int, command func(p, c int) Command) []PushEntry {
	tb.Helper()
	var entries []PushEntry
	var rows int
	for p := 0; ; p++ {
		e := pushEntry(fmt.Sprint(prefix, "-", i, "-", p))
		for c := 0; c < MaxCommandsPerPush; c++ {
			e.Commands = append(e.Commands, command(p, c))
			if recordRows(rows+rowBound(Proposal{Commands: e.Commands, Pack: e.Pack})) > MaxRecordRows {
				e.Commands = e.Commands[:c]
				break
			}
		}
		if len(e.Commands) == 0 {
			break
		}
		entries = append(entries, e)
		rows += rowBound(Proposal{Commands: e.Commands, Pack: e.Pack})
	}
	// FULL: no further command of the kind fits what the bound leaves.
	if left := MaxRecordRows - recordRows(rows); left < 0 || left >= commandRows(command(len(entries), 0)) {
		tb.Fatalf("the fullest record of %s bounds %d rows, %d short of MaxRecordRows (%d)", prefix,
			recordRows(rows), left, MaxRecordRows)
	}
	return entries
}

// BenchmarkApplyAPushRecord is a push record APPLIED AND COMMITTED, the time it
// holds the replicated estate's one write lane that every other domain's
// applier on the node waits behind, into a repository of its own each
// iteration, set up outside the timer (10 000 refs unless its shape seeds
// otherwise). Its shapes, each described at its entry, are the records the
// committer forms and the ones the horizons make longest; removal alone is a
// record no committer of this build forms, whose cost the applier still
// bounds. The change shapes are decided over the rows first
// ([estate.decidedRecord]), since what a record writes about changes is what
// the repository holds. Each is held against statelog.ApplyTxTimeBudget
// (250 ms), checked only at a record's boundary; doc.go's "Measured" section
// records the figures.
func BenchmarkApplyAPushRecord(b *testing.B) {
	quiet(b)
	// A PUSH ID IS THE FLEET'S, not the repository's, so every iteration's
	// pushes carry their own: a repeated one is a redelivery and applies as
	// nothing.
	maximal := func(i int) []PushEntry {
		return fullest(b, "max", i, func(p, c int) Command {
			return move(fmt.Sprintf("refs/heads/new/%d/%04d", p, c), ZeroOID, oid(fmt.Sprint(p, c)))
		})
	}
	// THE DELETIONS OF e.seed's refs, as many as the committer puts in one
	// record.
	deleting := func(i int) []PushEntry {
		return fullest(b, "deleting", i, func(p, c int) Command {
			return move(fmt.Sprintf("refs/heads/r%06d", p*MaxCommandsPerPush+c), oid("seeded"), ZeroOID)
		})
	}
	group := func(i int) []PushEntry {
		var entries []PushEntry
		for p := range MaxPushesPerRecord {
			entries = append(entries, pushEntry(fmt.Sprint("tag-", i, "-", p),
				move(fmt.Sprintf("refs/tags/build-%02d", p), ZeroOID, oid(fmt.Sprint(p)))))
		}
		return entries
	}
	one := func(i int) []PushEntry {
		return []PushEntry{pushEntry(fmt.Sprint("one-", i), move("refs/heads/main", ZeroOID, oid("main")))}
	}
	// A GROUP BY AS MANY SEATS, each its own pusher day.
	pushers := func(i int) []PushEntry {
		entries := group(i)
		for p := range entries {
			entries[p].Principal = Principal{Kind: PrincipalAgent, Seat: fmt.Sprint("seat-", p)}
		}
		return entries
	}
	// THE FORCED UPDATES OF e.seed's refs, one maximal push of them.
	forcing := func(i int) []PushEntry {
		var cmds []Command
		for c := range MaxCommandsPerPush {
			cmds = append(cmds, forced(fmt.Sprintf("refs/heads/r%06d", c), oid("seeded"),
				oid(fmt.Sprint("forced", c))))
		}
		return []PushEntry{pushEntry(fmt.Sprint("forced-", i), cmds...)}
	}
	// THE DELETIONS OF e.seed's refs, three maximal pushes of them, which no
	// committer of this build puts in one record.
	removing := func(i int) []PushEntry {
		var entries []PushEntry
		for p := range 3 {
			var cmds []Command
			for c := range MaxCommandsPerPush {
				cmds = append(cmds, move(fmt.Sprintf("refs/heads/r%06d", p*MaxCommandsPerPush+c),
					oid("seeded"), ZeroOID))
			}
			entries = append(entries, pushEntry(fmt.Sprint("del-", i, "-", p), cmds...))
		}
		return entries
	}
	// A RECORD'S WORTH OF LIVE CHANGES, rebasingChanges' shape, and every
	// change it moves seeded live; and a group of pushes each moving one.
	heads, closes := rebasingChanges()
	rebasing := func(i int) []PushEntry {
		moves := make([]Command, heads)
		for c := range moves {
			moves[c] = move(liveSource(c), liveHead(c), oid(fmt.Sprint("rebased", c)))
		}
		deletions := make([]Command, closes)
		for c := range deletions {
			deletions[c] = move(liveSource(heads+c), liveHead(heads+c), ZeroOID)
		}
		return []PushEntry{pushEntry(fmt.Sprint("rebase-", i, "-heads"), moves...),
			pushEntry(fmt.Sprint("rebase-", i, "-closes"), deletions...)}
	}
	revising := func(i int) []PushEntry {
		var entries []PushEntry
		for p := range MaxPushesPerRecord {
			entries = append(entries, pushEntry(fmt.Sprint("revise-", i, "-", p),
				move(liveSource(p), liveHead(p), oid(fmt.Sprint("revised", p)))))
		}
		return entries
	}
	// THE WIDEST ROUTING A RECORD CARRIES: one push merging as many changes
	// as its rows leave room for, each telling MaxRoutedParticipants authors
	// of its own that it merged — a notice a handle and the change's count —
	// held by the decide's own count of them (noticeRows) to what
	// MaxRecordRows leaves of the push's rows.
	perMerged := MaxRoutedParticipants + 1
	merged := (MaxRecordRows - entryRows(one(0)[0])) / perMerged
	noticing := func(i int) []PushEntry {
		repo := fmt.Sprintf("repo-%06d", i)
		entries := one(i)
		for c := range merged {
			r := ChangeRouting{ChangeID: fmt.Sprintf("%s-change-%06d", repo, c), ChangeRef: fmt.Sprintf("r!%d", c+1),
				Merged: true}
			for a := range MaxRoutedParticipants {
				r.Authors = append(r.Authors, fmt.Sprintf("author-%03d-%02d", c, a))
			}
			entries[0].Routing = append(entries[0].Routing, r)
		}
		if rows := entryRows(entries[0]) + noticeRows(entries[0].Routing); rows > MaxRecordRows {
			b.Fatalf("the widest routing's record counts %d rows, past MaxRecordRows (%d)", rows, MaxRecordRows)
		}
		return entries
	}
	liveAt := func(changes int) func(e *estate, repo string) {
		return func(e *estate, repo string) {
			e.seed(repo, 0, 10_000, 0)
			e.seedLiveChanges(repo, changes)
		}
	}
	expired := time.Unix(0, 0)
	// THE ROWS EACH SHAPE MUST TOUCH, from the entries themselves: a ref,
	// a pack and a push row an entry, and the counters.
	written := func(entries []PushEntry) int {
		n := 1
		for _, e := range entries {
			n += len(e.Commands) + 2
		}
		return n
	}
	deletionsAt := func(e *estate, repo string) {
		e.seed(repo, 0, 2*MaxCommandsPerPush, 0)
		e.seedTombstones(repo, MaxTombstonesPerRepo, expired)
	}
	for _, shape := range []struct {
		name    string
		entries func(int) []PushEntry
		seed    func(e *estate, repo string)
		history bool
		rows    int
		// decide shapes are decided over the rows first, their rows their
		// entries' and the change rows the decide counted; revised and closed
		// are the changes the record must leave at revision 2 and closed.
		decide          bool
		revised, closed int
	}{
		// MAXIMAL: the most ref creations a record carries (fullest), nothing
		// left for the sweep since it makes no tombstone; +history waits
		// HorizonDeletesPerRecord of the widest expired push rows, of which
		// HorizonBytesPerRecord bounds the removal to three.
		{name: "maximal", entries: maximal, rows: written(maximal(0))},
		{name: "maximal+history", entries: maximal, history: true, rows: written(maximal(0))},
		// GROUP: what group commit turns a CI's burst of tag pushes into.
		{name: "group", entries: group, rows: 2 * MaxPushesPerRecord},
		// THE ACTIVITY AT ITS WIDEST: a pusher day an entry, and one move of every expired row's day and
		// pusher day down beside the push's up.
		{name: "group+pushers", entries: pushers, rows: 2*MaxPushesPerRecord + 1 + MaxPushesPerRecord},
		// ONE: the common case; +history waits HorizonDeletesPerRecord expired
		// rows, each on a day and by a pusher of its own: the activity's widest move.
		{name: "one", entries: one, rows: 3},
		{name: "one+history", entries: one, seed: func(e *estate, repo string) {
			e.seed(repo, 0, 10_000, 0)
			e.seedSpread(repo, HorizonDeletesPerRecord, HorizonDeletesPerRecord)
		}, rows: 3 + HorizonDeletesPerRecord + 2 + 2*HorizonDeletesPerRecord},
		// SWEEP: every expired tombstone the row budget leaves room for.
		{name: "sweep", entries: one, seed: func(e *estate, repo string) {
			e.seedTombstones(repo, MaxRecordRows, expired)
		}, rows: MaxRecordRows},
		// EVERY ROW OF THE BUDGET TOUCHED: the most deletions a record carries,
		// each three rows in the bound (rowBound: its tombstone, the sweep's
		// room for one, the object it left), and the sweep taking what they
		// leave of MaxTombstonesPerRepo expired: the most rows a record both
		// writes and deletes. +history is the longest record moving no change.
		{name: "deletions", entries: deleting, seed: deletionsAt, rows: MaxRecordRows},
		{name: "deletions+history", entries: deleting, seed: deletionsAt, history: true, rows: MaxRecordRows},
		// A RECORD THAT REMOVES: every ref it deletes at a repository already
		// holding MaxTombstonesPerRepo fresh tombstones, which no sweep can
		// take, so its 3 000 deletions are removals rather than rows, each
		// still writing the object it left. No committer forms it: two such
		// pushes never share a record.
		{name: "removal", entries: removing, seed: func(e *estate, repo string) {
			e.seed(repo, 0, 3*MaxCommandsPerPush, 0)
			e.seedTombstones(repo, MaxTombstonesPerRepo, applyClock.Add(-time.Hour))
		}, rows: 3 * (1 + 2*MaxCommandsPerPush + 1)},
		// EVERY ROW OF THE BUDGET TOUCHED BY THE DROPS: the record's own rows
		// and the expiry forgetting what they leave; forced writes a thousand
		// drops too, the most a record both writes and forgets.
		{name: "expiry", entries: one, seed: func(e *estate, repo string) {
			e.seedDrops(repo, 1, expired, MaxRecordRows)
		}, rows: MaxRecordRows},
		{name: "forced", entries: forcing, seed: func(e *estate, repo string) {
			e.seed(repo, 0, MaxCommandsPerPush, 0)
			e.seedDrops(repo, 1, expired, MaxRecordRows)
		}, rows: MaxRecordRows},
		// EVERY CHANGE ROW A RECORD CAN REWRITE: rebasingChanges, a bot's
		// force-push of every change it rebased, and a group whose every push
		// is a change's next revision.
		{name: "changes", entries: rebasing, seed: liveAt(heads + closes), decide: true, revised: heads,
			closed: closes},
		{name: "group+changes", entries: revising, seed: liveAt(MaxPushesPerRecord), decide: true,
			revised: MaxPushesPerRecord},
		// EVERY ROW THE PUSH'S OWN LEAVE A NOTICE OR A COUNT: the widest
		// routing, against the changes it names: the notice write at its widest.
		{name: "notices", entries: noticing, seed: func(e *estate, repo string) {
			e.seed(repo, 0, 10_000, 0)
			e.seedChangeRows(repo, merged, 1, 1, 0)
		}, rows: 3 + merged*perMerged},
	} {
		removal := shape.name == "removal"
		b.Run(shape.name, func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier(nil)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				repo := fmt.Sprintf("repo-%06d", i)
				e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
				if shape.history {
					e.seedWide(b, repo, HorizonDeletesPerRecord)
				}
				if shape.seed != nil {
					shape.seed(e, repo)
				} else {
					e.seed(repo, 0, 10_000, 0)
				}
				entries, rows := shape.entries(i), shape.rows
				if shape.decide {
					var changeRows int
					entries, changeRows = e.decidedRecord(b, repo, entries)
					rows = written(entries) + changeRows
				}
				e.applyTimed(b, a, pushOf(b, repo, entries...), rows)
				// EVERY NOTICE ITS ROUTING TELLS, and each change counting
				// its own.
				if shape.name == "notices" && (e.num(`SELECT COUNT(*) FROM code_notifications WHERE repo_id = ?`,
					repo) != int64(merged*MaxRoutedParticipants) || e.num(`SELECT COUNT(*) FROM code_changes
					WHERE repo_id = ? AND notices = ?`, repo, MaxRoutedParticipants) != int64(merged)) {
					b.Fatalf("the notices shape's record did not write and count %d notices for each of its %d "+
						"changes", MaxRoutedParticipants, merged)
				}
				// THE CHANGES IT NAMES, each moved as its command says.
				if shape.decide && (e.num(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ? AND revision = 2
					AND state = 'open'`, repo) != int64(shape.revised) || e.num(`SELECT COUNT(*) FROM code_changes
					WHERE repo_id = ? AND state = 'closed'`, repo) != int64(shape.closed)) {
					b.Fatalf("the %s shape's record did not move its %d changes' heads and close %d", shape.name,
						shape.revised, shape.closed)
				}
				// THE SHAPE IT NAMES: every deletion a removal, the count held.
				if removal && (e.num(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target = ''`,
					repo) != MaxTombstonesPerRepo || e.num(`SELECT refs FROM code_repos WHERE id = ?`, repo) != 0) {
					b.Fatal("the removal shape's record did not write its deletions as removals")
				}
				// AND A RECORD THE COMMITTER FORMS KEEPS EVERY DELETION A ROW.
				if strings.HasPrefix(shape.name, "deletions") {
					made := 0
					for _, p := range shape.entries(i) {
						made += len(p.Commands)
					}
					if kept := e.num(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target = ''
						AND ref LIKE 'refs/heads/r%'`, repo); kept != int64(made) {
						b.Fatalf("the deletions shape kept %d of its %d deletions as tombstones", kept, made)
					}
				}
			}
		})
	}
}

// BenchmarkApplyAPurgePart is a purge record APPLIED AND COMMITTED, in each
// shape its bounds allow (described at its entry), into a repository of its
// own each iteration, seeded outside the timer. The final shapes are the purge
// itself with MaxProtectionsPerRepo rules: the most its gate record's
// unbounded statements can meet, since the decide forms it only once what is
// left fits a part. Each is held against statelog.ApplyTxTimeBudget (250 ms),
// which cannot split a record; doc.go's "Measured" section records the
// figures.
func BenchmarkApplyAPurgePart(b *testing.B) {
	quiet(b)
	commands, introduced := widestPushRow(b)
	width := len(commands) + len(introduced)
	under := (PurgePartBytes - 1) / width
	// A CHANGE AT ITS WIDEST is four rows: its own, a revision, an author and
	// a body at MaxChangeBodyBytes.
	const perChange = 4
	bodied := func(e *estate, repo string, n int) { e.seedChangeRows(repo, n, 1, 1, MaxChangeBodyBytes) }
	// after is how many of those a part takes behind the widest rows short of
	// PurgePartBytes: the bodies that fill what the pushes left, the last
	// reaching the bound.
	after := (PurgePartBytes - under*width + MaxChangeBodyBytes - 1) / MaxChangeBodyBytes
	// mixed widest rows and beside changes fill both bounds: the changes the
	// rows leave room for, and the pushes their bodies leave room for.
	mixed := (PurgePartBytes - PurgePartRows/perChange*MaxChangeBodyBytes) / (width - MaxChangeBodyBytes/perChange)
	beside := (PurgePartRows - mixed) / perChange
	ruled := func(e *estate, repo string) {
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_protections (repo_id, pattern, direct_push)
			SELECT ?, printf('refs/heads/rule-%03d', i), 'persons' FROM k`,
			MaxProtectionsPerRepo, repo)
	}
	// A CHANGE THE WIDEST COMMENT'S AUDIENCE HEARD OF is a hundred rows: its
	// own, a revision, an author and heard notices.
	heard := MaxRoutedParticipants + MaxMentions + 1
	perNoticed := 3 + heard
	noticed := func(e *estate, repo string, n int) {
		e.seedChangeRows(repo, n, 1, 1, 0)
		e.seedNotices(repo, n, heard)
	}
	wholeNoticed := PurgePartRows / perNoticed
	for _, shape := range []struct {
		name  string
		seed  func(e *estate, repo string)
		final bool
		rows  int
		// pushes and changes are what the record left of the repository's
		// push rows and of the rows of its changes.
		pushes, changes int
	}{
		// NARROW: push rows of about a kilobyte, which the row bound stops at.
		{"narrow", func(e *estate, repo string) { e.seedNarrow(repo, PurgePartRows+500) },
			false, PurgePartRows, 500, 0},
		// THE ACTIVITY ON TOP: a part's pushes over every day the history keeps, each by its own pusher,
		// and a part taking what is left of it once no push is.
		{"pushers", func(e *estate, repo string) { e.seedSpread(repo, PurgePartRows+500, 182) },
			false, PurgePartRows + 182 + PurgePartRows, 500, 0},
		// LEFT: pusher days no push stands for, which a part finding no push takes.
		{"left", func(e *estate, repo string) {
			e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
				INSERT INTO code_pusher_days (repo_id, day, pusher_kind, pusher, pushes)
				SELECT ?, 0, 'person', printf('person-%06d', i), 1 FROM k`, PurgePartRows, repo)
		}, false, PurgePartRows, 0, 0},
		// WIDE: the widest push rows, which the byte bound stops at most one
		// row past PurgePartBytes.
		{"wide", func(e *estate, repo string) { e.seedWide(b, repo, 100) },
			false, (PurgePartBytes + width - 1) / width, 100 - (PurgePartBytes+width-1)/width, 0},
		// WIDE+REFS: the widest rows short of PurgePartBytes and the refs
		// PurgePartRows leaves, which neither bound cuts; final purges it.
		{"wide+refs", func(e *estate, repo string) {
			e.seedWide(b, repo, under)
			e.seed(repo, 0, PurgePartRows-under, 0)
		}, false, PurgePartRows, 0, 0},
		{"final", func(e *estate, repo string) {
			e.seedWide(b, repo, under)
			e.seed(repo, 0, PurgePartRows-under, 0)
			ruled(e, repo)
		}, true, PurgePartRows + MaxProtectionsPerRepo, 0, 0},
		// CHANGES at their widest, taken whole: the most changes one part's
		// walk counts and its four deletes take, 24 MiB of bodies.
		{"changes", func(e *estate, repo string) { bodied(e, repo, PurgePartRows/perChange) },
			false, PurgePartRows, 0, 0},
		// WIDE+CHANGES: the bodies share the pushes' bytes, so the part takes
		// only those filling what the pushes left; weighed by rows alone it
		// would take 732 and 24 MB more.
		{"wide+changes", func(e *estate, repo string) {
			e.seedWide(b, repo, under)
			bodied(e, repo, 1000)
		}, false, under + after*perChange, 0, (1000 - after) * perChange},
		// WIDE+BODIES: both bounds at once (mixed, beside); final+bodies purges it.
		{"wide+bodies", func(e *estate, repo string) {
			e.seedWide(b, repo, mixed)
			bodied(e, repo, beside)
		}, false, mixed + beside*perChange, 0, 0},
		{"final+bodies", func(e *estate, repo string) {
			e.seedWide(b, repo, mixed)
			bodied(e, repo, beside)
			ruled(e, repo)
		}, true, mixed + beside*perChange + MaxProtectionsPerRepo, 0, 0},
		// CUT: one change's revisions taken oldest first, leaving its author,
		// body and row.
		{"cut", func(e *estate, repo string) { e.seedChangeRows(repo, 1, PurgePartRows, 1, MaxChangeBodyBytes) },
			false, PurgePartRows, 0, 3},
		// NOTICES: changes the widest comment's audience heard of, taken whole,
		// each notice deleted and announced; cut+notices cuts one change's
		// oldest first and takes them off its count; final+notices is the most
		// notices the purge's unbounded statements meet.
		{"notices", func(e *estate, repo string) { noticed(e, repo, wholeNoticed+10) },
			false, wholeNoticed * perNoticed, 0, 10 * 3},
		{"cut+notices", func(e *estate, repo string) {
			e.seedChangeRows(repo, 1, 1, 1, 0)
			e.seedNotices(repo, 1, PurgePartRows)
		}, false, PurgePartRows, 0, 1},
		{"final+notices", func(e *estate, repo string) {
			noticed(e, repo, wholeNoticed-1)
			ruled(e, repo)
		}, true, (wholeNoticed-1)*perNoticed + MaxProtectionsPerRepo, 0, 0},
	} {
		b.Run(shape.name, func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier(nil)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				repo, name := fmt.Sprintf("repo-%06d", i), fmt.Sprintf("r%06d", i)
				e.apply(create(b, repo, name))
				shape.seed(e, repo)
				r := partOf(b, repo, 1)
				if shape.final {
					r = rec(b, RepoSubject(repo), OpPurge, Scope{NamePath(NameToken(name)), RepoPath(repo)},
						Purge{RepoID: repo, Name: name, Reason: "past retention"})
				}
				e.applyTimed(b, a, r, shape.rows)
				if left := e.num(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?`, repo); left != int64(shape.pushes) {
					b.Fatalf("the record left %d push rows, want %d", left, shape.pushes)
				}
				if left := e.changeRowsOf(repo); left != int64(shape.changes) {
					b.Fatalf("the record left %d rows of the changes, want %d", left, shape.changes)
				}
				// AND THE NOTICES ITS CHANGES STILL COUNT, no more and no
				// fewer: a cut takes off its change's count what it took.
				if left, counted := e.num(`SELECT COUNT(*) FROM code_notifications WHERE repo_id = ?`, repo),
					e.num(`SELECT COALESCE(SUM(notices), 0) FROM code_changes WHERE repo_id = ?`,
						repo); left != counted {
					b.Fatalf("the record left %d notices of changes that count %d", left, counted)
				}
			}
		})
	}
}

// BenchmarkApplyARetireSweep is a retire_sweep record APPLIED AND COMMITTED,
// the time it holds every data node's one write lane, into a repository of its
// own each iteration, its retired rows seeded outside the timer as one earlier
// record's: rows=8, a small compaction's few, whose cost is its commit's, and
// RetireSweepRows, the most one record removes. Each is held against
// statelog.ApplyTxTimeBudget (250 ms), which cannot split a record; doc.go's
// "Measured" section records the figures, and limits.go's RetentionLaneShare
// and RetentionPassBudget reason from them beside BenchmarkRetireSweepStep.
func BenchmarkApplyARetireSweep(b *testing.B) {
	quiet(b)
	old := applyClock.Add(-RetireGrace - time.Hour)
	for _, rows := range []int{8, RetireSweepRows} {
		b.Run(fmt.Sprint("rows=", rows), func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier(nil)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				repo := fmt.Sprintf("repo-%06d", i)
				e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
				// RETIRED BY THE CREATE'S OWN POSITION: any record below the
				// sweep's is one a sweep may name.
				retiredBy := e.next - 1
				e.seedRetired(repo, retiredBy, old, rows)
				// THE REPOSITORY'S OWN ROW beside the rows it removes.
				e.applyTimed(b, a, sweepOf(b, repo, retiredBy), rows+1)
				if left := e.num(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = ?`, repo); left != 0 {
					b.Fatalf("the sweep left %d of %s's %d retired rows", left, repo, rows)
				}
			}
		})
	}
}

// BenchmarkApplyALandRecord is a land record APPLIED AND COMMITTED, the time it
// holds every data node's one write lane: the queue's hot path. Each change it
// lands is merged by its enqueuer, each entry landed, its train's landed
// candidates removed, a landing row an entry, the green row moved, and a push
// an entry through the one apply every push takes, moving the target and
// deleting the source. Seeded outside the timer as one train (seedLanding):
// entries=1, a quiet target's, and MaxLandEntries, the most one record lands
// and the most refs one landing moves. Each is held against
// statelog.ApplyTxTimeBudget (250 ms), which cannot split a record; doc.go's
// "Measured" section records the figures, and MaxLandEntries reasons from them.
func BenchmarkApplyALandRecord(b *testing.B) {
	quiet(b)
	for _, n := range []int{1, MaxLandEntries} {
		b.Run(fmt.Sprint("entries=", n), func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier(nil)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				repo := fmt.Sprintf("repo-%06d", i)
				e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
				land := e.seedLanding(repo, n)
				// EACH ENTRY: its change merged, its entry landed, its landing
				// and its push row, at the least.
				e.applyTimed(b, a, rec(b, RepoSubject(repo), OpLand, Scope{RepoPath(repo)}, land), 4*n)
				if tip := e.str(`SELECT target FROM code_refs WHERE repo_id = ? AND ref = 'refs/heads/main'`,
					repo); tip != land.TipOID {
					b.Fatalf("main is at %s after the landing, want its tip %s", tip, land.TipOID)
				}
				if merged := e.num(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ? AND state = 'merged'`,
					repo); merged != int64(n) {
					b.Fatalf("the landing merged %d of its %d changes", merged, n)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Microseconds())/1000/float64(b.N), "ms/record")
		})
	}
}

// seedLanding writes what a land of n entries of repo lands over (n live
// changes into main, main at the base, the entries testing in one train of
// candidates of MaxQueueBatch) and answers that land: each entry onto the
// commit its candidate built, deleting its source.
func (e *estate) seedLanding(repo string, n int) Land {
	e.t.Helper()
	e.seedLiveChanges(repo, n)
	base := oid("base")
	e.exec(`INSERT INTO code_refs (repo_id, ref, target, position, at) VALUES (?, 'refs/heads/main', ?, 1, 0)`,
		repo, base)
	e.exec(`UPDATE code_repos SET refs = refs + 1, branches = branches + 1 WHERE id = ?`, repo)
	train := uuid.Must(uuid.NewV7()).String()
	land := Land{RepoID: repo, LandID: uuid.Must(uuid.NewV7()).String(), TargetRef: "refs/heads/main",
		BaseOID: base, Train: train, Node: "node-a", ReceivedAt: applyClock}
	from := base
	for i := range n {
		entry := LandEntry{EntryID: uuid.Must(uuid.NewV7()).String(),
			ChangeID: fmt.Sprintf("%s-change-%06d", repo, i), Commit: oid(fmt.Sprint("landed", i)),
			DeleteSource: true}
		ordinal := i/MaxQueueBatch + 1
		candidate := QueueCandidateID(train, ordinal)
		e.exec(`INSERT INTO code_queue (repo_id, entry_id, target_ref, change_id, seq, head_oid, method,
			delete_source, close_task, enqueued_by, enqueued_kind, enqueued_operator, state, train, candidate_id,
			commit_oid, enqueued_at) VALUES (?, ?, 'refs/heads/main', ?, ?, ?, 'merge', 1, 0, 'dev', 'agent', '',
			'testing', ?, ?, ?, 1)`, repo, entry.EntryID, entry.ChangeID, i+1, liveHead(i), train, candidate,
			entry.Commit)
		if i%MaxQueueBatch == MaxQueueBatch-1 || i == n-1 {
			e.exec(`INSERT INTO code_candidates (repo_id, candidate_id, target_ref, train, ordinal, base_oid,
				tip_oid, state, position, entries) VALUES (?, ?, 'refs/heads/main', ?, ?, ?, ?, 'testing', 1,
				'[]')`, repo, candidate, train, ordinal, from, entry.Commit)
			from = entry.Commit
		}
		land.Entries = append(land.Entries, entry)
	}
	land.TipOID = land.Entries[n-1].Commit
	return land
}

// BenchmarkApplyAQueueSweep is a queue_sweep record APPLIED AND COMMITTED, the
// time it holds every data node's one write lane, over a repository of its own
// each iteration holding QueueSweepRows ended entries past QueueHistory:
// narrow, landed entries weighing nothing, cut by the row bound; widest,
// conflict ejections naming MaxEjectedPaths paths within MaxConflictBytes, the
// widest row a sweep removes, cut by QueueSweepBytes at some 2 000 (by rows
// alone the step would remove all 3 999, about 64 MiB of overflow pages).
// Each is held against statelog.ApplyTxTimeBudget (250 ms), which cannot split
// a record, and the third of it PurgePartBytes is sized to; doc.go's
// "Measured" section records the figures, and QueueSweepBytes reasons from them.
func BenchmarkApplyAQueueSweep(b *testing.B) {
	quiet(b)
	paths := make([]string, MaxEjectedPaths)
	for i := range paths {
		paths[i] = fmt.Sprintf("%03d/", i) + strings.Repeat("p", 156)
	}
	if cost := ConflictsCost(paths); cost > MaxConflictBytes || cost < MaxConflictBytes-128 {
		b.Fatalf("the widest ejection's paths are %d bytes of JSON, want just under %d", cost, MaxConflictBytes)
	}
	widest, err := ejectedDetail(Ejection{Reason: EjectConflict, Paths: paths})
	if err != nil {
		b.Fatal(err)
	}
	old := applyClock.Add(-QueueHistory - time.Hour)
	for _, arm := range []struct {
		name, ejected string
		// swept is how many entries the step removes: every one, or those
		// reaching QueueSweepBytes, at most one past it.
		swept int
	}{
		{"narrow", "", QueueSweepRows},
		{"widest", widest, int((QueueSweepBytes + int64(len(widest)) - 1) / int64(len(widest)))},
	} {
		b.Run(arm.name, func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier(nil)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				repo := fmt.Sprintf("repo-%06d", i)
				e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
				e.seedEndedRows(repo, QueueSweepRows, old, arm.ejected)
				// THE REPOSITORY'S OWN ROW beside the rows it removes.
				e.applyTimed(b, a, rec(b, RepoSubject(repo), OpQueueSweep, Scope{RepoPath(repo)},
					QueueSweep{RepoID: repo, Through: applyClock.Add(-QueueHistory)}), arm.swept+1)
				if left := e.num(`SELECT COUNT(*) FROM code_queue WHERE repo_id = ?`, repo); left !=
					int64(QueueSweepRows-arm.swept) {
					b.Fatalf("the sweep left %d of %s's %d ended entries, want %d", left, repo, QueueSweepRows,
						QueueSweepRows-arm.swept)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Microseconds())/1000/float64(b.N), "ms/record")
		})
	}
}

// seedEndedRows writes n entries of repo that ended at at, each its
// ejection's JSON ejected — landed where it is empty — and weighed by it.
func (e *estate) seedEndedRows(repo string, n int, at time.Time, ejected string) {
	e.t.Helper()
	state, reason := "landed", ""
	if ejected != "" {
		state, reason = "ejected", string(EjectConflict)
	}
	ctx := context.Background()
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, e.db.Caps().MaxVariables,
			`INSERT INTO code_queue (repo_id, entry_id, target_ref, change_id, seq, head_oid, method,
				delete_source, close_task, enqueued_by, enqueued_kind, enqueued_operator, state,
				enqueued_at, ended_at, ended_position, end_reason, ejected, ejected_bytes) VALUES `,
			`(?, ?, 'refs/heads/main', ?, ?, ?, 'merge', 0, 0, 'dev', 'agent', '', ?, ?, ?, 2, ?, ?, ?)`, ``, n,
			func(i int) []any {
				id := fmt.Sprintf("%s-e-%06d", repo, i)
				return []any{repo, id, "change-" + id, i + 1, oid("B"), state, store.EncodeTime(at),
					store.EncodeTime(at), reason, ejected, len(ejected)}
			})
		return err
	}); err != nil {
		e.t.Fatalf("seed %s's ended entries: %v", repo, err)
	}
}

// widestCheck is the widest queued attempt and ending of it a writer forms for
// key: every field at its bound, of text JSON escapes to six bytes a character
// (as TestTheLargestCheckRecordsFitARecord builds them), so the row they join
// to is the widest check row.
func widestCheck(tb testing.TB, key CheckKey, at time.Time) (CheckQueued, CheckFinished) {
	tb.Helper()
	wide := func(n int) string { return strings.Repeat("&", n) }
	// A REPORT'S PATH AT ITS BOUND: sixteen components of the 255 bytes a
	// file name takes, under MaxTreePathBytes whole.
	report := func(lead string) string { return lead + strings.Repeat("/"+wide(254), 15) + "/x" }
	def := CheckDef{Name: key.Name, Run: wide(MaxCheckRunBytes),
		TimeoutSeconds: int(CheckTimeoutCeiling / time.Second),
		Report:         CheckReport{JUnit: report(wide(254)), SARIF: report("s" + wide(253))}}
	for i := range MaxCheckPaths {
		head := fmt.Sprintf("%02d/", i)
		def.Paths = append(def.Paths, head+wide(128-len(head)))
	}
	for i := range coneList.max {
		def.Checkout.Cone = append(def.Checkout.Cone, fmt.Sprintf("%02d", i)+wide(253))
	}
	for i := range MaxCheckEnv {
		def.Env = append(def.Env, fmt.Sprintf("V%02d_", i)+strings.Repeat("A", MaxCheckEnvNameBytes-4))
	}
	queued := CheckQueued{CheckKey: key, Purpose: PurposeTip, Reason: QueueDispatch,
		Def: &def, QueuedAt: at}
	var failures []CheckFailure
	for i := 0; ; i++ {
		f := CheckFailure{Path: "p", Line: i, TestID: "t", Message: wide(100)}
		encoded, err := json.Marshal(append(failures, f))
		if err != nil {
			tb.Fatal(err)
		}
		if len(failures) == MaxCheckFailures || len(encoded) > MaxCheckFailureBytes {
			break
		}
		failures = append(failures, f)
	}
	exit := 1
	ended := CheckFinished{CheckKey: key, State: CheckStateFailed, Reason: CheckReasonExit, ExitCode: &exit,
		DurationMs: 1 << 40, Summary: wide(CheckSummaryBytes), Tail: wide(CheckTailBytes), Failures: failures,
		FailuresOmitted: 1 << 30, Log: logOf(key.Name + "-log"),
		URL: "https://ci.example.com/" + wide(MaxCheckURLBytes-len("https://ci.example.com/")), FinishedAt: at}
	for _, p := range []interface{ Validate() error }{queued, ended} {
		if err := p.Validate(); err != nil {
			tb.Fatalf("the widest check record is not one a writer forms: %v", err)
		}
	}
	return queued, ended
}

// seedPastTheHorizon writes CheckHorizonRows+1 narrow failed check rows of a
// repository, each ended at at and with its log: one more than an ending's
// horizon removes.
func (e *estate) seedPastTheHorizon(repo string, at time.Time) {
	e.t.Helper()
	n := CheckHorizonRows + 1
	ended, overdue := store.EncodeTime(at), store.EncodeTime(at.Add(600*time.Second))
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose, change_id,
			revision, candidate_id, entry_id, base_oid, defs_oid, required, timeout_seconds,
			reason, runner_node, placement, box_handle, reporter, queued_at, started_at, finished_at,
			duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, overdue_at,
			url, summary, def, failures, tail)
		SELECT ?, printf('%040d', i), 'refs/heads/main', 'unit', 1, 'failed', 'tip', '', 0, '', '', '', '', 0,
			600, 'exit', 'node-a', 'container', 'box', '', ?, ?, ?, 1000, 1, 0, 1, 2, 76, ?, '',
			'1 test failed', '{"name":"unit","run":"make test","timeout_seconds":600}', '', '--- FAIL' FROM k`,
		n, repo, ended, ended, ended, overdue)
	e.exec(`INSERT INTO code_check_logs (repo_id, commit_oid, target_ref, name, attempt, object, hash, size)
		SELECT repo_id, commit_oid, target_ref, name, attempt, 'logs/' || commit_oid, 'sha256:' || commit_oid,
			4096
		FROM code_checks WHERE repo_id = ?`, repo)
}

// BenchmarkApplyACheckRecord is a record on a repository's checks subject
// APPLIED AND COMMITTED, the time it holds every data node's one write lane,
// into a repository of its own each iteration, set up outside the timer.
// Every data node applies four an attempt (queued, claimed, box named, ended),
// and every ending runs its repository's horizon. claim is a runner's
// check_started of the widest attempt, a join reading the row whole and
// writing it back; ending is the widest ending alone; ending+narrow removes
// CheckHorizonRows rows and logs, the most an ending touches; ending+wide
// removes widest rows to CheckHorizonBytes, about a mebibyte of overflow
// pages beside a check row's five secondary indexes. Each is held against
// statelog.ApplyTxTimeBudget (250 ms); doc.go's "Measured" section records the
// figures, and CheckHorizonRows' and CheckHorizonBytes' docs reason from them.
func BenchmarkApplyACheckRecord(b *testing.B) {
	quiet(b)
	old := applyClock.Add(-CheckHistory - time.Hour)
	// width is what a row of the widest attempt weighs, and through how many
	// of them a horizon reaches CheckHorizonBytes.
	probe := CheckKey{RepoID: checkRepo, OID: oid("probe"), Target: "refs/heads/main", Name: "probe", Attempt: 1}
	pq, pf := widestCheck(b, probe, applyClock)
	pe, err := finishedPart(pf, applyContext{})
	if err != nil {
		b.Fatal(err)
	}
	width := checkRow{queue: *queuedPart(pq, applyContext{}).queue, ending: *pe.ending}.rowBytes()
	through := int((CheckHorizonBytes + width - 1) / width)
	for _, arm := range []struct {
		name  string
		claim bool
		// past seeds the repository's rows past the history, and answers
		// how many the timed ending's horizon removes.
		past func(b *testing.B, e *estate, repo string) int
	}{
		{"claim", true, func(*testing.B, *estate, string) int { return 0 }},
		{"ending", false, func(*testing.B, *estate, string) int { return 0 }},
		{"ending+narrow", false, func(_ *testing.B, e *estate, repo string) int {
			e.seedPastTheHorizon(repo, old)
			return CheckHorizonRows
		}},
		{"ending+wide", false, func(b *testing.B, e *estate, repo string) int {
			subject, scope := ChecksSubject(repo), Scope{ChecksPath(repo)}
			for i := range through + 1 {
				key := CheckKey{RepoID: repo, OID: oid(fmt.Sprint("old", i)), Target: "refs/heads/main", Name: "wide", Attempt: 1}
				q, f := widestCheck(b, key, old)
				e.apply(rec(b, subject, OpCheckQueued, scope, q).at(old),
					rec(b, subject, OpCheckFinished, scope, f).at(old))
			}
			return through
		}},
	} {
		b.Run(arm.name, func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier(nil)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				repo := fmt.Sprintf("0190f3c4-0000-7000-8000-%012d", i)
				e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
				removed := arm.past(b, e, repo)
				held := e.num(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ?`, repo)
				key := CheckKey{RepoID: repo, OID: oid("checked"), Target: "refs/heads/main", Name: "unit", Attempt: 1}
				queued, ended := widestCheck(b, key, applyClock)
				subject, scope := ChecksSubject(repo), Scope{ChecksPath(repo)}
				e.apply(rec(b, subject, OpCheckQueued, scope, queued))
				claim := rec(b, subject, OpCheckStarted, scope, CheckStarted{CheckKey: key, Runner: "node-a",
					Placement: PlacementContainer, StartedAt: applyClock})
				if arm.claim {
					e.applyTimed(b, a, claim, 1)
					continue
				}
				e.apply(claim)
				// ITS ROW AND LOG, AND EACH ROW AND LOG ITS HORIZON REMOVES.
				e.applyTimed(b, a, rec(b, subject, OpCheckFinished, scope, ended), 2+2*removed)
				if left := e.num(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ?`, repo); left != held+1-int64(removed) {
					b.Fatalf("the ending left %d of the repository's %d rows and its own, want its horizon to "+
						"remove %d", left, held, removed)
				}
			}
		})
	}
}

// BenchmarkApplyACompaction is a compaction record APPLIED AND COMMITTED, the
// time it holds every data node's one write lane, into a repository of its own
// each iteration holding MaxPacksPerRepo live packs, seeded outside the timer
// and held to what a committer forms ([estate.admitted]). inputs=N writes as
// many outputs as N inputs may, the most rows N can touch; a publication
// replaces every layer of a full chain but the root (widestPublication) and
// also reads the chain whole and recounts the live packs, so
// publication/inputs=1024, not inputs=1024, is the op's widest; expunge/begin
// (widestBegin) is the code domain's widest record; expunge/commit is
// widestCommit. The 1024 arms and expunge/begin are held under a third of
// statelog.ApplyTxTimeBudget, which sizes MaxCompactInputs, a publication's
// repack and MaxExpungeInputs (their docs, and doc.go's "Measured" section).
func BenchmarkApplyACompaction(b *testing.B) {
	quiet(b)
	for _, arm := range []struct {
		name        string
		inputs      int
		publication bool
	}{
		{"inputs=8", 8, false},
		{"inputs=64", 64, false},
		{fmt.Sprint("inputs=", MaxCompactInputs), MaxCompactInputs, false},
		{"publication/inputs=0", 0, true},
		{fmt.Sprint("publication/inputs=", MaxCompactInputs), MaxCompactInputs, true},
	} {
		outputs := min(arm.inputs, MaxCompactOutputs)
		b.Run(arm.name, func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier(nil)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				// A UUID, as the committer takes a repository's id.
				repo := fmt.Sprintf("0190f3c4-5b6a-7c8d-9e0f-%012d", i)
				e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
				var c Compaction
				if arm.publication {
					c = e.widestPublication(b, repo, e.seedChain(b, repo), arm.inputs)
				} else {
					e.seed(repo, 0, 0, MaxPacksPerRepo)
					retire := make([]string, arm.inputs)
					for k := range retire {
						retire[k] = idOf(repo, k)
					}
					cid := statelog.NewOpID(applyClock, "")
					add := make([]PackRef, outputs)
					for k := range add {
						add[k] = outputOf(cid, k, 1, applyClock)
					}
					c = Compaction{RepoID: repo, CompactionID: cid, Node: "node-a", Seen: codeAt(e.next - 1),
						Retire: retire, Add: add}
				}
				e.admitted(b, c)
				e.applyTimed(b, a, recordOf(b, c), compactDecidedRows(len(c.Retire), len(c.Add), c.Layer))
				if left := e.num(`SELECT live_packs FROM code_repos WHERE id = ?`, repo); left !=
					int64(MaxPacksPerRepo-arm.inputs+outputs) {
					b.Fatalf("the compaction left %d live packs, want %d", left, MaxPacksPerRepo-arm.inputs+outputs)
				}
				if !arm.publication {
					continue
				}
				// THE CHAIN IT LEAVES: the root and the layer, which covers
				// every live pack the root does not.
				if chain := e.chainOf(repo); len(chain) != 2 || chain[1] != c.Layer.LayerID {
					b.Fatalf("the publication left the chain %v, want the root and %s", chain, c.Layer.LayerID)
				}
				if tail := e.num(`SELECT live_packs - published_packs FROM code_repos WHERE id = ?`, repo); tail != 0 {
					b.Fatalf("the publication left %d live packs no layer covers, want none", tail)
				}
			}
		})
	}
	b.Run("expunge/begin", func(b *testing.B) {
		e := openApplyEstate(b)
		a := NewApplier(nil)
		b.ReportAllocs()
		b.ResetTimer()
		b.StopTimer()
		for i := range b.N {
			repo := fmt.Sprintf("0190f3c4-5b6a-7c8d-9e0f-%012d", i)
			e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
			begin := e.widestBegin(b, repo)
			e.expungeAdmitted(b, ExpungeProposal{Begin: &begin})
			e.applyTimed(b, a, expungeRec(b, ExpungeProposal{Begin: &begin}), expungeBeginRows(len(begin.Retire),
				len(begin.Add)+len(begin.Dropping), len(begin.DropLayers)))
			if live, want := e.num(`SELECT live_packs FROM code_repos WHERE id = ?`, repo),
				int64(MaxPacksPerRepo-len(begin.Retire)+len(begin.Add)); live != want {
				b.Fatalf("the begin left %d live packs, want %d", live, want)
			}
			if chain := e.chainOf(repo); len(chain) != 0 {
				b.Fatalf("the begin left the chain %v, want none: its inputs are under the root", chain)
			}
		}
	})
	b.Run("expunge/commit", func(b *testing.B) {
		e := openApplyEstate(b)
		a := NewApplier(nil)
		b.ReportAllocs()
		b.ResetTimer()
		b.StopTimer()
		for i := range b.N {
			repo := fmt.Sprintf("0190f3c4-5b6a-7c8d-9e0f-%012d", i)
			e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
			commit := e.widestCommit(b, repo)
			e.expungeAdmitted(b, ExpungeProposal{Commit: &commit})
			e.applyTimed(b, a, expungeRec(b, ExpungeProposal{Commit: &commit}),
				expungeCommitRows(len(commit.Retire), len(commit.Add)))
			if dropping := e.num(`SELECT COUNT(*) FROM code_packs WHERE repo_id = ? AND state = 'dropping'`,
				repo); dropping != 0 {
				b.Fatalf("the commit left %d packs dropping, want none", dropping)
			}
			if id, _ := e.openRequest(repo); id != commit.RequestID {
				b.Fatalf("the commit served request %s with packs from before it live (open %q)",
					commit.RequestID, id)
			}
		}
	})
}

// ones is n packs' objects, one each: the outputs of the widest expunge
// records, each holding an object of the inputs they replace.
func ones(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = 1
	}
	return out
}

// widestBegin seeds repository id's chain as the publication arms do
// (seedChain) and answers an expunge's begin at every bound at once:
// MaxExpungeInputs inputs, the first packs, which the root names, so it drops
// every layer; MaxExpungeOutputs outputs, half kept and half left dropping:
// expungeBeginRows' 2 625 rows.
func (e *estate) widestBegin(b *testing.B, id string) ExpungeBegin {
	b.Helper()
	chain := e.seedChain(b, id)
	retire := make([]string, MaxExpungeInputs)
	for k := range retire {
		retire[k] = idOf(id, k)
	}
	layers := make([]string, len(chain))
	for k, l := range chain {
		layers[k] = l.LayerID
	}
	begin := beginOver(id, 1, retire, ones(MaxExpungeOutputs/2), ones(MaxExpungeOutputs-MaxExpungeOutputs/2),
		layers...)
	begin.Seen = codeAt(e.next - 1)
	return begin
}

// widestCommit seeds repository id with MaxPacksPerRepo live packs and applies
// a purge request's begin of MaxExpungeInputs inputs leaving MaxExpungeOutputs
// dropping, and answers the commit at every bound: it retires them all into as
// many rescue packs (expungeCommitRows' 97 rows) and judges the open request
// with one seek of the packs' content index.
func (e *estate) widestCommit(b *testing.B, id string) ExpungeCommit {
	b.Helper()
	e.seed(id, 0, 0, MaxPacksPerRepo)
	request := requestOf(id, statelog.NewOpID(applyClock, "code_purge_unreachable"))
	e.apply(expungeRec(b, ExpungeProposal{Request: &request}))
	retire := make([]string, MaxExpungeInputs)
	for k := range retire {
		retire[k] = idOf(id, k)
	}
	begin := beginOver(id, 1, retire, nil, ones(MaxExpungeOutputs))
	begin.RequestID, begin.Grace, begin.Seen = request.RequestID, 0, codeAt(e.next-1)
	e.expungeAdmitted(b, ExpungeProposal{Begin: &begin})
	e.apply(expungeRec(b, ExpungeProposal{Begin: &begin}))
	commit := commitOver(begin, ones(MaxExpungeOutputs)...)
	commit.Seen = codeAt(e.next - 1)
	return commit
}

// expungeAdmitted fails b unless p is an expunge record the committer takes
// and whose decide over e's rows forms its record — [estate.admitted]'s rule
// for an expunge's.
func (e *estate) expungeAdmitted(b *testing.B, p ExpungeProposal) {
	b.Helper()
	if err := p.Validate(); err != nil {
		b.Fatalf("the committer refuses the %s's proposal: %v", opOf(p), err)
	}
	switch d, err := expungeDecidedIn(b, e, p, applyClock); {
	case err != nil:
		b.Fatalf("decide the %s: %v", opOf(p), err)
	case d.refusal != "":
		b.Fatalf("the %s is refused %s: %s", opOf(p), d.refusal, d.detail)
	}
}

// admitted fails b unless c is a compaction the committer takes and whose
// decide over e's rows forms its record, so a benchmark never applies a record
// no build forms.
func (e *estate) admitted(b *testing.B, c Compaction) {
	b.Helper()
	if err := (CompactionProposal{Compaction: c}).Validate(); err != nil {
		b.Fatalf("the committer refuses the compaction's proposal: %v", err)
	}
	switch d, err := compactionDecidedIn(b, e, c, applyClock); {
	case err != nil:
		b.Fatalf("decide the compaction: %v", err)
	case d.refusal != "":
		b.Fatalf("the compaction is refused %s: %s", d.refusal, d.detail)
	}
}

// widestLayer is a layer named for name over packs (git checksums) on base,
// replacing replaces, its bytes cut into MaxLayerParts parts — the most rows
// one layer writes, and deletes once it is replaced.
func widestLayer(name, base string, replaces []string, packs ...string) LayerRef {
	l := layerOver(name, base, replaces, applyClock, packs...)
	l.Parts = make([]LayerPart, MaxLayerParts)
	for i := range l.Parts {
		l.Parts[i] = LayerPart{Object: objstore.KeyAt(applyClock),
			Hash: objstore.HashOf([]byte(fmt.Sprint(name, i))), Size: LayerPartBytes}
	}
	l.MidxBytes, l.BitmapBytes = MaxLayerParts*LayerPartBytes-1<<20, 1<<20
	return l
}

// seedChain publishes MaxPublishedLayers layers (widestLayer) on repository
// id as its packs arrive, each over MaxPacksPerRepo/MaxPublishedLayers packs,
// decided and applied, so the chain names every live pack and is one its
// committer formed. It answers the layers, root first.
func (e *estate) seedChain(b *testing.B, id string) []LayerRef {
	b.Helper()
	per := MaxPacksPerRepo / MaxPublishedLayers
	chain := make([]LayerRef, 0, MaxPublishedLayers)
	base := ""
	for k := range MaxPublishedLayers {
		e.seedPacks(id, k*per, per)
		sums := make([]string, per)
		for n := range sums {
			sums[n] = sumOf(id, k*per+n)
		}
		c := publication(id, e.next-1, widestLayer(fmt.Sprint(id, "/", k), base, nil, sums...), nil)
		e.admitted(b, c)
		e.apply(recordOf(b, c))
		chain = append(chain, *c.Layer)
		base = c.Layer.LayerID
	}
	return chain
}

// widestPublication replaces every layer of chain but its root with one layer
// of MaxLayerParts parts, beside a repack of inputs of the first packs the
// replaced layers cover into as many outputs as inputs may write; the layer
// covers the outputs and what the replaced layers did but those inputs.
func (e *estate) widestPublication(b *testing.B, id string, chain []LayerRef, inputs int) Compaction {
	b.Helper()
	per := MaxPacksPerRepo / MaxPublishedLayers
	retire := make([]string, inputs)
	for k := range retire {
		retire[k] = idOf(id, per+k)
	}
	cid := statelog.NewOpID(applyClock, "")
	add := make([]PackRef, min(inputs, MaxCompactOutputs))
	sums := make([]string, 0, MaxPacksPerRepo)
	for k := range add {
		add[k] = outputOf(cid, k, 1, applyClock)
		sums = append(sums, add[k].GitChecksum)
	}
	for n := per + inputs; n < MaxPacksPerRepo; n++ {
		sums = append(sums, sumOf(id, n))
	}
	replaces := make([]string, 0, len(chain)-1)
	for _, l := range chain[1:] {
		replaces = append(replaces, l.LayerID)
	}
	return publication(id, e.next-1, widestLayer(id+"/top", chain[0].LayerID, replaces, sums...), retire, add...)
}

// BenchmarkPushHistoryBytes is what ONE PUSH LEAVES ON EVERY DATA NODE'S DISK,
// per push over records each landing one: the whole file's growth (bytes/row)
// and, since they live differently, each of the push's two rows' by its own
// table's pages ([pageTally]): code_pushes-bytes/row, the push row and its
// index entries, kept usage.History (181 days), and code_packs-bytes/row,
// its pack's, kept while the pack is live. Never the file's growth with a
// pack less without one: that counts the push row's pack columns as the pack
// row's and moves with how many rows fit a page. A data node's push history
// costs the first times pushes a day times 181, plus the second times its
// live packs (about CompactTargetPacks a compacted repository, one more per
// FullPackBytes of history, never past MaxPacksPerRepo), plus the retired
// arm's. doc.go's "Measured" section records the arms.
func BenchmarkPushHistoryBytes(b *testing.B) {
	quiet(b)
	const typicalRef = "refs/heads/feature/push-history-measured"
	if len(typicalRef) != 40 {
		b.Fatalf("the typical ref is %d bytes, not 40", len(typicalRef))
	}
	widestRef := func(c int) string {
		ref := fmt.Sprintf("refs/heads/%08d", c)
		return ref + strings.Repeat("w", MaxRefNameBytes-len(ref))
	}
	// EVERY ID IS PRODUCTION'S SHAPE, IN ITS ORDER: an index appends its
	// entries at its end only while they arrive in order, so short ids or
	// unordered keys would price every row lighter than a data node's. A
	// repository, push or compaction id is a UUIDv7 minted in turn, a pack
	// takes its push's id and an object key minted at its put
	// (objstore.KeyAt), and a person's credential is a token's name (`pat:`
	// and its id) beside the human seat they hold.
	person := Principal{Kind: PrincipalOperator, Seat: "ada",
		OperatorID: iam.MachineTokenName(uuid.NewSHA1(uuid.NameSpaceOID, []byte("ada's token")).String())}
	// pushed is one person's push of cmds, ids and key minted as production's.
	pushed := func(cmds ...Command) PushEntry {
		entry := pushEntry(uuid.Must(uuid.NewV7()).String(), cmds...)
		entry.Principal, entry.Pack.Object = person, objstore.KeyAt(time.Now())
		return entry
	}
	// seatRun is the run the engine stamps on the r-th push of a seat, at each
	// field's usual size.
	seatRun := func(r int) RunProvenance {
		return RunProvenance{
			TurnID:      uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprint("turn-", r))).String(),
			WorkBackend: "native", WorkKey: "ENG-1234", Model: "claude-sonnet-4-5",
		}
	}
	for _, shape := range []struct {
		name string
		// records is how many pushes are measured: enough that the file
		// grows by thousands of pages, so a page's granularity is lost in
		// the figure.
		records   int
		ref       func(c int) string
		cmds, ids int
		// by stamps who made the r-th push, and how, on its entry: nil for
		// a person's, through git.
		by func(e *PushEntry, r int)
	}{
		// TYPICAL: a person's push through git of one commit on a 40-byte ref.
		{"typical", 4_000, func(int) string { return typicalRef }, 1, 1, nil},
		// AGENT: the same by a seat, built by the engine: what a seat's
		// provenance costs over typical.
		{"agent", 4_000, func(int) string { return typicalRef }, 1, 1, func(e *PushEntry, r int) {
			e.Principal = Principal{Kind: PrincipalAgent, Seat: "docs-writer", Run: seatRun(r)}
			e.Via = ViaEngine
		}},
		// BOX: the same by the seat's coding box through git: what its launch
		// and harness fields cost over agent.
		{"box", 4_000, func(int) string { return typicalRef }, 1, 1, func(e *PushEntry, r int) {
			run := seatRun(r)
			run.RunID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprint("launch-", r))).String()
			run.Harness = "claude-code"
			e.Principal = Principal{Kind: PrincipalAgent, Seat: "docs-writer", Run: run}
			e.Via = ViaGit
		}},
		{"widest", 100, widestRef, MaxCommandsPerPush, MaxIntroduced, nil},
	} {
		// pack=no carries no objects (a deletion, or a ref moved to a commit
		// the repository holds): its row names no pack and leaves no pack row.
		for _, pack := range []string{"no", "yes"} {
			b.Run("push="+shape.name+"/pack="+pack, func(b *testing.B) {
				var perRow, pushRow, packRow float64
				for b.Loop() {
					b.StopTimer()
					e := openApplyEstate(b)
					repo := uuid.Must(uuid.NewV7()).String()
					e.apply(create(b, repo, "history"))
					// THE REFS EXIST BEFORE THE FILE IS MEASURED, so a ref's row
					// is rewritten in place, never counted: it lives as long as
					// it is kept, not as long as the history. Any other table
					// growing past [inPlaceShare] of the push rows fails the
					// benchmark, as a third row nothing here prices.
					cmds := make([]Command, shape.cmds)
					for c := range cmds {
						cmds[c] = move(shape.ref(c), ZeroOID, oid(fmt.Sprint("tip-0-", c)))
					}
					e.apply(pushOf(b, repo, pushed(cmds...)))
					before := e.tally(b)
					b.StartTimer()
					for r := 1; r <= shape.records; r++ {
						for c := range cmds {
							cmds[c] = move(cmds[c].Ref, cmds[c].New, oid(fmt.Sprint("tip-", r, "-", c)))
						}
						entry := pushed(cmds...)
						entry.Introduced = make([]string, shape.ids)
						for i := range entry.Introduced {
							entry.Introduced[i] = oid(fmt.Sprint("introduced-", r, "-", i))
						}
						entry.IntroducedCount = shape.ids
						if pack == "no" {
							entry.Pack = nil
						}
						if shape.by != nil {
							shape.by(&entry, r)
						}
						e.apply(pushOf(b, repo, entry))
					}
					b.StopTimer()
					if got := e.num(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?`, repo); got != int64(shape.records+1) {
						b.Fatalf("the estate holds %d push rows, want %d", got, shape.records+1)
					}
					after := e.tally(b)
					per := func(pages int64) float64 {
						return float64(pages*after.pageBytes) / float64(shape.records)
					}
					perRow = per(after.pages - before.pages)
					pushRow = per(after.tables["code_pushes"] - before.tables["code_pushes"])
					packRow = per(after.tables["code_packs"] - before.tables["code_packs"])
					var elsewhere int64
					var grown []string
					for table, pages := range after.tables {
						if grew := pages - before.tables[table]; grew > 0 && table != "code_pushes" &&
							table != "code_packs" {
							elsewhere += grew
							grown = append(grown, fmt.Sprintf("%s by %d", table, grew))
						}
					}
					pushes := after.tables["code_pushes"] - before.tables["code_pushes"]
					if elsewhere*inPlaceShare > pushes {
						slices.Sort(grown)
						b.Fatalf("over %d pushes the push rows grew by %d pages and the tables a push rewrites "+
							"in place by %d (%s), past a %dth of them: a row each push leaves beside its two, "+
							"whose lifetime nothing here prices — measure it with theirs", shape.records, pushes,
							elsewhere, strings.Join(grown, ", "), inPlaceShare)
					}
					b.StartTimer()
				}
				b.ReportMetric(perRow, "bytes/row")
				b.ReportMetric(pushRow, "code_pushes-bytes/row")
				b.ReportMetric(packRow, "code_packs-bytes/row")
			})
		}
	}
	// RETIRED: typical pushes merged CompactMinMerge at a time by a compaction
	// a committer forms, so the figure is a retired pack's row and its three
	// index entries, kept RetireGrace until the code-retention duty's
	// retire_sweep. A data node holds it times the packs retired a day (a
	// push's, and the outputs merged again: internal/gitlet/compact's steady
	// arm's retired/push) times RetireGrace and RetentionInterval over a day.
	b.Run(fmt.Sprintf("retired/inputs=%d", CompactMinMerge), func(b *testing.B) {
		// retiredRows is how many packs the arm's compactions retire: enough
		// that code_packs_retired grows by hundreds of pages, so a page's
		// granularity is lost in the figure.
		const retiredRows = 4_096
		var retiredRow float64
		for b.Loop() {
			b.StopTimer()
			e := openApplyEstate(b)
			repo := uuid.Must(uuid.NewV7()).String()
			e.apply(create(b, repo, "history"))
			tip := oid("tip-0")
			e.apply(pushOf(b, repo, pushed(move(typicalRef, ZeroOID, tip))))
			before := e.tally(b)
			b.StartTimer()
			for r := range retiredRows / CompactMinMerge {
				retire := make([]string, 0, CompactMinMerge)
				for k := range CompactMinMerge {
					next := oid(fmt.Sprint("tip-", r, "-", k))
					entry := pushed(move(typicalRef, tip, next))
					e.apply(pushOf(b, repo, entry))
					retire, tip = append(retire, entry.Pack.PackID), next
				}
				slices.Sort(retire)
				// AS ITS HOME BUILDS ONE: the id minted when built, the
				// output's key at its put, each after the last.
				cid := statelog.NewOpID(time.Now(), "")
				c := Compaction{RepoID: repo, CompactionID: cid, Node: "node-a", Seen: codeAt(e.next - 1),
					Retire: retire, Add: []PackRef{outputOf(cid, 0, int64(len(retire)), time.Now())}}
				if err := (CompactionProposal{Compaction: c}).Validate(); err != nil {
					b.Fatalf("the committer refuses the compaction's proposal: %v", err)
				}
				switch d, err := compactionDecidedIn(b, e, c, time.Now()); {
				case err != nil:
					b.Fatalf("decide the compaction: %v", err)
				case d.refusal != "":
					b.Fatalf("the compaction is refused %s: %s", d.refusal, d.detail)
				}
				e.apply(recordOf(b, c))
			}
			b.StopTimer()
			if got := e.num(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = ?`, repo); got != retiredRows {
				b.Fatalf("the estate holds %d retired pack rows, want %d", got, retiredRows)
			}
			after := e.tally(b)
			retired := after.tables["code_packs_retired"] - before.tables["code_packs_retired"]
			retiredRow = float64(retired*after.pageBytes) / retiredRows
			// THE RETIREMENT LEAVES ONE ROW: beside code_packs and the arm's
			// own pushes, every other table a record touches is rewritten in
			// place.
			var elsewhere int64
			var grown []string
			for table, pages := range after.tables {
				if grew := pages - before.tables[table]; grew > 0 && table != "code_packs_retired" &&
					table != "code_packs" && table != "code_pushes" {
					elsewhere += grew
					grown = append(grown, fmt.Sprintf("%s by %d", table, grew))
				}
			}
			if elsewhere*inPlaceShare > retired {
				slices.Sort(grown)
				b.Fatalf("over %d retirements the retired rows grew by %d pages and the tables a "+
					"compaction rewrites in place by %d (%s), past a %dth of them: a row each retirement "+
					"leaves beside its own, whose lifetime nothing here prices — measure it with theirs",
					retiredRows, retired, elsewhere, strings.Join(grown, ", "), inPlaceShare)
			}
			b.StartTimer()
		}
		b.ReportMetric(retiredRow, "code_packs_retired-bytes/row")
	})
}

// inPlaceShare is how small, against the push rows' growth over a history
// benchmark, the growth of every table a push only rewrites in place (its
// refs, its repository's counters) must stay: a HUNDREDTH. Such a table grows
// only as a rewrite splits a page or a row's numbers widen (code_refs by four
// pages over the widest arm, against its push rows' 10 256, and nothing else,
// measured with the bound at zero); a third row each push left would be a few
// percent of a push row at least (a 30-byte counter row is five), and under a
// hundredth no forecast figure moves past its rounding.
const inPlaceShare = 100

// BenchmarkApplyAViewedMark prices a viewed record's apply on the shared write lane, in three arms:
// one file marked by a person holding 100 marks; MaxViewedPerMark files marked by one at the cap, so
// as many of their oldest go; and a record forgetting a review of MaxViewedFiles files whose change
// ended.
func BenchmarkApplyAViewedMark(b *testing.B) {
	quiet(b)
	key := ViewedKey(strings.Repeat("ab", ViewedKeyBytes))
	marks := func(prefix string, n int) []ViewedMark {
		out := make([]ViewedMark, n)
		for i := range out {
			path := fmt.Sprintf("%s/%04d.go", prefix, i)
			out[i] = ViewedMark{PathDigest: key.PathDigest(path), FileDigest: key.FileDigest(path, oid(path), "100644")}
		}
		return out
	}
	const ended, live = "0192f6c1-0000-7000-8000-000000000001", "0192f6c1-0000-7000-8000-000000000002"
	for _, arm := range []struct {
		name   string
		held   int
		record ViewedMarks
		// rows is what the apply writes at least: the marks, and the rows it drops or forgets.
		rows int
	}{
		{"one of 100", 100, ViewedMarks{ChangeID: live, Viewed: marks("new", 1)}, 1},
		{"32 at the cap", MaxViewedFiles, ViewedMarks{ChangeID: live, Viewed: marks("new", MaxViewedPerMark)},
			2 * MaxViewedPerMark},
		{"forgetting 2000", MaxViewedFiles, ViewedMarks{ChangeID: live, Viewed: marks("new", 1),
			Forget: []string{ended}}, MaxViewedFiles + 1},
	} {
		b.Run(arm.name, func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier(nil)
			held := marks("old", arm.held)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				handle := fmt.Sprintf("p%06d", i)
				ctx := context.Background()
				if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
					_, err := store.InsertRows(ctx, tx, e.db.Caps().MaxVariables,
						`INSERT INTO code_viewed_files (handle, change_id, path_digest, file_digest, at, position) VALUES `,
						`(?, ?, ?, ?, 0, ?)`, ``, len(held), func(j int) []any {
							return []any{handle, ended, held[j].PathDigest, held[j].FileDigest, j + 1}
						})
					return err
				}); err != nil {
					b.Fatalf("seed %s's marks: %v", handle, err)
				}
				e.applyTimed(b, a, viewedOn(b, handle, arm.record), arm.rows)
				if n := e.num(`SELECT COUNT(*) FROM code_viewed_files WHERE handle = ?`, handle); n > MaxViewedFiles {
					b.Fatalf("%s holds %d marks after the record, past the cap", handle, n)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Microseconds())/1000/float64(b.N), "ms/record")
		})
	}
}
