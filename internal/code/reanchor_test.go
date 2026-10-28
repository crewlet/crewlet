package code_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A REANCHOR'S AUTHOR READS ALIKE ON EVERY LOG: the name bare, its kind in a
// column of its own, and the credential they acted through beside it.
//
// The code log recorded `"operator:" + name` with the NAME in the credential
// column, ignoring the kind and the credential the facts carry — so the audit
// feed, which reads every log's generation records side by side, showed one
// person as two (`jane.doe` on the knowledge base's and the tracker's,
// `operator:jane.doe` here), read their reanchor as a token named after them,
// and lost the credential that ran it. Held against the knowledge base's and
// the tracker's own records for the same facts: the name, and the credential
// wherever the facts state one, are theirs; the kind is this domain's word for
// theirs (a person is an operator here, as the code log's eviction gate names
// one).
//
// Mutation: record `"operator:" + By` again, or drop the facts' credential —
// each case below red.
func TestAGenerationRecordNamesItsAuthorAsEveryLogDoes(t *testing.T) {
	t.Parallel()
	at := time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		facts    statelog.GenerationFacts
		actor    string
		kind     code.ActorKind
		operator string
	}{
		"a person through a machine token": {
			facts: statelog.GenerationFacts{By: "jane.doe", ByKind: "human",
				OperatorID: "pat:0b8f6b44"},
			actor: "jane.doe", kind: code.ActorOperator, operator: "pat:0b8f6b44",
		},
		"a person named by their principal kind": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: "person",
				OperatorID: "session:0196f0c2"},
			actor: "eng", kind: code.ActorOperator, operator: "session:0196f0c2",
		},
		"a Tier A token": {
			facts: statelog.GenerationFacts{By: "token:ops", ByKind: "operator",
				OperatorID: "token:ops"},
			actor: "token:ops", kind: code.ActorOperator, operator: "token:ops",
		},
		// A SEAT IS AN AGENT, keeping the credential the facts name.
		"a seat named by its principal kind": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: "seat", OperatorID: "eng"},
			actor: "eng", kind: code.ActorAgent, operator: "eng",
		},
		"a seat named by its actor kind": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: "agent"},
			actor: "eng", kind: code.ActorAgent,
		},
		// AN OPERATOR WITH NO CREDENTIAL STATED is a credential acting under
		// its own login, so the login is the credential — which this domain
		// requires of every operator's record, a person's included.
		"a machine with no credential stated": {
			facts: statelog.GenerationFacts{By: "ci:release", ByKind: "machine"},
			actor: "ci:release", kind: code.ActorOperator, operator: "ci:release",
		},
		"a kind nobody stated": {
			facts: statelog.GenerationFacts{By: "token:ops"},
			actor: "token:ops", kind: code.ActorOperator, operator: "token:ops",
		},
		"a person with no credential stated": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: "human"},
			actor: "eng", kind: code.ActorOperator, operator: "eng",
		},
		// THE ENGINE IS THE SYSTEM, and acts through no credential.
		"the engine itself": {
			facts: statelog.GenerationFacts{By: "engine", ByKind: "system"},
			actor: "engine", kind: code.ActorSystem,
		},
		"the engine named by its principal kind": {
			facts: statelog.GenerationFacts{By: "engine", ByKind: "engine"},
			actor: "engine", kind: code.ActorSystem,
		},
		// NOBODY NAMED IS THE NODE, as this domain records the node's own
		// work — never an empty author column.
		"nobody named": {
			facts: statelog.GenerationFacts{},
			actor: "node-a", kind: code.ActorSystem,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			facts := tc.facts
			facts.Generation, facts.Writer, facts.At = 2, "node-a", at
			record, keeps, err := code.GenerationRecord{}.GenerationRecord(facts)
			if err != nil || !keeps {
				t.Fatalf("GenerationRecord = (keeps %v, %v), want a record", keeps, err)
			}
			decoded, err := code.Decode(record.Payload)
			if err != nil {
				t.Fatalf("decode the record: %v", err)
			}
			if decoded.Actor != tc.actor || decoded.ActorKind != tc.kind || decoded.OperatorID != tc.operator {
				t.Errorf("the record is authored (%q, %s, %q), want (%q, %s, %q)",
					decoded.Actor, decoded.ActorKind, decoded.OperatorID, tc.actor, tc.kind, tc.operator)
			}
			mutation, err := code.DecodeMutation(decoded)
			if err != nil {
				t.Fatalf("decode the generation: %v", err)
			}
			generation, ok := mutation.(code.Generation)
			if !ok {
				t.Fatalf("the record carries a %T", mutation)
			}
			if generation.By != tc.actor {
				t.Errorf("the generation names %q as who opened it, want %q", generation.By, tc.actor)
			}

			// AS THE OTHER LOGS NAME THE SAME PARTY.
			pagesRecord, _, err := pages.GenerationRecord{}.GenerationRecord(facts)
			if err != nil {
				t.Fatalf("the knowledge base's record: %v", err)
			}
			pagesDecoded, err := pages.Decode(pagesRecord.Payload)
			if err != nil {
				t.Fatalf("decode the knowledge base's record: %v", err)
			}
			trackerRecord, _, err := tracker.GenerationRecord{}.GenerationRecord(facts)
			if err != nil {
				t.Fatalf("the tracker's record: %v", err)
			}
			trackerDecoded, err := tracker.Decode(trackerRecord.Payload)
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
				if facts.OperatorID != "" && decoded.OperatorID != other.operator {
					t.Errorf("the code log names the credential %q, %s %q", decoded.OperatorID, log,
						other.operator)
				}
			}
		})
	}
}
