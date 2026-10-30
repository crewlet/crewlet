package code

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// A CHANGE'S REVISIONS ARE DERIVED FROM THE PUSHES THAT MOVE IT, IN THE PUSH'S
// OWN RECORD: no record of a change's own says its head moved, because the
// push that moved its source branch already says so, and the apply of that
// push writes what it did to every live change it reached — pure over the
// record and the change rows it finds, so every data node derives the same
// rows. A push record carries its entries in the order they were decided, and
// each entry moves the changes its OK BRANCH COMMANDS reach, seeing what the
// entries before it did, in three steps:
//
//  1. A HEAD MOVE: a command that moves a live change's source branch to
//     another commit writes the change's next revision (code_revisions: n + 1,
//     the new head, who pushed it and the push that did), moves the change's
//     head and revision, and makes the pusher one of its authors from that
//     revision — a pusher already one, its owner among them, stays as they
//     were. A command never sets a ref to where it is (validCommands), and a
//     live change's head is its source's — its open and its reopen read it
//     there, and every push that moves the source moves it — so every such
//     command moves the head.
//  2. A MERGE: a command that sets a branch to exactly the head of a live
//     change whose target it is — after the push's own head moves, so a push
//     that moves a branch and its target to one commit merges it whatever
//     order git sent them in — marks the change merged, by push, at that
//     commit. At most [MaxPushMerges] a record, the changes with the lowest
//     numbers first; a change past it stays open with its head in its target.
//  3. A CLOSE: a command that deletes a live change's source branch closes the
//     change, its reason [SourceDeleted] — after the push's merges, so a push
//     that merges a change and deletes its branch leaves it merged.
//  4. AN OPEN: an entry carrying the change its decide opened from the push's
//     options ([PushEntry.Change], pushchange.go) opens it on its one branch
//     command, at the head that command set — after the entry's own moves,
//     so the push's move is the change's FIRST revision, made by the push,
//     rather than a second — numbered the repository's next, its pusher its
//     owner and first author ([changeBook.open]); the record's later entries
//     revise, merge or close it as any live change.
//
// A CHANGE A RECORD MOVES LEAVES ITS QUEUE: its live queue entry, where its
// target's queue holds one (queue.go), is EJECTED head_moved by a move of its
// head — the queue would otherwise test a head nobody enqueued — and DEQUEUED
// by its merge or its close, in the push's own record, one row an entry, which
// the book counts with the rest ([changeBook.rowCount]).
//
// Every change a record moves counts ONE VERSION and takes the record's
// position, however many of its entries moved it: a change's version counts
// the records that changed its row, which a write of its lifecycle states as
// its expectation, so a push that moved its head stales a write formed before
// it — and a change a record opens is at version 1 however many of its later
// entries move it, the opening record being its first. Nothing else moves a
// change: a push to a ref that is neither a live change's source nor the
// target its head is set to concerns no change.
//
// # One model, which the decide counts by and the apply writes from
//
// [changeBook] is the live changes a record can move and the authors its
// pushers already are of them, read in three statements — the changes its
// branch commands name as sources, the changes whose targets and heads its
// commands set, and the authors among its pushers of the changes whose heads
// they move — and moved by each entry in the record's order. The push decide
// moves it by each entry it admits, so it counts EXACTLY the rows the
// record's apply will write about changes beside its entries' own — the apply
// reads the same rows, which the broker's arbitration on the repository's
// subject guarantees, and writes what the same model makes of the same
// entries — and holds the record to [MaxRecordRows] with them. It tries each
// entry on the book itself, journaled, and takes back one the record cannot
// hold ([bookTrial]): what an entry costs a round is what its own commands
// reach. A push whose changes' rows would take its record past the bound
// beside the entries before it is CARRIED, decided by none of its batch's
// rounds, to lead the committer's next batch; one whose rows would take even
// a record of its own past it is refused [RefusalTooManyChanges].
//
// # The record's format does not move
//
// What a push record's commands do to a change — a revision, a merge, a close
// — is derived from the commands it already carries, and the one thing it
// carries for a change, the change a push opens from its options, is a field
// with a stated condition of its own ([PushEntry.Change]). Under the code
// domain's stated condition —
// no build that carries the code domain has run without these rows, since none
// of the domain is on main — every build that meets a push record derives
// them. Once a build without them has run, a push record such a build would
// apply short of a change's rows must say so: the committer would stamp each
// entry whose commands move a live change with how many it moves, and the push
// op would take {Name: "PushEntry.Changes", Since: 2, Op: "push", Path:
// []string{"mutation", "pushes", "changes"}} in versionedFields, with
// RecordVersion at 2, its line under "What each version added" and a Carrying
// record in the statelogtest candidate, so such a build retains such a record
// rather than apply its refs without its changes.

// SourceDeleted is the close reason (code_changes.close_reason) of a change a
// push closed by deleting its source branch — the one reason no person wrote.
const SourceDeleted = "source_deleted"

// MaxPushMerges is the most changes one push record marks merged: MANY LIVE
// CHANGES AT ONE HEAD INTO ONE TARGET IS NO PUSH'S ORDINARY WORK — each heads a
// branch of its own, so it takes as many branches pointing at one commit, each
// with a change open — and a record's rows are bounded, so a push to a target
// is never refused for the changes it would merge. Sixty-four, as many as the
// pushes one record carries ([MaxPushesPerRecord]): one each for a full batch
// of pushes that each merge their own change, sixty-four rows of the record's
// [MaxRecordRows] at most. A change past it, in number order, is left open
// with its head in its target — reached by nothing in this build, and by the
// repository's home git node once its change work lands what such a change
// holds.
const MaxPushMerges = MaxPushesPerRecord

// changeBook is the live changes a push record's commands can move, and the
// authors its pushers already are of them, as the record's entries so far
// leave them: what the push decide counts the record's change rows by, and
// what the push apply writes them from.
type changeBook struct {
	// repo is the repository the record is about, which a change it opens
	// is of.
	repo string
	// packed and at are the record's position and broker instant, which every
	// row it writes carries — zero where the decide counts and writes nothing.
	packed, at int64
	// next is the number the record's next open gives its change — the
	// repository's next_change as the apply reads it, moved on by each open —
	// and zero in the decide, which counts opens without numbering them: a
	// number is the apply's to give ([Applier.applyChangeOpen]'s rule).
	next int64
	// rows is every change read, by id, as the entries so far moved it.
	rows map[string]*changeRow
	// bySource is the live change each branch heads, and byTarget the
	// changes read into each branch, in number order.
	bySource map[string]string
	byTarget map[string][]string
	// authored is every pair of a change and an author it holds that the
	// record's head moves can reach: the ones read, and the ones the entries
	// so far added.
	authored map[[2]string]bool
	// queued is every change read that holds a live queue entry, which the
	// record ends where it moves the change.
	queued map[string]bool
	// writes is what the entries so far make the record's apply write.
	writes bookWrites
	// trial is what the entry the decide is trying has done to the book so
	// far ([changeBook.try]), kept to take it back; nil while no entry is on
	// trial — and always in the apply, which writes every entry it meets.
	trial *bookTrial
}

// bookWrites is what a record's entries make its apply write about changes:
// the rows a decide counts and the apply writes ([changeBook.rowCount]).
type bookWrites struct {
	// opened is every change the entries opened ([changeBook.open]), whose
	// row the apply INSERTS where it writes every other back; and bodies the
	// descriptions of those that carry one.
	opened map[string]bool
	bodies []bodyRow
	// touched is every change the entries moved, in the order first moved —
	// the order the apply writes them in — and isTouched the same changes as
	// a set, since a push that moves hundreds asks it once a move.
	touched   []string
	isTouched map[string]bool
	// revisions are the revisions the entries wrote, in order, and authors
	// the authors they added, each pair of a change and an author once.
	revisions []revisionRow
	authors   []authorRow
	// merges is how many changes the entries merged, and merged which, in
	// the order merged — what a push decide routes each entry's merges by.
	merges int
	merged []string
}

// noWrites is a record's writes before its first entry.
func noWrites() bookWrites {
	return bookWrites{opened: map[string]bool{}, isTouched: map[string]bool{}}
}

// bookTrial is what one entry on trial did to a [changeBook] — everything it
// changed, each as it was before — so that taking the entry back leaves the
// book exactly as the entry found it.
//
// # Why a journal, never a copy
//
// The decide reads ONE book a round for its whole batch — every live change
// any of its commands can reach — and tries each entry on it before it knows
// the record can hold what the entry writes. A copy of the book an entry
// cost every change the BATCH reached, on every entry and on every
// compare-and-set round of the repository's one committer goroutine, while it
// holds a connection of the reader pool: a push moving a thousand change
// branches beside sixty-three one-command pushes copied sixty-four thousand
// rows a round — 79–80 ms and 64 MB, against 47–50 ms and 11 MB tried in
// place (doc.go's measured section). The journal holds only what the entry's
// own commands reached, so an entry costs what it moves — nothing at all for
// one that reaches no change.
type bookTrial struct {
	// touched, bodies, revisions, authors and merges are the record's writes'
	// lengths and count as the entry found them, and next the book's next
	// number: what taking it back cuts them to.
	touched, bodies, revisions, authors, merges int
	next                                        int64
	// moved is every change the entry moved, by id, as it was before the
	// entry first moved it; and added every change it opened, which taking
	// it back forgets.
	moved map[string]changeRow
	added []string
	// authored is every pair of a change and an author the entry added.
	authored [][2]string
	// sources and targets are what its opens pointed at their changes, each
	// as it was, in the order set: a source's change ([changeBook.bySource]),
	// and a target's changes by how many it held ([changeBook.byTarget]).
	sources []sourceWas
	targets []targetWas
}

// sourceWas is the change the book named for a branch before an entry's open
// named its own there: id, where held says it named one.
type sourceWas struct {
	source, id string
	held       bool
}

// targetWas is how many changes a branch was the target of before an entry's
// open added one, where held says the book named it at all.
type targetWas struct {
	target string
	n      int
	held   bool
}

// revisionRow is one code_revisions row a record writes.
type revisionRow struct {
	change string
	n      int64
	head   string
	pusher string
	kind   PartyKind
	pushID string
}

// authorRow is one code_change_authors row a record writes.
type authorRow struct {
	change, author string
	kind           PartyKind
	first          int64
}

// bodyRow is one code_change_bodies row a record writes: the description of a
// change it opened.
type bodyRow struct{ change, body string }

// readChangeBook reads the live changes of repoID that the commands of entries
// the decide has not refused can move — the change each branch they name
// heads, and every live change whose target they set to exactly its head —
// and the authors each entry's pusher already is of the changes whose heads
// it moves, into a book whose rows carry packed and at once moved. A command
// that is no branch's moves no change, so it is read for none.
func readChangeBook(ctx context.Context, tx *sql.Tx, repoID string, entries []PushEntry, packed, at int64) (
	*changeBook, error) {

	b := &changeBook{repo: repoID, packed: packed, at: at, rows: map[string]*changeRow{},
		bySource: map[string]string{}, byTarget: map[string][]string{}, authored: map[[2]string]bool{},
		queued: map[string]bool{}, writes: noWrites()}
	var sources []string
	var pairs [][2]string
	seenSource, seenPair := map[string]bool{}, map[[2]string]bool{}
	for _, e := range entries {
		for _, c := range e.Commands {
			if c.Result == ResultNG || !strings.HasPrefix(c.Ref, branchPrefix) {
				continue
			}
			if !seenSource[c.Ref] {
				seenSource[c.Ref] = true
				sources = append(sources, c.Ref)
			}
			if pair := [2]string{c.Ref, c.New}; c.New != ZeroOID && !seenPair[pair] {
				seenPair[pair] = true
				pairs = append(pairs, pair)
			}
		}
	}
	if err := inChunks(sources, func(part []string) error {
		return b.read(ctx, tx, liveBySourceSQL, repoID, part)
	}); err != nil {
		return nil, fmt.Errorf("code: read the live changes a push's branches head: %w", err)
	}
	if err := inChunks(pairs, func(part [][2]string) error {
		return b.read(ctx, tx, liveByTargetSQL, repoID, part)
	}); err != nil {
		return nil, fmt.Errorf("code: read the live changes a push's targets are set to the heads of: %w", err)
	}
	// EACH TARGET'S CHANGES IN NUMBER ORDER, the order a record merges them
	// in: what bounds the merges ([MaxPushMerges]) picks the same ones on
	// every node.
	for _, ids := range b.byTarget {
		slices.SortFunc(ids, func(x, y string) int { return cmp.Compare(b.rows[x].number, b.rows[y].number) })
	}
	// THE AUTHORS THE PUSHERS ALREADY ARE of the changes whose heads they
	// move, so the book adds only an author a change does not hold: a row
	// the record writes, counted, rather than a conflict its insert skips.
	var held [][2]string
	seenHeld := map[[2]string]bool{}
	for _, e := range entries {
		pusher, _ := e.Principal.party()
		if pusher == "" {
			continue
		}
		for _, c := range e.Commands {
			id, heads := b.bySource[c.Ref]
			if pair := [2]string{id, pusher}; heads && c.Result != ResultNG && c.New != ZeroOID && !seenHeld[pair] {
				seenHeld[pair] = true
				held = append(held, pair)
			}
		}
	}
	if err := inChunks(held, func(part [][2]string) error { return b.readAuthors(ctx, tx, part) }); err != nil {
		return nil, fmt.Errorf("code: read the authors a push's pushers are of the changes it moves: %w", err)
	}
	// THE CHANGES THAT HOLD A LIVE QUEUE ENTRY, of those read: a move of
	// one ends its entry, a row the record writes, counted.
	ids := make([]string, 0, len(b.rows))
	for id := range b.rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if err := inChunks(ids, func(part []string) error { return b.readQueued(ctx, tx, part) }); err != nil {
		return nil, fmt.Errorf("code: read which changes a push moves are queued: %w", err)
	}
	return b, nil
}

// readQueued marks every change of ids that holds a live queue entry.
func (b *changeBook) readQueued(ctx context.Context, tx *sql.Tx, ids []string) error {
	encoded, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, queuedChangesSQL, string(encoded))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		b.queued[id] = true
	}
	return rows.Err()
}

// read adds the changes one statement answers for list to the book, each once.
func (b *changeBook) read(ctx context.Context, tx *sql.Tx, query, repoID string, list any) error {
	encoded, err := json.Marshal(list)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, query, repoID, string(encoded))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		row, err := scanChange(rows.Scan)
		if err != nil {
			return err
		}
		if _, held := b.rows[row.id]; held {
			continue
		}
		b.rows[row.id] = &row
		b.bySource[row.source] = row.id
		b.byTarget[row.target] = append(b.byTarget[row.target], row.id)
	}
	return rows.Err()
}

// readAuthors marks every pair of a change and an author pairs names that the
// change holds.
func (b *changeBook) readAuthors(ctx context.Context, tx *sql.Tx, pairs [][2]string) error {
	encoded, err := json.Marshal(pairs)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, heldAuthorsSQL, string(encoded))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return err
		}
		b.authored[pair] = true
	}
	return rows.Err()
}

// try puts the decide's next entry on trial: what its moves and its open do
// to the book from here is journaled ([bookTrial]), so that [changeBook.takeBack]
// can undo it, until [changeBook.keep] admits it.
func (b *changeBook) try() {
	w := &b.writes
	b.trial = &bookTrial{touched: len(w.touched), bodies: len(w.bodies), revisions: len(w.revisions),
		authors: len(w.authors), merges: w.merges, next: b.next}
}

// keep admits the entry on trial: its moves are the record's.
func (b *changeBook) keep() { b.trial = nil }

// takeBack undoes everything the entry on trial did, leaving the book exactly
// as [changeBook.try] found it — every change it moved as it was, every one it
// opened forgotten, and the record's writes cut back to what they held —
// at the cost of what the entry reached.
func (b *changeBook) takeBack() {
	t, w := b.trial, &b.writes
	b.trial = nil
	for id, was := range t.moved {
		*b.rows[id] = was
	}
	for _, id := range t.added {
		delete(b.rows, id)
	}
	for _, pair := range t.authored {
		delete(b.authored, pair)
	}
	for i := len(t.sources) - 1; i >= 0; i-- {
		if s := t.sources[i]; s.held {
			b.bySource[s.source] = s.id
		} else {
			delete(b.bySource, s.source)
		}
	}
	for i := len(t.targets) - 1; i >= 0; i-- {
		if was := t.targets[i]; was.held {
			b.byTarget[was.target] = b.byTarget[was.target][:was.n]
		} else {
			delete(b.byTarget, was.target)
		}
	}
	// THE CHANGES THE ENTRY FIRST MOVED are the record's writes past their
	// length before it — the ones it opened among them.
	for _, id := range w.touched[t.touched:] {
		delete(w.isTouched, id)
		delete(w.opened, id)
	}
	w.touched, w.bodies = w.touched[:t.touched], w.bodies[:t.bodies]
	w.revisions, w.authors, w.merges, b.next = w.revisions[:t.revisions], w.authors[:t.authors], t.merges, t.next
	w.merged = w.merged[:t.merges]
}

// alone is the rows e — and the change it opens, where it carries one — would
// make its record write about changes, and how many it moves, as the first
// entry of a record of its own once the book's record has landed: the
// changes as that record leaves them, the ones it opened among them as rows
// the next record finds, and none of its writes. What the decide weighs a
// push it cannot fit beside the entries before it by. It leaves the book as
// it found it, its record's writes and every change.
func (b *changeBook) alone(e PushEntry) (rows, moved int) {
	record := b.writes
	b.writes = noWrites()
	b.try()
	b.move(e)
	if e.Change != nil {
		b.open(e)
	}
	rows, moved = b.rowCount(), b.moved()
	b.takeBack()
	b.writes = record
	return rows, moved
}

// rowCount is the rows the book's moves make the record's apply write: every
// change it moved or opened, every revision and every author it added, the
// description of every change it opened that carries one, and the live queue
// entry of every change it moved that holds one. The repository's own row,
// whose next number an open moves, is the record's already ([recordRows]).
func (b *changeBook) rowCount() int {
	w := &b.writes
	return len(w.touched) + len(w.revisions) + len(w.authors) + len(w.bodies) + b.queuedMoved()
}

// queuedMoved is how many of the changes the book's moves reached hold a live
// queue entry — none of those it opened, which no queue holds yet.
func (b *changeBook) queuedMoved() int {
	n := 0
	for _, id := range b.writes.touched {
		if b.queued[id] && !b.writes.opened[id] {
			n++
		}
	}
	return n
}

// moved is how many changes the book's moves reached.
func (b *changeBook) moved() int { return len(b.writes.touched) }

// touch counts change id moved by this record — one version, at the record's
// position, however many entries move it — and answers its row for the move
// to change, saved first for an entry on trial.
func (b *changeBook) touch(id string) *changeRow {
	row := b.rows[id]
	if t := b.trial; t != nil {
		if _, saved := t.moved[id]; !saved {
			if t.moved == nil {
				t.moved = map[string]changeRow{}
			}
			t.moved[id] = *row
		}
	}
	if w := &b.writes; !w.isTouched[id] {
		w.isTouched[id] = true
		w.touched = append(w.touched, id)
		row.version++
		row.position = b.packed
	}
	return row
}

// author adds pair, a change and an author it did not hold, journaled for an
// entry on trial.
func (b *changeBook) author(pair [2]string) {
	b.authored[pair] = true
	if b.trial != nil {
		b.trial.authored = append(b.trial.authored, pair)
	}
}

// liveOn is the live change source heads, and false for none.
func (b *changeBook) liveOn(source string) (*changeRow, bool) {
	id, ok := b.bySource[source]
	if !ok {
		return nil, false
	}
	row := b.rows[id]
	return row, row.state.Live()
}

// move moves the book by one entry of the record — its ok branch commands, in
// three steps (this file's head): every head move, then every merge, then
// every close.
func (b *changeBook) move(e PushEntry) {
	pusher, kind := e.Principal.party()
	var commands []Command
	for _, c := range e.Commands {
		if c.Result == ResultOK && strings.HasPrefix(c.Ref, branchPrefix) {
			commands = append(commands, c)
		}
	}
	for _, c := range commands {
		row, live := b.liveOn(c.Ref)
		if !live || c.New == ZeroOID {
			continue
		}
		row = b.touch(row.id)
		row.head, row.revision = c.New, row.revision+1
		b.writes.revisions = append(b.writes.revisions, revisionRow{change: row.id, n: row.revision, head: c.New,
			pusher: pusher, kind: kind, pushID: e.PushID})
		// A PUSH NO SEAT OR PERSON MADE adds no author — no decide admits
		// one, since a token bound to nobody reads only, and an author row
		// naming nobody would name what no review could ask after.
		if pair := [2]string{row.id, pusher}; pusher != "" && !b.authored[pair] {
			b.author(pair)
			b.writes.authors = append(b.writes.authors, authorRow{change: row.id, author: pusher, kind: kind,
				first: row.revision})
		}
	}
	for _, c := range commands {
		if c.New == ZeroOID {
			continue
		}
		for _, id := range b.byTarget[c.Ref] {
			row := b.rows[id]
			if !row.state.Live() || row.head != c.New || b.writes.merges >= MaxPushMerges {
				continue
			}
			row = b.touch(id)
			row.state, row.mergedVia, row.mergeOID = ChangeStateMerged, MergedViaPush, c.New
			row.mergedAt = sql.NullInt64{Int64: b.at, Valid: true}
			b.writes.merges++
			b.writes.merged = append(b.writes.merged, id)
		}
	}
	for _, c := range commands {
		row, live := b.liveOn(c.Ref)
		if !live || c.New != ZeroOID {
			continue
		}
		row = b.touch(row.id)
		row.state, row.closeReason = ChangeStateClosed, SourceDeleted
		row.closedAt = sql.NullInt64{Int64: b.at, Valid: true}
	}
}

// open opens the change e carries ([PushEntry.Change]) on e's one branch
// command ([ChangeSource]), at the head that command set — the record's rows
// at its position: the change's, numbered the repository's next where the
// book numbers (the apply), open or a draft as the change says; its
// description, where it has one; its pusher as its owner and first author,
// with what made the push — the turn behind a seat's push and, for a push its
// coding box made, the coding run that pushed ([RunProvenance.RunID]) — as the
// turn and the sandbox run that opened it, an open's own ([ChangeOpen.TurnID],
// [ChangeOpen.RunID]); and its first revision, made by the push. From here on
// it is a live change on its source and into its target, which the record's
// later entries revise, merge or close as any other, at the version the open
// gave it: the record that opens a change is its first, however many of its
// entries move it.
//
// IT CARRIES NO WORK ITEM: a change carries an item by its identity in its
// tracker, and a push names its run's item by that tracker and the key it
// shows alone ([RunProvenance]).
//
// It answers the change's id, or "" and why it opened none — a guard the
// decide's rules keep from ever failing (pushOpen), which the apply logs and
// skips (code_change_not_applied), never refuses: an apply that refused would
// stop the log on every node.
func (b *changeBook) open(e PushEntry) (string, string) {
	i, r, _ := ChangeSource(e.Commands)
	if r != "" {
		return "", "its push sets no one branch to open it from (" + string(r) + ")"
	}
	source := e.Commands[i]
	owner, kind := e.Principal.party()
	id := PushChangeID(e.PushID)
	switch {
	case source.Result != ResultOK:
		return "", "its push's update of " + source.Ref + " did not land"
	case owner == "":
		return "", "its pusher names no seat, and only a seat or a person opens a change"
	case b.rows[id] != nil:
		return "", "a change already holds its id"
	}
	if live, ok := b.liveOn(source.Ref); ok {
		return "", "its source already heads live change " + live.id
	}
	state := ChangeStateOpen
	if e.Change.Draft {
		state = ChangeStateDraft
	}
	// THE CREDENTIAL A PERSON PUSHED WITH, the change's own attribution as
	// an open's is ([Applier.applyChangeOpen]); a seat acts as itself.
	var operator string
	if kind == PartyPerson {
		operator = e.Principal.OperatorID
	}
	row := &changeRow{id: id, repoID: b.repo, number: b.next, title: e.Change.Title,
		bodyBytes: int64(len(e.Change.Body)), state: state, source: source.Ref, target: e.Change.Target,
		head: source.New, revision: 1, owner: owner, ownerKind: kind, operator: operator,
		turn: e.Principal.Run.TurnID, run: e.Principal.Run.RunID, openedAt: b.at, opened: b.packed, version: 1,
		position: b.packed}
	if b.next > 0 {
		b.next++
	}
	// WHAT IT POINTS AT ITS CHANGE, as it was, for an entry on trial.
	if t := b.trial; t != nil {
		was, held := b.bySource[row.source]
		ids, named := b.byTarget[row.target]
		t.added = append(t.added, id)
		t.sources = append(t.sources, sourceWas{source: row.source, id: was, held: held})
		t.targets = append(t.targets, targetWas{target: row.target, n: len(ids), held: named})
	}
	b.rows[id] = row
	b.bySource[row.source] = id
	b.byTarget[row.target] = append(b.byTarget[row.target], id)
	// TOUCHED BY ITS OPEN, so the record's later moves of it count no
	// second version.
	w := &b.writes
	w.isTouched[id] = true
	w.touched = append(w.touched, id)
	w.opened[id] = true
	w.revisions = append(w.revisions, revisionRow{change: id, n: 1, head: source.New, pusher: owner, kind: kind,
		pushID: e.PushID})
	b.author([2]string{id, owner})
	w.authors = append(w.authors, authorRow{change: id, author: owner, kind: kind, first: 1})
	if e.Change.Body != "" {
		w.bodies = append(w.bodies, bodyRow{change: id, body: e.Change.Body})
	}
	return id, ""
}

// The live changes a push record reads — the changes its branches head and
// the changes its targets are set to the heads of — and the authors its
// pushers already are of them, each registered in plan_test.go: driven from
// its JSON list, one seek an element — of the index partial on the live
// states, its predicate as the index spells it ([liveChanges]), by a branch
// or by a target and a head; of the authors' key by a change and an author.
// Every column a change's row holds is read qualified, since json_each has an
// `id` of its own; [changeColumns]'s order, which
// TestTheQualifiedChangeColumnsAreTheChangeColumns holds them to.
const (
	qualifiedChangeColumns = `c.id, c.repo_id, c.number, c.title, c.body_bytes, c.state, c.source_ref,
		c.target_ref, c.head_oid, c.revision, c.owner, c.owner_kind, c.owner_operator_id, c.work_backend,
		c.work_id, c.work_key, c.work_project, c.created_turn, c.created_run, c.opened_at, c.opened_position,
		c.closed_at, c.close_reason, c.merged_at, c.merged_via, c.merge_oid, c.merged_by, c.merged_by_kind,
		c.merged_operator_id, c.override_reason, c.version, c.position`
	liveBySourceSQL = `SELECT ` + qualifiedChangeColumns + ` FROM json_each(?2) AS j
		CROSS JOIN code_changes AS c INDEXED BY code_changes_open_idx
		WHERE c.repo_id = ?1 AND c.source_ref = CAST(j.value AS TEXT) AND ` + liveChanges
	liveByTargetSQL = `SELECT ` + qualifiedChangeColumns + ` FROM json_each(?2) AS j
		CROSS JOIN code_changes AS c INDEXED BY code_changes_target_idx
		WHERE c.repo_id = ?1 AND c.target_ref = CAST(j.value ->> 0 AS TEXT)
			AND c.head_oid = CAST(j.value ->> 1 AS TEXT) AND ` + liveChanges
	heldAuthorsSQL = `SELECT a.change_id, a.author FROM json_each(?) AS j
		CROSS JOIN code_change_authors AS a
		WHERE a.change_id = CAST(j.value ->> 0 AS TEXT) AND a.author = CAST(j.value ->> 1 AS TEXT)`
	// queuedChangesSQL is the changes of a list that hold a LIVE queue
	// entry, one seek of code_queue_change_idx a change, the live states
	// spelled as code_queue_live_idx is partial on them.
	queuedChangesSQL = `SELECT q.change_id FROM json_each(?) AS j
		CROSS JOIN code_queue AS q INDEXED BY code_queue_change_idx
		WHERE q.change_id = CAST(j.value AS TEXT) AND q.state IN ('waiting', 'testing')`
)
