package codingagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

var log = logging.Get("sandbox.coding_agent")

// deliveredRefs is what a run delivered, as its own account names it: the
// branches it pushed and the pull or merge requests it opened, read from the
// texts given in the order given — the report before the CLI's last message.
//
// THE RUN NAMES THEM, ON LINES OF THEIR OWN. The report is asked for one
// `Delivered: <branch or URL>` line per ref ([FindingsInstruction]), and the
// first text holding any such line is the answer, whole and in its order: a
// branch as well as a pull request, on any host and any scheme. WHICH HOSTS a
// box can push to is set by the seat's own environment and setup steps, which
// the engine deliberately never names, and a seat can push to a host no
// integration block mentions — so no list of hosts the engine knows can say
// what a run delivered, and the two it used to know left a GitHub Enterprise,
// a self-managed GitLab and the walkthrough's own GitLab on a port delivering
// nothing, and a pushed branch, which no URL pattern matches, never delivered
// at all.
//
// A RUN THAT NAMES NONE is read by the URL's shape instead ([prPattern]):
// the first text holding a pull request's URL gives every one it holds. A
// named ref outranks a scraped one wherever each is found, because a scrape
// cannot tell a pull request the run opened from one it only read.
func deliveredRefs(texts ...string) []string {
	for _, text := range texts {
		if refs := namedRefs(text); len(refs) > 0 {
			return refs
		}
	}
	for _, text := range texts {
		if refs := prPattern.FindAllString(text, -1); len(refs) > 0 {
			return refs
		}
	}
	return nil
}

// deliveredLine is one line naming a delivered ref — `Delivered: <ref>` — read
// whatever a report lays it out with: a list marker before it, emphasis
// around the label, any case. What follows the colon is [namedRef]'s to judge.
var deliveredLine = regexp.MustCompile(
	`(?im)^[ \t]*(?:[-*+][ \t]+|\d+[.)][ \t]+)?[*_]*delivered[*_]*[ \t]*:[*_]*[ \t]*(.*)$`)

// namedRefs is every ref a text names on a `Delivered:` line, in its order.
func namedRefs(text string) []string {
	var refs []string
	for _, m := range deliveredLine.FindAllStringSubmatch(text, -1) {
		if ref, ok := namedRef(m[1]); ok {
			refs = append(refs, ref)
		}
	}
	return refs
}

// namedRef is the ref a `Delivered:` line holds after its label, if it holds
// one: a URL, or a name git accepts for a branch ([sandbox.ValidBranch]),
// unwrapped from the backticks, emphasis or angle brackets a report puts
// around it and from the full stop a sentence puts after it.
//
// ALONE ON ITS LINE, because the label is not proof of a ref: "Delivered: the
// fix for the flake" is a sentence, and its first word is no branch anybody
// pushed. And NEVER A URL CARRYING A CREDENTIAL — that is a remote an agent
// pasted, not a ref to show anybody.
func namedRef(rest string) (string, bool) {
	ref := strings.TrimSuffix(strings.TrimSpace(rest), ".")
	ref = strings.Trim(ref, "`*")
	if strings.HasPrefix(ref, "<") && strings.HasSuffix(ref, ">") {
		ref = ref[1 : len(ref)-1]
	}
	ref = strings.TrimSuffix(ref, ".")
	if ref == "" || strings.ContainsFunc(ref, unicode.IsSpace) {
		return "", false
	}
	if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
		return ref, u.User == nil
	}
	return ref, sandbox.ValidBranch(ref)
}

// prPattern is a pull or merge request's URL by its SHAPE, on any host: the
// fallback for an account that names no ref on a line of its own.
//
// THE HOST IS NOT THE ENGINE'S TO KNOW (see [deliveredRefs]), so what is
// matched is the path every forge spells one with — `/pull/N` (GitHub, GitHub
// Enterprise), `/pulls/N` (Gitea, Forgejo), `/-/merge_requests/N` (GitLab),
// `/pull-requests/N` (Bitbucket), `/pullrequest/N` (Azure DevOps) — after any
// host, with or without a port, over http or https. Never a host carrying
// credentials (`https://user:token@…`), which is a remote, not a ref. The
// number ends at a word boundary, so `/pull/12` is not read out of `/pull/12abc`.
//
// A GUESS, and the reason it is the fallback: a URL the run only referenced —
// the pull request it followed, the one it was asked to fix — reads exactly
// like one it opened, which is why a report that names its refs is never
// scraped. And it can never match a branch.
var prPattern = regexp.MustCompile(
	`https?://[^\s/?#@:]+(?::\d+)?/[\w.\-/]+/(?:pull|pulls|pull-requests|pullrequest|merge_requests)/\d+\b`)

// CLI is what one coding agent contributes on top of the shared plumbing.
//
// WHAT IS THE CLI'S, AND ONLY THAT: its invocation, its config file, where its
// stdout lands and how its result is read back ([Output]), how its event
// stream is decoded ([Decoder]), how its result is parsed, and whether its
// stream can say it is done before its process exits. Everything else — where
// the markers go, how the box is torn down, how a question is signalled, how
// a stream is read and how much of one is kept — is the same for every CLI,
// and a runner that could override it would be a second implementation of the
// completion protocol.
type CLI interface {
	// Name is the coding agent's config name: "claude-code", "opencode".
	Name() string

	// Command builds the headless invocation. The brief is already final —
	// the report instruction is appended by the base.
	Command(req sandbox.RunRequest, paths Paths, configPath string) string

	// Output is where the CLI's stdout lands and how its result is read
	// back — DECLARED, so the shared wrapper and the shared reads follow it
	// rather than knowing which CLI they are driving.
	Output(paths Paths) Output

	// Events is a new decoder for one of this CLI's event streams, read
	// whole or followed across many reads ([Decoder]). Asked only when
	// [Output.Events] is set.
	Events() Decoder

	// Parse maps this agent's RESULT, read whole, onto a result: the file
	// [Output.Result] names, or — for a CLI whose result is its stream —
	// the stream's whole text, decoded.
	Parse(result string) sandbox.Result

	// WriteConfig renders the agent's config file into the box and returns
	// the path the CLI is pointed at, or "" when there is nothing to write.
	WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error)

	// Finished reports whether the END of the stream says the agent has done
	// its work, for one that finishes but never exits: lines is the stream's
	// last complete lines. Asked only when [Output.Terminal] is set.
	Finished(lines string) bool
}

// Output is where a CLI's stdout lands in the box, and how it is read back.
type Output struct {
	// Stdout is the file the wrapper redirects the CLI's stdout into.
	Stdout string

	// Events says Stdout is an EVENT STREAM, decoded a line at a time by
	// [CLI.Events] — never read whole, because it grows with every tool
	// call the run makes.
	Events bool

	// Terminal says the stream's end can show the agent has finished
	// though its process has not exited, so the poll reads that end
	// ([CLI.Finished]). A CLI that exits cleanly leaves it unset and the
	// done marker is its only signal — and the poll reads nothing more.
	Terminal bool

	// Result is the file the CLI's RESULT is read from, whole ([CLI.Parse]),
	// or "" when the result is the stream's own. When it names a file other
	// than Stdout, the wrapper writes it after the CLI exits: the stream's
	// last line, which is where a CLI that streams puts its result — and
	// where the wrapper died before it could, the collection reads that line
	// from the stream in its place.
	Result string
}

// Runner drives one CLI through the detached lifecycle.
type Runner struct {
	cli CLI

	// failureBound is how much of a run's error stream, from its END, a
	// collection reads, and the most a failure it composes may be:
	// [sandbox.MaxFailureBytes] in every runner [New] builds.
	//
	// The error stream becomes the failure's detail, which the resumed
	// executor acts on and which the coordinator condenses past the record's
	// bound — and the whole failure a runner composes is held to what one
	// condensation can take beside the coordinator's own prefix. A longer
	// read is text no model could be shown, so it is not read; what was left
	// unread is said by size where it was. From the END, because a process's
	// conclusion — the line naming what broke — is the last thing it prints.
	//
	// THIS IS THE READ, NOT WHAT THE FAILURE CARRIES OF IT. The failure opens
	// with the engine's own sentences — a piece it could not read, the exit
	// status, the CLI's error — and the error stream gets what is left of the
	// bound after them ([failureDetail]). Read to the bound and carried whole
	// behind them, as it was, a failure built around a long stderr was always
	// a few hundred bytes past what the compactor reads, refused before a
	// model was asked.
	//
	// A FIELD rather than the constant at each use so the suite can stage a
	// failure past it in kilobytes: at two mebibytes every such case built,
	// redacted and measured megabytes of error stream, two to four times.
	failureBound int

	// lineBound is the longest line of a run's event stream the runner
	// reads: [maxLineBytes] in every runner New builds, a field for the
	// failure bound's reason.
	lineBound int
}

var _ sandbox.Runner = (*Runner)(nil)

// New wraps a CLI in the shared plumbing.
func New(cli CLI) *Runner {
	return &Runner{cli: cli, failureBound: sandbox.MaxFailureBytes, lineBound: maxLineBytes}
}

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
	out := r.cli.Output(p)
	targets := []string{p.Done(), p.ExitCode(), p.Result(), p.Findings(), p.Ask()}
	// The stream too, where it lives apart from the result: it is only
	// truncated when the next job starts, and a peek between the clear and
	// the start would show the previous job's stream as this one's.
	if out.Stdout != p.Result() {
		targets = append(targets, out.Stdout)
	}
	quoted := make([]string, len(targets))
	for i, path := range targets {
		quoted[i] = shellQuote(path)
	}
	if _, err := box.Exec(ctx, "rm -f "+strings.Join(quoted, " "), sandbox.ExecOptions{}); err != nil {
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
	script := wrapperScript(inner, r.cli.Output(paths), paths)

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
	log.InfoContext(ctx, "coding_agent_started", "agent", r.cli.Name(), "pid", pid)
	return sandbox.RunHandle{CommandID: pid}, nil
}

// wrapperScript is the shell line a job runs under: the CLI with its stdout
// and stderr redirected, then its result, its exit code and its done marker.
//
// THE EXIT STATUS IS THE CLI'S OWN. It is read into code before anything else
// runs, so the result line a streaming CLI's wrapper copies out afterwards
// cannot replace it — and it is copied after the CLI EXITS rather than piped
// beside it, because a pipe's status is its last command's under a plain
// `sh`, which has no pipefail to say otherwise.
func wrapperScript(inner string, out Output, paths Paths) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s < /dev/null > %s 2> %s; code=$?; ",
		inner, shellQuote(out.Stdout), shellQuote(paths.Err()))
	if out.Result != "" && out.Result != out.Stdout {
		fmt.Fprintf(&b, "tail -n 1 %s > %s; ", shellQuote(out.Stdout), shellQuote(out.Result))
	}
	fmt.Fprintf(&b, "echo $code > %s; echo $code > %s",
		shellQuote(paths.ExitCode()), shellQuote(paths.Done()))
	return b.String()
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

// terminalWindow is how much of the end of a stream the poll reads to find a
// CLI's terminal event ([CLI.Finished]).
//
// The terminal event is the stream's LAST line by construction — the CLI
// prints it when the session stops and prints nothing after it — and is a few
// hundred bytes (a step's finish reason, a session going idle), or an error
// event carrying a provider's message, which a provider's HTML error page can
// stretch to tens of KiB. 256 KiB holds that whole with room to spare, at one
// ranged read per running box per poll tick; the whole stream it replaced was
// tens of MiB on a long run, re-read and re-decoded every fifteen seconds.
const terminalWindow = 256 << 10

// Poll reports whether the detached job has finished.
//
// THREE SIGNALS, because a finished, hung or dead job must not be able to
// wedge the run — and there is no run-time TTL to fall back on, since the
// waiter keeps the box alive for exactly as long as the job needs:
//
//  1. THE DONE MARKER. The wrapper returned, cleanly or not, and wrote its
//     exit code.
//  2. A TERMINAL SIGNAL AT THE END OF THE STREAM, for an agent that finishes
//     its work but never exits — an open file watcher or an MCP subprocess
//     keeps its event loop alive, so the shell never reaches the marker
//     write. The keepalive holds the box open so the terminal event lands and
//     Collect reads it. Only the stream's END is read ([terminalWindow]),
//     and only for a CLI that declares its stream can say so.
//  3. PROCESS LIVENESS. The wrapper is gone yet no marker was written: the
//     whole process group died abnormally, before the tail echo ran. Without
//     this the run hangs forever; with it, Collect surfaces the partial result
//     and the failure. The box answers it ([sandbox.Sandbox.JobRunning]),
//     because only the backend knows whether its handle is a pid in the box's
//     own namespace or a host pid the kernel may since have handed to a
//     stranger. The wrapper is the right process to ask about: while the
//     coding agent runs, or has finished but not exited, the wrapper is alive,
//     and once it exits it has already written the done marker, which is
//     checked first.
//
// A READ THAT FAILS NEVER SKIPS THE LIVENESS PROBE. It used to return before
// it: a stdout the whole read refused made a finished-but-hung or dead job
// poll as still running for good, with its keepalive refreshed every tick and
// its seat busy until a person stopped it. The read's error is still reported
// — beside a "not done" — once the probe has had its say.
//
// A still-alive wrapper reports NOT DONE, whether it is working or hung: a
// genuinely hung-but-alive process is indistinguishable from a working one
// without a timer, and imposing one is exactly what this design refuses.
func (r *Runner) Poll(ctx context.Context, box sandbox.Sandbox, handle sandbox.RunHandle) (bool, error) {
	paths := PathsFor(box)
	out := r.cli.Output(paths)
	marker, readErr := box.ReadFile(ctx, paths.Done())
	if readErr == nil && len(marker) > 0 {
		return true, nil
	}
	if readErr == nil && out.Terminal {
		tail, err := box.ReadTail(ctx, out.Stdout, terminalWindow)
		if err != nil {
			readErr = err
		} else if lines, _ := tail.Lines(); len(lines) > 0 && r.cli.Finished(string(lines)) {
			return true, nil
		}
	}
	if handle.CommandID != "" {
		alive, err := box.JobRunning(ctx, handle.CommandID)
		// An unreadable liveness probe is not proof of death, and declaring
		// the run over on one would collect a partial result from a job
		// that is still working. The next tick asks again.
		if err == nil && !alive {
			log.WarnContext(ctx, "coding_agent_process_gone",
				"agent", r.cli.Name(), "pid", handle.CommandID)
			return true, nil
		}
	}
	return false, readErr
}

// Collect reads the finished job's result out of the box.
//
// THE STREAMS ARE READ AS STREAMS. The event stream is decoded a line at a
// time in bounded memory ([eachLine]), and the error stream is read from its
// end ([Runner.failureBound]); neither is refused for its size, because a run
// is never lost to the size of its own log. The pieces meant to be read WHOLE
// — the report, the question, the result line, the exit code — are, and one
// past [sandbox.MaxFileBytes] degrades ONLY ITSELF: it is described by its size,
// the run reads as not succeeded with that as the reason, and the run's
// tokens, refs and transcript are still collected, charged and published. A
// refusal used to fail the whole collection, which settled the run as
// unreachable and took its charge and its record with it. So does ANY piece,
// a stream included, whose path the box refuses as not a regular file
// ([sandbox.ErrNotRegularFile]): what a path names is the box's own doing, and
// retrying the collection would only read it again.
//
// A READ THAT FAILS is an error, as it always was: that is a box that could
// not be read back, not a piece that was too large or was not a file.
//
// EVERYTHING IS REDACTED WHOLE before anything is bounded: every file here
// came out of a box whose environment holds the seat's credentials, and a
// secret straddling a cut survives as a fragment the pattern no longer
// recognises. The report, the failure detail and the transcript all leave
// here WHOLE: the coordinator holds each to the record's bound — condensing
// the two a model acts on, keeping the transcript's start and end in whole
// lines — in one place ([sandbox.MaxRunTextBytes]).
func (r *Runner) Collect(ctx context.Context, box sandbox.Sandbox, handle sandbox.RunHandle) (sandbox.Result, error) {
	paths := PathsFor(box)
	out := r.cli.Output(paths)
	var refused []string

	var (
		result sandbox.Result
		last   streamLast
	)
	if out.Events {
		streamed, end, refusal, err := r.decodeStream(ctx, box, out.Stdout)
		if err != nil {
			return sandbox.Result{}, err
		}
		if refusal != "" {
			refused = append(refused, refusal)
		}
		result, last = streamed, end
	}
	if out.Result != "" {
		raw, refusal, err := readWhole(ctx, box, out.Result, "the result its CLI printed")
		if err == nil && out.Events && out.Result != out.Stdout && last.said() &&
			(refusal != "" || strings.TrimSpace(raw) == "") {
			// THE STREAM'S LAST LINE IS WHAT THE RESULT FILE HOLDS: the
			// wrapper copies it there once the CLI has exited. So where the
			// file has nothing to give — a run whose process group died (an
			// OOM kill, a host restart) left it empty, and Parse("") called
			// a run that streamed a whole session "a run that produced no
			// output"; or the box refused it, and the run's spend went
			// uncharged — the line is read in its place: a result message
			// the CLI did print, or whatever it was saying when it stopped,
			// which Parse names for what it is. A refused file is still said:
			// the run did not report as it was asked to.
			//
			// ONLY WHERE THE STREAM SAID SOMETHING: the wrapper writes the
			// file whatever the CLI did, so a CLI that exited at once leaves
			// it empty beside an empty stream, and there is nothing to read
			// in its place — nor anything to say about it beside the run's
			// real failure.
			why := "was empty"
			if refusal != "" {
				refused = append(refused, refusal)
				why = "was refused"
			}
			raw, refusal = last.resultLine(out.Stdout)
			log.WarnContext(ctx, "coding_agent_result_from_stream", "agent", r.cli.Name(),
				"detail", "the result file "+why+", so the event stream's last line, which the "+
					"wrapper copies into it, was read in its place")
		}
		switch {
		case err != nil:
			return sandbox.Result{}, err
		case refusal != "":
			refused = append(refused, refusal)
		default:
			parsed := r.cli.Parse(raw)
			if out.Events {
				parsed.Transcript = result.Transcript
			}
			result = parsed
		}
	}

	// The transcript is the observability surface for an agent that emits no
	// telemetry of its own. The parser may have built one from streamed
	// events; otherwise the error stream is it. Read once, reused below for
	// the failure detail.
	errTail, err := box.ReadTail(ctx, paths.Err(), r.failureBound+redactContext)
	if refusal := notRegularPiece(err, paths.Err(), "the error stream"); refusal != "" {
		refused = append(refused, refusal)
		errTail, err = sandbox.FileTail{}, nil
	}
	if err != nil {
		return sandbox.Result{}, fmt.Errorf("codingagent: reading the error stream: %w", err)
	}
	stderr, unread := streamEnd(errTail, r.failureBound)
	stderr = strings.TrimSpace(stderr)
	if result.Transcript == "" && stderr != "" {
		result.Transcript = stderr
		if unread > 0 {
			result.Transcript = unreadNote(unread, false) + "\n" + stderr
		}
	}

	codeText, refusal, err := readWhole(ctx, box, paths.ExitCode(), "its exit status")
	if err != nil {
		return sandbox.Result{}, err
	}
	if refusal != "" {
		refused = append(refused, refusal)
	}
	code := strings.TrimSpace(codeText)
	crashed := code != "" && code != "0"

	// THE FINDINGS FILE IS THE RESULT CARRIER OF RECORD. The brief asks the
	// agent to write its report there before stopping, and it survives both
	// a finished-but-never-exited run whose streamed message was lost and a
	// tool-only run that parses to no text — so it wins for the result text,
	// and its presence is the success signal unless the process crashed.
	findingsText, refusal, err := readWhole(ctx, box, paths.Findings(), "the report the coding agent wrote")
	if err != nil {
		return sandbox.Result{}, err
	}
	if refusal != "" {
		refused = append(refused, refusal)
	}
	findings := strings.TrimSpace(findingsText)
	// What the failure says after any piece that could not be read: the
	// CLI's own account where the run reported something, and where it
	// produced nothing at all, the exit status, the CLI's error and the
	// error stream.
	var said []string
	errStream := ""
	switch {
	case findings != "":
		// THE REFS ARE THE REPORT'S BEFORE THE MESSAGE'S, read by one rule
		// over both ([deliveredRefs]) — the message being what the CLI's
		// Parse already read its own refs from, by the same rule — so a
		// report that names its refs is never outranked by a pull request
		// the message only mentions, nor a message that names its refs by
		// one the report only mentions.
		result.DeliveredRefs = deliveredRefs(findings, result.Text)
		result.Text = findings
		if !crashed {
			result.Success = true
			result.Error = ""
		}
		said = append(said, result.Error)
	case !result.Success && strings.TrimSpace(result.Text) == "":
		// No report AND nothing parsed: the job produced nothing. Surface
		// the exit status, the CLI's own error and the stderr — ALL THREE,
		// in that order — so the completion reports a real failure rather
		// than a silent stall. The CLI's error used to be REPLACED by the
		// stderr whenever the stderr held anything at all, so a run that
		// hit its turn cap and printed one warning reported the warning.
		if crashed {
			said = append(said, "the coding agent exited with status "+code)
		}
		said = append(said, result.Error)
		errStream = stderr
	default:
		said = append(said, result.Error)
	}

	result, refusal, err = r.overlayAsk(ctx, box, result)
	if err != nil {
		return sandbox.Result{}, err
	}
	if refusal != "" {
		refused = append(refused, refusal)
	}
	if len(refused) > 0 {
		// A PIECE THAT COULD NOT BE READ IS A RUN THAT DID NOT FULLY REPORT,
		// so it does not read as a success whatever else it said — and the
		// reason is the first thing its failure says, because it is the one
		// a reader cannot find anywhere else.
		result.Success = false
	}
	result.Error = failureDetail(append(refused, said...), errStream, unread, r.failureBound)

	result.Text = redact.Secrets(result.Text)
	result.Error = redact.Secrets(result.Error)
	// The refs too: a ref is whatever word the report put on its line,
	// and a word from inside the box can be a credential.
	for i, ref := range result.DeliveredRefs {
		result.DeliveredRefs[i] = redact.Secrets(ref)
	}
	// WHOLE, as the report and the failure leave here: the coordinator is
	// the one home of the record's bound, and holds the transcript to it
	// after redacting it again ([sandbox.MaxRunTextBytes]).
	result.Transcript = redact.Secrets(result.Transcript)
	return result, nil
}

// failureDetail composes a run's failure: the engine's own sentences in the
// order given — a piece it could not read, the exit status, the CLI's error —
// then as much of the error stream's END as is left of bound — the runner's
// [Runner.failureBound], [sandbox.MaxFailureBytes] — after them, with what was
// not shown of it said by size. unread is how much of the stream's start the
// read itself left.
//
// HELD TO THE BOUND AS A WHOLE, AND EXACTLY, because the whole failure is what
// the coordinator condenses, and the compactor refuses one past what it reads
// before any model is asked. The sentences are taken as they are — they are
// what nobody can find anywhere else — and only the error stream gives way.
//
// MEASURED AS IT LEAVES, REDACTED. A marker can be longer than the credential
// it replaces, and a credential's name closing one piece and its value opening
// the next — across the `:\n` between them — is one match neither piece
// holds; measured piece by piece, the failure grew past its bound when the
// whole was redacted. So the composition is redacted whole and measured, and
// what is over comes off the stream's share. The room the stream is given
// also holds the mark [sandbox.KeepEnd] puts on a single line too long to keep
// whole, which is not part of what it counts.
func failureDetail(sentences []string, errStream string, unread int64, bound int) string {
	var parts []string
	for _, s := range sentences {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	head := redact.Secrets(strings.Join(parts, ":\n"))
	if errStream == "" {
		return head
	}
	sep := ""
	if head != "" {
		sep = ":\n"
	}
	// The note's widest form, so the one it gets always fits the room.
	room := bound - len(head) - len(sep) -
		(len(unreadNote(math.MaxInt64, false)) + len("\n")) - len(sandbox.KeepEndMark)
	for {
		kept, at := "", len(errStream)
		if room > 0 {
			kept, at = sandbox.KeepEnd(errStream, room)
		}
		if left := unread + int64(at); left > 0 {
			if kept == "" {
				kept = unreadNote(left, true)
			} else {
				kept = unreadNote(left, false) + "\n" + kept
			}
		}
		out := redact.Secrets(head + sep + kept)
		// Over only by what redaction added at a join, which the stream's
		// share gives back. The room shrinks on every pass, so this ends —
		// at the latest where the stream is given none, and what is left is
		// the engine's own account, which is never cut here.
		if len(out) <= bound || room <= 0 {
			return out
		}
		room -= len(out) - bound
	}
}

// unreadNote is the line standing where the start of the error stream was
// left out of a failure: none says nothing of it was shown at all.
func unreadNote(unread int64, none bool) string {
	if none {
		return fmt.Sprintf("(the error stream, %s, is not shown: the engine's own account of the "+
			"run fills what one condensation can take)", humanSize(unread))
	}
	return fmt.Sprintf("(the error stream's first %s were not read: a run's failure is read from "+
		"its end, as much of it as one condensation can take beside the engine's own account)",
		humanSize(unread))
}

// decodeStream reads one event stream through a fresh decoder, and keeps its
// last line — or answers a refusal where the box would not open the stream
// because it is not a regular file ([notRegularPiece]).
func (r *Runner) decodeStream(ctx context.Context, box sandbox.Sandbox, path string) (sandbox.Result, streamLast, string, error) {
	stream, err := box.OpenFile(ctx, path)
	if refusal := notRegularPiece(err, path, "the event stream"); refusal != "" {
		return sandbox.Result{}, streamLast{}, refusal, nil
	}
	if err != nil {
		return sandbox.Result{}, streamLast{}, "", fmt.Errorf("codingagent: opening the event stream: %w", err)
	}
	defer func() { _ = stream.Close() }()
	dec := &keepingLast{Decoder: r.cli.Events()}
	if err := eachLine(stream, dec, r.lineBound); err != nil {
		return sandbox.Result{}, streamLast{}, "", fmt.Errorf("codingagent: reading the event stream: %w", err)
	}
	return dec.Result(), dec.last, "", nil
}

// streamLast is an event stream's last line, as `tail -n 1` would copy it
// into a result file: the line itself, or the size of one too long to read
// and the bound it was past.
type streamLast struct {
	line    []byte
	skipped int64
	bound   int
}

// said reports whether the stream ended on anything at all: a line, or one
// too long to read.
func (l streamLast) said() bool { return len(l.line) > 0 || l.skipped > 0 }

// resultLine is the stream's last line read in place of the result file: the
// line, or a refusal where it was past what one line of a stream may hold — as
// the copy itself would have been past what a result file is read to.
func (l streamLast) resultLine(stream string) (string, string) {
	if l.skipped > 0 {
		return "", fmt.Sprintf("the result its CLI printed (the last line of %s, read in place of "+
			"the result file) is %s, past the %s one line of a run's output may hold, "+
			"so it was not read", stream, humanSize(l.skipped), humanSize(int64(l.bound)))
	}
	return string(l.line), ""
}

// keepingLast is a decoder that also keeps the stream's last non-blank line,
// in one buffer it reuses, so what it holds is one line however long the
// stream.
type keepingLast struct {
	Decoder
	last streamLast
}

func (k *keepingLast) Line(line []byte) {
	if len(bytes.TrimSpace(line)) > 0 {
		k.last.line = append(k.last.line[:0], line...)
		k.last.skipped = 0
	}
	k.Decoder.Line(line)
}

func (k *keepingLast) Skipped(n int64, bound int) {
	k.last.line, k.last.skipped, k.last.bound = k.last.line[:0], n, bound
	k.Decoder.Skipped(n, bound)
}

// readWhole reads a file meant to be read whole, answering a file past
// [sandbox.MaxFileBytes], or a path that is not a regular file, with a
// REFUSAL — a sentence saying what it was — rather than an error, so the piece
// degrades and the collection does not.
func readWhole(ctx context.Context, box sandbox.Sandbox, path, what string) (string, string, error) {
	raw, err := box.ReadFile(ctx, path)
	if refusal := notRegularPiece(err, path, what); refusal != "" {
		return "", refusal, nil
	}
	var tooLarge *sandbox.FileTooLargeError
	switch {
	case errors.As(err, &tooLarge):
		return "", refusedPiece(ctx, box, path, what, tooLarge.Limit), nil
	case err != nil:
		return "", "", err
	}
	return string(raw), "", nil
}

// notRegularPiece describes a piece whose path the box refused because it is
// not a regular file ([sandbox.ErrNotRegularFile]), or "" for any other
// answer.
//
// A REFUSAL RATHER THAN AN ERROR, because what the path names is the box's
// own doing and a retry reads it again: an error would have the collection
// retried until the run was settled as lost, uncharged and unrecorded — for a
// pipe the agent made where its report goes.
func notRegularPiece(err error, path, what string) string {
	var notRegular *sandbox.NotRegularFileError
	if !errors.As(err, &notRegular) {
		return ""
	}
	return fmt.Sprintf("%s (%s) %s", what, path, notRegular.Reason())
}

// refusedPiece describes a file the engine would not read whole, by its size
// and the cap the box refused it at — [sandbox.MaxFileBytes] on every box the
// engine builds, read off the refusal rather than restated here.
func refusedPiece(ctx context.Context, box sandbox.Sandbox, path, what string, limit int) string {
	size := "past " + humanSize(int64(limit))
	if end, err := box.ReadTail(ctx, path, 0); err == nil && end.Size > 0 {
		size = humanSize(end.Size)
	}
	return fmt.Sprintf("%s (%s) is %s, past the %s the engine reads back from a box whole, "+
		"so it was not read", what, path, size, humanSize(int64(limit)))
}

// overlayAsk surfaces a question the shim recorded, if there is one, and a
// refusal where the question file was too large to read whole.
func (r *Runner) overlayAsk(ctx context.Context, box sandbox.Sandbox, result sandbox.Result) (sandbox.Result, string, error) {
	blob, refusal, err := readWhole(ctx, box, PathsFor(box).Ask(), "the question the coding agent recorded")
	if err != nil || refusal != "" {
		// A question nobody can read whole is not one to park on: the run
		// is not waiting for an answer to something nobody was shown.
		return result, refusal, err
	}
	blob = strings.TrimSpace(blob)
	if blob == "" {
		return result, "", nil
	}
	var ask struct {
		Question string `json:"question"`
		To       string `json:"to"`
		Branch   string `json:"branch"`
	}
	if err := json.Unmarshal([]byte(blob), &ask); err != nil {
		// A malformed signal is not a reason to lose the result the run
		// did produce; the run reads as finished rather than as parked.
		log.WarnContext(ctx, "coding_agent_ask_unreadable", "agent", r.cli.Name(), "error", err.Error())
		return result, "", nil
	}
	if ask.Question == "" {
		return result, "", nil
	}
	result.NeedsInput = true
	result.Question = redact.Secrets(ask.Question)
	result.AskTo = ask.To
	if result.AskTo == "" {
		result.AskTo = "requester"
	}
	// The branch as the ask recorded it, redacted like the question: it is
	// a word from inside the box. Whether it is a branch at all, and short
	// enough to carry, is the coordinator's to decide for every runner
	// alike ([sandbox.MaxBranchBytes]).
	result.WIPBranch = redact.Secrets(strings.TrimSpace(ask.Branch))
	return result, "", nil
}
