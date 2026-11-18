package assistantdomain

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ASSISTANT LOG'S HALF OF A REANCHOR: one record.
//
// [statelog.Reanchor] owns the steps and their order; this domain says, in its
// own record format, that its log was adopted at a new generation, and the
// applier writes `assistant_log_generations` from it. No version reset: every
// row's version guard and history position compares against positions in the
// new generation, above every old one by construction.

// GenerationRecord is the assistant log's [statelog.GenerationEncoder].
type GenerationRecord struct{}

// GenerationSubject is the subject generation gen's record is published on.
func (GenerationRecord) GenerationSubject(gen uint32) (statelog.Subject, bool) {
	return GenerationSubject(gen).framework(), true
}

// GenerationRecord encodes the reanchor's record for the new generation,
// create-only on the generation's own subject, under the operation id the
// framework derives ([statelog.GenerationFacts.OpID]) and authored by the
// operator who ran it.
func (GenerationRecord) GenerationRecord(f statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	subject := GenerationSubject(f.Generation)
	actor := generationActor(f)
	body, err := json.Marshal(Generation{
		V: GateRecordVersion, Generation: f.Generation, By: actor.Name,
		PrevHighest: f.Inputs.Highest, StreamAt: f.Inputs.StreamCreatedAt.UTC(),
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, fmt.Errorf("assistantdomain: "+
			"encode generation %d: %w", f.Generation, err)
	}
	opID := f.OpID()
	encoded, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			OpID: opID, Subject: subject, Op: OpGeneration,
			CreatedAt: f.At.UTC(), Gen: f.Generation, Writer: f.Writer,
			Scope: ScopeSet{Subject: true},
		},
		Mutation: body,
		Actor:    actor.Name, ActorKind: actor.Kind, OperatorID: actor.OperatorID,
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, err
	}
	return statelog.GenerationRecord{
		Subject: subject.framework(), OpID: opID, Payload: encoded,
	}, true, nil
}

// generationActor is a generation record's author:
// [statelog.GenerationFacts.Author]'s party, as every domain's names it.
// NOBODY NAMED IS THE NODE, recorded under its own id as the engine.
func generationActor(f statelog.GenerationFacts) iam.Actor {
	party, named := f.Author()
	if !named {
		return iam.Actor{Name: f.Writer, Kind: iam.ActorSystem}
	}
	return party
}
