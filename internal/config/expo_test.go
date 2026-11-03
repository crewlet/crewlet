package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// completeExpo is an enabled block with every requirement answered.
const completeExpo = `    enabled: true
    provisioning:
      account_name: example-org
      access_token: "${EXPO_ACCESS_TOKEN}"`

// A COMPLETE BLOCK VALIDATES, so the refusals below are requirements rather
// than a refusal of everything.
func TestACompleteExpoBlockValidates(t *testing.T) {
	t.Parallel()
	if err := validateIntegrationDoc(t, "expo", completeExpo); err != nil {
		t.Fatalf("a complete Expo block was refused: %v", err)
	}
	if err := validateIntegrationDoc(t, "expo", "    enabled: false"); err != nil {
		t.Fatalf("a switched-off Expo block was refused: %v", err)
	}
}

// AN ENABLED BLOCK NAMES ITS ACCOUNT AND ITS TOKEN, each refusal naming the
// field an operator edits.
func TestExpoRefusesAnIncompleteOrMalformedBlock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, path string
	}{
		{"no provisioning", "    enabled: true", "integrations.expo.provisioning"},
		{"no account", "    enabled: true\n    provisioning: {access_token: \"${T}\"}",
			"integrations.expo.provisioning.account_name"},
		{"an account written as a mention", "    enabled: true\n    provisioning: {account_name: \"@example-org\", access_token: \"${T}\"}",
			"integrations.expo.provisioning.account_name"},
		{"an account written as an address", "    enabled: true\n    provisioning: {account_name: \"accounts/example-org\", access_token: \"${T}\"}",
			"integrations.expo.provisioning.account_name"},
		{"no token", "    enabled: true\n    provisioning: {account_name: example-org}",
			"integrations.expo.provisioning.access_token"},
	} {
		err := validateIntegrationDoc(t, "expo", tc.body)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: %v does not name %s", tc.name, err, tc.path)
		}
	}
}

// A REFERENCE FOR THE ACCOUNT IS UNKNOWN, NOT WRONG: it is resolved where the
// client is built.
func TestExpoTakesTheAccountByReference(t *testing.T) {
	t.Parallel()
	body := "    enabled: true\n    provisioning: {account_name: \"${EXPO_ACCOUNT}\", access_token: \"${T}\"}"
	if err := validateIntegrationDoc(t, "expo", body); err != nil {
		t.Fatalf("an account named by reference was refused: %v", err)
	}
}

// A SEAT'S LEVEL IS ONE OF THE CLOSED SET, and empty reads as viewer.
func TestAnExpoSeatNamesALevelTheEngineOffers(t *testing.T) {
	t.Parallel()
	doc := func(level string) string {
		return "name: Acme\nintegrations:\n  expo:\n" + completeExpo +
			"\nroles:\n  - {name: QA, handle: qa, integrations: {expo: {enabled: true" + level + "}}}\n"
	}
	for _, refused := range []string{"admin", "owner", "VIEW"} {
		if _, err := config.ParseCompany([]byte(doc(", level: " + refused))); err == nil ||
			!strings.Contains(err.Error(), "roles[0].integrations.expo.level") {
			t.Errorf("level %q = %v, want a refusal naming the seat's level", refused, err)
		}
	}
	for _, level := range config.ExpoLevels() {
		c, err := config.ParseCompany([]byte(doc(", level: " + level)))
		if err != nil {
			t.Fatalf("%s was refused: %v", level, err)
		}
		if got := c.Roles[0].Integrations.Expo.LevelOrDefault(); got != level {
			t.Errorf("level = %q, want %q", got, level)
		}
	}
	c, err := config.ParseCompany([]byte(doc("")))
	if err != nil {
		t.Fatalf("a seat naming no level was refused: %v", err)
	}
	if got := c.Roles[0].Integrations.Expo.LevelOrDefault(); got != config.ExpoLevelDefault ||
		config.ExpoLevelDefault != config.ExpoLevelViewer {
		t.Errorf("an unset level = %q, want the default, viewer", got)
	}
}

// A PERSON SIGNS IN AS THEMSELVES: a human seat carries no Expo robot.
func TestAHumanSeatCarriesNoExpoRobot(t *testing.T) {
	t.Parallel()
	doc := "name: Acme\nintegrations:\n  expo:\n" + completeExpo +
		"\nroles:\n  - {name: Founder, handle: founder, kind: human, contact: {slack_user_id: U0FOUNDER}, " +
		"integrations: {expo: {enabled: false}}}\n"
	if _, err := config.ParseCompany([]byte(doc)); err == nil ||
		!strings.Contains(err.Error(), "roles[0].integrations.expo") {
		t.Errorf("a human seat's expo block = %v, want a refusal naming it", err)
	}
}

// THE ENGINE HOLDS A SEAT'S TOKEN ONLY WHERE IT HOLDS THE PROVISIONER: an
// enabled block with provisioning, and nowhere else.
func TestExpoSeatCredentialsAreTheEnginesOnlyWithAProvisioner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		expo *config.Expo
		want config.SeatCredentialMode
	}{
		{"absent", nil, config.SeatCredentialExternal},
		{"switched off", &config.Expo{Provisioning: &config.ExpoProvisioning{}}, config.SeatCredentialExternal},
		{"no provisioning", &config.Expo{Enabled: true}, config.SeatCredentialExternal},
		{"provisioned", &config.Expo{Enabled: true, Provisioning: &config.ExpoProvisioning{}}, config.SeatCredentialEngine},
	} {
		c := &config.Company{}
		c.Integrations.Expo = tc.expo
		if got := c.SeatCredentialMode("expo", false); got != tc.want {
			t.Errorf("%s: mode = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := config.SeatSealedFields("expo"); len(got) != 1 || got[0] != "TOKEN" {
		t.Errorf("sealed fields = %v, want [TOKEN]", got)
	}
}

// WHAT A SEAT'S BOX SETS A VARIABLE TO is its env's value, else the last of
// its own setup steps that sets it, as a launch layers them.
func TestARoleSandboxEnvValueLayersAsALaunch(t *testing.T) {
	t.Parallel()
	steps := []config.SandboxSetupStep{
		{Name: "first", Env: map[string]string{"EXPO_TOKEN": "${A}", "NPM_TOKEN": "${NPM_1}"}},
		{Name: "second", Env: map[string]string{"NPM_TOKEN": "${NPM_2}"}},
	}
	box := &config.RoleSandbox{Env: map[string]string{"EXPO_TOKEN": "${EXPO_TOKEN_QA}"}, Setup: steps}
	for key, want := range map[string]string{"EXPO_TOKEN": "${EXPO_TOKEN_QA}", "NPM_TOKEN": "${NPM_2}"} {
		if got, ok := box.EnvValue(key); !ok || got != want {
			t.Errorf("%s = %q (%v), want %q", key, got, ok, want)
		}
	}
	if _, ok := box.EnvValue("OTHER"); ok {
		t.Error("a variable nothing sets reads as set")
	}
	var none *config.RoleSandbox
	if _, ok := none.EnvValue("EXPO_TOKEN"); ok {
		t.Error("no box sets anything")
	}
}
