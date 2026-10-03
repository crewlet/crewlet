package config_test

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
)

// THE SETTINGS DOCUMENT, and the one refusal it exists to make.
//
// A settings revision carries no org chart: the chart is a log of its own, and
// every door that writes a revision keeps `roles:` and `units:` out of it. A
// stored revision that carries them anyway is the wrong shape, and what the
// apply path must NEVER do is decode one by dropping them: the node would
// boot, serve, and run a company with no seats in it — every mailbox gone,
// every routing decision answering nobody — and nothing would say why, because
// from the engine's point of view the configuration decoded cleanly.

// A REVISION THAT CARRIES A CHART IS REFUSED, NAMING WHERE THE CHART LIVES.
//
// Every document here is in the STORED form — JSON, as marshalling a
// [config.Company] writes it — because that is the only form this reader ever
// meets, and a case in the authored YAML would be testing a probe against
// bytes the apply path never hands it.
func TestDecodeSettingsRefusesARevisionCarryingAChart(t *testing.T) {
	t.Parallel()

	indented, err := json.MarshalIndent(config.Company{
		Name:  "Acme",
		Roles: []config.Role{{Name: "CEO", Handle: "ceo"}},
	}, "", "\t")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for name, document := range map[string]string{
		"seats and units": `{"name":"Acme",` +
			`"roles":[{"name":"CEO","handle":"ceo"}],` +
			`"units":[{"name":"Engineering","id":"engineering"}]}`,
		"seats alone": `{"name":"Acme","roles":[{"name":"CEO","handle":"ceo"}]}`,
		"units alone": `{"name":"Acme","units":[{"name":"Engineering","id":"engineering"}]}`,
		"indented":    string(indented),
		// A KEY NAMED TWICE is a document encoding/json decodes (the last
		// one wins) and yaml.v3 refuses. While the chart was looked for
		// with the YAML parser, this revision slipped past the refusal
		// and decoded cleanly with its seats dropped.
		"a key named twice": `{"name":"Acme","name":"Acme",` +
			`"roles":[{"name":"CEO","handle":"ceo"}]}`,
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
			// WHERE THE CHART LIVES, not the field. An operator reading
			// this has to know that a chart is written through /chart and
			// that the import divides a company file between the two —
			// "unknown key: roles" sends them looking for a typo they did
			// not make.
			for _, names := range []string{"/chart", "crewlet config import"} {
				if !strings.Contains(err.Error(), names) {
					t.Errorf("the refusal does not name %s: %v", names, err)
				}
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

	got, err := config.DecodeSettings([]byte(`{"name":"Acme","mission":"ship it",` +
		`"timezone":"Europe/Berlin","token_budget":{"day":1000}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "Acme" || got.Mission != "ship it" || got.Timezone != "Europe/Berlin" {
		t.Errorf("decoded %+v", got)
	}
	if want := (org.TokenCeilings{period.Day: 1000}); !maps.Equal(got.TokenBudget.Ceilings(), want) {
		t.Errorf("token_budget = %v, want %v", got.TokenBudget.Ceilings(), want)
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

// A TOKEN BUDGET OF ONE NUMBER IS REFUSED BY WHAT TO WRITE INSTEAD, through
// the JSON door as through the YAML one.
//
// One number was a ceiling for the life of the deployment, and there is no
// reading of it this build could honour: as a day, a week or a month it would
// be a different company from the one its author meant. encoding/json's own
// refusal names a Go type, which is no help to the person holding the
// document, so the refusal names the window form with their own number in it.
//
// A WINDOW THIS BUILD DOES NOT KNOW decodes rather than failing the revision,
// for the reason every stored field does: a newer peer may have written it.
func TestATokenBudgetOfOneNumberIsRefusedInTheStoredForm(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		doc  string
		says string
	}{
		"a number":        {`{"name":"Acme","token_budget":5000000}`, "token_budget: {month: 5000000}"},
		"a zero":          {`{"name":"Acme","token_budget":0}`, "not one number"},
		"a quoted number": {`{"name":"Acme","token_budget":"5000000"}`, "not one number"},
		"a list":          {`{"name":"Acme","token_budget":[1,2]}`, "must be a mapping"},
		"a seat's number": {`{"name":"Acme","roles":[{"name":"CEO","token_budget":10}]}`, "not one number"},
		"a window's word": {`{"name":"Acme","token_budget":{"day":"many"}}`, "token_budget.day must be a whole number"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := config.DecodeCompany([]byte(tc.doc))
			if err == nil {
				t.Fatalf("%s decoded", tc.doc)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("refusal = %q, want it to say %q", err, tc.says)
			}
		})
	}

	got, err := config.DecodeSettings([]byte(
		`{"name":"Acme","token_budget":{"month":40000000,"fortnight":3}}`))
	if err != nil {
		t.Fatalf("a window a newer build wrote failed the revision: %v", err)
	}
	if want := (org.TokenCeilings{period.Month: 40000000}); !maps.Equal(got.TokenBudget.Ceilings(), want) {
		t.Errorf("token_budget = %v, want %v", got.TokenBudget.Ceilings(), want)
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

// A CHART KEY IS SPELT THE SAME IN BOTH FORMS.
//
// The chart keys are derived from the YAML tags, because that is what an
// operator authors and what the config door reads — but [config.DecodeSettings]
// looks for them in a STORED revision, which is JSON. A chart field whose JSON
// name differed from its YAML one would be a key that refusal never finds, and
// the revision carrying it would decode with its chart silently dropped.
func TestEveryChartKeyIsSpeltTheSameInTheStoredForm(t *testing.T) {
	t.Parallel()

	chart := config.ChartKeys()
	company := reflect.TypeFor[config.Company]()
	for i := range company.NumField() {
		field := company.Field(i)
		yamlName, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if !slices.Contains(chart, yamlName) {
			continue
		}
		jsonName, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if jsonName != yamlName {
			t.Errorf("config.Company.%s is %q in YAML and %q in JSON — "+
				"a stored revision carrying it would not be refused",
				field.Name, yamlName, jsonName)
		}
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
	if settings.Timezone != company.Timezone || settings.Timezone == "" {
		t.Errorf("timezone = %q, want %q — the company's one clock, which "+
			"every day boundary the engine cuts is on",
			settings.Timezone, company.Timezone)
	}
	if got, want := settings.TokenBudget.Ceilings(), company.TokenBudget.Ceilings(); len(want) == 0 ||
		!maps.Equal(got, want) {
		t.Errorf("token budget = %v, want %v", got, want)
	}
	// A COPY, NOT A VIEW: the ceilings are pointers, and an edit of the
	// settings an import stores must not move the file it was divided from.
	for _, ceiling := range []**int{
		&settings.TokenBudget.Day, &settings.TokenBudget.Week, &settings.TokenBudget.Month,
	} {
		if *ceiling != nil {
			**ceiling++
		}
	}
	if maps.Equal(settings.TokenBudget.Ceilings(), company.TokenBudget.Ceilings()) {
		t.Error("the settings share their token budget's ceilings with the file " +
			"they were divided from")
	}

	// NIL IN, NIL OUT, because a caller that held no company must not be
	// handed a settings document full of zero values it would then store.
	if config.SettingsOf(nil) != nil {
		t.Error("a nil company produced a settings document, which a caller " +
			"would store as a company with no name and no providers")
	}
}
