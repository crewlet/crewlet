package artifactdataapi

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A DATASTORE'S REFUSAL IS TOLD IN ITS SENTENCE.
//
// This surface cut "artifactdata: " from the front of a refusal and left the
// rest, so a field refused read "invalid: collection …", a write the caller's
// standing refused "forbidden: …", and a schema change that lost its race
// carried the framework's error and the broker subject after the datastore's
// sentence. A lost race is told in the state log's words for a person, as on
// every surface; the rest in the datastore's own sentence.
//
// Mutation: cut the front prefix only again, or drop the statelog arm, and a
// row goes red.
func TestADatastoresRefusalIsToldInItsSentence(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err  error
		says string
	}{
		"a field": {fmt.Errorf("%w: collection %q is not in the schema",
			artifactdata.ErrInvalid, "rows"), `collection "rows" is not in the schema`},
		"a standing": {fmt.Errorf("%w: only an editor undoes another's write",
			artifactdata.ErrForbidden), "only an editor undoes another's write"},
		"a query": {fmt.Errorf("%w: %w", artifactdata.ErrInvalid,
			fmt.Errorf("%w: 9 filters is more than 8", query.ErrInvalid)),
			"9 filters is more than 8"},
		"a lost race": {fmt.Errorf("%w: d-1: %w", artifactdata.ErrConflict,
			fmt.Errorf("%w: crewlet.artifacts.data.d-1 kept changing under this write",
				statelog.ErrConflict)), statelog.LostRace},
	} {
		if got := said(c.err); got != c.says {
			t.Errorf("%s is told %q, want %q", name, got, c.says)
		}
		for _, leak := range []string{"artifactdata", "query:", "statelog", "invalid:",
			"forbidden:", "crewlet."} {
			if strings.Contains(said(c.err), leak) {
				t.Errorf("%s is told with %q in it: %s", name, leak, said(c.err))
			}
		}
	}
}
