package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/crewlet/crewlet/internal/envfile"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

var log = logging.Get("sandbox.executor")

// deliveredRefs is what a run delivered as its own account names it — the
// branches it pushed, the pull or merge requests it opened — read from texts
// in the order given (the report before the harness's last message).
//
// The first text with `Delivered: <ref>` lines ([FindingsInstruction]) gives
// them all, in order: a branch or a URL on any host, since which hosts a box
// pushes to is the seat's setup's, never the engine's to list. Only where no
// text names one is a URL scraped by shape ([prPattern]), which cannot tell a
// pull request the run opened from one it only read. Refs stay byte for byte:
// the coordinator judges what the record can carry
// ([sandbox.Result.DeliveredRefs]), and a ref mended here would read as one
// somebody delivered.
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

// namedRef is the ref a `Delivered:` line holds after its label, if any: a URL,
// bytes as they are ([deliveredRefs]), or a branch [gitref.ValidBranch] takes,
// unwrapped from backticks, emphasis, angle brackets and a closing full stop.
// Alone on its line, since "Delivered: the fix for the flake" is a sentence,
// not a branch; never a URL carrying a credential, which is a pasted remote.
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
	return ref, gitref.ValidBranch(ref)
}

// prPattern is a pull or merge request's URL by its shape, on any host (see
// [deliveredRefs]): the fallback for an account that names no ref on a line of
// its own. It matches the path each forge uses — `/pull/N` (GitHub), `/pulls/N`
// (Gitea, Forgejo), `/-/merge_requests/N` (GitLab), `/pull-requests/N`
// (Bitbucket), `/pullrequest/N` (Azure DevOps) — over http or https, any port,
// never a host with credentials (a remote, not a ref); the number ends at a
// word boundary. Only a fallback, because a URL the run merely referenced reads
// exactly like one it opened.
var prPattern = regexp.MustCompile(
	`https?://[^\s/?#@:]+(?::\d+)?/[\w.\-/]+/(?:pull|pulls|pull-requests|pullrequest|merge_requests)/\d+\b`)

// Harness is what one harness contributes on top of the shared plumbing: its
// invocation, its config file, where its stdout lands and how its result is
// read back ([Output]), how its event stream is decoded ([Decoder]) and how its
// result is parsed. Everything else — the done marker that ends every run, the
// markers' places, teardown, question signalling, stream reading and bounds — is
// shared: a runner that could override it would be a second implementation of
// the completion protocol.
type Harness interface {
	// Name is the harness's config name: "claude-code", "opencode".
	Name() string

	// Session is the session req's run works in ([Session]), its ids in the
	// harness's own form and minted where new. Settled before the harness
	// starts, so even a run killed before it reported anything has one
	// ([sandbox.RunHandle.SessionID]).
	Session(req sandbox.RunRequest) Session

	// Command builds the headless invocation of a run that works in session.
	// The brief and system text are final (the base appended the report
	// instruction), except the notes instruction, which a harness that reads
	// notes adds where a note can reach its run ([noteReader]). System text goes
	// on the channel that adds to the harness's own system prompt, never into
	// the prompt it runs.
	Command(req sandbox.RunRequest, session Session, paths Paths, configPath string) string

	// Output is where the harness's stdout lands and how its result is read
	// back, declared so the shared wrapper and reads need not know which
	// harness they drive.
	Output(paths Paths) Output

	// Pins is every variable the wrapper sets for req's run to a value of its
	// own, OVER whatever the seat's role.sandbox.env, the image or a profile the
	// login shell sources set it to, exported after [Output.Unset] for its reason.
	Pins(ctx context.Context, req sandbox.RunRequest, paths Paths) []Pinned

	// Events is a new decoder for one of this harness's event streams, read
	// whole or followed across many reads ([Decoder]). Asked only when
	// [Output.Events] is set.
	Events() Decoder

	// Parse maps this agent's RESULT, read whole, onto a result: the file
	// [Output.Result] names, or — for a harness whose result is its stream —
	// the stream's whole text, decoded.
	Parse(result string) sandbox.Result

	// WriteConfig renders the agent's config file into the box and returns
	// the path the harness is pointed at, or "" when there is nothing to write.
	// It also writes the system text where the harness reads it from
	// ([Paths.System]) when there is any, and nothing when there is none.
	WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error)
}

// Session is the harness session one run works in, as [Runner.Start] settles it
// before the harness starts ([Harness.Session]).
type Session struct {
	// ID is the session the run works in: the one it continues
	// ([sandbox.RunRequest.Resume]), or one minted for it.
	ID string

	// Continues says ID is an earlier run's session, which the run takes up,
	// rather than one it creates.
	Continues bool

	// Fresh is, for a run that continues ID, the session it starts in ID's
	// place where its box neither holds nor can restore ID: minted beside ID so
	// that start is a new conversation under a new id, never an empty one under
	// the old id, which a harness answers as if it remembered
	// ([sandbox.Result.Continued]). "" for a run that continues nothing.
	Fresh string

	// Replaces says the carried archive replaces the box's own copy of the
	// session even where that copy holds it ([sandbox.CarriedSession.Replace]):
	// the copy is one its harness was cut off in. False for a run carried no
	// archive, or one whose box keeps a copy that holds it.
	Replaces bool
}

// resumer is a harness that continues a session an earlier run reported, and
// wires its run to the session it started: a continuing run is handed the
// seat's tools under two bridge tokens, the continued session's and one for a
// fresh session its script may start instead ([sandbox.RunRequest.FreshWiring]);
// its [Harness.WriteConfig] writes both and its script installs the started
// one's, removing the other, before the harness starts. Otherwise a fresh
// session would reach the seat's tools as the continued one, held to that
// session's skill guard.
type resumer interface{ Resumes() bool }

// carrier is a harness whose session the runner carries between boxes
// ([sandbox.SessionArchive]): its run's script packs the session into
// [Paths.SessionCapture] once the harness exits, and restores one carried into
// [Paths.SessionCarried] before it starts. Optional: a run handed a session on
// a harness that is not one is refused, never started in a conversation its
// caller believes is the old one.
type carrier interface {
	// ArchiveFormat is the version of the layout the harness's script packs a
	// session in — the only one it restores.
	ArchiveFormat() int

	// VersionCommand is the command that prints the harness's release in a box.
	VersionCommand() string

	// Continues reports whether a session written by release writer, carried
	// from another box, can be continued by release box (each as `--version`
	// gave it), and if not, why, in a person's words. An unreadable release is
	// taken for a current one, as the notes gate takes it ([claudeNotesGate]).
	Continues(box, writer string) (bool, string)

	// Pack is the script that packs session now, the same pack the run's
	// script makes once its harness exits, for a job that never got to
	// ([Runner.PackCapture]): one that is over or, where working says so, one
	// its box would not end, its harness still working — then held to a
	// stricter rule where a pack reads the harness's files beside it
	// ([claudeHeldTar]). Staged in staged ([Paths.SessionPackStaging]) and moved
	// over dest, this pack's own [Paths.SessionPackHeld] for its reader alone,
	// never [Paths.SessionCapture]; its digest kept the same way. Run in a login
	// shell with the box's bin directory first on PATH, as the wrapper runs.
	Pack(paths Paths, session, staged, dest string, working bool) string

	// layout is how the harness's archive is laid out, which the scrub a
	// capture streams it through reads and writes it as ([scrubArchive]).
	layout() archiveLayout

	// transcript reads an archive of ArchiveFormat, decompressed, as the
	// session's transcript entries, handing each to b in recorded order until
	// ctx is done ([ReadTranscript]); kept beside packing, as reading the
	// harness's own record is the same knowledge.
	transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error
}

// saver is a [carrier] whose session the engine packs while its run still
// works, as a save ([Runner.PackSave]): beside its harness, from outside the
// run's script, to a place of the save's own, named after its taker. Both
// runners are: Claude Code's session is files a pack beside the harness can
// read, in a store it takes no lock on; OpenCode's is exported by a private
// server beside the run's own, from the database both open in WAL mode with a
// busy timeout (the package doc's "An OpenCode session saved beside its
// running server").
type saver interface {
	// PackSave is the script that packs rec's session now, beside a harness
	// that may still be working ([claudeHeldTar]), staged in staged
	// ([Paths.SessionSaveStaging]) and moved to dest ([Paths.SessionSave]) —
	// never [Paths.SessionCapture] nor a place another save moves over — its
	// digest kept as every pack's is. It ends as [carrier.Pack] does
	// ([packNothing] where the harness holds none of the session, [packFailed]
	// where no save was made, its error stream saying why), or [packNoRoom]
	// where its box lacks the memory to pack it now, and runs as it does. An
	// error, and no script, where the record alone refuses the save: handed
	// the record as the box holds it ([Runner.packNow]), a saver judges it
	// before any script runs.
	PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error)
}

// mcpHolder is a harness that can be held to the MCP servers a run hands it,
// starting none its own configuration, a repository's or a plugin's declares
// ([sandbox.RunRequest.OnlyMCPServers]). Only Claude Code's runner is one, as
// the one run that asks (the assistant's deep work) runs on Claude Code alone;
// such a run on another harness is refused rather than started on every server
// its configuration adds up to.
type mcpHolder interface{ HoldsToItsMCPServers() bool }

// sessionTotals is a harness whose result, on a run that continues a session,
// may state the session's spend rather than the run's: from the release
// TotalsRestored names, a continued session starts from the totals the job
// before saved. The runner keeps that job's result as the run starts
// ([Paths.BaseResult]) and charges the difference, unless the stream stated
// the run's own spend ([runSpender]), which is exact. Optional: OpenCode
// measures its session around the run in its own script instead
// ([openCodeUsageBefore]).
type sessionTotals interface {
	// TotalsRestored reports whether the harness release version, as its
	// `--version` printed it, restores a continued session's totals. A
	// version that cannot be read is taken for a current release.
	TotalsRestored(version string) bool
}

// runSpender is a decoder that read the run's OWN spend off its stream: the
// figures its harness states for the calls this process made, which nothing a
// resume or a kill does to the session's totals can add to.
type runSpender interface {
	// runSpend is that spend, and whether the stream stated any.
	runSpend() (runSpend, bool)
}

// contextReader is a decoder that reads off its run's stream what the newest
// request of the session the run worked in carried, for the measure of the copy
// the job's box holds as the job ends ([sandbox.Result.LastRequest]): Claude
// Code's off the main thread's assistant lines ([claudeEvents.measure]),
// OpenCode's off the pages of the session's messages its script asks its server
// for once the run has ended ([openCodeContext]).
type contextReader interface {
	// request is that measure, or why the stream gave none.
	request() contextRead
}

// limitReporter is a decoder that keeps its run's last limit report line
// ([sandbox.Result.Limits]): Claude Code's `rate_limit_event`.
type limitReporter interface {
	limitReport() string
}

// windowReporter is a decoder that reads off its run's stream the least context
// window the run's harness reported holding a model to
// ([sandbox.Result.ReportedWindow]): Claude Code's off its result lines
// ([claudeEvents.reportedWindow]).
type windowReporter interface {
	reportedWindow() int64
}

// contextRead is what a [contextReader] read: the measure, its Tokens above
// zero where the stream gave one; else why it gave none, and whether that is a
// reading that failed (a stream of a shape this build does not read, which the
// collection says at warning) rather than a run that left nothing to read.
type contextRead struct {
	measure sandbox.RequestMeasure
	why     string
	failed  bool
}

// runSpend is what one run spent, as its harness stated it: the prompt not served
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

// less is result with what base stated taken off: a continued run's own spend
// where its harness stated the session's. A negative difference passes on for
// the coordinator to floor and report ([sandbox.Result]'s floor,
// sandbox_usage_negative), as every negative count is. The turn count is the
// process's own on every release, so it is never a difference.
func less(result, base sandbox.Result) sandbox.Result {
	result.InputTokens -= base.InputTokens
	result.OutputTokens -= base.OutputTokens
	result.CacheReadTokens -= base.CacheReadTokens
	result.CacheWriteTokens -= base.CacheWriteTokens
	result.CostUSD -= base.CostUSD
	return result
}

// noteReader is a harness that takes a person's notes to its run's turn between
// its own steps, where its launch wires them ([sandbox.RunRequest.Steer]), and
// tells its model so: NoteInstruction is that standing instruction
// ([ClaudeCodeSteerInstruction], [OpenCodeSteerInstruction]), the harness's own
// because where a note appears is the harness's to decide, and the harness adds
// it to the system text of a run its notes can reach ([withNoteInstruction]).
// Optional: a harness not taught to read notes (hooks, a plugin) must not claim
// to, and the instruction and the never-asked warning both follow this.
type noteReader interface{ NoteInstruction() string }

// Output is how the wrapper connects a harness's standard streams in the box —
// where its stdout lands, what its stdin reads — and how its result is read
// back.
type Output struct {
	// Stdout is the file the wrapper redirects the harness's stdout into.
	Stdout string

	// Events says Stdout is an EVENT STREAM, decoded a line at a time by
	// [Harness.Events] — never read whole, because it grows with every tool
	// call the run makes.
	Events bool

	// Result is the file the harness's result is read from, whole
	// ([Harness.Parse]), or "" when the result is the stream's own. Where it
	// differs from Stdout, the wrapper writes the stream's last line into it
	// after the harness exits (where a streaming harness puts its result), and
	// where the wrapper died first, the collection reads that line instead.
	Result string

	// BriefOnStdin says the harness reads its brief on stdin, from a file Start
	// writes ([Paths.Brief]), rather than its argv: OpenCode re-quotes an argv
	// prompt, altering the quotes and code in a brief, while stdin arrives byte
	// for byte. Unset, stdin is /dev/null, so a headless agent never blocks on
	// input.
	BriefOnStdin bool

	// Unset is every variable the wrapper removes from the harness's
	// environment, from whatever source (role.sandbox.env, the image, a login
	// profile), because it switches off something the run depends on. Removed
	// in the wrapper, after the login shell sourced its profile: the one place
	// every source has been read.
	Unset []string
}

// Pinned is one variable the wrapper exports for its harness ([Harness.Pins]): its
// name, and its value as it is written, which the wrapper quotes.
type Pinned struct{ Name, Value string }

// Runner drives one harness through the detached lifecycle.
type Runner struct {
	harness Harness

	// failureBound is how much of a run's error stream, from its end, a
	// collection reads, and the most a composed failure may be:
	// [sandbox.MaxFailureBytes] in every runner [New] builds. The failure is
	// what the coordinator condenses, so it is held to what one condensation
	// takes beside the coordinator's prefix; from the end, because a process's
	// conclusion is the last thing it prints. The engine's own sentences come
	// first and the stream gets what is left ([failureDetail]). A field so the
	// suite can stage a failure past it in kilobytes, not mebibytes.
	failureBound int

	// lineBound is the longest line of a run's event stream the runner
	// reads: [maxLineBytes] in every runner New builds, a field for the
	// failure bound's reason.
	lineBound int

	// now is the clock a pack's release is bounded by ([releaseContext]):
	// time.Now in every runner New builds, a field so the suite holds a
	// release's deadline to the one its rule gives exactly, not one moved by how
	// long the case has run (internal/fixedwait).
	now func() time.Time
}

var _ sandbox.Runner = (*Runner)(nil)

// A runner names the key its harness hands each call's id under, or none
// ([Runner.CallIDMeta]).
var _ sandbox.CallIdentifier = (*Runner)(nil)

// New wraps a harness in the shared plumbing.
func New(harness Harness) *Runner {
	return &Runner{harness: harness, failureBound: sandbox.MaxFailureBytes, lineBound: maxLineBytes, now: time.Now}
}

// Name is the harness's config name.
func (r *Runner) Name() string { return r.harness.Name() }

// ReadsNotes reports whether this runner's harness takes a person's notes between
// its steps — see [noteReader]. The coordinator asks it before it says a run
// that succeeded never asked for its notes (internal/sandbox).
func (r *Runner) ReadsNotes() bool { return r.noteInstruction() != "" }

// CallIDMeta is the `_meta` key this runner's harness sends each call's own id
// under, by which the bridge serving its box records each call
// ([sandbox.CallIdentifier]): the key the harness's built-in profile reads
// (harnessprofile's Requests.CallIDMeta) — `claudecode/toolUseId` for Claude
// Code, none for OpenCode, whose `_meta` carries no call id. The profile is the
// one place the key lives, so a harness taught one there is read here unchanged.
// Never an entry's `overrides`: those reach its runs on the engine host, while
// a box's runner is the harness's, shared by every entry; a wrong entry key
// would cost its box's calls their ids, each then read by its place as an
// older build's are.
func (r *Runner) CallIDMeta() string { return callIDKeys()[r.harness.Name()] }

// callIDKeys is the `_meta` key of each harness this build drives
// ([Names]), read off its built-in profile once: every launch asks.
var callIDKeys = sync.OnceValue(func() map[string]string {
	keys := make(map[string]string, len(harnesses))
	for _, h := range harnesses {
		if p, ok := harnessprofile.Builtin(h.Name()); ok && p.Requests != nil {
			keys[h.Name()] = p.Requests.CallIDMeta
		}
	}
	return keys
})

// noteInstruction is the standing instruction about notes this runner's harness
// gives its runs, or "" for a harness that takes none — see [noteReader].
func (r *Runner) noteInstruction() string {
	if reader, ok := r.harness.(noteReader); ok {
		return reader.NoteInstruction()
	}
	return ""
}

// Install prepares the box: the artefact directory every file of a run's is
// written under ([Paths.WorkDir]), and the box's own bin directory first on
// the run's PATH ([Paths.BinDir]). The harness ships in the image, which makes
// a coding box a template rather than a per-run build, and provisioning (git
// auth, registry credentials, toolchains) is the launch's setup steps, which
// the manager applies after this returns.
func (r *Runner) Install(ctx context.Context, box sandbox.Sandbox) error {
	paths := PathsFor(box)
	if _, err := box.Exec(ctx, "mkdir -p "+sandbox.ShellQuote(paths.BinDir()), sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("executor: preparing %s: %w", paths.BinDir(), err)
	}
	return nil
}

// ClearArtifacts removes what a previous run left in a reused box, all but the
// checkout, which is the continued context and the reason to reuse a box.
// Left, a stale done marker fires the completion poll at once and Collect reads
// the old result; a previous job's hooks name a bridge session its end closed;
// its end-protocol records ([Paths.JobScript], [Paths.JobEnded]) would aim an
// end request at another process or read as this run's own; its session record,
// capture, saves and staging would be collected as this run's or wait in the box
// for its life; its fresh-session wiring ([Paths.MCPConfigFresh]) would sit
// beside a run that has none. Directories go whole, so no stale value, plugin or
// commit hook survives, and every file a run starts with is new to its box: across
// a VM mount, a harness opening a host-replaced file finds nothing for ~1 s.
// Never the session store a run continues, nor its [Paths.SessionDigests].
func (r *Runner) ClearArtifacts(ctx context.Context, box sandbox.Sandbox) error {
	p := PathsFor(box)
	out := r.harness.Output(p)
	files := []string{p.Done(), p.ExitCode(), p.MemoryBefore(), p.MemoryAfter(), p.Result(), p.Findings(), p.Brief(), p.FreshBrief(), p.System(),
		p.ClaudeNotesSystem(), p.ClaudeSettings(), p.ClaudeNotesSettings(), p.ClaudeSkillsSettings(),
		p.ClaudeSettingsPinned(), p.ClaudeNotesSettingsPinned(), p.ClaudeSkillsSettingsPinned(), p.OpenCodeConfig(),
		p.BaseResult(),
		p.SessionRecord(), p.SessionCapture(), p.SessionCarried(), p.MCPConfig(), p.MCPConfigFresh(),
		p.JobScript(), p.JobScript() + ".next", p.JobEnded(), p.JobEnded() + ".next"}
	// The stream too, where it lives apart from the result: it is only
	// truncated when the next job starts, and a peek between the clear and
	// the start would show the previous job's stream as this one's.
	if out.Stdout != p.Result() {
		files = append(files, out.Stdout)
	}
	// Only directories are removed recursively, in separate statements rather
	// than joined by `&&`, so a file that will not go never keeps a directory.
	// Saves ([Paths.SessionSave]) and on-demand pack staging with its held
	// archive ([Paths.SessionPackStaging]) are named per taker or per pack, so
	// they cannot be listed and are matched by a glob outside the quotes: one
	// whose node stopped part way would otherwise stay for the box's life.
	cmd := "rm -f " + shellQuoteAll(files) + "; rm -rf " +
		shellQuoteAll([]string{p.OpenCodeValues(), p.OpenCodeValuesFresh(), p.OpenCodeSteerPlugin(),
			p.CommitAttribution(), p.SessionStaging(), p.ClaudeSkillsPlugin(),
			p.OpenCodeSkills(), p.OpenCodeSkillsPlugin()}) +
		"; rm -rf " + sandbox.ShellQuote(p.WorkDir()) + "/" + saveFiles +
		"; rm -rf " + sandbox.ShellQuote(p.WorkDir()) + "/" + packStagingFiles
	if _, err := box.Exec(ctx, cmd, sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("executor: clearing the prior run's artefacts: %w", err)
	}
	return nil
}

// shellQuoteAll is paths quoted for sh, one word each, space-separated.
func shellQuoteAll(paths []string) string {
	quoted := make([]string, len(paths))
	for i, path := range paths {
		quoted[i] = sandbox.ShellQuote(path)
	}
	return strings.Join(quoted, " ")
}

// Start launches the harness detached and returns its handle.
//
// The job runs uncapped: no wall-clock timer stops it, so a long run may
// finish. On a clean exit the shell writes the exit code into the exit-code
// file and the done marker (non-empty, for the reason on [Paths.Done]). Its
// stdin is closed, or is the brief's file, which ends: with no timer, a
// headless agent blocked on input would hang forever.
func (r *Runner) Start(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest) (sandbox.RunHandle, error) {
	if c, ok := r.harness.(resumer); req.Resume != "" && (!ok || !c.Resumes()) {
		return sandbox.RunHandle{}, fmt.Errorf("executor: %s cannot continue an earlier "+
			"session, and this run asks it to", r.harness.Name())
	}
	// A session id names files in the box and was read back from a box a run's
	// agent could write, so one that is no id a harness mints is refused rather
	// than handed to the run's script as a path.
	if req.Resume != "" && !sandbox.ValidSessionID(req.Resume) {
		return sandbox.RunHandle{}, fmt.Errorf("executor: the session this run continues, %q, is "+
			"not a session id a harness names", req.Resume)
	}
	if err := r.refuseCarried(req); err != nil {
		return sandbox.RunHandle{}, err
	}
	if h, ok := r.harness.(mcpHolder); req.OnlyMCPServers && (!ok || !h.HoldsToItsMCPServers()) {
		return sandbox.RunHandle{}, fmt.Errorf("executor: %s starts the MCP servers its own "+
			"configuration declares, and this run asks to be held to the ones it is handed",
			r.harness.Name())
	}
	if err := r.refuseSkills(req); err != nil {
		return sandbox.RunHandle{}, err
	}
	// A SECOND WIRING IS A CONTINUING RUN'S ALONE: a run that starts a session
	// of its own has one session, and the other token written into its box
	// would be one its model could read and dial as a session it is not in.
	if req.FreshWiring != nil && req.Resume == "" {
		return sandbox.RunHandle{}, fmt.Errorf("executor: this run starts a session of its own and was " +
			"handed a second wiring for one, which only a run that continues a session is handed")
	}
	paths := PathsFor(box)
	// What the session had spent ([sessionTotals]), read before the clear
	// removes it, from the box a continued session lives in: the one its
	// previous job ran in.
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
	// The carried session, written after the clear (which removes a previous
	// carried archive) and streamed, never held: an archive grows with its
	// session, and a node restoring many seats' sessions would hold each whole.
	// The run's script restores it, or leaves a box that holds it as it is.
	if c := req.Carried; c != nil {
		if err = box.WriteStream(ctx, paths.SessionCarried(), c.Body); err != nil {
			return sandbox.RunHandle{}, fmt.Errorf("executor: writing the session carried "+
				"into the box: %w", err)
		}
	}
	// The session the run works in, named before the harness starts
	// ([Harness.Session]).
	session := r.harness.Session(req)
	// Final before the config is written, which is where a harness writes the
	// system text from; the notes instruction is the harness's to add
	// ([noteReader]).
	req.System = finalSystem(req.System, paths, req.ReportedRepos)
	// And the servers as this box dials them: their URLs, the tool bridge's
	// included, are in the engine host's network, and a loopback one reaches
	// nothing from inside a container ([sandbox.EngineAddress]). Here, because
	// every run reaches its harness's config through this one call.
	req.MCPServers = sandbox.EngineServers(box, req.MCPServers)
	// The notes route too, which a harness may poll by URL from inside the box
	// (OpenCode's plugin, [OpenCode.WriteConfig]). A copy, as req is this
	// call's own.
	if req.Steer.Set() {
		req.Steer.URL = sandbox.EngineAddress(box, req.Steer.URL)
	}
	// And its skill route, which OpenCode's plugin posts each load to
	// ([OpenCode.WriteConfig]), for the same reason.
	if req.SkillLoads.Set() {
		req.SkillLoads.URL = sandbox.EngineAddress(box, req.SkillLoads.URL)
	}
	// AND THE SAME THREE UNDER THE JOB'S OTHER TOKEN, for a session the run's
	// script starts of its own ([sandbox.RunRequest.FreshWiring]): dialled from
	// the same box, so held to the same addresses. A copy, as req is.
	if f := req.FreshWiring; f != nil {
		wired := sandbox.Wiring{MCPServers: sandbox.EngineServers(box, f.MCPServers), Steer: f.Steer,
			SkillLoads: f.SkillLoads}
		if wired.Steer.Set() {
			wired.Steer.URL = sandbox.EngineAddress(box, wired.Steer.URL)
		}
		if wired.SkillLoads.Set() {
			wired.SkillLoads.URL = sandbox.EngineAddress(box, wired.SkillLoads.URL)
		}
		req.FreshWiring = &wired
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
	inner := withBoxBin(r.harness.Command(req, session, paths, configPath), paths)
	script, err := wrapperScript(inner, out, r.harness.Pins(ctx, req, paths), paths, hooks)
	if err != nil {
		return sandbox.RunHandle{}, err
	}
	if out.BriefOnStdin {
		if err = box.WriteFile(ctx, paths.Brief(), []byte(req.Brief)); err != nil {
			return sandbox.RunHandle{}, fmt.Errorf("executor: writing the brief: %w", err)
		}
		// AND THE BRIEF A SESSION OF ITS OWN IS HANDED, for a run that
		// continues one: its script points its harness at it where it starts
		// a new session in the one's place ([sandbox.RunRequest.FreshBrief]).
		if session.Continues && req.FreshBrief != "" {
			if err = box.WriteFile(ctx, paths.FreshBrief(), []byte(req.FreshBrief)); err != nil {
				return sandbox.RunHandle{}, fmt.Errorf("executor: writing the brief a new session is handed: %w", err)
			}
		}
	}

	// A login shell, because a harness installed through nvm, asdf or similar
	// is on PATH only after a profile a login shell sources. So PATH inside is
	// the profile's, which is why the box's bin directory is prepended inside
	// the script ([withBoxBin]): an export around it would be replaced.
	pid, err := box.StartBackground(ctx, "sh -lc "+sandbox.ShellQuote(script), sandbox.ExecOptions{Env: req.Env})
	if err != nil {
		return sandbox.RunHandle{}, fmt.Errorf("executor: starting %s: %w", r.harness.Name(), err)
	}
	log.InfoContext(ctx, "harness_started", "harness", r.harness.Name(), "pid", pid,
		"session", session.ID, "continues", session.Continues, "carried", req.Carried != nil)
	return sandbox.RunHandle{CommandID: pid, SessionID: session.ID, FreshSessionID: session.Fresh}, nil
}

// refuseCarried is why req's carried session ([sandbox.RunRequest.Carried])
// cannot be restored by this runner, or nil where there is none or it can: a
// harness that carries no session, an archive another harness wrote or another format
// lays out, one that is not the session the run continues, or one with no
// bytes. Refused rather than started: a run handed a session to continue, that
// could only start a new one, would be answered as if it remembered.
func (r *Runner) refuseCarried(req sandbox.RunRequest) error {
	c := req.Carried
	if c == nil {
		return nil
	}
	carries, ok := r.harness.(carrier)
	switch {
	case !ok:
		return fmt.Errorf("executor: %s carries no session between boxes, and this run was "+
			"handed one", r.harness.Name())
	case c.Archive.Harness != r.harness.Name():
		return fmt.Errorf("executor: the carried session was written by %q, and this run is on "+
			"%s: a session is continued only by the harness that wrote it", c.Archive.Harness, r.harness.Name())
	case c.Archive.Format != carries.ArchiveFormat():
		return fmt.Errorf("executor: the carried session's archive is format %d, and this build "+
			"restores %s's format %d alone", c.Archive.Format, r.harness.Name(), carries.ArchiveFormat())
	case req.Resume == "" || c.Archive.SessionID != req.Resume:
		return fmt.Errorf("executor: the carried session %q is not the one this run continues (%q)",
			c.Archive.SessionID, req.Resume)
	case c.Body == nil:
		return fmt.Errorf("executor: the carried session %q has no archive to restore",
			c.Archive.SessionID)
	}
	return nil
}

// baseResult is the result the job before req left in box, for a run that
// continues its session on a harness whose result may state the session's spend
// ([sessionTotals]), and "" for every other run.
//
// One the box cannot give back is no base: the run still starts (it is the
// work a person waits for), charged what its harness states and logged so an
// overcharge has a cause on record. A read that failed outright fails the
// start, for the launch to retry. A box a session was carried into
// ([sandbox.RunRequest.Carried]) is expected to hold none, as carrying needs a
// release that states each run's own spend ([claudeCarryFloor]). A job before
// that stopped before its result was charged nothing, so the base it kept is
// carried forward and the next result is charged its calls too.
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
	if refusal == "" && r.statesTotalsOf(raw, req.Resume) {
		return raw, nil
	}
	if refusal == "" && strings.TrimSpace(raw) == "" && req.Carried != nil {
		return "", nil
	}
	if req.Carried == nil {
		kept, keptRefusal, err := readWhole(ctx, box, paths.BaseResult(), "the base the job before it kept")
		if err != nil {
			return "", fmt.Errorf("executor: reading what the continued session had spent: %w", err)
		}
		if keptRefusal == "" && r.statesTotalsOf(kept, req.Resume) {
			return kept, nil
		}
	}
	if refusal == "" && strings.TrimSpace(raw) != "" {
		refusal = "it is no result of the session this run continues"
	}
	log.WarnContext(ctx, "harness_session_spend_unread", "harness", r.harness.Name(),
		"detail", "this run continues a session, and the result the job before it left "+
			"could not be read, so the run is charged the spend its harness states, which counts "+
			"the session's earlier jobs again", "refusal", refusal)
	return "", nil
}

// statesTotalsOf reports whether raw is a harness result naming session, so
// what it states is that session's totals.
func (r *Runner) statesTotalsOf(raw, session string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	got := r.harness.Parse(raw).SessionID
	return sandbox.ValidSessionID(got) && got == session
}

// ownSpend is result charged what this run spent ([sessionTotals],
// [runSpender]): the spend its own stream stated, where it stated any; else,
// for a run that continued a session on a harness that restores its totals,
// the result less the base kept when the run took it up ([Paths.BaseResult]);
// else the result as stated, which is the run's own. Only a run that did
// continue its session, as its script recorded ([sessionRecord]), and whose
// result states that session's totals has the base taken off: one that started
// a new session, or stopped before its result, would be charged below nothing.
func (r *Runner) ownSpend(ctx context.Context, box sandbox.Sandbox, paths Paths,
	result sandbox.Result, streamed *runSpend, rec sessionRecord, stated string) (sandbox.Result, error) {

	if streamed != nil {
		return streamed.on(result), nil
	}
	totals, ok := r.harness.(sessionTotals)
	if !ok || !rec.continued {
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
	if !totals.TotalsRestored(rec.release) {
		return result, nil
	}
	// Only a result naming the base's own session (stated) has them taken off:
	// one that stopped before its result states none.
	base := r.harness.Parse(raw)
	if !sandbox.ValidSessionID(stated) || stated != base.SessionID {
		return result, nil
	}
	return less(result, base), nil
}

// wrapperScript is the shell line a job runs under: the harness with its stdout
// and stderr redirected, then its result, its exit code and its done marker.
//
// The exit status is the harness's own: read into code before anything else
// runs, with the result line copied after the harness exits rather than piped
// beside it, since a plain `sh` has no pipefail. hooks is the directory the
// run's git takes its hooks from ([attributionExports]), or "" for a run that
// credits nothing. A pin no assignment can carry ([envfile.FormatAssignment])
// refuses the run before its job starts.
func wrapperScript(inner string, out Output, pins []Pinned, paths Paths, hooks string) (string, error) {
	stdin := "/dev/null"
	if out.BriefOnStdin {
		stdin = sandbox.ShellQuote(paths.Brief())
	}
	var b strings.Builder
	// A STATEMENT OF ITS OWN, before the harness's: an assignment prefix on the
	// command could only add to its environment, never remove from it.
	if len(out.Unset) > 0 {
		fmt.Fprintf(&b, "unset %s; ", strings.Join(out.Unset, " "))
	}
	// And the pins after the unsets, so each replaces what an earlier source
	// gave it, written as envfile writes an assignment, the one form a dotenv
	// reader and `source` agree on: a value either would misread is refused.
	for _, pin := range pins {
		line, err := envfile.FormatAssignment(pin.Name, pin.Value)
		if err != nil {
			return "", fmt.Errorf("executor: pinning %s for the harness: %w", pin.Name, err)
		}
		b.WriteString(line + "; ")
	}
	// AFTER THE PROFILE, BEFORE THE HARNESS, for the reason the unset is: an
	// entry exported around the shell would be replaced by a profile that
	// sets the count.
	if hooks != "" {
		b.WriteString(attributionExports(hooks))
	}
	// THE JOB'S MEMORY DOMAIN EITHER SIDE OF ITS HARNESS, so a kill the kernel
	// made for memory meanwhile is told from any other end ([Paths.MemoryBefore]).
	fmt.Fprintf(&b, "%s\ncrewlet_memory > %s 2>/dev/null; ", sandbox.MemoryDomainScript,
		sandbox.ShellQuote(paths.MemoryBefore()))
	fmt.Fprintf(&b, "%s < %s > %s 2> %s; code=$?; crewlet_memory > %s 2>/dev/null; ",
		inner, stdin, sandbox.ShellQuote(out.Stdout), sandbox.ShellQuote(paths.Err()),
		sandbox.ShellQuote(paths.MemoryAfter()))
	if out.Result != "" && out.Result != out.Stdout {
		fmt.Fprintf(&b, "tail -n 1 %s > %s; ", sandbox.ShellQuote(out.Stdout), sandbox.ShellQuote(out.Result))
	}
	fmt.Fprintf(&b, "echo $code > %s; echo $code > %s",
		sandbox.ShellQuote(paths.ExitCode()), sandbox.ShellQuote(paths.Done()))
	return b.String(), nil
}

// finalSystem appends the report instruction, addressed at this box, to the
// run's standing instructions, naming which pushes the engine reports for it
// (native: the Gitlet repositories it may push to). Composed here because the
// findings path belongs to the box the run lands in, unknown when the request
// is built; on the system side because it is how every run works, not its task.
func finalSystem(system string, paths Paths, native []string) string {
	parts := make([]string, 0, 2)
	if system = strings.TrimSpace(system); system != "" {
		parts = append(parts, system)
	}
	parts = append(parts, strings.TrimSpace(FindingsInstruction(paths.Findings(), native)))
	return strings.Join(parts, "\n\n")
}

// withNoteInstruction is a run's final system text followed by its harness's
// instruction about notes ([noteReader]), the last of its standing
// instructions. The harness adds it, and only where a note can arrive, or it
// would describe a channel that never opens: whether one can is known only
// where the run starts, as a Claude Code box decides from its harness's
// release ([claudeNotesGate]).
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
	return "PATH=" + sandbox.ShellQuote(paths.BinDir()) + `:"$PATH" ` + cmd
}

// Poll reports what is known of the detached job ([sandbox.JobState]) from two
// signals, since there is no run-time TTL and a finished or dead job must never
// wedge the run: the done marker, which the wrapper writes however the harness
// exits (ENDED); and, with no marker, the box's account of the wrapper
// ([sandbox.Sandbox.Job]), which only the backend can give — whether its handle
// is a pid in the box's namespace or a reusable host pid, and whether the job
// went while an engine watched it (ENDED) or with the engine that last saw it
// (LOST). The marker is read again before a job is answered lost, in case the
// wrapper wrote it meanwhile. A failed read never skips the liveness probe, or
// a dead job would poll as running for good; its error is reported beside
// "running". A live wrapper is RUNNING, working or hung: telling those apart
// needs a timer, which this design refuses.
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
	log.WarnContext(ctx, "harness_process_gone",
		"harness", r.harness.Name(), "pid", handle.CommandID, "job", string(state))
	return state, nil
}

// Collect reads the finished job's result out of the box. The event stream is
// decoded a line at a time ([eachLine]) and the error stream read from its end
// ([Runner.failureBound]), so neither is refused for its size. A piece read
// whole (report, question, result line, exit code) past [sandbox.MaxFileBytes],
// or any piece whose path is not a regular file ([sandbox.ErrNotRegularFile],
// which a retry would read again), degrades only itself: described, the run
// not a success, its tokens, refs and transcript still collected and charged.
// A read that fails is an error: the box could not be read back. Everything is
// redacted whole before anything is bounded, since a secret cut in two
// survives as fragments; report, failure and transcript leave whole, bounded
// by the coordinator in one place ([sandbox.MaxRunTextBytes]).
func (r *Runner) Collect(ctx context.Context, box sandbox.Sandbox, handle sandbox.RunHandle) (sandbox.Result, error) {
	paths := PathsFor(box)
	out := r.harness.Output(paths)
	var refused []string

	var (
		result  sandbox.Result
		last    streamLast
		spent   *runSpend
		request contextRead
		window  int64
		limits  string
	)
	if out.Events {
		streamed, refusal, err := r.decodeStream(ctx, box, out.Stdout)
		if err != nil {
			return sandbox.Result{}, err
		}
		if refusal != "" {
			refused = append(refused, refusal)
		}
		result, last, spent, request = streamed.result, streamed.last, streamed.spent, streamed.request
		window, limits = streamed.window, streamed.limits
	}
	if out.Result != "" {
		raw, refusal, err := readWhole(ctx, box, out.Result, "the result its harness printed")
		if err == nil && out.Events && out.Result != out.Stdout && last.said() &&
			(refusal != "" || strings.TrimSpace(raw) == "") {
			// The result file holds the stream's last line, copied by the
			// wrapper after the harness exits. Where the file is empty (the
			// process group died) or refused, that line is read in its place,
			// so a streamed session is not "no output" and its spend is
			// charged; a refused file is still reported. Only where the stream
			// said something: a harness that exited at once leaves both empty.
			why := "was empty"
			if refusal != "" {
				refused = append(refused, refusal)
				why = "was refused"
			}
			raw, refusal = last.resultLine(out.Stdout)
			log.WarnContext(ctx, "harness_result_from_stream", "harness", r.harness.Name(),
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
	// What the session's newest request carried, off the stream, set once the
	// result is final: set above, it would be dropped wherever the result file
	// replaces the stream's result, as Claude Code's does.
	if out.Events {
		result.LastRequest = r.lastRequest(ctx, request)
		// And the least window the run reported, here for the same reason: a
		// continuation of the copy is judged under it where it is less than the
		// engine's own reading of the model's window.
		result.ReportedWindow = window
		result.Limits = limits
	}

	// Which session the run took up, as its script recorded before its harness
	// started ([Paths.SessionRecord]): its id, whether it continued, why it
	// started anew (the box's sentence, bounded where it enters the result,
	// [sandbox.BoundFellBack]), and the harness's release. A refused record is
	// a piece the run did not report.
	recorded, recordRefusal, err := readWhole(ctx, box, paths.SessionRecord(), "the record of the session its harness ran in")
	if err != nil {
		return sandbox.Result{}, err
	}
	if recordRefusal != "" {
		refused = append(refused, recordRefusal)
	}
	rec := parseSessionRecord(recorded)
	// The harness's own session id only where it is one a harness mints
	// ([sandbox.ValidSessionID]), else the record's: what it printed is a box
	// file too, and the id is handed to a later run's script.
	stated := result.SessionID
	if !sandbox.ValidSessionID(result.SessionID) {
		result.SessionID = rec.id
	}
	result.Continued, result.HarnessVersion = rec.continued, releaseOf(rec.release)
	result.FellBack = sandbox.BoundFellBack(rec.why)
	if result.Captured, err = r.captured(ctx, box, paths, rec); err != nil {
		return sandbox.Result{}, fmt.Errorf("executor: reading the session the box packed: %w", err)
	}

	// WHAT THIS RUN SPENT, never its session's: a harness that continued a
	// session can state the session's spend, and charged as it is, every
	// continued run would pay for the jobs before it again ([Runner.ownSpend]).
	result, err = r.ownSpend(ctx, box, paths, result, spent, rec, stated)
	if err != nil {
		return sandbox.Result{}, fmt.Errorf("executor: reading what the run itself spent: %w", err)
	}

	// The transcript is the observability surface of an agent with no
	// telemetry: the parser's from streamed events, else the error stream,
	// read once and reused below for the failure detail.
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

	// A KILL FOR MEMORY DURING THE JOB, as the kernel counted it in the job's
	// memory domain; the run ended at its ceiling where its harness was the
	// process killed (SIGKILL, status 137).
	result, refused, err = memoryKills(ctx, box, paths, result, refused, code)
	if err != nil {
		return sandbox.Result{}, err
	}

	// Whether the job was ended on request, as its script recorded
	// ([Paths.JobEnded]): such a run did not finish, whatever it wrote or exited
	// with (a harness may exit 0 on SIGTERM), so it is never a success and its
	// failure says so first. A word this build does not know is no end.
	endedText, refusal, err := readWhole(ctx, box, paths.JobEnded(), "the record of how its job was ended")
	if err != nil {
		return sandbox.Result{}, err
	}
	if refusal != "" {
		refused = append(refused, refusal)
	}
	if ended := sandbox.JobEnd(strings.TrimSpace(endedText)); ended.Valid() {
		result.JobEnd = ended
	}
	// AND WHETHER ITS HARNESS EXITED CLEANLY, for the session its script
	// packed after it whatever its status: one killed while the wrapper lived
	// on — a signal, its memory limit — may end mid-record, and so may one
	// whose status the box never recorded ([sandbox.Result.HarnessCutOff]).
	result.HarnessCutOff = code != "0"

	// The findings file is the result carrier of record: the agent is asked to
	// write its report there, and it survives a process group that died before
	// the final message and a tool-only run that parses to no text. So it wins
	// for the text, and its presence means success unless the process crashed.
	findingsText, refusal, err := readWhole(ctx, box, paths.Findings(), "the report the harness wrote")
	if err != nil {
		return sandbox.Result{}, err
	}
	if refusal != "" {
		refused = append(refused, refusal)
	}
	findings := strings.TrimSpace(findingsText)
	// What the failure says after any piece that could not be read: the
	// harness's own account where the run reported something, and where it
	// produced nothing at all, the exit status, the harness's error and the
	// error stream.
	var said []string
	errStream := ""
	switch {
	case findings != "":
		// The report's refs before the message's, by one rule over both
		// ([deliveredRefs]), so neither's named refs are outranked by a pull
		// request the other only mentions.
		result.DeliveredRefs = deliveredRefs(findings, result.Text)
		result.Text = findings
		if !crashed {
			result.Success = true
			result.Error = ""
		}
		said = append(said, result.Error)
	case !result.Success && strings.TrimSpace(result.Text) == "":
		// Nothing reported or parsed: surface the exit status, the harness's
		// error and the stderr, all three in that order, so the completion is
		// a real failure, not a silent stall, and a stray stderr warning never
		// displaces the harness's error. A run ended on request says its status
		// in saying so (below).
		if crashed && result.JobEnd == "" {
			said = append(said, "the harness exited with status "+code)
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
	// A RUN ENDED ON REQUEST says so before anything else, for the same
	// reason: nothing it wrote says it was stopped.
	var sentences []string
	if result.JobEnd != "" {
		result.Success = false
		sentences = append(sentences, endedSentence(result.JobEnd, code))
	}
	sentences = append(append(sentences, refused...), said...)
	result.Error = failureDetail(sentences, errStream, unread, r.failureBound)

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

// lastRequest is the measure read, which the collection carries back
// ([sandbox.Result.LastRequest]), saying why where there is none
// (`harness_context_unmeasured`): at warning where the reading failed against
// a stream of a shape this build does not read, since then every job of the
// release goes unmeasured; else at info. A relaunch that continues the copy
// the job's box holds is then held to the figure carried along the copy's
// lineage, or to none (internal/sandbox's measure.go).
func (r *Runner) lastRequest(ctx context.Context, read contextRead) sandbox.RequestMeasure {
	if read.measure.Tokens > 0 {
		return read.measure
	}
	why := read.why
	if why == "" {
		why = "the harness's event stream gives no measure of the session's newest request"
	}
	logAt := log.InfoContext
	if read.failed {
		logAt = log.WarnContext
	}
	logAt(ctx, "harness_context_unmeasured", "harness", r.harness.Name(), "detail", why,
		"outcome", "the job's record keeps the measure carried along its copy's lineage, or none")
	return sandbox.RequestMeasure{}
}

// endedSentence is what a run's failure opens with where its job was ended on
// request ([sandbox.Result.JobEnd]), its harness's exit status code where the
// box recorded one.
func endedSentence(ended sandbox.JobEnd, code string) string {
	const opening = "the run was ended on request before it finished: "
	switch ended {
	case sandbox.JobEndKilled:
		return opening + fmt.Sprintf("its harness did not stop within %s and was killed", sandbox.JobStopGrace)
	case sandbox.JobEndBefore:
		return opening + "before its harness started"
	}
	if code == "" {
		return opening + "its harness was told to stop"
	}
	return opening + "its harness was told to stop and exited " + code
}

// AskEnd asks the job handle names to end ([sandbox.JobEnder]), sending the
// request through the box ([sandbox.Sandbox.EndJob]) to the script whose end
// protocol it recorded ([Paths.JobScript]). A script that recorded no protocol
// this build speaks (an older build's, one not yet at its first line, a record
// not `1 <pid>` with a pid above 1) is asked nothing and answered
// [sandbox.EndUnheard], and a pid the box says is no process of the job
// [sandbox.EndGone]: the read kills such a job instead. The record is parsed,
// never handed on as text.
func (r *Runner) AskEnd(ctx context.Context, box sandbox.Sandbox, handle sandbox.RunHandle) (sandbox.EndAsk, error) {
	paths := PathsFor(box)
	recorded, refusal, err := readWhole(ctx, box, paths.JobScript(), "the end protocol its script recorded")
	if err != nil {
		return "", fmt.Errorf("executor: reading the end protocol the job's script recorded: %w", err)
	}
	script, ok := parseJobScript(recorded)
	if refusal != "" || !ok {
		return sandbox.EndUnheard, nil
	}
	switch err := box.EndJob(ctx, handle.CommandID, script); {
	case err == nil:
		return sandbox.EndAsked, nil
	case errors.Is(err, sandbox.ErrNotJobProcess):
		log.InfoContext(ctx, "harness_end_unheard", "harness", r.harness.Name(), "pid", script,
			"error", err.Error())
		return sandbox.EndGone, nil
	default:
		return "", fmt.Errorf("executor: asking %s's script to end its job: %w", r.harness.Name(), err)
	}
}

var _ sandbox.JobEnder = (*Runner)(nil)

// parseJobScript reads a script's record of its end protocol
// ([Paths.JobScript]): `1 <pid>`, one line, the protocol this build speaks and
// a pid above 1 — false for anything else.
func parseJobScript(recorded string) (int, bool) {
	fields := strings.Fields(recorded)
	if len(fields) != 2 || fields[0] != strconv.Itoa(jobScriptProtocol) {
		return 0, false
	}
	pid, err := strconv.Atoi(fields[1])
	if err != nil || pid < 2 {
		return 0, false
	}
	return pid, true
}

// failureDetail composes a run's failure: the engine's own sentences in order
// (an unreadable piece, the exit status, the harness's error), then as much of
// the error stream's end as bound ([Runner.failureBound]) leaves, with what was
// not shown said by size; unread is how much of the stream's start the read
// left. The whole is held to bound exactly, since the compactor refuses a
// failure past what it reads; only the stream gives way. Measured redacted, as
// it leaves: a marker can outgrow its credential, and a name ending one piece
// with its value opening the next (across the `:\n`) is one match neither
// holds, so the whole is redacted and measured and any excess comes off the
// stream's share, whose room also holds the mark [sandbox.KeepEnd] puts on a
// line too long to keep whole.
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
		return decodedStream{request: contextRead{why: "the run's event stream was not read: " + refusal}},
			refusal, nil
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
	if reader, ok := dec.Decoder.(contextReader); ok {
		out.request = reader.request()
	}
	if reporter, ok := dec.Decoder.(windowReporter); ok {
		out.window = reporter.reportedWindow()
	}
	if reporter, ok := dec.Decoder.(limitReporter); ok {
		out.limits = reporter.limitReport()
	}
	return out, "", nil
}

// decodedStream is what a run's event stream said: the result as its decoder
// read it, the stream's last line, the run's own spend where the stream stated
// it ([runSpender]), nil where it stated none, and what the session's newest
// request carried where the stream read one ([contextReader]).
type decodedStream struct {
	result  sandbox.Result
	last    streamLast
	spent   *runSpend
	request contextRead
	// window is the least context window the run reported ([windowReporter]).
	window int64
	// limits is the run's last limit report line ([limitReporter]).
	limits string
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
		return "", fmt.Sprintf("the result its harness printed (the last line of %s, read in place of "+
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
// A refusal rather than an error, because what the path names is the box's
// doing and a retry reads it again: an error would retry the collection until
// the run settled as lost, uncharged and unrecorded, over a pipe the agent
// made where its report goes.
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

// memoryKills is result with the kills the kernel made for memory in the job's
// domain while its harness ran, read off the script's two records of it.
func memoryKills(ctx context.Context, box sandbox.Sandbox, paths Paths, result sandbox.Result,
	refused []string, code string) (sandbox.Result, []string, error) {

	var read [2]sandbox.MemoryDomain
	for i, path := range []string{paths.MemoryBefore(), paths.MemoryAfter()} {
		text, refusal, err := readWhole(ctx, box, path, "the record of its memory")
		if err != nil {
			return sandbox.Result{}, nil, err
		}
		if refusal != "" {
			refused = append(refused, refusal)
		}
		read[i] = sandbox.ParseMemoryDomain(text)
	}
	if killed, ok := sandbox.KilledBetween(read[0], read[1]); ok && killed > 0 {
		result.MemoryKills, result.MemoryBytes = killed, read[1].Ceiling()
		result.OutOfMemory = code == harnessKilled
	}
	return result, refused, nil
}

// harnessKilled is the status a shell gives a child SIGKILL ended: 128 + 9.
const harnessKilled = "137"
