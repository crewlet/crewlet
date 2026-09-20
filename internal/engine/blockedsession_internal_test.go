package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
)

type capturingConversations struct{ entries []ledger.Session }

func (c *capturingConversations) Append(_ context.Context, _, _ string,
	e ledger.Session, _ string, _ time.Time, _ int,
) error {
	c.entries = append(c.entries, e)
	return nil
}

func (c *capturingConversations) History(context.Context, string, string, int) ([]ledger.Session, error) {
	return nil, nil
}

func (c *capturingConversations) Threads(context.Context, string, int) ([]ledgerstore.Thread, error) {
	return nil, nil
}

func (c *capturingConversations) Purge(context.Context, time.Time) (int64, error) { return 0, nil }

// THE ACCOUNT REACHES THE ROW, which is the half a ledger unit test cannot
// see: it proves BuildSession renders what it is given, not that the engine
// gives it anything.
//
// The outcome vocabulary is turn's, and turn imports ledger — so the
// dispatcher is the only frame that can say a round was blocked. Drop that
// one assignment and every blocked turn files a row indistinguishable from a
// turn that finished the work, with nothing failing.
func TestABlockedTurnsAccountReachesTheConversationLedger(t *testing.T) {
	t.Parallel()
	conv := &capturingConversations{}
	d := &Dispatcher{Conversations: conv}
	res := turn.Result{
		Decision: phase.Done,
		Artifact: "I've asked the founder which repo this belongs in.",
		LastWork: &turn.Work{
			Outcome:  turn.OutcomeBlocked,
			Summary:  "asked the founder which repo",
			Evidence: "asked @founder which repo to file against; waiting on them",
		},
		Delivered: true,
	}
	d.RecordSession(context.Background(), "ceo", "chan-1", "run-1", "wk-1",
		"Can you open a task for test purpose?", res, time.Now())

	if len(conv.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(conv.entries))
	}
	if got := conv.entries[0].BlockedOn; !strings.Contains(got, "waiting on them") {
		t.Errorf("the blocked account never reached the row: %q", got)
	}
}

// And a turn that finished the work files no account at all, so the line
// stays meaningful on the entries that carry it.
func TestAnUnblockedTurnFilesNoBlockedAccount(t *testing.T) {
	t.Parallel()
	conv := &capturingConversations{}
	d := &Dispatcher{Conversations: conv}
	res := turn.Result{
		Decision: phase.Done,
		Artifact: "Filed NIM-14.",
		LastWork: &turn.Work{
			Outcome: turn.OutcomeDelivered, Summary: "filed it",
			// Evidence is meaningless on a delivered round, but a stray
			// value must not leak into the row: the field is keyed on the
			// OUTCOME, not on whether some text happens to be present.
			Evidence: "left over from an earlier round",
		},
		Delivered: true,
	}
	d.RecordSession(context.Background(), "ceo", "chan-1", "run-1", "wk-1", "file it", res, time.Now())

	if len(conv.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(conv.entries))
	}
	if got := conv.entries[0].BlockedOn; got != "" {
		t.Errorf("a delivered turn filed a blocked account: %q", got)
	}
}
