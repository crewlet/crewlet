package artifactsdomain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A WRITE THE DELETION GATE DROPPED IS A PURGE, as the decide says when it
// finds the marker: the purge landed ahead of the record, after the decide
// passed. It is [artifactsdomain.ErrPurged] (the API's `404`, the tools'
// not-found), never a framework condition read as "try again" about an
// artifact nothing will write again, and an asset's subject names its
// artifact.
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
