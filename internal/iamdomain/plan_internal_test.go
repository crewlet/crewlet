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
// # Why this is asserted rather than reviewed
//
// A sign-in resolves a login or an address blind to its row, every directory
// decision asks whether anybody else holds the value it is about to give, and
// an open invitation holds its address and its seat as a person does — each a
// query with a BOUND value, over a table every login in the company is a row
// of. A statement there with no index behind it reads every person in the
// company and shows up as a slow sign-in, never as a failure; an index nobody
// reads is a write on every apply of every identity record on every data node,
// for ever. The planner is the only thing that knows which a statement is, so
// this runs EXPLAIN QUERY PLAN over the statements this package runs, planned
// as a deployment plans them — with no statistics, over an empty estate.
//
// It used to be a case of internal/store's schema suite, over COPIES of these
// statements, and the copies had drifted: two of them asked for a non-empty
// seat where no statement here does, so the plan they certified was not the
// plan any node ran. Planned from the package's own named statements, a change
// to one is a change to what is planned — and planned whole, the first run
// found what the copies never could (replicated 0069): an index on the people
// no statement reaches, `/health`'s question whether anybody is enrolled
// reading the people from the heap, the trail a person pages through read
// whole and sorted on every page, and the erasure of an address's trail
// seeking every row that names nobody while the index on the address was
// reached by no statement at all.
//
// # The tables it speaks for
//
// The people, the invitations and the trail: the three tables a sign-in, a
// directory decision and an investigation read by a value or in an order. The
// credentials, the sessions and the gates are read by their keys and by the
// sweep's own indexes, and are not this test's to speak for.
//
// # What it holds
//
//   - every statement in the module's source that reads `iam_people`,
//     `iam_invites` or `iam_history` is registered here, matched whole
//     ([storetest.StatementLiterals]): a reader nobody registered is one whose
//     plan nobody checked;
//   - a READ — a point read or write by a row's key, a lookup by a bound value,
//     a bucket's sweep, a page — seeks the index it names, or walks it in its
//     own order, and one about that order sorts nothing ([directoryReads]);
//   - a CENSUS — the open invitations, a search of the directory, an existence
//     question — reads every row its question names by definition, and is held
//     to no heap scan of a covered table ([directoryCensuses]);
//   - a PASS reads every row by definition — the counts a keyring rotation
//     retires a key on, a re-derivation of every invitation's seat — so it may
//     read the heap, and is held to no sort ([directoryPasses]);
//   - and the inverse: every index the schema declares on a covered table is
//     in some registered plan.
//
// # The plain lookups, and the one partial index
//
// The three lookups (`iam_people_login_idx`, `_email_idx`, `_seat_lookup_idx`)
// are PLAIN: a PARTIAL index over the non-empty rows is one a bound value
// cannot be proved to satisfy, and measured on this driver every lookup was
// then a scan of every person. The listing of every binding is the other
// direction: a non-empty seat IS the partial `iam_people_seat_claim_idx`'s
// predicate, so it walks that index — the bound people, in seat order — where
// without it the read is every person plus a sort, on every seat listing and
// every party-registry rebuild.
//
// Mutation: make the login index partial over its non-empty rows and the
// login's sighting and holder lookup read `SCAN iam_people`; leave
// `iam_people_bucket_idx` in place and the inventory names it; drop
// `iam_people_kind_idx` and the existence question reads the heap; drop
// `iam_history_version_idx` and every page of the trail reads it whole; read
// `iam_people` in a new literal anywhere in the module without registering
// it, and the walk names the file; drop `iam_removed_seat_idx` and one seat's
// removed holder and a bind's leavers read every tombstone.
func TestTheDirectoryStatementsArePlanned(t *testing.T) {
	t.Parallel()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
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
// a value's holder and every probe or sweep of one bucket, each with the index
// it seeks.
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
	// ONE SEAT'S REMOVALS NO BIND HAS STAMPED, sought by the tombstones' own
	// seat index (replicated 0117) in its third column's order, the person's
	// id: SeatHolder's second seek and the leavers a bind stamps and scopes.
	// A tombstone is permanent, so without the index each is every removal
	// the company ever made.
	out["one seat's removed holder"] = inOrder(removedSeat, seatRemovedHolderStatement, []any{"founder"})
	out["one seat's leavers"] = inOrder(removedSeat, leaversStatement, []any{"founder"})

	// THE BINDINGS IN SEAT ORDER, walked through the partial index whose
	// predicate their WHERE is — SeatClaims' and SeatHolders' shapes.
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

	// THE TRAIL'S PAGES, newest first by position, in every shape the
	// surface asks for: the whole trail and a window of it walk or seek the
	// position's own index, one person's trail seeks theirs (replicated
	// 0069), and an instant is resolved to a position by walking the
	// position's index from the newest row.
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
// reads whole: the listings' windows in every shape the surfaces ask for, the
// open invitations the seat claims read, the trail's join to its people, and
// the existence questions.
func directoryCensuses() map[string]directoryPlanned {
	out := map[string]directoryPlanned{
		"the seat claims' invitations": {openSeatInvitationsStatement, []any{1}},
		"any person":                   {anyPersonStatement, []any{"person"}},
		"any blind":                    {anyBlindStatement, nil},
	}
	// A PAGE OF THE OPEN INVITATIONS reads every open invitation through the
	// partial index whose predicate is the page's own, and sorts them: the set
	// is what is outstanding, plus what aged out within the week the sweep
	// waits, so it is bounded by what the company has issued and not by what
	// it ever did.
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
// definition — and a removal's read of the invitations, which is held to no
// index ON PURPOSE: its arm by the person a redemption created has none, and
// needs none, because the table holds only the invitations still outstanding
// and those the sweep has not collected a week on, and a removal reads it
// once; an index there would be a write on every invitation for that one read.
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
