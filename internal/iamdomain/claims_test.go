package iamdomain_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A DUPLICATE CLAIM IS REPORTED, NEVER ABORTED.
//
// Ordinary traffic cannot put one address on two people — the broker refuses
// the second create at zero — and a RESTORE can, because rows copied back from
// an artefact never pass through the broker. So this case makes the duplicate
// the way a restore does, by writing the rows, and then asks the two things
// the migration's non-unique indexes were shipped for:
//
//  1. NEVER ABORTED. Records go on applying on top of the duplicate — a status
//     change on one holder, a new enrolment beside them. A UNIQUE index here
//     would have refused the restore's own rows, or aborted the next apply on
//     every node at once, which in this domain is every sign-in in the company.
//  2. REPORTED. The report names the claim kind and both holders, for the
//     address (by its blind) and for the login.
//
// And the control, before any of it: a directory with no duplicate reports
// none, so the second half is not a report that names everybody.
func TestADuplicateClaimIsReportedNeverAborted(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)

	first := uuid.Must(uuid.NewV7()).String()
	second := uuid.Must(uuid.NewV7()).String()
	for _, p := range []struct{ id, name, email, login string }{
		{first, "Sarah Chen", "sarah.chen@example.com", "sarah.chen"},
		{second, "Omar Haddad", "omar.haddad@example.com", "omar.haddad"},
	} {
		if err := rig.enrol(iamdomain.Enrolment{
			PersonID: p.id, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: p.name, Email: p.email, Login: p.login,
			OpID: "enrol-" + p.login, Reason: "a joiner",
		}); err != nil {
			t.Fatalf("enrol %s: %v", p.login, err)
		}
	}

	// THE CONTROL.
	clean, err := reader.Claims(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Claims: %v", err)
	}
	if len(clean.Duplicates) != 0 || len(clean.Orphans) != 0 {
		t.Fatalf("a directory with no duplicate reported %+v", clean)
	}

	// A RESTORE WROTE IT: the second person's row carries the first's
	// address and login, with no record behind either.
	blind := rig.column(`SELECT email_blind FROM iam_people WHERE id = ?`, first)
	if len(blind) != 1 || blind[0] == "" {
		t.Fatalf("the first person holds no address blind: %v", blind)
	}
	if _, err := rig.db.Replicated().SQL().ExecContext(t.Context(), `
		UPDATE iam_people SET email_blind = ?, login = 'sarah.chen'
		WHERE id = ?`, blind[0], second); err != nil {
		t.Fatalf("the restore's rows were refused — a unique index stands where "+
			"the migration ships a non-unique one: %v", err)
	}

	// 1. NEVER ABORTED.
	if err := rig.during(func() error {
		_, err := rig.writer.SetStage(t.Context(), second, iam.StageSuspended,
			"suspend-omar", "while the duplicate is sorted out")
		return err
	}); err != nil {
		t.Fatalf("a record on a duplicate holder did not apply: %v", err)
	}
	third := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: third, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Ines Duarte", Email: "ines.duarte@example.com",
		Login: "ines.duarte", OpID: "enrol-ines", Reason: "a joiner",
	}); err != nil {
		t.Fatalf("an enrolment beside the duplicate did not apply: %v", err)
	}
	if stage := rig.column(`SELECT stage FROM iam_people WHERE id = ?`,
		second); len(stage) != 1 || stage[0] != string(iam.StageSuspended) {
		t.Fatalf("the suspension did not land on the duplicate holder: %v", stage)
	}

	// 2. REPORTED.
	report, err := reader.Claims(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Claims: %v", err)
	}
	holders := []string{first, second}
	slices.Sort(holders)
	want := map[iamdomain.ObjectKind]string{
		iamdomain.KindEmail: blind[0],
		iamdomain.KindLogin: "sarah.chen",
	}
	if len(report.Duplicates) != len(want) {
		t.Fatalf("report = %+v, want exactly the address and the login", report.Duplicates)
	}
	for _, dup := range report.Duplicates {
		token, ok := want[dup.Kind]
		if !ok || dup.Token != token || !slices.Equal(dup.People, holders) {
			t.Errorf("duplicate %+v, want %s %q held by %v", dup, dup.Kind,
				token, holders)
		}
	}
}

// A RESERVATION AN ENROLMENT LEFT BEHIND IS REPORTED ONCE IT IS AN HOUR OLD,
// and a removal of its id is the repair.
//
// A claim taken for somebody whose content record never followed holds a
// login nobody can use. Inside the grace it is a sequence still running and is
// not reported; past it, it is named with what it holds; and removing the
// reservation's id releases its claims, after which there is nothing to report.
func TestAnOrphanedReservationIsReportedAndARemovalRepairsIt(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)

	ghost := uuid.Must(uuid.NewV7()).String()
	if err := rig.during(func() error {
		_, err := rig.writer.Claim(t.Context(), iamdomain.KindLogin,
			"ghost.person", ghost, "claim-ghost")
		return err
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// AND A RESERVATION WHOSE ONLY CLAIM WAS RELEASED holds nothing, so
	// there is nothing it keeps out of circulation and nothing to report.
	released := uuid.Must(uuid.NewV7()).String()
	if err := rig.during(func() error {
		if _, err := rig.writer.Claim(t.Context(), iamdomain.KindLogin,
			"ghost.released", released, "claim-released"); err != nil {
			return err
		}
		_, err := rig.writer.Release(t.Context(), iamdomain.KindLogin,
			"ghost.released", released, "release-released", "given back")
		return err
	}); err != nil {
		t.Fatalf("Claim and Release: %v", err)
	}

	running, err := reader.Claims(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Claims: %v", err)
	}
	if len(running.Orphans) != 0 {
		t.Fatalf("a reservation seconds old was reported as orphaned: %+v — an "+
			"enrolment still running would be named a failure", running.Orphans)
	}

	later := time.Now().Add(iamdomain.OrphanGrace + time.Minute)
	stopped, err := reader.Claims(t.Context(), later)
	if err != nil {
		t.Fatalf("Claims: %v", err)
	}
	if len(stopped.Orphans) != 1 || stopped.Orphans[0].Person != ghost ||
		stopped.Orphans[0].Login != "ghost.person" ||
		!slices.Equal(stopped.Orphans[0].Holds, []iamdomain.ObjectKind{iamdomain.KindLogin}) {
		t.Fatalf("orphans = %+v, want the ghost reservation holding its login",
			stopped.Orphans)
	}

	if err := rig.during(func() error {
		_, err := rig.writer.Remove(t.Context(), ghost, "remove-ghost",
			"an enrolment that never finished")
		return err
	}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	repaired, err := reader.Claims(t.Context(), later)
	if err != nil {
		t.Fatalf("Claims: %v", err)
	}
	if len(repaired.Orphans) != 0 {
		t.Errorf("the removal did not release the reservation: %+v", repaired.Orphans)
	}
}
