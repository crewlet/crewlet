package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// WHOM A PUSH CONCERNS: only a change it MERGES, by setting the target to its
// head (its authors hear `merged`), and one it OPENS from its options (its
// text's mentions hear `mention`); every other move is bookkeeping
// (concerns.go). The push decide stamps them on the entry ([PushEntry.Routing]).
//
// A batch is gathered by [entryBound], which cannot know whom an entry will
// concern, so the decide holds routings to what the record leaves
// ([routingRoom]): an entry whose routings do not fit is CARRIED, with every
// push after it, to lead the next batch, as revisions.go carries change rows.
// An entry leading its record that still does not fit KEEPS THE ROUTINGS THAT
// FIT, in order, and the rest go unannounced: a push is never refused for whom
// it would tell, and the rest of its record is carried.

// routingRoom is one push decide round's record as it is filled with the
// routings of what its entries merge and open.
type routingRoom struct {
	batch  []Proposal
	open   []int
	repoID string
	repo   pushRepo
	roster Roster

	// bounds sums the entry bounds of every proposal the round decides, kept
	// or not: an upper bound on the record's entries, measured only once a
	// routing asks for room.
	bounds   int
	measured bool
	// used is the bytes of the admitted entries' routings, and notices the
	// notice rows they make the apply write.
	used, notices int

	// next is the repository's next change number, read at the first open;
	// numbers the number each admitted open takes, as the apply numbers
	// opens in order.
	next     int64
	nextRead bool
	numbers  map[string]int64
	// pending is the change the entry being routed opens, and its number,
	// recorded once the entry is admitted.
	pending       string
	pendingNumber int64

	// kept is the routings the last admitted entry carries.
	kept []ChangeRouting
}

// route is the routings of the changes e merges (merged, in its moves' order)
// and of the change it opens ("" for none), read in the round's snapshot tx
// and the book b as e's moves leave it.
func (r *routingRoom) route(ctx context.Context, tx *sql.Tx, b *changeBook, e PushEntry, merged []string,
	opened string) ([]ChangeRouting, error) {

	r.pending = ""
	if len(merged) == 0 && opened == "" {
		return nil, nil
	}
	// The pusher is the actor; whom it pushed for rides [PushEntry.WorkingFor].
	actor, actorKind := e.Principal.party()
	var out []ChangeRouting
	for _, id := range merged {
		row := *b.rows[id]
		if n, ok := r.numbers[id]; ok {
			// Opened by an earlier entry, which the book does not number.
			row.number = n
		}
		// Authors no row holds yet: made by this record's earlier moves and
		// opens.
		var added []Reviewer
		for _, a := range b.writes.authors {
			if a.change == id {
				added = append(added, Reviewer{Handle: a.author, Kind: a.kind})
			}
		}
		routed, err := routingFor(ctx, tx, concerning{repoName: r.repo.name, change: row, actor: actor,
			actorKind: actorKind, workingFor: e.WorkingFor, authors: true, addedAuthors: added, merged: true})
		if err != nil {
			return nil, err
		}
		if routed != nil {
			out = append(out, *routed)
		}
	}
	if opened != "" {
		if !r.nextRead {
			if err := tx.QueryRowContext(ctx, nextChangeSQL, r.repoID).Scan(&r.next); err != nil {
				return nil, fmt.Errorf("gitlet: read the next number of repository %s: %w", r.repoID, err)
			}
			r.nextRead = true
		}
		r.pending, r.pendingNumber = opened, r.next+int64(len(r.numbers))
		row := *b.rows[opened]
		row.number = r.pendingNumber
		routed, err := routingFor(ctx, tx, concerning{repoName: r.repo.name, change: row, actor: actor,
			actorKind: actorKind, workingFor: e.WorkingFor, roster: r.roster,
			mentionText: []string{e.Change.Title, e.Change.Body}})
		if err != nil {
			return nil, err
		}
		if routed != nil {
			out = append(out, *routed)
		}
	}
	return out, nil
}

// fit admits the routings of the round's i-th proposal's entry (first where it
// leads its record) by the rule this file's head states, and answers whether
// the entry is kept and whether every later one is carried. rowsLeft is the
// rows the record's entries and changes leave of its bound for notices.
func (r *routingRoom) fit(i int, first bool, routings []ChangeRouting, rowsLeft int) (fits, rest bool,
	err error) {

	r.kept = routings
	if len(routings) == 0 {
		r.admit(0, 0)
		return true, false, nil
	}
	whole := MaxRecordBytes - recordOverheadBytes
	if !r.measured {
		for _, j := range r.open {
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			bound, err := entryBound(r.batch[j])
			if err != nil {
				return false, false, fmt.Errorf("gitlet: measure push %s's entry: %w", r.batch[j].PushID, err)
			}
			r.bounds += bound
		}
		r.measured = true
	}
	size, err := routingBytes(routings)
	if err != nil {
		return false, false, err
	}
	notices := noticeRows(routings)
	if r.bounds+r.used+size <= whole && r.notices+notices <= rowsLeft {
		r.admit(size, notices)
		return true, false, nil
	}
	if !first {
		return false, false, nil
	}
	bound, err := entryBound(r.batch[i])
	if err != nil {
		return false, false, fmt.Errorf("gitlet: measure push %s's entry: %w", r.batch[i].PushID, err)
	}
	for len(routings) > 0 && (bound+size > whole || r.notices+notices > rowsLeft) {
		routings = routings[:len(routings)-1]
		if size, err = routingBytes(routings); err != nil {
			return false, false, err
		}
		notices = noticeRows(routings)
	}
	r.kept = routings
	r.admit(size, notices)
	return true, true, nil
}

// noticeRows is the notice rows routings make an apply write: one a handle,
// and one a change for its count ([noticeWrites]).
func noticeRows(routings []ChangeRouting) int {
	pointers := make([]*ChangeRouting, len(routings))
	for i := range routings {
		pointers[i] = &routings[i]
	}
	return noticeWrites(pointers...)
}

// admit counts an admitted entry's routings, size bytes and notices rows, and
// the number of the change it opened.
func (r *routingRoom) admit(size, notices int) {
	r.used += size
	r.notices += notices
	if r.pending == "" {
		return
	}
	if r.numbers == nil {
		r.numbers = map[string]int64{}
	}
	r.numbers[r.pending] = r.pendingNumber
	r.pending = ""
}

// routingBytes is what routings add to an entry: their JSON and its key, 0 for
// none.
func routingBytes(routings []ChangeRouting) (int, error) {
	if len(routings) == 0 {
		return 0, nil
	}
	encoded, err := json.Marshal(routings)
	if err != nil {
		return 0, fmt.Errorf("gitlet: encode a push's routings: %w", err)
	}
	return len(encoded) + len(`,"routing":`), nil
}
