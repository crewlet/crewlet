package seataccess_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// THE INTEGRATION WAITING ON THE MIGRATION IS NAMED, AND SO IS WHY.
//
// The pass for a connected integration the migration has not moved says which
// integration, that its access migration has not finished, and the reason the
// migration gave where it gave one (Datadog's role lookup failed); with no
// reason it says no more than it knows. Mutation: drop the integration's name
// or the reason, and a case here fails.
func TestTheWaitingIntegrationIsNamedWithItsReason(t *testing.T) {
	t.Parallel()
	why := `the lookup of the Datadog role "Datadog Admin Role" failed: 503 Service Unavailable`
	for name, tc := range map[string]struct {
		kind   integration.Kind
		reason string
		want   []string
		absent []string
	}{
		"with the migration's reason": {kind: integration.KindDatadog, reason: why,
			want: []string{"Datadog's access migration has not finished", why}},
		"with no reason": {kind: integration.KindGitLab,
			want:   []string{"GitLab's access migration has not finished"},
			absent: []string{": "}},
		"atlassian by its own name": {kind: integration.KindAtlassian,
			want: []string{"Atlassian's access migration has not finished"}},
	} {
		f := seataccess.UnmigratedFinding(tc.kind, tc.reason)
		for _, w := range tc.want {
			if !strings.Contains(f.Detail, w) {
				t.Errorf("%s: %q does not say %q", name, f.Detail, w)
			}
		}
		for _, a := range tc.absent {
			if strings.Contains(f.Detail, a) {
				t.Errorf("%s: %q says %q with no reason to give", name, f.Detail, a)
			}
		}
		if f.Kind != integration.FindingIdentityMissing || f.Remedy == "" {
			t.Errorf("%s: finding = %+v", name, f)
		}
	}
	// AND THE SYNC CARRIES IT: an unmigrated integration's one finding is
	// this one, with the reason it was handed.
	h := newHarness()
	report, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
		Vendor: h.v, Open: seataccess.OpenInput{Config: company(nil)}, Ledger: h.ledger,
		Sink: h.sink, Unmigrated: true, UnmigratedReason: why,
	})
	if err != nil || len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, why) {
		t.Errorf("an unmigrated sync = %+v, %v; want one finding carrying the reason", report, err)
	}
}
