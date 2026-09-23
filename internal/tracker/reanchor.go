package tracker

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TRACKER'S HALF OF A REANCHOR, and it is one record.
//
// A reanchor is the one recovery for a genuinely recreated stream: the durable
// tables are the record of truth and the stream is a replay window, so
// re-anchoring says "these rows are what they are; follow the new stream from
// its head". It is a GENERATION TRANSITION rather than a re-stamp, which is
// what makes an old position comparable and safely stale rather than
// indistinguishable from a current one.
//
// [statelog.Reanchor] owns the steps and their order. What lives here is what
// only this domain can do: say, in its own record format, that the transition
// happened. The audit row is what that record APPLIES as ([Applier] writes
// `tracker_log_generations` from it), so it is derived like every other row
// rather than written beside the checkpoint where no replay of the log could
// reproduce it.
//
// # There is no version reset, and there must not be one
//
// This file used to rewrite every object row's `version` into the new
// generation before the record went out, on the reasoning that a version from
// a dead number space forms an expectation the broker cannot arbitrate. That
// stopped being true when the expectation moved to the ARBITRATION ANCHOR: a
// version is what a caller's `if_match` and the applier's guard compare, and
// both compare it against a position in the NEW generation, which is above
// every old version by construction. The anchor below the current generation
// is already "no anchor here" to the publisher, which asks the broker and
// publishes at zero under the floor theorem. So the reset bought nothing — and
// it never ran: its table list named `version` columns that `tracker_comments`,
// `tracker_task_keys` and `tracker_tags` do not have, so every reanchor failed
// on its first statement, and it included `tracker_body_revisions`, whose
// `version` is the BODY's revision counter and a primary-key column, which it
// would have collapsed. Had it run, it would also have made every task's
// vector stale — a vector is current while its `source_rev` equals the task's
// version — and re-billed the company's whole embedding corpus.

// GenerationRecord is the tracker's [statelog.GenerationEncoder].
type GenerationRecord struct{}

// generationReason is the audit row's account of why the log was adopted,
// which differs by case.
//
// BY CASE, because `tracker_log_generations.reason` is the one place a later
// reader learns what happened to the log, and the two cases are different
// incidents with different consequences: a recreated log lost whatever this
// node never applied, and a restored one was followed from its end because the
// rows already held it. Every reanchor used to record the first, restored
// brokers included.
func generationReason(c statelog.ReanchorCase) string {
	if c == statelog.ReanchorRestored {
		return "the broker was restored from an older copy, so the log ended " +
			"below the checkpoint; it is followed from its end"
	}
	return "the stream was recreated and its sequences restarted"
}

// GenerationRecord encodes the reanchor's record for the NEW generation.
//
// CREATE-ONLY ON THE GENERATION'S OWN SUBJECT, which is first-writer-wins used
// for the one thing it is perfectly suited to: two operators deriving the same
// generation number race at the broker and exactly one record lands.
//
// THE OPERATOR IS THE AUTHOR. The record went out through the node's own
// writer once, which is the SYSTEM actor, so the audit row said the engine had
// reanchored itself; who ran the verb is the one fact a later reader of
// `tracker_log_generations` is asking.
func (GenerationRecord) GenerationRecord(f statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	subject := GenerationSubject(f.Generation)
	scope := ScopeSet{Subject: true}
	// THE STATE LOG'S OWN GRAMMAR ([statelog.GenerationFacts.OpID]), so
	// the record's id carries the instant a retry is judged by and two
	// operators deriving this generation from this stream name one
	// operation.
	opID := f.OpID()
	author, kind, operator := f.By, AuthorOperator, f.By
	if author == "" {
		author, kind, operator = f.Writer, AuthorSystem, ""
	}
	body, err := json.Marshal(Generation{
		V:                   GateRecordVersion,
		Gen:                 f.Generation,
		PrevStreamCreatedAt: f.Inputs.KeyedTo.UTC(),
		NewStreamCreatedAt:  f.Inputs.StreamCreatedAt.UTC(),
		PrevLastSeqSeen:     f.Inputs.Highest,
		ReanchoredBy:        author,
		Reason:              generationReason(f.Case),
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, fmt.Errorf("tracker: encode "+
			"generation %d: %w", f.Generation, err)
	}
	// THE GENERATION AND THE WRITER ARE STAMPED, and both are this
	// record's own: it opens the generation it names, and the eviction
	// gate reads the node that wrote it.
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
