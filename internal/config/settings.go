package config

import (
	"encoding/json"
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
// # Who reads one, and who deliberately does not
//
// EVERY PATH THAT APPLIES a revision, and there are two: the node's own boot,
// and the control plane's reconcile. Those are where a revision still carrying
// a chart must be refused — a node that applied one would run a company whose
// seats come from a document no other node reads.
//
// EVERYTHING ELSE STILL DECODES A [Company], and that is not an oversight.
// `crewlet config show`, `diff`, `export`, `GET /config`, and the prior a
// config write restores its masks from or merges onto all exist to LOOK at a
// revision — very much including the one this build refuses to run. A reader
// that refused it would lock an operator out of the revision they need in
// order to repair it, which is the one moment the read is load bearing.
// [TestEveryStoredDecodeSiteIsDeclared] is what holds each site to the reader
// it chose.
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

// DecodeSettings reads a STORED revision as settings.
//
// # It refuses a revision that still carries a chart
//
// And the refusal names the procedure rather than the field. Silently dropping
// `roles:` and `units:` would be the worst available outcome: the node would
// boot, serve, and run a company with no seats in it — every mailbox gone,
// every routing decision answering nobody — and nothing would say why, because
// from the engine's point of view the configuration decoded cleanly.
//
// # Everything else about it is [DecodeCompany]'s, and has to be
//
// This is the reader on the APPLY path — a node's boot and the control plane's
// reconcile — so it meets exactly the bytes [DecodeCompany] was written for,
// and each of that reader's three rules is load bearing here for the same
// reason it is there:
//
//   - JSON, not YAML. The stored form is what marshalling a [Company]
//     produced, which carries fields the authored form does not
//     (`providers.llm_order` is the declaration order of a Go map, recoverable
//     only while the YAML document exists). A YAML reader rejects it.
//   - LENIENT about a field it does not know, where the authored parser fails
//     closed. An unrecognised key in a stored revision is a PEER running a
//     newer build, and refusing it makes a mixed-version fleet an outage in
//     the older direction. Strictness belongs at the import, which is where a
//     person's document arrives.
//   - Onto the DEFAULTS rather than a zero value, so a field the payload omits
//     lands where the authored path would have put it. A revision written
//     before a setting existed would otherwise come back with that setting at
//     zero, which for a timeout or a round cap is a different company.
//
// It holds the revision to no rule beyond the chart: the caller validates, and
// which rules it holds it to is the caller's question — see [DecodeCompany].
func DecodeSettings(payload []byte) (*Settings, error) {
	if err := refuseChart(payload); err != nil {
		return nil, err
	}
	defaults := DefaultCompany()
	out := SettingsOf(&defaults)
	if err := json.Unmarshal(payload, out); err != nil {
		return nil, &Fault{Kind: ErrShape, Detail: err.Error()}
	}
	return out, nil
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
		"A revision written before the org chart moved onto its own log still "+
		"carries the chart inside it, and this build reads the two "+
		"separately. The chart is NOT dropped and this revision is NOT run: "+
		"until it is replaced, this node keeps serving whatever it already "+
		"applied.\n"+
		"To replace it, store the settings half from the company file — "+
		"`crewlet config import <company.yaml>`, which writes the settings "+
		"and leaves the chart alone. A deployment whose chart is still empty "+
		"gets one from `crewlet run -company <company.yaml>`, which seeds it "+
		"once",
		ErrRevisionCarriesAChart, strings.Join(held, " and "))
}

// ChartKeys is every top-level key the org chart owns, and therefore every
// key a SETTINGS document does not.
//
// EXPORTED because the config API's write door needs the same answer: a `PUT`
// or a `PATCH` carrying one of these is asking that surface to write something
// it no longer owns, and it refuses by name. Two lists would drift, and the
// drift would be a key refused at one door and silently dropped at the other.
func ChartKeys() []string { return chartKeys() }

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

// Company is this settings document as the type every consumer still takes.
//
// # Why the inverse of [SettingsOf] exists at all
//
// Nothing below this layer was re-typed when the chart left. An epoch is built
// from a [Company], a validator reads one, a provider chain is constructed from
// one — and every one of those reads the eighteen fields that STAYED. Re-typing
// them to take a [Settings] would be a rename across the engine buying nothing:
// the two types carry the same values, and the difference between them is which
// half of a stored revision they came from.
//
// # The chart half is EMPTY, and that is the whole point
//
// A company built here has no units and no seats. It is not a company with an
// empty org chart — it is the half of one that a stored revision holds, and the
// other half is the log. The [org.Organization] a node runs is derived from the
// chart's rows and composed with these settings by the engine's view; anything
// reading `Roles` or `Units` off this value is reading the wrong half and gets
// nothing rather than something stale, which is the failure that reports
// itself.
func (s *Settings) Company() *Company {
	if s == nil {
		return nil
	}
	return &Company{
		Name: s.Name, Mission: s.Mission, Vision: s.Vision,
		Policies:       append([]string(nil), s.Policies...),
		Integrations:   s.Integrations,
		Tracker:        s.Tracker,
		Knowledge:      s.Knowledge,
		SkillVariables: s.SkillVariables,
		Providers:      s.Providers,
		TurnEngine:     s.TurnEngine,
		Learning:       s.Learning,
		Scheduling:     s.Scheduling,
		MCPServers:     append([]MCPServer(nil), s.MCPServers...),
		TokenBudget:    s.TokenBudget,

		NotificationRateLimit:             s.NotificationRateLimit,
		NotificationCoalesceWindowSeconds: s.NotificationCoalesceWindowSeconds,
		NotificationCoalesceMaxBatch:      s.NotificationCoalesceMaxBatch,

		Workers: s.Workers,
	}
}

// DecodeSettingsAsCompany is [DecodeSettings] for the callers that go on to
// build something from a [Company].
//
// ONE FUNCTION rather than the two lines at each apply site, because the pair
// is a rule and not a convenience: a site that decoded a revision as a
// [Company] directly would accept the chart this one refuses, and the node
// would serve a revision no other node in the fleet can read.
func DecodeSettingsAsCompany(data []byte) (*Company, error) {
	settings, err := DecodeSettings(data)
	if err != nil {
		return nil, err
	}
	return settings.Company(), nil
}
