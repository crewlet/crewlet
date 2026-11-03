package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// completeDigitalOcean is an enabled block with every requirement answered.
const completeDigitalOcean = `    enabled: true
    provisioning:
      team: Example Team`

// A COMPLETE BLOCK VALIDATES, so the refusals below are requirements rather
// than a refusal of everything.
func TestACompleteDigitalOceanBlockValidates(t *testing.T) {
	t.Parallel()
	if err := validateIntegrationDoc(t, "digitalocean", completeDigitalOcean); err != nil {
		t.Fatalf("a complete DigitalOcean block was refused: %v", err)
	}
	if err := validateIntegrationDoc(t, "digitalocean", "    enabled: false"); err != nil {
		t.Fatalf("a switched-off DigitalOcean block was refused: %v", err)
	}
	body := "    enabled: true\n    provisioning: {team: \"${DO_TEAM}\"}"
	if err := validateIntegrationDoc(t, "digitalocean", body); err != nil {
		t.Fatalf("a team named by reference was refused: %v", err)
	}
}

// AN ENABLED BLOCK NAMES ITS TEAM as the control panel shows it, each refusal
// naming the field an operator edits.
func TestDigitalOceanRefusesAnIncompleteOrMalformedBlock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, path string
	}{
		{"no provisioning", "    enabled: true", "integrations.digitalocean.provisioning"},
		{"no team", "    enabled: true\n    provisioning: {}", "integrations.digitalocean.provisioning.team"},
		{"a blank team", "    enabled: true\n    provisioning: {team: \"  \"}",
			"integrations.digitalocean.provisioning.team"},
		{"a padded team", "    enabled: true\n    provisioning: {team: \" Example \"}",
			"integrations.digitalocean.provisioning.team"},
		{"a team on two lines", "    enabled: true\n    provisioning: {team: \"Example\\nTeam\"}",
			"integrations.digitalocean.provisioning.team"},
		{"a company token, which does not exist", "    enabled: true\n    provisioning: " +
			"{team: Example, api_token: \"${T}\"}", "api_token"},
	} {
		err := validateIntegrationDoc(t, "digitalocean", tc.body)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: %v does not name %s", tc.name, err, tc.path)
		}
	}
}

// A SEAT'S LEVEL IS ONE OF THE CLOSED SET, and empty reads as read.
func TestADigitalOceanSeatNamesALevelTheEngineOffers(t *testing.T) {
	t.Parallel()
	doc := func(level string) string {
		return "name: Acme\nintegrations:\n  digitalocean:\n" + completeDigitalOcean +
			"\nroles:\n  - {name: SRE, handle: sre, integrations: {digitalocean: {enabled: true" + level + "}}}\n"
	}
	for _, refused := range []string{"admin", "full_access", "READ"} {
		if _, err := config.ParseCompany([]byte(doc(", level: " + refused))); err == nil ||
			!strings.Contains(err.Error(), "roles[0].integrations.digitalocean.level") {
			t.Errorf("level %q = %v, want a refusal naming the seat's level", refused, err)
		}
	}
	for _, level := range config.DigitalOceanLevels() {
		c, err := config.ParseCompany([]byte(doc(", level: " + level)))
		if err != nil {
			t.Fatalf("%s was refused: %v", level, err)
		}
		if got := c.Roles[0].Integrations.DigitalOcean.LevelOrDefault(); got != level {
			t.Errorf("level = %q, want %q", got, level)
		}
	}
	c, err := config.ParseCompany([]byte(doc("")))
	if err != nil {
		t.Fatalf("a seat naming no level was refused: %v", err)
	}
	if got := c.Roles[0].Integrations.DigitalOcean.LevelOrDefault(); got != config.DigitalOceanLevelRead {
		t.Errorf("an unset level = %q, want read", got)
	}
}

// A PERSON SIGNS IN AS THEMSELVES: a human seat carries no DigitalOcean token.
func TestAHumanSeatCarriesNoDigitalOceanToken(t *testing.T) {
	t.Parallel()
	doc := "name: Acme\nintegrations:\n  digitalocean:\n" + completeDigitalOcean +
		"\nroles:\n  - {name: Founder, handle: founder, kind: human, contact: {slack_user_id: U0FOUNDER}, " +
		"integrations: {digitalocean: {enabled: false}}}\n"
	if _, err := config.ParseCompany([]byte(doc)); err == nil ||
		!strings.Contains(err.Error(), "roles[0].integrations.digitalocean") {
		t.Errorf("a human seat's digitalocean block = %v, want a refusal naming it", err)
	}
}

// THE ENGINE HOLDS A SEAT'S TOKEN ONLY FOR A CONNECTED TEAM: an enabled block
// with provisioning, and nowhere else.
func TestDigitalOceanSeatCredentialsAreTheEnginesOnlyWhenConnected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		do   *config.DigitalOcean
		want config.SeatCredentialMode
	}{
		{"absent", nil, config.SeatCredentialExternal},
		{"switched off", &config.DigitalOcean{Provisioning: &config.DigitalOceanProvisioning{}}, config.SeatCredentialExternal},
		{"no provisioning", &config.DigitalOcean{Enabled: true}, config.SeatCredentialExternal},
		{"connected", &config.DigitalOcean{Enabled: true, Provisioning: &config.DigitalOceanProvisioning{}}, config.SeatCredentialEngine},
	} {
		c := &config.Company{}
		c.Integrations.DigitalOcean = tc.do
		if got := c.SeatCredentialMode("digitalocean", false); got != tc.want {
			t.Errorf("%s: mode = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := config.SeatSealedFields("digitalocean"); len(got) != 2 || got[0] != "TOKEN" || got[1] != "RETIRED_TOKEN" {
		t.Errorf("sealed fields = %v, want [TOKEN RETIRED_TOKEN]", got)
	}
}

// A REPLACED TOKEN REACHES NO BOX: its sealed name is refused in the seat's
// box, while the seat's own token is the box's.
func TestADigitalOceanRetiredTokenIsNamedNowhere(t *testing.T) {
	t.Parallel()
	doc := func(env string) string {
		return "name: Acme\nproviders:\n  sandbox: {fake: true}\n  llm:\n    fast:\n      model: claude-golden\n" +
			"integrations:\n  digitalocean:\n" + completeDigitalOcean + "\nroles:\n  - name: SRE\n    llm: fast\n" +
			"    integrations: {digitalocean: {enabled: true}}\n    sandbox:\n      env:\n        " + env + "\n"
	}
	parse := func(text string) error {
		c, err := config.ParseCompany([]byte(text))
		if err != nil {
			return err
		}
		return c.Validate()
	}
	if err := parse(doc("DIGITALOCEAN_ACCESS_TOKEN: ${DIGITALOCEAN_TOKEN_SRE}")); err != nil {
		t.Fatalf("the seat's own token in its box was refused: %v", err)
	}
	if err := parse(doc("DIGITALOCEAN_ACCESS_TOKEN: ${DIGITALOCEAN_RETIRED_TOKEN_SRE}")); err == nil ||
		!strings.Contains(err.Error(), "DIGITALOCEAN_RETIRED_TOKEN_SRE") {
		t.Errorf("the retired token in a box = %v, want a refusal naming it", err)
	}
}
