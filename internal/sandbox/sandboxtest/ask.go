package sandboxtest

import (
	"context"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// RecordAsk records ask as the question the job a run's row holds asked, the
// way the run's `ask_and_wait` does ([sandbox.PendingStore.RecordAsk]), and
// errs unless it was recorded. A park reads its question off the row, so a
// case that parks a run on a question asks it first, while the job is running.
func RecordAsk(ctx context.Context, s sandbox.PendingStore, turnID string, ask sandbox.Ask) error {
	run, found, err := s.Get(ctx, turnID)
	if err != nil || !found {
		return fmt.Errorf("reading run %s to ask on: found %v, %w", turnID, found, err)
	}
	got, err := s.RecordAsk(ctx, turnID, run.LaunchID, ask)
	if err != nil {
		return err
	}
	if got != sandbox.AskRecorded {
		return fmt.Errorf("RecordAsk on %s (%s) = %q, want it recorded", turnID, run.Status, got)
	}
	return nil
}

// Ask is [RecordAsk] failing the case.
func Ask(t testing.TB, s sandbox.PendingStore, turnID string, ask sandbox.Ask) {
	t.Helper()
	if err := RecordAsk(t.Context(), s, turnID, ask); err != nil {
		t.Fatal(err)
	}
}
