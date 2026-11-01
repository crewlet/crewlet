package harnessprofile

import (
	"fmt"
	"regexp"
	"sync"
)

// THE TOOL NAMES A HARNESS CAN HAND ITS MODEL.
//
// Every tool a seat is offered reaches its harness as a tool of the
// engine's one MCP server, and the harness hands it to its model under a name of
// its own making: a prefix for the server and the tool's name after it,
// `mcp__crewlet__search` for Claude Code. The model vendors' function-calling
// APIs refuse a tool name longer than [ModelToolNameMax] or holding anything
// but letters, digits, `_` and `-`, and a harness handed one either fails the run
// or drops the tool where nobody sees it. So the engine files a tool only under
// a name that fits beside the LONGEST prefix any harness it ships a profile for
// puts in front of it ([ToolNameMax]), and refuses the rest with their reason
// rather than cutting them short: a shortened name is a new name, and two of
// them can collide.
//
// HERE because it is a fact about what each harness does with a name, read off the
// profile table: a profile added with a longer prefix lowers the limit on its
// own, and nothing else has to be told. Config holds a server's `tool_prefix`
// to it and the MCP bridge holds every tool it files to it.

// ModelToolNameMax is the most characters a model vendor's API takes in a tool
// name: 64, the bound OpenAI's and Google's function calling set, so a name
// that fits it reaches a model whichever vendor a harness calls.
const ModelToolNameMax = 64

// toolNameChars is the characters every one of those APIs takes in a tool name.
var toolNameChars = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ToolNameChars reports whether name is at least one character and only the
// characters a model's tool name may hold: letters, digits, `_` and `-`.
func ToolNameChars(name string) bool { return toolNameChars.MatchString(name) }

// ToolNameMax is the longest tool name the engine files: what the longest
// prefix a built-in harness puts in front of one of its tools leaves of
// [ModelToolNameMax]. 50 while Claude Code's `mcp__crewlet__` is the longest.
//
// THE BUILT-IN PROFILES ON EVERY PLATFORM, not the harnesses a company runs today:
// a tool is filed once for every phase of a seat, and a fallback chain or a
// new entry moves a phase to another harness without refiling anything, so a name
// is held to every harness the engine can run. And not an operator's `overrides`,
// which are per company: two nodes of one fleet file a server's tools under
// the same names whatever either company says about a harness.
func ToolNameMax() int { return toolNameMax() }

var toolNameMax = sync.OnceValue(func() int {
	var all []Profile
	for _, goos := range Platforms {
		table, err := decodeTable(profilesYAML, goos)
		if err != nil {
			// The table is embedded from this repository; builtins
			// panics on the same failure, for the same reason.
			panic(fmt.Sprintf("harnessprofile: profiles.yaml: %v", err))
		}
		for _, p := range table {
			all = append(all, p)
		}
	}
	return toolNameRoom(all)
})

// toolNameRoom is what the longest model prefix among profiles leaves of
// [ModelToolNameMax].
func toolNameRoom(profiles []Profile) int {
	longest := 0
	for i := range profiles {
		longest = max(longest, len(profiles[i].ModelToolPrefix()))
	}
	return ModelToolNameMax - longest
}

// ModelToolPrefix is what the harness puts in front of a tool of the engine's
// server in the name it hands its model, or "" for a profile that says
// nothing of the server's tools.
//
// [Requests.HostPrefix] where the harness's record names a call as its model does.
// A harness whose record keeps the namespace apart from the name
// ([Requests.HostNamespace]: Codex, Cursor Agent) hands its model the two
// joined, as Codex hands `mcp__crewlet__search`, so it is counted as the
// namespace and the two characters of the `__` Codex joins them with.
func (p *Profile) ModelToolPrefix() string {
	if p.Requests == nil {
		return ""
	}
	if p.Requests.HostPrefix != "" {
		return p.Requests.HostPrefix
	}
	if p.Requests.HostNamespace != "" {
		return p.Requests.HostNamespace + "__"
	}
	return ""
}
