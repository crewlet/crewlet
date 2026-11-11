package gitlet

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY REFUSAL THE CODE LOG CAN MAKE IS DECIDED HERE, BOTH WAYS: whether it is
// about THIS NODE, so a write is handed to the repository's next committer
// under the same id ([nodeRefusals]), and whether every publish this node makes
// meets it, so a reconcile ends rather than reporting it once a repository
// ([sharedFailure]).
//
// The walk is over [statelog.Reasons], so a reason added there fails here until
// somebody decides both. `usage_unknown` was such a reason, made without a
// type: a push nothing had appended was answered unknown and never handed on.
//
// Mutation: drop any reason from nodeRefusals or sharedFailure and its row
// goes red.
func TestEveryCodeLogRefusalIsDecided(t *testing.T) {
	t.Parallel()
	type decision struct{ node, shared bool }
	decided := map[statelog.Reason]decision{
		statelog.ReasonEvicted:         {node: true, shared: true},
		statelog.ReasonEvictionUnknown: {node: true, shared: true},
		statelog.ReasonBelowFloor:      {node: true, shared: true},
		statelog.ReasonFloorUnknown:    {node: true, shared: true},
		statelog.ReasonUsageUnknown:    {node: true, shared: true},
		statelog.ReasonSkew:            {node: true, shared: true},
		statelog.ReasonLogTruncated:    {node: true, shared: true},
		statelog.ReasonWrongStream:     {node: true, shared: true},
		// ABOUT THIS NODE AND ONE SUBJECT: another repository's publish
		// may pass.
		statelog.ReasonDeferred: {node: true},
		statelog.ReasonBehind:   {node: true},
		// THE SAME FOR EVERY NODE, so no runner-up does better.
		statelog.ReasonLogFull: {shared: true},
		// THE WRITE'S OR THE OBJECT'S OWN.
		statelog.ReasonDeleted:        {},
		statelog.ReasonAbandoned:      {},
		statelog.ReasonOvertaken:      {},
		statelog.ReasonRecordTooLarge: {},
		statelog.ReasonBrokerRefused:  {},
		statelog.ReasonOpReused:       {},
		statelog.ReasonSuperseded:     {},
	}
	for _, r := range statelog.Reasons() {
		d, ok := decided[r]
		if !ok {
			t.Errorf("%s is not decided here: is it about this node, and does every "+
				"publish this node makes meet it?", r)
			continue
		}
		if nodeRefusals[r] != d.node {
			t.Errorf("%s hands the write to the next committer: %v, want %v", r,
				nodeRefusals[r], d.node)
		}
		if got := sharedFailure(&statelog.Unavailable{Reason: r}); got != d.shared {
			t.Errorf("%s ends a reconcile: %v, want %v", r, got, d.shared)
		}
	}
	for r := range nodeRefusals {
		if !r.Valid() {
			t.Errorf("nodeRefusals names %q, which no build refuses with", r)
		}
	}
}
