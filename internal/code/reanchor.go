package code

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/iam"
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
// THE FACTS' THREE COLUMNS, as the knowledge base's and the tracker's
// generation records read them, and as this log's own eviction gate names its
// operator: the name BARE, the kind in a column of its own, the credential
// beside them. It took the name alone and recorded `"operator:" + name` with
// the NAME in the credential column, so the one record that says who moved
// this log named a party nothing else in the audit feed calls by that name,
// read a person's reanchor as an API token named after them, and lost the
// credential that actually ran it — the one a revocation would be about.
//
// THE KIND IS READ IN EITHER VOCABULARY the facts may carry: the one
// [iam.ActorFor] records a party under, or the principal's (a seat, a person, a
// machine, the engine). This domain has no word for a person: a person is an
// OPERATOR here, named by the human seat they hold with their credential
// beside it — the eviction gate's spelling — so a person, a machine, an
// operator and a kind nobody stated are each an operator. An operator's
// record always names a credential ([Actor.validate]), and one the facts name
// with none is a credential acting under its own login — the one party whose
// name [iam.ActorFor] also records as its credential — so the name stands in
// for it there and nowhere else. A SEAT is an agent and the ENGINE the
// system, each keeping the credential the facts name, if any. And NOBODY NAMED
// IS THE NODE, recorded as the node's own work is: its id, as the system.
func generationActor(f statelog.GenerationFacts) Actor {
	if strings.TrimSpace(f.By) == "" {
		return Actor{ID: f.Writer, Kind: ActorSystem}
	}
	switch {
	case iam.Kind(f.ByKind) == iam.KindSeat, iam.ActorKind(f.ByKind) == iam.ActorAgent:
		return Actor{ID: f.By, Kind: ActorAgent, OperatorID: f.OperatorID}
	case iam.Kind(f.ByKind) == iam.KindEngine, iam.ActorKind(f.ByKind) == iam.ActorSystem:
		return Actor{ID: f.By, Kind: ActorSystem, OperatorID: f.OperatorID}
	}
	operator := f.OperatorID
	if strings.TrimSpace(operator) == "" {
		operator = f.By
	}
	return Actor{ID: f.By, Kind: ActorOperator, OperatorID: operator}
}
