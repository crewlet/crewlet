package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
)

// A GROWTH ACT'S LEASE IS NAMED AT ONE WIDTH WHATEVER ITS ACCOUNT: a Search
// Console site the configuration accepts at any length (a domain property of
// 253 bytes, a URL-prefix property of ten kibibytes) names a lease every
// backend takes, one per account, the same account the same lease.
//
// Mutation: name the lease growthActClass.Resource(resource), and the long
// sites' leases are refused as past coord.MaxLeaseNameBytes.
func TestAGrowthActsLeaseIsNamedAtOneWidthWhateverItsAccount(t *testing.T) {
	t.Parallel()
	label := strings.Repeat("a", 63)
	domain := strings.Join([]string{label, label, label, strings.Repeat("b", 61)}, ".")
	sites := []string{"sc-domain:" + domain, "https://www.example.com/" + strings.Repeat("p/", 5<<10),
		"https://www.example.com/"}
	names := map[string]string{}
	for _, site := range sites {
		if !config.SearchConsoleSite(site) {
			t.Fatalf("premise: the configuration refuses the site of %d bytes", len(site))
		}
		name := growthActResource("write/search_console/" + site)
		if err := coord.CheckResource(name); err != nil {
			t.Errorf("the lease of a site of %d bytes is refused: %v", len(site), err)
		}
		if other, ok := names[name]; ok {
			t.Errorf("two sites share the lease %s: %q and %q", name, other, site)
		}
		names[name] = site
	}
	if growthActResource("write/search_console/"+sites[2]) != growthActResource("write/search_console/"+sites[2]) {
		t.Error("one account names two leases")
	}
}
