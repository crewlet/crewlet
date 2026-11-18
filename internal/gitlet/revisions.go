package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A change's revisions are derived from the push record that moves it — no
// record of the change's own, since the push already says its source moved —
// purely over the record and the change rows it finds, so every data node
// derives the same rows. Each entry, in decided order and seeing what earlier
// ones did, applies its OK branch commands in this order ([changeBook.move],
// then [changeBook.open]): HEAD MOVES (a live change's source moved: its next
// revision, its pusher an author); MERGES (a branch set to exactly the head of
// a live change into it — after the head moves, so git's command order is
// moot; at most [MaxPushMerges]); CLOSES (its source deleted, [SourceDeleted] —
// after the merges, so merge-then-delete stays merged); then the entry's OPEN
// ([PushEntry.Change]), after its own moves so the push is the change's FIRST
// revision. Nothing else moves a change.

// A change a record moves leaves its target's queue (queue.go) in the same
// record: a head move EJECTS its live entry (head_moved), since the queue would
// otherwise test a head nobody enqueued, and a merge or close DEQUEUES it
// ([changeBook.queueEnds]). Each change a record moves counts ONE version at
// the record's position however many entries move it ([changeBook.touch]), so
// a lifecycle write formed before a push that moved its head is stale; a change
// a record opens is at version 1.

// One model: the push decide moves a [changeBook] by each entry it admits, so
// it counts EXACTLY the change rows the apply writes ([changeBook.rowCount]) —
// the apply reads the same rows (the broker arbitrates on the repository's
// subject) and runs the same model — save each ended change's stored
// readiness, which the book counts whether or not the apply clears it, so the
// count is never short. Each entry is tried on the book in place and taken back
// where the record cannot hold it within [MaxRecordRows] ([bookTrial]); a push
// that fits no record beside earlier entries is CARRIED to lead the committer's
// next batch, and one that fits not even a record alone is refused
// [RefusalTooManyChanges].

// The record's format does not move: what a push does to a change is derived
// from the commands it already carries (the domain landed with these rows, so
// every build that meets a push record derives them), plus the one field a push
// opening a change carries ([PushEntry.Change]). Anything a later apply comes to
// derive, or a revision to keep beside them — as [PushEntry.WorkingFor] came
// with version 2 — needs a versionedFields row ([RecordVersion]).

// SourceDeleted is the close reason (code_changes.close_reason) of a change a
// push closed by deleting its source branch — the one reason no person wrote.
const SourceDeleted = "source_deleted"

// MaxPushMerges is the most changes one push record marks merged, so a push to
// a target is never refused for the changes it would merge, a record's rows
// being bounded; many live changes at one head into one target (each its own
// branch at that commit) is no push's ordinary work. It is
// [MaxPushesPerRecord]: one each for a full batch of pushes that each merge
// their own change, sixty-four rows of [MaxRecordRows] at most. A change past
// it, in number order, stays open with its head in its target.
const MaxPushMerges = MaxPushesPerRecord

// changeBook is the live changes a push record's commands can move, and the
// authors its pushers already are of them, as the record's entries so far
// leave them: the push decide counts the record's change rows by it, and the
// apply writes them from it.
type changeBook struct {
	// repo is the repository the record is about, and of any change it opens.
	repo string
	// packed and at are the record's position and broker instant, which every
	// row it writes carries — zero where the decide counts and writes nothing.
	packed, at int64
	// next is the number the record's next open gives its change (the
	// repository's next_change as the apply reads it), and zero in the decide,
	// which counts opens without numbering them: a number is the apply's to
	// give ([Applier.applyChangeOpen]'s rule).
	next int64
	// rows is every change read, by id, as the entries so far moved it.
	rows map[string]*changeRow
	// bySource is the live change each branch heads, and byTarget the
	// changes read into each branch, in number order.
	bySource map[string]string
	byTarget map[string][]string
	// authored is every (change, author) pair held that the record's head
	// moves can reach: those read, and those the entries so far added.
	authored map[[2]string]bool
	// queued is every change read holding a live queue entry, which the
	// record ends where it moves the change.
	queued map[string]bool
	// writes is what the entries so far make the record's apply write.
	writes bookWrites
	// trial journals the entry the decide is trying ([changeBook.try]); nil
	// while none is on trial, and always in the apply, which writes every
	// entry it meets.
	trial *bookTrial
}

// bookWrites is what a record's entries make its apply write about changes:
// the rows a decide counts and the apply writes ([changeBook.rowCount]).
type bookWrites struct {
	// opened is every change the entries opened ([changeBook.open]), whose
	// row the apply INSERTS rather than writes back; bodies the descriptions
	// of those that carry one.
	opened map[string]bool
	bodies []bodyRow
	// touched is every change the entries moved, in the order first moved
	// (the apply's write order); isTouched is it as a set, since a push moving
	// hundreds asks it once a move.
	touched   []string
	isTouched map[string]bool
	// revisions are the revisions the entries wrote, in order; authors the
	// authors they added, each (change, author) pair once.
	revisions []revisionRow
	authors   []authorRow
	// moves are the closes the entries made by deleting a source, each its
	// change's move ([MoveClosed]).
	moves []moveRow
	// merges counts the changes the entries merged and merged lists them in
	// order — what a push decide routes each entry's merges by.
	merges int
	merged []string
}

// noWrites is a record's writes before its first entry.
func noWrites() bookWrites {
	return bookWrites{opened: map[string]bool{}, isTouched: map[string]bool{}}
}

// bookTrial is what one entry on trial did to a [changeBook], each thing as it
// was before, so taking the entry back leaves the book exactly as it found it.
//
// A journal, never a copy: the decide reads ONE book a round for its whole
// batch and tries each entry on it, on the repository's one committer
// goroutine while it holds a reader-pool connection. A copy would cost every
// change the BATCH reached, per entry, per round (doc.go's "Measured"); the
// journal costs what the entry's own commands reach, nothing for one that
// reaches no change.
type bookTrial struct {
	// touched, bodies, revisions, authors, merges and next are the writes'
	// lengths, merge count and next number as the entry found them: what
	// taking it back cuts them to.
	touched, bodies, revisions, authors, moves, merges int
	next                                               int64
	// moved is every change the entry moved, by id, as before its first
	// move; added every change it opened, which taking it back forgets.
	moved map[string]changeRow
	added []string
	// authored is every (change, author) pair the entry added.
	authored [][2]string
	// sources and targets are what its opens repointed, each as it was, in
	// order: a source's change ([changeBook.bySource]) and how many changes a
	// target held ([changeBook.byTarget]).
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

// revisionRow is one code_revisions row a record writes, workingFor as its
// entry carries it ([PushEntry.WorkingFor]).
type revisionRow struct {
	change     string
	n          int64
	head       string
	pusher     string
	kind       PartyKind
	pushID     string
	workingFor *types.WorkingFor
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

// readChangeBook reads into a book the live changes of repoID that the
// unrefused entries' branch commands can move — the change each branch heads,
// and every one whose target they set to exactly its head — and the authors
// each pusher already is of the changes whose heads it moves; moved rows carry
// packed and at.
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
		return nil, fmt.Errorf("gitlet: read the live changes a push's branches head: %w", err)
	}
	if err := inChunks(pairs, func(part [][2]string) error {
		return b.read(ctx, tx, liveByTargetSQL, repoID, part)
	}); err != nil {
		return nil, fmt.Errorf("gitlet: read the live changes a push's targets are set to the heads of: %w", err)
	}
	// Number order, the order a record merges them in, so the merge bound
	// ([MaxPushMerges]) picks the same ones on every node.
	for _, ids := range b.byTarget {
		slices.SortFunc(ids, func(x, y string) int { return cmp.Compare(b.rows[x].number, b.rows[y].number) })
	}
	// The authors the pushers already are, so the book adds only an author a
	// change lacks: a counted row, never a conflict its insert skips.
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
		return nil, fmt.Errorf("gitlet: read the authors a push's pushers are of the changes it moves: %w", err)
	}
	// Which changes read hold a live queue entry: a move of one ends it, a
	// counted row.
	ids := make([]string, 0, len(b.rows))
	for id := range b.rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if err := inChunks(ids, func(part []string) error { return b.readQueued(ctx, tx, part) }); err != nil {
		return nil, fmt.Errorf("gitlet: read which changes a push moves are queued: %w", err)
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

// readAuthors marks every (change, author) pair of pairs that the change holds.
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

// try puts the decide's next entry on trial: its moves and open are journaled
// ([bookTrial]) for [changeBook.takeBack] until [changeBook.keep] admits it.
func (b *changeBook) try() {
	w := &b.writes
	b.trial = &bookTrial{touched: len(w.touched), bodies: len(w.bodies), revisions: len(w.revisions),
		authors: len(w.authors), moves: len(w.moves), merges: w.merges, next: b.next}
}

// keep admits the entry on trial: its moves are the record's.
func (b *changeBook) keep() { b.trial = nil }

// takeBack undoes the entry on trial, leaving the book exactly as
// [changeBook.try] found it, at the cost of what the entry reached.
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
	// The changes the entry first moved, its opens among them, are the
	// writes past their length before it.
	for _, id := range w.touched[t.touched:] {
		delete(w.isTouched, id)
		delete(w.opened, id)
	}
	w.touched, w.bodies = w.touched[:t.touched], w.bodies[:t.bodies]
	w.revisions, w.authors, w.merges, b.next = w.revisions[:t.revisions], w.authors[:t.authors], t.merges, t.next
	w.moves = w.moves[:t.moves]
	w.merged = w.merged[:t.merges]
}

// alone is the change rows e (and any change it opens) would make a record
// write ([changeBook.decidedRows]), and how many changes it moves, as the
// first entry of a record of its own after the book's record lands: the
// changes as that record leaves them, none of its writes. The decide weighs a
// push it cannot fit beside earlier entries by it. It leaves the book as it
// found it.
func (b *changeBook) alone(e PushEntry) (rows, moved int) {
	record := b.writes
	b.writes = noWrites()
	b.try()
	b.move(e)
	if e.Change != nil {
		b.open(e)
	}
	rows, moved = b.decidedRows(), b.moved()
	b.takeBack()
	b.writes = record
	return rows, moved
}

// rowCount is the rows the book's moves make the apply write: each change
// moved or opened, revision, author and opened change's description, the live
// queue entry of each moved change holding one, and the stored readiness of
// each change merged or closed — counted whether or not the apply clears it
// ([clearReadiness]), since the book holds no mergeability. The repository's
// own row is the record's already ([recordRows]).
func (b *changeBook) rowCount() int { return b.rowsEnding(len(b.ended())) }

// rowsEnding is [changeBook.rowCount] with the ended changes counted once by its caller.
func (b *changeBook) rowsEnding(ended int) int {
	w := &b.writes
	return len(w.touched) + len(w.revisions) + len(w.authors) + len(w.bodies) + len(w.moves) + b.queuedMoved() +
		ended
}

// decidedRows is [changeBook.rowCount] plus, where the moves open or end a
// change (a push's only state moves), the tally row the apply writes beside
// it, which no horizon is given.
func (b *changeBook) decidedRows() int {
	ended := len(b.ended())
	if len(b.writes.opened) > 0 || ended > 0 {
		return b.rowsEnding(ended) + tallyRows
	}
	return b.rowsEnding(ended)
}

// statesRead is every change the book holds, by id, in its state now: taken
// before the record's first move, the states the apply read them in.
func (b *changeBook) statesRead() map[string]ChangeState {
	read := make(map[string]ChangeState, len(b.rows))
	for id, row := range b.rows {
		read[id] = row.state
	}
	return read
}

// counted is what the book's moves do to the repository's counts from the
// states read ([changeBook.statesRead]): an opened change in its final state,
// every other moved from its read state to its final one.
func (b *changeBook) counted(read map[string]ChangeState) ChangeCounts {
	var c ChangeCounts
	for _, id := range b.writes.touched {
		if b.writes.opened[id] {
			c.add(b.rows[id].state, 1)
			continue
		}
		c.move(read[id], b.rows[id].state)
	}
	return c
}

// ended is the changes the book's moves merged or closed, whose stored
// readiness the apply clears ([clearReadiness]) — none it opened, whose
// readiness no record has judged yet.
func (b *changeBook) ended() []string {
	var ids []string
	for _, id := range b.writes.touched {
		if b.writes.opened[id] {
			continue
		}
		if kind := pushWrite(b.rows[id], false); kind == changeMerged || kind == changeStateMoved {
			ids = append(ids, id)
		}
	}
	return ids
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

// queueEnds is how the book's moves end the live queue entries of the changes
// they reached (none it opened, which no queue holds yet): a moved head
// ejected (head_moved), a merged or closed change dequeued. The apply ends the
// entries by it and the decide names the ejected to the committer
// (Committer.announcePushEjections), so the two agree.
func (b *changeBook) queueEnds() (ejected, merged, closed []string) {
	w := &b.writes
	for _, id := range w.touched {
		if w.opened[id] || !b.queued[id] {
			continue
		}
		switch pushWrite(b.rows[id], false) {
		case changeMerged:
			merged = append(merged, id)
		case changeStateMoved:
			closed = append(closed, id)
		default:
			ejected = append(ejected, id)
		}
	}
	return ejected, merged, closed
}

// moved is how many changes the book's moves reached.
func (b *changeBook) moved() int { return len(b.writes.touched) }

// touch counts change id moved by this record — one version, at the record's
// position, however many entries move it — and answers its row for the move to
// change, saved first for an entry on trial.
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
		row.position, row.updatedAt = b.packed, b.at
	}
	return row
}

// author adds pair, a change and an author it lacked, journaled for an entry
// on trial.
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

// move moves the book by one entry's ok branch commands in the order this
// file's head gives: every head move, then every merge, then every close. A
// head move never checks the head changed: validCommands never sets a ref to
// where it is, and a live change's head is its source's.
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
			pusher: pusher, kind: kind, pushID: e.PushID, workingFor: e.WorkingFor})
		// A push no seat or person made adds no author: an author row naming
		// nobody names what no review could ask after (and no decide admits
		// one, a token bound to nobody reading only).
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
		row.closedBy, row.closedByKind = pusher, kind
		b.writes.moves = append(b.writes.moves, moveRow{change: row.id, kind: MoveClosed, by: pusher, byKind: kind,
			reason: SourceDeleted})
	}
}

// open opens the change e carries ([PushEntry.Change]) on e's one branch
// command ([ChangeSource]) at the head it set, at version 1: numbered the
// repository's next where the book numbers (the apply); its pusher owner and
// first author; the push's turn and launch as an open's own ([ChangeOpen.TurnID],
// [ChangeOpen.RunID]). It carries the push's work item only where the push names
// its identity ([RunProvenance.WorkID]), in code_open_change's shape; a tracker
// and key alone, a label a tracker may reuse, carries none.
//
// It answers the change's id, or "" and why it opened none — a guard pushOpen
// keeps from failing, which the apply logs and skips (code_change_not_applied)
// rather than refuses: a refusing apply would stop the log on every node.
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
	// A person's credential is the change's attribution, as an open's is
	// ([Applier.applyChangeOpen]); a seat acts as itself.
	var operator string
	if kind == PartyPerson {
		operator = e.Principal.OperatorID
	}
	row := &changeRow{id: id, repoID: b.repo, number: b.next, title: e.Change.Title,
		bodyBytes: int64(len(e.Change.Body)), state: state, source: source.Ref, target: e.Change.Target,
		head: source.New, revision: 1, owner: owner, ownerKind: kind, operator: operator,
		turn: e.Principal.Run.TurnID, run: e.Principal.Run.RunID, openedAt: b.at, opened: b.packed, version: 1,
		position: b.packed, updatedAt: b.at}
	if run := e.Principal.Run; run.WorkID != "" {
		row.work = types.WorkItem{Backend: types.WorkBackend(run.WorkBackend), ID: run.WorkID, Key: run.WorkKey,
			Project: run.WorkProject}
	}
	if b.next > 0 {
		b.next++
	}
	// Journal what it repoints, as it was, for an entry on trial.
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
	// Touched by its open, so later moves in the record count no second
	// version.
	w := &b.writes
	w.isTouched[id] = true
	w.touched = append(w.touched, id)
	w.opened[id] = true
	w.revisions = append(w.revisions, revisionRow{change: id, n: 1, head: source.New, pusher: owner, kind: kind,
		pushID: e.PushID, workingFor: e.WorkingFor})
	b.author([2]string{id, owner})
	w.authors = append(w.authors, authorRow{change: id, author: owner, kind: kind, first: 1})
	if e.Change.Body != "" {
		w.bodies = append(w.bodies, bodyRow{change: id, body: e.Change.Body})
	}
	return id, ""
}

// The reads behind [readChangeBook], each registered in plan_test.go: driven
// from its JSON list, one seek an element — of the index partial on the live
// states, its predicate as the index spells it ([liveChanges]), or of the
// authors' key. A change's columns are read qualified, since json_each has an
// `id` of its own, in [changeColumns]'s order
// (TestTheQualifiedChangeColumnsAreTheChangeColumns).
const (
	qualifiedChangeColumns = `c.id, c.repo_id, c.number, c.title, c.body_bytes, c.state, c.source_ref,
		c.target_ref, c.head_oid, c.revision, c.owner, c.owner_kind, c.owner_operator_id, c.work_backend,
		c.work_id, c.work_key, c.work_project, c.created_turn, c.created_run, c.opened_at, c.opened_position,
		c.closed_at, c.close_reason, c.closed_by, c.closed_by_kind, c.merged_at, c.merged_via, c.merge_oid,
		c.merged_by, c.merged_by_kind, c.merged_operator_id, c.override_reason, c.version, c.position,
		c.updated_at`
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
	// queuedChangesSQL is the changes of a list holding a LIVE queue entry,
	// one seek of code_queue_change_idx a change, the live states spelled as
	// code_queue_live_idx is partial on them.
	queuedChangesSQL = `SELECT q.change_id FROM json_each(?) AS j
		CROSS JOIN code_queue AS q INDEXED BY code_queue_change_idx
		WHERE q.change_id = CAST(j.value AS TEXT) AND q.state IN ('waiting', 'testing')`
)
