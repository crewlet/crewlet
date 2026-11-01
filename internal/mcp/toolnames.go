package mcp

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// WHICH OF A SERVER'S TOOLS ARE NOT OFFERED UNDER THEIR NAME, AND WHY.
//
// A tool is filed under its server's `tool_prefix` and its own name, run
// together, in one namespace with the engine's built-ins and every other
// server's tools, and every coding CLI puts a prefix of its own in front of
// that before its model sees it. Three things keep a name from reaching a
// model, and each is decided where the engine can see it:
//
//   - a name holding characters no model's tool name can, or one too long
//     to fit beside the longest CLI prefix ([harnessprofile.ToolNameMax]), is
//     REFUSED here, when the bridge builds the server's tools from a listing:
//     it is never filed, so it is neither offered nor callable, and never
//     shadows a tool that could be. Never cut short to fit: a shortened name
//     is a new name, and two of them can collide;
//   - a name a built-in already holds is refused where the engine files the
//     tool into a seat's surface, since built-ins are filed first
//     ([NameBuiltin]);
//   - a name another server's tool holds is SHADOWED: the first server by
//     name keeps it on a bridge ([Bridge.ShadowedTools]), and a shared
//     server's keeps it against a seat's own server in that seat's surface.
//
// They used to be log lines alone (`tool_name_collision`, `mcp_tool_refused`),
// which nobody reading the server's settings ever saw. Each is now a fact the
// heartbeat carries per server and the status answer per server across the
// fleet, worded by [NameRefusal.Sentence] and [ShadowSentence].

// NameRefusal is why a tool a server lists is not offered under its name.
type NameRefusal string

const (
	// NameCharacters is a name holding a character a model's tool name
	// cannot, which is anything but letters, digits, `_` and `-`.
	NameCharacters NameRefusal = "characters"
	// NameTooLong is a name that, prefix included, is longer than
	// [harnessprofile.ToolNameMax].
	NameTooLong NameRefusal = "too_long"
	// NameBuiltin is a name a built-in tool holds, and built-ins are
	// filed first.
	NameBuiltin NameRefusal = "builtin"
)

// NameRefusals is every reason, for the gate holding the dashboard's copy.
var NameRefusals = []NameRefusal{NameCharacters, NameTooLong, NameBuiltin}

// Valid reports whether r is a reason this build sends.
func (r NameRefusal) Valid() bool { return slices.Contains(NameRefusals, r) }

// RefusedTool is one tool a server lists that is not offered, by the name it
// would have been filed under, and why.
type RefusedTool struct {
	Name   string
	Reason NameRefusal
}

// ShadowedTool is one tool a server lists that another server's tool of the
// same name is offered in place of, and that server's configured name.
type ShadowedTool struct {
	Name string
	By   string
}

// nameRefusal says why a tool cannot be filed under name, or "" when it can:
// its characters first, since a name holding one a model cannot read is not
// measured in characters a model counts.
func nameRefusal(name string) NameRefusal {
	switch {
	case !harnessprofile.ToolNameChars(name):
		return NameCharacters
	case len(name) > harnessprofile.ToolNameMax():
		return NameTooLong
	}
	return ""
}

// Sentence says why tool, filed with prefix (the server's `tool_prefix`, ""
// for none), is not offered, and what would offer it.
func (r NameRefusal) Sentence(tool, prefix string) string {
	switch r {
	case NameCharacters:
		if prefix != "" && !harnessprofile.ToolNameChars(prefix) {
			return fmt.Sprintf("%q is not offered: the server's tool_prefix %q holds characters "+
				"a model's tool name cannot. Use only letters, digits, _ and - in it", tool, prefix)
		}
		return fmt.Sprintf("%q is not offered: the server names it with characters a model's "+
			"tool name cannot hold, which is anything but letters, digits, _ and -", tool)
	case NameTooLong:
		most := harnessprofile.ToolNameMax()
		over := len(tool) - most
		switch {
		case prefix != "" && over < len(prefix):
			return fmt.Sprintf("%q is not offered: it is %d characters, and %d fit beside the "+
				"longest prefix a coding CLI gives the engine's tools. Shorten the tool_prefix "+
				"%q by %d to offer it", tool, len(tool), most, prefix, over)
		case prefix != "" && over == len(prefix):
			return fmt.Sprintf("%q is not offered: it is %d characters, and %d fit beside the "+
				"longest prefix a coding CLI gives the engine's tools. Take off the tool_prefix "+
				"%q to offer it", tool, len(tool), most, prefix)
		}
		return fmt.Sprintf("%q is not offered: it is %d characters, and %d fit beside the "+
			"longest prefix a coding CLI gives the engine's tools. The server's own name for "+
			"it is too long for any coding CLI to call", tool, len(tool), most)
	case NameBuiltin:
		return fmt.Sprintf("%q is not offered: a built-in tool has the name, and built-ins are "+
			"filed first. Give the server a tool_prefix to offer it under another name", tool)
	}
	return fmt.Sprintf("%q is not offered (%s)", tool, string(r))
}

// ShadowSentence says why tool is not offered from its server: by, another
// server, serves a tool of the same name, which is offered in its place.
func ShadowSentence(tool, by string) string {
	return fmt.Sprintf("%q is not offered: server %s serves a tool of the same name, which is "+
		"offered in its place. Give one of the two servers a tool_prefix to offer both", tool, by)
}

// sortRefused orders refusals by name, then reason.
func sortRefused(refused []RefusedTool) {
	slices.SortFunc(refused, func(a, b RefusedTool) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Reason, b.Reason))
	})
}

// RefusedTools is every tool the instance name's latest listing named that is
// not offered for its name ([NameCharacters], [NameTooLong]), sorted by name;
// nil when every one is, the instance has never listed, or the bridge does not
// hold it. A server that is down keeps what its last listing refused until a
// restart lists again, as its tools are kept.
//
// A built-in's name is not among them: the bridge knows no built-in, and the
// engine decides that refusal where it files a server's tools into a surface.
func (b *Bridge) RefusedTools(name string) []RefusedTool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.servers[name]
	if !ok {
		return nil
	}
	return slices.Clone(s.refused)
}

// ShadowedTools is every tool of the instance name that another instance on
// this bridge serves under the same name, and is offered in its place (the
// first by instance name, [Bridge.reindexLocked]), with the configured name
// of the server it belongs to, sorted by tool name. Nil where none is.
func (b *Bridge) ShadowedTools(name string) []ShadowedTool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.servers[name]
	if !ok {
		return nil
	}
	var out []ShadowedTool
	for _, t := range s.tools {
		if winner, live := b.tools[t.Name()]; live && winner != t {
			out = append(out, ShadowedTool{Name: t.Name(), By: ServerName(winner.Instance())})
		}
	}
	slices.SortFunc(out, func(a, b ShadowedTool) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.By, b.By))
	})
	return out
}
