package config_test

import (
	"slices"
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
	for _, level := range config.CloudflareLevels() {
		c, err := config.ParseCompany([]byte(doc(", level: " + level)))
		if err != nil {
			t.Fatalf("%s was refused: %v", level, err)
		}
		if got := c.Roles[0].Integrations.Cloudflare.LevelOrDefault(); got != level {
			t.Errorf("level = %q, want %s", got, level)
		}
	}
	if !slices.Contains(config.CloudflareLevels(), config.CloudflareLevelConfigure) {
		t.Errorf("levels %v do not offer configure", config.CloudflareLevels())
	}
	c, err := config.ParseCompany([]byte(doc("")))
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

// A SEAT'S GRANTS ARE HELD TO THE SHAPE THE ENGINE WRITES: a grant id, a
// permission, who granted it and when, why, a scope listed once, an expiry
// after its grant and a target that is one resource's name; one well-formed
// reads back with its instants and target. What a grant
// may name is the vendor's live catalogue, judged where it is saved.
// Mutation: drop checkSeatGrants from checkCloudflare and every case parses.
func TestACloudflareSeatsGrantsAreHeldToTheirShape(t *testing.T) {
	t.Parallel()
	doc := func(grant string) string {
		return "name: Acme\nintegrations:\n  cloudflare:\n" + completeCloudflare +
			"\nroles:\n  - {name: SRE, handle: sre, integrations: {cloudflare: {enabled: true, grants: [" +
			grant + "]}}}\n"
	}
	const good = `{id: gabcdefghij, permission: Origin Write, scope: [example.com], ` +
		`granted_by: Ada, granted_at: "2026-11-15T09:00:00Z", expires_at: "2026-11-15T13:00:00Z", ` +
		`reason: an origin rule, request: abcdefghij, target: api.example.com}`
	c, err := config.ParseCompany([]byte(doc(good)))
	if err != nil {
		t.Fatalf("a well-formed grant was refused: %v", err)
	}
	g := c.Roles[0].Integrations.Cloudflare.Grants[0]
	if g.Permission != "Origin Write" || g.ExpiresAt.Hour() != 13 || g.GrantedAt.Hour() != 9 ||
		g.Target != "api.example.com" {
		t.Errorf("grant = %+v, want it read back whole", g)
	}
	for name, grant := range map[string]string{
		"no id":          `{permission: Origin Write, granted_by: Ada, granted_at: "2026-11-15T09:00:00Z", reason: x}`,
		"no permission":  `{id: gabcdefghij, granted_by: Ada, granted_at: "2026-11-15T09:00:00Z", reason: x}`,
		"no author":      `{id: gabcdefghij, permission: Origin Write, granted_at: "2026-11-15T09:00:00Z", reason: x}`,
		"no reason":      `{id: gabcdefghij, permission: Origin Write, granted_by: Ada, granted_at: "2026-11-15T09:00:00Z"}`,
		"lapses earlier": `{id: gabcdefghij, permission: Origin Write, granted_by: Ada, granted_at: "2026-11-15T09:00:00Z", expires_at: "2026-11-15T08:00:00Z", reason: x}`,
		"twice a zone":   `{id: gabcdefghij, permission: Origin Write, scope: [a.com, a.com], granted_by: Ada, granted_at: "2026-11-15T09:00:00Z", reason: x}`,
		"one id twice":   good + ", " + good,
		"a sentence":     `{id: gabcdefghij, permission: Origin Write, granted_by: Ada, granted_at: "2026-11-15T09:00:00Z", reason: x, target: "the api host"}`,
	} {
		if _, err := config.ParseCompany([]byte(doc(grant))); err == nil ||
			!strings.Contains(err.Error(), "roles[0].integrations.cloudflare.grants") {
			t.Errorf("%s: %v, want a refusal naming the seat's grants", name, err)
		}
	}
}
