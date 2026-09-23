package pages

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE KNOWLEDGE BASE'S HALF OF A REANCHOR, and it is one record.
//
// [statelog.Reanchor] owns the steps and their order; this domain's part is to
// say, in its own record format, that its log was adopted at a new generation.
// The applier already knows what that record is — [Applier] writes
// `pages_log_generations` from it — and the record is what the audit row
// derives from, so nothing here writes a row of its own.
//
// It had no such part until now. A reanchor of CREWLET_PAGES_LOG published the
// TRACKER's generation record on the tracker's log, wrote the tracker's audit
// row and moved every domain's checkpoint to the pages log's first sequence —
// so the pages log's own history never said it had been adopted, and the
// tracker's claimed a transition its log never went through.
//
// # And, like the tracker's, no version reset
//
// A page row's `version` is compared by the applier's guards and by a save's
// `base_version` check, and both against positions in the NEW generation, which
// are above every old version by construction; the arbitration anchor below
// the current generation already reads as "no anchor here". The tracker's
// package doc gives the rest of the reasoning, and what a reset would have
// cost — here it would additionally have collapsed `pages_history`, whose
// `version` is the order the activity feed and its cursors read.

// GenerationRecord is the knowledge base's [statelog.GenerationEncoder].
type GenerationRecord struct{}

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
// THE OPERATOR IS THE AUTHOR, recorded the way every operator write here is:
// the token's own name and author kind `operator`, never a seat.
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
	actor := Actor{Kind: AuthorOperator, OperatorID: f.By}
	body, err := json.Marshal(Generation{
		V:               GateRecordVersion,
		Generation:      f.Generation,
		By:              actor.Name(),
		PrevHighest:     f.Inputs.Highest,
		StreamCreatedAt: f.Inputs.StreamCreatedAt.UTC(),
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, fmt.Errorf("pages: encode "+
			"generation %d: %w", f.Generation, err)
	}
	// THE GENERATION AND THE WRITER ARE STAMPED, and both are this
	// record's own: it opens the generation it names, and the eviction
	// gate reads the node that wrote it.
	encoded, err := Encode(MutationRecord{
		// NEVER [RecordVersion]: see [baseRecordVersion].
		RecordEnvelope: RecordEnvelope{
			V: baseRecordVersion, OpID: opID, Subject: subject, Op: OpGeneration,
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
		Subject: statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
		OpID:    opID,
		Payload: encoded,
	}, true, nil
}
