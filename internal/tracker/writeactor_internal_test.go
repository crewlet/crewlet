package tracker

import (
	"testing"
	"time"
)

// THE WRITER IS WHAT PUTS THE BOUND SEAT ON THE RECORD.
//
// [Provenance.Seat] reaches a writer through [Writer.As] and decided only
// WHOSE STATE a person write landed on; nothing carried it onto the record,
// so the wake's actor exclusion — which runs on another node, minutes later,
// from the payload alone — had no way to know the person behind a credential.
//
// IN-PACKAGE AND THROUGH [Writer.decide], because that is the one builder
// every write path shares and it needs no broker: a case that went through the
// round-trip harness would assert the same field through a store that has no
// column for it, and could only fail for reasons that are not about this.
func TestTheWriterStampsTheBoundSeatOntoItsRecord(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	base := &Writer{Actor: "system", ActorKind: AuthorSystem}

	for name, c := range map[string]struct {
		provenance Provenance
		want       string
	}{
		"a bound operator": {
			Provenance{OperatorID: "founder", Seat: "jane-founder"},
			"jane-founder",
		},
		// AN UNBOUND TOKEN AND A SEAT BOTH WRITE NOTHING HERE, which
		// is what keeps the wire identical for every company that
		// never bound a credential.
		"an unbound token": {Provenance{OperatorID: "ci"}, ""},
		"a seat":           {Provenance{TurnID: "run-1"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := base.As("founder", AuthorOperator, c.provenance)
			decision, err := w.decide(TaskSubject("t-1"), OpPatch, ChangeFields,
				ScopeSet{Subject: true, Container: "ENG"}, "op-1",
				struct{}{}, nil, at)
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			record, err := Decode(decision.Payload)
			if err != nil {
				t.Fatalf("decode the published record: %v", err)
			}
			if record.ActorSeat != c.want {
				t.Errorf("%s published a record naming seat %q, want %q — "+
					"the wake exclusion reads the payload and nothing else",
					name, record.ActorSeat, c.want)
			}
			// AND THE AUTHOR IS THE CREDENTIAL, on every one of them.
			if record.Actor != "founder" || record.ActorKind != AuthorOperator {
				t.Errorf("%s is authored by %q of kind %q — the seat travels "+
					"BESIDE the author and never becomes one",
					name, record.Actor, record.ActorKind)
			}
		})
	}
}
