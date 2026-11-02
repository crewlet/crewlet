package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// ClaudeCodeName is this runner's config name.
const ClaudeCodeName = "claude-code"

// ClaudeCode drives Claude Code headless.
//
// It reaches its model through the run ENVIRONMENT rather than a config file,
// which is why WriteConfig here renders only MCP servers and hooks: the
// credential comes from providers.llm via the sandbox env, and duplicating it
// into a file inside the box would put the secret somewhere the agent can read
// it back.
//
// # A person's notes, between its steps
//
// A run whose launch wired it to its turn's notes ([sandbox.RunRequest.Steer])
// is started with a settings file of its own ([Paths.ClaudeSettings]) holding
// two hooks, after each batch of the main agent's tool calls (PostToolBatch)
// and as it is about to finish (Stop), each a call of the bridge's unlisted
// `steer_take` tool over the run's own MCP connection; the bridge answers with
// the notes as the hook's additional context, or with nothing. Measured on
// 2.1.263 and 2.1.310 against a stub model API:
//
//   - a note handed back after a batch reaches the model after that batch's tool
//     results, and stays in the conversation for the rest of the run — WHERE
//     depends on the model: on 2.1.310 a system message for claude-opus-5-5 and
//     claude-sonnet-5, and a system reminder at the end of the batch's last
//     tool result for claude-sonnet-4-5, each headed "PostToolBatch hook
//     additional context:" ([ClaudeCodeSteerInstruction] says so, and tells the
//     model a note from what a tool produced by that alone);
//   - a note handed back at Stop continues the turn, and the run's result is the
//     answer the model gives after it;
//   - the hook's `timeout` is in seconds, and at it the CLI cancels the call and
//     discards a late answer ([ClaudeHookTimeout]);
//   - a call the bridge answers with an error makes the CLI re-initialise the
//     whole MCP server and tell the model its tools are still connecting — so
//     the bridge answers every take with text, never an error;
//   - the hook's `systemMessage` reaches only the run's own stream, as a
//     `system/informational` line the transcript shows ([claudeEvents]), never
//     a request.
//
// THE POSTURE IS THE SETTINGS FILE'S, and two settings beside it would undo it
// silently. `disableAllHooks` is written false ([claudeSettings]), because a
// repository's own `true` switches every hook off with no error, and an
// explicit false wins over it. And the wrapper UNSETS CLAUDE_CODE_SIMPLE and
// CLAUDE_CODE_SAFE_MODE before the CLI starts ([ClaudeCode.Output]): bare mode
// drops every hook (with CLAUDE.md, subagents and a plan's login), safe mode
// drops hooks and the --mcp-config bridge, and either can arrive from the seat's
// role.sandbox.env, the image, or a profile the login shell sources — of which
// only the wrapper, running after all three, reaches every one. Unset rather
// than refused in config for that reason, and for every run, not only one wired
// to notes: the bridge is every run's tools.
//
// NOTHING ELSE MOVES. No --setting-sources, which would drop the repository's
// CLAUDE.md along with its settings; no --include-hook-events, since the engine
// counts what the bridge hands over and a hook that stopped the loop is read
// off the result line.
//
// A RELEASE OLDER THAN 2.1.163 IS STARTED WITHOUT THE HOOKS ([claudeNotesFloor]):
// it runs them, and drops what a Stop hook answers, so a note taken as the run
// finishes would be recorded read by a run that never saw it. What switches the
// hooks off otherwise — a managed policy that allows only managed hooks
// (allowManagedHooksOnly, strictPluginOnlyCustomization) or disables every hook
// (disableAllHooks) — leaves, like that older release, a run that succeeded
// without asking once, which the collection says (internal/sandbox,
// steer_reader_never_ran).
type ClaudeCode struct{}

var _ CLI = ClaudeCode{}

// NewClaudeCode returns the runner.
func NewClaudeCode() *Runner { return New(ClaudeCode{}) }

// Name is the coding agent's key in the runner registry.
func (ClaudeCode) Name() string { return ClaudeCodeName }

// Resumes says Claude Code continues a session it reported (`--resume`).
func (ClaudeCode) Resumes() bool { return true }

// NoteInstruction is the standing instruction about notes a Claude Code run is
// given: it takes a person's notes between its steps, through the hooks its
// settings file holds — see [ClaudeCode].
func (ClaudeCode) NoteInstruction() string { return ClaudeCodeSteerInstruction }

// Command builds the headless `claude -p` invocation.
//
// Pure and deterministic so the exact flags can be pinned by a test. Budget
// caps are appended only when set, so a minimal call stays minimal.
//
// THE BRIEF IS NOT ON IT: `-p` with no prompt reads the prompt on stdin, which
// the wrapper points at the brief's file ([ClaudeCode.Output]) — verbatim,
// measured on 2.1.263 and 2.1.310. A brief carries every note the turn has
// read, so it grows; one argument is capped at 128 KiB (MAX_ARG_STRLEN); and a
// command line is readable by every account on a `direct` box's host, the
// reason [Paths.System] gives for the system text's file.
//
// It runs with permissions bypassed, in every cell, and that is a deliberate
// consequence of how the run is driven rather than an oversight: a headless
// run has nobody to answer a permission prompt, so a gate left to ask would
// hang the run or refuse the step, and there is no per-tool allowlist inside
// it. What the agent may reach is decided before it starts, engine-side: by
// its box's CELL — `container` and `e2b` keep it off the engine host, while
// `direct` runs it on the engine host as the engine's own user — by which MCP
// servers were rendered into its config, and by which credentials the run
// env carries.
//
// STREAM-JSON, AND --verbose WITH IT. `--output-format json` prints ONE object
// when the run ends and nothing before it, and in print mode this CLI writes
// nothing to stderr either — so a run on Claude Code showed a person nothing
// while it worked and left no transcript when it was done. stream-json prints
// every message as it happens, one JSON object a line, ending — as the vendor
// documents the stream — in the result message, the same object json printed
// alone. In print mode the CLI refuses stream-json without --verbose ("When
// using --print, --output-format=stream-json requires --verbose").
//
// THE SYSTEM TEXT IS APPENDED, NOT SUBSTITUTED: `--append-system-prompt-file`
// adds the run's standing instructions after Claude Code's own system prompt,
// which stays whole (measured on 2.1.310, off the request the CLI sent). That
// prompt is the harness this run exists to use — how to drive its shell and
// editor and work through a task — which a run on the engine host, denying
// every one of those tools, has no use for. The FILE variant, written by [ClaudeCode.WriteConfig]:
// see [Paths.System] for why never argv. Passed only for a run that has system
// text, so a run with none runs on the vendor's prompt alone.
func (ClaudeCode) Command(req sandbox.RunRequest, paths Paths, configPath string) string {
	parts := []string{
		"claude", "-p",
		"--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions",
	}
	// THE RUN'S OWN HOOKS, which hand it a person's notes between its steps
	// — only where its launch wired it to them ([ClaudeCode.WriteConfig]), and
	// only on a release that reads what they answer — AND ITS SYSTEM TEXT,
	// told of notes only where they are read: the gate below decides both,
	// and hands the flags in as the script's own arguments.
	if req.Steer.Set() {
		parts = append(parts, `"$@"`)
	}
	// THE EARLIER RUN'S OWN CONVERSATION, carried on with the brief — an
	// answer, an approval — as its next prompt ([sandbox.RunRequest.Resume]).
	// Claude Code keeps its sessions under the box's home, which a
	// continued box still has.
	if req.Resume != "" {
		parts = append(parts, "--resume", sandbox.ShellQuote(req.Resume))
	}
	if req.System != "" && !req.Steer.Set() {
		parts = append(parts, "--append-system-prompt-file", sandbox.ShellQuote(paths.System()))
	}
	// VERBATIM, because the only entry that reaches this runner is a
	// claude-code one, which already names the model in Claude Code's own
	// terms — its alias or id exactly as its runs on the engine host pass
	// it. A launch passes over every other CLI's entry, so none reaches here.
	if req.Model != "" {
		parts = append(parts, "--model", sandbox.ShellQuote(req.Model))
	}
	if req.Limits.MaxTurns > 0 {
		parts = append(parts, "--max-turns", strconv.Itoa(req.Limits.MaxTurns))
	}
	if req.Limits.MaxBudgetUSD > 0 {
		parts = append(parts, "--max-budget-usd",
			strconv.FormatFloat(req.Limits.MaxBudgetUSD, 'f', -1, 64))
	}
	if configPath != "" {
		// --strict-mcp-config so the agent gets EXACTLY the scoped surface:
		// without it the CLI also loads whatever config the box's home
		// happens to carry, which on a reused box is the previous run's.
		parts = append(parts, "--mcp-config", sandbox.ShellQuote(configPath), "--strict-mcp-config")
	}
	cmd := strings.Join(parts, " ")
	if !req.Steer.Set() {
		return cmd
	}
	// ONE SCRIPT, so the wrapper's redirections — the brief on stdin, the
	// stream, the error file — reach the gate and the CLI alike, and `exec`
	// leaves the CLI where the script was.
	plain := ""
	if req.System != "" {
		plain = paths.System()
	}
	return "sh -c " + sandbox.ShellQuote(claudeNotesGate(paths.ClaudeSettings(), paths.ClaudeNotesSystem(), plain)+
		"exec "+cmd)
}

// claudeNotesFloor is the oldest Claude Code release a run is started with its
// notes hooks on: 2.1.163, the first whose Stop hook's additional context
// reaches the model (its changelog: "Stop and SubagentStop hooks can now return
// hookSpecificOutput.additionalContext").
//
// An older release that has mcp_tool hooks (2.1.118 onwards) runs both hooks,
// and reads what the PostToolBatch one answers, but drops what the Stop one does
// — measured on 2.1.150: the note was taken at Stop, so recorded read and
// published delivered, and no request carried it, the run's answer the one from
// before the note. Started without the hooks, that release takes nothing at a
// boundary it drops: the notes wait for the review, the run is not recorded as
// having asked ([sandbox.RunSteer.Pulled]), and its collection says so
// (steer_reader_never_ran) — which tells an operator the box's CLI needs an
// upgrade, where taking the notes would have told them nothing at all.
const claudeNotesFloor = "2.1.163"

// claudeNotesGate is the start of a run's script wired to its notes: it reads
// the box's `claude --version` and sets the script's arguments. On a release at
// or after [claudeNotesFloor] they are `--settings <settings>` and the run's
// standing instructions told of notes (`--append-system-prompt-file <notes>`,
// [Paths.ClaudeNotesSystem]); on an older one, which the run's error stream
// says, the standing instructions alone (plain, [Paths.System]) — nothing for a
// run that has none. A version it cannot read is taken for a current release:
// every release prints `MAJOR.MINOR.PATCH (Claude Code)`, so one that prints
// anything else is newer than this script, and a CLI that is not there fails at
// the run itself.
//
// THE SYSTEM TEXT IS CHOSEN WITH THE HOOKS, because only this script knows
// whether a note can reach the run: told of notes, a run started without the
// hooks that read them would be told of a channel that never opens.
//
// POSIX parameter expansion, case and test alone, which every box's sh has
// (dash, bash, busybox ash): no awk, no sort -V. The version is cut at its first
// character that is neither a digit nor a dot, so each of its three parts is
// digits or nothing, and a part that is nothing reads as the floor.
func claudeNotesGate(settings, notes, plain string) string {
	major, minor, patch := floorParts(claudeNotesFloor)
	older := "set --; "
	if plain != "" {
		older = "set -- --append-system-prompt-file " + sandbox.ShellQuote(plain) + "; "
	}
	return "set --; " +
		`v=$(claude --version < /dev/null 2> /dev/null); v=${v%%[!0-9.]*}; ` +
		`a=${v%%.*}; r=${v#*.}; b=${r%%.*}; c=${r#*.}; c=${c%%.*}; ` +
		`case "$a.$b.$c" in [0-9]*.[0-9]*.[0-9]*) ;; *) a=` + major + ` b=` + minor + ` c=` + patch + `;; esac; ` +
		`if [ "$a" -gt ` + major + ` ] || { [ "$a" -eq ` + major + ` ] && { [ "$b" -gt ` + minor + ` ] || ` +
		`{ [ "$b" -eq ` + minor + ` ] && [ "$c" -ge ` + patch + ` ]; }; }; }; then ` +
		`set -- --settings ` + sandbox.ShellQuote(settings) + ` --append-system-prompt-file ` + sandbox.ShellQuote(notes) + `; ` +
		`else ` + older +
		`echo "crewlet: this box runs Claude Code $v, older than ` + claudeNotesFloor + `, the oldest ` +
		`that reads a note at the run's finish; the run starts without the hooks that read its notes, ` +
		`is not told of them, and they wait for its review" >&2; fi; `
}

// floorParts is a MAJOR.MINOR.PATCH version's three numbers, as text.
func floorParts(version string) (major, minor, patch string) {
	major, rest, _ := strings.Cut(version, ".")
	minor, patch, _ = strings.Cut(rest, ".")
	return major, minor, patch
}

// WriteConfig writes the run's system text and its settings file where
// [ClaudeCode.Command] names them, and renders the scoped MCP surface — each
// only when there is one: the settings for a run wired to its turn's notes
// ([sandbox.RunRequest.Steer]), which every seat's executor run is, and the
// assistant's deep work is not.
//
// A RUN WIRED TO ITS NOTES HAS ITS SYSTEM TEXT TWICE: as it stands
// ([Paths.System]), and told of notes ([Paths.ClaudeNotesSystem]) — which of the
// two it is started with is its box's to decide, by its CLI's release
// ([claudeNotesGate]).
//
// The settings file is written afresh, at the mode a box writes every file with
// (0600 on a local box): a box write replaces a file whole
// ([sandbox.Sandbox.WriteFile]), and the run's artefacts are cleared before it
// starts besides ([Runner.ClearArtifacts]), so a previous run's wider file is
// never kept.
func (c ClaudeCode) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error) {
	if req.System != "" {
		if err := box.WriteFile(ctx, paths.System(), []byte(req.System)); err != nil {
			return "", fmt.Errorf("codingagent: writing the system prompt: %w", err)
		}
	}
	if req.Steer.Set() {
		settings, err := claudeSteerSettings(req.Steer)
		if err != nil {
			return "", fmt.Errorf("codingagent: rendering Claude Code's settings: %w", err)
		}
		if err := box.WriteFile(ctx, paths.ClaudeSettings(), settings); err != nil {
			return "", fmt.Errorf("codingagent: writing Claude Code's settings: %w", err)
		}
		told := withNoteInstruction(req.System, c.NoteInstruction())
		if err := box.WriteFile(ctx, paths.ClaudeNotesSystem(), []byte(told)); err != nil {
			return "", fmt.Errorf("codingagent: writing the system prompt told of notes: %w", err)
		}
	}
	if len(req.MCPServers) == 0 {
		return "", nil
	}
	servers := make(map[string]any, len(req.MCPServers))
	for name, s := range req.MCPServers {
		servers[name] = claudeCodeMCP(s)
	}
	blob, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := box.WriteFile(ctx, paths.MCPConfig(), blob); err != nil {
		return "", err
	}
	return paths.MCPConfig(), nil
}

// claudeCodeMCP is one server in this CLI's own .mcp.json vocabulary.
//
// THE KEY NAMES ARE THIS RUNNER'S, which is why they live here: they are what
// `claude --mcp-config` reads, and nothing above this file should have to know
// them. internal/sandbox used to spell them out and then say in its own doc
// that they belonged to a runner.
//
// An empty `env` or `headers` is OMITTED rather than written as an empty
// object, which is what the CLI's own examples show and what this wrote
// before.
func claudeCodeMCP(s sandbox.MCPServer) map[string]any {
	if s.Transport == sandbox.TransportHTTP {
		out := map[string]any{"type": string(sandbox.TransportHTTP), "url": s.URL}
		if len(s.Headers) > 0 {
			out["headers"] = s.Headers
		}
		return out
	}
	out := map[string]any{"command": s.Command, "args": s.Args}
	if len(s.Env) > 0 {
		out["env"] = s.Env
	}
	return out
}

// Output is where a job writes under stream-json, and what it reads.
//
// The EVENT STREAM goes to its own file, read as a stream and from its end,
// and the RESULT is the stream's last line, which the wrapper copies to the
// result file after the CLI exits — so the result is read whole from a file of
// one line however long the stream grew. It exits cleanly, so the done marker
// is its only completion signal and the poll reads nothing of the stream.
//
// THE BRIEF ON STDIN, for the reasons [ClaudeCode.Command] gives; and the two
// variables that switch hooks and the bridge off UNSET before the CLI starts
// ([claudeSwitchesOff]).
func (ClaudeCode) Output(paths Paths) Output {
	return Output{Stdout: paths.Stream(), Events: true, Result: paths.Result(),
		BriefOnStdin: true, Unset: claudeSwitchesOff}
}

// claudeSwitchesOff is every variable that, set in a run's environment, starts
// Claude Code without the hooks and the bridge its run is given: bare mode
// (CLAUDE_CODE_SIMPLE), which also drops CLAUDE.md, subagents and a plan's
// login, and safe mode (CLAUDE_CODE_SAFE_MODE), which drops hooks and every
// --mcp-config server. See [ClaudeCode] for why they are unset rather than
// refused.
var claudeSwitchesOff = []string{"CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_SAFE_MODE"}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (ClaudeCode) Events() Decoder { return &claudeEvents{tools: map[string]string{}} }

// Parse maps the CLI's result — the stream's last line — onto a result.
//
// TOLERANT BY DESIGN: non-JSON or partial output yields a FAILED result
// carrying an account of it, never an error. A coding agent that crashed should
// surface as "did not deliver" and let the turn continue, not blow the turn up
// — the executor can still report what happened, which is more use to the
// requester than a failed turn.
//
// A RESULT IS AN OBJECT OF TYPE `result`, and nothing else is. Under the
// stream the result file holds the stream's LAST LINE, whatever it was: a run
// that died part-way leaves an assistant message or a tool result there, and
// an object of another type has no subtype and no is_error — read as a
// result, it was a run that SUCCEEDED and said nothing.
func (ClaudeCode) Parse(stdout string) sandbox.Result {
	text := strings.TrimSpace(stdout)
	if text == "" {
		return sandbox.Result{Error: "the coding agent produced no output"}
	}
	msg, ok := decodeClaudeResult(text)
	if !ok {
		if last := lastLine(text); strings.HasPrefix(last, "{") {
			// A LINE THAT STARTS AN OBJECT AND IS NOT ONE is a message the
			// process stopped writing part-way, not an account of anything:
			// its fragment — of a file it read, as like as not — is no use to
			// a reader, and it is what the error stream beside it explains.
			return sandbox.Result{Error: fmt.Sprintf("the coding agent's output ends in a line "+
				"that is not a whole JSON object (%s), so it never reported how its run ended",
				humanSize(int64(len(last))))}
		}
		// THE OUTPUT IS THE FAILURE'S DETAIL, WHOLE. Unparseable output is
		// the case where the text IS the account — there is no structured
		// field to fall back to — and the useful part of it (the actual
		// error, after the banner and the warnings) is at the END, which a
		// head cut discarded and a tail cut kept only by luck of size. It
		// is carried as the Error rather than as a report, because it is
		// not one: the coordinator condenses a failure past the record's
		// bound keeping its cause, where a report keeps its findings.
		return sandbox.Result{
			Error: "the coding agent's output could not be parsed:\n" + text,
		}
	}
	if msg.Type != "result" {
		what := "an object with no type"
		if msg.Type != "" {
			what = fmt.Sprintf("a %q message", msg.Type)
		}
		return sandbox.Result{Error: "the coding agent stopped before it reported how its run ended: " +
			"the last thing it printed was " + what + ", not its result"}
	}

	res := sandbox.Result{
		Text:          msg.Result,
		Success:       msg.succeeded(),
		SessionID:     msg.SessionID,
		CostUSD:       msg.TotalCostUSD,
		DeliveredRefs: deliveredRefs(msg.Result),
		// The run's own model turns, as its loop counted them: the
		// provider calls the run made, which the engine never saw.
		Turns: msg.NumTurns,
	}
	if u := msg.Usage; u != nil {
		// INPUT TOKENS ARE A SUM: the vendor's input_tokens is only the
		// UNCACHED remainder, and a coding run is almost entirely cached
		// rounds — so reading it alone put a fraction of every run's
		// prompt on the budget counter and the spend rollup.
		res.CacheReadTokens = u.CacheReadInputTokens
		res.CacheWriteTokens = u.CacheCreationInputTokens
		res.InputTokens = u.InputTokens + res.CacheReadTokens + res.CacheWriteTokens
		res.OutputTokens = u.OutputTokens
	}
	if !res.Success {
		res.Error = msg.failure()
		// THE LINE ITSELF, for the reader that classifies how the run
		// ended by the vendor's status rather than its words: a spent plan
		// is `is_error` with `api_error_status` 429, whatever the sentence
		// in `result` says this release ([sandbox.Result.Event]).
		res.Event = text
	}
	return res
}

// claudeResult is the CLI's result message — the stream's last line, and the
// one object `--output-format json` printed — as the vendor's Agent SDK
// documents it: `subtype` names HOW the run ended ("success",
// "error_max_turns", "error_during_execution", "error_max_budget_usd", …),
// `is_error` whether it failed, `result` the final text of a run that got as
// far as one, and `errors` what went wrong on a subtype that is not success,
// which carries no `result` at all.
//
// Decoded into named fields so the stream's decoder and [ClaudeCode.Parse] read
// one shape, and TOLERANTLY (a field of an unexpected type is left zero and
// the rest still read), because a field the vendor reshapes must cost that
// field rather than the run's whole account.
type claudeResult struct {
	Type         string   `json:"type"`
	Subtype      string   `json:"subtype"`
	IsError      bool     `json:"is_error"`
	Result       string   `json:"result"`
	Errors       []string `json:"errors"`
	SessionID    string   `json:"session_id"`
	TotalCostUSD float64  `json:"total_cost_usd"`
	// NumTurns is how many turns the run's agent loop took — one model
	// call each — on every subtype, a run stopped by its turn cap included.
	NumTurns int `json:"num_turns"`
	// TerminalReason is why the run's loop ended, beside the subtype:
	// "completed" for a loop that finished, and [claudeHookStopped] for one a
	// hook stopped. Absent on a CLI that does not report it.
	TerminalReason string `json:"terminal_reason"`
	Usage          *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// claudeHookStopped is the terminal reason of a run whose loop a hook stopped.
const claudeHookStopped = "hook_stopped"

// succeeded is whether the run finished its work. EVERY FIELD MUST SAY SO: a
// run that hit its turn cap reports no is_error but did not finish, one whose
// model call failed is a success subtype carrying is_error, and one a hook
// stopped is a success subtype with no error and an empty result, its loop
// ended part-way ([claudeHookStopped]; measured on 2.1.263 and 2.1.310, a
// repository's PostToolBatch hook that exited 2). Read as it reports itself, a
// hook's stop was a run that succeeded and said nothing.
func (r claudeResult) succeeded() bool {
	return (r.Subtype == "" || r.Subtype == "success") && !r.IsError &&
		r.TerminalReason != claudeHookStopped
}

// failure says how a run that did not succeed ended: the subtype that names
// how, and whatever the message says about why — its errors, or the final
// text it got as far as — or, for a loop a hook stopped, that.
func (r claudeResult) failure() string {
	if r.TerminalReason == claudeHookStopped {
		stopped := "a hook in the run stopped its loop before it finished (Claude Code " +
			"terminal_reason hook_stopped): a hook of the repository's, the image's or the box " +
			"home's own settings exited 2 or returned a block; the run's own steering hooks " +
			"never block"
		if why := strings.TrimSpace(firstNonBlank(strings.Join(r.Errors, "\n"), r.Result)); why != "" {
			stopped += ": " + why
		}
		return stopped
	}
	how := r.Subtype
	if how == "success" {
		how = ""
	}
	why := strings.TrimSpace(firstNonBlank(strings.Join(r.Errors, "\n"), r.Result))
	switch {
	case how != "" && why != "":
		return how + ": " + why
	case how != "":
		return how
	case why != "":
		return why
	}
	return "it reported an error and said nothing about it"
}

// decodeClaudeResult reads the result message: the one line the result file
// holds, which the wrapper copied from the end of the stream.
func decodeClaudeResult(text string) (claudeResult, bool) {
	if !strings.HasPrefix(text, "{") {
		return claudeResult{}, false
	}
	var msg claudeResult
	err := json.Unmarshal([]byte(text), &msg)
	var mismatch *json.UnmarshalTypeError
	// A syntax error is reported before any field is decoded, so a mismatch
	// means a whole object with one field of another type.
	if err == nil || errors.As(err, &mismatch) {
		return msg, true
	}
	return claudeResult{}, false
}

// lastLine is the text's last line, trimmed.
func lastLine(text string) string {
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return strings.TrimSpace(text)
}
