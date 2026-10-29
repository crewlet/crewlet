package code

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
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
// moves a copy of it by each entry it admits, so it counts EXACTLY the rows
// the record's apply will write about changes beside its entries' own — the
// apply reads the same rows, which the broker's arbitration on the
// repository's subject guarantees, and writes what the same model makes of
// the same entries — and holds the record to [MaxRecordRows] with them. A
// push whose changes' rows would take its record past the bound beside the
// entries before it is CARRIED, decided by none of its batch's rounds, to
// lead the committer's next batch; one whose rows would take even a record
// of its own past it is refused [RefusalTooManyChanges].
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
	// opened is every change the record's entries opened ([changeBook.open]),
	// whose row the apply INSERTS where it writes every other back; and
	// bodies the descriptions of those that carry one.
	opened map[string]bool
	bodies []bodyRow
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
	// touched is every change the entries moved, in the order first moved —
	// the order the apply writes them in — and isTouched the same changes as
	// a set, since a push that moves hundreds asks it once a move.
	touched   []string
	isTouched map[string]bool
	// revisions are the revisions the entries wrote, in order, and authors
	// the authors they added, each pair of a change and an author once.
	revisions []revisionRow
	authors   []authorRow
	// merges is how many changes the entries merged.
	merges int
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

	b := &changeBook{repo: repoID, packed: packed, at: at, opened: map[string]bool{},
		rows: map[string]*changeRow{}, bySource: map[string]string{}, byTarget: map[string][]string{},
		authored: map[[2]string]bool{}, isTouched: map[string]bool{}}
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
	return b, nil
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

// clone is a book of its own with b's state, which moving changes nothing of
// b: what the decide tries an entry on before it admits it.
func (b *changeBook) clone() *changeBook {
	c := &changeBook{repo: b.repo, packed: b.packed, at: b.at, next: b.next, opened: maps.Clone(b.opened),
		bodies: slices.Clone(b.bodies), rows: make(map[string]*changeRow, len(b.rows)),
		bySource: maps.Clone(b.bySource), byTarget: make(map[string][]string, len(b.byTarget)),
		authored: maps.Clone(b.authored), touched: slices.Clone(b.touched), isTouched: maps.Clone(b.isTouched),
		revisions: slices.Clone(b.revisions), authors: slices.Clone(b.authors), merges: b.merges}
	for id, row := range b.rows {
		copied := *row
		c.rows[id] = &copied
	}
	for target, ids := range b.byTarget {
		c.byTarget[target] = slices.Clone(ids)
	}
	return c
}

// afresh is a book of its own whose changes and authors are as b's moves left
// them and which has moved nothing yet: the changes as the NEXT record would
// read them, were b's record to land — the ones b's opened among them, as
// rows that record finds — what the decide weighs a push it cannot fit beside
// the entries before it by, as the first of a record of its own.
func (b *changeBook) afresh() *changeBook {
	c := b.clone()
	c.touched, c.isTouched, c.revisions, c.authors, c.merges = nil, map[string]bool{}, nil, nil, 0
	c.opened, c.bodies = map[string]bool{}, nil
	return c
}

// rowCount is the rows the book's moves make the record's apply write: every
// change it moved or opened, every revision and every author it added, and
// the description of every change it opened that carries one. The
// repository's own row, whose next number an open moves, is the record's
// already ([recordRows]).
func (b *changeBook) rowCount() int {
	return len(b.touched) + len(b.revisions) + len(b.authors) + len(b.bodies)
}

// moved is how many changes the book's moves reached.
func (b *changeBook) moved() int { return len(b.touched) }

// touch counts change id moved by this record — one version, at the record's
// position, however many entries move it.
func (b *changeBook) touch(id string) *changeRow {
	row := b.rows[id]
	if !b.isTouched[id] {
		b.isTouched[id] = true
		b.touched = append(b.touched, id)
		row.version++
		row.position = b.packed
	}
	return row
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
		b.revisions = append(b.revisions, revisionRow{change: row.id, n: row.revision, head: c.New, pusher: pusher,
			kind: kind, pushID: e.PushID})
		// A PUSH NO SEAT OR PERSON MADE adds no author — no decide admits
		// one, since a token bound to nobody reads only, and an author row
		// naming nobody would name what no review could ask after.
		if pair := [2]string{row.id, pusher}; pusher != "" && !b.authored[pair] {
			b.authored[pair] = true
			b.authors = append(b.authors, authorRow{change: row.id, author: pusher, kind: kind, first: row.revision})
		}
	}
	for _, c := range commands {
		if c.New == ZeroOID {
			continue
		}
		for _, id := range b.byTarget[c.Ref] {
			row := b.rows[id]
			if !row.state.Live() || row.head != c.New || b.merges >= MaxPushMerges {
				continue
			}
			row = b.touch(id)
			row.state, row.mergedVia, row.mergeOID = ChangeStateMerged, MergedViaPush, c.New
			row.mergedAt = sql.NullInt64{Int64: b.at, Valid: true}
			b.merges++
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
// description, where it has one; its pusher as its owner and first author;
// and its first revision, made by the push. From here on it is a live change
// on its source and into its target, which the record's later entries revise,
// merge or close as any other, at the version the open gave it: the record
// that opens a change is its first, however many of its entries move it.
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
		turn: e.Principal.Run.TurnID, openedAt: b.at, opened: b.packed, version: 1, position: b.packed}
	if b.next > 0 {
		b.next++
	}
	b.rows[id] = row
	b.bySource[row.source] = id
	b.byTarget[row.target] = append(b.byTarget[row.target], id)
	// TOUCHED BY ITS OPEN, so the record's later moves of it count no
	// second version.
	b.isTouched[id] = true
	b.touched = append(b.touched, id)
	b.opened[id] = true
	b.revisions = append(b.revisions, revisionRow{change: id, n: 1, head: source.New, pusher: owner, kind: kind,
		pushID: e.PushID})
	b.authored[[2]string{id, owner}] = true
	b.authors = append(b.authors, authorRow{change: id, author: owner, kind: kind, first: 1})
	if e.Change.Body != "" {
		b.bodies = append(b.bodies, bodyRow{change: id, body: e.Change.Body})
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
		c.closed_at, c.close_reason, c.merged_at, c.merged_via, c.merge_oid, c.version, c.position`
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
)
