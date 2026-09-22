package tracker_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// THE BOUND SEAT RIDES BESIDE THE AUTHOR, AND IT IS NOT ONE.
//
// `actor_seat` exists for exactly one reader — the wake's actor exclusion,
// which cannot see the person behind a credential without it. Everything that
// renders a writer reads `actor` and `actor_kind`, and this case asserts both
// halves in one place: the field survives the wire, and the two that ARE the
// audit trail are untouched by its arrival.
func TestTheActorSeatRidesBesideTheAuthor(t *testing.T) {
	t.Parallel()
	record := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "op-1",
			Subject: tracker.TaskSubject("t-1"), Op: tracker.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
			Scope: tracker.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: json.RawMessage(`{}`), Kind: tracker.ChangeFields,
		Actor: "founder", ActorKind: tracker.AuthorOperator,
		OperatorID: "founder", ActorSeat: "jane-founder",
	}
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := tracker.Decode(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.ActorSeat != "jane-founder" {
		t.Errorf("the record came back with seat %q — without it the wake "+
			"exclusion compares a candidate against a credential", back.ActorSeat)
	}
	if back.Actor != "founder" || back.ActorKind != tracker.AuthorOperator ||
		back.OperatorID != "founder" {

		t.Errorf("the author is %q of kind %q — a seat beside the author must "+
			"not become one", back.Actor, back.ActorKind)
	}
	// AND IT IS A KNOWN KEY, so the decoder does not ALSO file it in
	// Extra — a key carried in both is a key encoded twice, which is the
	// hazard [knownKeys] exists to prevent and the one a new field walks
	// straight into.
	if _, wrong := back.Extra["actor_seat"]; wrong {
		t.Errorf("actor_seat is carried in Extra as well as in its own field, "+
			"so it is re-encoded twice: %+v", back.Extra)
	}

	// AND THE PARTY IS BOTH NAMES, THE PERSON FIRST. The alias is matched
	// against and never rendered, which is [tracker.Party]'s own rule.
	if got := back.ActorParty().Handles(); !slices.Equal(
		got, []string{"jane-founder", "founder"}) {

		t.Errorf("the writer's party is %v, want the seat first and the "+
			"credential behind it", got)
	}
}

// AND A WRITER THAT IS ALREADY A SEAT IS A PARTY OF ONE.
//
// Every agent, every human at the dashboard and every token nobody bound: the
// field is empty, the party is the author's own handle, and the exclusion
// compares exactly what it compared before this field existed.
func TestAWriterWithNoBoundSeatIsAPartyOfOne(t *testing.T) {
	t.Parallel()
	for name, record := range map[string]tracker.MutationRecord{
		"a seat": {Actor: "eng", ActorKind: tracker.AuthorAgent},
		"an unbound token": {
			Actor: "ci", ActorKind: tracker.AuthorOperator, OperatorID: "ci",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := record.ActorParty().Handles()
			if len(got) != 1 || got[0] != record.Actor {
				t.Errorf("%s answers to %v, want its own name alone", name, got)
			}
		})
	}
}
