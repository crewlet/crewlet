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

// A RELEASE IS FILED UNDER ITS HOLDER'S BUCKET.
//
// The scope is where a node that cannot decode a record files the deferral,
// and a read about a person looks in THAT PERSON'S bucket. A release used to
// state the bucket of the TOKEN — a hash of a seat handle, a login or a blind
// that no read consults — so a node deferring an unbinding went on serving the
// holder as bound to the seat and never said it was behind about them. And the
// record named nobody, so the trail row for "Sarah was unbound" was on nobody's
// history.
func TestAReleaseIsFiledUnderItsHoldersBucket(t *testing.T) {
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
	if err := rig.claim(iamdomain.KindSeat, seat, holder, "op-bind"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	// A RELEASE NAMING SOMEBODY ELSE is refused naming who holds it, so a
	// record can never be filed under a bucket the claim is not in.
	someoneElse := uuid.New().String()
	_, err := rig.writer.Release(rig.t.Context(), iamdomain.KindSeat, seat,
		someoneElse, "op-wrong", "moved teams")
	var claimed *iamdomain.ErrClaimed
	if !errors.As(err, &claimed) || claimed.Holder != holder {
		t.Fatalf("a release naming the wrong holder answered %v, want a "+
			"refusal naming %s", err, holder)
	}

	if err := rig.draining(func() error {
		_, err := rig.writer.Release(rig.t.Context(), iamdomain.KindSeat, seat,
			holder, "op-unbind", "moved teams")
		return err
	}); err != nil {
		t.Fatalf("release: %v", err)
	}
	scope, person := rig.lastRecord()
	if !slices.Contains(scope.Paths, iamdomain.BucketOf(holder).Path()) {
		t.Errorf("the release declares %v, which does not cover the holder's "+
			"bucket %s — a node that cannot decode it files the deferral where "+
			"no read about them looks", scope.Paths, iamdomain.BucketOf(holder).Path())
	}
	if slices.Contains(scope.Paths, iamdomain.BucketOf(seat).Path()) {
		t.Errorf("the release still declares the seat handle's bucket %s",
			iamdomain.BucketOf(seat).Path())
	}
	if person != holder {
		t.Errorf("the release names person %q, want the holder %s — its trail "+
			"row lands on nobody's history", person, holder)
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
