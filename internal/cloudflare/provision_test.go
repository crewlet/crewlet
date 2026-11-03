package cloudflare_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// THE SEAT'S TOKEN IS BOUND TO CLOUDFLARE'S OWN SERVER as a bearer header,
// under the field the vendor seals and the document rules know.
func TestTheSeatsTokenIsBoundToCloudflaresServer(t *testing.T) {
	t.Parallel()
	b, ok := config.SeatToolBindingFor("cloudflare")
	if !ok || b.Server != cloudflare.SeatEnv || len(b.Keys) != 1 {
		t.Fatalf("binding = %+v (%v), want Cloudflare's server under %q", b, ok, cloudflare.SeatEnv)
	}
	if k := b.Keys[0]; k.Field != cloudflare.TokenField || k.Header != "Authorization" || k.Prefix != "Bearer " {
		t.Errorf("the server reads %+v, want the token as a bearer Authorization header", k)
	}
	if _, keys, ok := catalogue.SeatKeysFor(catalogue.Launch{Transport: catalogue.TransportHTTP,
		URL: "https://mcp.cloudflare.com/mcp"}); !ok || !slices.Contains(cloudflare.CredentialKeys, keys[0].Header) {
		t.Errorf("Cloudflare's server is recognised reading %+v", keys)
	}
	vendor, ok := seataccess.Lookup(integration.KindCloudflare)
	if !ok {
		t.Fatal("no Cloudflare vendor is registered")
	}
	if got := vendor.Catalog().Credentials; !slices.Equal(got, config.SeatSealedFields("cloudflare")) {
		t.Errorf("the vendor seals %v, the document rules know %v", got, config.SeatSealedFields("cloudflare"))
	}
	if got := seataccess.SinkName(integration.KindCloudflare, cloudflare.TokenField, "sre-lead"); got != "CLOUDFLARE_API_TOKEN_SRE_LEAD" {
		t.Errorf("sealed name = %q", got)
	}
}

// A TOKEN'S NAME SAYS WHICH COMPANY'S AGENT IT IS, and the lifetime and the
// renewal window leave a pass a month to renew.
func TestATokenIsNamedAndTimedForAPerson(t *testing.T) {
	t.Parallel()
	if got := cloudflare.TokenName("Acme Corp", "sre"); got != "crewlet-acme-corp-sre" {
		t.Errorf("TokenName = %q", got)
	}
	if got := cloudflare.TokenName("", "sre"); got != "crewlet-sre" {
		t.Errorf("TokenName with no company = %q", got)
	}
	if cloudflare.RenewWithin >= cloudflare.TokenLifetime || cloudflare.RenewWithin < integration.ExpiryWarning {
		t.Errorf("renewal window %v within a lifetime of %v", cloudflare.RenewWithin, cloudflare.TokenLifetime)
	}
}
