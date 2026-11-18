package builtin

import (
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE ARTIFACTS' WRITE FAILURES FALL BACK TO THE DOMAINS' ONE TABLE: a
// refusal a domain decided that no arm of publishFailure or dataFailure names
// (a write refused because the purge had begun, an undo of a write that
// cannot be undone) is that domain's class, never the node's fault; the
// unmarked rest is still this node's.
//
// Mutation: drop the WriterClass arm from publishFailure or dataFailure and
// its domain rows go red, as internal_error.
func TestArtifactWriteFailuresFallBackToTheDomainsTable(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err  error
		want tools.Refusal
	}{
		{fmt.Errorf("%w: 0190a3c4", artifactsdomain.ErrPurging), tools.RefusalConflict},
		{fmt.Errorf("%w: write 4 is a schema change", artifactdata.ErrNotUndoable), tools.RefusalConflict},
		{&statelog.Unavailable{Reason: statelog.ReasonLogFull}, tools.RefusalUnavailable},
		{errors.New("artifactsdomain: read the head: disk I/O"), tools.RefusalInternalError},
	} {
		for name, got := range map[string]tools.Result{
			"publishFailure": publishFailure(t.Context(), "publish_artifact", artifactCaller{}, c.err),
			"dataFailure":    dataFailure(t.Context(), "write_artifact_data", c.err),
		} {
			if !got.Failed || tools.RefusalOf(got) != c.want {
				t.Errorf("%s(%v) = failed %v class %q, want %q", name, c.err, got.Failed,
					tools.RefusalOf(got), c.want)
			}
		}
	}
}
