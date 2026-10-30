package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/store"
)

// applyPush applies a batch of pushes to one repository: each entry IN ORDER,
// a later entry seeing an earlier one's refs, inside the repository's
// horizons — its expired push history removed before the refs are written,
// its due tombstones after, and its ref drops past their grace last.
//
// # Two reads for the whole record, never one per entry or per command
//
// The apply transaction holds the replicated estate's write lock — each log
// has its own pinned writer, but they are writers of ONE file, so every other
// domain's applier on the node waits behind this transaction — and a record
// the committer fills carries nearly four thousand commands. So the push ids
// the record's entries carry are read in ONE statement over their primary key,
// the refs their ok commands name in ONE statement over the repository's
// primary-key range, and the ref state is carried across the entries IN
// MEMORY: the last value of a ref is the whole truth, since every row a record
// writes carries the record's one position and instant. (The repository's
// tombstone count is one more read, of one row by its key.)
//
// # The tombstones are bounded exactly, and refuse nobody
//
// A repository holds at most MaxTombstonesPerRepo tombstones after EVERY
// record, by two moves, each a pure function of the record and the rows. The
// sweep ([Applier.sweepTombstones]) takes the oldest past the bound as well
// as those past their horizon, within what the record's row budget leaves it:
// 3 995 rows behind a one-command push, and behind any record the committer
// forms at least one for every deletion the record carries, since the
// committer keeps a row in a record's bound for the old tombstone each
// deletion's new one may need swept ([commandRows]) — and none at all behind
// a record it filled to the row bound with creations,
// which makes no tombstone. A record that would leave the count further past
// the bound than its sweep can take — a record no committer of this build
// forms, a newer build's or one formed by hand — writes its deletions as
// REMOVALS instead: each deleted ref's row is deleted rather than emptied,
// which is the one row its tombstone would have cost, so the budget and the
// counters stay exact, and the count after it is at most the count before.
// Its mark rises to its own position, since a cache synced below it would
// otherwise be handed a delta missing those deletions; such a cache — every
// one on every git node, since none has synced the record yet — reads the
// repository whole, which is why the committer leaves no record of its own
// needing it. Decided BEFORE a ref is written, from the counter and the
// record — never from a count a write already moved.
//
// # The counters are the rows, kept as the rows move
//
// code_repos counts what the repository's refs hold — live refs, the branches
// and tags among them, and tombstones — so no read counts them by walking a
// namespace: a hundred thousand refs is tens of milliseconds a COUNT, on every
// detail read, on whichever node answers. Each counter moves by the difference
// between every named ref's state before the record and after it, which is
// exact however many entries moved the ref in between.
//
// # What a forced update or a deletion left is recorded
//
// Every command the record applies that moves its ref OFF an object — a
// forced update or a deletion ([leavesObject]) — writes a code_ref_drops row:
// the ref, the object it held (Old) and the record's position and broker
// instant. No ref names that object any more, and the drops are how anything
// dropping the repository's unreachable objects tells what such a push left,
// which it keeps for [PruneGrace], from what nothing has reached for longer:
// the refs no longer say, and the push history would be 181 days of rows to
// read for it. A fast-forward leaves nothing — its old object is behind its
// new one — and neither does a create. ONE RECORD MAY LEAVE SEVERAL OBJECTS ON
// ONE REF, when its entries move the ref in turn, each seeing the one before:
// every one is a row, keyed by the object as well, and an object one record
// leaves twice is written once. The record's expiry removes its repository's
// drops past the grace LAST, with whatever its budget still holds once the
// tombstones have had theirs ([Applier.expireRefDrops]).
//
// THE PUSH RECORD'S FORMAT DOES NOT MOVE, under the code domain's stated
// condition: no build carrying the code domain has run without these rows —
// none of the domain is on main, so no image and no release holds it — and
// no record carries anything new. Once a build without them has run, a push
// record whose apply writes one is a record such a build would apply short
// of its rows, and the push op takes two versionedFields rows, {Name:
// "Command.Forced", Since: 2, Op: "push", Path: []string{"mutation",
// "pushes", "commands", "forced"}} and {Name: "Command.New deleting", Since:
// 2, Op: "push", Path: []string{"mutation", "pushes", "commands", "new"},
// Equals: ZeroOID}, with RecordVersion at 2 — so the older build retains
// such a record rather than apply it without its drops.
//
// # An applied push never applies twice
//
// A push id already in code_pushes — a redelivery, a failover's second record,
// or a repeat inside this record, which no decide forms — is skipped rather
// than allowed to abort the insert on its primary key, which would stall the
// log on every node.
//
// # The changes its pushes move are its own rows
//
// A push that moves a live change's source branch writes the change's next
// revision and makes its pusher an author; one that sets a change's target to
// exactly its head merges it; one that deletes its source closes it; and one
// whose entry carries the change its decide opened from the push's options
// opens it, numbered the repository's next — each in this record, from the
// change rows it finds, in its entries' order ([applyChangeMoves],
// revisions.go). Their rows are the record's own, written
// before any horizon and counted in its rows: the decide admitted each entry
// only while they fit with its own, counted by the same model this writes them
// from ([changeBook]).
//
// # A seat's pushes are counted on its company day
//
// Every seat's entry the record applies is counted on the day its decide
// stamped on it, in code_seat_days, the counter the decide holds a seat's
// pushes_per_seat_day to — a row a seat and day, two statements a record, and
// the rows they touch counted in the record's before any horizon takes what
// is left ([countSeatDays]).
func (a *Applier) applyPush(ctx context.Context, tx *sql.Tx, at applyContext, p PushBatch) (int, error) {
	ids := make([]string, 0, len(p.Pushes))
	for _, e := range p.Pushes {
		ids = append(ids, e.PushID)
	}
	applied, err := keySet(ctx, tx, pushesAppliedSQL, ids)
	if err != nil {
		return 0, fmt.Errorf("code: read the applied pushes at %s: %w", at.position, err)
	}
	var named []string
	seen := map[string]bool{}
	for _, e := range p.Pushes {
		if applied[e.PushID] {
			continue
		}
		for _, c := range e.Commands {
			if c.Result == ResultOK && !seen[c.Ref] {
				seen[c.Ref] = true
				named = append(named, c.Ref)
			}
		}
	}
	prior, err := refsNamed(ctx, tx, p.RepoID, named)
	if err != nil {
		return 0, fmt.Errorf("code: read the refs the push at %s moves: %w", at.position, err)
	}

	var (
		final       = map[string]string{}
		pushRows    []PushEntry
		packRows    []PackRef
		drops       []refDrop
		dropped     = map[refDrop]bool{}
		packBytes   int64
		packObjects int64
	)
	for _, e := range p.Pushes {
		if applied[e.PushID] {
			continue
		}
		applied[e.PushID] = true
		for _, c := range e.Commands {
			if c.Result != ResultOK {
				continue
			}
			// IN THE RECORD'S ORDER, never a map's, so every node inserts
			// the drops alike.
			if d := (refDrop{ref: c.Ref, old: c.Old}); leavesObject(c) && !dropped[d] {
				dropped[d] = true
				drops = append(drops, d)
			}
			if c.New == ZeroOID {
				final[c.Ref] = ""
				continue
			}
			final[c.Ref] = c.New
		}
		if e.Pack != nil {
			packRows = append(packRows, *e.Pack)
			packBytes += e.Pack.Size
			packObjects += e.Pack.Objects
		}
		pushRows = append(pushRows, e)
	}
	if len(pushRows) == 0 {
		// A WHOLLY REDELIVERED BATCH writes nothing at all.
		return 0, nil
	}

	refs := make([]string, 0, len(final))
	for ref := range final {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	rows := len(refs) + len(packRows) + len(pushRows) + len(drops) + 1

	// THE SEATS' DAYS BEFORE ANY HORIZON, the rows they touch counted in the
	// record's before a horizon takes what the record leaves: the committer
	// counts a seat's entry seatDayRows more (rowBound), so what it kept for
	// a deletion's tombstone is still the sweep's.
	counted, err := countSeatDays(ctx, tx, at, pushRows)
	if err != nil {
		return 0, err
	}
	rows += counted

	// THE CHANGES THE RECORD MOVES, before any horizon too: their rows are
	// the record's own, which its decide counted as this writes them
	// (revisions.go) — each entry in order, each change one version.
	changed, err := applyChangeMoves(ctx, tx, at, p.RepoID, pushRows)
	if err != nil {
		return 0, err
	}
	rows += changed

	// THE PUSH HISTORY FIRST, before any ref is written: what it removes is
	// rows of the record's budget the tombstones cannot have, and the
	// tombstones' removal backstop is decided from that budget before the
	// refs it changes are written.
	expired, err := a.expirePushes(ctx, tx, at, p.RepoID, MaxRecordRows-rows)
	if err != nil {
		return 0, err
	}
	// THE MARK AND THE COUNTER ARE ONE ROW, written once the tombstones are
	// swept: the budget's last.
	sweep := MaxRecordRows - rows - expired - 1

	held, err := tombstonesHeld(ctx, tx, p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("code: read the tombstones the push at %s finds: %w", at.position, err)
	}
	// HOW FAR PAST ITS BOUND the record would leave the tombstones were its
	// deletions tombstones: more than the sweep can take, and they are
	// removals.
	over := held + moves(prior, final, false).tombstones - MaxTombstonesPerRepo
	removal := over > max(sweep, 0)
	moved := moves(prior, final, removal)
	var kept, removed []string
	for _, ref := range refs {
		if removal && final[ref] == "" {
			removed = append(removed, ref)
			continue
		}
		kept = append(kept, ref)
	}
	if _, err = store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_refs (repo_id, ref, target, position, at) VALUES `, `(?, ?, ?, ?, ?)`,
		`ON CONFLICT (repo_id, ref) DO UPDATE SET target = excluded.target, position = excluded.position, at = excluded.at`,
		len(kept), func(i int) []any {
			return []any{p.RepoID, kept[i], final[kept[i]], at.packed, at.at()}
		}); err != nil {
		return 0, fmt.Errorf("code: write the refs at %s: %w", at.position, err)
	}
	if len(removed) > 0 {
		list, listErr := jsonList(removed)
		if listErr != nil {
			return 0, listErr
		}
		if _, err = tx.ExecContext(ctx, deleteRefsSQL, p.RepoID, list); err != nil {
			return 0, fmt.Errorf("code: remove the refs deleted at %s: %w", at.position, err)
		}
	}
	// A PUSH'S PACK DATES ITS CONTENT FROM ITS OWN POSITION ([PackRow.Content]):
	// what a push brings reaches the repository here — a base a thin pack was
	// completed with among it, which a ref reached when the push was checked —
	// so a purge requested before it asks nothing about it.
	if _, err = store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_packs (repo_id, pack_id, part, object, hash, size, index_bytes,
			reverse_bytes, git_checksum, objects, kind, state, added_position, content_position) VALUES `,
		`(?, ?, 'pack', ?, ?, ?, ?, ?, ?, ?, 'push', 'live', ?, ?)`,
		`ON CONFLICT (repo_id, pack_id, part) DO NOTHING`,
		len(packRows), func(i int) []any {
			k := packRows[i]
			return []any{p.RepoID, k.PackID, k.Object.String(), string(k.Hash), k.Size,
				k.IndexBytes, k.ReverseBytes, k.GitChecksum, k.Objects, at.packed, at.packed}
		}); err != nil {
		return 0, fmt.Errorf("code: write the packs at %s: %w", at.position, err)
	}
	// THE ROW'S WIDTH IS WRITTEN BESIDE IT: the bytes of its wide columns —
	// the commands, the introduced ids and a seat's run, up to six fields
	// of MaxProvenanceBytes — which is what deleting it costs and what the
	// history's horizon and a purge part are bounded by.
	encoded := make([]string, len(pushRows))
	introduced := make([]string, len(pushRows))
	for i, e := range pushRows {
		if encoded[i], err = recordedCommands(e.Commands); err != nil {
			return 0, err
		}
		introduced[i] = strings.Join(e.Introduced, " ")
	}
	// WHO PUSHED, AS THE HISTORY NAMES THEM (Principal.Actor): an operator's
	// credential beside the seat it is bound to, or a seat's own handle as
	// both — and how the push was built and, for a seat's, the run behind it
	// (a coding box's naming its run and coding agent too), each written as
	// the record carries it.
	if _, err = store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat,
			via, turn_id, work_backend, work_key, model, run_id, coding_agent, node, atomic, commands,
			pack_id, pack_bytes, introduced, introduced_count, received_at, row_bytes) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, ``,
		len(pushRows), func(i int) []any {
			e := pushRows[i]
			packID, size := "", int64(0)
			if e.Pack != nil {
				packID, size = e.Pack.PackID, e.Pack.Size
			}
			run := e.Principal.Run
			return []any{e.PushID, p.RepoID, at.packed, at.at(), e.Principal.Actor(),
				string(e.Principal.Kind), e.Principal.Seat, string(e.Via), run.TurnID, run.WorkBackend,
				run.WorkKey, run.Model, run.RunID, run.CodingAgent, e.Node, e.Atomic, encoded[i], packID, size,
				introduced[i], e.IntroducedCount, store.EncodeTime(e.ReceivedAt),
				pushRowBytes(encoded[i], introduced[i], run)}
		}); err != nil {
		return 0, fmt.Errorf("code: write the pushes at %s: %w", at.position, err)
	}
	// THE CONFLICT IS A SKIP, never an abort, though no record this build
	// applies meets one — every drop it writes is new at its own position —
	// because an insert aborted on its primary key would stall the log on
	// every node.
	if _, err = store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_ref_drops (repo_id, at, position, ref, old) VALUES `, `(?, ?, ?, ?, ?)`,
		`ON CONFLICT (repo_id, at, position, ref, old) DO NOTHING`,
		len(drops), func(i int) []any {
			return []any{p.RepoID, at.at(), at.packed, drops[i].ref, drops[i].old}
		}); err != nil {
		return 0, fmt.Errorf("code: record the objects the push at %s left: %w", at.position, err)
	}
	// A REMOVAL RAISES THE MARK TO THE RECORD'S OWN POSITION, in the counters'
	// own row: a delta from below it would miss the deletions it removed.
	var mark int64
	if removal {
		mark = at.packed
	}
	// AND THE IDLE CLOCK MOVES WITH THE LAST PUSH — a refused entry's too,
	// which is still somebody working in the repository: what a scratch
	// repository expires by (Retention), at the record's own instant.
	if _, err = tx.ExecContext(ctx, pushCountersSQL, moved.live, moved.branches, moved.tags,
		moved.tombstones, len(packRows), packBytes, packObjects, len(pushRows), at.at(), at.at(), at.packed,
		mark, p.RepoID); err != nil {
		return 0, fmt.Errorf("code: count the pushes at %s: %w", at.position, err)
	}

	swept, err := a.sweepTombstones(ctx, tx, at, p.RepoID, sweep,
		held+moved.tombstones-MaxTombstonesPerRepo)
	if err != nil {
		return 0, err
	}
	// THE DROPS LAST, with what the tombstones left: their mark's row too,
	// where they swept nothing.
	forgotten, err := a.expireRefDrops(ctx, tx, at, p.RepoID, MaxRecordRows-rows-expired-swept)
	if err != nil {
		return 0, err
	}
	return rows + expired + swept + forgotten, nil
}

// applyChangeMoves writes what the record's entries — pushes, in order, none
// already applied — do to the live changes their ok commands reach: a head
// move's revision and its pusher as an author, a merge, a close, and the
// change an entry opens from its push's options (revisions.go), from the
// change rows the record finds, and answers the rows it wrote. A record
// carrying an open reads the repository's next number first, which its opens
// take in their entries' order.
func applyChangeMoves(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	pushes []PushEntry) (int, error) {

	book, err := readChangeBook(ctx, tx, repoID, pushes, at.packed, at.at())
	if err != nil {
		return 0, fmt.Errorf("code: the push at %s: %w", at.position, err)
	}
	if slices.ContainsFunc(pushes, func(e PushEntry) bool { return e.Change != nil }) {
		if err := tx.QueryRowContext(ctx, nextChangeSQL, repoID).Scan(&book.next); err != nil {
			return 0, fmt.Errorf("code: read %s's next change at %s: %w", repoID, at.position, err)
		}
	}
	for _, e := range pushes {
		book.move(e)
		if e.Change == nil {
			continue
		}
		if _, why := book.open(e); why != "" {
			// A RECORD NO CORRECT LOG CARRIES — its decide opens a change
			// only where every guard holds — applies the push and opens
			// nothing, logged: a refusal would stop the log on every node.
			_, _ = changeNotApplied(ctx, at, repoID, PushChangeID(e.PushID), why)
		}
	}
	written, err := book.write(ctx, tx, at.maxVariables)
	if err != nil {
		return 0, fmt.Errorf("code: the changes the push at %s moves: %w", at.position, err)
	}
	return written, nil
}

// refDrop is one object a forced update or a deletion moved a ref off: one
// code_ref_drops row of the record that applies it.
type refDrop struct{ ref, old string }

// leavesObject reports a command whose apply moves its ref off an object
// that nothing the command writes reaches: a forced update, whose old
// object is not behind its new one, or a deletion — never a create, which
// moved its ref off nothing, nor a fast-forward, whose old object its new
// one still reaches. A tag never moves (RefusalTagMoved), so it leaves an
// object only by its deletion; a forced flag on a create, which no receiver
// sets, leaves none.
func leavesObject(c Command) bool {
	return c.Old != ZeroOID && (c.Forced || c.New == ZeroOID)
}

// expireRefDrops removes, inside the record's own repository and within limit
// rows, its ref drops older than PruneGrace by the record's broker instant —
// oldest first, by instant and then by the rest of the key, the order
// code_ref_drops' key keeps ([dropsKey]) — and answers how many it removed.
//
// # The drops take what the tombstones leave, and no fixed share
//
// A record's own drops are rows the committer counts in its bound
// ([commandRows]); their removal past the grace is a horizon, which takes
// only what the record's rows leave of MaxRecordRows — after the push
// history's share and the tombstones, whose sweep must keep the room the
// committer kept for a deletion's tombstone or a record it formed would
// remove its deletions. So it has every row a record leaves when no
// tombstone is due: 3 996 behind a one-command push, and 1 997 behind one
// push of MaxCommandsPerPush forced updates, which writes a thousand drops.
// A fixed share would have fallen behind that push — MaxRecordRows/8, five
// hundred a record, against its thousand — as a fixed share of the
// tombstones once did (HorizonDeletesPerRecord); and a share bounds no lane
// time a full budget does not, since a record touches MaxRecordRows at most
// either way, and deleting a drop moves one index entry, its key's, where
// the tombstones a sweep may take as many of move three — measured, 22 ms
// for a record forgetting 3 996 drops against 40 ms for one sweeping 3 995
// tombstones (doc.go's measured section).
//
// # Where it falls behind, and how it catches up
//
// It keeps pace — forgets at least as many drops as its record writes, so
// the drops past the grace do not grow — wherever what the record leaves is
// at least that: behind a record of up to some 1 330 forced updates (one
// push of MaxCommandsPerPush always) while no tombstone is due, and behind
// any record of deletions the committer forms while their tombstones are
// not yet due. Elsewhere it falls behind by the difference. Behind the
// committer's fullest record of forced updates — 1 965 in two pushes,
// counted two rows each — it has 65 rows for their 1 965 drops, some 1 900
// short. Behind deletions whose tombstones come due in turn, the sweep takes
// a row for each first: 996 rows are left for the thousand drops of one
// maximal push of them, four short, and 64 for the 1 310 of the committer's
// fullest record of them, 1 246 short. And up to 64 fewer while the push
// history is expiring too. No reservation in the committer's bound closes
// that for deletions: a deletion's lifetime is four row operations — its
// tombstone written and swept, its drop written and forgotten — and one
// push of MaxCommandsPerPush of them would need 4 002 rows of a record's
// 4 000.
//
// WHAT IT FALLS SHORT BY WAITS for a record of the repository with room —
// up to MaxRecordRows-4 (3 996) a record behind a one-command push — so the
// drops past the grace grow for as long as the repository's records stay
// full of forced updates or deletions, and drain, oldest first, once they
// are not. A drop forgotten late costs its row on every data node; one
// forgotten early would cost the object it keeps, which is why nothing
// forgets one inside its grace.
//
// BY THE INSTANT, which the key leads with, so the read seeks the expired
// rows and stops at the first inside the grace or at its limit, whatever
// order a leader change stamped the records in — every node holds the same
// rows and removes the same ones, never one inside the grace; and by the rest
// of the key after it, so a removal cut at its limit cuts every node's rows
// at one row. A record's own drops are never past the grace at its own
// instant.
func (a *Applier) expireRefDrops(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	limit int) (int, error) {

	if limit <= 0 {
		return 0, nil
	}
	horizon := store.EncodeTime(at.brokerAt.Add(-PruneGrace))
	res, err := tx.ExecContext(ctx, expireDropsSQL, repoID, horizon, limit)
	if err != nil {
		return 0, fmt.Errorf("code: remove %s's ref drops past their grace: %w", repoID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// moves is what the record does to the repository's counts: every ref it
// names from its state before the record (prior, absent for a ref with no
// row) to its state after (final, "" for a deletion). A deletion is a
// TOMBSTONE, or with removal a ref with NO ROW, which counts nothing.
func moves(prior, final map[string]string, removal bool) refCounts {
	var moved refCounts
	for ref, target := range final {
		if before, had := prior[ref]; had {
			moved.count(ref, before, -1)
		}
		if target != "" || !removal {
			moved.count(ref, target, +1)
		}
	}
	return moved
}

// tombstonesHeld is the tombstones the repository's row counts, and none for a
// repository with no row — a record about one is applied as the rows it
// writes, never refused, since an error here would stall the log on every node.
func tombstonesHeld(ctx context.Context, tx *sql.Tx, repoID string) (int, error) {
	var held int
	err := tx.QueryRowContext(ctx, tombstonesHeldSQL, repoID).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return held, err
}

// refCounts is what a repository's refs hold, as code_repos counts it.
type refCounts struct {
	live, branches, tags, tombstones int
}

// count adds sign times one ref holding target — a tombstone when target is
// empty — to the counts.
func (c *refCounts) count(ref, target string, sign int) {
	if target == "" {
		c.tombstones += sign
		return
	}
	c.live += sign
	switch {
	case strings.HasPrefix(ref, branchPrefix):
		c.branches += sign
	case strings.HasPrefix(ref, tagPrefix):
		c.tags += sign
	}
}

// recordedCommands is the commands a push row stores: every one with its
// result, and no Detail — a sentence is an answer, not state.
func recordedCommands(cmds []Command) (string, error) {
	stored := make([]Command, len(cmds))
	for i, c := range cmds {
		c.Detail = ""
		stored[i] = c
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return "", fmt.Errorf("code: encode a push's commands: %w", err)
	}
	return string(b), nil
}

// pushRowBytes is what a push row weighs as code_pushes.row_bytes records it:
// the bytes of the columns that make one row wider than another — its
// commands as stored, its introduced ids and a seat's run, which is six
// fields of up to MaxProvenanceBytes — because deleting a wide row frees its
// overflow pages, and that is what the history's horizon and a purge part are
// bounded by. Every other column is a few dozen bytes on every row. Each of
// these is its record's own, carried there with more (quoted, keyed, escaped),
// so the bytes one record adds to the history are never more than the record
// (HorizonBytesPerRecord).
func pushRowBytes(commands, introduced string, run RunProvenance) int {
	return len(commands) + len(introduced) + len(run.TurnID) + len(run.WorkBackend) + len(run.WorkKey) +
		len(run.Model) + len(run.RunID) + len(run.CodingAgent)
}

// countSeatDays counts each SEAT'S entry of the record that the apply
// inserts — pushes, the redelivered ones already left out — on the company day
// its decide stamped on it ([PushEntry.Day]), in code_seat_days, the counter
// the decide reads a seat's day's quota from; and forgets, for every seat it
// counts, the seat's oldest day beyond its two newest. It answers the rows it
// touched.
//
// # The log decides what is old, so every node counts alike
//
// The day is the decide's, never the applier's clock: so the count is a pure
// function of the log, and every data node holds the same rows whatever its
// own clock says. What is OLD is read off those rows too — a seat's days
// beyond the two newest the log has counted it on — never off a calendar
// this apply would need a zone for. Only the inserted entries count, because
// M2's redelivery rule skips a push id code_pushes already holds, and its
// count with it.
//
// # Two days, because a committer behind midnight still counts the older
//
// Each repository's committer stamps the day ITS OWN clock reads, so while
// two committers' clocks straddle the company's midnight a seat's pushes are
// counted on two days at once: the one still on the 2nd reads the 2nd's row
// after the other's record of the 3rd has applied. Forgetting the 2nd as the
// 3rd is counted would hand that committer a day of no pushes: it would land
// the seat's pushes past the bound again, its count starting over at every
// record the other lands, and a record it decided late would count its day
// from none. Kept, the 2nd's row holds them all: the lagging committer
// refuses the seat past the bound, and its late record adds to its day's
// whole count. A day is forgotten only once the seat holds two newer ones,
// and the newer of those was stamped by a clock that had reached a day
// beginning a whole company day after the forgotten one ended, so no
// committer still deciding on it stands less than a day behind another —
// while no node raises clock_skew, any two stand within two hours (twice
// objstore.AssumedClockSkew). By the rows
// rather than by the calendar's day before the newest: where a zone skips a
// date (Samoa's 30 December 2011, a window period.At never answers), the
// calendar's day before the 31st is one no committer stamps, and a rule
// keeping it would forget the 29th, which a committer two seconds behind is
// still deciding on.
//
// # One day at most, so the rows a record touches are bounded
//
// A record's seat entries all carry the one day its decide's round counted
// ([seatQuota]), so a seat holds at most two days before a record and three
// after its count, and forgetting one an entry keeps the table at two rows at
// most a seat that ever pushed. ONE, not every day beyond the two: the
// committer bounds a seat's entry at [seatDayRows], the day's row and one
// forgotten, and an unbounded forget could take rows the record's tombstone
// sweep was promised. Two statements a record, never one a seat: the counts
// driven from their list, and the forgetting from the seats'.
func countSeatDays(ctx context.Context, tx *sql.Tx, at applyContext, pushes []PushEntry) (int, error) {
	type seatDay struct{ seat, day string }
	var counted []seatDay
	counts := map[seatDay]int{}
	var seats []string
	listed := map[string]bool{}
	for _, e := range pushes {
		if e.Principal.Kind != PrincipalAgent || e.Day == "" {
			continue
		}
		k := seatDay{e.Principal.Seat, e.Day}
		if counts[k] == 0 {
			counted = append(counted, k)
		}
		counts[k]++
		if !listed[k.seat] {
			listed[k.seat] = true
			seats = append(seats, k.seat)
		}
	}
	if len(counted) == 0 {
		return 0, nil
	}
	if _, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_seat_days (seat, day, pushes) VALUES `, `(?, ?, ?)`,
		`ON CONFLICT (seat, day) DO UPDATE SET pushes = pushes + excluded.pushes`,
		len(counted), func(i int) []any {
			k := counted[i]
			return []any{k.seat, k.day, counts[k]}
		}); err != nil {
		return 0, fmt.Errorf("code: count the seats' pushes at %s: %w", at.position, err)
	}
	list, err := jsonList(seats)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, forgetSeatDaysSQL, list)
	if err != nil {
		return 0, fmt.Errorf("code: forget the seats' older days at %s: %w", at.position, err)
	}
	forgotten, _ := res.RowsAffected()
	return len(counted) + int(forgotten), nil
}

// expirePushes removes, inside the record's own repository and within the
// rows the record has left of MaxRecordRows (budget), its push rows older than
// PushHistory by the record's broker instant — at most HorizonDeletesPerRecord
// of them, and no more once their bytes reach HorizonBytesPerRecord — and
// answers how many it removed.
//
// OLDEST BY POSITION, STOPPING AT THE FIRST INSIDE THE HORIZON. The rows are
// read in log order (code_pushes_repo_idx, replicated 0056) and taken while
// their instant is before the horizon, and the first row that is not ends the
// read. NEVER A FILTER ON THE INSTANT: the index orders by position, so `at <
// ?` seeks nothing there, and over position order it would walk the
// repository's whole history — 181 days of pushes — on every push record
// that found fewer than its LIMIT expired, to find the few that were.
// Stopped, the read costs the rows it removes and one more. A record's
// instant is the broker's, and a stream leader's change can stamp a later
// record earlier; such a row waits for the one before it, at most the
// members' clock skew past its own horizon — and every node, holding the
// same rows in the same order, removes the same ones, never one inside the
// horizon.
func (a *Applier) expirePushes(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	budget int) (int, error) {

	limit := min(HorizonDeletesPerRecord, budget)
	if limit <= 0 {
		return 0, nil
	}
	horizon := store.EncodeTime(at.brokerAt.Add(-PushHistory))
	expired, _, _, err := pushesWithin(ctx, tx, HorizonBytesPerRecord,
		func(instant int64) bool { return instant >= horizon }, expiredPushesSQL, repoID, limit)
	if err != nil {
		return 0, fmt.Errorf("code: read %s's expired pushes: %w", repoID, err)
	}
	if len(expired) == 0 {
		return 0, nil
	}
	list, err := jsonList(expired)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, deletePushesSQL, list)
	if err != nil {
		return 0, fmt.Errorf("code: delete %s's expired pushes: %w", repoID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// sweepTombstones removes, inside the record's own repository and within limit
// rows, its tombstones oldest first — by deletion instant, then ref — each
// that is among the first excess or older than RefTombstoneHorizon by the
// record's broker instant, stopping at the first that is neither; and raises
// tombstones_through to the highest position a swept tombstone held, so a
// delta from below it is answered in full. It answers the rows it wrote: the
// tombstones and the mark.
//
// # Two reasons a tombstone goes, one pass
//
// AGE: a tombstone is kept so a cache syncing a delta learns of the deletion,
// and a cache synced longer ago than RefTombstoneHorizon is answered whole
// instead. COUNT: excess is how far the record left the repository past
// MaxTombstonesPerRepo, and the oldest go first, because the cache a forgotten
// deletion sends to a full answer is one synced before it, and the longer ago
// that was, the likelier it is idle — and a full answer re-reads the rows and
// reuses every pack it holds, never a download. Both read the one order
// code_refs_tombstones_idx keeps, so the pass is one seek and a scan that ends
// at the first tombstone it keeps: one row read when nothing is due.
//
// # The tombstones take everything the record left
//
// A record adds at most MaxPushesPerRecord push rows, which a fixed share of
// HorizonDeletesPerRecord keeps pace with. It can make some thirteen hundred
// TOMBSTONES behind a record this build's committer forms, every deletion
// counted in its rows with a row kept for its sweep and one for the object it
// left ([commandRows]) — and behind one formed otherwise, a newer
// build's or one written by hand, as many as one append carries, since
// nothing at the apply holds a record to this build's committer's bounds:
// some sixty thousand deletions of short refs in the log's largest record
// (CodeLogMaxRecordBytes), which is why the removal backstop takes whatever
// the record carries. A fixed share of those falls behind any repository
// whose pushes delete refs in bulk — a CI deleting its build tags, a branch
// clean-up: measured, records that each made a thousand refs and deleted the
// previous thousand grew the table by about 936 tombstones a record with a
// share of 64, for ever. The rest of the row budget is room the framework's transaction bound
// already pays for, and every node computes the same budget from the same
// record and the same rows, so the sweep stays a pure function of the record:
// 3 995 tombstones behind a one-command push, at least as many as its own
// deletions behind any record a committer forms ([rowBound]), and none behind
// one it filled to the row bound with creations. That is what makes the COUNT
// exact with one backstop no record of this build's committer reaches: a
// record whose excess is more than its limit writes its deletions as removals
// instead ([Applier.applyPush]), so excess here never passes limit. The ref
// drops past their grace come after it, and take only what it leaves
// ([Applier.expireRefDrops]): a drop forgotten late costs a row, and a
// tombstone's sweep room taken costs every git node a full read.
func (a *Applier) sweepTombstones(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	limit, excess int) (int, error) {

	if limit <= 0 {
		return 0, nil
	}
	aged := store.EncodeTime(at.brokerAt.Add(-RefTombstoneHorizon))
	rows, err := tx.QueryContext(ctx, tombstonesOldestSQL, repoID, limit)
	if err != nil {
		return 0, fmt.Errorf("code: read %s's oldest tombstones: %w", repoID, err)
	}
	var stones []string
	var through int64
	for rows.Next() {
		var ref string
		var position, deleted int64
		if scanErr := rows.Scan(&ref, &position, &deleted); scanErr != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("code: read %s's oldest tombstones: %w", repoID, scanErr)
		}
		if len(stones) >= excess && deleted >= aged {
			break
		}
		stones = append(stones, ref)
		through = max(through, position)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("code: read %s's oldest tombstones: %w", repoID, err)
	}
	_ = rows.Close()
	if len(stones) == 0 {
		return 0, nil
	}
	list, err := jsonList(stones)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, deleteRefsSQL, repoID, list)
	if err != nil {
		return 0, fmt.Errorf("code: delete %s's swept tombstones: %w", repoID, err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, sweptTombstonesSQL, through, n, repoID); err != nil {
		return 0, fmt.Errorf("code: raise %s's tombstone mark: %w", repoID, err)
	}
	return int(n) + 1, nil
}

// applyPurgePart deletes one bounded part of a deleted repository's rows, in a
// fixed order — its pushes, then its refs, then its packs, then the packs a
// record retired, then its ref drops, then its changes, each whole: its
// revisions, its authors, its conversation, its body and then its row
// ([purgeChanges]) — then its checks, each with its log ([purgeChecks]), then
// its home's dispatches and its targets' readings ([purgeDispatches]) — then
// its merge queues' entries, candidates, landings and green rows
// ([purgeQueues]) — so every node deletes the same rows. THE PART IS THE
// GUARD: only the row's purge_parts + 1 deletes anything, so a redelivered or
// stale part deletes nothing more.
//
// BOUNDED BY ROWS AND BY BYTES: at most PurgePartRows rows, and its wide rows
// only until what they weigh reaches PurgePartBytes — the push rows by their
// row_bytes, and after them the changes by their bodies' body_bytes and their
// conversations' text_bytes, and the checks by their own row_bytes, one budget
// for all, because deleting any of them frees overflow pages a count does not
// see. A part whose pushes were cut by bytes deletes nothing else: the refs
// and packs wait for a part whose pushes are done, and the changes and the
// checks for one whose wide rows left room.
//
// A RETIRED PACK'S ROW GOES WITH ITS REPOSITORY, its grace or not: the grace
// is for a reader that began on the pack before it was retired, and no read
// begins on a deleted repository — it is refused to every new one from its
// deletion, and code.deleted_retention_days, a day at least, outlasts the
// reads that had begun. Its object is then named by nothing, and the
// collector takes it as it takes the repository's live packs'. SO DOES A REF
// DROP, its grace or not: it keeps an object of the repository's own packs
// reachable, and those packs go with it.
func (a *Applier) applyPurgePart(ctx context.Context, tx *sql.Tx, at applyContext, p PurgePart) (int, error) {
	var parts int
	if err := tx.QueryRowContext(ctx, purgePartsSQL, p.RepoID).Scan(&parts); err != nil {
		return 0, fmt.Errorf("code: read %s's purge at %s: %w", p.RepoID, at.position, err)
	}
	if p.Part != parts+1 {
		return 0, nil
	}
	budget := PurgePartRows
	deleted := 0

	pushIDs, weighed, cut, err := pushesWithin(ctx, tx, PurgePartBytes, nil, purgePushesSQL, p.RepoID, budget)
	if err != nil {
		return 0, fmt.Errorf("code: read %s's pushes to purge: %w", p.RepoID, err)
	}
	if len(pushIDs) > 0 {
		list, err := jsonList(pushIDs)
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, deletePushesSQL, list)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s's pushes: %w", p.RepoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
		budget -= len(pushIDs)
	}
	if cut {
		budget = 0
	}

	var refs []string
	var gone refCounts
	if budget > 0 {
		rows, err := tx.QueryContext(ctx, purgeRefsSQL, p.RepoID, budget)
		if err != nil {
			return 0, fmt.Errorf("code: read %s's refs to purge: %w", p.RepoID, err)
		}
		for rows.Next() {
			var ref, target string
			if err := rows.Scan(&ref, &target); err != nil {
				_ = rows.Close()
				return 0, fmt.Errorf("code: read %s's refs to purge: %w", p.RepoID, err)
			}
			refs = append(refs, ref)
			gone.count(ref, target, +1)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("code: read %s's refs to purge: %w", p.RepoID, err)
		}
		_ = rows.Close()
	}
	if len(refs) > 0 {
		list, err := jsonList(refs)
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, deleteRefsSQL, p.RepoID, list)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s's refs: %w", p.RepoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
		budget -= len(refs)
	}

	var packIDs []string
	livePacksGone, liveBytesGone, liveObjectsGone := 0, int64(0), int64(0)
	if budget > 0 {
		rows, err := tx.QueryContext(ctx, purgePacksSQL, p.RepoID, budget)
		if err != nil {
			return 0, fmt.Errorf("code: read %s's packs to purge: %w", p.RepoID, err)
		}
		for rows.Next() {
			var id, state string
			var size, objects int64
			if err := rows.Scan(&id, &size, &objects, &state); err != nil {
				_ = rows.Close()
				return 0, fmt.Errorf("code: read %s's packs to purge: %w", p.RepoID, err)
			}
			packIDs = append(packIDs, id)
			if PackState(state) == PackLive {
				livePacksGone++
				liveBytesGone += size
				liveObjectsGone += objects
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("code: read %s's packs to purge: %w", p.RepoID, err)
		}
		_ = rows.Close()
	}
	if len(packIDs) > 0 {
		list, err := jsonList(packIDs)
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, deletePacksSQL, p.RepoID, list)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s's packs: %w", p.RepoID, err)
		}
		n, _ := res.RowsAffected()
		// ONE PART A PACK is what makes deleting by pack id exact: every
		// pack this build stores is one bundle, one row. A pack split
		// across rows would be deleted whole here while the budget counted
		// one of them — a writer's bug, refused rather than miscounted.
		if int(n) != len(packIDs) {
			return 0, fmt.Errorf("code: purging %s deleted %d pack rows for %d packs — "+
				"a pack this build stores is one row", p.RepoID, n, len(packIDs))
		}
		deleted += int(n)
		budget -= len(packIDs)
	}
	if budget > 0 {
		res, err := tx.ExecContext(ctx, purgeRetiredSQL, p.RepoID, budget)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s's retired packs: %w", p.RepoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
		budget -= int(n)
	}
	if budget > 0 {
		res, err := tx.ExecContext(ctx, purgeDropsSQL, p.RepoID, budget)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s's ref drops: %w", p.RepoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
		budget -= int(n)
	}
	if budget > 0 {
		n, w, whole, err := purgeChanges(ctx, tx, p.RepoID, budget, weighed)
		if err != nil {
			return 0, err
		}
		deleted += n
		budget -= n
		weighed = w
		// AND THEIR NOTICES, which no part counts: the table is divergent,
		// and the part's count decides which replicated rows go
		// (apply_notices.go).
		removed, err := a.removeNotices(ctx, tx, whole)
		if err != nil {
			return 0, err
		}
		deleted += removed
	}
	if budget > 0 {
		n, err := purgeChecks(ctx, tx, p.RepoID, budget, weighed)
		if err != nil {
			return 0, err
		}
		deleted += n
		budget -= n
	}
	if budget > 0 {
		n, err := purgeDispatches(ctx, tx, p.RepoID, budget)
		if err != nil {
			return 0, err
		}
		deleted += n
		budget -= n
	}
	if budget > 0 {
		n, err := purgeQueues(ctx, tx, p.RepoID, budget)
		if err != nil {
			return 0, err
		}
		deleted += n
	}
	if _, err := tx.ExecContext(ctx, purgeCountersSQL, p.Part, gone.live, gone.branches, gone.tags,
		gone.tombstones, livePacksGone, liveBytesGone, liveObjectsGone, at.packed, p.RepoID); err != nil {
		return 0, fmt.Errorf("code: count %s's purge at %s: %w", p.RepoID, at.position, err)
	}
	return deleted + 1, nil
}

// applyPurge removes what remains of a repository and writes its deletion
// marker — THE GATE. Applied whatever the row's status (the decide is the
// guard, and the applier never refuses ordinary traffic) and idempotent: a
// redelivery of the marker's own record, which its gate lets through by op id,
// deletes nothing and inserts nothing. Unbounded statements are safe because
// the decide forms a purge only once at most PurgePartRows of the
// repository's pushes, refs, packs, retired packs, ref drops, changes'
// revisions, authors, bodies and rows and their conversations' rows — its
// reviewers, participants, comments, reviews, findings, revisions' facts and
// mergeability among them — its checks and their logs, and its merge queues'
// entries, candidates, landings and green rows remain AND the
// pushes, the bodies, the conversations' text and the checks among them
// weigh at most PurgePartBytes — a part's own bounds — and it holds at most
// MaxProtectionsPerRepo rules, MaxPathOwners path owners (one row each, their
// globs packed into it) and a published chain of at most MaxPublishedLayers
// layers of MaxLayerParts parts each: with the name claim, the row, its kind's
// count and the marker that is PurgePartRows + MaxProtectionsPerRepo +
// MaxPathOwners + MaxPublishedLayers × (1 + MaxLayerParts) + 4 rows, which the
// limits test holds within MaxRecordRows. A change's revisions, authors, body
// and conversation go before the changes, through which alone they are found.
//
// AN OLD NAME a rename left redirecting to the repository is NOT released
// here: how many a repository holds is bounded by nothing a part counts — one
// a rename, for RedirectWindow — so they are released one record each by the
// code-retention pass at their window's end ([Store.ReleaseName]), whatever
// became of the repository meanwhile, which keeps every redirect's end in one
// place and this record's rows bounded.
//
// NO PART REMOVES A LAYER: the chain is bounded apart from the rows a part
// counts, and a layer row naming a pack a part already removed harms no
// reader, since a deleted repository serves nothing and every cache drops it
// whole. The collector takes the parts' objects at its first pass after this
// record, since a derived row keeps its object only while it stands.
func (a *Applier) applyPurge(ctx context.Context, tx *sql.Tx, at applyContext, p Purge) (int, error) {
	// THE NOTICES OF THE CHANGES THE PURGE STILL FINDS, before their rows
	// go: the only way to them from the repository is its changes.
	remaining, err := changesOf(ctx, tx, p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("code: read %s's remaining changes at %s: %w", p.RepoID, at.position, err)
	}
	deleted, err := a.removeNotices(ctx, tx, remaining)
	if err != nil {
		return 0, err
	}
	for _, stmt := range []string{
		purgeAllPushesSQL, purgeAllRefsSQL, purgeAllPacksSQL, purgeAllRetiredSQL, purgeAllDropsSQL,
		purgeAllRevisionsSQL, purgeAllAuthorsSQL, purgeAllReviewersSQL, purgeAllParticipantsSQL,
		purgeAllFindingsSQL, purgeAllCommentsSQL, purgeAllFindingTextsSQL, purgeAllReviewsSQL,
		purgeAllFactsSQL, purgeAllBodiesSQL, purgeAllMergeabilitySQL, purgeAllConversationsSQL,
		purgeAllChangesSQL, purgeAllCheckLogsSQL, purgeAllChecksSQL, purgeAllDispatchesSQL, purgeAllCheckTargetsSQL,
		purgeAllQueueSQL, purgeAllCandidatesSQL, purgeAllLandingsSQL, purgeAllGreenSQL,
		purgeAllLayerPartsSQL, purgeAllLayersSQL, clearProtectionsSQL, clearPathOwnersSQL,
	} {
		res, err := tx.ExecContext(ctx, stmt, p.RepoID)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s at %s: %w", p.RepoID, at.position, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
	}
	// THE RECORD STATES THE NAME IT RELEASES, and only a claim naming THIS
	// repository is released: a claim another repository holds on the same
	// token is not this purge's to remove.
	res, err := tx.ExecContext(ctx, releaseNameSQL, NameToken(p.Name), p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("code: release %q at %s: %w", p.Name, at.position, err)
	}
	n, _ := res.RowsAffected()
	deleted += int(n)
	// THE ROW'S KIND, read before the row goes: the repository is counted out
	// of it ([countKindSQL]) by the record that removes the row, and only
	// where it does — a redelivery finds no row and counts nobody out twice.
	var kind string
	switch err = tx.QueryRowContext(ctx, repoKindSQL, p.RepoID).Scan(&kind); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, fmt.Errorf("code: read %s's kind at %s: %w", p.RepoID, at.position, err)
	}
	if res, err = tx.ExecContext(ctx, removeRepoSQL, p.RepoID); err != nil {
		return 0, fmt.Errorf("code: remove %s at %s: %w", p.RepoID, at.position, err)
	}
	n, _ = res.RowsAffected()
	deleted += int(n)
	if n > 0 {
		if res, err = tx.ExecContext(ctx, uncountKindSQL, kind); err != nil {
			return 0, fmt.Errorf("code: count %s out at %s: %w", p.RepoID, at.position, err)
		}
		n, _ = res.RowsAffected()
		deleted += int(n)
	}
	if res, err = tx.ExecContext(ctx, markSQL, p.RepoID, p.Name, at.at(), at.record.Actor,
		p.Reason, at.record.OpID, at.packed); err != nil {
		return 0, fmt.Errorf("code: mark %s purged at %s: %w", p.RepoID, at.position, err)
	}
	n, _ = res.RowsAffected()
	return deleted + int(n), nil
}

// keySet reads the keys among keys a statement over json_each finds.
func keySet(ctx context.Context, tx *sql.Tx, query string, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(keys) == 0 {
		return out, nil
	}
	list, err := jsonList(keys)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, query, list)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// pushesWithin reads the push ids query answers with their row_bytes, in its
// order, and keeps them until their bytes reach bytes — always the first,
// whatever it weighs, so a step always advances, and so at most one row past
// the bound — answering what the kept rows weigh, which a purge part's
// changes take what is left of the same bound from ([purgeChanges]). cut
// reports that it left a row the query answered: the pushes were bounded by
// what they weigh rather than by the query's LIMIT. Every node holds the same
// rows with the same widths, so every node keeps the same ids.
//
// stop, where it is given, ends the read at the first row it holds for: the
// query then selects a THIRD column, which stop is asked of, and the row it
// holds for is not kept — how the push-history horizon ends at the first push
// inside it, in the one order every node reads (expirePushes), where a filter
// in the statement would walk every push the repository keeps. A read ended by
// stop is not cut: what bounded it is the rows' age, not their weight. The
// purge passes none and keeps its two-column statement.
func pushesWithin(ctx context.Context, tx *sql.Tx, bytes int64, stop func(int64) bool, query string,
	args ...any) (ids []string, weighed int64, cut bool, err error) {

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if weighed >= bytes {
			return ids, weighed, true, nil
		}
		var id string
		var width, third int64
		dest := []any{&id, &width}
		if stop != nil {
			dest = append(dest, &third)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, 0, false, err
		}
		if stop != nil && stop(third) {
			return ids, weighed, false, nil
		}
		ids = append(ids, id)
		weighed += width
	}
	return ids, weighed, false, rows.Err()
}

// refsNamed is what the repository holds of each of refs — its target, empty
// for a tombstone, and absent from the map where the repository has no row —
// in ONE statement over the primary key.
func refsNamed(ctx context.Context, tx *sql.Tx, repoID string, refs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(refs) == 0 {
		return out, nil
	}
	list, err := jsonList(refs)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, refsNamedSQL, repoID, list)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ref, target string
		if err := rows.Scan(&ref, &target); err != nil {
			return nil, err
		}
		out[ref] = target
	}
	return out, rows.Err()
}

// The push apply's and the purge's statements: every one with a WHERE is
// registered in plan_test.go, so the pinned driver's planner is held to the
// seek each is written for — and planner.go says why a batch is driven from
// its list rather than tested with IN.
const (
	pushesAppliedSQL = `SELECT push_id FROM code_pushes
		WHERE push_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	refsNamedSQL = `SELECT r.ref, r.target FROM json_each(?2) AS j CROSS JOIN code_refs AS r
		WHERE r.repo_id = ?1 AND r.ref = CAST(j.value AS TEXT)`
	pushCountersSQL = `UPDATE code_repos SET refs = refs + ?, branches = branches + ?,
		tags = tags + ?, tombstones = tombstones + ?, live_packs = live_packs + ?,
		live_bytes = live_bytes + ?, live_objects = live_objects + ?, pushes = pushes + ?,
		last_push_at = ?, last_activity_at = ?, version = ?, tombstones_through = MAX(tombstones_through, ?)
		WHERE id = ?`
	tombstonesHeldSQL = `SELECT tombstones FROM code_repos WHERE id = ?`
	// forgetSeatDaysSQL forgets, for each seat of its list, the seat's day
	// beyond its two newest — its oldest, of the three at most a record leaves
	// it ([countSeatDays]) — one row a seat at most, each the third a seek of
	// the counter's key finds newest first, driven from the list.
	forgetSeatDaysSQL = `DELETE FROM code_seat_days WHERE rowid IN (SELECT (SELECT d.rowid
		FROM code_seat_days AS d WHERE d.seat = CAST(j.value AS TEXT)
		ORDER BY d.day DESC LIMIT 1 OFFSET 2) FROM json_each(?) AS j)`
	// expiredPushesSQL reads a repository's oldest pushes in log order with
	// the instant expirePushes stops at — no filter on it: see there.
	expiredPushesSQL = `SELECT push_id, row_bytes, at FROM code_pushes WHERE repo_id = ?
		ORDER BY position, push_id LIMIT ?`
	deletePushesSQL = `DELETE FROM code_pushes
		WHERE push_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	tombstonesOldestSQL = `SELECT ref, position, at FROM code_refs WHERE repo_id = ? AND target = ''
		ORDER BY at, ref LIMIT ?`
	deleteRefsSQL = `DELETE FROM code_refs WHERE rowid IN (SELECT r.rowid
		FROM json_each(?2) AS j CROSS JOIN code_refs AS r
		WHERE r.repo_id = ?1 AND r.ref = CAST(j.value AS TEXT))`
	sweptTombstonesSQL = `UPDATE code_repos SET tombstones_through = MAX(tombstones_through, ?),
		tombstones = tombstones - ? WHERE id = ?`
	// expireDropsSQL is the drops' expiry: the rowid idiom over the key's
	// order, the instant's, so the rows are the oldest the key reaches, cut
	// at the record's budget at one row on every node.
	expireDropsSQL = `DELETE FROM code_ref_drops WHERE rowid IN (SELECT rowid
		FROM code_ref_drops INDEXED BY ` + dropsKey + `
		WHERE repo_id = ? AND at < ? ORDER BY at, position, ref, old LIMIT ?)`
	purgePartsSQL  = `SELECT purge_parts FROM code_repos WHERE id = ?`
	purgePushesSQL = `SELECT push_id, row_bytes FROM code_pushes WHERE repo_id = ?
		ORDER BY position, push_id LIMIT ?`
	purgeRefsSQL  = `SELECT ref, target FROM code_refs WHERE repo_id = ? ORDER BY ref LIMIT ?`
	purgePacksSQL = `SELECT pack_id, size, objects, state FROM code_packs WHERE repo_id = ?
		ORDER BY pack_id, part LIMIT ?`
	deletePacksSQL = `DELETE FROM code_packs WHERE rowid IN (SELECT p.rowid
		FROM json_each(?2) AS j CROSS JOIN code_packs AS p
		WHERE p.repo_id = ?1 AND p.pack_id = CAST(j.value AS TEXT))`
	// purgeRetiredSQL is a part's retired packs, oldest retirement first and
	// no more than the part has left — the sweep's own order.
	purgeRetiredSQL = `DELETE FROM code_packs_retired WHERE rowid IN (SELECT rowid
		FROM code_packs_retired INDEXED BY code_packs_retired_repo_idx WHERE repo_id = ?
		ORDER BY retired_position, pack_id, part LIMIT ?)`
	// purgeDropsSQL is a part's ref drops, oldest first and no more than the
	// part has left — the expiry's own order.
	purgeDropsSQL = `DELETE FROM code_ref_drops WHERE rowid IN (SELECT rowid
		FROM code_ref_drops INDEXED BY ` + dropsKey + ` WHERE repo_id = ?
		ORDER BY at, position, ref, old LIMIT ?)`
	purgeCountersSQL = `UPDATE code_repos SET status = 'purging', purge_parts = ?, refs = refs - ?,
		branches = branches - ?, tags = tags - ?, tombstones = tombstones - ?,
		live_packs = live_packs - ?, live_bytes = live_bytes - ?, live_objects = live_objects - ?,
		version = ? WHERE id = ?`
	releaseNameSQL     = `DELETE FROM code_names WHERE token = ? AND repo_id = ?`
	purgeAllPushesSQL  = `DELETE FROM code_pushes WHERE repo_id = ?`
	purgeAllRefsSQL    = `DELETE FROM code_refs WHERE repo_id = ?`
	purgeAllPacksSQL   = `DELETE FROM code_packs WHERE repo_id = ?`
	purgeAllRetiredSQL = `DELETE FROM code_packs_retired INDEXED BY code_packs_retired_repo_idx
		WHERE repo_id = ?`
	purgeAllDropsSQL = `DELETE FROM code_ref_drops INDEXED BY ` + dropsKey + ` WHERE repo_id = ?`
	// THE PUBLISHED CHAIN, the final purge's alone, each table by its key.
	purgeAllLayerPartsSQL = `DELETE FROM code_layer_parts WHERE repo_id = ?`
	purgeAllLayersSQL     = `DELETE FROM code_layers WHERE repo_id = ?`
	removeRepoSQL         = `DELETE FROM code_repos WHERE id = ?`
	markSQL               = `INSERT INTO code_deletions
			(repo_id, name, at, by, reason, purge_record_id, rejects, last_reject_at, version)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, ?)
		ON CONFLICT (repo_id) DO NOTHING`
)
