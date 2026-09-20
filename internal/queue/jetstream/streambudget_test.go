package jetstream

import "testing"

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
