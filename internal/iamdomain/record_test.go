package iamdomain_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD A NEWER BUILD WROTE DECODES ITS ENVELOPE, FAILS ITS PAYLOAD, AND IS
// RETAINED.
//
// The deferral contract: without an envelope there is no position, no kind, no
// subject and no scope, so a record a build cannot decode could not be
// indexed, probed for or reported on — only dropped.
func TestAFutureRecordYieldsAnEnvelopeAndRefusesItsPayload(t *testing.T) {
	t.Parallel()
	payload := []byte(`{
		"v": 99,
		"op_id": "018f-0001",
		"subject": {"k": "person", "i": "p1"},
		"op": "update",
		"scope": [17],
		"writer": "node-a",
		"something_a_newer_build_added": {"deep": true}
	}`)

	env, err := iamdomain.DecodeEnvelope(payload)
	if err != nil {
		t.Fatalf("the envelope pass failed on a newer build's record: %v — it "+
			"must never fail on version, or the record could only be dropped",
			err)
	}
	if env.Subject.Kind != iamdomain.KindPerson || env.Subject.ID != "p1" {
		t.Errorf("the envelope's subject is %+v", env.Subject)
	}
	if env.Scope.Root || !slices.Equal(env.Scope.Buckets,
		[]iamdomain.Bucket{17}) {
		t.Errorf("the envelope's scope is %+v, and a scope a writer stated in "+
			"plain buckets must be readable at every version", env.Scope)
	}

	rec, err := iamdomain.Decode(payload)
	var future *iamdomain.ErrFutureVersion
	if !errors.As(err, &future) {
		t.Fatalf("the payload pass answered %v, want a future-version refusal", err)
	}
	if rec.Subject != env.Subject {
		t.Errorf("the refusal came back without the subject the deferral arm "+
			"has to file the record under: %+v", rec.Subject)
	}
	if future.Got != 99 || future.Want != iamdomain.RecordVersion {
		t.Errorf("the refusal reports %d against %d", future.Got, future.Want)
	}
}

// AND A RECORD ROUND-TRIPS LOSSLESSLY THROUGH A BUILD THAT ONLY HALF READS IT.
//
// The reanchor path reads a record and republishes it, so a node that stripped
// the half it did not understand would make every upgrade a silent data loss
// on exactly the records a newer peer had just written.
func TestARecordRoundTripsThroughThisBuildLosslessly(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"v":1,"op_id":"018f-0001",` +
		`"subject":{"k":"person","i":"p1"},"op":"update","scope":[17],` +
		`"person":"p1","actor":"jane.doe","actor_kind":"person",` +
		`"a_field_from_a_newer_build":{"deep":[1,2]}}`)

	rec, err := iamdomain.Decode(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.Person != "p1" || rec.Actor != "jane.doe" ||
		rec.ActorKind != iam.KindPerson {
		t.Fatalf("the known half decoded as %+v", rec)
	}
	out, err := iamdomain.Encode(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back map[string]json.RawMessage
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("the re-encoded record is not an object: %v", err)
	}
	if got := string(back["a_field_from_a_newer_build"]); got != `{"deep":[1,2]}` {
		t.Errorf("the carried field came back as %q — a reanchor republishes "+
			"what it read, so stripping it is data loss with no record of "+
			"itself", got)
	}
}

// A RECORD WITH NO VERSION IS REFUSED, because it cannot be told apart from a
// newer build's.
func TestARecordWithNoVersionIsRefused(t *testing.T) {
	t.Parallel()
	for name, payload := range map[string]string{
		"no version at all":      `{"subject":{"k":"person","i":"p1"},"op":"update"}`,
		"a zero version":         `{"v":0,"subject":{"k":"person","i":"p1"},"op":"update"}`,
		"a negative one":         `{"v":-1,"subject":{"k":"person","i":"p1"},"op":"update"}`,
		"a subject with no kind": `{"v":1,"subject":{"i":"p1"},"op":"update"}`,
		"not an object":          `["v",1]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := iamdomain.DecodeEnvelope([]byte(payload)); err == nil {
				t.Errorf("%s was accepted", payload)
			}
		})
	}
}

// THE GATE IS ANSWERED FROM THE OP AND NOT FROM THE KIND.
//
// A removal rides the ORDINARY PERSON SUBJECT, so a reader that keyed on the
// kind would defer it — and a deferred removal here is a person the company
// off-boarded still signing in on one node, with no later record that ever
// corrects it because nothing names a removed person again. An invalidation is
// the same failure one blast radius wider: deferred, a node goes on honouring
// every bearer the company just ended.
func TestOnlyARemovalAnInvalidationAndAnEvictionInstallAGate(t *testing.T) {
	t.Parallel()
	gates := []iamdomain.OpKind{
		iamdomain.OpRemove, iamdomain.OpInvalidate, iamdomain.OpEviction,
	}
	for _, op := range iamdomain.OpKinds {
		env := iamdomain.RecordEnvelope{Op: op}
		want := slices.Contains(gates, op)
		if got := env.InstallsGate(); got != want {
			t.Errorf("%s reports InstallsGate() = %v, want %v", op, got, want)
		}
		// And the domain's own answer is the SAME answer, read rather
		// than restated — a gate rule written twice is a record that
		// stops the applier on the write side and is filed for later on
		// the read side.
		if got := (iamdomain.Domain{}).InstallsGate(
			statelogEnvelope(op)); got != want {
			t.Errorf("the domain reports InstallsGate(%s) = %v, want %v",
				op, got, want)
		}
	}
	// A REMOVAL ON THE PERSON SUBJECT IS THE CASE THAT MATTERS: its kind
	// is the ordinary one, so nothing about the subject says gate.
	rec := iamdomain.RecordEnvelope{
		Subject: iamdomain.PersonSubject("p1"), Op: iamdomain.OpRemove}
	if !rec.InstallsGate() {
		t.Error("a removal on the ordinary person subject does not install a " +
			"gate — it would be deferred, and a deferred removal is somebody " +
			"the company off-boarded still signing in on one node")
	}
}

// THE ENVELOPE CARRIES NOTHING PERSONAL, and that is a property of the format
// rather than of any one writer.
//
// Every envelope field is readable by every build for ever, which means it is
// also rendered by every operator surface that reports a record nobody could
// decode: a deferral row, a stalled-log report, a broker listing. A name, an
// address or a login in one of those is a durable, replicated statement about
// a person that this domain spends the rest of its design avoiding.
func TestTheEnvelopeHasNoFieldThatCouldHoldPersonalData(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(iamdomain.RecordEnvelope{
		V: 1, Subject: iamdomain.PersonSubject("p1"),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(data, &named); err != nil {
		t.Fatalf("the envelope does not marshal to an object: %v", err)
	}
	// The eight reserved keys, and nothing else may join them. The list is
	// spelled out rather than derived, because the point is that adding a
	// field to this struct is a decision somebody has to write down here.
	want := []string{"v", "subject", "op", "scope"}
	for _, key := range want {
		if _, ok := named[key]; !ok {
			t.Errorf("the envelope no longer carries %q, which every build "+
				"reads for ever", key)
		}
	}
	for key := range named {
		if slices.Contains(want, key) {
			continue
		}
		t.Errorf("the envelope carries %q, which no zero value used to "+
			"marshal — every field here is rendered by an operator surface "+
			"for a record nobody could decode, so a new one is a decision "+
			"that belongs in this case's list", key)
	}
	// And the two fields that DO name a person live on the mutation half,
	// which the envelope pass never opens.
	mutation, err := json.Marshal(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{V: 1}, Person: "p1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(mutation), `"person":"p1"`) {
		t.Error("the person id is not on the mutation record, so the applier " +
			"cannot attach a claim's rows to anybody")
	}
}

// EVERY OP IS CLASSIFIED OR DELIBERATELY SILENT, in both directions.
//
// An unclassified op writes no authentication trail at all, which is
// indistinguishable from one that was never meant to — so the check is
// two-sided and names the op rather than counting, for internal/skipgate's
// reason: a count goes green when one omission is fixed and another appears.
func TestEveryOpIsClassifiedOrDeliberatelySilent(t *testing.T) {
	t.Parallel()
	if err := iamdomain.CheckHistoryClasses(); err != nil {
		t.Fatal(err)
	}
	// And the classification is reachable for the ops that have one.
	for _, op := range []iamdomain.OpKind{
		iamdomain.OpEnrol, iamdomain.OpRevoke, iamdomain.OpRemove,
		iamdomain.OpBootstrap,
	} {
		class, ok := iamdomain.ClassOf(op)
		if !ok || class != iamdomain.ClassChange {
			t.Errorf("%s classifies as (%q, %v), want the change horizon",
				op, class, ok)
		}
	}
	for _, op := range []iamdomain.OpKind{iamdomain.OpOpen, iamdomain.OpClose} {
		class, ok := iamdomain.ClassOf(op)
		if !ok || class != iamdomain.ClassSession {
			t.Errorf("%s classifies as (%q, %v), want the session horizon",
				op, class, ok)
		}
	}
	for _, op := range []iamdomain.OpKind{
		iamdomain.OpSweep, iamdomain.OpBarrier, iamdomain.OpEviction,
		iamdomain.OpGeneration,
	} {
		if _, ok := iamdomain.ClassOf(op); ok {
			t.Errorf("%s writes a history row — a sweep's whole job is "+
				"deleting them, and a log-level fact is not somebody's "+
				"identity changing", op)
		}
	}
}

// A CLASS OFF THE WIRE THAT THIS BUILD DOES NOT KNOW IS A VALUE.
func TestAnUnknownHistoryClassIsAValue(t *testing.T) {
	t.Parallel()
	if iamdomain.HistoryClass("forensics").Valid() {
		t.Error("a class this build has never heard of reported itself valid")
	}
	for _, c := range iamdomain.HistoryClasses {
		if !c.Valid() {
			t.Errorf("%s is declared and reports itself invalid", c)
		}
	}
}

// statelogEnvelope is the framework's view of a record carrying one op, which
// is all [iamdomain.Domain.InstallsGate] is given.
func statelogEnvelope(op iamdomain.OpKind) statelog.Envelope {
	return statelog.Envelope{Op: string(op)}
}
