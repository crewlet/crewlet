package mcp

import (
	"encoding/json"
	"fmt"
)

// Hint is one MCP behavioural hint, and it has THREE values, not two.
//
// "The server did not advertise this" and "the server said no" are different
// facts, and every classifier below depends on telling them apart. A bool
// cannot hold that, which is exactly how the distinction gets lost: see the
// note on annotationsFromSDK (probe.go) about the SDK's own struct, where two
// of the four hint fields are plain bools, so an absent hint is
// indistinguishable there from an explicit false.
//
// The zero value is Unknown, so Annotations{} is an all-unknown set — which is
// the correct reading of a server that advertises nothing.
type Hint uint8

const (
	// Unknown means the hint was not advertised. Never coerce it to No.
	Unknown Hint = iota
	// Yes means the server (or an operator override) asserted the hint.
	Yes
	// No means the server (or an operator override) denied the hint.
	No
)

// String renders the hint for logs and test failures.
func (h Hint) String() string {
	switch h {
	case Yes:
		return "yes"
	case No:
		return "no"
	default:
		return "unknown"
	}
}

// known reports whether the hint carries an assertion either way.
func (h Hint) known() bool { return h == Yes || h == No }

// hintOf converts a decoded JSON/YAML value into a Hint. Anything that is not
// a bool is treated as not advertised: a server that sends "readOnlyHint":
// "true" has not told us anything we are entitled to act on.
func hintOf(v any) Hint {
	b, ok := v.(bool)
	if !ok {
		return Unknown
	}
	if b {
		return Yes
	}
	return No
}

// Annotations are the behavioural hints an MCP server advertises for one tool,
// plus whatever an operator overrode on top.
//
// The engine reads these instead of matching tool NAMES. A hardcoded list of
// slack_* / jira_* / confluence_* names couples the engine to one tool stack
// and silently fails for every other one; a hint is something any compliant
// server can advertise and any operator can supply. See
// docs/concepts/tool-capabilities.md.
type Annotations struct {
	// Title is a human-readable name. "" means not advertised.
	Title string
	// ReadOnly is readOnlyHint — the tool does not modify any state.
	ReadOnly Hint
	// Destructive is destructiveHint — the tool may perform irreversible
	// updates.
	Destructive Hint
	// Idempotent is idempotentHint — repeat calls have no additional effect.
	Idempotent Hint
	// OpenWorld is openWorldHint — the tool interacts with entities outside
	// the local system: the network, external services, surfaces a human
	// reads.
	OpenWorld Hint
}

// AnnotationsFrom builds a set from a decoded object.
//
// It accepts the MCP spec's camelCase wire keys (readOnlyHint, …) AND the
// snake_case spelling operator config uses (read_only, …), because both reach
// this function: the first from a server, the second from a YAML override
// block. A key that is absent, null, or not a bool leaves its hint Unknown.
func AnnotationsFrom(raw map[string]any) Annotations {
	pick := func(keys ...string) Hint {
		for _, k := range keys {
			if v, ok := raw[k]; ok && v != nil {
				if h := hintOf(v); h != Unknown {
					return h
				}
			}
		}
		return Unknown
	}
	title, _ := raw["title"].(string)
	return Annotations{
		Title:       title,
		ReadOnly:    pick("readOnlyHint", "read_only"),
		Destructive: pick("destructiveHint", "destructive"),
		Idempotent:  pick("idempotentHint", "idempotent"),
		OpenWorld:   pick("openWorldHint", "open_world"),
	}
}

// annotationsFromJSON decodes the raw annotations object exactly as the server
// serialized it. This is the ONLY lossless path — see annotationsFromSDK.
func annotationsFromJSON(raw json.RawMessage) (Annotations, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return Annotations{}, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Annotations{}, fmt.Errorf("decode tool annotations: %w", err)
	}
	return AnnotationsFrom(obj), nil
}

// Merge returns a copy with every SET field of override applied.
//
// This is the escape hatch for servers that under-annotate: an operator states
// the hints in config and they win over whatever the server advertised. An
// Unknown field in the override changes nothing — an operator who annotated
// one hint has not thereby denied the other three.
func (a Annotations) Merge(override Annotations) Annotations {
	out := a
	if override.Title != "" {
		out.Title = override.Title
	}
	if override.ReadOnly.known() {
		out.ReadOnly = override.ReadOnly
	}
	if override.Destructive.known() {
		out.Destructive = override.Destructive
	}
	if override.Idempotent.known() {
		out.Idempotent = override.Idempotent
	}
	if override.OpenWorld.known() {
		out.OpenWorld = override.OpenWorld
	}
	return out
}

// WritesToSharedSurface reports whether a tool writes somewhere outside the
// local engine — a channel, an issue, a pull request, anything a human reads.
//
// This is the question the SUB-AGENT GUARD asks: a sub-agent must not post
// under its parent's identity. It is deliberately conservative about unknowns
// in ONE direction:
//
//   - ReadOnly == Yes                          -> false (a pure read)
//   - Destructive == Yes                       -> true
//   - ReadOnly == No and OpenWorld != No       -> true
//   - anything else, including all-unknown     -> false
//
// # The trap
//
// FALSE FROM THIS FUNCTION IS NOT "READ-ONLY". It answers the opposite
// question from ReadOnly, and it answers false for a tool nobody annotated at
// all. That is right for its caller — the parent's explicit allowlist already
// curates the sub-agent surface, so the engine does not block what it cannot
// classify — and it is exactly wrong as a read-only fence, where an
// unclassifiable tool must be refused, not admitted. A fence that reads
// !WritesToSharedSurface(a) admits every under-annotated write tool in the
// company and looks like it is checking something. Use ReadOnlyProven.
func WritesToSharedSurface(a Annotations) bool {
	if a.ReadOnly == Yes {
		return false
	}
	if a.Destructive == Yes {
		return true
	}
	return a.ReadOnly == No && a.OpenWorld != No
}

// ReadOnlyProven reports whether the tool was ASSERTED read-only.
//
// It exists so a caller that needs a fence has one that fails closed: an
// unannotated tool is not proven read-only and this says so. It is not the
// negation of WritesToSharedSurface and the two disagree for every
// unannotated tool, which is the whole point.
func ReadOnlyProven(a Annotations) bool { return a.ReadOnly == Yes }
