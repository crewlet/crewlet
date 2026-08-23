package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/org"
)

// Toggle is a boolean a config may leave UNSET, where unset is a third
// answer rather than false.
//
// It is the org model's type, aliased rather than redeclared: a config's
// learning_enabled and a seat's learning_enabled are the same value moving
// between layers, and two structurally identical types would need a
// conversion whose only job is to lose that.
//
// Every field that uses it would be a bug as a plain bool. An unset
// learning_enabled means "inherit the system setting" while false means
// "opt this seat out"; extension_enabled and the schedule toggles default
// to TRUE, so a bare bool would silently disable everything an operator did
// not annotate.
type Toggle = org.Toggle

// ProviderKeys is an ordered LLM provider fallback chain, written as a bare
// string for the ordinary one-provider case and as a list for a chain.
//
// Also the org model's type. The scalar form is not a special case
// downstream — it is a chain of one — so nothing in the engine holds an
// `any` for this field or learns which shape the operator wrote.
type ProviderKeys = org.ProviderKeys

// PhaseLLM is the `llm:` field of a seat, which accepts three shapes:
//
//	llm: fast                       # one provider for every phase
//	llm: [fast, backup]             # a fallback chain for every phase
//	llm: {default: fast, plan: big} # a chain per phase
//
// The mapping form exists because the phases have genuinely different
// shapes of work — planning wants a strong model, the extension judge wants
// a cheap fast one — and splitting them by hand across seven flat fields is
// how a config ends up with six of them agreeing and one stale.
//
// It decodes to ONE type rather than an `any`, so no consumer type-switches
// on what the operator happened to write. A phase left unset here falls
// back to Default; a flat llm_<phase> field on the seat wins over both.
type PhaseLLM struct {
	Default   ProviderKeys `yaml:"default,omitempty" json:"default,omitzero"`
	Plan      ProviderKeys `yaml:"plan,omitempty" json:"plan,omitzero"`
	Execute   ProviderKeys `yaml:"execute,omitempty" json:"execute,omitzero"`
	Review    ProviderKeys `yaml:"review,omitempty" json:"review,omitzero"`
	Subagent  ProviderKeys `yaml:"subagent,omitempty" json:"subagent,omitzero"`
	Auxiliary ProviderKeys `yaml:"auxiliary,omitempty" json:"auxiliary,omitzero"`
	Judge     ProviderKeys `yaml:"judge,omitempty" json:"judge,omitzero"`
	Sandbox   ProviderKeys `yaml:"sandbox,omitempty" json:"sandbox,omitzero"`
}

// phaseLLMFields decodes the mapping form without re-entering PhaseLLM's
// own unmarshaler. A distinct type is the standard way to break that
// recursion, and it keeps the field list in exactly one place.
type phaseLLMFields PhaseLLM

// A custom unmarshaler is matched structurally by the yaml package, so a
// signature that drifts by one character stops being called with no error
// anywhere. This turns that into a compile failure.
var _ yaml.Unmarshaler = (*PhaseLLM)(nil)

// UnmarshalYAML accepts a scalar, a sequence, or the per-phase mapping.
func (p *PhaseLLM) UnmarshalYAML(node *yaml.Node) error {
	// An anchored value (`llm: *shared`) arrives as an alias; the shape
	// that matters is what it points at.
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	switch {
	case node.Tag == "!!null":
		*p = PhaseLLM{}
		return nil
	case node.Kind == yaml.ScalarNode, node.Kind == yaml.SequenceNode:
		var keys ProviderKeys
		if err := keys.UnmarshalYAML(node); err != nil {
			return err
		}
		*p = PhaseLLM{Default: keys}
		return nil
	case node.Kind == yaml.MappingNode:
		var fields phaseLLMFields
		// Back through the strict decoder: a mapping is the one shape a
		// typo can hide in, and `llm: {pln: big}` would otherwise point
		// every phase at the default while looking configured.
		if err := decodeKnown(node, &fields); err != nil {
			return err
		}
		*p = PhaseLLM(fields)
		return nil
	default:
		return fmt.Errorf("line %d: %w: llm must be a provider key, a list of "+
			"keys, or a per-phase mapping", node.Line, ErrShape)
	}
}

// UnmarshalJSON accepts the same three shapes, so a config that round-trips
// through the store keeps the form its author wrote.
func (p *PhaseLLM) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*p = PhaseLLM{}
		return nil
	}
	if trimmed[0] == '{' {
		var fields phaseLLMFields
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&fields); err != nil {
			return err
		}
		*p = PhaseLLM(fields)
		return nil
	}
	var keys ProviderKeys
	if err := keys.UnmarshalJSON(trimmed); err != nil {
		return err
	}
	*p = PhaseLLM{Default: keys}
	return nil
}

// MarshalYAML writes the scalar form back when only Default is set, so
// re-serialising a hand-written config does not rewrite every `llm:` line
// into a mapping.
func (p PhaseLLM) MarshalYAML() (any, error) {
	if p.IsZero() {
		return nil, nil
	}
	if p.onlyDefault() {
		return p.Default, nil
	}
	return phaseLLMFields(p), nil
}

// MarshalJSON mirrors MarshalYAML.
func (p PhaseLLM) MarshalJSON() ([]byte, error) {
	if p.IsZero() {
		return []byte("null"), nil
	}
	if p.onlyDefault() {
		return json.Marshal(p.Default)
	}
	return json.Marshal(phaseLLMFields(p))
}

// phases lists every chain in the order a per-phase mapping declares them,
// so IsZero, the fallback resolution and the schema all read one list.
func (p *PhaseLLM) phases() []ProviderKeys {
	return []ProviderKeys{p.Default, p.Plan, p.Execute, p.Review, p.Subagent, p.Auxiliary, p.Judge, p.Sandbox}
}

// IsZero reports that nothing was configured, which is what lets both
// encoders drop the field rather than write an empty mapping.
func (p PhaseLLM) IsZero() bool {
	for _, chain := range p.phases() {
		if len(chain) > 0 {
			return false
		}
	}
	return true
}

// onlyDefault reports the scalar-or-list form: a default and no per-phase
// override.
func (p PhaseLLM) onlyDefault() bool {
	for _, chain := range p.phases()[1:] {
		if len(chain) > 0 {
			return false
		}
	}
	return true
}
