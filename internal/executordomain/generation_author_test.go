package executordomain_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REANCHOR'S RECORD NAMES ITS AUTHOR BY THE ONE RULE EVERY LOG READS IT BY.
//
// [statelog.GenerationFacts.Author] states the rule once, and the audit feed
// reads every log's generation records side by side. The record names exactly
// the party that rule names, in the actor kinds this log's columns hold, and
// this node when the facts name nobody; its Generation payload's By, which the
// audit row is written from, is the same name.
//
// Mutation: put a private reading back that maps a principal's word to an actor
// kind of its own, or reads a name of spaces as a party, and its case says so.
func TestAGenerationRecordNamesItsAuthorByTheOneRule(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]statelog.GenerationFacts{
		"nobody named":     {},
		"a name of spaces": {By: "  ", ByKind: "human"},
		"a person, through a token": {By: "eng", ByKind: "human",
			OperatorID: "pat:founder"},
		"a credential acting as itself":   {By: "ci:release", ByKind: "operator"},
		"a principal's word":              {By: "eng", ByKind: "person"},
		"a kind this build does not know": {By: "jane.doe", ByKind: "daemon"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f.Generation, f.Writer = 2, "node-a"
			f.At = time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
			f.Inputs = statelog.ReanchorInputs{Stream: executordomain.Domain{}.Stream().Name,
				StreamCreatedAt: time.Date(2031, 3, 30, 9, 0, 0, 0, time.UTC)}
			want, named := f.Author()
			if !named {
				want = iam.Actor{Name: "node-a", Kind: iam.ActorSystem}
			}
			rec, kept, err := executordomain.GenerationRecord{}.GenerationRecord(f)
			if err != nil || !kept {
				t.Fatalf("encode: %v (kept %v)", err, kept)
			}
			got, err := executordomain.Decode(rec.Payload)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Actor != want.Name || got.ActorKind != want.Kind ||
				got.OperatorID != want.OperatorID {
				t.Errorf("the record names (%q, %q, %q), want (%q, %q, %q)",
					got.Actor, got.ActorKind, got.OperatorID,
					want.Name, want.Kind, want.OperatorID)
			}
			body, err := executordomain.DecodeMutation(got)
			if err != nil {
				t.Fatalf("decode the generation: %v", err)
			}
			g, ok := body.(executordomain.Generation)
			if !ok {
				t.Fatalf("the record's payload is a %T, want a Generation", body)
			}
			if g.By != want.Name {
				t.Errorf("the generation is by %q, want %q", g.By, want.Name)
			}
		})
	}
}
