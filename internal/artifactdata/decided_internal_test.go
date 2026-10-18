package artifactdata

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// A WRITE THAT DELIVERED NO VERDICT IS THE DATASTORE OBJECTIVES' FAILURE: one
// pending on this node, and one whose outcome nobody could establish, are bad
// events however fast they returned. Mutation: answer true for a nil error
// alone, or for any error, and a case fails.
func TestOnlyAVerdictAnswersAWrite(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		res  WriteResult
		err  error
		want bool
	}{
		"the applier's verdict":          {WriteResult{Verdict: artifacts.DataApplied}, nil, true},
		"the applier's refusal":          {WriteResult{Verdict: artifacts.DataConflict}, nil, true},
		"pending on this node":           {WriteResult{}, nil, false},
		"input refused before the log":   {WriteResult{}, fmt.Errorf("%w: a field", ErrInvalid), true},
		"the caller's own standing":      {WriteResult{}, fmt.Errorf("%w: not yours", ErrForbidden), true},
		"a purged store":                 {WriteResult{}, ErrGone, true},
		"this node's write budget":       {WriteResult{}, &RateLimited{Scope: "principal", Cost: 1}, true},
		"a store nobody could reach":     {WriteResult{}, errors.New("the broker did not answer"), false},
		"a request that ran out of time": {WriteResult{}, context.DeadlineExceeded, false},
	} {
		if got := decided(c.res, c.err); got != c.want {
			t.Errorf("%s: decided %v, want %v", name, got, c.want)
		}
	}
}
