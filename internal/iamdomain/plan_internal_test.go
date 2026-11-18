package iamdomain

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE DIRECTORY'S STATEMENTS ARE THE ONES PLANNED, EVERY ONE OF THEM, AND
// EVERY INDEX ON ITS TABLES SERVES ONE.
//
// A sign-in and every directory decision look up a bound value in a table with
// a row per login: unindexed, that is a slow sign-in, never a failure, and an
// index nobody reads is a write on every apply on every data node. Only the
// planner knows which, so this plans the package's own statements (copies of
// them drift) as a deployment does, with no statistics over an empty estate.
// Every module statement reading a covered table is registered, matched whole
// ([storetest.StatementLiterals]), and every index on one is in some plan.
// Mutation: read `iam_people` in an unregistered literal and the walk names the
// file; leave `iam_people_bucket_idx` in place and the inventory names it.
func TestTheDirectoryStatementsArePlanned(t *testing.T) {
	t.Parallel()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	// The tables read by a value or in an order; the credentials, sessions and
	// gates are read by their keys and the sweep's own indexes.
	covered := []string{"iam_people", "iam_invites", "iam_history"}

	used := map[string]bool{}
	var planned []string
	plan := func(t *testing.T, statement string, args []any) []string {
		t.Helper()
		steps := storetest.Plan(t, db, statement, args)
		for _, index := range storetest.IndexesIn(steps) {
			used[index] = true
		}
		return steps
	}
	refuseHeap := func(t *testing.T, steps []string) {
		t.Helper()
		for _, table := range covered {
			if storetest.ScansHeap(steps, table) {
				t.Errorf("this read reads %s with no index:\n%s", table,
					strings.Join(steps, "\n"))
			}
		}
	}

	for name, read := range directoryReads() {
		planned = append(planned, read.sql)
		t.Run(name, func(t *testing.T) {
			steps := plan(t, read.sql, read.args)
			refuseHeap(t, steps)
			shown := strings.Join(steps, "\n")
			switch {
			case !read.walk && !storetest.Seeks(steps, read.index):
				t.Errorf("this read does not SEEK %s:\n%s", read.index, shown)
			case read.walk && !storetest.Walks(steps, read.index):
				t.Errorf("this read does not walk %s in its order:\n%s", read.index, shown)
			case (read.walk || read.ordered) && storetest.Sorts(steps):
				t.Errorf("this read reaches %s and then sorts what it read:\n%s",
					read.index, shown)
			}
		})
	}
	for name, census := range directoryCensuses() {
		planned = append(planned, census.sql)
		t.Run(name, func(t *testing.T) {
			refuseHeap(t, plan(t, census.sql, census.args))
		})
	}
	for name, pass := range directoryPasses() {
		planned = append(planned, pass.sql)
		t.Run(name, func(t *testing.T) {
			if steps := plan(t, pass.sql, pass.args); storetest.Sorts(steps) {
				t.Errorf("this pass sorts the rows it reads:\n%s",
					strings.Join(steps, "\n"))
			}
		})
	}

	// EVERY READER IN THE SOURCE IS ONE OF THEM.
	for _, literal := range storetest.StatementLiterals(t, "internal/iamdomain", covered...) {
		if !slices.ContainsFunc(planned, func(statement string) bool {
			return strings.Contains(statement, literal.Text)
		}) {
			t.Errorf("%s reads a directory table in a statement this test "+
				"does not plan — register it with the reads, the censuses or "+
				"the passes:\n%s", literal.File, literal.Text)
		}
	}

	// AND EVERY INDEX IS IN SOME PLAN.
	for _, index := range storetest.IndexesOn(t, db, covered) {
		if !used[index] {
			t.Errorf("no registered statement reaches %s: an index no plan "+
				"reaches is a write on every apply for no reader", index)
		}
	}
}

// directoryPlanned is one registered statement and the arguments it is
// planned with.
type directoryPlanned struct {
	sql  string
	args []any
}

// directoryIndexed is a registered read and the index it is held to: sought,
// or walked in its own order with no sort — and a sought read that is about
// the index's order (ordered) is held to no sort either.
type directoryIndexed struct {
	directoryPlanned
	index   string
	walk    bool
	ordered bool
}

// directoryReads is every read or write of one row by its key, every lookup of
// a value's holder, every probe or sweep of one bucket and every page, each
// seeking its index or walking it in its order, and one about that order sorts
// nothing. The three lookups (login, email, seat_lookup) are PLAIN: a bound
// value cannot be proved to satisfy a partial index, and on this driver every
// lookup then scanned every person. Mutation: make the login index partial and
// the login's sighting and holder lookup read `SCAN iam_people`; drop
// `iam_history_version_idx` and every page of the trail reads it whole; drop
// `iam_removed_seat_idx` and a seat's removed holder and a bind's leavers read
// every tombstone.
func directoryReads() map[string]directoryIndexed {
	const (
		person     = "sqlite_autoindex_iam_people_1"
		invitation = "sqlite_autoindex_iam_invites_1"

		login     = "iam_people_login_idx"
		address   = "iam_people_email_idx"
		seat      = "iam_people_seat_lookup_idx"
		invitedAt = "iam_invites_email_idx"
		invitedTo = "iam_invites_seat_idx"
		open      = "iam_invites_open_idx"
		spent     = "iam_invites_spent_idx"

		removedSeat = "iam_removed_seat_idx"
	)
	seek := func(index, statement string, args ...any) directoryIndexed {
		return directoryIndexed{directoryPlanned: directoryPlanned{statement, args}, index: index}
	}
	walk := func(index, statement string, args []any) directoryIndexed {
		return directoryIndexed{directoryPlanned: directoryPlanned{statement, args}, index: index, walk: true}
	}
	inOrder := func(index, statement string, args []any) directoryIndexed {
		return directoryIndexed{directoryPlanned: directoryPlanned{statement, args}, index: index, ordered: true}
	}
	out := map[string]directoryIndexed{
		// ONE PERSON'S ROW, by its key: the appliers', the decides' and the
		// readers'.
		"a person's login":                seek(person, personLoginStatement, "p1"),
		"a person's version":              seek(person, personVersionStatement, "p1"),
		"a person's content written":      seek(person, writePersonStatement, "person", "active", "", "", 1, 1, "{}", "p1", 1),
		"a person's identity before":      seek(person, identityBeforeStatement, "p1", 1),
		"a person's identity written":     seek(person, writeIdentityStatement, "jane.doe", "founder", 1, 1, "p1", 1),
		"a person's stage before":         seek(person, stageBeforeStatement, "p1", 1),
		"a person's stage written":        seek(person, writeStageStatement, "active", "{}", 1, 1, "p1", 1),
		"a person removed":                seek(person, removePersonStatement, "p1"),
		"a person enrolled":               seek(person, enrolledStatement, "p1"),
		"a person's unique values":        seek(person, heldIdentifiersStatement, "p1"),
		"a person's kind and values":      seek(person, identityOfStatement, "p1"),
		"a person's document and stage":   seek(person, heldPersonStatement, "p1"),
		"a person's standing":             seek(person, personStandingStatement, "p1"),
		"a person's directory row":        seek(person, personRowStatement, "p1"),
		"a person's guards":               seek(person, personGuardsStatement, "p1", "p1"),
		"a swept person's document":       seek(person, personDocumentStatement, "p1"),
		"a swept person's document again": seek(person, sweptDocumentStatement, "{}", 1, "p1"),
		"a reset link's person":           seek(person, resetLinkStatement, "c1", string(MethodReset)),

		// ONE INVITATION, by its key.
		"an invitation's state":      seek(invitation, invitationStateStatement, "i1"),
		"an invitation's person":     seek(invitation, invitationInStatement, "i1"),
		"an invitation by id":        seek(invitation, invitationByIDStatement, "i1"),
		"an invitation spent":        seek(invitation, spendInvitationStatement, 1, "p1", 1, "i1"),
		"an invitation cancelled":    seek(invitation, cancelInvitationStatement, "i1"),
		"an invitation erased":       seek(invitation, eraseInvitationStatement, "{}", "i1"),
		"an invitation's seat fixed": seek(invitation, rederiveSeatStatement, "founder", "i1"),

		// WHO HOLDS A VALUE: the sightings a sign-in resolves, the holder
		// every directory decision refuses a value to, and the open
		// invitation that holds an address or a seat.
		"a login's sighting":        seek(login, sightingByLogin.statement, "jane.doe"),
		"an address's sighting":     seek(address, sightingByBlind.statement, "blind"),
		"a login taken":             seek(login, takenStatements[UniqueLogin], "jane.doe", "p1"),
		"an address taken":          seek(address, takenStatements[UniqueEmail], "blind", "p1"),
		"a seat taken":              seek(seat, takenStatements[UniqueSeat], "founder", "p1"),
		"an address invited":        seek(invitedAt, invitedStatements[UniqueEmail], "blind", 1),
		"a seat invited":            seek(invitedTo, invitedStatements[UniqueSeat], "founder", 1),
		"a removed seat's holder":   seek(seat, removedHoldersStatement),
		"one seat's bindings":       seek(seat, seatBindingStatement, "founder"),
		"a bucket's lapsed invites": seek(open, sweepOpenInvitationsStatement, 1, 1, 100),
		"a bucket's spent invites":  seek(spent, sweepSpentInvitationsStatement, 1, 1, 100),
		"a bucket's lapsed probe":   seek(open, dueOpenInvitationsStatement, 1, 1),
		"a bucket's spent probe":    seek(spent, dueSpentInvitationsStatement, 1, 1),

		// THE TRAIL'S: a horizon resolved to a position, one bucket's range
		// delete and its probe, and a removal's rows.
		"a horizon's position":      seek("iam_history_class_idx", trailBoundStatement, "change", 1),
		"a bucket's trail swept":    seek("iam_history_sweep_idx", sweepTrailStatement, 1, "change", 1, 100),
		"a bucket's trail probe":    seek("iam_history_sweep_idx", dueTrailStatement, 1, "change", 1),
		"a removal's trail":         seek("iam_history_object_idx", erasedTrailStatement("(?)"), "p1", "blind"),
		"a machine's removal trail": seek("iam_history_person_idx", erasedTrailStatement("('')"), "p1"),
		"a trail row erased":        seek("sqlite_autoindex_iam_history_1", eraseTrailStatement, "{}", "h1"),
	}
	// ONE SEAT'S REMOVALS NO BIND HAS STAMPED (SeatHolder's second seek, a
	// bind's leavers), sought by the tombstones' seat index (replicated 0122)
	// in its third column's order, the person's id. A tombstone is permanent,
	// so without the index each read is every removal the company ever made.
	out["one seat's removed holder"] = inOrder(removedSeat, seatRemovedHolderStatement, []any{"founder"})
	out["one seat's leavers"] = inOrder(removedSeat, leaversStatement, []any{"founder"})

	// THE BINDINGS IN SEAT ORDER (SeatClaims', SeatHolders'), walked through
	// the partial index whose predicate their WHERE is; without it, every
	// person plus a sort on every seat listing and party-registry rebuild.
	out["the seat claims' bindings"] = walk("iam_people_seat_claim_idx", seatClaimBindingsStatement, nil)
	out["the seat holders' bindings"] = walk("iam_people_seat_claim_idx", seatHoldersStatement, nil)

	// THE DIRECTORY'S PAGES, in the order of the people's own key, and the
	// invitations' in theirs.
	statement, args := peoplePageStatement(PeopleQuery{After: "p0"}, DefaultPageSize)
	out["a page of the directory"] = inOrder(person, statement, args)
	statement, args = peoplePageStatement(PeopleQuery{After: "p0", Stage: iam.StageActive}, DefaultPageSize)
	out["a page of one stage"] = inOrder("iam_people_stage_idx", statement, args)
	statement, args = invitationsPageStatement(InvitationsQuery{After: "i0", All: true}, DefaultPageSize)
	out["a page of every invitation"] = inOrder(invitation, statement, args)

	// THE TRAIL'S PAGES, newest first by position: the whole trail and a
	// window walk or seek the position's index, one person's trail seeks
	// theirs (replicated 0069), and an instant resolves to a position by
	// walking the position's index from the newest row.
	for name, page := range map[string]struct {
		q     HistoryQuery
		index string
		walk  bool
	}{
		"the trail":           {HistoryQuery{}, "iam_history_version_idx", true},
		"one op's trail":      {HistoryQuery{Op: OpEnrol}, "iam_history_version_idx", true},
		"the trail before":    {HistoryQuery{Before: 10}, "iam_history_version_idx", false},
		"a window of a trail": {HistoryQuery{Since: 1, Before: 10}, "iam_history_version_idx", false},
		"one person's trail":  {HistoryQuery{Person: "p1"}, "iam_history_person_idx", false},
		"one person's before": {HistoryQuery{Person: "p1", Before: 10}, "iam_history_person_idx", false},
	} {
		statement, args := historyPageStatement(page.q, DefaultHistoryPage)
		read := inOrder(page.index, statement, args)
		read.walk = page.walk
		out[name] = read
	}
	out["an instant's position"] = walk("iam_history_version_idx", positionAtStatement, []any{1})
	return out
}

// directoryCensuses is every statement whose question names a set of rows it
// reads whole (the listings' windows, the open invitations the seat claims
// read, the existence questions), held to no heap scan of a covered table.
// Mutation: drop `iam_people_kind_idx` and the existence question reads the
// heap.
func directoryCensuses() map[string]directoryPlanned {
	out := map[string]directoryPlanned{
		"the seat claims' invitations": {openSeatInvitationsStatement, []any{1}},
		"any person":                   {anyPersonStatement, []any{"person"}},
		"any blind":                    {anyBlindStatement, nil},
	}
	// A PAGE OF THE OPEN INVITATIONS reads every open one through the partial
	// index whose predicate is the page's own, and sorts them: a set bounded
	// by what is outstanding plus what aged out within the week the sweep
	// waits, not by history.
	statement, args := invitationsPageStatement(InvitationsQuery{After: "i0"}, DefaultPageSize)
	out["a page of the open invitations"] = directoryPlanned{statement, args}

	// A SEARCH OF THE DIRECTORY matches a term anywhere in a login or a seat,
	// which no index orders by: it reads the people in their key's order
	// until a page of them matches.
	statement, args = peoplePageStatement(PeopleQuery{After: "p0", Q: "jane"}, DefaultPageSize)
	out["a page of a search"] = directoryPlanned{statement, args}
	return out
}

// directoryPasses is every statement that reads every row it names by
// definition (a keyring rotation's counts, every invitation's seat re-derived),
// held to no sort, and a removal's read of the invitations, held to no index ON
// PURPOSE: the table holds only invitations outstanding or a week past, and a
// removal reads it once, so an index on its arm by the person a redemption
// created would be a write on every invitation for that one read.
func directoryPasses() map[string]directoryPlanned {
	return map[string]directoryPlanned{
		"every invitation's seat":    {invitationSeatsStatement, nil},
		"every removal's seat":       {removalSeatsStatement, nil},
		"every sealed value":         {sealedPeopleStatement, nil},
		"every open sealed address":  {sealedInvitationsStatement, []any{1}},
		"every sealed name":          {sealedNamesStatement, nil},
		"every sealed address":       {sealedAddressesStatement, nil},
		"a removal's invitations":    {erasedInvitationsStatement("(?)"), []any{"p1", "blind"}},
		"a machine's removal erases": {erasedInvitationsStatement("('')"), []any{"p1"}},
	}
}
