package worklet

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TRACKER'S HALF OF A REANCHOR, and it is one record. A reanchor recovers
// a recreated stream (the tables are the truth, the stream a replay window) as
// a GENERATION TRANSITION, not a re-stamp, so an old position stays comparable
// and safely stale. [statelog.Reanchor] owns the steps and their order; this
// domain records that the transition happened, applied by [Applier] as the
// `tracker_log_generations` audit row, so a replay of the log reproduces it.
//
// There is no version reset, and there must not be one: `if_match` and the
// applier's guard compare a version against a position in the NEW generation,
// above every old version by construction. A reset would collapse
// `tracker_body_revisions`' key column and make every task's vector stale
// (current while `source_rev` equals the task's version), re-billing embeddings.

// GenerationRecord is the tracker's [statelog.GenerationEncoder].
type GenerationRecord struct{}

// generationReason is the audit row's account of why the log was adopted, BY
// CASE: `tracker_log_generations.reason` is the one place a later reader learns
// what happened to the log, and the cases have different consequences. A
// restored log also names what the operator accepted discarding, so the row
// says it was decided rather than lost.
func generationReason(f statelog.GenerationFacts) string {
	switch f.Case {
	case statelog.ReanchorRestored:
		reason := "the broker was restored from an older copy — the log ended " +
			"below the checkpoint or had been written past it — and it is " +
			"followed from its end"
		if f.Discarded != nil {
			reason += fmt.Sprintf("; the records written to it after the restore "+
				"that these rows did not hold were discarded on the operator's "+
				"word, the newest being %s", *f.Discarded)
		}
		return reason
	case statelog.ReanchorAbandoned:
		return "the log continued in a generation only an evicted node held; it " +
			"is followed from these rows' checkpoint and that generation's " +
			"records are void"
	}
	return "the stream was recreated and its sequences restarted"
}

// generationActor is the author, the author kind and the credential a
// generation record carries, from [statelog.GenerationFacts.Author]'s party.
// It takes the facts' own kind and credential, never the author's name for
// both: the credential that ran the verb is what a revocation is about.
// Nobody named is the node, recorded as the node's own writer records itself.
func generationActor(f statelog.GenerationFacts) (author string, kind AuthorKind, operator string) {
	party, named := f.Author()
	if !named {
		return f.Writer, AuthorSystem, ""
	}
	return party.Name, AuthorKind(party.Kind), party.OperatorID
}

// GenerationSubject is the subject generation gen's record is published on —
// what a reader of the log asks to learn who opened a generation
// ([statelog.GenerationOpeners]).
func (GenerationRecord) GenerationSubject(gen uint32) (statelog.Subject, bool) {
	return wire(GenerationSubject(gen)), true
}

// GenerationRecord encodes the reanchor's record for the NEW generation.
//
// It is create-only on the generation's own subject: two operators deriving the
// same generation number race at the broker and exactly one record lands.
//
// The operation id names the writer ([statelog.GenerationFacts.OpID]): two
// nodes deriving the same number are two operations, or the broker's duplicate
// window would acknowledge the second as landed. A re-run on one node is the
// same operation. [statelog.Reanchor] reads back whose record landed.
//
// The operator is the author ([generationActor]), not the node's SYSTEM actor:
// who ran the verb is what a reader of `tracker_log_generations` asks.
func (GenerationRecord) GenerationRecord(f statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	subject := GenerationSubject(f.Generation)
	scope := ScopeSet{Subject: true}
	// The state log's grammar: the id carries the instant a retry is judged
	// by and names the writing node.
	opID := f.OpID()
	author, kind, operator := generationActor(f)
	body, err := json.Marshal(Generation{
		V:                   GateRecordVersion,
		Gen:                 f.Generation,
		PrevStreamCreatedAt: f.Inputs.KeyedTo.UTC(),
		NewStreamCreatedAt:  f.Inputs.StreamCreatedAt.UTC(),
		PrevLastSeqSeen:     f.Inputs.Highest,
		ReanchoredBy:        author,
		Reason:              generationReason(f),
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, fmt.Errorf("worklet: encode "+
			"generation %d: %w", f.Generation, err)
	}
	// Gen and Writer are the record's own: it opens the generation it names,
	// and the eviction gate reads its writer.
	encoded, err := MutationRecord{
		// NEVER [RecordVersion]: see [baseRecordVersion].
		RecordEnvelope: RecordEnvelope{
			V: baseRecordVersion, OpID: opID, Subject: subject, Op: OpGeneration,
			CreatedAt: f.At.UTC(), Gen: f.Generation, Writer: f.Writer,
			Scope: scope,
		},
		Mutation:   body,
		Actor:      author,
		ActorKind:  kind,
		OperatorID: operator,
	}.Encode()
	if err != nil {
		return statelog.GenerationRecord{}, false, err
	}
	return statelog.GenerationRecord{
		Subject: wire(subject),
		OpID:    opID,
		Payload: encoded,
	}, true, nil
}
