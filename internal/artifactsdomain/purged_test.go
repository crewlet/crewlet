package artifactsdomain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A WRITE THE DELETION GATE DROPPED IS A PURGE, as the decide says when it is
// the one that finds the marker: the artifact's purge landed ahead of the
// record, after the decide passed, and the framework states that as one of its
// conditions — which the artifacts API answered `503` "try again" and a tool
// "try again shortly", about an artifact nothing will ever write again. As
// [artifactsdomain.ErrPurged] it is the API's `404` and the tools' not-found,
// and an asset's subject names the artifact the asset was part of.
//
// Mutation: drop the deletion arm from publishError and every case fails.
func TestAWriteTheDeletionGateDroppedIsAPurge(t *testing.T) {
	t.Parallel()
	id := artifacts.IDFor("op")
	gated := &statelog.Unavailable{Reason: statelog.ReasonDeleted,
		Detail: "the record at 3:4 was durable and applied nowhere"}
	for name, subject := range map[string]artifactsdomain.Subject{
		"an artifact": artifactsdomain.ArtifactSubject(id),
		"an asset":    artifactsdomain.AssetSubject(id, "logo"),
	} {
		err := artifactsdomain.PublishError(statelog.Subject{Kind: string(subject.Kind),
			ID: subject.ID}, gated)
		if !errors.Is(err, artifactsdomain.ErrPurged) {
			t.Errorf("%s's gated deletion is told %v, want ErrPurged", name, err)
			continue
		}
		if !strings.HasSuffix(err.Error(), id.String()) || strings.Contains(err.Error(), "statelog") {
			t.Errorf("%s's purge reads %q, want it to name the artifact and nothing "+
				"of the framework", name, err)
		}
	}
}
