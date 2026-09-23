package iamdomain

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

type countingEvents struct{ n int }

func (c *countingEvents) Emit(context.Context, events.Payload) { c.n++ }

// A WRITE WHOSE OUTCOME IS UNKNOWN IS NOT ANNOUNCED, AND NEITHER IS ONE THAT
// FAILED.
//
// `unknown` means the record may or may not be on the log; the `iam_history`
// row the applier writes is what settles it. A live row claiming a change that
// did not happen would be the one false line in the feed. `pending` IS
// announced, because a pending record is durable at its position and every node
// will apply it.
//
// Internal because no broker can be made to answer `unknown` on demand, and the
// rule is the writer's, not the broker's. Mutation: drop the outcome check and
// the unknown case announces.
func TestOnlyALandedWriteIsAnnounced(t *testing.T) {
	t.Parallel()
	row := types.IAMSessionGenerationBumped{Generation: 1}
	for _, tc := range []struct {
		name    string
		outcome statelog.Outcome
		err     error
		want    int
	}{
		{"applied", statelog.OutcomeApplied, nil, 1},
		{"pending", statelog.OutcomePending, nil, 1},
		{"unknown", statelog.OutcomeUnknown, nil, 0},
		{"failed", statelog.OutcomeApplied, errors.New("refused"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sink := &countingEvents{}
			w := &Writer{events: sink}
			w.announce(t.Context(), statelog.Result{Outcome: tc.outcome}, tc.err, row)
			if sink.n != tc.want {
				t.Errorf("announced %d, want %d", sink.n, tc.want)
			}
		})
	}
	// A writer with nowhere to announce is a tool or a test, and says nothing.
	(&Writer{}).announce(t.Context(), statelog.Result{Outcome: statelog.OutcomeApplied},
		nil, row)
}

// THE DELTA IS A SET DIFFERENCE, SORTED, so two nodes describing one write
// describe it byte for byte — and a duplicate in a stored set does not make
// one grant into two rows.
func TestAGrantDeltaIsSortedAndDeduplicated(t *testing.T) {
	t.Parallel()
	added, removed := grantDelta(
		[]iam.Grant{iam.GrantWorkWrite, iam.GrantSecretRead, iam.GrantWorkWrite},
		[]iam.Grant{iam.GrantSecretWrite, iam.GrantSecretRead, iam.GrantAuditRead,
			iam.GrantSecretWrite},
	)
	if !slices.Equal(added, []string{"audit:read", "secrets:write"}) {
		t.Errorf("added %v", added)
	}
	if !slices.Equal(removed, []string{"work:write"}) {
		t.Errorf("removed %v", removed)
	}
	if a, r := grantDelta(nil, nil); a != nil || r != nil {
		t.Errorf("an empty change is %v / %v, want nothing", a, r)
	}
}
