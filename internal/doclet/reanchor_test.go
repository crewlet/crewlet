package doclet_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
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
		kind     doclet.AuthorKind
		operator string
	}{
		"a person through a machine token": {
			facts: statelog.GenerationFacts{By: "jane.doe", ByKind: iam.ActorHuman,
				OperatorID: "pat:0b8f6b44"},
			actor: "jane.doe", kind: doclet.AuthorHuman, operator: "pat:0b8f6b44",
		},
		"a Tier A token": {
			facts: statelog.GenerationFacts{By: "token:ops", ByKind: iam.ActorOperator,
				OperatorID: "token:ops"},
			actor: "token:ops", kind: doclet.AuthorOperator, operator: "token:ops",
		},
		// AN OPERATOR WITH NO CREDENTIAL STATED is a credential acting under
		// its own login, so the login is the credential — and only there.
		"a machine with no credential stated": {
			facts: statelog.GenerationFacts{By: "ci:release", ByKind: iam.ActorOperator},
			actor: "ci:release", kind: doclet.AuthorOperator, operator: "ci:release",
		},
		"a kind nobody stated": {
			facts: statelog.GenerationFacts{By: "token:ops"},
			actor: "token:ops", kind: doclet.AuthorOperator, operator: "token:ops",
		},
		"a person with no credential stated": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: iam.ActorHuman},
			actor: "eng", kind: doclet.AuthorHuman,
		},
		// A KIND THIS DOMAIN HAS NO WORD FOR is a party acting on the
		// company's behalf, which is an operator — and the engine acts
		// through no credential, so none is invented for it.
		"the engine itself": {
			facts: statelog.GenerationFacts{By: "engine", ByKind: iam.ActorSystem},
			actor: "engine", kind: doclet.AuthorOperator,
		},
		// NOBODY NAMED IS THE NODE, never an empty author column.
		"nobody named": {
			facts: statelog.GenerationFacts{},
			actor: "node-a", kind: doclet.AuthorOperator,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			facts := tc.facts
			facts.Generation, facts.Writer, facts.At = 2, "node-a", wednesday
			record, keeps, err := doclet.GenerationRecord{}.GenerationRecord(facts)
			if err != nil || !keeps {
				t.Fatalf("GenerationRecord = (keeps %v, %v), want a record", keeps, err)
			}
			decoded, err := doclet.Decode(record.Payload)
			if err != nil {
				t.Fatalf("decode the record: %v", err)
			}
			if decoded.Actor != tc.actor || decoded.ActorKind != tc.kind ||
				decoded.OperatorID != tc.operator {
				t.Errorf("the record is authored (%q, %s, %q), want (%q, %s, %q)",
					decoded.Actor, decoded.ActorKind, decoded.OperatorID,
					tc.actor, tc.kind, tc.operator)
			}
			mutation, err := doclet.DecodeMutation(decoded)
			if err != nil {
				t.Fatalf("decode the generation: %v", err)
			}
			generation, ok := mutation.(doclet.Generation)
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
