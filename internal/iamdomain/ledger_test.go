package iamdomain_test

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE LEDGER ANSWERS FIRST, and what that asks of this domain's writer.
//
// The framework answers an operation its ledger already holds BEFORE it runs a
// decide ([statelog.Snap.Held]), collapsed into the copy that landed
// ([statelog.Result.Collapsed]) — so whatever a gesture computes inside its
// decide is, on that answer, a value from a decision nothing published, or
// from none at all. The cases here are the gestures whose answer IS such a
// value, and each is held to handing out nothing it cannot vouch for.

// A RETRIED INVALIDATION IS NOT ANNOUNCED AGAIN.
//
// The generation an invalidation announces is read and incremented inside its
// decide, and the retry the ledger answers never runs one — so it announced
// generation zero, beside a row that says the company moved to one.
//
// Mutation: announce a collapsed result and the retry announces generation 0.
func TestARetriedInvalidationIsNotAnnouncedAgain(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	op := statelog.NewOpID(time.Now(), "invalidate")
	if _, err := rig.writer.InvalidateAll(rig.t.Context(), op,
		"restored from a backup"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	rig.drain()
	if seen := rig.events.take(); len(seen) != 1 {
		t.Fatalf("the invalidation announced %d events, want 1", len(seen))
	}
	again, err := rig.writer.InvalidateAll(rig.t.Context(), op,
		"restored from a backup")
	if err != nil || !again.Collapsed || again.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the retry answered %+v (%v), want applied, collapsed into "+
			"the first copy", again, err)
	}
	if seen := rig.events.take(); len(seen) != 0 {
		t.Errorf("the retry announced %#v — its decide never ran, so every "+
			"fact it carries describes nothing that was published", seen)
	}
}

// A RETRIED SESSION START HANDS OUT NO COUNTERS.
//
// A bearer carries the revocation epoch and the session generation its
// session was OPENED at, and both are read inside the start's decide. The
// retry the ledger answers never runs that decide, so the counters beside it
// are zeros — a bearer carrying them is one the first revocation ended long
// ago — and where a later round did run, they may be a round's that was not
// the one that landed. The generation is on no record or row to read back.
//
// Mutation: hand a collapsed start's counters out and the retry answers nil.
func TestARetriedSessionStartHandsOutNoCounters(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := enrolSomebody(t, rig, "sarah.chen")
	lineage := uuid.Must(uuid.NewV7()).String()
	start := iamdomain.SessionStart{
		Lineage: lineage, Person: person,
		AbsoluteExpiresAt: brokerAt.Add(24 * time.Hour),
		OpID:              statelog.StepOpID(lineage, "session"),
	}
	if err := rig.during(func() error {
		_, err := rig.writer.OpenSession(rig.t.Context(), start)
		return err
	}); err != nil {
		t.Fatalf("open the session: %v", err)
	}
	var opened iamdomain.SessionOpened
	err := rig.during(func() error {
		var err error
		opened, err = rig.writer.OpenSession(rig.t.Context(), start)
		return err
	})
	if !errors.Is(err, iamdomain.ErrCollapsed) || !opened.Result.Collapsed {
		t.Fatalf("the retry answered %+v (%v), want %v on a collapsed result",
			opened, err, iamdomain.ErrCollapsed)
	}
	// UNAVAILABLE, so every surface answers it as it answers a credential
	// it could not mint — 503 — and never as a fault.
	if !errors.Is(err, statelog.ErrUnavailable) {
		t.Errorf("the retry's refusal %v is not %v", err, statelog.ErrUnavailable)
	}
}

// A RETRIED MINT HANDS OUT NO GRANTS.
//
// What a token carries is cut, inside the decide, to what its owner holds in
// that snapshot; the retry the ledger answers never ran that cut, so the grants
// beside it describe no token. The copy that landed is a token nobody was
// shown.
//
// Mutation: hand a collapsed mint's grants out and the retry answers nil.
func TestARetriedMintHandsOutNoGrants(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	machine := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: machine, Kind: iam.KindMachine, Stage: iam.StageActive,
		Name: "Release pipeline", Login: "ci:release",
		Grants: []iam.Grant{iam.GrantStateRead}, OpID: operationKey(),
		Reason: "a service account",
	}); err != nil {
		t.Fatalf("enrol the service account: %v", err)
	}
	rig.drain()
	credential := uuid.Must(uuid.NewV7()).String()
	mint := iamdomain.TokenMint{
		PersonID: machine, ID: credential, Verifier: "sha256:verifier",
		Label: "release", ExpiresAt: brokerAt.Add(24 * time.Hour),
		OpID: statelog.NewOpID(time.Now(), "credentials-mint"), Reason: "a token",
	}
	if err := rig.during(func() error {
		_, err := rig.writer.MintToken(rig.t.Context(), mint)
		return err
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	var minted iamdomain.TokenMinted
	err := rig.during(func() error {
		var err error
		minted, err = rig.writer.MintToken(rig.t.Context(), mint)
		return err
	})
	if !errors.Is(err, iamdomain.ErrCollapsed) || !minted.Result.Collapsed ||
		len(minted.Grants) != 0 {
		t.Fatalf("the retry answered %+v (%v), want %v and no grants",
			minted, err, iamdomain.ErrCollapsed)
	}
}

// A REDEMPTION RETRIED AFTER IT LANDED IS A LINK ALREADY USED.
//
// A retry of the enrolment a redemption made is answered by the ledger before
// the decide that refuses a spent link runs — and answered as landed, the
// sign-in surface opens a session on it: whoever holds the link and the first
// password signs in, past any second factor the person enrolled since.
//
// Mutation: answer a collapsed redemption as it came back and the retry lands.
func TestARedemptionRetriedAfterItLandedIsALinkAlreadyUsed(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	issued, err := inviteFor(t, rig, "joiner@example.com", "")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := redeemAs(t, rig, issued, "joiner@example.com", issued.Secret, ""); err != nil {
		t.Fatalf("the redemption: %v", err)
	}
	rig.drain()
	if _, err := redeemAs(t, rig, issued, "joiner@example.com", issued.Secret,
		""); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("the redemption retried after it landed answered %v, want %v",
			err, iamdomain.ErrRefused)
	}
}

// A WRITE ABOUT A REMOVED PERSON IS REFUSED BEFORE IT IS PUBLISHED.
//
// A record whose subject is not the person's own — a session, a claim, a
// spend — names its person in its payload, which the applier's removal gate
// decodes and the publisher's reader of the gates is never handed. Published
// for somebody already removed, it was dropped by every node and read back by
// its own publisher as a record applied without a ledger row: an error that
// said the estate was broken, about a person who simply was not there.
//
// Mutation: drop the refusal from the writer and the log moves, answering a
// fault rather than the deletion.
func TestAWriteAboutARemovedPersonIsRefusedBeforeItIsPublished(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := enrolSomebody(t, rig, "leaver.one")
	if err := rig.during(func() error {
		_, err := rig.writer.Remove(rig.t.Context(), person, operationKey(), "left")
		return err
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	rig.drain()
	end, err := rig.log.End(t.Context())
	if err != nil {
		t.Fatalf("End: %v", err)
	}
	lineage := uuid.Must(uuid.NewV7()).String()
	err = rig.during(func() error {
		_, err := rig.writer.OpenSession(rig.t.Context(), iamdomain.SessionStart{
			Lineage: lineage, Person: person,
			AbsoluteExpiresAt: brokerAt.Add(24 * time.Hour),
			OpID:              statelog.StepOpID(lineage, "session"),
		})
		return err
	})
	var refused *statelog.Unavailable
	if !errors.As(err, &refused) || refused.Reason != statelog.ReasonDeleted {
		t.Fatalf("a session for a removed person answered %v, want the "+
			"deletion refusal", err)
	}
	if after, _ := rig.log.End(t.Context()); after != end {
		t.Errorf("the log moved from %d to %d — the record was published for a "+
			"person every node drops", end, after)
	}
}

// A RETRIED ISSUE OF AN INVITATION THE SWEEP COLLECTED IS A KEY ALREADY USED.
//
// The ledger answers the retry before the issue's decide runs, so its terms and
// deadline are read back from the invitation's own row — and a row this node
// APPLIED and no longer holds is one the retention sweep collected, which it
// does only to an invitation redeemed or aged out. Read as "not applied here
// yet", the retry was told to come back under the same key, and met the same
// absence on every attempt for as long as the ledger kept the operation.
//
// Mutation: read an absent row as not yet applied whatever the result says, and
// the retry answers unavailable.
func TestARetriedIssueOfACollectedInvitationIsAKeyAlreadyUsed(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	mint := iamdomain.InviteMint{
		Email: "lapsed@example.com", Grants: []iam.Grant{iam.GrantStateRead},
		// LAPSED LONG BEFORE THE SWEEP BELOW RUNS, which is what makes the
		// row one it collects.
		ExpiresAt: brokerAt.Add(168 * time.Hour),
		OpID:      operationKey(), Reason: "onboarding",
	}
	issue := func() (iamdomain.InviteIssued, error) {
		var issued iamdomain.InviteIssued
		err := rig.during(func() error {
			var err error
			issued, err = rig.writer.Invite(rig.t.Context(), mint)
			return err
		})
		return issued, err
	}
	first, err := issue()
	if err != nil || first.Outcome != statelog.OutcomeApplied {
		t.Fatalf("issue: %+v (%v), want applied", first.Result, err)
	}
	rig.drain()
	if err := rig.during(func() error {
		_, err := rig.sweeper(time.Now()).Sweep(t.Context(), defaultHorizons)
		return err
	}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	rig.drain()
	if held := rig.column(`SELECT id FROM iam_invites WHERE id = ?`,
		first.ID); len(held) != 0 {
		t.Fatalf("the sweep left invitation %s in place, so this case is not "+
			"reading a collected row at all", first.ID)
	}
	again, err := issue()
	if !errors.Is(err, iamdomain.ErrOperationReused) ||
		errors.Is(err, statelog.ErrUnavailable) {
		t.Errorf("the retry of a collected invitation's issue answered %+v (%v), "+
			"want %v — a retry of the same key can never find the row again",
			again.Result, err, iamdomain.ErrOperationReused)
	}
}

// EVERY OPERATION A GESTURE DERIVES CARRIES ITS MINT INSTANT.
//
// The publisher judges whether its ledger can vouch for a retry by the instant
// an id was minted at, read off the id itself; an id outside the grammar reads
// as minted at the epoch, and once the ledger has lost a row of its kind —
// every month for most kinds, every hour for a session's — every write under
// one is answered `unknown` without being published. An enrolment's steps named themselves with a colon
// suffix and the sweep with a string of its own; both are in the grammar now.
//
// Mutation: go back to either spelling and the ledger holds an id with no
// instant.
func TestEveryOperationAGestureDerivesCarriesItsMintInstant(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	key := operationKey()
	person, err := iamdomain.CreatedPersonID(key)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com", Login: "sarah.chen",
		OpID: key, Reason: "the joiner",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	wall := time.Now().UTC()
	lineage := uuid.Must(uuid.NewV7()).String()
	if err := rig.during(func() error {
		_, err := rig.writer.OpenSession(rig.t.Context(), iamdomain.SessionStart{
			Lineage: lineage, Person: person, AbsoluteExpiresAt: wall.Add(time.Hour),
			OpID: statelog.StepOpID(lineage, "session"),
		})
		return err
	}); err != nil {
		t.Fatalf("open a session: %v", err)
	}
	sweeper := rig.sweeper(wall.Add(defaultHorizons.Sessions + 25*time.Hour))
	var report iamdomain.SweepReport
	if err := rig.during(func() error {
		report, err = sweeper.Sweep(t.Context(), defaultHorizons)
		return err
	}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	rig.drain()
	if len(report.Published) == 0 {
		t.Fatalf("the sweep published nothing (plan %+v), so this case is not "+
			"reading a sweep's id at all", report.Plan)
	}
	keyAt, _ := statelog.OpMintedAt(key)
	sweeps := 0
	for _, op := range rig.column(`SELECT op_id FROM iam_ops ORDER BY op_id`) {
		at, minted := statelog.OpMintedAt(op)
		switch {
		case !minted:
			t.Errorf("the ledger holds %q, which carries no mint instant", op)
		case len(op) > len(key) && op[:len(key)] == key && !at.Equal(keyAt):
			t.Errorf("the enrolment's step %q is minted at %s, want the "+
				"gesture's %s", op, at, keyAt)
		}
		if env := rig.envelopeOf(op); env.Op == iamdomain.OpSweep {
			sweeps++
			if !at.Equal(report.Plan.At.Truncate(time.Millisecond)) {
				t.Errorf("the sweep's id %q is minted at %s, want its plan's "+
					"instant %s", op, at, report.Plan.At)
			}
		}
	}
	if sweeps != len(report.Published) {
		t.Errorf("the ledger holds %d sweep operations for %d published buckets",
			sweeps, len(report.Published))
	}
}

// enrolSomebody enrols one person holding login and answers their id.
func enrolSomebody(t *testing.T, rig *writeRig, login string) string {
	t.Helper()
	person := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Some Body", Email: login + "@example.com", Login: login,
		OpID: operationKey(), Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol %s: %v", login, err)
	}
	rig.drain()
	return person
}

// envelopeOf is the envelope of the record the ledger names for op, read back
// off the log.
func (r *writeRig) envelopeOf(op string) iamdomain.RecordEnvelope {
	r.t.Helper()
	positions := r.column(`SELECT position FROM iam_ops WHERE op_id = ?`, op)
	if len(positions) != 1 {
		r.t.Fatalf("the ledger names %d positions for %q", len(positions), op)
	}
	packed, err := strconv.ParseInt(positions[0], 10, 64)
	if err != nil {
		r.t.Fatalf("the ledger's position for %q reads %q: %v", op, positions[0], err)
	}
	_, framed, _, ok, err := r.log.At(r.t.Context(), uint64(packed)&(1<<40-1))
	if err != nil || !ok {
		r.t.Fatalf("read the record at %d: %v (held %t)", packed, err, ok)
	}
	body, verdict := r.verifier.Open(framed)
	if verdict != statelog.Verified {
		r.t.Fatalf("the record at %d did not verify: %s", packed, verdict)
	}
	env, err := iamdomain.DecodeEnvelope(body)
	if err != nil {
		r.t.Fatalf("decode the record at %d: %v", packed, err)
	}
	return env
}
