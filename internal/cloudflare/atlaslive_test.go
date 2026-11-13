//go:build cloudflarelive

package cloudflare_test

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// A LIVE PROBE OF THE ATLAS'S CLOUDFLARE READ, against a real account, for a
// person who chose to run it: `go test -tags cloudflarelive -run
// TestLiveCloudflareAtlas -v ./internal/cloudflare/`. It is no part of any
// suite, it makes GET calls only, and it prints COUNTS AND KINDS ONLY, never
// a name, an id or an address.
//
// It reads CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN (the provisioner
// token) from the environment, runs the connect check and one read of the
// atlas, and says which optional parts the token reads.
func TestLiveCloudflareAtlas(t *testing.T) {
	account, token := os.Getenv("CLOUDFLARE_ACCOUNT_ID"), os.Getenv("CLOUDFLARE_API_TOKEN")
	if account == "" || token == "" {
		t.Fatal("set CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN to the provisioner token")
	}
	verdict, err := cloudflare.Probe(nil)(t.Context(), setup.Check{Field: "api_token", Value: func(field string) string {
		return map[string]string{"account_id": account, "api_token": token}[field]
	}})
	if err != nil {
		t.Fatalf("the connect check: %v", err)
	}
	suggested := slices.ContainsFunc(verdict.Facts, func(f setup.Fact) bool { return f.Label == "See more in Atlas" })
	t.Logf("connect check: %s, %d warnings, suggests optional permissions: %v", verdict.Outcome, len(verdict.Warnings), suggested)

	c := &config.Company{Name: "live-probe"}
	c.Integrations.Cloudflare = &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{
		AccountID: account, APIToken: "${CLOUDFLARE_API_TOKEN}"}}
	src, _ := collect.Lookup(integration.KindCloudflare)
	read, err := src.Collect(t.Context(), collect.Input{Config: c, Now: time.Now().UTC(),
		Env: config.NewResolver(config.MapSource{"CLOUDFLARE_API_TOKEN": token})})
	if err != nil {
		t.Fatalf("the read: %v", err)
	}
	kinds := map[atlas.Kind]int{}
	parts := map[string]int{}
	for _, claim := range read.Claims {
		if claim.Facet.Kind != atlas.FacetExists {
			continue
		}
		kinds[claim.Subject.Kind]++
		if segments := strings.Split(claim.Subject.Key, "/"); len(segments) > 2 && segments[0] == "cloudflare" {
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
	t.Logf("claims %d, calls %d, withheld %d, optional parts not read %v", len(read.Claims), read.Calls, len(read.Withheld), unread)
	if suggested != (len(unread) > 0) {
		t.Errorf("the connect check suggests optional permissions (%v) and the read left %d parts unread: the two disagree",
			suggested, len(unread))
	}
}
