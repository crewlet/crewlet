package iamdomain_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE KEY DELETE IS RETRIED UNTIL IT LANDS.
//
// A removal commits its rows and then destroys the person's key, post-commit
// and best effort — so a coordination blip at that instant leaves a person
// removed from every row and still readable from every copy made before: the
// backups, the donated snapshots, the records still on the log. The key duty
// is the only thing that ever finishes that, and this case walks it through:
//
//  1. THE CONTROL, which is the reason the duty exists. With the store
//     refusing deletes, the removal applies and the name sealed into an
//     earlier copy still opens. If it did not, the case below would prove
//     nothing about retrying.
//  2. A pass while the blip lasts finds the person pending, destroys nothing
//     and SAYS so, rather than reporting a clean pass.
//  3. The first pass after the blip destroys the key, and the name that
//     opened a moment ago answers ErrShredded.
//  4. And it leaves everybody else alone: a colleague who was never removed
//     keeps a key that opens, because a duty that destroyed every key it
//     listed would be an outage with a retention policy's name.
func TestTheDekDeleteIsRetriedUntilItLands(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	sealer, err := iamdomain.NewSealer(rig.keys)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	leaver := uuid.Must(uuid.NewV7()).String()
	stayer := uuid.Must(uuid.NewV7()).String()
	for _, p := range []struct{ id, name, email, login string }{
		{leaver, "Sarah Chen", "sarah.chen@example.com", "sarah.chen"},
		{stayer, "Omar Haddad", "omar.haddad@example.com", "omar.haddad"},
	} {
		if err := rig.enrol(iamdomain.Enrolment{
			PersonID: p.id, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: p.name, Email: p.email, Login: p.login,
			OpID: "enrol-" + p.login, Reason: "a joiner",
		}); err != nil {
			t.Fatalf("enrol %s: %v", p.login, err)
		}
	}
	// THE EARLIER COPY: what a backup taken before the removal holds.
	sealed := rig.column(`SELECT name_sealed FROM iam_people WHERE id = ?`, leaver)
	kept := rig.column(`SELECT name_sealed FROM iam_people WHERE id = ?`, stayer)
	if len(sealed) != 1 || len(kept) != 1 {
		t.Fatalf("the two people were not both enrolled: %v %v", sealed, kept)
	}

	blip := errors.New("coordination store: no responders")
	rig.keys.blip(blip)
	if err := rig.during(func() error {
		_, err := rig.writer.Remove(t.Context(), leaver, "remove-sarah", "left")
		return err
	}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// 1. THE CONTROL.
	if plain, err := sealer.Open(t.Context(), leaver, iamdomain.FieldName,
		sealed[0]); err != nil || plain != "Sarah Chen" {
		t.Fatalf("with the delete refused the name should still open from an "+
			"earlier copy, and it answered (%q, %v) — so this case cannot tell a "+
			"retry from a shred that landed first time", plain, err)
	}

	// 2. A PASS DURING THE BLIP SAYS IT DID NOT FINISH.
	report, err := iamdomain.ShredKeys(t.Context(), reader, rig.keys, sealer,
		brokerAt, rig.end)
	if err == nil {
		t.Fatal("a pass that could destroy nothing reported success")
	}
	if len(report.Pending) != 1 || report.Pending[0] != leaver ||
		len(report.Destroyed) != 0 {
		t.Fatalf("pass during the blip: %+v, want the leaver pending and "+
			"nothing destroyed", report)
	}

	// 3. THE FIRST PASS AFTER IT LANDS.
	rig.keys.blip(nil)
	report, err = iamdomain.ShredKeys(t.Context(), reader, rig.keys, sealer,
		brokerAt, rig.end)
	if err != nil {
		t.Fatalf("ShredKeys: %v", err)
	}
	if len(report.Destroyed) != 1 || report.Destroyed[0] != leaver {
		t.Fatalf("pass after the blip: %+v, want the leaver's key destroyed", report)
	}
	if _, err := sealer.Open(t.Context(), leaver, iamdomain.FieldName,
		sealed[0]); !errors.Is(err, iamdomain.ErrShredded) {
		t.Fatalf("the leaver's name still opens from an earlier copy after "+
			"the duty ran (err %v)", err)
	}
	if again, err := iamdomain.ShredKeys(t.Context(), reader, rig.keys,
		sealer, brokerAt, rig.end); err != nil || len(again.Pending) != 0 {
		t.Errorf("a pass after the key landed still found work: %+v, %v", again, err)
	}

	// 4. NOBODY ELSE'S KEY.
	if plain, err := sealer.Open(t.Context(), stayer, iamdomain.FieldName,
		kept[0]); err != nil || plain != "Omar Haddad" {
		t.Fatalf("a colleague who was never removed lost their key: (%q, %v)",
			plain, err)
	}
}

// end is the identity log's last sequence as the broker holds it, which is what
// a node that has drained the log is proved against.
func (r *writeRig) end(ctx context.Context) (uint64, error) { return r.log.End(ctx) }

// behind is the log's end as a node that has not applied its tail sees it —
// the state every node is in for a moment after it boots: records past what
// this rig drained.
func (r *writeRig) behind(ctx context.Context) (uint64, error) {
	end, err := r.log.End(ctx)
	return end + 3, err
}

// A KEY NOBODY OWNS IS DESTROYED ONLY WHEN IT IS CERTAINLY NOBODY'S.
//
// Two administrators add one joiner: the second enrolment mints its key, seals
// the address under it and is refused on the address. Nothing then owns that
// key — no person, no reservation, no invitation, no removal — and nothing
// else would ever name it or destroy it, so a sealed copy of the address lives
// for the life of the deployment. It must go, and only when two things hold:
//
//  1. INSIDE THE GRACE it stays, because a key minted a moment ago is exactly
//     what an enrolment still between its mint and its first claim looks like.
//  2. On a node BEHIND THE LOG it stays past the grace too, because a row this
//     node has not applied reads exactly like a row that does not exist — and
//     the pass says it could not judge rather than reporting a clean one.
//  3. Past the grace on a node that is current, it is destroyed.
//  4. The winner keeps theirs throughout: they own a row.
func TestAKeyNobodyOwnsIsDestroyedOnlyWhenItIsCertainlyNobodys(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	sealer, err := iamdomain.NewSealer(rig.keys)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	winner := uuid.Must(uuid.NewV7()).String()
	loser := uuid.Must(uuid.NewV7()).String()
	joiner := func(id, op string) iamdomain.Enrolment {
		return iamdomain.Enrolment{
			PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: "Sarah Chen", Email: "sarah.chen@example.com",
			Login: "sarah.chen", OpID: op, Reason: "a joiner",
		}
	}
	if err := rig.enrol(joiner(winner, "enrol-1")); err != nil {
		t.Fatalf("the first enrolment: %v", err)
	}
	var claimed *iamdomain.ErrClaimed
	if err := rig.enrol(joiner(loser, "enrol-2")); !errors.As(err, &claimed) {
		t.Fatalf("the second enrolment of one address answered %v, want the "+
			"address refused", err)
	}
	loserKey := iamdomain.PersonDEKName(loser)
	if rig.keys.value(loserKey) == "" {
		t.Fatal("the refused enrolment minted no key, so this case cannot " +
			"tell a collection from a key that was never there")
	}

	// 1. INSIDE THE GRACE.
	report, err := iamdomain.ShredKeys(t.Context(), reader, rig.keys, sealer,
		brokerAt.Add(iamdomain.OrphanKeyGrace/2), rig.end)
	if err != nil {
		t.Fatalf("ShredKeys inside the grace: %v", err)
	}
	if len(report.Collected) != 0 || report.Waiting != 1 ||
		rig.keys.value(loserKey) == "" {
		t.Fatalf("inside the grace: %+v — a key a running enrolment may be "+
			"about to claim with was destroyed, or not counted as waiting", report)
	}

	// 2. PAST THE GRACE, ON A NODE THAT IS BEHIND.
	past := brokerAt.Add(2 * iamdomain.OrphanKeyGrace)
	report, err = iamdomain.ShredKeys(t.Context(), reader, rig.keys, sealer,
		past, rig.behind)
	if err != nil {
		t.Fatalf("ShredKeys behind the log: %v", err)
	}
	if len(report.Collected) != 0 || rig.keys.value(loserKey) == "" {
		t.Fatalf("behind the log: %+v — a node that cannot tell an absent "+
			"row from an unapplied one destroyed a key", report)
	}
	if !errors.Is(report.Unjudged, iamdomain.ErrNotCurrent) || report.Unproven != 1 {
		t.Errorf("behind the log the pass reported %+v, want it to say it "+
			"could not judge the one unowned key", report)
	}

	// 3. PAST THE GRACE, ON A NODE THAT IS CURRENT.
	report, err = iamdomain.ShredKeys(t.Context(), reader, rig.keys, sealer,
		past, rig.end)
	if err != nil {
		t.Fatalf("ShredKeys: %v", err)
	}
	if len(report.Collected) != 1 || report.Collected[0] != loser {
		t.Fatalf("past the grace on a current node: %+v, want the refused "+
			"enrolment's key collected", report)
	}
	if rig.keys.value(loserKey) != "" {
		t.Fatal("the pass reported the key collected and it is still there")
	}

	// 4. THE WINNER'S KEY.
	sealed := rig.column(`SELECT name_sealed FROM iam_people WHERE id = ?`, winner)
	if len(sealed) != 1 {
		t.Fatalf("the winner has no row: %v", sealed)
	}
	if plain, err := sealer.Open(t.Context(), winner, iamdomain.FieldName,
		sealed[0]); err != nil || plain != "Sarah Chen" {
		t.Fatalf("the person who owns their key lost it: (%q, %v)", plain, err)
	}
}

// A REMOVAL'S KEY IS DESTROYED ON A NODE THAT IS BEHIND, because a tombstone is
// definitive wherever it is: nobody comes back from a removal, so a node that
// holds one needs no proof it is current to finish it. Gating this arm on the
// node being current as well would keep a removed person's name readable from
// every backup for as long as some node lagged.
func TestARemovalsKeyIsDestroyedOnANodeBehindTheLog(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	sealer, err := iamdomain.NewSealer(rig.keys)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	leaver := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: leaver, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "enrol-sarah", Reason: "a joiner",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.keys.blip(errors.New("coordination store: no responders"))
	if err := rig.during(func() error {
		_, err := rig.writer.Remove(t.Context(), leaver, "remove-sarah", "left")
		return err
	}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	rig.keys.blip(nil)
	if rig.keys.value(iamdomain.PersonDEKName(leaver)) == "" {
		t.Fatal("the removal's own shred landed despite the blip, so this " +
			"case cannot show the duty finishing it")
	}

	report, err := iamdomain.ShredKeys(t.Context(), reader, rig.keys, sealer,
		brokerAt, rig.behind)
	if err != nil {
		t.Fatalf("ShredKeys: %v", err)
	}
	if len(report.Destroyed) != 1 || report.Destroyed[0] != leaver {
		t.Fatalf("a node behind the log left a removed person's key alive: %+v",
			report)
	}
}

// AN INVITATION OWNS ITS KEY FOR EXACTLY AS LONG AS ITS ROW LIVES.
//
// An invitation's address is sealed under a key minted for the invitation's own
// id, and nothing destroyed that key — not a refusal, and not the sweep that
// collects an expired invitation's row — so every address anybody ever typed
// into an invitation stayed readable from every backup for ever. The same
// ownership rule collects both, and the control is the live invitation, whose
// key must survive every pass while its row is there.
func TestAnInvitationsKeyLivesExactlyAsLongAsItsRow(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	sealer, err := iamdomain.NewSealer(rig.keys)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	const (
		live    = "018f3a9c-0000-7000-8000-0000000000d1"
		refused = "018f3a9c-0000-7000-8000-0000000000d2"
	)
	invite := func(id, op string) error {
		return rig.during(func() error {
			_, err := rig.writer.Invite(t.Context(), iamdomain.InviteMint{
				ID: id, Email: "sarah@example.com",
				Grants:    []iam.Grant{iam.GrantStateRead},
				ExpiresAt: brokerAt.Add(24 * time.Hour),
				OpID:      op, Reason: "onboarding",
			})
			return err
		})
	}
	if err := invite(live, "op-1"); err != nil {
		t.Fatalf("the first invitation: %v", err)
	}
	var claimed *iamdomain.ErrClaimed
	if err := invite(refused, "op-2"); !errors.As(err, &claimed) {
		t.Fatalf("the second invitation to one address answered %v, want it "+
			"refused on the address", err)
	}
	past := brokerAt.Add(2 * iamdomain.OrphanKeyGrace)
	report, err := iamdomain.ShredKeys(t.Context(), reader, rig.keys, sealer,
		past, rig.end)
	if err != nil {
		t.Fatalf("ShredKeys: %v", err)
	}
	if len(report.Collected) != 1 || report.Collected[0] != refused {
		t.Fatalf("the pass collected %v, want exactly the refused "+
			"invitation's key", report.Collected)
	}
	if rig.keys.value(iamdomain.PersonDEKName(live)) == "" {
		t.Fatal("a live invitation's key was destroyed while its row still " +
			"holds an address sealed under it")
	}

	// THE SWEEP COLLECTS THE ROW, and the next pass the key.
	sweeper := rig.sweeper(time.Now().UTC().Add(defaultHorizons.Changes))
	if err := rig.during(func() error {
		_, err := sweeper.Sweep(t.Context(), defaultHorizons)
		return err
	}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	rig.drain()
	if rows := rig.column(`SELECT id FROM iam_invites`); len(rows) != 0 {
		t.Fatalf("the sweep left the expired invitation %v, so this case "+
			"cannot show its key following it", rows)
	}
	report, err = iamdomain.ShredKeys(t.Context(), reader, rig.keys, sealer,
		past, rig.end)
	if err != nil {
		t.Fatalf("ShredKeys after the sweep: %v", err)
	}
	if len(report.Collected) != 1 || report.Collected[0] != live {
		t.Fatalf("after the sweep collected the invitation the pass collected "+
			"%v, want its key", report.Collected)
	}
}

// collectAfter is the key duty's destroyer with a gesture run at the one
// instant that matters: after the census judged a key nobody's, before the
// destroy of it lands.
type collectAfter struct {
	*iamdomain.Sealer
	between func()
}

func (c *collectAfter) Collect(ctx context.Context, personID string,
	version uint64) (bool, error) {

	if c.between != nil {
		between := c.between
		c.between = nil
		between()
	}
	return c.Sealer.Collect(ctx, personID, version)
}

// A KEY A RETRY RE-USES IS NEVER DESTROYED UNDER IT.
//
// A redemption names a DERIVED person, so its retry names the same one — and
// the same key its first attempt minted, however long ago that was. Here the
// first attempt mints the key and stops before its first claim lands (the
// broker answers nothing for the address), so nothing owns the key and it is
// aged by that attempt's write. Two hours later the key duty judges it
// nobody's, and between the judgement and the destroy the retry runs to
// completion, sealing the person's name and address under that key.
//
// Destroyed anyway, every value the retry sealed was unreadable from the
// moment it was written: a person enrolled with a name nobody could read and
// nothing reporting it. The retry TOUCHES the key it re-uses, and the destroy
// is of the version the census judged, so the key is spared, the pass says so,
// and the person's name opens.
func TestAKeyARetryReusesIsNeverDestroyedUnderIt(t *testing.T) {
	t.Parallel()
	var broker *silentBroker
	rig := newWriteRigWith(t, func(inner statelog.Appender) statelog.Appender {
		broker = &silentBroker{Appender: inner, on: ".email."}
		return broker
	})
	reader := rig.reader(t)
	sealer, err := iamdomain.NewSealer(rig.keys)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	person, err := iamdomain.InvitedPersonID("018f3a9c-0000-7000-8000-0000000000a7")
	if err != nil {
		t.Fatalf("InvitedPersonID: %v", err)
	}
	redemption := iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Dana Reyes", Email: "dana.reyes@example.com", Login: "dana.reyes",
		OpID: "invite:redeem:dana", Reason: "a redemption",
	}

	// THE FIRST ATTEMPT: the key is minted, the address claim goes
	// unanswered, and nothing owns the key.
	broker.silent.Store(true)
	if err := rig.draining(func() error {
		_, err := rig.writer.Enrol(t.Context(), redemption)
		return err
	}); err != nil {
		t.Fatalf("the first attempt: %v", err)
	}
	broker.silent.Store(false)
	name := iamdomain.PersonDEKName(person)
	if rig.keys.value(name) == "" {
		t.Fatal("the first attempt minted no key, so there is nothing to re-use")
	}

	// THE DUTY, two hours on, with the retry landing between its census and
	// its destroy.
	destroyer := &collectAfter{Sealer: sealer, between: func() {
		if err := rig.enrol(redemption); err != nil {
			t.Errorf("the retry: %v", err)
		}
	}}
	report, err := iamdomain.ShredKeys(t.Context(), reader, rig.keys, destroyer,
		brokerAt.Add(2*iamdomain.OrphanKeyGrace), rig.end)
	if err != nil {
		t.Fatalf("ShredKeys: %v", err)
	}
	if destroyer.between != nil {
		t.Fatalf("the pass never tried to destroy the first attempt's key (%+v), "+
			"so this case proves nothing about a destroy racing the retry", report)
	}
	if len(report.Collected) != 0 || report.Moved != 1 {
		t.Errorf("the pass reported %+v, want the key spared as written since "+
			"it was judged", report)
	}
	sealed := rig.column("SELECT name_sealed FROM iam_people WHERE id = ?", person)
	if len(sealed) != 1 {
		t.Fatalf("the retry enrolled nobody: %v", sealed)
	}
	if plain, err := sealer.Open(t.Context(), person, iamdomain.FieldName,
		sealed[0]); err != nil || plain != "Dana Reyes" {
		t.Fatalf("the retried redemption's name reads (%q, %v) — its key was "+
			"destroyed under it", plain, err)
	}
}

// A KEY A REMOVAL DESTROYED IS NEVER WRITTEN BACK BY A LATER MINT.
//
// A mint that finds a key keeps it and re-dates it, and a stale retry of a
// removed person's gesture is still a mint of that person's id. Written back,
// the destroyed key would open every copy of their name in every backup — the
// one thing a removal promises cannot happen. The touch finds no key and the
// mint makes a fresh one, under which the removed person's values stay sealed.
// Over the real secret store, because the rule lives in its conditional write.
func TestAKeyARemovalDestroyedIsNeverWrittenBackByAMint(t *testing.T) {
	t.Parallel()
	store := fleetsecrets.New(coordmem.NewFleet(), realCipher(t)).Estate()
	sealer, err := iamdomain.NewSealer(store)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	ctx := t.Context()
	id := uuid.Must(uuid.NewV7()).String()
	if err := sealer.Mint(ctx, id, "node-a", time.Now()); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	artefact, err := sealer.Seal(ctx, id, iamdomain.FieldName, "Sarah Chen")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if destroyed, err := sealer.Shred(ctx, id); err != nil || !destroyed {
		t.Fatalf("Shred = (%v, %v)", destroyed, err)
	}
	if err := sealer.Mint(ctx, id, "node-a", time.Now()); err != nil {
		t.Fatalf("the stale mint: %v", err)
	}
	if plain, err := sealer.Open(ctx, id, iamdomain.FieldName, artefact); err == nil {
		t.Fatalf("a removed person's name opens again (%q) after a later mint "+
			"of their id — the destroyed key was written back", plain)
	}
}
