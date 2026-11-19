package chatlet

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// CHAT'S HALF OF A REANCHOR, and it is one record.
//
// [statelog.Reanchor] owns the steps and their order; this domain says, in its
// own record, that its log was adopted at a new generation. The engine's
// reanchor refuses a log whose domain cannot. The record is what
// `chat_log_generations` (via [Applier]) derives from, so nothing here writes a
// row of its own.
//
// No version reset: a room's `version` is compared against positions in the NEW
// generation, above every old version by construction. A message's
// `channel_seq` is NOT a position (it is minted from the room's high-water
// mark row), so it is left alone too and the transcript stays contiguous.

// GenerationRecord is chat's [statelog.GenerationEncoder].
type GenerationRecord struct{}

// GenerationSubject is the subject generation gen's record is published on —
// what a reader of the log asks to learn who opened a generation
// ([statelog.GenerationOpeners]).
func (GenerationRecord) GenerationSubject(gen uint32) (statelog.Subject, bool) {
	return wire(GenerationSubject(gen)), true
}

// GenerationRecord encodes the reanchor's record for the NEW generation,
// create-only on the generation's own subject so two operators deriving the
// same number race at the broker and exactly one record lands.
//
// THE OPERATION ID NAMES THE WRITER ([statelog.GenerationFacts.OpID]): two nodes
// deriving the same number are two operations, and one shared id let the
// broker's duplicate window acknowledge the second as landed. The transition
// reads back whose record landed ([statelog.Reanchor]). The principal who ran it
// is the author ([generationActor]); a reanchor is about a log, not a room, so
// it needs no seat.
func (GenerationRecord) GenerationRecord(f statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	subject := GenerationSubject(f.Generation)
	scope := ScopeSet{Subject: true}
	if err := scope.Validate(); err != nil {
		return statelog.GenerationRecord{}, false, err
	}
	// THE STATE LOG'S OWN GRAMMAR ([statelog.GenerationFacts.OpID]), so
	// the record's id carries the instant a retry is judged by, names the
	// node that writes it, and is the same operation on every re-run there.
	opID := f.OpID()
	actor := generationActor(f)
	payload := Generation{
		V:               GateRecordVersion,
		Generation:      f.Generation,
		By:              actor.Name(),
		PrevHighest:     f.Inputs.Highest,
		StreamCreatedAt: f.Inputs.StreamCreatedAt.UTC(),
	}
	if err := payload.Validate(); err != nil {
		return statelog.GenerationRecord{}, false, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.GenerationRecord{}, false, fmt.Errorf("chatlet: encode "+
			"generation %d: %w", f.Generation, err)
	}
	// THE GENERATION AND THE WRITER ARE STAMPED, and both are this
	// record's own: it opens the generation it names, and the eviction
	// gate reads the node that wrote it.
	encoded, err := Encode(MutationRecord{
		// NO VERSION: the encoder stamps the base one, and refuses a
		// generation carrying a versioned field
		// ([RecordEnvelope.readByEveryBuild]).
		RecordEnvelope: RecordEnvelope{
			OpID: opID, Subject: subject, Op: OpGeneration,
			CreatedAt: f.At.UTC(), Gen: f.Generation, Writer: f.Writer,
			Scope: scope,
		},
		Mutation:   body,
		Actor:      actor.Name(),
		ActorKind:  actor.Kind,
		OperatorID: actor.OperatorID,
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, err
	}
	return statelog.GenerationRecord{
		Subject: wire(subject),
		OpID:    opID,
		Payload: encoded,
	}, true, nil
}

// generationActor is who a generation record names as its author.
//
// [statelog.GenerationFacts.Author]'s party, which every domain's generation
// record names alike, in this package's words — which are the actor kinds
// themselves ([AuthorKind] is its four words), the engine as the system
// included.
//
// NOBODY NAMED IS THE NODE, recorded under its own id and as the system, as
// this domain's own narration is, rather than as an empty author column.
func generationActor(f statelog.GenerationFacts) Actor {
	party, named := f.Author()
	if !named {
		return Actor{Handle: f.Writer, Kind: AuthorSystem}
	}
	return Actor{Handle: party.Name, Kind: AuthorKind(party.Kind),
		OperatorID: party.OperatorID}
}
