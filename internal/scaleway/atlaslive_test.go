//go:build scalewaylive

package scaleway_test

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/scaleway"
)

// A LIVE PROBE OF THE ATLAS APPLICATION, against a real Organization, for a
// person who chose to run it: `go test -tags scalewaylive -run
// TestLiveAtlasApplication -v ./internal/scaleway/`. It is no part of any
// suite, and it prints COUNTS AND KINDS ONLY, never a name, an id or an
// address.
//
// It reads SCW_ORGANIZATION_ID and a provisioner key (SCW_ACCESS_KEY,
// SCW_SECRET_KEY) from the environment, makes the atlas application exactly as
// the company pass does, reads the products under its key until Scaleway has
// applied the policy, and then deletes the application, its policy and its
// key. A run that fails part way still deletes what it made.
func TestLiveAtlasApplication(t *testing.T) {
	org, access, secret := os.Getenv("SCW_ORGANIZATION_ID"), os.Getenv("SCW_ACCESS_KEY"), os.Getenv("SCW_SECRET_KEY")
	if org == "" || access == "" || secret == "" {
		t.Fatal("set SCW_ORGANIZATION_ID, SCW_ACCESS_KEY and SCW_SECRET_KEY to the provisioner key")
	}
	client, err := scaleway.NewClient(scaleway.ClientOptions{OrganizationID: org, SecretKey: secret})
	if err != nil {
		t.Fatal(err)
	}
	const company = "live-probe"
	sink := newMemSink()
	pass := func(want bool) []integration.Finding {
		found, err := scaleway.ReconcileAtlas(t.Context(), scaleway.AtlasOptions{
			Client: client, Company: company, Want: want, Sink: sink,
			ApplicationID: sink.values[scaleway.AtlasApplicationName],
			AccessKey:     sink.values[scaleway.AtlasAccessName],
			SecretHeld:    sink.values[scaleway.AtlasSecretName] != "",
		})
		if err != nil {
			t.Fatalf("the pass (want %v): %v", want, err)
		}
		return found
	}
	t.Cleanup(func() {
		if err := scaleway.RemoveAtlas(t.Context(), client, sink.values[scaleway.AtlasApplicationName]); err != nil {
			t.Errorf("the atlas application was NOT deleted, delete %s by hand: %v", scaleway.AtlasName(company), err)
		}
	})
	made := pass(true)
	t.Logf("make: %d findings %v", len(made), titles(made))
	if len(sink.values) != 3 {
		t.Fatalf("the pass sealed %d values, want 3", len(sink.values))
	}
	c := &config.Company{Name: company}
	c.Integrations.Scaleway = &config.Scaleway{Enabled: true, Atlas: true, Provisioning: &config.ScalewayProvisioning{
		OrganizationID: "${SCW_ORGANIZATION_ID}", AccessKey: access, SecretKey: "${SCW_SECRET_KEY}"}}
	source := config.MapSource{"SCW_ORGANIZATION_ID": org, "SCW_SECRET_KEY": secret}
	maps.Copy(source, sink.values)
	src, _ := collect.Lookup(integration.KindScaleway)
	// Scaleway documents up to five minutes for a new policy to apply to
	// Object Storage, so a refused part is asked again, twenty times at most.
	var read collect.Read
	for attempt := range 20 {
		read, err = src.Collect(t.Context(), collect.Input{Config: c, Env: config.NewResolver(source), Now: time.Now().UTC()})
		if err != nil {
			t.Fatalf("the read: %v", err)
		}
		if len(read.Unread) == 0 || attempt == 19 {
			break
		}
		time.Sleep(20 * time.Second) //fixedwait:liveness
	}
	kinds := map[atlas.Kind]int{}
	parts := map[string]int{}
	for _, claim := range read.Claims {
		if claim.Facet.Kind != atlas.FacetExists {
			continue
		}
		kinds[claim.Subject.Kind]++
		if segments := strings.Split(claim.Subject.Key, "/"); len(segments) > 2 && segments[0] == "scaleway" {
			parts[segments[1]]++
		}
	}
	for _, kind := range slices.Sorted(maps.Keys(kinds)) {
		t.Logf("entries of kind %s: %d", kind, kinds[kind])
	}
	for _, part := range slices.Sorted(maps.Keys(parts)) {
		t.Logf("entries under %s: %d", part, parts[part])
	}
	unread := make([]string, 0, len(read.Unread))
	for _, u := range read.Unread {
		unread = append(unread, u.Part)
	}
	t.Logf("claims %d, calls %d, withheld %d, unread parts %v", len(read.Claims), read.Calls, len(read.Withheld), unread)
	if len(unread) != 0 {
		t.Errorf("%d parts stayed unread", len(unread))
	}
	if found := pass(false); len(found) != 0 || len(sink.values) != 0 {
		t.Errorf("the delete left %d findings and %d sealed values", len(found), len(sink.values))
	}
}
