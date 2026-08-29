// Package codingagent is the detached-run plumbing every coding-agent runner
// shares.
//
// Both supported CLIs run headless in a box and follow the same shape: a
// background process redirects its output to a file and drops a done marker,
// Poll checks that marker, Collect reads and parses the file, and an ask
// overlay surfaces a mid-run clarification. This package owns that plumbing so
// each runner supplies only its CLI invocation and its output parser.
//
// It touches only the [sandbox.Sandbox] interface, so the in-process fake is a
// faithful substitute and every runner is testable without a real CLI.
package codingagent

import (
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// Paths is every artefact path for one box, derived from its home.
//
// ONE OBJECT rather than a bag of constants, and derived rather than fixed. A
// relocated result file paired with a default-home done marker polls forever,
// and a local backend runs many boxes on one filesystem — a shared
// /home/user/.crewlet would have every run reading its neighbour's marker.
type Paths struct{ Home string }

// PathsFor returns the artefact paths for a box.
func PathsFor(box sandbox.Sandbox) Paths {
	home := sandbox.DefaultHome
	if box != nil {
		if h := strings.TrimRight(box.Home(), "/"); h != "" {
			home = h
		}
	}
	return Paths{Home: home}
}

// WorkDir is the side directory for the detached artefacts.
//
// Outside the checkout, so a `git clean` in the brief cannot wipe them.
func (p Paths) WorkDir() string { return p.Home + "/.crewlet" }

// Result is where the agent's stdout is redirected.
func (p Paths) Result() string { return p.WorkDir() + "/result.json" }

// Err is where its stderr goes — the transcript's fallback source.
func (p Paths) Err() string { return p.WorkDir() + "/err.log" }

// Done is the done marker AND the exit code.
//
// It must be NON-EMPTY. ReadFile returns nothing for both a missing and an
// empty file, so a zero-byte marker would read identically to "not written
// yet" and Poll would never see it — which is why the shell writes the exit
// code into it rather than touching it.
func (p Paths) Done() string { return p.WorkDir() + "/done" }

// ExitCode is the wrapper's status, surfaced on collect to explain a crash.
func (p Paths) ExitCode() string { return p.WorkDir() + "/exit_code" }

// Ask is where the crewlet-ask shim drops a question.
func (p Paths) Ask() string { return p.WorkDir() + "/ask.json" }

// MCPConfig is where the scoped MCP surface is rendered.
func (p Paths) MCPConfig() string { return p.WorkDir() + "/mcp.json" }

// Findings is the agent's authoritative final report.
//
// THE RESULT CARRIER OF RECORD. A run that finishes but never exits can lose
// its streamed final message, and a tool-only run parses to no text at all —
// but this file survives both, so Collect prefers it for the result text and
// treats its presence as the success signal. Under WorkDir for the same reason
// the rest is.
func (p Paths) Findings() string { return p.WorkDir() + "/findings.md" }

// BinDir is where the crewlet-ask shim is installed.
//
// Under the box's own home, not a system path: a local backend runs as an
// unprivileged engine user with no business writing to /usr/local/bin, and two
// boxes there would overwrite each other's shim. Start prepends this to PATH
// so the shim resolves exactly as it would from a system directory.
func (p Paths) BinDir() string { return p.WorkDir() + "/bin" }

// AskShim is the shim's own path.
func (p Paths) AskShim() string { return p.BinDir() + "/crewlet-ask" }
