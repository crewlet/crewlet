package artifactsdomain_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REANCHOR'S RECORD NAMES ITS AUTHOR BY THE ONE RULE EVERY LOG READS IT BY:
// exactly the party [statelog.GenerationFacts.Author] names, in the actor kinds
// this log's columns hold, and this node when the facts name nobody.
// Mutation: a private reading that maps a principal's word to an actor kind of
// its own, or reads a name of spaces as a party, fails its case.
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
			f.Inputs = statelog.ReanchorInputs{Stream: topics.ArtifactsLogStream,
				StreamCreatedAt: time.Date(2031, 3, 30, 9, 0, 0, 0, time.UTC)}
			want, named := f.Author()
			if !named {
				want = iam.Actor{Name: "node-a", Kind: iam.ActorSystem}
			}
			rec, kept, err := artifactsdomain.GenerationRecord{}.GenerationRecord(f)
			if err != nil || !kept {
				t.Fatalf("encode: %v (kept %v)", err, kept)
			}
			got, err := artifactsdomain.Decode(rec.Payload)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Actor != want.Name || got.ActorKind != want.Kind ||
				got.OperatorID != want.OperatorID {
				t.Errorf("the record names (%q, %q, %q), want (%q, %q, %q)",
					got.Actor, got.ActorKind, got.OperatorID,
					want.Name, want.Kind, want.OperatorID)
			}
		})
	}
}
