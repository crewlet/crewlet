package inbox_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/inbox"
)

// THE INBOX HORIZON IS READ OFF THE EPOCH ONE WAY: the days the company states
// under the one key, whatever number type the epoch carries them in, and the
// company setting's own default where it states none — so a node with no
// company yet ages an inbox exactly as a company that left the setting unset
// does, and every domain's applier and sweep agree.
//
// Mutation: default to any other number than config's, or read the days under
// another key, and the case fails.
func TestTheInboxHorizonIsReadOffTheEpochOneWay(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	if inbox.RetentionDefaultDays != config.DefaultInboxRetentionDays {
		t.Fatalf("an epoch stating no horizon keeps %d days, and a company that sets none keeps %d",
			inbox.RetentionDefaultDays, config.DefaultInboxRetentionDays)
	}
	for name, tc := range map[string]struct {
		epoch map[string]any
		want  time.Duration
	}{
		"no epoch":    {nil, inbox.RetentionDefaultDays * day},
		"an int":      {map[string]any{inbox.RetentionKey: 30}, 30 * day},
		"an int64":    {map[string]any{inbox.RetentionKey: int64(90)}, 90 * day},
		"a float64":   {map[string]any{inbox.RetentionKey: float64(400)}, 400 * day},
		"no days":     {map[string]any{inbox.RetentionKey: 0}, 0},
		"another key": {map[string]any{"tracker.inbox_retention_days": 30}, inbox.RetentionDefaultDays * day},
	} {
		if got := inbox.Horizon(tc.epoch); got != tc.want {
			t.Errorf("%s: the horizon is %v, want %v", name, got, tc.want)
		}
	}
}
