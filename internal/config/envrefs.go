package config

import (
	"cmp"
	"reflect"
	"slices"
	"strings"

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
// caller passes: the walk is shape-agnostic, but a path derived from a map or
// a yaml.Node names Go's own field structure rather than the operator's
// document.
func References(payload any) []Reference {
	var out []Reference
	walkStrings(reflect.ValueOf(payload), nil, func(path Path, s string) {
		for _, name := range envref.Names(s) {
			out = append(out, Reference{Path: path.String(), Name: name})
		}
	})
	slices.SortFunc(out, func(a, b Reference) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// VariablesRead is every variable the company reads, paired with the field
// that reads it: every ${VAR} its document names ([References]), and every
// variable it reads WITHOUT naming it — a credential field left empty, which
// the engine fills from a conventional variable instead.
//
// The second half is what the Secrets screen's "what breaks if I remove this"
// needs and a walk of the document cannot see. `crewlet llm login KEY
// -capture-token` stores CLAUDE_CODE_OAUTH_TOKEN and the entry signs in with
// it while naming nothing, so an index of the names written called it read by
// nothing, and deleting it signed the entry out. Reported at the field that
// falls back — `providers.llm.default.auth.token` — because that is where an
// operator would write the reference instead.
func (c *Company) VariablesRead() []Reference {
	if c == nil {
		return References(nil)
	}
	out := References(c)
	for key, entry := range c.Providers.LLM {
		out = append(out, fallbackReads(key, entry)...)
	}
	if e := c.Providers.Embeddings; e != nil && strings.TrimSpace(e.APIKey) == "" {
		out = append(out, Reference{Path: "providers.embeddings.api_key", Name: EmbeddingKeyVar})
	}
	slices.SortFunc(out, func(a, b Reference) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// LLMEntryReads is every variable one providers.llm entry reads, each once,
// sorted: each ${VAR} the entry names, and each it signs in from without
// naming one. Nil for a key the company does not declare.
//
// THE ENTRY'S PART OF [Company.VariablesRead], by the same rule, for a caller
// that builds one entry and must read nothing of another's: `crewlet llm
// login work` asks a running node for exactly these, each a reveal logged by
// name. Taken from the entry itself rather than by matching the paths
// VariablesRead reports, because a key may hold a dot — `gpt-5` beside
// `gpt-5.1` — and a path prefix cannot tell one entry from the other.
func (c *Company) LLMEntryReads(key string) []string {
	if c == nil {
		return nil
	}
	entry, ok := c.Providers.LLM[key]
	if !ok {
		return nil
	}
	var names []string
	for _, ref := range append(References(entry), fallbackReads(key, entry)...) {
		names = append(names, ref.Name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// fallbackReads is every variable an entry reads without naming it: a
// credential field left empty, filled from a conventional variable instead
// ([LLMProvider.CredentialSources]). Reported at the field that falls back,
// because that is where an operator would write the reference instead.
func fallbackReads(key string, entry LLMProvider) []Reference {
	var out []Reference
	sources := entry.CredentialSources(key)
	for field, source := range map[string]CredentialSource{
		"auth.token":             sources.Token,
		"auth.api_key":           sources.APIKey,
		"auth.credential_bundle": sources.Bundle,
	} {
		if source.Value == "" && source.Fallback != "" {
			out = append(out, Reference{Path: at(entryPath(key), field).String(), Name: source.Fallback})
		}
	}
	return out
}

// walkStrings visits every string reachable from v, with the JSON path that
// reaches it.
//
// It is the READ-ONLY half of [mapStrings], which is the ONE reachability walk,
// because Tier A's normalization has to agree with the reference index about
// which strings a payload holds: a string the trim cannot see keeps the drift
// [Bootstrap.normalize] describes, exactly as a string this cannot see is a
// ${VAR} nothing reports.
func walkStrings(v reflect.Value, path Path, visit func(path Path, s string)) {
	mapStrings(v, path, func(path Path, s string) string {
		visit(path, s)
		return s // read-only: an unchanged string writes nothing back
	})
}
