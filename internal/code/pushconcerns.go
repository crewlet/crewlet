package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// WHOM A PUSH CONCERNS: a push record tells somebody about exactly two things
// it does to changes — a change it MERGES, by setting the change's target to
// its head, whose authors hear `merged`; and a change it OPENS from its
// options, whose text's mentions hear `mention` — and nothing else: a head
// move, a close by a deleted branch and a push to a ref no change uses are
// bookkeeping (concerns.go). Each entry carries the routings of the changes
// its own commands merge and open ([PushEntry.Routing]), its pusher their
// actor, stamped by the push decide in the snapshot it decides the entry in.
//
// # The bytes
//
// A push record's bytes are the committer's to bound: it gathers a batch by
// what each proposal's entry can reach in a record ([entryBound]), which knows
// nothing of whom an entry will concern — so a routing is bytes the batch was
// not measured for. The decide holds them to what the record leaves
// ([routingRoom]): an entry whose routings do not fit beside every proposal
// the round decides is CARRIED, with every push after it, to lead the next
// batch — where it meets a record of its own — as a push whose change rows do
// not fit is (revisions.go). An entry that leads its record and still does not
// fit beside its own bytes KEEPS THE ROUTINGS THAT FIT, in its order, and the
// rest of its record is carried: the push must land — a push is never refused
// for whom it would tell — and only a push of nearly a whole record's bytes
// that merges changes meets it, a change past the room merged and announced
// to nobody.

// routingRoom is one push decide round's record as it is filled with the
// routings of what its entries merge and open.
type routingRoom struct {
	batch  []Proposal
	open   []int
	repoID string
	repo   pushRepo
	roster Roster

	// bounds is the sum of the round's open proposals' entry bounds — every
	// proposal it decides, kept or not, an upper bound on what the record's
	// entries take — read the first time a routing asks for room, so a round
	// that routes nothing measures nothing.
	bounds   int
	measured bool
	// used is the bytes the routings of the entries admitted so far take,
	// and notices the notice rows they make the record's apply write.
	used, notices int

	// next is the repository's next change number, read the first time an
	// entry opens one; numbers the number each change an admitted entry
	// opened takes in the record's apply, which numbers its opens in order.
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

// route is the routings of the changes e merges — merged, in the order its
// moves merged them — and of the change it opens, opened ("" for none), read
// in the round's snapshot tx and the book b as e's moves leave it.
func (r *routingRoom) route(ctx context.Context, tx *sql.Tx, b *changeBook, e PushEntry, merged []string,
	opened string) ([]ChangeRouting, error) {

	r.pending = ""
	if len(merged) == 0 && opened == "" {
		return nil, nil
	}
	actor, _ := e.Principal.party()
	var out []ChangeRouting
	for _, id := range merged {
		row := *b.rows[id]
		if n, ok := r.numbers[id]; ok {
			// OPENED BY AN EARLIER ENTRY of this record, which the decide's
			// book does not number: the number its open took.
			row.number = n
		}
		// THE AUTHORS NO ROW HOLDS YET: the ones this record's entries so
		// far made, by moving the change's head or opening it.
		var added []Reviewer
		for _, a := range b.writes.authors {
			if a.change == id {
				added = append(added, Reviewer{Handle: a.author, Kind: a.kind})
			}
		}
		routed, err := routingFor(ctx, tx, concerning{repoName: r.repo.name, change: row, actor: actor,
			authors: true, addedAuthors: added, merged: true})
		if err != nil {
			return nil, err
		}
		out = append(out, *routed)
	}
	if opened != "" {
		if !r.nextRead {
			if err := tx.QueryRowContext(ctx, nextChangeSQL, r.repoID).Scan(&r.next); err != nil {
				return nil, fmt.Errorf("code: read the next number of repository %s: %w", r.repoID, err)
			}
			r.nextRead = true
		}
		r.pending, r.pendingNumber = opened, r.next+int64(len(r.numbers))
		row := *b.rows[opened]
		row.number = r.pendingNumber
		routed, err := routingFor(ctx, tx, concerning{repoName: r.repo.name, change: row, actor: actor,
			roster: r.roster, mentionText: []string{e.Change.Title, e.Change.Body}})
		if err != nil {
			return nil, err
		}
		out = append(out, *routed)
	}
	return out, nil
}

// fit admits the routings of the round's i-th proposal's entry, first where it
// leads its record, and answers whether it may be kept, and whether every
// entry after it must be carried — this file's head: kept where the routings
// fit beside every proposal the round decides, and their notices within
// rowsLeft — the rows the record's entries and changes leave of its bound;
// carried where they do not and another entry leads the record; and, leading
// it, kept with the routings that fit beside its own bytes and within
// rowsLeft, every later entry carried.
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
			bound, err := entryBound(r.batch[j])
			if err != nil {
				return false, false, fmt.Errorf("code: measure push %s's entry: %w", r.batch[j].PushID, err)
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
		return false, false, fmt.Errorf("code: measure push %s's entry: %w", r.batch[i].PushID, err)
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

// noticeRows is the notice rows routings make an apply write: one a handle
// they tell.
func noticeRows(routings []ChangeRouting) int {
	pointers := make([]*ChangeRouting, len(routings))
	for i := range routings {
		pointers[i] = &routings[i]
	}
	return heardCount(pointers...)
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

// routingBytes is what routings take of an entry in its record: their JSON
// and the key that names them, nothing for none.
func routingBytes(routings []ChangeRouting) (int, error) {
	if len(routings) == 0 {
		return 0, nil
	}
	encoded, err := json.Marshal(routings)
	if err != nil {
		return 0, fmt.Errorf("code: encode a push's routings: %w", err)
	}
	return len(encoded) + len(`,"routing":`), nil
}
