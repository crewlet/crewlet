package config

import (
	"cmp"
	"maps"
	"reflect"
	"slices"

	"github.com/crewlet/crewlet/internal/envref"
)

// The ${VAR} references a config payload actually holds, and where it holds
// them.
//
// Answering "which variables does this config need" is a question three very
// different callers ask: an operator wants to know what to set before a boot
// fails on a missing one, the example suite wants to know that every
// reference a shipped config makes is one the docs explain, and the Secrets
// screen wants to know what BREAKS if a credential is removed. Only the last
// one needs the path, and it is the one that cannot be answered without it: a
// name on its own says a reference exists somewhere, which is not something
// an operator can act on before deleting a row.
//
// It is a REFLECTION WALK rather than a re-parse of the YAML, because the
// question is about the loaded config — the thing the engine will actually
// resolve — and a document walk would miss defaults the loader filled in and
// include keys it discarded.

// Reference is one ${VAR} a payload names, and the field that names it.
//
// Path is the JSON path an operator can find in their own document, the same
// spelling [Company.UnresolvedMasks] reports: dotted fields, `[i]` for a list
// element, the key itself for a map entry.
type Reference struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// References is every ${VAR} the payload names, paired with where it is
// named, ordered by path and then by name.
//
// A name appears once per field that references it, which is the whole point:
// one credential routinely has several readers — `role.integrations.slack.bot_token`
// and `role.mcp_env.slack.SLACK_MCP_XOXB_TOKEN` are two pointers at one row —
// and an operator about to delete that row needs to see both.
//
// Paths are meaningful for a decoded [Company], which is the shape every
// caller passes. The walk itself is shape-agnostic and will happily traverse
// the map a store row decodes to or a raw yaml.Node, but a path derived from
// those names Go's own field structure rather than the operator's document,
// so [ReferencedNames] is what those callers want.
func References(payload any) []Reference {
	var out []Reference
	walkStrings(reflect.ValueOf(payload), "", func(path, s string) {
		for _, name := range envref.Names(s) {
			out = append(out, Reference{Path: path, Name: name})
		}
	})
	slices.SortFunc(out, func(a, b Reference) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// ReferencedNames is every ${VAR} a payload mentions anywhere, sorted and
// de-duplicated.
//
// It walks the value rather than a serialised form so it works on a decoded
// [Company], on the map a store row decodes to, and on a raw yaml.Node
// alike — the three shapes a payload actually arrives in.
func ReferencedNames(payload any) []string {
	seen := map[string]struct{}{}
	walkStrings(reflect.ValueOf(payload), "", func(_, s string) {
		for _, name := range envref.Names(s) {
			seen[name] = struct{}{}
		}
	})
	out := slices.Sorted(maps.Keys(seen))
	return out
}

// walkStrings visits every string reachable from v, with the JSON path that
// reaches it.
//
// ONE WALK for both questions above. Two would be two chances to disagree
// about which fields are reachable, and the fingerprint going blind to a
// reference the reference index can see is a silent failure on the half that
// decides whether a config change is a change at all.
//
// It is the READ-ONLY half of [mapStrings], which is that one walk — the
// same argument reaches one field further now, because Tier A's
// normalization has to agree about reachability too: a string the trim
// cannot see keeps the drift [Bootstrap.normalize] describes, exactly as a
// string this cannot see is a ${VAR} nothing reports.
func walkStrings(v reflect.Value, path string, visit func(path, s string)) {
	mapStrings(v, path, func(path, s string) string {
		visit(path, s)
		return s // read-only: an unchanged string writes nothing back
	})
}
