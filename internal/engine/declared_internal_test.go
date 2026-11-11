package engine

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
)

// ONLY A NODE ON AN EXTERNAL BROKER IS ASKED FOR ITS ACCOUNT'S LIMIT, AND ONLY
// AN ACCOUNT THAT ANSWERED WITH NONE IS REFUSED. A member reads its own
// server's cap and a leaf its members' account, which the engine's own broker
// never limits, so neither is refused however its account reads. A budget the
// broker did not answer is load rather than the case's declaration, and an
// account that states zero refuses every create alike on any machine, so
// neither is refused either; nor is a case opened under SizedByTheMachine.
//
// Mutations: drop the broker-kind check (the member and leaf rows are
// refused); refuse whatever the account states (the stated row is refused);
// refuse a budget that was not read (its row is refused); read a limit of
// zero as unstated (its row is refused); ignore the opt-out (its row is
// refused).
func TestOnlyANodeOnAnExternalBrokerIsAskedForItsAccountsLimit(t *testing.T) {
	t.Parallel()
	client := func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamNATS
		b.Stream.URL = "nats://broker.example.com:4222"
	}
	member := func(*config.Bootstrap) {}
	leaf := func(b *config.Bootstrap) {
		b.Stream.Leaf.URLs = []string{"nats-leaf://member-a.example.com:7422"}
	}
	unstated := budgetOf{Limit: -1, Source: jetstream.BudgetUnstated}
	for _, tc := range []struct {
		name    string
		shape   func(*config.Bootstrap)
		broker  any
		machine bool
		refused bool
	}{
		{"a client whose account states none", client, unstated, false, true},
		{"a client whose account states its limit", client,
			budgetOf{Limit: 16 * gib, Source: jetstream.BudgetAccount}, false, false},
		{"a client whose account grants nothing", client,
			budgetOf{Limit: 0, Source: jetstream.BudgetAccountNoTier}, false, false},
		{"a client whose budget the broker did not answer", client, unreadBudget{}, false, false},
		{"a client whose case is the derivation", client, unstated, true, false},
		{"a member", member, unstated, false, false},
		{"a leaf", leaf, unstated, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			tc.shape(&b)
			ctx := t.Context()
			if tc.machine {
				ctx = SizedByTheMachine(ctx, "this case is about what a node derives where its "+
					"broker's account states no limit")
			}
			err := undeclaredBrokerLimit(ctx, &b, tc.broker)
			switch {
			case tc.refused && !errors.Is(err, ErrUndeclaredBrokerLimit):
				t.Fatalf("answered %v, want ErrUndeclaredBrokerLimit", err)
			case !tc.refused && err != nil:
				t.Fatalf("refused: %v", err)
			}
		})
	}
}
