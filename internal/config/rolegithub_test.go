package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
)

// THE TWO TIER LISTS ARE WRITTEN TWICE AND MUST NOT DRIFT.
//
// config is the leaf every integration package depends on, so it restates the
// vocabulary rather than importing internal/github and inverting that. The
// cost of restating is exactly this: a tier added on one side and not the
// other would validate here and be read as read-only there, or be refused
// here and be perfectly good there.
func TestTheTierVocabularyAgreesWithTheCodeHostsOwn(t *testing.T) {
	t.Parallel()
	if len(config.CodeAccessTiers) != len(github.Tiers) {
		t.Fatalf("config knows %v, github knows %v", config.CodeAccessTiers, github.Tiers)
	}
	for i, tier := range config.CodeAccessTiers {
		if string(github.Tiers[i]) != tier {
			t.Errorf("tier %d: config says %q, github says %q", i, tier, github.Tiers[i])
		}
	}
	if config.TierReadOnly != string(github.DefaultTier) {
		t.Errorf("config defaults to %q, github to %q", config.TierReadOnly, github.DefaultTier)
	}
}

// A SEAT NOBODY HAS THOUGHT ABOUT ASKS FOR MORE RATHER THAN ALREADY HOLDING
// IT. Silence is read-only, and a hyphen an operator typed is read the way
// the code host reads it rather than refused for punctuation.
func TestASeatsTierDefaultsToTheLeastAccess(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"":              config.TierReadOnly,
		"full-access":   config.TierFullAccess,
		"  Review ":     config.TierReview,
		"administrator": config.TierReadOnly,
	} {
		block := &config.RoleGitHub{Tier: raw}
		if got := block.TierOrDefault(); got != want {
			t.Errorf("TierOrDefault(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A SEAT WITH AN APP HALF BUILT IS NOT HELD. Creating the app and installing
// it are two acts by a person, and the second can be a day after the first,
// so the engine must be able to tell the states apart rather than minting
// against a record that cannot answer.
func TestASeatIsHeldOnlyWithAnAppAnInstallationAndAKey(t *testing.T) {
	t.Parallel()
	full := config.RoleGitHub{AppID: 7, InstallationID: 9, PrivateKey: "${SEAT_PEM}"}
	if !full.Held() {
		t.Error("a seat with an app, an installation and a key is not held")
	}
	for name, block := range map[string]config.RoleGitHub{
		"no installation yet": {AppID: 7, PrivateKey: "${SEAT_PEM}"},
		"no key":              {AppID: 7, InstallationID: 9},
		"nothing":             {},
	} {
		if block.Held() {
			t.Errorf("%s reports held", name)
		}
	}
}

// WORKFLOW WRITES ARE FOR A FULL_ACCESS SEAT, AND ONLY WHEN IT SAYS SO.
//
// GitHub refuses a push touching `.github/workflows` without them, and the
// same grant lets the seat's branches run a workflow it rewrote with the
// repository's secrets. A seat that cannot push code has nothing to push a
// workflow with, so the opt-in on any other tier is a document saying two
// things at once, and it is refused at the field rather than read as either.
func TestWorkflowWritesAreRefusedBelowFullAccess(t *testing.T) {
	t.Parallel()
	seat := func(github string) string {
		return "name: Acme\nroles:\n  - name: Dev\n    integrations:\n      github:\n        enabled: false\n" + github
	}
	for name, block := range map[string]string{
		"silent tier": "        workflows: true\n",
		"read_only":   "        tier: read_only\n        workflows: true\n",
		"review":      "        tier: review\n        workflows: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseCompany([]byte(seat(block)))
			var found *config.Problem
			for _, p := range config.Problems(err) {
				if p.Path == "roles[0].integrations.github.workflows" {
					found = &p
				}
			}
			if found == nil {
				t.Fatalf("no problem at the workflows field: %v", err)
			}
			if found.Kind != "conflict" || !strings.Contains(found.Message, "full_access") {
				t.Errorf("workflows = %s %q, want a conflict naming full_access", found.Kind, found.Message)
			}
		})
	}

	// The counterfactual, so the table cannot pass on a validator that
	// refuses the field outright: full access may opt in, hyphen and all,
	// and the running seat carries what the document said.
	for _, tier := range []string{"full_access", "full-access"} {
		company, err := config.ParseCompany([]byte(seat("        tier: " + tier + "\n        workflows: true\n")))
		if err != nil {
			t.Fatalf("a %s seat opting in to workflows was refused: %v", tier, err)
		}
		if app := company.Roles[0].Seat().GitHub; app == nil || !app.Workflows {
			t.Errorf("a %s seat's opt-in did not reach the running seat: %+v", tier, app)
		}
	}
	// OFF UNLESS WRITTEN.
	company, err := config.ParseCompany([]byte(seat("        tier: full_access\n")))
	if err != nil {
		t.Fatal(err)
	}
	if app := company.Roles[0].Seat().GitHub; app == nil || app.Workflows {
		t.Errorf("a full_access seat that said nothing opts in to workflows: %+v", app)
	}
}
