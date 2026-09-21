package config_test

import (
	"encoding/json"
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

// AND A REVISION WITH NO CHART DECODES, ONTO THE DEFAULTS.
//
// The base matters as much as the fields. A revision written before a setting
// existed omits it, and a decode onto a zero value would bring that setting
// back as 0 — which for a timeout, a round cap or a retention window is a
// different company running on the same bytes.
func TestDecodeSettingsReadsARevisionWithNoChart(t *testing.T) {
	t.Parallel()

	got, err := config.DecodeSettings([]byte(
		`{"name":"Acme","mission":"ship it","token_budget":1000}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "Acme" || got.Mission != "ship it" || got.TokenBudget != 1000 {
		t.Errorf("decoded %+v", got)
	}
	defaults := config.DefaultCompany()
	if got.TurnEngine.MaxToolRounds != defaults.TurnEngine.MaxToolRounds ||
		got.TurnEngine.MaxToolRounds == 0 {
		t.Errorf("max_tool_rounds = %d, want the default %d — a revision that "+
			"omits a setting has to land where the authored path puts it",
			got.TurnEngine.MaxToolRounds, defaults.TurnEngine.MaxToolRounds)
	}
}

// A SETTING THIS BUILD DOES NOT KNOW IS KEPT RATHER THAN REFUSED.
//
// This is the reader on the APPLY path, so the bytes it meets were written by
// whichever node served the write — possibly a NEWER one, mid rolling upgrade.
// Refusing an unrecognised key here makes that upgrade an outage in the older
// direction: every older node stops applying the fleet's revision, and each
// one reports a configuration it cannot read. Strictness belongs at the import,
// which is where a person's document arrives and where a typo is a mistake to
// catch.
func TestDecodeSettingsKeepsRunningOnAFieldANewerBuildWrote(t *testing.T) {
	t.Parallel()

	got, err := config.DecodeSettings([]byte(
		`{"name":"Acme","a_setting_from_a_newer_build":{"depth":3}}`))
	if err != nil {
		t.Fatalf("a revision a newer peer wrote was refused: %v", err)
	}
	if got.Name != "Acme" {
		t.Errorf("decoded %+v", got)
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
	settings, err := config.DecodeSettings([]byte(`{"name":"Acme"}`))
	if err != nil {
		t.Fatalf("decode a minimal settings document: %v", err)
	}
	if settings.Name != "Acme" {
		t.Errorf("decoded %+v", settings)
	}

	// EXACTLY THOSE TWO, and this is the assertion with teeth: the split is
	// the FILE's keys minus the SETTINGS' keys, so a field added to
	// [config.Company] and forgotten on [config.Settings] silently becomes
	// a chart key — and then `PUT /config` refuses a document carrying it,
	// naming the org chart, for a setting that has nothing to do with one.
	// Nothing else in the tree would notice.
	chart := config.ChartKeys()
	slices.Sort(chart)
	if !slices.Equal(chart, []string{"roles", "units"}) {
		t.Errorf("the chart owns %v, want exactly roles and units — a key "+
			"here that is neither is a SETTING missing from config.Settings, "+
			"and the config door now refuses every document carrying it",
			chart)
	}
}

// A SETTINGS DOCUMENT IS A COMPANY WITH NO CHART IN IT, BOTH WAYS.
//
// Nothing below the config layer was re-typed when the chart left: an epoch is
// built from a [config.Company], a validator reads one, a provider chain is
// constructed from one. So a stored revision becomes a Company again on the
// apply path, and a field that fell out on either leg is a setting an operator
// wrote and the engine silently never read.
func TestASettingsDocumentRoundTripsThroughACompany(t *testing.T) {
	t.Parallel()

	company := loadCompany(t, exampleNimbus)
	back := config.SettingsOf(company).Company()

	// THE CHART IS GONE, which is the whole point of the trip.
	if len(back.Roles) != 0 || len(back.Units) != 0 {
		t.Errorf("the settings half carries %d seats and %d units, want none",
			len(back.Roles), len(back.Units))
	}
	// AND EVERYTHING ELSE SURVIVED IT, compared as the documents a node
	// stores: the same company minus its chart, encoded the same way.
	want := *company
	want.Roles, want.Units = nil, nil
	if got, expected := mustJSON(t, back), mustJSON(t, &want); got != expected {
		t.Errorf("the round trip lost or changed a setting:\n got %s\nwant %s",
			got, expected)
	}
}

// mustJSON renders a company as the bytes a revision stores.
func mustJSON(t *testing.T, c *config.Company) string {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(raw)
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
