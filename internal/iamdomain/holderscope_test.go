package iamdomain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// lastRecord is the newest record on the rig's log: its declared scope, and
// the person it names.
func (r *writeRig) lastRecord() (statelog.ScopeSet, string) {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	_, payload, _, ok, err := r.log.At(r.t.Context(), last)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: %v (present %v)", last, err, ok)
	}
	body, verdict := r.verifier.Open(payload)
	if verdict != statelog.Verified {
		r.t.Fatalf("record %d did not verify: %s", last, verdict)
	}
	env, err := iamdomain.DecodeEnvelope(body)
	if err != nil {
		r.t.Fatalf("decode the envelope: %v", err)
	}
	record, err := iamdomain.Decode(body)
	if err != nil {
		r.t.Fatalf("decode the record: %v", err)
	}
	return env.Scope.Resolve(env.Subject), record.Person
}

// personAwayFrom is a person id whose bucket differs from token's, so a scope
// naming the token's bucket and one naming the person's cannot be mistaken for
// each other by coincidence.
func personAwayFrom(token string) string {
	for {
		id := uuid.New().String()
		if iamdomain.BucketOf(id) != iamdomain.BucketOf(token) {
			return id
		}
	}
}

// AN IDENTITY CHANGE IS FILED UNDER ITS PERSON'S BUCKET — AND EVERY LEAVER'S
// WHOSE TOMBSTONE ITS BIND STAMPS.
//
// The scope is where a node that cannot decode a record files the deferral,
// and a read about a person looks in THAT PERSON'S bucket. A directory record's
// subject names nobody, so an unbinding that stated the bucket of the seat's
// handle — a hash no read consults — would leave a node deferring it serving
// the holder as bound to the seat, never saying it was behind about them; and
// a record naming nobody would put "Sarah was unbound" on nobody's history. A
// bind of a seat somebody was removed from also stamps that leaver's tombstone,
// which is filed under the leaver, so their bucket is declared too.
//
// Mutation: drop the leavers from SetIdentity's scope, and the rig's apply
// refuses the bind for writing outside what it declared.
func TestAnIdentityChangeIsFiledUnderItsPersonAndEveryLeaver(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const seat = "platform-lead"
	holder := personAwayFrom(seat)
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: holder, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-enrol",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
	rig.seatOnly(seat)
	if err := rig.bind(seat, holder, "op-bind"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := rig.draining(func() error {
		unbound := ""
		_, err := rig.writer.SetIdentity(rig.t.Context(), iamdomain.IdentityEdit{
			PersonID: holder, Seat: &unbound, OpID: "op-unbind",
			Reason: "moved teams"})
		return err
	}); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	scope, person := rig.lastRecord()
	if !slices.Contains(scope.Paths, iamdomain.BucketOf(holder).Path()) {
		t.Errorf("the unbind declares %v, which does not cover the holder's "+
			"bucket %s — a node that cannot decode it files the deferral where "+
			"no read about them looks", scope.Paths, iamdomain.BucketOf(holder).Path())
	}
	if slices.Contains(scope.Paths, iamdomain.BucketOf(seat).Path()) {
		t.Errorf("the unbind declares the seat handle's bucket %s",
			iamdomain.BucketOf(seat).Path())
	}
	if person != holder {
		t.Errorf("the unbind names person %q, want the holder %s — its trail "+
			"row lands on nobody's history", person, holder)
	}

	// A LEAVER'S TOMBSTONE, stamped by the next bind of their seat.
	if err := rig.bind(seat, holder, "op-rebind"); err != nil {
		t.Fatalf("bind again: %v", err)
	}
	if err := rig.draining(func() error {
		_, err := rig.writer.Remove(rig.t.Context(), holder, "op-remove", "left")
		return err
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	successor := personAwayFrom(holder)
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: successor, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Dana Sre", Email: "dana@example.com", Login: "dana.sre",
		OpID: "op-successor",
	}); err != nil {
		t.Fatalf("enrol the successor: %v", err)
	}
	if err := rig.bind(seat, successor, "op-successor-bind"); err != nil {
		t.Fatalf("bind the successor: %v", err)
	}
	scope, _ = rig.lastRecord()
	for _, who := range []string{successor, holder} {
		if !slices.Contains(scope.Paths, iamdomain.BucketOf(who).Path()) {
			t.Errorf("the successor's bind declares %v, without %s's bucket %s",
				scope.Paths, who, iamdomain.BucketOf(who).Path())
		}
	}
}

// AND SO IS A SESSION CLOSE, under the session's person.
//
// Validation asks whether a deferral covers the PERSON's bucket; a close filed
// under the lineage's bucket was one a node could not decode and never knew it
// was behind about — so it went on serving the session somebody had signed out
// of.
func TestASessionCloseIsFiledUnderItsPersonsBucket(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	lineage := uuid.New().String()
	person := personAwayFrom(lineage)
	if err := rig.draining(func() error {
		_, err := rig.writer.OpenSession(rig.t.Context(), iamdomain.SessionStart{
			Lineage: lineage, Person: person,
			AbsoluteExpiresAt: brokerAt.Add(time.Hour), OpID: "op-open",
		})
		return err
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	rig.drain()

	// A CLOSE NAMING SOMEBODY ELSE is refused against this node's row.
	if _, err := rig.writer.CloseSession(rig.t.Context(), lineage,
		uuid.New().String(), "signed out", "op-wrong"); !errors.Is(err,
		iamdomain.ErrRefused) {
		t.Fatalf("a close naming another person answered %v, want %v", err,
			iamdomain.ErrRefused)
	}

	if err := rig.draining(func() error {
		_, err := rig.writer.CloseSession(rig.t.Context(), lineage, person,
			"signed out", "op-close")
		return err
	}); err != nil {
		t.Fatalf("close: %v", err)
	}
	scope, named := rig.lastRecord()
	if !slices.Contains(scope.Paths, iamdomain.BucketOf(person).Path()) {
		t.Errorf("the close declares %v, which does not cover the person's "+
			"bucket %s", scope.Paths, iamdomain.BucketOf(person).Path())
	}
	if slices.Contains(scope.Paths, iamdomain.BucketOf(lineage).Path()) {
		t.Errorf("the close still declares the lineage's bucket %s",
			iamdomain.BucketOf(lineage).Path())
	}
	if named != person {
		t.Errorf("the close names person %q, want %s", named, person)
	}
}

// scopeOf is the declared scope of the newest record on the rig's log carrying
// op.
func (r *writeRig) scopeOf(op iamdomain.OpKind) statelog.ScopeSet {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	for seq := last; seq > 0; seq-- {
		_, payload, _, ok, err := r.log.At(r.t.Context(), seq)
		if err != nil || !ok {
			continue
		}
		body, verdict := r.verifier.Open(payload)
		if verdict != statelog.Verified {
			continue
		}
		env, err := iamdomain.DecodeEnvelope(body)
		if err != nil || env.Op != op {
			continue
		}
		return env.Scope.Resolve(env.Subject)
	}
	r.t.Fatalf("no %s record on the log", op)
	return statelog.ScopeSet{}
}

// A REDEMPTION AND A REMOVAL ARE FILED UNDER EVERY BUCKET THEIR APPLY WRITES.
//
// An invitation — and the trail row its record writes — is filed under its
// ADDRESS's bucket, since it has no person until it is redeemed. Redeeming it
// marks that row spent, and removing the person who redeemed it erases it; so
// each record declares the person's bucket AND the address's. Declaring the
// person's alone, a node holding back the invitation's own record — one signed
// under a keyring key it was not restarted with, or a newer build's — applied
// the redemption or the removal ahead of it, found no row to mark or to erase,
// and wrote the invitation back unspent, or with the removed person's address
// sealed in it, when it reprocessed the invitation. (The rig's apply asks
// every record it applies whether what it wrote is inside what it declared;
// this is the case that names the two records it caught.)
//
// Mutation: drop the address's bucket from Enrol's scope for a redemption, and
// the redemption's scope fails here.
func TestARedemptionAndARemovalAreFiledUnderTheAddressTheyWrite(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const address = "sarah@example.com"
	issued, err := inviteFor(t, rig, address, "")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	blind := blindOf(t, address)
	// A PERSON FILED APART FROM THEIR ADDRESS, so a scope of either bucket
	// cannot pass for the other by coincidence.
	person := personAwayFrom(blind)
	if err := rig.draining(func() error {
		_, err := nodeWriter(rig).Enrol(t.Context(), iamdomain.Enrolment{
			PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: "Sarah", Email: address, Login: iam.LoginFromAddress(address),
			Grants:     []iam.Grant{iam.GrantStateRead},
			Invitation: issued.ID, InvitationSecret: issued.Secret,
			OpID: "invite:" + issued.ID, Reason: "redeemed an invitation",
		})
		return err
	}); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	both := []string{iamdomain.BucketOf(person).Path(), iamdomain.BucketOf(blind).Path()}
	covers := func(scope statelog.ScopeSet) bool {
		for _, path := range both {
			if !slices.Contains(scope.Paths, path) {
				return false
			}
		}
		return true
	}
	if scope := rig.scopeOf(iamdomain.OpEnrol); !covers(scope) {
		t.Errorf("the redemption declares %v, want the person's and the "+
			"address's buckets, %v", scope.Paths, both)
	}
	if err := rig.during(func() error {
		_, err := rig.writer.Remove(t.Context(), person, "op-remove", "left")
		return err
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if scope := rig.scopeOf(iamdomain.OpRemove); !covers(scope) {
		t.Errorf("the removal declares %v, want the person's and the address's "+
			"buckets, %v", scope.Paths, both)
	}
}
