package config

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// THE SETTINGS DOCUMENT: what a stored revision holds once the org chart has
// left it.
//
// # What changed, and what deliberately did not
//
// A company used to be ONE document with twenty top-level fields, of which two
// — `roles` and `units` — were the org chart. Those two are a log now, and the
// eighteen that are left are settings: which integrations this company runs,
// which models, how its turn engine behaves, what its scheduling defaults are.
//
// The difference is not tidiness. A settings document is edited by an operator
// a few times a month and read whole; a chart is edited per object by whoever
// is hiring, and every write contends with every other. Keeping them in one
// document put both on one revision counter — so adding a seat and rotating a
// token were the same kind of write, colliding on the same version, and a
// document that lost the race was refused in full.
//
// THE FILE KEEPS BOTH. [Company] is unchanged and still carries all twenty
// fields, because that is what an operator authors and what
// `crewlet config import` reads: a founder writes one file describing a
// company, and splitting the authoring surface would be a change to the
// product rather than to where the engine keeps things. The import is what
// separates them — the chart's objects to the log, the settings here.
//
// # Why this ships with no caller
//
// It is additive, on purpose, and for exactly one change: removing the chart
// from the stored decode path is what gives this a caller, and doing both at
// once would leave every commit in between unable to build. What ships here is
// the type, its decoder and the refusal a stored revision written before the
// split earns — so the change that re-points the decode path is a small one
// over a shape that is already tested.
type Settings struct {
	// Name is half of every seat's derived id, so it is effectively
	// permanent. It is here rather than on the chart because it names the
	// COMPANY rather than anything in the hierarchy — and because a seat's
	// id is derived from it, a rename is a migration rather than an edit.
	Name string `yaml:"name" json:"name"`

	Mission  string   `yaml:"mission,omitempty" json:"mission,omitempty"`
	Vision   string   `yaml:"vision,omitempty" json:"vision,omitempty"`
	Policies []string `yaml:"policies,omitempty" json:"policies,omitempty"`

	Integrations Integrations `yaml:"integrations,omitempty" json:"integrations"`
	Tracker      Tracker      `yaml:"tracker,omitempty" json:"tracker,omitempty"`
	Knowledge    Knowledge    `yaml:"knowledge,omitempty" json:"knowledge"`

	SkillVariables map[string]string `yaml:"skill_variables,omitempty" json:"skill_variables,omitempty"`

	Providers  Providers  `yaml:"providers,omitempty" json:"providers"`
	TurnEngine TurnEngine `yaml:"turn_engine,omitempty" json:"turn_engine"`
	Learning   Learning   `yaml:"learning,omitempty" json:"learning"`
	Scheduling Scheduling `yaml:"scheduling,omitempty" json:"scheduling"`

	MCPServers []MCPServer `yaml:"mcp_servers,omitempty" json:"mcp_servers,omitempty"`

	TokenBudget int `yaml:"token_budget,omitempty" json:"token_budget,omitempty"`

	NotificationRateLimit             int     `yaml:"notification_rate_limit,omitempty" json:"notification_rate_limit,omitempty"`
	NotificationCoalesceWindowSeconds float64 `yaml:"notification_coalesce_window_seconds,omitempty" json:"notification_coalesce_window_seconds,omitempty"`
	NotificationCoalesceMaxBatch      int     `yaml:"notification_coalesce_max_batch,omitempty" json:"notification_coalesce_max_batch,omitempty"`

	Workers map[string]Worker `yaml:"workers,omitempty" json:"workers,omitempty"`
}

// ErrRevisionCarriesAChart reports a stored revision written before the split.
//
// A SENTINEL because the caller's response is a PROCEDURE rather than a retry:
// the revision is valid, it is just the wrong shape for this build, and the
// operator has to import it once so its chart reaches the log. A caller that
// treated it as a decode failure would report a corrupt configuration for one
// that is merely older.
var ErrRevisionCarriesAChart = fmt.Errorf("config: this revision still carries an org chart")

// DecodeSettings reads a stored revision as settings.
//
// IT REFUSES A REVISION THAT STILL CARRIES A CHART, and the refusal names the
// procedure rather than the field. Silently dropping `roles:` and `units:`
// would be the worst available outcome: the node would boot, serve, and run a
// company with no seats in it — every mailbox gone, every routing decision
// answering nobody — and nothing would say why, because from the engine's
// point of view the configuration decoded cleanly.
//
// STRICT, like every other decode here: an unknown field fails rather than
// being ignored, so a typo in a setting is a refusal at the moment it is
// written instead of a value that silently never took effect.
func DecodeSettings(data []byte) (*Settings, error) {
	if err := refuseChart(data); err != nil {
		return nil, err
	}
	var out Settings
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&out); err != nil {
		return nil, fmt.Errorf("config: decode the settings: %w", err)
	}
	return &out, nil
}

// refuseChart reports a document that still holds the org chart's own keys.
//
// IT READS THE DOCUMENT rather than the decoded value, because a strict decode
// would already have failed on the unknown field — with a message about an
// unknown key, which sends an operator looking for a typo. This one runs first
// and says what actually happened.
func refuseChart(data []byte) error {
	var probe map[string]yaml.Node
	if err := yaml.Unmarshal(data, &probe); err != nil {
		// NOT AN ERROR HERE. A document this cannot parse is one the
		// decode below will refuse with a better message, and reporting
		// a malformed document as "it carries a chart" would be wrong in
		// the one direction that matters.
		return nil
	}
	var held []string
	for _, key := range chartKeys() {
		if _, carries := probe[key]; carries {
			held = append(held, key)
		}
	}
	if len(held) == 0 {
		return nil
	}
	return fmt.Errorf("%w: it holds %s.\n"+
		"A revision written before the org chart moved onto its own log "+
		"still carries the chart inside it, and this build reads the two "+
		"separately. It is NOT dropped: `crewlet config import` reads the "+
		"revision whole, writes its units and seats to the chart log, and "+
		"stores what is left as settings. Until that runs, this node keeps "+
		"serving the revision it already applied",
		ErrRevisionCarriesAChart, strings.Join(held, " and "))
}

// chartKeys is every top-level key the org chart owns.
//
// DERIVED FROM THE FILE TYPE rather than typed here, on [CompanyKeys]'s own
// reasoning and for the failure it records: a hand-written copy of a key list
// in this package drifted into counting a key that appears nowhere in the tree
// while omitting five that do, and a document with only `units:` was then
// reported undecidable — the commonest authoring shape there is.
func chartKeys() []string {
	company := reflect.TypeFor[Company]()
	settings := map[string]bool{}
	for _, name := range yamlNames(reflect.TypeFor[Settings]()) {
		settings[name] = true
	}
	var out []string
	for _, name := range yamlNames(company) {
		if !settings[name] {
			out = append(out, name)
		}
	}
	return out
}

// yamlNames is every top-level yaml key a struct declares.
func yamlNames(t reflect.Type) []string {
	out := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// SettingsOf is the settings half of an authored file.
//
// WHAT AN IMPORT STORES, and the other half is what it publishes to the chart
// log. It is a copy rather than a view, because the file it came from is what
// an operator holds and a settings value that aliased it would change under a
// second edit of the same document.
func SettingsOf(c *Company) *Settings {
	if c == nil {
		return nil
	}
	return &Settings{
		Name: c.Name, Mission: c.Mission, Vision: c.Vision,
		Policies:       append([]string(nil), c.Policies...),
		Integrations:   c.Integrations,
		Tracker:        c.Tracker,
		Knowledge:      c.Knowledge,
		SkillVariables: c.SkillVariables,
		Providers:      c.Providers,
		TurnEngine:     c.TurnEngine,
		Learning:       c.Learning,
		Scheduling:     c.Scheduling,
		MCPServers:     append([]MCPServer(nil), c.MCPServers...),
		TokenBudget:    c.TokenBudget,

		NotificationRateLimit:             c.NotificationRateLimit,
		NotificationCoalesceWindowSeconds: c.NotificationCoalesceWindowSeconds,
		NotificationCoalesceMaxBatch:      c.NotificationCoalesceMaxBatch,

		Workers: c.Workers,
	}
}
