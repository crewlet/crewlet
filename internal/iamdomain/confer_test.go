package iamdomain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// WHAT AN ENROLMENT MAY CONFER, AND ON WHOSE AUTHORITY.
//
// An enrolment is a grant change from nothing to what it carries, and it was
// held to nothing: a party holding people:manage alone could enrol somebody
// carrying secrets:read — or a colleague holding every grant, and then sign in
// as them. [iamdomain.Writer.UpdatePerson] already refused conferring what the
// writer does not hold; these cases hold the enrolment and the invitation to
// the same rule, and hold the two enrolments that are NOT the writer's to
// authorise — the first person and a redemption — to the basis each names.

// nodeWriter is the party a running node's own writer acts as: the grants
// `internal/engine` gives it and nothing else.
func nodeWriter(rig *writeRig) *iamdomain.Writer {
	return rig.writer.As("node-a", iam.KindMachine,
		[]iam.Grant{iam.GrantFleetOperate, iamdomain.AdminGrant})
}

// narrowAdmin manages people and holds nothing else.
func narrowAdmin(rig *writeRig) *iamdomain.Writer {
	return rig.writer.As("ana.admin", iam.KindPerson,
		[]iam.Grant{iamdomain.AdminGrant})
}

func TestAnEnrolmentConfersOnlyWhatItsWriterHolds(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	enrol := func(w *iamdomain.Writer, op, login string, grants []iam.Grant) error {
		return rig.draining(func() error {
			_, err := w.Enrol(rig.t.Context(), iamdomain.Enrolment{
				PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindMachine,
				Stage: iam.StageActive, Login: login, Grants: grants,
				OpID: op, Reason: "a pipeline",
			})
			return err
		})
	}
	if err := enrol(narrowAdmin(rig), "op-widen", "ci:widen",
		[]iam.Grant{iam.GrantSecretRead}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("a party holding only %s enrolled somebody carrying "+
			"secrets:read (%v)", iamdomain.AdminGrant, err)
	}
	// REFUSED BEFORE THE FIRST CLAIM, so the refusal leaves nothing behind:
	// the writer's own grants need no read, and a claimed login behind a
	// refusal that was knowable up front is residue for nothing.
	rig.drain()
	if got := rig.column(`SELECT id FROM iam_people`); len(got) != 0 {
		t.Errorf("a refused enrolment left rows behind: %v", got)
	}
	// AND THE CONTROL: what the writer holds, it may confer.
	if err := enrol(narrowAdmin(rig), "op-within", "ci:within",
		[]iam.Grant{iamdomain.AdminGrant}); err != nil {
		t.Errorf("a party holding %s was refused conferring it: %v",
			iamdomain.AdminGrant, err)
	}
}

func TestAnInvitationConfersOnlyWhatItsIssuerHolds(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	invite := func(w *iamdomain.Writer, address, op string, grants []iam.Grant) error {
		return rig.draining(func() error {
			_, err := w.Invite(rig.t.Context(), iamdomain.InviteMint{
				ID: uuid.Must(uuid.NewV7()).String(), Email: address,
				Grants: grants, ExpiresAt: brokerAt.Add(168 * time.Hour),
				OpID: op, Reason: "onboarding",
			})
			return err
		})
	}
	if err := invite(narrowAdmin(rig), "sarah@example.com", "op-widen",
		[]iam.Grant{iam.GrantConfigWrite}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("a party holding only %s issued an invitation conferring "+
			"config:write (%v) — the redemption would hand out what the "+
			"issuer never held", iamdomain.AdminGrant, err)
	}
	if err := invite(narrowAdmin(rig), "ravi@example.com", "op-within",
		[]iam.Grant{iamdomain.AdminGrant}); err != nil {
		t.Errorf("an invitation conferring what the issuer holds was "+
			"refused: %v", err)
	}
}

// A REDEMPTION CONFERS WHAT THE INVITATION SAID, and the node's own writer —
// which is what processes every redemption — may not stand in for it.
func TestARedemptionConfersWhatTheInvitationSaid(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	offered := []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite}
	issue := func(address string) string {
		t.Helper()
		id := uuid.Must(uuid.NewV7()).String()
		if err := rig.draining(func() error {
			_, err := rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
				ID: id, Email: address, Grants: offered,
				Colleague: iam.ColleagueRead,
				ExpiresAt: brokerAt.Add(168 * time.Hour),
				OpID:      "op-invite-" + id, Reason: "onboarding",
			})
			return err
		}); err != nil {
			t.Fatalf("invite %s: %v", address, err)
		}
		rig.drain()
		return id
	}
	redeem := func(invitation, address string, grants []iam.Grant,
		colleague iam.Colleague) error {

		person := uuid.Must(uuid.NewV7()).String()
		return rig.draining(func() error {
			_, err := nodeWriter(rig).Enrol(rig.t.Context(), iamdomain.Enrolment{
				PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
				Name: "A joiner", Email: address, Grants: grants,
				Colleague: colleague, Invitation: invitation,
				OpID: "op-redeem-" + person, Reason: "redeemed an invitation",
			})
			return err
		})
	}

	sarah := issue("sarah@example.com")
	if err := redeem(sarah, "sarah@example.com", offered,
		iam.ColleagueRead); err != nil {
		t.Fatalf("redeeming exactly what was offered was refused: %v — the "+
			"node's writer holds none of it, and the invitation is the "+
			"authority", err)
	}

	for _, refused := range []struct {
		name      string
		address   string
		grants    []iam.Grant
		colleague iam.Colleague
		other     string
	}{
		{"a grant the invitation did not carry", "a1@example.com",
			[]iam.Grant{iam.GrantStateRead, iam.GrantSecretRead}, iam.ColleagueRead, ""},
		{"more reach than the invitation offered", "a2@example.com",
			offered, iam.ColleagueWrite, ""},
		{"an address the invitation was not issued to", "a3@example.com",
			offered, iam.ColleagueRead, "someone-else@example.com"},
	} {
		invitation := issue(refused.address)
		address := refused.address
		if refused.other != "" {
			address = refused.other
		}
		if err := redeem(invitation, address, refused.grants,
			refused.colleague); !errors.Is(err, iamdomain.ErrRefused) {
			t.Errorf("a redemption asking for %s was not refused (%v)",
				refused.name, err)
		}
	}

	// A SPENT LINK IS NOBODY'S AUTHORITY, however many times it is shown —
	// including once the address it enrolled is free again. The spend binds
	// the address to whoever redeemed it, so the address claim refuses a
	// second redemption while they hold it; what is left to refuse is the
	// link outliving them, which is what releasing their address stands in
	// for here.
	spent := issue("spent@example.com")
	blind := blindOf(t, "spent@example.com")
	redeemer := uuid.Must(uuid.NewV7()).String()
	if err := rig.draining(func() error {
		if _, err := rig.writer.SpendInvitation(rig.t.Context(),
			iamdomain.InvitationSpend{
				ID: spent, Blind: blind, Person: redeemer,
				OpID: "op-spend", Reason: "redeemed",
			}); err != nil {
			return err
		}
		_, err := rig.writer.Release(rig.t.Context(), iamdomain.KindEmail,
			blind, redeemer, "op-release", "the redeemer left")
		return err
	}); err != nil {
		t.Fatalf("spend and release: %v", err)
	}
	rig.drain()
	if err := redeem(spent, "spent@example.com", offered,
		iam.ColleagueRead); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("an invitation somebody already redeemed enrolled a second "+
			"person once the first had gone (%v)", err)
	}

	// AND WITHOUT NAMING THE INVITATION the node writer is only itself: it
	// holds neither state:read nor work:write, so it may confer neither.
	if err := redeem("", "unnamed@example.com", offered,
		iam.ColleagueRead); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("the node's own writer conferred grants it does not hold "+
			"with no invitation behind them (%v)", err)
	}
}

// THE FIRST PERSON MAY CARRY THE CEILING, and the exemption closes behind them.
func TestTheFirstPersonMayCarryTheCeilingAndNobodyAfterThem(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	mint := func(id string, expires time.Time) {
		t.Helper()
		if err := rig.draining(func() error {
			_, err := rig.writer.MintBootstrap(rig.t.Context(),
				iamdomain.BootstrapMint{
					ID: id, Verifier: id, MintedBy: "node-a",
					ExpiresAt: expires, OpID: "op-mint-" + id,
					Reason: "a fresh estate",
				})
			return err
		}); err != nil {
			t.Fatalf("mint %s: %v", id, err)
		}
		rig.drain()
	}
	first := func(code, login string) error {
		person := uuid.Must(uuid.NewV7()).String()
		return rig.draining(func() error {
			_, err := nodeWriter(rig).Enrol(rig.t.Context(), iamdomain.Enrolment{
				PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
				Name: "The founder", Email: login + "@example.com",
				Grants: iam.AllGrants, Colleague: iam.ColleagueWrite,
				BootstrapCode: code,
				OpID:          "op-first-" + person, Reason: "the first operator",
			})
			return err
		})
	}

	// A CODE NOBODY MINTED, and one that has aged out, are nobody's way in,
	// even on an empty estate.
	if err := first("no-such-code", "ghost"); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("a code that is not on the log created the first person (%v)",
			err)
	}
	mint("stale-code", brokerAt.Add(-time.Minute))
	if err := first("stale-code", "late"); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("an aged-out code created the first person (%v)", err)
	}

	mint("live-code", brokerAt.Add(24*time.Hour))
	if err := first("live-code", "founder"); err != nil {
		t.Fatalf("the first person was refused the ceiling on a live code: "+
			"%v — the node's writer could never confer it on its own grants, "+
			"and the code is the one stated exemption", err)
	}
	// THE EXEMPTION IS CLOSED THE MOMENT SOMEBODY EXISTS, even though this
	// code has not been spent yet — spending it is a second record, and the
	// window between the two is exactly when a second caller would try.
	if err := first("live-code", "second"); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("a second person was created on the first-person exemption "+
			"(%v)", err)
	}
	// AND WITHOUT THE CODE the node writer confers only what it holds.
	person := uuid.Must(uuid.NewV7()).String()
	if err := rig.draining(func() error {
		_, err := nodeWriter(rig).Enrol(rig.t.Context(), iamdomain.Enrolment{
			PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: "Nobody", Email: "nobody@example.com", Grants: iam.AllGrants,
			OpID: "op-nobasis", Reason: "no basis",
		})
		return err
	}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("the node's own writer conferred the whole ceiling with no "+
			"code behind it (%v)", err)
	}
}

// blindOf is the keyed blind this rig's writer derives an address's subject
// from, which is what an invitation's spend arbitrates on.
func blindOf(t *testing.T, address string) string {
	t.Helper()
	blinder, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("NewBlinder: %v", err)
	}
	blind, err := blinder.Email(address)
	if err != nil {
		t.Fatalf("blind %s: %v", address, err)
	}
	return blind
}
