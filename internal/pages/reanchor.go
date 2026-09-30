package pages

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/iam"
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

// generationActor is who a generation record names as its author.
//
// THE FACTS' THREE COLUMNS, as every other write here records them: the name
// BARE, the kind in a column of its own and the credential beside them. It
// took the name alone and rendered it `"operator:" + name` with no credential,
// so the one record that says who moved this log named a party no other row
// in the audit feed calls by that name, and never said through what.
//
// THE KIND IS READ IN EITHER VOCABULARY, as the tracker's generation record
// and the identity estate's read it: the one [iam.ActorFor] records a party
// under, which is this package's own, or the principal's (a seat, a person, a
// machine). Read in this package's alone, a reanchor the engine named by its
// principal kind recorded a person as an operator on this log and as a person
// on the tracker's, which is one party as two in the audit feed the two
// histories are read side by side in.
//
// A KIND THIS DOMAIN HAS NO WORD FOR IS AN OPERATOR — the engine's own, or
// none stated — because it is a party acting on the company's behalf and
// neither a seat nor a person. An OPERATOR the facts name with no credential
// beside them is a credential acting under its own login — the one party whose
// name [iam.ActorFor] also records as its credential — so the name stands in
// for it there, and nowhere else: under any other kind it would put a seat's
// handle or a person's in the credential column, and the engine acts through
// no credential at all. And NOBODY NAMED IS THE NODE, recorded under its own
// id rather than as an empty author column.
func generationActor(f statelog.GenerationFacts) Actor {
	if strings.TrimSpace(f.By) == "" {
		return Actor{Handle: f.Writer, Kind: AuthorOperator}
	}
	operator := f.OperatorID
	var kind AuthorKind
	switch {
	case iam.Kind(f.ByKind) == iam.KindSeat:
		kind = AuthorAgent
	case iam.Kind(f.ByKind) == iam.KindPerson:
		kind = AuthorHuman
	case iam.Kind(f.ByKind) == iam.KindEngine,
		iam.ActorKind(f.ByKind) == iam.ActorSystem:
		return Actor{Handle: f.By, Kind: AuthorOperator, OperatorID: operator}
	case AuthorKind(f.ByKind).Valid():
		kind = AuthorKind(f.ByKind)
	default:
		// A MACHINE, or a kind nobody stated.
		kind = AuthorOperator
	}
	if kind == AuthorOperator && operator == "" {
		operator = f.By
	}
	return Actor{Handle: f.By, Kind: kind, OperatorID: operator}
}
