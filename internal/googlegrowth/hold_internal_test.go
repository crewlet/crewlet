package googlegrowth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/growth"
)

// failingLeases answers every hold with err.
type failingLeases struct{ err error }

func (l failingLeases) Hold(context.Context, string) (func(), error) { return nil, l.err }

// ONLY A LEASE ANOTHER ACT HOLDS IS ANSWERED AS "CALL AGAIN": that is a wait
// another call ends, unavailable for now. Any other failure of the lease (a
// store that refused the name, one that did not answer) is answered as itself,
// naming what failed, never as a wait a retry ends.
//
// Mutation: answer every Hold error with the unavailable refusal, as hold did,
// and the store's refusal reads as another act; drop failEarly's mark, and the
// act reads as one whose outcome is unknown.
func TestOnlyALeaseAnotherActHoldsIsAnsweredAsAWait(t *testing.T) {
	t.Parallel()
	held := fmt.Errorf("%w: another act on write/search_console/s is being applied", growth.ErrActHeld)
	_, err := (&Client{Name: "Search Console", Leases: failingLeases{err: held}}).hold(t.Context(), "r")
	if refusal, ok := errors.AsType[*growth.Refusal](err); !ok || refusal.Kind != growth.RefusedUnavailable ||
		!strings.Contains(refusal.Said, "Call again") {
		t.Errorf("a lease another act holds = %v, want the unavailable refusal to call again", err)
	}
	failed := errors.New("coord: a lease's resource of 295 bytes as stored, past the 256")
	c := &Client{Name: "Search Console", Kind: "search_console", Leases: failingLeases{err: failed}}
	_, err = c.Apply(t.Context(), growth.Call{Tool: "search_console_submit"},
		Write{Req: Request{Op: growth.Operation{ID: "submit"}}, Class: growth.ClassOperate}, "")
	if _, refused := errors.AsType[*growth.Refusal](err); refused || !errors.Is(err, failed) ||
		!errors.Is(err, growth.ErrNotSent) || strings.Contains(err.Error(), "Call again") {
		t.Errorf("an act whose lease the store failed = %v, want the store's failure as itself, not sent", err)
	}
}
