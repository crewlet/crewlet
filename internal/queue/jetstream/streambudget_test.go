package jetstream

import (
	"context"
	"errors"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// A DECLARED STORE LIMIT IS THE LIMIT THE BUDGET REPORTS.
//
// # What this holds together
//
// [Config.StoreMaxBytes] is the one lever an operator has over an embedded
// broker's cap, and it works by being handed to nats-server as its own
// JetStreamMaxStore rather than by being read anywhere in this package. So
// nothing in the budget's own code mentions it, and nothing would fail if the
// option stopped being passed: [Queue.StreamBudget] would go on reporting
// [BudgetServerStore], truthfully, for a cap the operator did not choose.
// This is the test that ties the two ends together.
//
// # Why the read-back is the assertion rather than the option
//
// Because there are two levers and only one of them is enforced on both
// topologies. Setting the ACCOUNT's limit satisfies an account read and leaves
// a clustered create unbounded; setting the SERVER's bounds both and is
// invisible to an account read. A test asserting that the option was passed
// would pass with the wrong lever set, which is exactly the mistake that
// reinstates sizing a company's logs from free disk.
func TestADeclaredStoreLimitIsTheBudgetsLimit(t *testing.T) {
	t.Parallel()
	const declared = int64(9) << 30
	q := newQueueWith(t, Config{StoreDir: t.TempDir(), StoreMaxBytes: declared})

	budget, err := q.StreamBudget(t.Context())
	if err != nil {
		t.Fatalf("StreamBudget: %v", err)
	}
	if budget.Source != BudgetServerStore || budget.Limit != declared {
		t.Fatalf("budget = %+v, want a %d-byte limit from %s: every state-log "+
			"ceiling is sized against this number, so a broker reporting "+
			"anything else is one whose logs were sized against a cap nobody "+
			"declared", budget, declared, BudgetServerStore)
	}
	if room := budget.Available(); room <= 0 || room > declared {
		t.Errorf("a fresh broker declared %d bytes has %d to reserve",
			declared, room)
	}
}

// accountInfoFails is a broker whose status request does not answer, and whose
// everything else does.
type accountInfoFails struct {
	jetstream.JetStream
	err error
}

func (f accountInfoFails) AccountInfo(context.Context) (*jetstream.AccountInfo, error) {
	return nil, f.err
}

// A BROKER THAT CANNOT SAY WHAT ITS ACCOUNT ALLOWS STILL SAYS WHAT IT IS HELD
// TO.
//
// The two halves come from different places and fail independently: an
// embedded server's own cap is a struct field in this process and cannot fail
// to answer, while the account's limit is a round trip — and on an embedded
// broker it is a limit nobody sets, so it is nearly always "unstated" anyway.
// Failing the whole read on it therefore threw away the one number this node
// definitely had, for the one that is usually absent: [internal/engine] reads
// a failure here as "no limit" and sizes its state logs from FREE DISK, which
// is looser than the cap that just went unread and is the arithmetic that cap
// exists to replace.
func TestAnUnreadableAccountStillReportsTheServersOwnCap(t *testing.T) {
	t.Parallel()
	const declared = int64(9) << 30
	q := newQueueWith(t, Config{StoreDir: t.TempDir(), StoreMaxBytes: declared})

	live := q.js
	q.js = accountInfoFails{JetStream: live, err: errors.New("no responders")}
	budget, err := q.StreamBudget(t.Context())
	q.js = live

	if err != nil {
		t.Fatalf("the whole read failed because the half this node does not "+
			"need failed: %v — the caller answers that by sizing every state "+
			"log from free disk", err)
	}
	if budget.Source != BudgetServerStore || budget.Limit != declared {
		t.Errorf("budget = %+v, want the declared %d-byte limit from %s",
			budget, declared, BudgetServerStore)
	}
}

// AND AN EXTERNAL BROKER'S DOES FAIL, because there is nothing else to hold a
// ceiling to.
//
// The account's limit is all a client of somebody else's cluster can read.
// Degrading to "unstated" there would report no limit on a broker that has
// one, and the caller reads no limit as room for the volume it measured.
func TestAnUnreadableAccountFailsWhereItIsTheOnlyLimit(t *testing.T) {
	t.Parallel()
	q := newQueueWith(t, Config{StoreDir: t.TempDir()})

	// AN EXTERNAL QUEUE IS ONE WITH NO EMBEDDED SERVER, which is the field
	// [Queue.StreamBudget] branches on and nothing else.
	live, embedded := q.js, q.embedded
	q.js, q.embedded = accountInfoFails{JetStream: live,
		err: errors.New("no responders")}, nil
	_, err := q.StreamBudget(t.Context())
	q.js, q.embedded = live, embedded

	if err == nil {
		t.Fatal("an external broker that could not state its account limit " +
			"reported a budget anyway, so every ceiling is held to a limit " +
			"nobody read")
	}
}
