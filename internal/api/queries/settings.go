// The company's settings that are not numbers, as the engine applies them.

package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/config"
)

// SettingGroup is which screen changes a setting in place.
type SettingGroup string

// The groups.
const (
	// SettingCompany is the company's own: its clock.
	SettingCompany SettingGroup = "company"
	// SettingCode is Gitlet's: the domain its commits are addressed in.
	SettingCode SettingGroup = "code"
	// SettingKnowledge is the knowledge base's: where its pages live and
	// how it is searched.
	SettingKnowledge SettingGroup = "knowledge"
	// SettingSignIn is the company's sign-in policy, answered on
	// `sign_in_policy`.
	SettingSignIn SettingGroup = "sign_in"
)

// SettingGroups are every group.
var SettingGroups = []SettingGroup{SettingCompany, SettingCode, SettingKnowledge, SettingSignIn}

// SettingKind is what a setting's value is, and so how a screen types it.
type SettingKind string

// The kinds.
const (
	// SettingText is a line of text: a domain name.
	SettingText SettingKind = "text"
	// SettingZone is an IANA time zone.
	SettingZone SettingKind = "zone"
	// SettingContainer is one container key: a space.
	SettingContainer SettingKind = "container"
	// SettingContainers is a list of container keys.
	SettingContainers SettingKind = "containers"
	// SettingSwitch is on or off.
	SettingSwitch SettingKind = "switch"
)

// SettingKinds are every kind.
var SettingKinds = []SettingKind{SettingText, SettingZone, SettingContainer, SettingContainers, SettingSwitch}

// SettingRow is one setting of the company document that is not a number:
// where the document sets it, the value the engine applies, and the engine's
// default for it. [LimitRow] is its numeric sibling.
//
// THE VALUE IS THE APPLIED ONE, its default applied, for [LimitRow]'s reason:
// a screen that kept its own copy of `HOME`, `seats.invalid` or UTC would
// drift from the engine the day either moved.
//
// SET IS KNOWABLE HERE, unlike on a limit: each of these is a pointer or an
// empty-is-unset field, so the applied company remembers whether the
// document wrote it. A screen says Default or Set from it, and a document that
// writes the default value says Set, which is true.
//
// The validator holds each value, and its refusal carries a person's words
// ([config.Fault.Said]): no rule of these is copied onto the row.
type SettingRow struct {
	// Key is the setting's path in the company document, as `PATCH /config`
	// names it.
	Key   string       `json:"key"`
	Group SettingGroup `json:"group"`
	Kind  SettingKind  `json:"kind"`
	// Value is what the engine applies: a string for text, a zone or a
	// container, a list of strings for containers, a bool for a switch.
	Value any `json:"value"`
	// Default is what it applies where the document writes nothing. Where
	// the engine derives it from the rest of the document (search by meaning,
	// from providers.embeddings), what it derives for this company.
	Default any `json:"default"`
	// Set is whether the company document writes Key.
	Set bool `json:"set"`
	// Off is whether Value switches the setting off: an empty skills
	// container is no tool skills.
	Off bool `json:"off"`
	// HasOff is whether the setting has an Off of its own, an empty value
	// that is a setting distinct from the default (`skills_container: ""`),
	// so a screen offers it exactly where this is true.
	HasOff bool `json:"has_off"`
}

// SettingsAnswer is what `settings` answers.
type SettingsAnswer struct {
	Settings []SettingRow `json:"settings"`
}

// setting declares one row: whether it applies to a company, and how to read
// it off one.
type setting struct {
	key     string
	group   SettingGroup
	kind    SettingKind
	hasOff  bool
	applies func(*config.Company) bool
	// read is the applied value and whether the document writes the key.
	read func(*config.Company) (value any, set bool)
	// unset removes the key from a copy of a company, which read then
	// answers the default of.
	unset func(*config.Company)
}

// always applies a setting to every company.
func always(*config.Company) bool { return true }

// hostsCode applies a setting to a company Gitlet hosts code for: the
// validator refuses a code setting on any other.
func hostsCode(c *config.Company) bool { return c.Code.Backend == config.CodeGitlet }

// hasKnowledge applies a setting to a company that runs a knowledge base.
func hasKnowledge(c *config.Company) bool {
	return c.KnowledgeBackendFor() != config.KnowledgeNone
}

// settings is every row the answer reports, in the order a screen draws them.
//
// ONE TABLE, each row read through one getter for both the applied company
// and its default, so a row's value and its default can never be read from
// two different fields ([limits] says the same of the numbers).
var settings = []setting{
	{key: "timezone", group: SettingCompany, kind: SettingZone, applies: always,
		read: func(c *config.Company) (any, bool) {
			return c.Location().String(), c.Timezone != ""
		},
		unset: func(c *config.Company) { c.Timezone = "" }},
	{key: "code.identity_domain", group: SettingCode, kind: SettingText, applies: hostsCode,
		read: func(c *config.Company) (any, bool) {
			return c.Code.IdentityDomainResolved(), c.Code.IdentityDomain != ""
		},
		unset: func(c *config.Company) { c.Code.IdentityDomain = "" }},
	{key: "knowledge.root_space", group: SettingKnowledge, kind: SettingContainer,
		hasOff: true, applies: hasKnowledge,
		read: func(c *config.Company) (any, bool) {
			return c.RootSpaceKey(), c.Knowledge.RootSpace != nil
		},
		unset: func(c *config.Company) { c.Knowledge.RootSpace = nil }},
	{key: "knowledge.skills_container", group: SettingKnowledge, kind: SettingContainer,
		hasOff: true, applies: hasKnowledge,
		read: func(c *config.Company) (any, bool) {
			return c.SkillsContainerKey(), c.Knowledge.SkillsContainer != nil
		},
		unset: func(c *config.Company) { c.Knowledge.SkillsContainer = nil }},
	// EMPTY IS EVERY SPACE, the default, never Off: an unscoped search.
	{key: "knowledge.scope", group: SettingKnowledge, kind: SettingContainers, applies: hasKnowledge,
		read: func(c *config.Company) (any, bool) {
			scope := append([]string{}, c.Knowledge.KnowledgeScope...)
			return scope, len(scope) > 0
		},
		unset: func(c *config.Company) { c.Knowledge.KnowledgeScope = nil }},
	{key: "knowledge.vectors", group: SettingKnowledge, kind: SettingSwitch, applies: hasKnowledge,
		read: func(c *config.Company) (any, bool) {
			return c.VectorsEnabled(), c.Knowledge.Vectors != nil
		},
		unset: func(c *config.Company) { c.Knowledge.Vectors = nil }},
}

// settingsAnswer is every row that applies to c, beside its defaults.
func settingsAnswer(c *config.Company) SettingsAnswer {
	out := make([]SettingRow, 0, len(settings))
	for _, s := range settings {
		if !s.applies(c) {
			continue
		}
		value, set := s.read(c)
		// A COPY WITH THE KEY UNWRITTEN, so a default the engine derives
		// from the rest of the document is derived from this company's
		// (vectors follow its embeddings provider). Shallow is enough: each
		// unset assigns a field of a value struct, never through a pointer.
		d := *c
		s.unset(&d)
		def, _ := s.read(&d)
		out = append(out, SettingRow{
			Key: s.key, Group: s.group, Kind: s.kind,
			Value: value, Default: def, Set: set,
			Off:    s.hasOff && value == "",
			HasOff: s.hasOff,
		})
	}
	return SettingsAnswer{Settings: out}
}

// companySettings answers the settings of the company this node runs, and
// none on a node running no company.
func (s Sources) companySettings(context.Context, Params) (any, error) {
	company, _ := s.Company()
	if company == nil {
		return SettingsAnswer{Settings: []SettingRow{}}, nil
	}
	return settingsAnswer(company), nil
}
