package harnessprofile

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// THE TOOL NAMES A HARNESS CAN HAND ITS MODEL.
//
// A harness hands its model each of the engine's MCP tools under a prefix plus
// the tool's name: `mcp__crewlet__search` for Claude Code. The vendors' APIs
// refuse a name longer than [ModelToolNameMax] or holding anything but
// letters, digits, `_` and `-`, and a harness handed one fails the run or
// silently drops the tool. So the engine files a tool only under a name that
// fits beside the LONGEST prefix any shipped profile adds ([ToolNameMax]), and
// refuses the rest rather than cutting them short: two shortened names can
// collide. HERE because it is read off the profile table, so a longer prefix
// lowers the limit on its own; config holds `tool_prefix` to it, the bridge
// every tool it files.

// ModelToolNameMax is the most characters a model vendor's API takes in a tool
// name: 64, the bound OpenAI's and Google's function calling set, so a name
// that fits it reaches a model whichever vendor a harness calls.
const ModelToolNameMax = 64

// toolNameChars is the characters every one of those APIs takes in a tool name.
var toolNameChars = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ToolNameChars reports whether name is at least one character and only the
// characters a model's tool name may hold: letters, digits, `_` and `-`.
func ToolNameChars(name string) bool { return toolNameChars.MatchString(name) }

// bridgeToolName is the grammar of a name the engine's own server lists:
// runs of letters and digits joined by single `_`.
var bridgeToolName = regexp.MustCompile(`^[A-Za-z0-9]+(_[A-Za-z0-9]+)*$`)

// BridgeToolName reports whether the engine's own server (the bridge's
// `crewlet`) may list a tool under name: one NO OTHER SERVER'S TOOL CAN BE
// NAMED LIKE in a harness that joins server and tool with `__` or `-`. Another
// server spells `<prefix>crewlet<sep>x` only by a name opening `crewlet_`, and
// then the part after the bridge's prefix opens with `_` or holds `__` (or
// `-`), which this grammar never holds. One shorter than [ToolNameMax]: Pi
// and Kimi Code cut a longer name to 55 characters, `_` and a hash, exactly
// 64, which a 50-character name beside `mcp__crewlet__` would equal. Each
// runner's docs section names its grammar; one joining with a single `_`
// (Gemini CLI, OpenCode) is checked at run time.
func BridgeToolName(name string) bool {
	return len(name) < ToolNameMax() && bridgeToolName.MatchString(name)
}

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

// ModelToolPrefixes is every prefix a built-in profile puts in front of the
// engine's tool names for its model, longest first: the spellings a model may
// write when it names one of those tools in an argument.
func ModelToolPrefixes() []string { return slices.Clone(modelToolPrefixes()) }

var modelToolPrefixes = sync.OnceValue(func() []string {
	var out []string
	for _, goos := range Platforms {
		table, err := decodeTable(profilesYAML, goos)
		if err != nil {
			panic(fmt.Sprintf("harnessprofile: profiles.yaml: %v", err))
		}
		for _, p := range table {
			if prefix := p.ModelToolPrefix(); prefix != "" && !slices.Contains(out, prefix) {
				out = append(out, prefix)
			}
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		return cmp.Or(len(b)-len(a), strings.Compare(a, b))
	})
	return out
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
