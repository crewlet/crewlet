// Package codingagent is the detached-run plumbing every coding-agent runner
// shares.
//
// Both supported CLIs run headless in a box and follow the same shape: a
// background process redirects its output to a file and drops a done marker,
// Poll checks that marker and whether the process is still alive, Collect
// decodes the stream and reads the result,
// and an ask overlay surfaces a mid-run clarification. This package owns that
// plumbing so each runner supplies only what is its own: its invocation, its
// config, where its output lands, and how its stream and result are read —
// and how a person's notes to its run's turn reach it between its own steps
// (steer.go: Claude Code's hooks, [ClaudeCode]; OpenCode's plugin,
// opencode_steer.js, [OpenCode]).
//
// # A run's commits credit Crewlet, through git
//
// A run whose request asks for it ([sandbox.RunRequest.Attribution]) has every
// commit it authors as itself carry `Co-authored-by: Crewlet
// <noreply@crewlet.ai>` and the run's own `Crewlet-Run:`, added by a git hook
// the runner installs in the box and the wrapper points git at through the
// run's environment — the same for every CLI, and whether or not the model
// remembered anything (attribution.go, attribution_hook.sh).
//
// # A run's output is read as a stream
//
// A run's event stream and its error stream grow with the run, so neither is
// ever read whole: the stream is decoded a line at a time in bounded memory
// and the error stream is read from its end, while the files meant to be
// read whole (the report, the question, the result line) are, and one too
// large degrades only itself. See stream.go for the reasons, and what each
// used to cost.
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

// Result is where the agent's RESULT is read from whole: its stdout, for a
// CLI whose stdout is its result or its event stream; the stream's last line,
// copied there after exit, for one that keeps its stream apart ([Output]).
func (p Paths) Result() string { return p.WorkDir() + "/result.json" }

// Stream is where a CLI that keeps its event stream apart from its result
// sends its stdout — a file that grows with every tool call the run makes,
// read as a stream and from its end, never whole.
func (p Paths) Stream() string { return p.WorkDir() + "/stream.jsonl" }

// BaseResult is where a run that CONTINUES a session in this box keeps the
// result the job before it left ([Paths.Result]), copied there as the run
// starts and before its own artefacts are cleared: what the session had
// already spent when this run took it up. A CLI whose result states the
// SESSION's spend rather than the run's own ([sessionTotals]) is charged the
// difference, so a continued run is never charged its predecessors' calls
// again.
func (p Paths) BaseResult() string { return p.WorkDir() + "/base-result.json" }

// CLIVersion is where a run that continues a session records its CLI's own
// `--version`, which says whether that CLI's result states the session's spend
// or the run's ([sessionTotals]). Written by the run's script in the box,
// because the CLI that answers is the one the box's PATH finds there.
func (p Paths) CLIVersion() string { return p.WorkDir() + "/cli-version" }

// Brief is where the brief is written for a CLI that reads it on stdin
// ([Output.BriefOnStdin]) rather than on its argv.
func (p Paths) Brief() string { return p.WorkDir() + "/brief.md" }

// System is where a run's standing instructions ([sandbox.RunRequest.System])
// are written for its CLI to add to its own system prompt: named on Claude
// Code's command line, listed in the configuration an OpenCode run is handed
// ([Paths.OpenCodeConfig]).
//
// A FILE, never argv. The text carries the seat's org chart, policies and
// memory, and a command line is readable by every process that can list them
// — on a `direct` box, every account on the engine host — and bounded by
// ARG_MAX. Under WorkDir, out of the checkout, so a `git clean` cannot remove
// it from under a run and a commit cannot carry it into a repository.
func (p Paths) System() string { return p.WorkDir() + "/system.md" }

// ClaudeNotesSystem is where a Claude Code run wired to its turn's notes has
// its standing instructions written a second time, with its CLI's instruction
// about notes after them ([ClaudeCodeSteerInstruction]): the text the run's
// script names in place of [Paths.System] on a release whose note hooks it
// starts ([claudeNotesGate]), so the instruction reaches a run its notes can
// reach and no other. Beside it, and a file for the reason it is.
func (p Paths) ClaudeNotesSystem() string { return p.WorkDir() + "/system-notes.md" }

// CredentialDocument is where a box keeps the login a CLI holds in a database
// of its own: the CLI's own export of it, which the engine seeds a local box
// with and writes back from when the box closes ([CredentialDocument]).
func (p Paths) CredentialDocument() string { return p.Home + "/" + CredentialDocument }

// CredentialDocument is [Paths.CredentialDocument] relative to the box home —
// the key a run's credential files map it under, because a provider seeds
// those relative to the box's home ([sandbox.Spec.CredentialFiles]).
//
// Under the artefact directory rather than the CLI's own data directory: no
// CLI reads this file on its own — the runner imports it into the CLI's
// database before the run and exports the database back into it while the run
// works and once more after — so it lives with the rest of what the engine
// keeps in a box, where a `git clean` in the brief cannot reach it.
const CredentialDocument = ".crewlet/exported-credentials"

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

// BinDir is the box's own bin directory, first on the run's PATH
// ([withBoxBin]): where a setup step installs an executable for the run, a
// coding CLI among them, without root.
//
// Under the box's own home, not a system path: a local backend runs as an
// unprivileged engine user with no business writing to /usr/local/bin, and two
// boxes there would overwrite each other's tools.
func (p Paths) BinDir() string { return p.WorkDir() + "/bin" }

// MCPConfig is where the scoped MCP surface is rendered.
func (p Paths) MCPConfig() string { return p.WorkDir() + "/mcp.json" }

// ClaudeSettings is where a Claude Code run's own settings file is written —
// the hooks that hand its main agent a person's notes between its steps
// ([ClaudeCode.WriteConfig]) — named on its command line with `--settings`.
//
// BESIDE mcp.json, never in the checkout's `.claude/`: the checkout is the
// repository's, which the run edits and a commit carries, and a file there would
// replace the repository's own project settings rather than sit above them.
// `--settings` adds a layer the CLI ranks above the repository's settings and
// below a managed policy, so the repository keeps its own and the run's hooks
// are there whatever it holds. Under WorkDir, so a `git clean` in the brief
// cannot remove it from under the run.
func (p Paths) ClaudeSettings() string { return p.WorkDir() + "/claude-settings.json" }

// OpenCodeConfig is where an OpenCode run's own configuration is written
// ([OpenCode.WriteConfig]), and read from by the run's script, which hands its
// content to the run's server alone as OPENCODE_CONFIG_CONTENT
// ([OpenCode.Command]).
//
// BESIDE the run's other artefacts, never in the checkout: the checkout is the
// repository's, and an opencode.json written there would replace the
// repository's own file, where content handed to the server ranks above it and
// leaves it in place. Under WorkDir, so a `git clean` in the brief cannot
// remove it from under the run.
func (p Paths) OpenCodeConfig() string { return p.WorkDir() + "/opencode.json" }

// OpenCodeValues is the directory holding the values an OpenCode run's MCP
// servers are configured with — each server's url, each header value, each
// environment value — one file apiece, which [Paths.OpenCodeConfig] names by a
// `{file:<path>}` reference rather than carrying the value
// ([openCodeValueFiles]). A directory of the run's own, removed whole before
// every run ([Runner.ClearArtifacts]), so a run configured with fewer values
// than the last keeps none of the last one's.
func (p Paths) OpenCodeValues() string { return p.WorkDir() + "/opencode-values" }

// OpenCodeSteerPlugin is the DIRECTORY an OpenCode run's notes plugin is written
// into, as index.js ([OpenCode.WriteConfig]), and named by the run's
// configuration — the plugin that admits a person's notes to the run's turn into
// its session at its next step (opencode_steer.js).
//
// A DIRECTORY, because OpenCode 2 loads a configured local plugin only from one
// — a file named there is skipped with a warning — trying its `server` module
// and then its `index`. Under WorkDir, beside the configuration that names it,
// never in the checkout or a `.opencode/plugins/` OpenCode would discover on its
// own; and its own, removed whole before every run ([Runner.ClearArtifacts]):
// OpenCode watches a configured plugin's directory and reloads the plugin on any
// change inside it, so nothing is written there once the run's server starts —
// the plugin itself writes nothing at all.
func (p Paths) OpenCodeSteerPlugin() string { return p.WorkDir() + "/opencode-steer" }

// CommitAttribution is the DIRECTORY holding a run's commit attribution
// (attribution.go): the hook ([Paths.AttributionHook]) and the directory the
// run's core.hooksPath names ([Paths.GitHooks]). Under WorkDir, never in the
// checkout, where a commit could carry it and a `git clean` could remove it;
// and its own, removed whole before every run ([Runner.ClearArtifacts]).
func (p Paths) CommitAttribution() string { return p.WorkDir() + "/commit-attribution" }

// AttributionHook is the one script every hook name in [Paths.GitHooks] links
// to.
func (p Paths) AttributionHook() string { return p.CommitAttribution() + "/hook" }

// GitHooks is the directory a run's core.hooksPath names, linking
// [Paths.AttributionHook] under every hook git runs by name.
func (p Paths) GitHooks() string { return p.CommitAttribution() + "/hooks" }

// Findings is the agent's authoritative final report.
//
// THE RESULT CARRIER OF RECORD. A run whose process group died can lose its
// streamed final message, and a tool-only run parses to no text at all — but
// this file survives both, so Collect prefers it for the result text and
// treats its presence as the success signal. Under WorkDir for the same reason
// the rest is.
func (p Paths) Findings() string { return p.WorkDir() + "/findings.md" }
