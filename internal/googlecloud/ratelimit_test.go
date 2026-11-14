package googlecloud_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/googlecloud"
)

// A READ WAITS OUT A SHORT RATE LIMIT AS GOOGLE ASKS, AND A WRITE NEVER DOES:
// a listing Google limits twice is sent a third time after the two waits it
// named, on the client's own clock, and a create Google refuses with the same
// status, which is how it answers a spent quota, is handed back at once.
//
// Mutation: hand a read's 429 straight back, and one busy minute fails the
// hour's read; wait on a write's, and a project at its quota of service
// accounts costs every create three waits.
func TestAReadWaitsOutGooglesRateLimitAndAWriteDoesNot(t *testing.T) {
	t.Parallel()
	key, pemText := fakeKey(t)
	clock := newTestClock()
	f := newFakeGoogle(t, &key.PublicKey, clock)
	services := "GET /v1/projects/" + projectWeb + "/services"
	f.limited = map[string]int{services: 2}
	client, err := googlecloud.NewClient(googlecloud.ClientOptions{Company: testCompany, Provisioner: provisioner,
		Provider: testProvider, FederationKey: pemText, HTTP: f.server(t)})
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	googlecloud.SetClock(client, clock.now, func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return clock.sleep(ctx, d)
	})
	enabled, err := client.EnabledServices(t.Context(), projectWeb)
	if err != nil || !enabled["compute.googleapis.com"] {
		t.Fatalf("the limited listing answered %v and %v", enabled, err)
	}
	if !slices.Equal(waits, []time.Duration{7 * time.Second, 7 * time.Second}) {
		t.Errorf("the read waited %v, want the two waits Google named", waits)
	}
	create := "POST /v1/projects/" + identity + "/serviceAccounts"
	f.edit(func(f *fakeGoogle) { f.limited[create] = 1 })
	waits = nil
	if _, err = client.CreateServiceAccount(t.Context(), "crewlet-sre", "SRE", "an agent"); googlecloud.Status(err) != http.StatusTooManyRequests {
		t.Fatalf("a limited create answered %v", err)
	}
	if len(waits) != 0 {
		t.Errorf("a write waited %v on a 429", waits)
	}
}
