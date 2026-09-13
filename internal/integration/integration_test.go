package integration_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
)

// THE TWO ORDERS HOLD THE SAME SURFACES, which is the only thing keeping a
// second slice honest.
//
// [Kinds] is the order an operator reads them and [ConvergeOrder] is the order
// they depend on each other in. A surface added to one and forgotten in the
// other fails silently and in opposite ways: missing from Kinds it vanishes
// from every listing, missing from ConvergeOrder it sorts to index -1 and
// converges first, ahead of the surface that creates its accounts.
func TestTheConvergeOrderCoversEverySurfaceExactlyOnce(t *testing.T) {
	t.Parallel()
	if len(integration.ConvergeOrder) != len(integration.Kinds) {
		t.Fatalf("ConvergeOrder has %d surfaces, Kinds has %d",
			len(integration.ConvergeOrder), len(integration.Kinds))
	}
	seen := map[integration.Kind]int{}
	for _, kind := range integration.ConvergeOrder {
		seen[kind]++
	}
	for _, kind := range integration.Kinds {
		switch seen[kind] {
		case 1:
		case 0:
			t.Errorf("%s is in no converge order, so it sorts ahead of every "+
				"surface that has one", kind)
		default:
			t.Errorf("%s appears %d times in ConvergeOrder", kind, seen[kind])
		}
	}
}

// THE ACCOUNTS ARE CREATED BEFORE THE PRODUCTS THAT AUTHENTICATE AS THEM.
//
// Atlassian is where an agent's service account is made; Jira and Confluence
// are products that account then works in, and each checks for it with the
// credential Atlassian minted. In reading order both products came first.
//
// Measured over a reconnect: the tracker checked, found the seat still mapped
// to the account the disconnect had deleted, and showed "Action required — you,
// at the third-party app" with a 401 for about thirty-five seconds, until
// Atlassian's own pass made the new account. Nobody had anything to do.
func TestAtlassianConvergesBeforeTheProductsThatUseItsAccounts(t *testing.T) {
	t.Parallel()
	at := func(kind integration.Kind) int {
		return slices.Index(integration.ConvergeOrder, kind)
	}
	org := at(integration.KindAtlassian)
	for _, product := range []integration.Kind{
		integration.KindJira, integration.KindConfluence,
	} {
		if org >= at(product) {
			t.Errorf("atlassian converges at %d and %s at %d: the products "+
				"would ask about an account one pass away from existing",
				org, product, at(product))
		}
	}
}
