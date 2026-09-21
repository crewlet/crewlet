package config_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// THE SETTINGS DOCUMENT, and the one refusal it exists to make.
//
// A stored revision written before the org chart moved onto its own log still
// carries `roles:` and `units:` inside it. This build reads the two separately,
// and what it must NEVER do is decode such a revision by dropping them: the
// node would boot, serve, and run a company with no seats in it — every
// mailbox gone, every routing decision answering nobody — and nothing would
// say why, because from the engine's point of view the configuration decoded
// cleanly.

// A REVISION THAT STILL CARRIES A CHART IS REFUSED, NAMING THE PROCEDURE.
func TestDecodeSettingsRefusesARevisionCarryingAChart(t *testing.T) {
	t.Parallel()

	for name, document := range map[string]string{
		"seats and units": `
name: Acme
roles:
  - name: CEO
    handle: ceo
units:
  - name: Engineering
    id: engineering
`,
		"seats alone": `
name: Acme
roles:
  - name: CEO
    handle: ceo
`,
		"units alone": `
name: Acme
units:
  - name: Engineering
    id: engineering
`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := config.DecodeSettings([]byte(document))
			if !errors.Is(err, config.ErrRevisionCarriesAChart) {
				t.Fatalf("decoding answered %v, want the chart refusal — a "+
					"decode that dropped the chart would leave this node "+
					"serving a company with no seats in it, and nothing "+
					"would say so", err)
			}
			// THE PROCEDURE, not the field. An operator reading this has
			// to know what to run, and "unknown key: roles" sends them
			// looking for a typo they did not make.
			if !strings.Contains(err.Error(), "crewlet config import") {
				t.Errorf("the refusal does not name what to run: %v", err)
			}
		})
	}
}

// AND A REVISION WITH NO CHART DECODES.
func TestDecodeSettingsReadsARevisionWithNoChart(t *testing.T) {
	t.Parallel()

	got, err := config.DecodeSettings([]byte(`
name: Acme
mission: ship it
token_budget: 1000
`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "Acme" || got.Mission != "ship it" || got.TokenBudget != 1000 {
		t.Errorf("decoded %+v", got)
	}
}

// AN UNKNOWN SETTING IS REFUSED RATHER THAN IGNORED.
//
// Strict, like every other decode here: a typo in a setting is a refusal at
// the moment it is written instead of a value that silently never took effect.
func TestDecodeSettingsRefusesAnUnknownField(t *testing.T) {
	t.Parallel()

	_, err := config.DecodeSettings([]byte("name: Acme\nmision: typo\n"))
	if err == nil {
		t.Fatal("a misspelled setting decoded cleanly, so it would silently " +
			"never take effect")
	}
	if errors.Is(err, config.ErrRevisionCarriesAChart) {
		t.Errorf("a typo was reported as a chart: %v", err)
	}
}

// THE SPLIT IS DERIVED FROM THE TYPES, NOT TYPED TWICE.
//
// A hand-written key list in this package has already drifted once: it counted
// a key that appears nowhere in this tree, the schema, the examples or the
// docs, while omitting five that do — so a document with only `units:` scored
// nothing on either tier and was reported undecidable, which is the commonest
// authoring shape there is. This asserts the derivation rather than the list.
func TestTheChartKeysAreExactlyWhatTheFileHasAndTheSettingsDoNot(t *testing.T) {
	t.Parallel()

	// THE FILE STILL CARRIES BOTH, which is what keeps `crewlet config
	// import` reading one document a founder authored.
	keys := config.CompanyKeys()
	for _, want := range []string{"roles", "units"} {
		if !slices.Contains(keys, want) {
			t.Errorf("the FILE type no longer declares %q — an operator "+
				"authors one document describing a company, and splitting "+
				"the authoring surface is a change to the product rather "+
				"than to where the engine keeps things", want)
		}
	}

	// AND THE SETTINGS DO NOT.
	settings, err := config.DecodeSettings([]byte("name: Acme\n"))
	if err != nil {
		t.Fatalf("decode a minimal settings document: %v", err)
	}
	if settings.Name != "Acme" {
		t.Errorf("decoded %+v", settings)
	}
}

// AN AUTHORED FILE SPLITS INTO SETTINGS WITHOUT LOSING ONE.
//
// What an import stores is this half; the other half is what it publishes to
// the chart log. A field that fell between them would be a setting an operator
// wrote and the engine silently never read.
func TestEverySettingOfAnAuthoredFileReachesTheSettings(t *testing.T) {
	t.Parallel()

	company := loadCompany(t, exampleNimbus)
	settings := config.SettingsOf(company)
	if settings == nil {
		t.Fatal("an authored company produced no settings")
	}

	// THE FIELDS AN OPERATOR IS MOST LIKELY TO NOTICE MISSING, checked by
	// value rather than by reflection: a reflective walk would pass for a
	// copy that filled every field with its zero value.
	if settings.Name != company.Name {
		t.Errorf("name = %q, want %q", settings.Name, company.Name)
	}
	if settings.Mission != company.Mission {
		t.Errorf("mission = %q, want %q", settings.Mission, company.Mission)
	}
	if len(settings.MCPServers) != len(company.MCPServers) {
		t.Errorf("%d mcp servers, want %d — every tool server an operator "+
			"declared is one this company would stop running",
			len(settings.MCPServers), len(company.MCPServers))
	}
	if settings.Tracker.Backend != company.Tracker.Backend {
		t.Errorf("tracker backend = %q, want %q",
			settings.Tracker.Backend, company.Tracker.Backend)
	}
	if settings.Knowledge.Backend != company.Knowledge.Backend {
		t.Errorf("knowledge backend = %q, want %q",
			settings.Knowledge.Backend, company.Knowledge.Backend)
	}
	if len(settings.Providers.LLM) != len(company.Providers.LLM) {
		t.Errorf("%d llm providers, want %d",
			len(settings.Providers.LLM), len(company.Providers.LLM))
	}
	if settings.TokenBudget != company.TokenBudget {
		t.Errorf("token budget = %d, want %d",
			settings.TokenBudget, company.TokenBudget)
	}

	// NIL IN, NIL OUT, because a caller that held no company must not be
	// handed a settings document full of zero values it would then store.
	if config.SettingsOf(nil) != nil {
		t.Error("a nil company produced a settings document, which a caller " +
			"would store as a company with no name and no providers")
	}
}
