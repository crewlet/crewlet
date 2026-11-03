package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// completeCloudflare is an enabled block with every requirement answered.
const completeCloudflare = `    enabled: true
    provisioning:
      account_id: 0123456789abcdef0123456789abcdef
      api_token: "${CLOUDFLARE_API_TOKEN}"
      zones: [example.com, Example.org.]`

// A COMPLETE BLOCK VALIDATES, so the refusals below are requirements rather
// than a refusal of everything.
func TestACompleteCloudflareBlockValidates(t *testing.T) {
	t.Parallel()
	if err := validateIntegrationDoc(t, "cloudflare", completeCloudflare); err != nil {
		t.Fatalf("a complete Cloudflare block was refused: %v", err)
	}
	if err := validateIntegrationDoc(t, "cloudflare", "    enabled: false"); err != nil {
		t.Fatalf("a switched-off Cloudflare block was refused: %v", err)
	}
}

// AN ENABLED BLOCK NAMES ITS ACCOUNT, ITS TOKEN AND ONLY ZONE NAMES, each
// refusal naming the field an operator edits.
func TestCloudflareRefusesAnIncompleteOrMalformedBlock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, path string
	}{
		{"no provisioning", "    enabled: true", "integrations.cloudflare.provisioning"},
		{"no account", "    enabled: true\n    provisioning: {api_token: \"${T}\"}",
			"integrations.cloudflare.provisioning.account_id"},
		{"an account that is not an id", "    enabled: true\n    provisioning: {account_id: acme, api_token: \"${T}\"}",
			"integrations.cloudflare.provisioning.account_id"},
		{"no token", "    enabled: true\n    provisioning: {account_id: 0123456789abcdef0123456789abcdef}",
			"integrations.cloudflare.provisioning.api_token"},
		{"a zone with a scheme", "    enabled: true\n    provisioning: {account_id: 0123456789abcdef0123456789abcdef, " +
			"api_token: \"${T}\", zones: [\"https://example.com\"]}", "integrations.cloudflare.provisioning.zones[0]"},
		{"a zone listed twice", "    enabled: true\n    provisioning: {account_id: 0123456789abcdef0123456789abcdef, " +
			"api_token: \"${T}\", zones: [example.com, EXAMPLE.com]}", "integrations.cloudflare.provisioning.zones[1]"},
	} {
		err := validateIntegrationDoc(t, "cloudflare", tc.body)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: %v does not name %s", tc.name, err, tc.path)
		}
	}
}

// A REFERENCE FOR THE ACCOUNT IS UNKNOWN, NOT WRONG: it is resolved and checked
// where the client is built.
func TestCloudflareTakesTheAccountByReference(t *testing.T) {
	t.Parallel()
	body := "    enabled: true\n    provisioning: {account_id: \"${CF_ACCOUNT}\", api_token: \"${T}\"}"
	if err := validateIntegrationDoc(t, "cloudflare", body); err != nil {
		t.Fatalf("an account named by reference was refused: %v", err)
	}
}

// A SEAT'S LEVEL IS ONE OF THE CLOSED SET, and empty reads as read.
func TestACloudflareSeatNamesALevelTheEngineOffers(t *testing.T) {
	t.Parallel()
	doc := func(level string) string {
		return "name: Acme\nintegrations:\n  cloudflare:\n" + completeCloudflare +
			"\nroles:\n  - {name: SRE, handle: sre, integrations: {cloudflare: {enabled: true" + level + "}}}\n"
	}
	if _, err := config.ParseCompany([]byte(doc(", level: admin"))); err == nil ||
		!strings.Contains(err.Error(), "roles[0].integrations.cloudflare.level") {
		t.Errorf("an unknown level = %v, want a refusal naming the seat's level", err)
	}
	c, err := config.ParseCompany([]byte(doc(", level: operate")))
	if err != nil {
		t.Fatalf("operate was refused: %v", err)
	}
	if got := c.Roles[0].Integrations.Cloudflare.LevelOrDefault(); got != config.CloudflareLevelOperate {
		t.Errorf("level = %q, want operate", got)
	}
	c, err = config.ParseCompany([]byte(doc("")))
	if err != nil {
		t.Fatalf("a seat naming no level was refused: %v", err)
	}
	if got := c.Roles[0].Integrations.Cloudflare.LevelOrDefault(); got != config.CloudflareLevelRead {
		t.Errorf("an unset level = %q, want read", got)
	}
}

// THE ENGINE HOLDS A SEAT'S TOKEN ONLY WHERE IT HOLDS THE PROVISIONER: an
// enabled block with provisioning, and nowhere else.
func TestCloudflareSeatCredentialsAreTheEnginesOnlyWithAProvisioner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cf   *config.Cloudflare
		want config.SeatCredentialMode
	}{
		{"absent", nil, config.SeatCredentialExternal},
		{"switched off", &config.Cloudflare{Provisioning: &config.CloudflareProvisioning{}}, config.SeatCredentialExternal},
		{"no provisioning", &config.Cloudflare{Enabled: true}, config.SeatCredentialExternal},
		{"provisioned", &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{}}, config.SeatCredentialEngine},
	} {
		c := &config.Company{}
		c.Integrations.Cloudflare = tc.cf
		if got := c.SeatCredentialMode("cloudflare", false); got != tc.want {
			t.Errorf("%s: mode = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := config.SeatSealedFields("cloudflare"); len(got) != 1 || got[0] != "API_TOKEN" {
		t.Errorf("sealed fields = %v, want [API_TOKEN]", got)
	}
}

// A ZONE IS COMPARED AS CLOUDFLARE MATCHES IT.
func TestCloudflareZoneKey(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"example.com": "example.com", " Example.COM. ": "example.com", "": "",
	} {
		if got := config.CloudflareZoneKey(in); got != want {
			t.Errorf("CloudflareZoneKey(%q) = %q, want %q", in, got, want)
		}
	}
}
