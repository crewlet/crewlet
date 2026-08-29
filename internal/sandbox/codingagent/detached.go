package codingagent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

var log = logging.Get("sandbox.coding_agent")

// MaxTranscript caps the captured activity transcript carried on the result.
//
// The TAIL is kept: the most recent activity plus the conclusion is what a
// reader wants, and the head — the clone, the dependency install — is the
// least interesting thing to drop. 100k characters is a few hundred lines of
// a coding agent's streamed events, which is enough to see what it did without
// putting a megabyte of log through the event store per run.
const MaxTranscript = 100_000

// errorDetailLimit bounds the error text a failed run reports.
//
// It reaches a model as a tool message, so it is a diagnosis rather than a
// log: the first 500 characters of a crash carry the exception and its top
// frames, and the rest is a stack the model cannot act on.
const errorDetailLimit = 500

// prPattern matches a pull-request URL, on either of the two hosts this engine
// integrates with. It is a FALLBACK: a runner whose output names its delivered
// refs explicitly is preferred, and this scrapes the findings for one when the
// agent only wrote the URL in prose.
var prPattern = regexp.MustCompile(
	`https://(?:github\.com|gitlab\.com)/[\w.\-/]+/(?:pull|merge_requests|-/merge_requests)/\d+`)

// CLI is what one coding agent contributes on top of the shared plumbing.
//
// Two methods, and deliberately only two: the invocation and the parser. Every
// runner difference this engine has met is one of those — where the marker
// goes, how the box is torn down, how a question is signalled and how a
// transcript is capped are the same for all of them, and a runner that could
// override those would be a second implementation of the completion protocol.
type CLI interface {
	// Name is the coding agent's config name: "claude-code", "opencode".
	Name() string

	// Command builds the headless invocation. The brief is already final —
	// the report instruction is appended by the base.
	Command(req sandbox.RunRequest, paths Paths, configPath string) string

	// Parse maps this agent's stdout onto a result.
	Parse(stdout string) sandbox.Result

	// WriteConfig renders the agent's config file into the box and returns
	// the path the CLI is pointed at, or "" when there is nothing to write.
	WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error)

	// Finished reports whether the streamed output says the agent has done
	// its work, for one that finishes but never exits. A runner whose CLI
	// exits cleanly answers false and relies on the done marker.
	Finished(stdout string) bool
}

// Runner drives one CLI through the detached lifecycle.
type Runner struct{ cli CLI }

var _ sandbox.Runner = (*Runner)(nil)

// New wraps a CLI in the shared plumbing.
func New(cli CLI) *Runner { return &Runner{cli: cli} }

// Name is the coding agent's config name.
func (r *Runner) Name() string { return r.cli.Name() }

// Install prepares the box: the artefact directory and the ask shim.
//
// The CLI itself is NOT installed here — it ships in the image, which is what
// makes a coding box a template rather than a per-run build. Environment
// provisioning (git auth, registry credentials, toolchains) is not the
// runner's concern either: the manager applies the launch's setup steps after
// this returns.
func (r *Runner) Install(ctx context.Context, box sandbox.Sandbox) error {
	paths := PathsFor(box)
	if _, err := box.Exec(ctx, "mkdir -p "+shellQuote(paths.BinDir()), sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("codingagent: preparing %s: %w", paths.BinDir(), err)
	}
	if err := box.WriteFile(ctx, paths.AskShim(), []byte(AskShim(paths.Ask()))); err != nil {
		return fmt.Errorf("codingagent: installing the ask shim: %w", err)
	}
	if _, err := box.Exec(ctx, "chmod +x "+shellQuote(paths.AskShim()), sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("codingagent: making the ask shim executable: %w", err)
	}
	return nil
}

// ClearArtifacts removes the previous run's markers before a reuse run.
//
// A reused box still carries the prior run's done marker, result, findings and
// ask file. Without clearing them the completion poll fires immediately on the
// stale marker and Collect reads the old result. THE CHECKOUT IS LEFT INTACT —
// that disk state is the continued context, and it is the whole reason to
// reuse a box.
func (r *Runner) ClearArtifacts(ctx context.Context, box sandbox.Sandbox) error {
	p := PathsFor(box)
	cmd := "rm -f " + strings.Join([]string{
		shellQuote(p.Done()), shellQuote(p.ExitCode()), shellQuote(p.Result()),
		shellQuote(p.Findings()), shellQuote(p.Ask()),
	}, " ")
	if _, err := box.Exec(ctx, cmd, sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("codingagent: clearing the prior run's artefacts: %w", err)
	}
	return nil
}

// Start launches the coding agent detached and returns its handle.
//
// The job runs UNCAPPED: nothing force-stops it on a wall-clock timer, so a
// legitimately long run is free to finish. On a clean exit the shell writes the
// exit code into both the exit-code file and the done marker — non-empty, for
// the reason on [Paths.Done]. When the agent finishes but never exits, Poll
// falls back to the streamed output, and the box teardown reaps the husk.
//
// Its stdin is closed: a headless agent must never block waiting for input,
// and one that does would hang forever with no timer to stop it.
func (r *Runner) Start(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest) (sandbox.RunHandle, error) {
	paths := PathsFor(box)
	// The artefacts are cleared unconditionally rather than only on a reuse
	// path: the runner cannot tell a fresh box from a reused one, and
	// clearing a fresh box's absent files costs one exec.
	if err := r.ClearArtifacts(ctx, box); err != nil {
		return sandbox.RunHandle{}, err
	}
	configPath, err := r.cli.WriteConfig(ctx, box, req, paths)
	if err != nil {
		return sandbox.RunHandle{}, err
	}
	req.Brief = finalBrief(req.Brief, paths)
	inner := withShimPath(r.cli.Command(req, paths, configPath), paths)

	script := fmt.Sprintf(
		"%s < /dev/null > %s 2> %s; code=$?; echo $code > %s; echo $code > %s",
		inner, shellQuote(paths.Result()), shellQuote(paths.Err()),
		shellQuote(paths.ExitCode()), shellQuote(paths.Done()))

	// A LOGIN SHELL, and the -l is load-bearing: a coding CLI is commonly
	// installed through nvm, asdf or a similar version manager, whose PATH
	// entries exist only in a profile a login shell sources. A plain `sh -c`
	// finds no CLI at all in exactly the images operators build.
	//
	// It also means the box's PATH inside the wrapper is the PROFILE's, not
	// the environment the engine handed the process — which is why the shim
	// directory is prepended INSIDE the script (see withShimPath) rather
	// than exported around it: an assignment made outside would be replaced
	// by the profile before the agent ever ran.
	pid, err := box.StartBackground(ctx, "sh -lc "+shellQuote(script), sandbox.ExecOptions{Env: req.Env})
	if err != nil {
		return sandbox.RunHandle{}, fmt.Errorf("codingagent: starting %s: %w", r.cli.Name(), err)
	}
	log.Info("coding_agent_started", "agent", r.cli.Name(), "pid", pid)
	return sandbox.RunHandle{CommandID: pid}, nil
}

// finalBrief appends the report instruction, addressed at THIS box.
//
// Composed here rather than at the launch because the findings path is a
// property of the box the run lands in, which the caller does not know when it
// builds the brief.
func finalBrief(brief string, paths Paths) string {
	return strings.TrimRight(brief, "\n") + "\n" + FindingsInstruction(paths.Findings())
}

// withShimPath prepends the box's shim directory to PATH for one command.
//
// The shim lives under the box's own home rather than a system directory (see
// [Paths.BinDir]), so the brief's `crewlet-ask "..."` instruction only
// resolves if that directory is on the agent's PATH.
func withShimPath(cmd string, paths Paths) string {
	return "PATH=" + shellQuote(paths.BinDir()) + `:"$PATH" ` + cmd
}

// Poll reports whether the detached job has finished.
//
// THREE SIGNALS, because a finished, hung or dead job must not be able to
// wedge the run — and there is no run-time TTL to fall back on, since the
// waiter keeps the box alive for exactly as long as the job needs:
//
//  1. THE DONE MARKER. The wrapper returned, cleanly or not, and wrote its
//     exit code.
//  2. A TERMINAL SIGNAL IN THE OUTPUT, for an agent that finishes its work but
//     never exits — an open file watcher or an MCP subprocess keeps its event
//     loop alive, so the shell never reaches the marker write. The keepalive
//     holds the box open so the terminal event lands and Collect reads it.
//  3. PROCESS LIVENESS. The wrapper's pid is gone yet no marker was written:
//     the whole process group died abnormally, before the tail echo ran.
//     Without this the run hangs forever; with it, Collect surfaces the
//     partial result and the failure.
//
// A still-alive wrapper reports NOT DONE, whether it is working or hung: a
// genuinely hung-but-alive process is indistinguishable from a working one
// without a timer, and imposing one is exactly what this design refuses.
func (r *Runner) Poll(ctx context.Context, box sandbox.Sandbox, handle sandbox.RunHandle) (bool, error) {
	paths := PathsFor(box)
	marker, err := box.ReadFile(ctx, paths.Done())
	if err != nil {
		return false, err
	}
	if len(marker) > 0 {
		return true, nil
	}
	stdout, err := readText(ctx, box, paths.Result())
	if err != nil {
		return false, err
	}
	if stdout != "" && r.cli.Finished(stdout) {
		return true, nil
	}
	if handle.CommandID != "" {
		alive, err := processAlive(ctx, box, handle.CommandID)
		if err != nil {
			// An unreadable liveness probe is not proof of death, and
			// declaring the run over on one would collect a partial result
			// from a job that is still working. The next tick asks again.
			//nolint:nilerr // Deliberate: see the paragraph above.
			return false, nil
		}
		if !alive {
			log.Warn("coding_agent_process_gone",
				"agent", r.cli.Name(), "pid", handle.CommandID)
			return true, nil
		}
	}
	return false, nil
}

// processAlive probes the wrapper with kill -0, which sends no signal.
//
// The pid is the shell wrapper Start launched: while the coding agent runs —
// or has finished but not exited — the wrapper is alive, and once it exits it
// has already written the done marker, which Poll checks first. So a DEAD
// WRAPPER WITH NO MARKER means the process group was killed outright and the
// run is over.
func processAlive(ctx context.Context, box sandbox.Sandbox, pid string) (bool, error) {
	res, err := box.Exec(ctx, "kill -0 "+shellQuote(pid)+" 2>/dev/null", sandbox.ExecOptions{})
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

// Collect reads the finished job's result out of the box.
func (r *Runner) Collect(ctx context.Context, box sandbox.Sandbox, handle sandbox.RunHandle) (sandbox.Result, error) {
	paths := PathsFor(box)
	stdout, err := readText(ctx, box, paths.Result())
	if err != nil {
		return sandbox.Result{}, err
	}
	result := r.cli.Parse(stdout)

	// The transcript is the observability surface for an agent that emits no
	// telemetry of its own. The parser may have built one from streamed
	// events; otherwise the raw stderr is it. Read once, reused below for
	// the crash detail.
	stderr, err := readText(ctx, box, paths.Err())
	if err != nil {
		return sandbox.Result{}, err
	}
	stderr = strings.TrimSpace(stderr)
	switch {
	case result.Transcript != "":
		result.Transcript = tail(result.Transcript, MaxTranscript)
	case stderr != "":
		result.Transcript = tail(stderr, MaxTranscript)
	}

	code, err := readText(ctx, box, paths.ExitCode())
	if err != nil {
		return sandbox.Result{}, err
	}
	code = strings.TrimSpace(code)
	crashed := code != "" && code != "0"

	// THE FINDINGS FILE IS THE RESULT CARRIER OF RECORD. The brief asks the
	// agent to write its report there before stopping, and it survives both
	// a finished-but-never-exited run whose streamed message was lost and a
	// tool-only run that parses to no text — so it wins for the result text,
	// and its presence is the success signal unless the process crashed.
	findings, err := readText(ctx, box, paths.Findings())
	if err != nil {
		return sandbox.Result{}, err
	}
	findings = strings.TrimSpace(findings)
	switch {
	case findings != "":
		result.Text = findings
		if len(result.DeliveredRefs) == 0 {
			result.DeliveredRefs = prPattern.FindAllString(findings, -1)
		}
		if !crashed {
			result.Success = true
			result.Error = ""
		}
	case !result.Success && strings.TrimSpace(result.Text) == "":
		// No report AND nothing parsed: the job produced nothing. Surface
		// the stderr and the exit code so the completion reports a real
		// failure rather than a silent stall.
		detail := stderr
		if crashed {
			crash := "the coding agent exited with status " + code
			if detail != "" {
				detail = crash + ": " + detail
			} else {
				detail = crash
			}
		}
		if detail != "" {
			result.Error = truncate(detail, errorDetailLimit)
		}
	}

	// Redacted HERE, at the boundary: everything above came out of a box
	// whose environment holds the seat's credentials, and everything below
	// reaches a model, an event store and a screen.
	result.Text = redact.Secrets(result.Text)
	result.Error = redact.Secrets(result.Error)
	result.Transcript = redact.Secrets(result.Transcript)

	return r.overlayAsk(ctx, box, result)
}

// overlayAsk surfaces a question the shim recorded, if there is one.
func (r *Runner) overlayAsk(ctx context.Context, box sandbox.Sandbox, result sandbox.Result) (sandbox.Result, error) {
	blob, err := readText(ctx, box, PathsFor(box).Ask())
	if err != nil {
		return result, err
	}
	blob = strings.TrimSpace(blob)
	if blob == "" {
		return result, nil
	}
	var ask struct {
		Question string `json:"question"`
		To       string `json:"to"`
	}
	if err := json.Unmarshal([]byte(blob), &ask); err != nil {
		// A malformed signal is not a reason to lose the result the run
		// did produce; the run reads as finished rather than as parked.
		log.Warn("coding_agent_ask_unreadable", "agent", r.cli.Name(), "error", err.Error())
		return result, nil
	}
	if ask.Question == "" {
		return result, nil
	}
	result.NeedsInput = true
	result.Question = redact.Secrets(ask.Question)
	result.AskTo = ask.To
	if result.AskTo == "" {
		result.AskTo = "requester"
	}
	return result, nil
}

func readText(ctx context.Context, box sandbox.Sandbox, path string) (string, error) {
	raw, err := box.ReadFile(ctx, path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// tail keeps the last n characters with a marker saying it cut.
func tail(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return "…[earlier output truncated]…\n" + text[len(text)-n:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
