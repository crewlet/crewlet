package org

import (
	"encoding/json"
	"fmt"
	"maps"

	"gopkg.in/yaml.v3"
)

// ProviderKeys is an ordered LLM provider fallback chain: the runtime tries
// each key in turn and moves on when the previous one raises a retryable
// error. One key is the ordinary case, so config authors write a bare
// string, and both forms decode here:
//
//	llm: claude-sonnet
//	llm: [claude-sonnet, claude-haiku, gpt-4o]
//
// The scalar form is not a special case downstream — it is a chain of one.
// Callers get a []string and never learn which shape the operator wrote,
// which is why nothing in the engine has to hold an `any` for this field.
//
// A nil chain means UNSET, and that is load-bearing for the per-phase
// fields: an unset llm_plan falls back to llm, while an explicitly empty
// one would have to mean "no provider at all". Config cannot express the
// second, so nil is the only reading.
type ProviderKeys []string

// Ensure both decoders are wired. A custom unmarshaler is matched
// structurally by the encoding libraries, so a signature that drifts by one
// character stops being called with no error anywhere — these assertions
// turn that into a compile failure.
var (
	_ yaml.Unmarshaler = (*ProviderKeys)(nil)
	_ json.Unmarshaler = (*ProviderKeys)(nil)
	_ yaml.Marshaler   = (ProviderKeys)(nil)
	_ json.Marshaler   = (ProviderKeys)(nil)
)

// UnmarshalYAML accepts a scalar or a sequence of scalars.
func (k *ProviderKeys) UnmarshalYAML(node *yaml.Node) error {
	// An anchored value (`llm: *shared_chain`) arrives as an alias; the
	// shape that matters is what it points at.
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	switch {
	case node.Tag == "!!null":
		*k = nil
		return nil
	case node.Kind == yaml.ScalarNode:
		var one string
		if err := node.Decode(&one); err != nil {
			return err
		}
		*k = compactKeys([]string{one})
		return nil
	case node.Kind == yaml.SequenceNode:
		var many []string
		if err := node.Decode(&many); err != nil {
			return err
		}
		*k = compactKeys(many)
		return nil
	default:
		return fmt.Errorf("line %d: %w", node.Line, ErrProviderKeysShape)
	}
}

// UnmarshalJSON accepts a string or an array of strings, so a config that
// round-trips through the API keeps the shape its author wrote.
func (k *ProviderKeys) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case nil:
		*k = nil
	case string:
		*k = compactKeys([]string{v})
	case []any:
		many := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return fmt.Errorf("%w: %T in list", ErrProviderKeysShape, item)
			}
			many = append(many, s)
		}
		*k = compactKeys(many)
	default:
		return fmt.Errorf("%w: %T", ErrProviderKeysShape, raw)
	}
	return nil
}

// MarshalYAML writes a one-key chain back as the bare scalar it was
// probably authored as, so re-serialising a hand-written config does not
// rewrite every llm: line into a list.
func (k ProviderKeys) MarshalYAML() (any, error) {
	switch len(k) {
	case 0:
		// Unset, and unset is not an empty chain: both encoders write null
		// so a value that round-trips reads back as unset rather than as
		// "explicitly no provider".
		return nil, nil
	case 1:
		return k[0], nil
	default:
		return []string(k), nil
	}
}

// MarshalJSON mirrors MarshalYAML: one key is a string, many are an array.
func (k ProviderKeys) MarshalJSON() ([]byte, error) {
	if len(k) == 1 {
		return json.Marshal(k[0])
	}
	return json.Marshal([]string(k))
}

// IsZero reports an unset chain, which is what makes `omitempty` (YAML) and
// `omitzero` (JSON) drop the field rather than emit a null.
func (k ProviderKeys) IsZero() bool { return len(k) == 0 }

// compactKeys drops empty entries. An empty provider key can never match a
// provider map, so keeping it would only push the decision to every reader:
// "unset" and "set to nothing" resolve identically, and one of them is
// harder to reason about.
func compactKeys(in []string) ProviderKeys {
	out := make(ProviderKeys, 0, len(in))
	for _, key := range in {
		if key != "" {
			out = append(out, key)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Toggle is a boolean a config may leave unset, where unset is a third
// answer rather than false.
//
// Three fields need it and each would be a bug as a plain bool: an unset
// learning_enabled means "inherit the system setting", while false means
// "opt this seat out"; schedule enabled and catchup default to TRUE, so a
// bare bool would silently disable every schedule an operator did not
// annotate. The zero Toggle is unset, so the zero Schedule is an enabled
// one — which is the reading its author intended.
//
// It is a struct rather than a *bool so no consumer can dereference a nil,
// and so that reading it forces a decision: [Toggle.Or] takes the default
// that applies when the operator said nothing.
type Toggle struct {
	value bool
	set   bool
}

// On returns a Toggle explicitly set to true.
func On() Toggle { return Toggle{value: true, set: true} }

// Off returns a Toggle explicitly set to false.
func Off() Toggle { return Toggle{set: true} }

// Or resolves the toggle, using def when the operator left it unset.
func (t Toggle) Or(def bool) bool {
	if !t.set {
		return def
	}
	return t.value
}

// IsSet reports whether the config said anything at all. Callers that only
// need the value should use Or; this is for the places where "the operator
// chose false" differs from "the operator was silent".
func (t Toggle) IsSet() bool { return t.set }

// IsZero reports an unset toggle. Both encoders consult it, so an unset
// toggle is omitted rather than written as false — which is what stops a
// config round trip from freezing today's default into the document.
func (t Toggle) IsZero() bool { return !t.set }

var (
	_ yaml.Unmarshaler = (*Toggle)(nil)
	_ json.Unmarshaler = (*Toggle)(nil)
	_ yaml.Marshaler   = Toggle{}
	_ json.Marshaler   = Toggle{}
)

// UnmarshalYAML reads a YAML boolean; an explicit null reads as unset.
func (t *Toggle) UnmarshalYAML(node *yaml.Node) error {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	if node.Tag == "!!null" {
		*t = Toggle{}
		return nil
	}
	var v bool
	if err := node.Decode(&v); err != nil {
		return err
	}
	*t = Toggle{value: v, set: true}
	return nil
}

// UnmarshalJSON reads a JSON boolean; null reads as unset.
func (t *Toggle) UnmarshalJSON(data []byte) error {
	var v *bool
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	if v == nil {
		*t = Toggle{}
		return nil
	}
	*t = Toggle{value: *v, set: true}
	return nil
}

// MarshalYAML writes the value, or null when unset.
func (t Toggle) MarshalYAML() (any, error) {
	if !t.set {
		return nil, nil
	}
	return t.value, nil
}

// MarshalJSON writes the value, or null when unset.
func (t Toggle) MarshalJSON() ([]byte, error) {
	if !t.set {
		return []byte("null"), nil
	}
	return json.Marshal(t.value)
}

// MCPEnv is per-agent overrides for MCP servers, keyed by server name and
// then by variable: environment variables for a stdio server, HTTP headers
// for an http one.
//
// This is how each seat authenticates as a distinct identity — its own Jira
// token, its own Slack bot token, its own GitHub authorization header —
// which is the whole reason a `shared: false` server is launched once per
// seat rather than once per company.
type MCPEnv map[string]map[string]string

// Clone returns a deep copy. The org model is mutated during normalisation
// and read concurrently afterwards, so inheritance never aliases a map two
// seats can both reach.
func (e MCPEnv) Clone() MCPEnv {
	if e == nil {
		return nil
	}
	out := make(MCPEnv, len(e))
	for server, vars := range e {
		out[server] = maps.Clone(vars)
	}
	return out
}

// WithDefaults layers base underneath e: every server in either appears in
// the result, and where both set a variable, e wins.
//
// This is unit-to-role inheritance, and the direction is the point — a unit
// declares the credentials its whole team shares, and a seat that needs its
// own overrides exactly the variables it names instead of restating the
// block. Merging is per VARIABLE, not per server: a role that overrides one
// header must not silently drop the token beside it.
func (e MCPEnv) WithDefaults(base MCPEnv) MCPEnv {
	if len(base) == 0 {
		return e
	}
	out := e.Clone()
	if out == nil {
		out = make(MCPEnv, len(base))
	}
	for server, baseVars := range base {
		merged := maps.Clone(baseVars)
		if merged == nil {
			merged = map[string]string{}
		}
		maps.Copy(merged, out[server])
		out[server] = merged
	}
	return out
}
