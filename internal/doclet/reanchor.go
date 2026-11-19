package doclet

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE KNOWLEDGE BASE'S HALF OF A REANCHOR, and it is one record.
//
// [statelog.Reanchor] owns the steps and their order; this domain says, in its
// own record format, that its log was adopted at a new generation. [Applier]
// writes `pages_log_generations` from that record and the audit row derives from
// it, so nothing here writes a row. Without this part, a reanchor of
// CREWLET_PAGES_LOG published the TRACKER's generation record and audit row, so
// the pages log never said it was adopted.

// GenerationRecord is the knowledge base's [statelog.GenerationEncoder].
//
// Like the tracker's, it resets no version. A page row's `version` is compared
// by the applier's guards and a save's `base_version` check against positions in
// the NEW generation, above every old version by construction; a reset would
// have collapsed `pages_history`, whose `version` orders the activity feed and
// its cursors. The tracker's package doc has the rest.
type GenerationRecord struct{}

// GenerationSubject is the subject generation gen's record is published on —
// what a reader of the log asks to learn who opened a generation
// ([statelog.GenerationOpeners]).
func (GenerationRecord) GenerationSubject(gen uint32) (statelog.Subject, bool) {
	subject := GenerationSubject(gen)
	return statelog.Subject{Kind: string(subject.Kind), ID: subject.ID}, true
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
// THE OPERATOR IS THE AUTHOR, recorded the way every write here is — see
// [generationActor].
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
	body, err := json.Marshal(Generation{
		V:               GateRecordVersion,
		Generation:      f.Generation,
		By:              actor.Name(),
		PrevHighest:     f.Inputs.Highest,
		StreamCreatedAt: f.Inputs.StreamCreatedAt.UTC(),
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, fmt.Errorf("doclet: encode "+
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
		Subject: statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
		OpID:    opID,
		Payload: encoded,
	}, true, nil
}

// generationActor is who a generation record names as its author.
//
// [statelog.GenerationFacts.Author]'s party, which every domain's generation
// record names alike, in this package's words. Rendering the bare name as
// `"operator:" + name` with no credential named a party no other audit row
// calls that, and never said through what.
//
// THE ENGINE IS AN OPERATOR HERE: this domain has no word for the system, and
// the engine acts on the company's behalf as neither seat nor person; it is
// recorded beside the facts' credential, which for the engine is none. NOBODY
// NAMED IS THE NODE, recorded under its own id as that kind of operator rather
// than as an empty author column.
func generationActor(f statelog.GenerationFacts) Actor {
	party, named := f.Author()
	if !named {
		return Actor{Handle: f.Writer, Kind: AuthorOperator}
	}
	kind := AuthorKind(party.Kind)
	if party.Kind == iam.ActorSystem {
		kind = AuthorOperator
	}
	return Actor{Handle: party.Name, Kind: kind, OperatorID: party.OperatorID}
}
