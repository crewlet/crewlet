package gitlet_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A REANCHOR'S AUTHOR READS ALIKE ON EVERY LOG: the name bare, its kind in a
// column of its own, and the credential they acted through beside it — the
// party [statelog.GenerationFacts.Author] names, the one rule every log's
// generation record reads the facts by.
//
// The code log recorded `"operator:" + name` with the NAME in the credential
// column, ignoring the kind and the credential the facts carry — so the audit
// feed, which reads every log's generation records side by side, showed one
// person as two (`jane.doe` on the knowledge base's and the tracker's,
// `operator:jane.doe` here), read their reanchor as a token named after them,
// and lost the credential that ran it. It then kept a reading of the facts of
// its own, an eighth copy of the rule, which still took a principal's word (a
// seat, a person, a machine, the engine) as a kind. Held against the rule and
// against the knowledge base's and the tracker's own records for the same
// facts: the name and the credential are theirs; the kind is this domain's
// word for the rule's (a person is an operator here, as the code log's
// eviction gate names one).
//
// Mutations: record `"operator:" + By` again, or drop the facts' credential;
// read a principal's word as a kind again, and its case is an agent or the
// system rather than an operator; lend a person's handle to the credential
// column, and the person with no credential stated names one — each red.
func TestAGenerationRecordNamesItsAuthorAsEveryLogDoes(t *testing.T) {
	t.Parallel()
	at := time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		facts    statelog.GenerationFacts
		actor    string
		kind     gitlet.ActorKind
		operator string
	}{
		// A PERSON IS AN OPERATOR, beside the credential they acted through.
		"a person through a machine token": {
			facts: statelog.GenerationFacts{By: "jane.doe", ByKind: iam.ActorHuman,
				OperatorID: "pat:0b8f6b44"},
			actor: "jane.doe", kind: gitlet.ActorOperator, operator: "pat:0b8f6b44",
		},
		// A person's handle is no credential: the rule lends the name to the
		// credential column only for a credential acting as itself.
		"a person with no credential stated": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: iam.ActorHuman},
			actor: "eng", kind: gitlet.ActorOperator,
		},
		"a Tier A token": {
			facts: statelog.GenerationFacts{By: "token:ops", ByKind: iam.ActorOperator,
				OperatorID: "token:ops"},
			actor: "token:ops", kind: gitlet.ActorOperator, operator: "token:ops",
		},
		// AN OPERATOR WITH NO CREDENTIAL STATED is a credential acting under
		// its own login, so the login is the credential.
		"a credential acting as itself": {
			facts: statelog.GenerationFacts{By: "ci:release", ByKind: iam.ActorOperator},
			actor: "ci:release", kind: gitlet.ActorOperator, operator: "ci:release",
		},
		"a kind nobody stated": {
			facts: statelog.GenerationFacts{By: "token:ops"},
			actor: "token:ops", kind: gitlet.ActorOperator, operator: "token:ops",
		},
		// A SEAT IS AN AGENT, keeping the credential the facts name.
		"a seat acting as itself": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: iam.ActorAgent},
			actor: "eng", kind: gitlet.ActorAgent,
		},
		"a seat through its credential": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: iam.ActorAgent,
				OperatorID: "seat:eng"},
			actor: "eng", kind: gitlet.ActorAgent, operator: "seat:eng",
		},
		// THE ENGINE IS THE SYSTEM, and acts through no credential.
		"the engine itself": {
			facts: statelog.GenerationFacts{By: "engine", ByKind: iam.ActorSystem},
			actor: "engine", kind: gitlet.ActorSystem,
		},
		// A PRINCIPAL'S WORD IS NO ACTOR KIND, so it is an operator whatever
		// it spells — as the rule reads it, and as every other log records it.
		"a seat's principal word": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: iam.ActorKind(iam.KindSeat),
				OperatorID: "seat:eng"},
			actor: "eng", kind: gitlet.ActorOperator, operator: "seat:eng",
		},
		"the engine's principal word": {
			facts: statelog.GenerationFacts{By: "engine", ByKind: iam.ActorKind(iam.KindEngine)},
			actor: "engine", kind: gitlet.ActorOperator, operator: "engine",
		},
		// NOBODY NAMED IS THE NODE, as this domain records the node's own
		// work — never an empty author column — and so is a name of spaces.
		"nobody named": {
			facts: statelog.GenerationFacts{},
			actor: "node-a", kind: gitlet.ActorSystem,
		},
		"a name of spaces": {
			facts: statelog.GenerationFacts{By: "  ", ByKind: iam.ActorHuman},
			actor: "node-a", kind: gitlet.ActorSystem,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			facts := tc.facts
			facts.Generation, facts.Writer, facts.At = 2, "node-a", at
			record, keeps, err := gitlet.GenerationRecord{}.GenerationRecord(facts)
			if err != nil || !keeps {
				t.Fatalf("GenerationRecord = (keeps %v, %v), want a record", keeps, err)
			}
			decoded, err := gitlet.Decode(record.Payload)
			if err != nil {
				t.Fatalf("decode the record: %v", err)
			}
			if decoded.Actor != tc.actor || decoded.ActorKind != tc.kind || decoded.OperatorID != tc.operator {
				t.Errorf("the record is authored (%q, %s, %q), want (%q, %s, %q)",
					decoded.Actor, decoded.ActorKind, decoded.OperatorID, tc.actor, tc.kind, tc.operator)
			}
			// AS THE ONE RULE NAMES THEM: the name and the credential are the
			// rule's own, whatever this domain calls the kind.
			if party, named := facts.Author(); named &&
				(decoded.Actor != party.Name || decoded.OperatorID != party.OperatorID) {
				t.Errorf("the record names (%q, %q), the rule (%q, %q)",
					decoded.Actor, decoded.OperatorID, party.Name, party.OperatorID)
			}
			mutation, err := gitlet.DecodeMutation(decoded)
			if err != nil {
				t.Fatalf("decode the generation: %v", err)
			}
			generation, ok := mutation.(gitlet.Generation)
			if !ok {
				t.Fatalf("the record carries a %T", mutation)
			}
			if generation.By != tc.actor {
				t.Errorf("the generation names %q as who opened it, want %q", generation.By, tc.actor)
			}

			// AS THE OTHER LOGS NAME THE SAME PARTY.
			pagesRecord, _, err := doclet.GenerationRecord{}.GenerationRecord(facts)
			if err != nil {
				t.Fatalf("the knowledge base's record: %v", err)
			}
			pagesDecoded, err := doclet.Decode(pagesRecord.Payload)
			if err != nil {
				t.Fatalf("decode the knowledge base's record: %v", err)
			}
			trackerRecord, _, err := worklet.GenerationRecord{}.GenerationRecord(facts)
			if err != nil {
				t.Fatalf("the tracker's record: %v", err)
			}
			trackerDecoded, err := worklet.Decode(trackerRecord.Payload)
			if err != nil {
				t.Fatalf("decode the tracker's record: %v", err)
			}
			for log, other := range map[string]struct{ actor, operator string }{
				"the knowledge base": {pagesDecoded.Actor, pagesDecoded.OperatorID},
				"the tracker":        {trackerDecoded.Actor, trackerDecoded.OperatorID},
			} {
				if decoded.Actor != other.actor {
					t.Errorf("the code log names %q, %s names %q: one reanchor's author is two names",
						decoded.Actor, log, other.actor)
				}
				if decoded.OperatorID != other.operator {
					t.Errorf("the code log names the credential %q, %s %q", decoded.OperatorID, log,
						other.operator)
				}
			}
		})
	}
}
