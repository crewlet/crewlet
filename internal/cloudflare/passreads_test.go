package cloudflare_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// ONE PASS READS THE ACCOUNT'S ZONES AND PERMISSION GROUPS ONCE: the agents'
// session takes the inspection the company half made rather than listing the
// same account again, so a pass costs the token one listing of each, not two.
//
// Mutation: drop the inspection from the session's open, and the second
// listing comes back.
func TestOnePassListsTheZonesOnce(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	client, err := cloudflare.NewClient(cloudflare.ClientOptions{AccountID: testAccount, Token: provisioner,
		HTTP: w.f.server(t)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := cloudflare.Reconcile(t.Context(), cloudflare.Options{
		Client: client, Config: w.company.Integrations.Cloudflare, Company: "Acme",
		Now: func() time.Time { return w.clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Inspection == nil || res.Inspection.ReadFor() != integration.KindCloudflare {
		t.Fatal("the pass hands its session no inspection")
	}
	session, err := cloudflare.OpenAccess(t.Context(), w.vendor.(fakeVendor).Vendor, seataccess.OpenInput{
		Config: w.company, Env: config.NewResolver(config.MapSource{"CF_TOKEN": provisioner}),
		Reads: res.Inspection,
	}, w.f.server(t), func() time.Time { return w.clock })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Options(t.Context(), seataccess.LaneCloudflare); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Observe(t.Context(), seataccess.ObserveInput{}); err != nil {
		t.Fatal(err)
	}
	for _, listing := range []string{"GET /zones", "GET /accounts/" + testAccount + "/tokens/permission_groups",
		"GET /accounts/" + testAccount + "/tokens/verify"} {
		n := 0
		for _, r := range w.f.requests {
			if r == listing {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s was read %d times in one pass, want once", listing, n)
		}
	}
}
