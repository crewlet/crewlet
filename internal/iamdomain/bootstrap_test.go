package iamdomain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THE FIRST PERSON'S CODE, AND EVERY PARTY THAT ASKS WHETHER THE COMPANY HAS
// STARTED.
//
// A bootstrap is a sequence — the address, the login, then the person — and
// its authority used to be read only at the last step. So a code a day old
// was refused after the founder's address and login were already claimed:
// the route answered "this company has started", the reservation left behind
// held the founder's own address against the fresh code that would have let
// them in, and every reader except the record counted that reservation as
// somebody, closing the only way into the company for good.

// bootstrapRig is the write rig with the gestures a bootstrap is made of.
type bootstrapRig struct {
	*writeRig
}

func newBootstrapRig(t *testing.T) bootstrapRig {
	return bootstrapRig{newWriteRig(t)}
}

// mint publishes one code expiring at expires and applies it.
func (r bootstrapRig) mint(id string, expires time.Time) error {
	r.t.Helper()
	err := r.draining(func() error {
		_, err := r.writer.MintBootstrap(r.t.Context(), iamdomain.BootstrapMint{
			ID: id, Verifier: id, MintedBy: "node-a", ExpiresAt: expires,
			OpID: "op-mint-" + id, Reason: "a fresh estate",
		})
		return err
	})
	r.drain()
	return err
}

// withdraw supersedes one code, as a re-issue does.
func (r bootstrapRig) withdraw(id string) {
	r.t.Helper()
	if err := r.draining(func() error {
		_, err := r.writer.WithdrawBootstrap(r.t.Context(), id,
			"op-withdraw-"+id, "superseded by a re-issued code")
		return err
	}); err != nil {
		r.t.Fatalf("withdraw %s: %v", id, err)
	}
	r.drain()
}

// found enrols the founder a code names, the way the route does: the person
// is derived from the code, and the address and login are the founder's own.
func (r bootstrapRig) found(code string) error {
	r.t.Helper()
	held, err := r.reader(r.t).BootstrapCode(r.t.Context(), code)
	if err != nil {
		r.t.Fatalf("read code %s: %v", code, err)
	}
	person := iamdomain.BootstrappedPersonID(code, held.MintedAt)
	if held.ID == "" {
		person = uuid.Must(uuid.NewV7()).String()
	}
	err = r.draining(func() error {
		_, err := nodeWriter(r.writeRig).Enrol(r.t.Context(), iamdomain.Enrolment{
			PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: "The founder", Email: "founder@example.com",
			Login: "jane.founder", Grants: iam.AllGrants,
			Colleague: iam.ColleagueWrite, BootstrapCode: code,
			OpID: "bootstrap:" + code, Reason: "the first operator",
		})
		return err
	})
	r.drain()
	return err
}

// started is the one-bit answer every surface reads.
func (r bootstrapRig) started() bool {
	r.t.Helper()
	held, err := r.reader(r.t).AnyPerson(r.t.Context())
	if err != nil {
		r.t.Fatalf("AnyPerson: %v", err)
	}
	return held
}

// A DEAD CODE IS REFUSED BEFORE ANYTHING IS CLAIMED, AND THE FRESH ONE LETS
// THE SAME FOUNDER IN.
//
// Mutation: drop the read [iamdomain.Writer.Enrol] makes of its basis before
// the first claim, and the aged-out attempt leaves the founder's address and
// login reserved — the rows check below fails, and the fresh code's enrolment
// is refused as "that address belongs to somebody" by the attempt before it.
func TestADeadCodeClaimsNothingAndTheReissuedOneLetsTheFounderIn(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)

	if err := rig.mint("aged-out", brokerAt.Add(-time.Minute)); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := rig.mint("withdrawn", brokerAt.Add(time.Hour)); err != nil {
		t.Fatalf("mint: %v", err)
	}
	rig.withdraw("withdrawn")

	for _, code := range []string{"aged-out", "withdrawn", "never-minted"} {
		err := rig.found(code)
		if !errors.Is(err, iamdomain.ErrRefused) ||
			!errors.Is(err, iamdomain.ErrBootstrapCodeDead) {
			t.Errorf("a %s code answered %v, want a refusal naming a dead "+
				"code — never the closed company", code, err)
		}
		if errors.Is(err, iamdomain.ErrBootstrapClosed) {
			t.Errorf("a %s code was answered as a company that has started "+
				"(%v), which sends its founder away from a company still "+
				"waiting for them", code, err)
		}
	}
	// NOTHING WAS CLAIMED: no reservation holds the founder's address or
	// login, and the company has not started.
	if rows := rig.column(`SELECT id FROM iam_people`); len(rows) != 0 {
		t.Errorf("a refused bootstrap left rows behind: %v — the founder's "+
			"own address is now held against their next code", rows)
	}
	if rig.started() {
		t.Error("a refused bootstrap started the company")
	}

	// THE RE-ISSUE: a fresh code, and the same founder with the same
	// address and login walks in.
	if err := rig.mint("reissued", brokerAt.Add(24*time.Hour)); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := rig.found("reissued"); err != nil {
		t.Fatalf("the founder was refused on a fresh code after a dead one: %v",
			err)
	}
	if !rig.started() {
		t.Error("the founder's enrolment did not start the company")
	}
}

// A RESERVATION IS NOBODY TO EVERY PARTY THAT ASKS, AND A PERSON IS SOMEBODY
// TO ALL OF THEM.
//
// The readers counted every row and the record counted only enrolled ones, so
// the route read "closed" from a reservation the record would have admitted a
// founder past. One predicate answers both now, and the mint asks it too.
//
// Mutation: count a reservation in the shared predicate (drop its kind filter)
// and the reservation below closes the company — AnyPerson answers true and
// the mint and the founder are both refused. Remove the mint's own decide and
// a code is minted for a company that has started.
func TestEveryPartyAgreesWhetherTheCompanyHasStarted(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)

	// SOMEBODY ELSE'S HALF-FINISHED ENROLMENT: a claimed address and no
	// person behind it.
	if err := rig.claim(iamdomain.KindEmail, blindOf(t, "someone@example.com"),
		uuid.Must(uuid.NewV7()).String(), "op-reserve"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	rig.drain()
	if rig.started() {
		t.Error("a reservation reads as somebody enrolled")
	}
	if err := rig.mint("first", brokerAt.Add(time.Hour)); err != nil {
		t.Fatalf("a reservation refused the mint of the company's first "+
			"code: %v", err)
	}
	if err := rig.found("first"); err != nil {
		t.Fatalf("a reservation closed the first-person exemption: %v", err)
	}

	// NOW SOMEBODY IS: every party says so, and says it with the closed
	// answer rather than the dead-code one.
	if !rig.started() {
		t.Error("the founder does not read as somebody enrolled")
	}
	err := rig.mint("second", brokerAt.Add(time.Hour))
	if !errors.Is(err, iamdomain.ErrBootstrapClosed) {
		t.Errorf("a code was minted for a company that has started (%v)", err)
	}
	if got := rig.column(`SELECT id FROM iam_bootstrap_codes WHERE id = 'second'`); len(got) != 0 {
		t.Errorf("the refused mint landed a row: %v", got)
	}
}

// A MINT THAT STATES NO EXPIRY IS REFUSED, because a code that never ages is a
// superuser claim in a file for as long as the file survives.
func TestACodeWithNoExpiryIsNeverMinted(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)
	if err := rig.mint("eternal", time.Time{}); !errors.Is(err, iamdomain.ErrInvalid) {
		t.Errorf("a code with no expiry was minted (%v)", err)
	}
}

// WHAT A CODE IS, ONCE, FOR EVERY READER.
//
// The route, the boot path, the re-issue and the record each decided liveness
// for themselves, and two of them disagreed about a row with no expiry. The
// cases walk every state and the boundary instant.
func TestACodesStateIsOnePredicate(t *testing.T) {
	t.Parallel()
	now := brokerAt
	minted := now.Add(-time.Hour)
	cases := []struct {
		name string
		code iamdomain.BootstrapCode
		want iamdomain.CodeState
	}{
		{"absent", iamdomain.BootstrapCode{}, iamdomain.CodeAbsent},
		{"live", iamdomain.BootstrapCode{ID: "c", MintedAt: minted,
			ExpiresAt: now.Add(time.Millisecond)}, iamdomain.CodeLive},
		// THE EXPIRY IS EXCLUSIVE: at the instant itself the code is
		// over, the reading every other lifetime here takes.
		{"at its expiry", iamdomain.BootstrapCode{ID: "c", MintedAt: minted,
			ExpiresAt: now}, iamdomain.CodeAgedOut},
		{"no expiry", iamdomain.BootstrapCode{ID: "c", MintedAt: minted},
			iamdomain.CodeAgedOut},
		{"withdrawn", iamdomain.BootstrapCode{ID: "c", MintedAt: minted,
			ExpiresAt: now.Add(time.Hour), SpentAt: now},
			iamdomain.CodeWithdrawn},
		{"redeemed", iamdomain.BootstrapCode{ID: "c", MintedAt: minted,
			ExpiresAt: now.Add(time.Hour), SpentAt: now, Person: "p"},
			iamdomain.CodeRedeemed},
		// A SPEND IS A RECORD AND AN EXPIRY A CLOCK: a code somebody
		// used stays used once its hours run out.
		{"redeemed and aged out", iamdomain.BootstrapCode{ID: "c",
			MintedAt: minted, ExpiresAt: now.Add(-time.Minute),
			SpentAt: now.Add(-time.Hour), Person: "p"}, iamdomain.CodeRedeemed},
	}
	seen := map[iamdomain.CodeState]bool{}
	for _, c := range cases {
		got := c.code.State(now)
		if got != c.want {
			t.Errorf("%s: state %q, want %q", c.name, got, c.want)
		}
		if !got.Valid() {
			t.Errorf("%s: state %q is not one this build knows", c.name, got)
		}
		seen[got] = true
	}
	for _, s := range iamdomain.CodeStates {
		if !seen[s] {
			t.Errorf("no case reaches %q", s)
		}
	}
	if iamdomain.CodeState("spent").Valid() {
		t.Error("a state this build does not know reads as valid")
	}
}

// THE READER ANSWERS EVERY CODE, AND ITS LISTING IS THE LIVE ONES.
//
// A code that no longer works still resolves to its row, which is what lets a
// founder holding one be told why rather than told they typed it wrong; and
// the outstanding listing a re-issue withdraws is exactly the codes whose
// state is live.
func TestTheReaderSaysWhatBecameOfEveryCode(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)
	for _, c := range []struct {
		id      string
		expires time.Time
	}{
		{"live", brokerAt.Add(time.Hour)},
		{"aged-out", brokerAt.Add(-time.Minute)},
		{"withdrawn", brokerAt.Add(time.Hour)},
	} {
		if err := rig.mint(c.id, c.expires); err != nil {
			t.Fatalf("mint %s: %v", c.id, err)
		}
	}
	rig.withdraw("withdrawn")

	reader := rig.reader(t)
	for id, want := range map[string]iamdomain.CodeState{
		"live": iamdomain.CodeLive, "aged-out": iamdomain.CodeAgedOut,
		"withdrawn": iamdomain.CodeWithdrawn, "nobody": iamdomain.CodeAbsent,
	} {
		code, err := reader.BootstrapCode(t.Context(), id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got := code.State(brokerAt); got != want {
			t.Errorf("code %s reads %q, want %q", id, got, want)
		}
		if want != iamdomain.CodeAbsent && code.MintedAt.IsZero() {
			t.Errorf("code %s carries no mint instant, so the founder it "+
				"names cannot be derived", id)
		}
	}
	outstanding, err := reader.OutstandingBootstrapCodes(t.Context(), brokerAt)
	if err != nil {
		t.Fatalf("outstanding: %v", err)
	}
	if len(outstanding) != 1 || outstanding[0].ID != "live" {
		t.Errorf("outstanding codes %+v, want exactly the live one", outstanding)
	}
}
