package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// VALIDATE READS A CHECKS FILE as the code host does — code.ParseChecks, the
// one grammar — told apart from either config tier by a `checks:` beside no
// key of either tier (detectTier), or named with -tier checks: a valid file
// summarises its checks, and a broken
// one reports every fault located at its field and line in the payload a
// config document's refusal has, and fails the run in both output modes.
//
// Mutation: drop the TierChecks branch of validateOne and the file is
// validated as a company document — red.
func TestValidateReadsAChecksFile(t *testing.T) {
	t.Parallel()
	good := writeYAML(t, "checks.yaml", "checks:\n"+
		"  - name: unit\n    run: make test\n    timeout_seconds: 600\n    paths: ['**/*.go']\n"+
		"  - name: lint\n    run: make lint\n    timeout_seconds: 300\n    run_in: e2b\n")

	for name, args := range map[string][]string{
		"told apart by its keys": {"validate", good, "-json"},
		"named with -tier":       {"validate", good, "-tier", "checks", "-json"},
	} {
		var out, errOut bytes.Buffer
		if err := run(args, &out, &errOut); err != nil {
			t.Fatalf("%s: a valid checks file was refused: %v (stderr %s)", name, err, errOut.String())
		}
		var res struct {
			Valid    bool             `json:"valid"`
			Tier     string           `json:"tier"`
			Problems []config.Problem `json:"problems"`
			Warnings []config.Warning `json:"warnings"`
			Summary  struct {
				Checks int      `json:"checks"`
				Names  []string `json:"names"`
			} `json:"summary"`
		}
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatalf("%s: the payload is not JSON: %v\n%s", name, err, out.String())
		}
		if !res.Valid || res.Tier != string(TierChecks) || res.Problems == nil || res.Warnings == nil ||
			res.Summary.Checks != 2 || !slices.Equal(res.Summary.Names, []string{"unit", "lint"}) {
			t.Errorf("%s: the payload is %s", name, out.String())
		}
	}

	var prose bytes.Buffer
	if err := run([]string{"validate", good}, &prose, &bytes.Buffer{}); err != nil {
		t.Fatalf("the prose form refused a valid file: %v", err)
	}
	if want := good + ": 2 checks (unit, lint)"; !strings.Contains(prose.String(), want) {
		t.Errorf("the prose summary is %q, want %q", prose.String(), want)
	}

	bad := writeYAML(t, "checks.yaml", "checks:\n"+
		"  - name: unit\n    run: make test\n    timeout_seconds: 99999\n"+
		"  - name: lint\n    run: make lint\n    timeout_seconds: 60\n    run_in: direct\n    timout: 5\n")
	var out bytes.Buffer
	err := run([]string{"validate", bad, "-json"}, &out, &bytes.Buffer{})
	if !errors.Is(err, errSilent) {
		t.Fatalf("a broken file exited %v in -json mode; want the silent failure", err)
	}
	var refused struct {
		Valid    bool             `json:"valid"`
		Tier     string           `json:"tier"`
		Problems []config.Problem `json:"problems"`
	}
	if err := json.Unmarshal(out.Bytes(), &refused); err != nil {
		t.Fatalf("the refusal is not JSON: %v\n%s", err, out.String())
	}
	type located struct {
		path, kind string
		line       int
	}
	var got []located
	for _, p := range refused.Problems {
		got = append(got, located{p.Path, p.Kind, p.Line})
	}
	want := []located{
		{"checks[0].timeout_seconds", "out_of_range", 4},
		{"checks[1].timout", "unknown_field", 9},
		{"checks[1].run_in", "unknown_value", 8},
	}
	if refused.Valid || refused.Tier != string(TierChecks) || !slices.Equal(got, want) {
		t.Errorf("the refusal is valid=%v tier=%q problems %+v; want %+v", refused.Valid, refused.Tier, got, want)
	}
	if err := run([]string{"validate", bad}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "checks[1].run_in") {
		t.Errorf("the prose form of a broken file answered %v; want a failure naming checks[1].run_in", err)
	}
}

// A CHECKS FILE IS NO CONFIG DOCUMENT, so `checks` is a key neither tier has:
// one that had it would score a checks file for that tier and validate it
// against the wrong grammar — and a checks file with a typo'd key beside its
// `checks:` is still told apart, so the typo is reported at its line.
func TestAChecksFileIsToldApartFromBothTiers(t *testing.T) {
	t.Parallel()
	if slices.Contains(config.CompanyKeys(), checksFileKey) ||
		slices.Contains(config.BootstrapKeys(), checksFileKey) {
		t.Fatalf("%q is a config tier's key, so a checks file scores for that tier", checksFileKey)
	}
	for doc, want := range map[string]Tier{
		"checks: []\n":             TierChecks,
		"checks: []\nversoin: 1\n": TierChecks,
		"checks: []\nname: Acme\n": TierCompany,
		"checks: []\nnode: {}\n":   TierBootstrap,
		"name: Acme\nroles: []\n":  TierCompany,
		"store:\n  path: ./x.db\n": TierBootstrap,
	} {
		got, err := detectTier([]byte(doc))
		if err != nil || got != want {
			t.Errorf("%q is told as %q (%v), want %q", doc, got, err, want)
		}
	}
	if _, err := detectTier([]byte("colour: blue\n")); err == nil || !strings.Contains(err.Error(), "-tier checks") {
		t.Errorf("an undecidable document drew %v; want a refusal naming -tier checks among the tiers", err)
	}
}
