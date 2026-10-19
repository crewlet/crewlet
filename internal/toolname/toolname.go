// Package toolname declares, once, the names of the engine's own tools that a
// package must SPELL without being able to import the package that implements
// the tool.
//
// # Why a leaf
//
// A tool's name is its owner's: the runner implements the discovery pair, and
// a package that can import the runner reads the name off it. But most of the
// packages that must name these tools sit BELOW their owner. The skill guard
// exempts the discovery pair from every trigger and the runner imports the
// skill guard; the worker deny list keeps a child off its parent's pair and
// the runner imports the worker package; the prompts that tell a model to
// call them import only the org model and the provenance fence; and the
// delivery remedy in the turn loop imports neither. Each of them spelled the
// names again, as a literal, with nothing comparing the copies — so a rename
// would have stopped exempting the pair from the skill guard, let a worker
// inherit the parent's `activate_tool` (the one escalation its deny list
// exists to stop), and told every model to call tools that no longer exist,
// all of it compiling.
//
// So the name lives here, importing nothing, and every package above or below
// the owner reads it from one place. A name EVERY reader can import stays
// with its owner; a name lands here only when some package that must spell it
// cannot import the package that owns it.
//
// # Held by a walk
//
// This package's suite reads every constant declared here off its source and
// walks every non-test Go file in the module: a string literal anywhere else
// that carries one of these names, as a word of its own, fails the build. A
// prompt that names a tool builds its text from the constant, so a new copy
// of a name is caught the day it is written rather than the day the name
// changes.
package toolname

// The discovery pair: the two meta-tools the executor reaches MCP tools
// through, since it is shown a server's name rather than its catalogue.
const (
	// ListMCPServerTools lists the tools one MCP server publishes.
	ListMCPServerTools = "list_mcp_server_tools"
	// Activate promotes a tool from the role's catalogue onto the next round.
	Activate = "activate_tool"
)

// Discovery is the discovery pair, in a slice of its own: a caller may keep or
// change what it is handed without reaching any other caller's copy.
func Discovery() []string {
	return []string{Activate, ListMCPServerTools}
}
