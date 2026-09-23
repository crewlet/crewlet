package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
)

// THE SIGN-IN PROVIDER CARRIES EVERY SETTING ITS VERIFIER READS.
//
// The provider is built here from Tier A, field by field, and a field left out
// is a setting that validates, prints in `crewlet config` and does nothing:
// `groups_claim` was one, so every group mapping read a fixed claim whatever
// the operator named, and the scopes and the probe interval were two more. The
// redirect is derived rather than copied, from `api.external_url`, which is why
// it is compared to that.
//
// Mutation: drop any one field from identityProvider's literal and its row
// here goes red.
func TestTheSignInProviderCarriesEverySettingItsVerifierReads(t *testing.T) {
	t.Parallel()
	boot := config.DefaultBootstrap()
	boot.API.ExternalURL = "https://crewlet.example.com"
	boot.API.Auth.OIDC = &config.APIOIDC{
		Issuer: "https://idp.example.com", ClientID: "crewlet",
		ClientSecret: "not-a-real-secret", GroupsClaim: "roles",
		RequireACR:           "urn:example:mfa",
		Scopes:               []string{"email", "groups"},
		DeactivationProbeRaw: "2h",
	}
	provider := identityProvider(&boot)
	if provider == nil {
		t.Fatal("a configured provider built nothing")
	}
	got := provider.Config()
	block := boot.API.Auth.OIDC
	for _, c := range []struct{ field, got, want string }{
		{"issuer", got.Issuer, block.Issuer},
		{"client_id", got.ClientID, block.ClientID},
		{"client_secret", got.ClientSecret, block.ClientSecret},
		{"groups_claim", got.GroupsClaim, block.GroupsClaim},
		{"require_acr", got.RequireACR, block.RequireACR},
		{"the redirect", got.RedirectURI,
			boot.API.ExternalBase() + auth.PathAuthOIDCCallback},
	} {
		if c.got != c.want {
			t.Errorf("%s reached the verifier as %q, want %q", c.field, c.got, c.want)
		}
	}
	if !slices.Equal(got.Scopes, block.RequestedScopes()) {
		t.Errorf("scopes reached the verifier as %v, want %v", got.Scopes,
			block.RequestedScopes())
	}
	if got.DeactivationProbe != 2*time.Hour {
		t.Errorf("the probe interval reached the provider as %s, want 2h",
			got.DeactivationProbe)
	}

	boot.API.Auth.OIDC = nil
	if identityProvider(&boot) != nil {
		t.Error("a deployment with no provider built one")
	}
}
