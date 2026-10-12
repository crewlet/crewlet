package code

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CODE HOST'S HALF OF A REANCHOR, and it is one record.
//
// [statelog.Reanchor] owns the steps and their order; this domain's part is to
// say, in its own record format, that its log was adopted at a new generation.
// The applier writes `code_log_generations` from that record, so nothing here
// writes a row of its own.
//
// # No version reset
//
// A repository row's `version` is compared only by the applier's guard, against
// positions in the NEW generation, which are above every old version by
// construction; the arbitration anchor below the current generation already
// reads as "no anchor here".

// GenerationRecord is the code host's [statelog.GenerationEncoder].
type GenerationRecord struct{}

// GenerationSubject is the subject generation gen's record is published on.
func (GenerationRecord) GenerationSubject(gen uint32) (statelog.Subject, bool) {
	return GenerationSubject(gen).statelog(), true
}

// GenerationRecord encodes the reanchor's record for the NEW generation,
// create-only on the generation's own subject so two operators deriving the
// same number race at the broker and exactly one record lands. The operation
// id names the writer ([statelog.GenerationFacts.OpID]), and the operator is
// the author, recorded as every operator write is: the credential's own id.
func (GenerationRecord) GenerationRecord(f statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	subject := GenerationSubject(f.Generation)
	opID := f.OpID()
	// THE KNOWLEDGE BASE'S SPELLING of an operator with no credential name,
	// so the audit row of a reanchor reads alike on every log.
	actor := Actor{ID: "operator:anonymous", Kind: ActorOperator, OperatorID: f.By}
	if f.By != "" {
		actor.ID = "operator:" + f.By
	}
	body, err := json.Marshal(Generation{
		V:               GateRecordVersion,
		Generation:      f.Generation,
		By:              actor.ID,
		PrevHighest:     f.Inputs.Highest,
		StreamCreatedAt: f.Inputs.StreamCreatedAt.UTC(),
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, fmt.Errorf("code: encode generation %d: %w",
			f.Generation, err)
	}
	// THE GENERATION AND THE WRITER ARE STAMPED, both this record's own: it
	// opens the generation it names, and the eviction gate reads its writer.
	encoded, err := Encode(MutationRecord{
		// NEVER [RecordVersion]: see [baseRecordVersion].
		RecordEnvelope: RecordEnvelope{
			V: baseRecordVersion, OpID: opID, Subject: subject, Op: OpGeneration,
			CreatedAt: f.At.UTC(), Gen: f.Generation, Writer: f.Writer,
			Scope: Scope{DomainPath()},
		},
		Mutation:   body,
		Actor:      actor.ID,
		ActorKind:  actor.Kind,
		OperatorID: actor.OperatorID,
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, err
	}
	return statelog.GenerationRecord{Subject: subject.statelog(), OpID: opID, Payload: encoded}, true, nil
}
