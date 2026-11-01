package executor

import (
	"bytes"
	"context"
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

var log = logging.Get("sandbox.executor")

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
// stream is decoded ([Decoder]) and how its result is parsed. Everything else
// — when a run has ended included, which is the done marker for every CLI —
// where
// the markers go, how the box is torn down, how a question is signalled, how
// a stream is read and how much of one is kept — is the same for every CLI,
// and a runner that could override it would be a second implementation of the
// completion protocol.
type Harness interface {
	// Name is the coding agent's config name: "claude-code", "opencode".
	Name() string

	// Command builds the headless invocation. The brief and the system text
	// are already final — the report instruction is appended to the system
	// text by the base — but for the instruction about notes, which a CLI
	// that reads them adds where a note can reach its run ([noteReader]); and
	// a CLI given system text puts it on the channel that adds to its own
	// system prompt, never in the prompt it runs.
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
	// It also writes the system text where the CLI reads it from
	// ([Paths.System]) when there is any, and nothing when there is none.
	WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error)
}

// resumer is a CLI that continues a session an earlier run reported.
type resumer interface{ Resumes() bool }

// sessionTotals is a CLI whose result, on a run that continues a session, can
// state the SESSION's spend rather than the run's own: from the release
// TotalsRestored names, the CLI starts a continued session from the totals
// the job before it saved, and every result after counts them again. The
// runner keeps that job's result as the run starts ([Paths.BaseResult]) and
// charges a continued run the difference — unless its own stream stated what
// the run spent ([runSpender]), which is exact on any release that says it.
//
// OPTIONAL, as [resumer] is: a CLI whose result is the run's own spend on
// every release needs nothing kept, and OpenCode measures its session before
// and after the run in the run's own script instead ([openCodeUsageBefore]).
type sessionTotals interface {
	// TotalsRestored reports whether the CLI release version, as its
	// `--version` printed it, restores a continued session's totals. A
	// version that cannot be read is taken for a current release.
	TotalsRestored(version string) bool
}

// runSpender is a decoder that read the run's OWN spend off its stream: the
// figures its CLI states for the calls this process made, which nothing a
// resume or a kill does to the session's totals can add to.
type runSpender interface {
	// runSpend is that spend, and whether the stream stated any.
	runSpend() (runSpend, bool)
}

// runSpend is what one run spent, as its CLI stated it: the prompt not served
// from the cache, the cache's reads and writes, the reply, and the price.
type runSpend struct {
	input, cacheRead, cacheWrite, output int
	costUSD                              float64
}

// on is result charged s: the whole prompt, its cached shares beside it, the
// reply and the price — the convention every engine meter charges
// ([sandbox.Result.InputTokens]).
func (s runSpend) on(result sandbox.Result) sandbox.Result {
	result.CacheReadTokens = s.cacheRead
	result.CacheWriteTokens = s.cacheWrite
	result.InputTokens = s.input + s.cacheRead + s.cacheWrite
	result.OutputTokens = s.output
	result.CostUSD = s.costUSD
	return result
}

// less is result with what base already stated taken off: a continued run's
// own spend, where its CLI stated the session's. A difference below zero is
// passed on as it is, for the coordinator to floor and say
// ([sandbox.Result]'s floor, sandbox_usage_negative), as every negative count a
// CLI reports is. The turn count is the process's own on every release, so it
// is never a difference.
func less(result, base sandbox.Result) sandbox.Result {
	result.InputTokens -= base.InputTokens
	result.OutputTokens -= base.OutputTokens
	result.CacheReadTokens -= base.CacheReadTokens
	result.CacheWriteTokens -= base.CacheWriteTokens
	result.CostUSD -= base.CostUSD
	return result
}

// noteReader is a CLI that takes a person's notes to its run's turn between its
// own steps, where its launch wires it to them ([sandbox.RunRequest.Steer]),
// and tells its model so: NoteInstruction is the standing instruction about
// notes its runs are given ([ClaudeCodeSteerInstruction],
// [OpenCodeSteerInstruction]) — the CLI's own, because where a note appears in
// what the model reads is the CLI's to decide, and the CLI adds it to its run's
// system text itself ([withNoteInstruction]), on the run its notes can reach.
//
// OPTIONAL, as [resumer] is, because a CLI is taught to read notes in its own
// way — hooks, a plugin — and one not taught yet must not be claimed to: the
// instruction, and the warning that a run never asked for any, both follow it.
type noteReader interface{ NoteInstruction() string }

// Output is how the wrapper connects a CLI's standard streams in the box —
// where its stdout lands, what its stdin reads — and how its result is read
// back.
type Output struct {
	// Stdout is the file the wrapper redirects the CLI's stdout into.
	Stdout string

	// Events says Stdout is an EVENT STREAM, decoded a line at a time by
	// [CLI.Events] — never read whole, because it grows with every tool
	// call the run makes.
	Events bool

	// Result is the file the CLI's RESULT is read from, whole ([CLI.Parse]),
	// or "" when the result is the stream's own. When it names a file other
	// than Stdout, the wrapper writes it after the CLI exits: the stream's
	// last line, which is where a CLI that streams puts its result — and
	// where the wrapper died before it could, the collection reads that line
	// from the stream in its place.
	Result string

	// BriefOnStdin says the CLI reads its brief on STDIN, from a file Start
	// writes ([Paths.Brief]), rather than on its argv — for a CLI that does
	// not take an argv prompt verbatim. OpenCode re-quotes one: a brief
	// passed as an argument reached the model wrapped in double quotes with
	// every quote inside it escaped, so code and quoted text in a brief came
	// out altered (measured on 1.18.35 and 2.0.32 alike), while the same
	// brief on stdin arrived byte for byte. Unset, the CLI's stdin is
	// /dev/null, so a headless agent can never block waiting for input.
	BriefOnStdin bool

	// Unset is every variable the wrapper removes from the CLI's environment
	// before it starts: one that, from wherever it came — the seat's
	// role.sandbox.env, the image, a profile the login shell sources —
	// switches off something the run depends on. Removed IN the wrapper,
	// after the login shell has sourced its profile, because that is the
	// one place every source has already been read.
	Unset []string
}

// Runner drives one CLI through the detached lifecycle.
type Runner struct {
	harness Harness

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
func New(harness Harness) *Runner {
	return &Runner{harness: harness, failureBound: sandbox.MaxFailureBytes, lineBound: maxLineBytes}
}

// Name is the coding agent's config name.
func (r *Runner) Name() string { return r.harness.Name() }

// ReadsNotes reports whether this runner's CLI takes a person's notes between
// its steps — see [noteReader]. The coordinator asks it before it says a run
// that succeeded never asked for its notes (internal/sandbox).
func (r *Runner) ReadsNotes() bool { return r.noteInstruction() != "" }

// noteInstruction is the standing instruction about notes this runner's CLI
// gives its runs, or "" for a CLI that takes none — see [noteReader].
func (r *Runner) noteInstruction() string {
	if reader, ok := r.harness.(noteReader); ok {
		return reader.NoteInstruction()
	}
	return ""
}

// Install prepares the box: the artefact directory every file of a run's is
// written under ([Paths.WorkDir]), and the box's own bin directory first on
// the run's PATH ([Paths.BinDir]).
//
// The CLI itself is NOT installed here — it ships in the image, which is what
// makes a coding box a template rather than a per-run build. Environment
// provisioning (git auth, registry credentials, toolchains) is not the
// runner's concern either: the manager applies the launch's setup steps after
// this returns.
func (r *Runner) Install(ctx context.Context, box sandbox.Sandbox) error {
	paths := PathsFor(box)
	if _, err := box.Exec(ctx, "mkdir -p "+shellQuote(paths.BinDir()), sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("executor: preparing %s: %w", paths.BinDir(), err)
	}
	return nil
}

// ClearArtifacts removes the previous run's markers before a reuse run.
//
// A reused box still carries the prior run's done marker, result and
// findings. Without clearing them the completion poll fires immediately on the
// stale marker and Collect reads the old result. Its brief, system text,
// settings and configuration go too: a run's instructions are its own — a
// previous job's hooks name a bridge session that job's end closed. (Whatever
// mode a previous run left on one, the file this run writes has the box's
// own: a box write replaces a file whole ([sandbox.Sandbox.WriteFile]).)
// OpenCode's values ([Paths.OpenCodeValues]) and its notes plugin
// ([Paths.OpenCodeSteerPlugin]) are DIRECTORIES, removed whole: a run writes
// one file per value, so one with fewer values than the last would otherwise
// keep the last one's beside its own, and a run wired to no notes would
// otherwise leave a plugin no configuration names. The commit attribution
// ([Paths.CommitAttribution]) is a directory too, removed whole: a run that
// credits nothing must leave no hook, and one that names another run must not
// credit the last one. THE CHECKOUT IS LEFT INTACT — that disk state is the
// continued context, and it is the whole reason to reuse a box.
func (r *Runner) ClearArtifacts(ctx context.Context, box sandbox.Sandbox) error {
	p := PathsFor(box)
	out := r.harness.Output(p)
	files := []string{p.Done(), p.ExitCode(), p.Result(), p.Findings(), p.Brief(), p.System(),
		p.ClaudeNotesSystem(), p.ClaudeSettings(), p.OpenCodeConfig(), p.BaseResult(), p.CLIVersion()}
	// The stream too, where it lives apart from the result: it is only
	// truncated when the next job starts, and a peek between the clear and
	// the start would show the previous job's stream as this one's.
	if out.Stdout != p.Result() {
		files = append(files, out.Stdout)
	}
	// The three directories alone are removed recursively; every other target
	// is a file, removed as it always was. Two statements rather than one
	// joined by `&&`, so a file that will not go never keeps a directory.
	cmd := "rm -f " + shellQuoteAll(files) + "; rm -rf " +
		shellQuoteAll([]string{p.OpenCodeValues(), p.OpenCodeSteerPlugin(), p.CommitAttribution()})
	if _, err := box.Exec(ctx, cmd, sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("executor: clearing the prior run's artefacts: %w", err)
	}
	return nil
}

// shellQuoteAll is paths quoted for sh, one word each, space-separated.
func shellQuoteAll(paths []string) string {
	quoted := make([]string, len(paths))
	for i, path := range paths {
		quoted[i] = shellQuote(path)
	}
	return strings.Join(quoted, " ")
}

// Start launches the coding agent detached and returns its handle.
//
// The job runs UNCAPPED: nothing force-stops it on a wall-clock timer, so a
// legitimately long run is free to finish. On a clean exit the shell writes the
// exit code into both the exit-code file and the done marker — non-empty, for
// the reason on [Paths.Done].
//
// Its stdin is closed — or, for a CLI that reads its brief there, the brief's
// file, which ends: a headless agent must never block waiting for input, and
// one that does would hang forever with no timer to stop it.
func (r *Runner) Start(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest) (sandbox.RunHandle, error) {
	if c, ok := r.harness.(resumer); req.Resume != "" && (!ok || !c.Resumes()) {
		return sandbox.RunHandle{}, fmt.Errorf("executor: %s cannot continue an earlier "+
			"session, and this run asks it to", r.harness.Name())
	}
	paths := PathsFor(box)
	// WHAT THE SESSION HAD SPENT, read before the clear removes it: the
	// result the job before this one left in the box, where this run
	// continues its session and its CLI's result may state the session's
	// spend ([sessionTotals]). Read where the job before left it — the box
	// a continued session lives in is the box that job ran in.
	base, err := r.baseResult(ctx, box, paths, req)
	if err != nil {
		return sandbox.RunHandle{}, err
	}
	// The artefacts are cleared unconditionally rather than only on a reuse
	// path: the runner cannot tell a fresh box from a reused one, and
	// clearing a fresh box's absent files costs one exec.
	if err = r.ClearArtifacts(ctx, box); err != nil {
		return sandbox.RunHandle{}, err
	}
	if base != "" {
		if err = box.WriteFile(ctx, paths.BaseResult(), []byte(base)); err != nil {
			return sandbox.RunHandle{}, fmt.Errorf("executor: keeping the session's spend so far: %w", err)
		}
	}
	// FINAL BEFORE THE CONFIG IS WRITTEN, which is where a CLI writes the
	// system text from — all but the instruction about notes, which a CLI
	// that reads them adds itself, where a note can reach its run
	// ([noteReader]).
	req.System = finalSystem(req.System, paths)
	// AND THE SERVERS AS THIS BOX DIALS THEM, for the same reason: their
	// URLs, the tool bridge's included, are written in the engine host's
	// network, and a loopback one reaches nothing from inside a container
	// ([sandbox.EngineAddress]). Here, because every run reaches its CLI's
	// config through this one call, whichever caller launched it.
	req.MCPServers = sandbox.EngineServers(box, req.MCPServers)
	// The run's notes route too: it is the same bridge under a path of its
	// own, and a CLI that polls it by URL rather than through its MCP server
	// (OpenCode's plugin, [OpenCode.WriteConfig]) dials it from inside the
	// box as well. A copy, since req is this call's own.
	if req.Steer.Set() {
		req.Steer.URL = sandbox.EngineAddress(box, req.Steer.URL)
	}
	configPath, err := r.harness.WriteConfig(ctx, box, req, paths)
	if err != nil {
		return sandbox.RunHandle{}, err
	}
	// THE CREDIT ON THE RUN'S COMMITS, where its request asks for it: the
	// hook here, and the wrapper pointing git at it ([installAttribution]).
	hooks := ""
	if req.Attribution != nil {
		if err = installAttribution(ctx, box, paths, *req.Attribution); err != nil {
			return sandbox.RunHandle{}, err
		}
		hooks = paths.GitHooks()
	}
	out := r.harness.Output(paths)
	if out.BriefOnStdin {
		if err = box.WriteFile(ctx, paths.Brief(), []byte(req.Brief)); err != nil {
			return sandbox.RunHandle{}, fmt.Errorf("executor: writing the brief: %w", err)
		}
	}
	inner := withBoxBin(r.harness.Command(req, paths, configPath), paths)
	script := wrapperScript(inner, out, paths, hooks)

	// A LOGIN SHELL, and the -l is load-bearing: a coding CLI is commonly
	// installed through nvm, asdf or a similar version manager, whose PATH
	// entries exist only in a profile a login shell sources. A plain `sh -c`
	// finds no CLI at all in exactly the images operators build.
	//
	// It also means the box's PATH inside the wrapper is the PROFILE's, not
	// the environment the engine handed the process, which is why the box's
	// bin directory is prepended INSIDE the script ([withBoxBin]) rather
	// than exported around it: an assignment made outside would be replaced
	// by the profile before the agent ever ran.
	pid, err := box.StartBackground(ctx, "sh -lc "+shellQuote(script), sandbox.ExecOptions{Env: req.Env})
	if err != nil {
		return sandbox.RunHandle{}, fmt.Errorf("executor: starting %s: %w", r.harness.Name(), err)
	}
	log.InfoContext(ctx, "coding_agent_started", "agent", r.harness.Name(), "pid", pid)
	return sandbox.RunHandle{CommandID: pid}, nil
}

// baseResult is the result the job before req left in box, for a run that
// continues its session on a CLI whose result may state the session's spend
// ([sessionTotals]), and "" for every other run.
//
// A RESULT THE BOX COULD NOT GIVE BACK IS NO BASE, and the run still starts: a
// continued session is the work the person was waiting for, and its spend is
// then the one its CLI states, logged so that an overcharge has a cause on
// record. A read that failed outright fails the start, as every other read
// of Start's does, for the launch to try again.
func (r *Runner) baseResult(ctx context.Context, box sandbox.Sandbox, paths Paths, req sandbox.RunRequest) (string, error) {
	if _, totals := r.harness.(sessionTotals); !totals || req.Resume == "" {
		return "", nil
	}
	out := r.harness.Output(paths)
	if out.Result == "" {
		return "", nil
	}
	raw, refusal, err := readWhole(ctx, box, out.Result, "the result the job before it left")
	if err != nil {
		return "", fmt.Errorf("executor: reading what the continued session had spent: %w", err)
	}
	if refusal != "" || strings.TrimSpace(raw) == "" {
		log.WarnContext(ctx, "coding_agent_session_spend_unread", "agent", r.harness.Name(),
			"detail", "this run continues a session, and the result the job before it left "+
				"could not be read, so the run is charged the spend its CLI states, which counts "+
				"the session's earlier jobs again", "refusal", refusal)
		return "", nil
	}
	return raw, nil
}

// ownSpend is result charged what THIS run spent ([sessionTotals],
// [runSpender]): the spend the run's own stream stated, where it stated any;
// else, for a run that continued a session on a CLI that restores the
// session's totals, the result less what the session had spent when the run
// took it up ([Paths.BaseResult]); else the result as its CLI stated it, which
// is the run's own.
func (r *Runner) ownSpend(ctx context.Context, box sandbox.Sandbox, paths Paths,
	result sandbox.Result, streamed *runSpend) (sandbox.Result, error) {

	if streamed != nil {
		return streamed.on(result), nil
	}
	totals, ok := r.harness.(sessionTotals)
	if !ok {
		return result, nil
	}
	raw, refusal, err := readWhole(ctx, box, paths.BaseResult(), "what the continued session had spent")
	if err != nil {
		return sandbox.Result{}, err
	}
	if refusal != "" || strings.TrimSpace(raw) == "" {
		// NO BASE: a run that started a session of its own, or one whose
		// base could not be kept ([Runner.baseResult] said so).
		return result, nil
	}
	version, _, err := readWhole(ctx, box, paths.CLIVersion(), "its CLI's version")
	if err != nil {
		return sandbox.Result{}, err
	}
	if !totals.TotalsRestored(strings.TrimSpace(version)) {
		return result, nil
	}
	return less(result, r.harness.Parse(raw)), nil
}

// wrapperScript is the shell line a job runs under: the CLI with its stdout
// and stderr redirected, then its result, its exit code and its done marker.
//
// THE EXIT STATUS IS THE CLI'S OWN. It is read into code before anything else
// runs, so the result line a streaming CLI's wrapper copies out afterwards
// cannot replace it — and it is copied after the CLI EXITS rather than piped
// beside it, because a pipe's status is its last command's under a plain
// `sh`, which has no pipefail to say otherwise.
//
// hooks is the directory the run's git takes its hooks from — the run's
// commit attribution ([attributionExports]) — or "" for a run that credits
// nothing.
func wrapperScript(inner string, out Output, paths Paths, hooks string) string {
	stdin := "/dev/null"
	if out.BriefOnStdin {
		stdin = shellQuote(paths.Brief())
	}
	var b strings.Builder
	// A STATEMENT OF ITS OWN, before the CLI's: an assignment prefix on the
	// command could only add to its environment, never remove from it.
	if len(out.Unset) > 0 {
		fmt.Fprintf(&b, "unset %s; ", strings.Join(out.Unset, " "))
	}
	// AFTER THE PROFILE, BEFORE THE CLI, for the reason the unset is: an
	// entry exported around the shell would be replaced by a profile that
	// sets the count.
	if hooks != "" {
		b.WriteString(attributionExports(hooks))
	}
	fmt.Fprintf(&b, "%s < %s > %s 2> %s; code=$?; ",
		inner, stdin, shellQuote(out.Stdout), shellQuote(paths.Err()))
	if out.Result != "" && out.Result != out.Stdout {
		fmt.Fprintf(&b, "tail -n 1 %s > %s; ", shellQuote(out.Stdout), shellQuote(out.Result))
	}
	fmt.Fprintf(&b, "echo $code > %s; echo $code > %s",
		shellQuote(paths.ExitCode()), shellQuote(paths.Done()))
	return b.String()
}

// finalSystem appends the report instruction, addressed at THIS box, to the
// run's standing instructions.
//
// Composed here rather than at the launch because the findings path is a
// property of the box the run lands in, which the caller does not know when it
// builds the request. On the system side, because it is not part of the task:
// it is how every run works, whatever it was asked.
func finalSystem(system string, paths Paths) string {
	parts := make([]string, 0, 2)
	if system = strings.TrimSpace(system); system != "" {
		parts = append(parts, system)
	}
	parts = append(parts, strings.TrimSpace(FindingsInstruction(paths.Findings())))
	return strings.Join(parts, "\n\n")
}

// withNoteInstruction is a run's final system text with its CLI's instruction
// about notes after it ([noteReader]) — what a CLI that reads notes hands a run
// wired to them, as the last of its standing instructions.
//
// THE CLI'S TO ADD, AND ONLY WHERE A NOTE CAN ARRIVE: told to a run whose CLI
// takes none, a run wired to none, or a run started without what reads them, it
// would describe a channel that never opens. Which of those a run is can be
// known only where it starts — a Claude Code run's box decides from its CLI's
// release ([claudeNotesGate]) — so the runner, which composes everything else,
// leaves this to the CLI.
func withNoteInstruction(system, instruction string) string {
	if system == "" {
		return instruction
	}
	return system + "\n\n" + instruction
}

// withBoxBin prepends the box's own bin directory ([Paths.BinDir]) to PATH for
// one command, so an executable a setup step installed there resolves ahead of
// the image's.
func withBoxBin(cmd string, paths Paths) string {
	return "PATH=" + shellQuote(paths.BinDir()) + `:"$PATH" ` + cmd
}

// Poll reports what is known of the detached job ([sandbox.JobState]).
//
// TWO SIGNALS, because a finished or dead job must not be able to wedge the
// run — and there is no run-time TTL to fall back on, since the waiter keeps
// the box alive for exactly as long as the job needs:
//
//  1. THE DONE MARKER. The wrapper returned, cleanly or not, and wrote its
//     exit code: the job ENDED.
//  2. THE BOX'S ACCOUNT OF THE JOB ([sandbox.Sandbox.Job]). The wrapper is
//     gone yet no marker was written: the whole process group died
//     abnormally, before the tail echo ran. Without this the run hangs
//     forever. The box answers it, because only the backend knows whether its
//     handle is a pid in the box's own namespace or a host pid the kernel may
//     since have handed to a stranger — and whether the job went while an
//     engine watched it, so that what it left is its result, which Collect
//     surfaces with the failure (ENDED), or went with the engine that last saw
//     it running, so that what it left is not (LOST). The wrapper is the right
//     process to ask about: while the coding agent runs the wrapper is alive,
//     and once it exits it has already written the done marker, which is
//     checked first.
//
// THE MARKER IS READ AGAIN before a job is answered lost: a wrapper that wrote
// it and exited between the first read and the box's answer finished, and is
// collected like any job that did.
//
// There was a third — a terminal event read off the end of the stream, for an
// agent that finished its work and did not exit, which was OpenCode 1.x. No
// CLI this build drives does that: Claude Code exits, and OpenCode 2 exits
// when its session does. A signal no runner gives is a branch nothing tests.
//
// A READ THAT FAILS NEVER SKIPS THE LIVENESS PROBE. It used to return before
// it: a read the box refused made a dead job poll as still running for good,
// with its keepalive refreshed every tick and its seat busy until a person
// stopped it. The read's error is still reported — beside a "running" — once
// the probe has had its say.
//
// A still-alive wrapper reports RUNNING, whether it is working or hung: a
// genuinely hung-but-alive process is indistinguishable from a working one
// without a timer, and imposing one is exactly what this design refuses.
func (r *Runner) Poll(ctx context.Context, box sandbox.Sandbox, handle sandbox.RunHandle) (sandbox.JobState, error) {
	paths := PathsFor(box)
	marker, readErr := box.ReadFile(ctx, paths.Done())
	if readErr == nil && len(marker) > 0 {
		return sandbox.JobEnded, nil
	}
	if handle.CommandID == "" {
		return sandbox.JobRunning, readErr
	}
	state, err := box.Job(ctx, handle.CommandID)
	if err != nil || !state.Over() {
		// An unreadable liveness probe is not proof of death, and declaring
		// the run over on one would collect a partial result from a job
		// that is still working. The next tick asks again.
		return sandbox.JobRunning, readErr
	}
	if state == sandbox.JobLost {
		if again, err := box.ReadFile(ctx, paths.Done()); err == nil && len(again) > 0 {
			return sandbox.JobEnded, nil
		}
	}
	log.WarnContext(ctx, "coding_agent_process_gone",
		"agent", r.harness.Name(), "pid", handle.CommandID, "job", string(state))
	return state, nil
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
	out := r.harness.Output(paths)
	var refused []string

	var (
		result sandbox.Result
		last   streamLast
		spent  *runSpend
	)
	if out.Events {
		streamed, refusal, err := r.decodeStream(ctx, box, out.Stdout)
		if err != nil {
			return sandbox.Result{}, err
		}
		if refusal != "" {
			refused = append(refused, refusal)
		}
		result, last, spent = streamed.result, streamed.last, streamed.spent
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
			log.WarnContext(ctx, "coding_agent_result_from_stream", "agent", r.harness.Name(),
				"detail", "the result file "+why+", so the event stream's last line, which the "+
					"wrapper copies into it, was read in its place")
		}
		switch {
		case err != nil:
			return sandbox.Result{}, err
		case refusal != "":
			refused = append(refused, refusal)
		default:
			parsed := r.harness.Parse(raw)
			if out.Events {
				parsed.Transcript = result.Transcript
			}
			result = parsed
		}
	}

	// WHAT THIS RUN SPENT, never its session's: a CLI that continued a
	// session can state the session's spend, and charged as it is, every
	// continued run would pay for the jobs before it again ([Runner.ownSpend]).
	result, err := r.ownSpend(ctx, box, paths, result, spent)
	if err != nil {
		return sandbox.Result{}, fmt.Errorf("executor: reading what the run itself spent: %w", err)
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
		return sandbox.Result{}, fmt.Errorf("executor: reading the error stream: %w", err)
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

	// THE FINDINGS FILE IS THE RESULT CARRIER OF RECORD. The system text asks
	// the agent to write its report there before stopping, and it survives both
	// a run whose process group died before its streamed final message was
	// written and a tool-only run that parses to no text — so it wins for the
	// result text, and its presence is the success signal unless the process
	// crashed.
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
func (r *Runner) decodeStream(ctx context.Context, box sandbox.Sandbox, path string) (decodedStream, string, error) {
	stream, err := box.OpenFile(ctx, path)
	if refusal := notRegularPiece(err, path, "the event stream"); refusal != "" {
		return decodedStream{}, refusal, nil
	}
	if err != nil {
		return decodedStream{}, "", fmt.Errorf("executor: opening the event stream: %w", err)
	}
	defer func() { _ = stream.Close() }()
	dec := &keepingLast{Decoder: r.harness.Events()}
	if err := eachLine(stream, dec, r.lineBound); err != nil {
		return decodedStream{}, "", fmt.Errorf("executor: reading the event stream: %w", err)
	}
	out := decodedStream{result: dec.Result(), last: dec.last}
	if spender, ok := dec.Decoder.(runSpender); ok {
		if spent, stated := spender.runSpend(); stated {
			out.spent = &spent
		}
	}
	return out, "", nil
}

// decodedStream is what a run's event stream said: the result as its decoder
// read it, the stream's last line, and the run's own spend where the stream
// stated it ([runSpender]), nil where it stated none.
type decodedStream struct {
	result sandbox.Result
	last   streamLast
	spent  *runSpend
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
