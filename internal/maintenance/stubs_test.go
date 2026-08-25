package maintenance_test

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
)

// The ledger stubs exist only so the job builders can be called: what is
// under test is which jobs come back and over what horizon, and a real store
// would answer that question with a database.

type stubConversations struct{}

func (stubConversations) Append(context.Context, string, string, ledger.Session, string, time.Time, int) error {
	return nil
}

func (stubConversations) History(context.Context, string, string, int) ([]ledger.Session, error) {
	return nil, nil
}

func (stubConversations) Threads(context.Context, string, int) ([]ledgerstore.Thread, error) {
	return nil, nil
}
func (stubConversations) Purge(context.Context, time.Time) (int64, error) { return 0, nil }
