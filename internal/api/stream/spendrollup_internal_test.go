package stream

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/tokens"
)

// THE SPEND PUSH NAMES THE COMPANY'S ENTRIES TO AN ADMIN ALONE, as the `tokens`
// question answers. Its breakdown by `providers.llm` entry is the company's
// configuration; every other reader is handed every figure but that one, and
// told it was withheld. The snapshot hands each reader the same shape its push
// would.
//
// Mutation: broadcast the rollup unshaped, or put it in the snapshot whole, and
// an overview reader is handed the entry's key.
func TestTheSpendPushNamesEntriesToAnAdminAlone(t *testing.T) {
	t.Parallel()
	rollup := SpendRollup(tokens.Rollup{ByProvider: []tokens.ProviderRow{{ProviderKey: "zulu"}}})
	for name, tc := range map[string]struct {
		audience Audience
		variant  string
		entries  bool
	}{
		"an admin":     {NewAudience(adminPrincipal, nil), "admin", true},
		"a reader":     {NewAudience(workletPrincipal, nil), "overview", false},
		"the overview": {NewAudience(overviewPrincipal, nil), "overview", false},
	} {
		variant, data := rollup.For(tc.audience)
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatalf("%s: Marshal: %v", name, err)
		}
		if variant != tc.variant || strings.Contains(string(raw), "zulu") != tc.entries ||
			strings.Contains(string(raw), `"entries_withheld":true`) == tc.entries {
			t.Errorf("%s is handed %q: %s", name, variant, raw)
		}
	}

	svc := newServiceOver(t, authz.NoChart{}, func(string) (SeatState, bool) {
		return SeatState{}, false
	})
	got, _ := svc.Snapshot(NewAudience(overviewPrincipal, nil))["tokens"].(tokens.Rollup)
	if !got.EntriesWithheld {
		t.Errorf("an overview snapshot's spend = %+v, want its entries withheld", got)
	}
	if whole, _ := svc.Snapshot(NewAudience(adminPrincipal, nil))["tokens"].(tokens.Rollup); whole.EntriesWithheld {
		t.Error("an admin's snapshot withheld the spend's entries")
	}
}
