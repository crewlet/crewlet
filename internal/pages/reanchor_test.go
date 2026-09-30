package pages_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A GENERATION RECORD NAMES WHO MOVED THIS LOG THE WAY EVERY OTHER ROW NAMES
// THEM — the name bare, its kind in a column of its own, and the credential
// they acted through beside it.
//
// It took the name alone and recorded it as `"operator:" + name`, with no
// credential: the one record that says who re-anchored the knowledge base named
// a party nothing else in the audit feed calls by that name — the tracker's
// generation record, read beside it, names the same operator bare — and never
// said through what.
func TestAGenerationRecordNamesItsAuthorAsEveryRowDoes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		facts    statelog.GenerationFacts
		actor    string
		kind     pages.AuthorKind
		operator string
	}{
		"a person through a machine token": {
			facts: statelog.GenerationFacts{By: "jane.doe", ByKind: "human",
				OperatorID: "pat:0b8f6b44"},
			actor: "jane.doe", kind: pages.AuthorHuman, operator: "pat:0b8f6b44",
		},
		"a Tier A token": {
			facts: statelog.GenerationFacts{By: "token:ops", ByKind: "operator",
				OperatorID: "token:ops"},
			actor: "token:ops", kind: pages.AuthorOperator, operator: "token:ops",
		},
		// THE PRINCIPAL'S VOCABULARY IS READ TOO, as the tracker's record
		// reads it, so one reanchor names one party the same way on both
		// logs.
		"a person named by their principal kind": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: "person",
				OperatorID: "session:0196f0c2"},
			actor: "eng", kind: pages.AuthorHuman, operator: "session:0196f0c2",
		},
		"a seat named by its principal kind": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: "seat",
				OperatorID: "eng"},
			actor: "eng", kind: pages.AuthorAgent, operator: "eng",
		},
		// AN OPERATOR WITH NO CREDENTIAL STATED is a credential acting under
		// its own login, so the login is the credential — and only there.
		"a machine with no credential stated": {
			facts: statelog.GenerationFacts{By: "ci:release", ByKind: "machine"},
			actor: "ci:release", kind: pages.AuthorOperator, operator: "ci:release",
		},
		"a kind nobody stated": {
			facts: statelog.GenerationFacts{By: "token:ops"},
			actor: "token:ops", kind: pages.AuthorOperator, operator: "token:ops",
		},
		"a person with no credential stated": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: "human"},
			actor: "eng", kind: pages.AuthorHuman,
		},
		// A KIND THIS DOMAIN HAS NO WORD FOR is a party acting on the
		// company's behalf, which is an operator — and the engine acts
		// through no credential, so none is invented for it.
		"the engine itself": {
			facts: statelog.GenerationFacts{By: "engine", ByKind: "system"},
			actor: "engine", kind: pages.AuthorOperator,
		},
		"the engine named by its principal kind": {
			facts: statelog.GenerationFacts{By: "engine", ByKind: "engine"},
			actor: "engine", kind: pages.AuthorOperator,
		},
		// NOBODY NAMED IS THE NODE, never an empty author column.
		"nobody named": {
			facts: statelog.GenerationFacts{},
			actor: "node-a", kind: pages.AuthorOperator,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			facts := tc.facts
			facts.Generation, facts.Writer, facts.At = 2, "node-a", wednesday
			record, keeps, err := pages.GenerationRecord{}.GenerationRecord(facts)
			if err != nil || !keeps {
				t.Fatalf("GenerationRecord = (keeps %v, %v), want a record", keeps, err)
			}
			decoded, err := pages.Decode(record.Payload)
			if err != nil {
				t.Fatalf("decode the record: %v", err)
			}
			if decoded.Actor != tc.actor || decoded.ActorKind != tc.kind ||
				decoded.OperatorID != tc.operator {
				t.Errorf("the record is authored (%q, %s, %q), want (%q, %s, %q)",
					decoded.Actor, decoded.ActorKind, decoded.OperatorID,
					tc.actor, tc.kind, tc.operator)
			}
			mutation, err := pages.DecodeMutation(decoded)
			if err != nil {
				t.Fatalf("decode the generation: %v", err)
			}
			generation, ok := mutation.(pages.Generation)
			if !ok {
				t.Fatalf("the record carries a %T", mutation)
			}
			if generation.By != tc.actor {
				t.Errorf("the generation names %q as who opened it, want %q",
					generation.By, tc.actor)
			}
		})
	}
}
