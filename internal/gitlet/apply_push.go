package gitlet

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

// applyPush applies a batch of pushes to one repository through
// [Applier.landPushes], each entry keeping whom its push was made for
// ([PushEntry.WorkingFor]) only from a record at [WorkingForVersion] or above
// ([keptCarrier]). THE PUSH RECORD'S FORMAT DOES NOT MOVE ([RecordVersion]):
// every build that meets one writes the rows landPushes writes, and no record
// carries anything new for them.
func (a *Applier) applyPush(ctx context.Context, tx *sql.Tx, at applyContext, p PushBatch) (int, error) {
	entries := make([]PushEntry, len(p.Pushes))
	for i, e := range p.Pushes {
		e.WorkingFor = keptCarrier(at, e.WorkingFor)
		entries[i] = e
	}
	p.Pushes = entries
	return a.landPushes(ctx, tx, at, p, recordPacks{}, 0)
}

// recordPacks is packs a record lands beside its pushes' own, each of kind:
// an import chunk's or a mirror sync's, several to one push, which its push
// row names none of.
type recordPacks struct {
	packs []PackRef
	kind  PackKind
}

// landPushes is the one apply of pushes — a push record's batch
// ([Applier.applyPush]) and a merge record's push ([Applier.applyMerge]) — so
// a merge's move of its target is a push to every reader of the history, the
// counters, the tombstones and the changes (ADR-0008). Each entry applies IN
// ORDER, seeing an earlier one's refs. spent is the rows the record's apply
// already wrote, which its horizons leave to it; it answers the rows it wrote.
// extra is packs the record lands beside its entries' own ([recordPacks]),
// written with them, and not at all where every entry was applied already.
//
// The horizons (doc.go's "Five horizons") run inside the repository in one
// order: the expired push history before a ref is written, the due tombstones
// after, the ref drops past their grace last.
func (a *Applier) landPushes(ctx context.Context, tx *sql.Tx, at applyContext, p PushBatch, extra recordPacks,
	spent int) (int, error) {

	// TWO READS FOR THE WHOLE RECORD, never one per entry or command: the
	// apply holds the estate's one write lock, which every other domain's
	// applier waits behind, and a full record carries nearly 4 000 commands.
	// The ref state is then carried IN MEMORY: every row a record writes
	// carries its one position and instant, so a ref's last value is the truth.
	ids := make([]string, 0, len(p.Pushes))
	for _, e := range p.Pushes {
		ids = append(ids, e.PushID)
	}
	applied, err := pushesApplied(ctx, tx, ids)
	if err != nil {
		return 0, fmt.Errorf("gitlet: read the applied pushes at %s: %w", at.position, err)
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
		return 0, fmt.Errorf("gitlet: read the refs the push at %s moves: %w", at.position, err)
	}

	var (
		final = map[string]string{}
		// setBy is, beside each ref's final target, the principal of the
		// entry whose ok command last set it in the record's order — the
		// writer whose push the ref's row names a seat for ([seatOf]).
		setBy       = map[string]Principal{}
		pushRows    []PushEntry
		packRows    []PackRef
		packKinds   []PackKind
		drops       []refDrop
		dropped     = map[refDrop]bool{}
		packBytes   int64
		packObjects int64
	)
	// A PUSH ID ALREADY APPLIED — a redelivery, a failover's second record, a
	// repeat inside this record — is skipped: its primary-key insert would
	// abort and stall the log on every node.
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
			setBy[c.Ref] = e.Principal
			if c.New == ZeroOID {
				final[c.Ref] = ""
				continue
			}
			final[c.Ref] = c.New
		}
		if e.Pack != nil {
			packRows = append(packRows, *e.Pack)
			packKinds = append(packKinds, e.packKind())
			packBytes += e.Pack.Size
			packObjects += e.Pack.Objects
		}
		pushRows = append(pushRows, e)
	}
	if len(pushRows) == 0 {
		// A WHOLLY REDELIVERED BATCH writes nothing at all.
		return 0, nil
	}
	for _, k := range extra.packs {
		packRows = append(packRows, k)
		packKinds = append(packKinds, extra.kind)
		packBytes += k.Size
		packObjects += k.Objects
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

	// THE CHANGES THE RECORD MOVES, before any horizon too: their rows are the
	// record's own, which its decide admitted by the model this writes them
	// from ([changeBook]). THE TALLY ROWS STAY OUT OF rows: a horizon is given
	// what an older build's rows leave it (record.go).
	changed, tallied, err := applyChangeMoves(ctx, tx, at, p.RepoID, pushRows)
	if err != nil {
		return 0, err
	}
	rows += changed

	// THE LANDINGS ON PROTECTED BRANCHES its decide stamped, before any
	// horizon too: their rows are the record's own ([entryRows]).
	landedRows, err := writePushLandings(ctx, tx, at, p.RepoID, pushRows)
	if err != nil {
		return 0, err
	}
	rows += landedRows

	// THE PUSH HISTORY FIRST: what it removes the tombstones cannot have, and
	// the removal backstop below is decided from what is left. The budget is
	// MaxRecordRows less the record's rows, a merge record's own (spent) too.
	budget := MaxRecordRows - spent
	expired, gone, err := a.expirePushes(ctx, tx, at, p.RepoID, budget-rows)
	if err != nil {
		return 0, err
	}
	// THE MARK AND THE COUNTER ARE ONE ROW, written once the tombstones are
	// swept: the budget's last.
	sweep := budget - rows - expired - 1

	held, err := tombstonesHeld(ctx, tx, p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: read the tombstones the push at %s finds: %w", at.position, err)
	}
	// THE TOMBSTONES ARE BOUNDED EXACTLY, decided here from the counter and
	// the record, never from a count a write already moved. A record leaving
	// them past MaxTombstonesPerRepo by more than the sweep can take — none
	// this build's committer forms, which keeps a row for each deletion's
	// sweep ([commandRows]) — writes its deletions as REMOVALS, each row
	// deleted rather than emptied, so the count ends no higher. It raises the
	// mark to its own position (below), since a delta would miss those
	// deletions, so every cache reads the repository whole: why the committer
	// forms none.
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
	// EVERY ROW NAMES THE SEAT THAT SET IT IN ITS OWN NAMESPACE, or none
	// ([seatOf]) — and never its held_position, which a hold alone writes, so
	// a push leaves the mark of the last hold on the ref where it was.
	if _, err = store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_refs (repo_id, ref, target, position, at, seat) VALUES `, `(?, ?, ?, ?, ?, ?)`,
		`ON CONFLICT (repo_id, ref) DO UPDATE SET target = excluded.target, position = excluded.position, `+
			`at = excluded.at, seat = excluded.seat`,
		len(kept), func(i int) []any {
			ref := kept[i]
			return []any{p.RepoID, ref, final[ref], at.packed, at.at(), seatOf(setBy[ref], ref, final[ref])}
		}); err != nil {
		return 0, fmt.Errorf("gitlet: write the refs at %s: %w", at.position, err)
	}
	if len(removed) > 0 {
		list, listErr := jsonList(removed)
		if listErr != nil {
			return 0, listErr
		}
		if _, err = tx.ExecContext(ctx, deleteRefsSQL, p.RepoID, list); err != nil {
			return 0, fmt.Errorf("gitlet: remove the refs deleted at %s: %w", at.position, err)
		}
	}
	// A PUSH'S PACK DATES ITS CONTENT FROM ITS OWN POSITION ([PackRow.Content]),
	// a thin pack's completing base among it, so a purge requested before it
	// asks nothing about it; and it ARRIVED at the record's broker instant
	// ([PackRow.Arrived]), which the maintainer's expunge keeps for PruneGrace.
	if _, err = store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_packs (repo_id, pack_id, part, object, hash, size, index_bytes,
			reverse_bytes, git_checksum, objects, kind, state, added_position, content_position,
			arrived_at) VALUES `,
		`(?, ?, 'pack', ?, ?, ?, ?, ?, ?, ?, ?, 'live', ?, ?, ?)`,
		`ON CONFLICT (repo_id, pack_id, part) DO NOTHING`,
		len(packRows), func(i int) []any {
			k := packRows[i]
			return []any{p.RepoID, k.PackID, k.Object.String(), string(k.Hash), k.Size,
				k.IndexBytes, k.ReverseBytes, k.GitChecksum, k.Objects, string(packKinds[i]), at.packed,
				at.packed, at.at()}
		}); err != nil {
		return 0, fmt.Errorf("gitlet: write the packs at %s: %w", at.position, err)
	}
	// THE ROW'S WIDTH IS WRITTEN BESIDE IT: the bytes of its wide columns,
	// which is what deleting it costs and what the history's horizon and a
	// purge part are bounded by ([pushRowWideOf]).
	wide := make([]pushRowWide, len(pushRows))
	for i, e := range pushRows {
		if wide[i], err = pushRowWideOf(e); err != nil {
			return 0, err
		}
	}
	// WHO PUSHED, AS THE HISTORY NAMES THEM (Principal.Actor): an operator's
	// credential beside its bound seat, or a seat's handle as both — with how
	// the push was built, a seat's run behind it and whom it was made for, as
	// the record carries them; a landing's whom as its entry's row keeps it
	// ([Land.pushes]).
	if _, err = store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat,
			via, turn_id, work_backend, work_key, model, run_id, harness, node, atomic, commands,
			pack_id, pack_bytes, introduced, introduced_count, offered, received_at, row_bytes,
			working_for) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, ``,
		len(pushRows), func(i int) []any {
			e := pushRows[i]
			packID, size := "", int64(0)
			if e.Pack != nil {
				packID, size = e.Pack.PackID, e.Pack.Size
			}
			run := e.Principal.Run
			return []any{e.PushID, p.RepoID, at.packed, at.at(), e.Principal.Actor(),
				string(e.Principal.Kind), e.Principal.Seat, string(e.Via), run.TurnID, run.WorkBackend,
				run.WorkKey, run.Model, run.RunID, run.Harness, e.Node, e.Atomic, wide[i].commands, packID, size,
				wide[i].introduced, e.IntroducedCount, e.Offered, store.EncodeTime(e.ReceivedAt), wide[i].bytes,
				wide[i].madeFor}
		}); err != nil {
		return 0, fmt.Errorf("gitlet: write the pushes at %s: %w", at.position, err)
	}
	// THE PUSHES COUNTED IN AND THE HORIZON'S OUT, in one move and apart from rows (record.go).
	activity, err := countPushActivity(ctx, tx, at, p.RepoID, pushRows, gone)
	if err != nil {
		return 0, err
	}
	// THE CONFLICT IS A SKIP, though no record this build applies meets one:
	// an insert aborted on its primary key would stall the log on every node.
	if _, err = store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_ref_drops (repo_id, at, position, ref, old) VALUES `, `(?, ?, ?, ?, ?)`,
		`ON CONFLICT (repo_id, at, position, ref, old) DO NOTHING`,
		len(drops), func(i int) []any {
			return []any{p.RepoID, at.at(), at.packed, drops[i].ref, drops[i].old}
		}); err != nil {
		return 0, fmt.Errorf("gitlet: record the objects the push at %s left: %w", at.position, err)
	}
	// A REMOVAL RAISES THE MARK TO THE RECORD'S OWN POSITION, in the counters'
	// own row: a delta from below it would miss the deletions it removed.
	var mark int64
	if removal {
		mark = at.packed
	}
	// THE IDLE CLOCK MOVES WITH THE LAST PUSH, a refused entry's too — still
	// somebody working there, what a scratch repository expires by
	// (Retention). NOT FOR A RECORD OF EXPIRIES ALONE (M3-decisions M3-O54):
	// the engine deleting a seat's idle branches is housekeeping, so it keeps
	// the last push and idle clock; an expiry batched beside anybody's push
	// moves both. The pushes ever and the version move for every record.
	keep := onlyExpiry(pushRows)
	if _, err = tx.ExecContext(ctx, pushCountersSQL, moved.live, moved.branches, moved.tags,
		moved.tombstones, len(packRows), packBytes, packObjects, len(pushRows), keep, at.at(), keep, at.at(),
		at.packed, mark, p.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: count the pushes at %s: %w", at.position, err)
	}

	swept, err := a.sweepTombstones(ctx, tx, at, p.RepoID, sweep,
		held+moved.tombstones-MaxTombstonesPerRepo)
	if err != nil {
		return 0, err
	}
	// THE DROPS LAST, with what the tombstones left: their mark's row too,
	// where they swept nothing.
	forgotten, err := a.expireRefDrops(ctx, tx, at, p.RepoID, budget-rows-expired-swept)
	if err != nil {
		return 0, err
	}
	return rows + expired + swept + forgotten + tallied + activity, nil
}

// packKind is what added e's pack, as its code_packs row names it: a merge
// record's push its merge's pack ([PackMerge]), and every other push its own
// ([PackPush]). A land record's pushes carry none: what they land is their
// train's pack, which its candidate record added ([PackCandidate]).
func (e PushEntry) packKind() PackKind {
	if e.Via == ViaMerge {
		return PackMerge
	}
	return PackPush
}

// applyChangeMoves writes what the record's unapplied entries, in order, do to
// the live changes their ok commands reach — a head move's revision and its
// pusher as an author, a merge, a close, and the change an entry opens from
// its push's options (revisions.go) — and answers the rows it wrote. Opens
// take the repository's next numbers in their entries' order. It answers
// apart the tally rows counting the changes' states (apply_counts.go), which
// no horizon is given.
func applyChangeMoves(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	pushes []PushEntry) (written, tallied int, err error) {

	book, err := readChangeBook(ctx, tx, repoID, pushes, at.packed, at.at())
	if err != nil {
		return 0, 0, fmt.Errorf("gitlet: the push at %s: %w", at.position, err)
	}
	read := book.statesRead()
	if slices.ContainsFunc(pushes, func(e PushEntry) bool { return e.Change != nil }) {
		if err = tx.QueryRowContext(ctx, nextChangeSQL, repoID).Scan(&book.next); err != nil {
			return 0, 0, fmt.Errorf("gitlet: read %s's next change at %s: %w", repoID, at.position, err)
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
	if written, err = book.write(ctx, tx, at.maxVariables); err != nil {
		return 0, 0, fmt.Errorf("gitlet: the changes the push at %s moves: %w", at.position, err)
	}
	if tallied, err = countChanges(ctx, tx, repoID, book.counted(read)); err != nil {
		return 0, 0, fmt.Errorf("%w (the push at %s)", err, at.position)
	}
	return written, tallied, nil
}

// refDrop is one object a forced update or a deletion moved a ref off
// ([leavesObject]): one code_ref_drops row of the record that applies it, how
// an expunge keeps what such a push left for [PruneGrace] (doc.go's "Five
// horizons") — no ref names it now, and the push history would be 181 days
// of rows to read. One record may leave several objects on one ref, its
// entries moving it in turn: a row each, keyed by the object too, and an
// object it leaves twice is written once.
type refDrop struct{ ref, old string }

// leavesObject reports a command moving its ref off an object nothing it
// writes reaches: a forced update or a deletion — never a create, nor a
// fast-forward, whose new object still reaches its old. A tag never moves
// (RefusalTagMoved), so it leaves one only by its deletion; a forced flag on
// a create, which no receiver sets, leaves none.
func leavesObject(c Command) bool {
	return c.Old != ZeroOID && (c.Forced || c.New == ZeroOID)
}

// expireRefDrops removes, inside the record's own repository and within limit
// rows, its ref drops older than PruneGrace by the record's broker instant,
// and answers how many it removed. limit is whatever the record leaves once
// the push history and the tombstones have had theirs, never a fixed share:
// why, and where it falls behind and catches up, is doc.go's "Five horizons".
// None goes inside its grace: that would cost the object it keeps.
//
// BY THE INSTANT, which code_ref_drops' key leads with ([dropsKey]), then the
// rest of the key: the read seeks the expired rows and stops at the first
// inside the grace, whatever order a leader change stamped the records in,
// and a removal cut at limit cuts every node's rows at one row. A record's
// own drops are never past the grace at its own instant.
func (a *Applier) expireRefDrops(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	limit int) (int, error) {

	if limit <= 0 {
		return 0, nil
	}
	horizon := store.EncodeTime(at.brokerAt.Add(-PruneGrace))
	res, err := tx.ExecContext(ctx, expireDropsSQL, repoID, horizon, limit)
	if err != nil {
		return 0, fmt.Errorf("gitlet: remove %s's ref drops past their grace: %w", repoID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// moves is what the record does to code_repos' counts, kept as the rows move
// so no read COUNTs a namespace (tens of milliseconds at 100 000 refs, on
// every detail read): each named ref from its state before the record (prior,
// absent for a ref with no row) to its state after (final, "" for a
// deletion), exact however many entries moved it between. A deletion is a
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
		return "", fmt.Errorf("gitlet: encode a push's commands: %w", err)
	}
	return string(b), nil
}

// pushRowBytes is what a push row weighs as code_pushes.row_bytes records it:
// the columns that make one row wider than another — its commands as stored,
// its introduced ids, a seat's run (six fields of up to MaxProvenanceBytes)
// and whom it was made for (at most [MaxCarrierBytes]) — because deleting a
// wide row frees overflow pages, which bound the history's horizon and a
// purge part. A push's and a merge's are their record's own, so never more
// than it. A LANDING'S ARE NOT: each row keeps its entry's carrier
// (code_queue.working_for, [Land.pushes]) and names off the change's row —
// at most [MaxLandEntries] rows of two commands, one id and one carrier,
// under a quarter of [MaxRecordBytes] (the carriers 12 800 bytes), which
// HorizonBytesPerRecord bounds as every record's
// (TestAPushMeasuredForItsRecordCountsWhomItWasMadeFor).
func pushRowBytes(commands, introduced string, run RunProvenance, workingFor string) int {
	return len(commands) + len(introduced) + len(run.TurnID) + len(run.WorkBackend) + len(run.WorkKey) +
		len(run.Model) + len(run.RunID) + len(run.Harness) + len(workingFor)
}

// pushRowWide is the wide columns of one push row as the apply writes them —
// its commands as stored, its introduced ids space-separated, and whom its
// push was made for — and what the row weighs ([pushRowBytes]).
type pushRowWide struct {
	commands, introduced string
	madeFor              sql.NullString
	bytes                int
}

// pushRowWideOf is e's push row's wide columns and weight: the one reading of
// them every push row is written by, a landing's among them ([Land.pushes]).
func pushRowWideOf(e PushEntry) (pushRowWide, error) {
	commands, err := recordedCommands(e.Commands)
	if err != nil {
		return pushRowWide{}, err
	}
	madeFor, err := carrierColumn(e.WorkingFor)
	if err != nil {
		return pushRowWide{}, err
	}
	introduced := strings.Join(e.Introduced, " ")
	return pushRowWide{commands: commands, introduced: introduced, madeFor: madeFor,
		bytes: pushRowBytes(commands, introduced, e.Principal.Run, madeFor.String)}, nil
}

// countSeatDays counts each SEAT'S entry the apply inserts on the company day
// its decide stamped ([PushEntry.Day]) in code_seat_days, which the decide
// reads pushes_per_seat_day from, and forgets each counted seat's oldest day
// beyond its two newest, in two statements a record, answering the rows it
// touched. The day is the decide's and "old" is read off the rows — never a
// node's clock or a calendar a zone skips dates in — so every node agrees.
//
// TWO DAYS: committers' clocks may straddle the company's midnight (within
// twice objstore.AssumedClockSkew while no node raises clock_skew), and one
// still on the older day must find its whole count. ONE forgotten, as a
// record's entries carry one day ([seatQuota]) and the committer bounds an
// entry at [seatDayRows], keeping the tombstone sweep's rows.
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
		return 0, fmt.Errorf("gitlet: count the seats' pushes at %s: %w", at.position, err)
	}
	list, err := jsonList(seats)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, forgetSeatDaysSQL, list)
	if err != nil {
		return 0, fmt.Errorf("gitlet: forget the seats' older days at %s: %w", at.position, err)
	}
	forgotten, _ := res.RowsAffected()
	return len(counted) + int(forgotten), nil
}

// expirePushes removes, inside the record's own repository and within budget
// (the rows the record leaves of MaxRecordRows), its push rows older than
// PushHistory by the record's broker instant — at most HorizonDeletesPerRecord,
// and no more once their bytes reach HorizonBytesPerRecord — answering how
// many, and what as the activity counts it ([countPushActivity]).
//
// OLDEST BY POSITION (code_pushes_repo_idx), STOPPING AT THE FIRST INSIDE THE
// HORIZON, so the read costs the rows it removes and one more. NEVER A FILTER
// ON THE INSTANT: the index orders by position, so `at < ?` would walk 181
// days of pushes on every record finding few expired. A row stamped earlier
// than the one before it waits for that one (doc.go's "Five horizons").
func (a *Applier) expirePushes(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	budget int) (int, pushActivity, error) {

	limit := min(HorizonDeletesPerRecord, budget)
	if limit <= 0 {
		return 0, pushActivity{}, nil
	}
	horizon := store.EncodeTime(at.brokerAt.Add(-PushHistory))
	expired, _, _, err := pushesWithin(ctx, tx, HorizonBytesPerRecord,
		func(instant int64) bool { return instant >= horizon }, expiredPushesSQL, repoID, limit)
	if err != nil {
		return 0, pushActivity{}, fmt.Errorf("gitlet: read %s's expired pushes: %w", repoID, err)
	}
	if len(expired) == 0 {
		return 0, pushActivity{}, nil
	}
	n, gone, err := deletePushes(ctx, tx, expired)
	if err != nil {
		return 0, pushActivity{}, fmt.Errorf("gitlet: delete %s's expired pushes: %w", repoID, err)
	}
	return n, gone, nil
}

// sweepTombstones removes, inside the record's own repository and within limit
// rows, its tombstones oldest first — by deletion instant, then ref — each
// among the first excess (how far the record left the count past
// MaxTombstonesPerRepo) or older than RefTombstoneHorizon by the record's
// broker instant, stopping at the first that is neither; and raises
// tombstones_through to the highest position it swept, so a delta from below
// it is answered in full, answering the rows it wrote, tombstones and mark.
// Both reasons read code_refs_tombstones_idx's one order: one seek, and one
// row when nothing is due. Oldest first, since a forgotten deletion sends only
// a cache synced before it to a full answer (rows, never a download). limit is
// all the record leaves, never a fixed share (doc.go's "Five horizons"), and
// excess never passes it: [Applier.landPushes] writes removals instead.
func (a *Applier) sweepTombstones(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	limit, excess int) (int, error) {

	if limit <= 0 {
		return 0, nil
	}
	aged := store.EncodeTime(at.brokerAt.Add(-RefTombstoneHorizon))
	rows, err := tx.QueryContext(ctx, tombstonesOldestSQL, repoID, limit)
	if err != nil {
		return 0, fmt.Errorf("gitlet: read %s's oldest tombstones: %w", repoID, err)
	}
	var stones []string
	var through int64
	for rows.Next() {
		var ref string
		var position, deleted int64
		if scanErr := rows.Scan(&ref, &position, &deleted); scanErr != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("gitlet: read %s's oldest tombstones: %w", repoID, scanErr)
		}
		if len(stones) >= excess && deleted >= aged {
			break
		}
		stones = append(stones, ref)
		through = max(through, position)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("gitlet: read %s's oldest tombstones: %w", repoID, err)
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
		return 0, fmt.Errorf("gitlet: delete %s's swept tombstones: %w", repoID, err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, sweptTombstonesSQL, through, n, repoID); err != nil {
		return 0, fmt.Errorf("gitlet: raise %s's tombstone mark: %w", repoID, err)
	}
	return int(n) + 1, nil
}

// applyPurgePart deletes one bounded part of a deleted repository's rows in a
// fixed order, so every node deletes the same rows: pushes, refs, packs,
// retired packs, ref drops, changes each whole ([purgeChanges]), checks
// ([purgeChecks]), dispatches and readings ([purgeDispatches]), merge queues
// ([purgeQueues]). THE PART IS THE GUARD: only purge_parts + 1 deletes, so a
// redelivered or stale part deletes nothing more. At most PurgePartRows rows,
// and wide rows (pushes, change bodies and conversations, checks) only until
// they weigh PurgePartBytes, one budget, since a count misses their overflow
// pages; a part whose pushes were cut by bytes deletes nothing else. Retired
// packs and ref drops go whatever their grace (doc.go's "Five horizons"), as
// code.deleted_retention_days outlasts reads begun before. Deleted pushes
// leave the activity too, a part finding none taking the rest (apply_activity.go).
func (a *Applier) applyPurgePart(ctx context.Context, tx *sql.Tx, at applyContext, p PurgePart) (int, error) {
	var parts int
	if err := tx.QueryRowContext(ctx, purgePartsSQL, p.RepoID).Scan(&parts); err != nil {
		return 0, fmt.Errorf("gitlet: read %s's purge at %s: %w", p.RepoID, at.position, err)
	}
	if p.Part != parts+1 {
		return 0, nil
	}
	// ITS REPOSITORY TALLY ROW moves on the first part, deleted to purging, beside the
	// part's own rows and never charged to its budget.
	before, err := tallyStandingIn(ctx, tx, p.RepoID)
	if err != nil {
		return 0, err
	}
	budget := PurgePartRows
	deleted := 0

	pushIDs, weighed, cut, err := pushesWithin(ctx, tx, PurgePartBytes, nil, purgePushesSQL, p.RepoID, budget)
	if err != nil {
		return 0, fmt.Errorf("gitlet: read %s's pushes to purge: %w", p.RepoID, err)
	}
	if len(pushIDs) > 0 {
		n, counted, deleteErr := deletePushes(ctx, tx, pushIDs)
		if deleteErr != nil {
			return 0, fmt.Errorf("gitlet: purge %s's pushes: %w", p.RepoID, deleteErr)
		}
		moved, moveErr := moveActivity(ctx, tx, at.maxVariables, p.RepoID, counted.negated())
		if moveErr != nil {
			return 0, moveErr
		}
		deleted += n + moved
		budget -= len(pushIDs)
	} else {
		left, leftErr := purgeLeftActivity(ctx, tx, p.RepoID)
		if leftErr != nil {
			return 0, leftErr
		}
		deleted += left
	}
	if cut {
		budget = 0
	}

	var refs []string
	var gone refCounts
	if budget > 0 {
		rows, err := tx.QueryContext(ctx, purgeRefsSQL, p.RepoID, budget)
		if err != nil {
			return 0, fmt.Errorf("gitlet: read %s's refs to purge: %w", p.RepoID, err)
		}
		for rows.Next() {
			var ref, target string
			if err := rows.Scan(&ref, &target); err != nil {
				_ = rows.Close()
				return 0, fmt.Errorf("gitlet: read %s's refs to purge: %w", p.RepoID, err)
			}
			refs = append(refs, ref)
			gone.count(ref, target, +1)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("gitlet: read %s's refs to purge: %w", p.RepoID, err)
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
			return 0, fmt.Errorf("gitlet: purge %s's refs: %w", p.RepoID, err)
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
			return 0, fmt.Errorf("gitlet: read %s's packs to purge: %w", p.RepoID, err)
		}
		for rows.Next() {
			var id, state string
			var size, objects int64
			if err := rows.Scan(&id, &size, &objects, &state); err != nil {
				_ = rows.Close()
				return 0, fmt.Errorf("gitlet: read %s's packs to purge: %w", p.RepoID, err)
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
			return 0, fmt.Errorf("gitlet: read %s's packs to purge: %w", p.RepoID, err)
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
			return 0, fmt.Errorf("gitlet: purge %s's packs: %w", p.RepoID, err)
		}
		n, _ := res.RowsAffected()
		// ONE PART A PACK is what makes deleting by pack id exact: every
		// pack this build stores is one bundle, one row. A pack split
		// across rows would be deleted whole here while the budget counted
		// one of them — a writer's bug, refused rather than miscounted.
		if int(n) != len(packIDs) {
			return 0, fmt.Errorf("gitlet: purging %s deleted %d pack rows for %d packs — "+
				"a pack this build stores is one row", p.RepoID, n, len(packIDs))
		}
		deleted += int(n)
		budget -= len(packIDs)
	}
	if budget > 0 {
		res, err := tx.ExecContext(ctx, purgeRetiredSQL, p.RepoID, budget)
		if err != nil {
			return 0, fmt.Errorf("gitlet: purge %s's retired packs: %w", p.RepoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
		budget -= int(n)
	}
	if budget > 0 {
		res, err := tx.ExecContext(ctx, purgeDropsSQL, p.RepoID, budget)
		if err != nil {
			return 0, fmt.Errorf("gitlet: purge %s's ref drops: %w", p.RepoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
		budget -= int(n)
	}
	if budget > 0 {
		// AND THEIR NOTICES, charged by each change's count of them: the
		// table is divergent, and what the part charges decides which
		// replicated rows go after them (apply_purge_changes.go).
		done, err := a.purgeChanges(ctx, tx, at, p.RepoID, budget, weighed)
		if err != nil {
			return 0, err
		}
		deleted += done.deleted
		budget -= done.spent
		weighed = done.weighed
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
		return 0, fmt.Errorf("gitlet: count %s's purge at %s: %w", p.RepoID, at.position, err)
	}
	moved, tallyErr := retally(ctx, tx, p.RepoID, before)
	if tallyErr != nil {
		return 0, tallyErr
	}
	return deleted + 1 + moved, nil
}

// applyPurge removes what remains of a repository and writes its deletion
// marker — THE GATE — whatever the row's status (the decide is the guard) and
// idempotently: its own record redelivered deletes and inserts nothing. Its
// statements are unbounded because the decide forms it only once what remains
// fits one part ([applyPurgePart]) beside MaxProtectionsPerRepo rules,
// MaxPathOwners owners and MaxPublishedLayers × (1 + MaxLayerParts) layer
// rows, plus 6 (name claim, row, kind count, tally row, viewed key, marker),
// which the limits test holds within MaxRecordRows. A change's rows go before
// the change, through which alone they are found. A rename's old names are
// released by the code-retention pass at their window's end
// ([Store.ReleaseName]), as nothing a part counts bounds them; no part removes
// a layer, which harms nothing in a repository that serves nothing.
func (a *Applier) applyPurge(ctx context.Context, tx *sql.Tx, at applyContext, p Purge) (int, error) {
	// THE NOTICES OF THE CHANGES THE PURGE STILL FINDS, before their rows
	// go: the only way to them from the repository is its changes. They
	// are bounded with the rest, since the decide forms this record only
	// once the rows left and the changes' counts of their notices fit one
	// part (purge.go).
	remaining, err := changesOf(ctx, tx, p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: read %s's remaining changes at %s: %w", p.RepoID, at.position, err)
	}
	deleted, err := a.removeNotices(ctx, tx, at, remaining)
	if err != nil {
		return 0, err
	}
	for _, stmt := range []string{
		purgeAllPushesSQL, purgeAllRefsSQL, purgeAllPacksSQL, purgeAllRetiredSQL, purgeAllDropsSQL,
		purgeAllPushDaysSQL, purgeAllPusherDaysSQL,
		purgeAllRevisionsSQL, purgeAllAuthorsSQL, purgeAllReviewersSQL, purgeAllParticipantsSQL,
		purgeAllFindingsSQL, purgeAllCommentsSQL, purgeAllFindingTextsSQL, purgeAllReviewsSQL,
		purgeAllFactsSQL, purgeAllBodiesSQL, purgeAllMergeabilitySQL, purgeAllConversationsSQL,
		purgeAllChangesSQL, purgeAllCheckLogsSQL, purgeAllChecksSQL, purgeAllDispatchesSQL, purgeAllCheckTargetsSQL,
		purgeAllQueueSQL, purgeAllCandidatesSQL, purgeAllLandingsSQL, purgeAllGreenSQL, purgeAllTipsDueSQL,
		purgeAllLayerPartsSQL, purgeAllLayersSQL, clearProtectionsSQL, clearPathOwnersSQL, purgeViewedKeySQL,
	} {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		res, err := tx.ExecContext(ctx, stmt, p.RepoID)
		if err != nil {
			return 0, fmt.Errorf("gitlet: purge %s at %s: %w", p.RepoID, at.position, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
	}
	// THE RECORD STATES THE NAME IT RELEASES, and only a claim naming THIS
	// repository is released: a claim another repository holds on the same
	// token is not this purge's to remove.
	res, err := tx.ExecContext(ctx, releaseNameSQL, NameToken(p.Name), p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: release %q at %s: %w", p.Name, at.position, err)
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
		return 0, fmt.Errorf("gitlet: read %s's kind at %s: %w", p.RepoID, at.position, err)
	}
	before, err := tallyStandingIn(ctx, tx, p.RepoID)
	if err != nil {
		return 0, err
	}
	if res, err = tx.ExecContext(ctx, removeRepoSQL, p.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: remove %s at %s: %w", p.RepoID, at.position, err)
	}
	n, _ = res.RowsAffected()
	deleted += int(n)
	// AND COUNTED OUT OF ITS REPOSITORY TALLY ROW, pruned where it was the key's last.
	moved, err := retally(ctx, tx, p.RepoID, before)
	if err != nil {
		return 0, err
	}
	deleted += moved
	if n > 0 {
		if res, err = tx.ExecContext(ctx, uncountKindSQL, kind); err != nil {
			return 0, fmt.Errorf("gitlet: count %s out at %s: %w", p.RepoID, at.position, err)
		}
		n, _ = res.RowsAffected()
		deleted += int(n)
	}
	if res, err = tx.ExecContext(ctx, markSQL, p.RepoID, p.Name, at.at(), at.record.Actor,
		p.Reason, at.record.OpID, at.packed); err != nil {
		return 0, fmt.Errorf("gitlet: mark %s purged at %s: %w", p.RepoID, at.position, err)
	}
	n, _ = res.RowsAffected()
	return deleted + int(n), nil
}

// pushesApplied is the push ids among ids that the history holds already: a
// push landed ([pushesAppliedSQL]).
func pushesApplied(ctx context.Context, tx *sql.Tx, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, pushesAppliedSQL, list)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// pushesWithin reads the push ids query answers with their row_bytes, in its
// order, keeping them until their bytes reach bytes — always the first, so a
// step always advances and runs at most one row past — and answers what the
// kept rows weigh, the rest of which a purge part's changes take
// ([purgeChanges]). cut reports a row left unkept: the weight bounded the
// read, not the query's LIMIT. Every node keeps the same ids.
//
// stop, where given, ends the read at the first row it holds for, read off a
// THIRD column the query then selects, keeping that row out and reporting no
// cut: how the push-history horizon ends at its first push inside it
// ([Applier.expirePushes]). The purge passes none.
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

// onlyExpiry reports a record whose every entry it applies is the ref
// lifecycle's expiry ([ViaExpiry]) — the one record whose apply keeps the
// repository's activity clocks: false as soon as any other way is among them.
func onlyExpiry(entries []PushEntry) bool {
	for _, e := range entries {
		if e.Via != ViaExpiry {
			return false
		}
	}
	return len(entries) > 0
}

// seatOf is the seat a ref's row names (code_refs.seat) once p's push set it
// to target: the pushing seat where an AGENT principal — its coding box, or an
// engine write in its turn — set the ref INSIDE ITS OWN NAMESPACE
// ([SeatNamespace]), and "" for every other write: a person's or a token's
// push, a deletion, and a seat's push to a branch it was granted, never its
// own to lose.
//
// BY THE PRINCIPAL'S KIND, NEVER ITS SEAT: a person's principal names their
// human seat too (ADR-0032), and a person pushing under
// refs/heads/seats/<their handle>/ is no seat's push of its own branch.
func seatOf(p Principal, ref, target string) string {
	if target == "" || p.Kind != PrincipalAgent || p.Seat == "" ||
		!strings.HasPrefix(ref, SeatNamespace(p.Seat)) {
		return ""
	}
	return p.Seat
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
		last_push_at = CASE WHEN ? THEN last_push_at ELSE ? END,
		last_activity_at = CASE WHEN ? THEN last_activity_at ELSE ? END,
		version = ?, tombstones_through = MAX(tombstones_through, ?)
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
		WHERE push_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
		RETURNING at, via, actor_kind, actor_seat`
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
	// purgeViewedKeySQL deletes the key the repository's viewed marks were
	// digested under: those marks are each person's and stay, naming no file
	// once the log is trimmed past the key's record.
	purgeViewedKeySQL = `DELETE FROM code_viewed_keys WHERE repo_id = ?`
)
