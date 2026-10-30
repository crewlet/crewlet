package chatlet

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// CHAT'S HALF OF A REANCHOR, and it is one record.
//
// [statelog.Reanchor] owns the steps and their order; this domain's part is to
// say, in its own record format, that its log was adopted at a new generation.
// The applier already knows what that record is — [Applier] writes
// `chat_log_generations` from it — and the record is what the audit row
// derives from, so nothing here writes a row of its own.
//
// It had no such part until now, and the engine's reanchor refuses, by name, a
// log whose domain cannot say it was adopted — so a recreated CREWLET_CHAT_LOG
// was a log no operator could ever move the fleet onto.
//
// # And, like the tracker's and the knowledge base's, no version reset
//
// A room's `version` is compared by the applier's guards and by a caller's
// revision check, and both against positions in the NEW generation, which are
// above every old version by construction. A message's `channel_seq` is the
// one number here that is NOT a position — it is minted from the room's own
// high-water mark — and a reanchor leaves it alone for the same reason: the
// room's mark is a row the new generation's records go on counting from, so
// the transcript stays contiguous across the move and a browser's gap check
// keeps meaning what it meant.

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
// THE OPERATION ID NAMES THE WRITER ([statelog.GenerationFacts.OpID]), for
// the tracker's reason: two nodes deriving the same number are two operations,
// and one id between them let the broker's duplicate window acknowledge the
// second as though it had landed. The transition reads back whose record
// landed ([statelog.Reanchor]).
//
// THE PRINCIPAL WHO RAN IT IS THE AUTHOR, recorded the way every write here
// is — see [generationActor]. A reanchor is said about a log, not in a room,
// so it needs no seat to speak as.
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
