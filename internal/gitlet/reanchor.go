package gitlet

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// GITLET'S HALF OF A REANCHOR, and it is one record.
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

// GenerationRecord is Gitlet's [statelog.GenerationEncoder].
type GenerationRecord struct{}

// GenerationSubject is the subject generation gen's record is published on.
func (GenerationRecord) GenerationSubject(gen uint32) (statelog.Subject, bool) {
	return GenerationSubject(gen).statelog(), true
}

// GenerationRecord encodes the reanchor's record for the NEW generation,
// create-only on the generation's own subject so two operators deriving the
// same number race at the broker and exactly one record lands. The operation
// id names the writer ([statelog.GenerationFacts.OpID]), and the author is
// whoever ran the reanchor, recorded as every other log's generation record
// records them ([generationActor]).
func (GenerationRecord) GenerationRecord(f statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	subject := GenerationSubject(f.Generation)
	opID := f.OpID()
	actor := generationActor(f)
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

// generationActor is who a generation record names as its author, in this
// domain's actor kinds.
//
// [statelog.GenerationFacts.Author]'s party, which every domain's generation
// record names alike, in this package's words: the name bare, the kind in a
// column of its own, and the credential the rule names beside them. It took
// the name alone and recorded `"operator:" + name` with the NAME in the
// credential column, so the one record that says who moved this log named a
// party nothing else in the audit feed calls by that name, read a person's
// reanchor as an API token named after them, and lost the credential that
// actually ran it — the one a revocation would be about.
//
// A PERSON IS AN OPERATOR HERE, because this domain has no word for a person
// ([ActorOperator] is a person or a credential acting on the company's
// behalf, as the code log's eviction gate names one), kept beside exactly the
// credential the rule names: the token or the session they acted through. A
// person's handle never stands in for one — the rule lends the name to the
// credential column only for a credential acting under its own login — so
// facts that name a person and no credential, which [iam.ActorFor] never
// produces, carry none here either. A seat is an agent, the engine the system
// and an operator an operator, each as the rule reads it. And NOBODY NAMED IS
// THE NODE, recorded as the node's own work is: its id, as the system.
func generationActor(f statelog.GenerationFacts) Actor {
	party, named := f.Author()
	if !named {
		return Actor{ID: f.Writer, Kind: ActorSystem}
	}
	kind := ActorKind(party.Kind)
	if party.Kind == iam.ActorHuman {
		kind = ActorOperator
	}
	return Actor{ID: party.Name, Kind: kind, OperatorID: party.OperatorID}
}
