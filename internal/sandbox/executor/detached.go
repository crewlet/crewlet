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
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
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
		if refs := prPattern().FindAllString(text, -1); len(refs) > 0 {
			return refs
		}
	}
	return nil
}

// deliveredLine is one line naming a delivered ref — `Delivered: <ref>` — read
// whatever a report lays it out with: a list marker before it, emphasis
// around the label, any case. What follows the colon is [namedRef]'s to judge.
var deliveredLine = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(
		`(?im)^[ \t]*(?:[-*+][ \t]+|\d+[.)][ \t]+)?[*_]*delivered[*_]*[ \t]*:[*_]*[ \t]*(.*)$`)
})

// namedRefs is every ref a text names on a `Delivered:` line, in its order.
func namedRefs(text string) []string {
	var refs []string
	for _, m := range deliveredLine().FindAllStringSubmatch(text, -1) {
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
var prPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(
		`https?://[^\s/?#@:]+(?::\d+)?/[\w.\-/]+/(?:pull|pulls|pull-requests|pullrequest|merge_requests)/\d+\b`)
})

// Harness is what one harness contributes on top of the shared plumbing: its
// invocation, its config file, where its stdout lands and how its result is
// read back ([Output]), how its event stream is decoded ([Decoder]) and how its
// result is parsed. Everything else — the done marker that ends every run, the
// markers' places, teardown, question signalling, stream reading and bounds — is
// shared: a runner that could override it would be a second implementation of
// the completion protocol.
type Harness interface {
	// Name is the harness's config name: "claude-code", "codex", "opencode".
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
	// whole or followed across many reads ([Decoder]). Asked for the stream
	// only when [Output.Events] is set, and for a result file read apart from
	// it ([Runner.endsRun]).
	Events() Decoder

	// Parse maps this agent's RESULT, read whole, onto a result: the file
	// [Output.Result] names, or — for a harness whose result is its stream —
	// the stream's whole text, decoded from the bytes as read.
	Parse(result []byte) sandbox.Result

	// WriteConfig renders the agent's config file into the box and returns
	// the path the harness is pointed at, or "" when there is nothing to write.
	// It also writes the system text where the harness reads it from
	// ([Paths.System]) when there is any, and nothing when there is none.
	// session is the run's, named before it ([Harness.Session]).
	WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, session Session,
		paths Paths) (string, error)

	// MCPCallCeiling is how long the harness waits for one bridged call
	// whatever its box's settings say, or zero where nothing cuts a call short
	// of the wait those settings carry ([sandbox.MCPServer.CallTimeout], the
	// bridge's own bound). Every harness states it, with the reason at the
	// declaration; the bridge answers a call still running before it
	// (internal/api/mcpbridge, "A call longer than its harness waits").
	MCPCallCeiling() time.Duration
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
	// gave it), and if not, why, in a person's words: [carrier.ContinuesWriter]'s
	// answer, and the box's release held to the harness's own. A box release
	// that does not read refuses ([boxNamedNoRelease]): the box's harness is
	// one no rule here was read for.
	Continues(box, writer string) (bool, string)

	// ContinuesWriter is [carrier.Continues] judged of the writer alone, for a
	// reader with no box to ask ([Runner.Restores]).
	ContinuesWriter(writer string) (bool, string)

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

	// meter reads an archive of ArchiveFormat, as kept, for what its
	// session's last request carried, the harness's count of it and its
	// estimate of the next request ([newContextMeter]), session's as release
	// wrote it, within ctx and under large; err is the archive's own.
	meter(ctx context.Context, r io.Reader, session, release string, large *passBudget) (meterReading, error)

	// charges reports whether meter's count of session charges the bytes of
	// the member an archive of ArchiveFormat names so, as kept, where a scrub
	// that took bytes out of it would lower the count; an export's one
	// document is named "" ([scrubTally]).
	charges(session, member string) bool

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

// enforcerReporter is a decoder that tells from its stream whether what holds
// the run to its cap in its box ran at all: Copilot's and Pi's extensions.
// unheld says why it did not, "" where it ran or the run had no cap. A REPORT
// NEVER OUTWEIGHS IT, as it does the harness's own words of a failure
// ([Runner.Collect]): the run went on with nothing holding it to its cap.
type enforcerReporter interface {
	unheld() string
}

// limitReporter is a decoder that keeps its run's last limit report line
// ([sandbox.Result.Limits]): Claude Code's `rate_limit_event`.
type limitReporter interface {
	limitReport() string
}

// limitGate is a harness whose run's limit is its own word only where the
// harness itself failed the run on its vendor's API error, which its exit
// status names ([GeminiCLI.limitExit]): elsewhere the collection marks the
// run's ending as no limit ([sandbox.Result.NoLimitExit]), so a line the run's
// own output put in the stream (a box file its model can write) sits nothing
// out.
type limitGate interface {
	limitExit(code int) bool
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

	// failureBound is how much of the end of a run's error stream's
	// redaction a collection keeps, and the most a composed failure may be:
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

	// memo is what a take's scrub learned of texts it found unchanged
	// ([scrubMemo]): the process's in every runner New builds, a field so a
	// case reads its own.
	memo *scrubMemo
}

var _ sandbox.Runner = (*Runner)(nil)

// A runner names the key its harness hands each call's id under, or none
// ([Runner.CallIDMeta]).
var _ sandbox.CallIdentifier = (*Runner)(nil)

// And states how long its harness waits for one call ([Runner.MCPCallCeiling]).
var _ sandbox.CallCeilinged = (*Runner)(nil)

// New wraps a harness in the shared plumbing.
func New(harness Harness) *Runner {
	return &Runner{harness: harness, failureBound: sandbox.MaxFailureBytes, lineBound: maxLineBytes, now: time.Now,
		memo: processScrubMemo}
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

// MCPCallCeiling is how long this runner's harness waits for one bridged call
// whatever its box's settings say, zero for no such ceiling
// ([Harness.MCPCallCeiling], [sandbox.CallCeilinged]).
func (r *Runner) MCPCallCeiling() time.Duration { return r.harness.MCPCallCeiling() }

// callIDKeys is the `_meta` key of each harness this build drives
// ([Names]), read off its built-in profile once: every launch asks.
var callIDKeys = sync.OnceValue(func() map[string]string {
	harnesses := Registered()
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
	if _, err := box.Exec(ctx, "mkdir -p "+shellword.Quote(paths.BinDir()), sandbox.ExecOptions{}); err != nil {
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
// report, capture, saves and staging would be collected as this run's (a stale
// report bound as this run's session) or wait in the box for its life; its fresh-session wiring ([Paths.MCPConfigFresh]) would sit
// beside a run that has none. Directories go whole, so no stale value, plugin or
// commit hook survives, and every file a run starts with is new to its box: across
// a VM mount, a harness opening a host-replaced file finds nothing for ~1 s.
// Never the session store a run continues, nor its [Paths.SessionDigests].
func (r *Runner) ClearArtifacts(ctx context.Context, box sandbox.Sandbox) error {
	p := PathsFor(box)
	out := r.harness.Output(p)
	files := []string{p.Done(), p.ExitCode(), p.MemoryBefore(), p.MemoryAfter(), p.Result(), p.Findings(), p.Brief(), p.FreshBrief(), p.System(),
		p.BaseResult(),
		p.SessionRecord(), p.SessionReport(), p.sessionReportStaging(), p.SessionCapture(),
		trimMarkOf(p.SessionCapture()), p.SessionCarried(),
		p.MCPConfig(), p.MCPConfigFresh(),
		p.JobScript(), p.JobScript() + ".next", p.JobEnded(), p.JobEnded() + ".next",
		p.CountRecord(), p.CountRecord() + ".next"}
	dirs := []string{p.CommitAttribution(), p.SessionStaging(), p.ProgramStaging()}
	// EVERY HARNESS'S, not this run's alone: the box may last have run another.
	// A home file a killed run left holding its settings, the bridge's token
	// among them, is put back first ([homeMender], [homeKeeper]).
	for _, h := range Registered() {
		if m, ok := h.(homeMender); ok {
			if err := m.mendHome(ctx, box, p); err != nil {
				return fmt.Errorf("executor: taking a killed %s run's entries out of the box home: %w", h.Name(), err)
			}
		}
		if k, ok := h.(homeKeeper); ok {
			// A PUT-BACK THAT FAILS FAILS THE CLEAR: the home file would keep the
			// killed run's settings, its bridge token among them, for this run.
			if err := execOK(ctx, box, k.homeRestore(p), "putting back the box home's own files a killed "+
				h.Name()+" run held"); err != nil {
				return err
			}
		}
		if a, ok := h.(artifactHolder); ok {
			files = append(files, a.artifactFiles(p)...)
		}
		if a, ok := h.(artifactDirHolder); ok {
			dirs = append(dirs, a.artifactDirs(p)...)
		}
	}
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
	cmd := "rm -f " + shellQuoteAll(files) + "; rm -rf " + shellQuoteAll(dirs) +
		"; rm -rf " + shellword.Quote(p.WorkDir()) + "/" + saveFiles +
		"; rm -rf " + shellword.Quote(p.WorkDir()) + "/" + packStagingFiles
	if _, err := box.Exec(ctx, cmd, sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("executor: clearing the prior run's artefacts: %w", err)
	}
	return nil
}

// heldInBox reports whether path is there in box, a held copy of a home file
// a run has not put back yet ([homeMender]).
func heldInBox(ctx context.Context, box sandbox.Sandbox, path string) (bool, error) {
	res, err := box.Exec(ctx, "[ ! -e "+shellword.Quote(path)+" ] || echo held", sandbox.ExecOptions{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	if err != nil {
		return false, fmt.Errorf("executor: looking for %s: %w", path, err)
	}
	return strings.TrimSpace(res.Stdout) == "held", nil
}

// execOK runs cmd in box, an error saying doing where it does not exit 0.
func execOK(ctx context.Context, box sandbox.Sandbox, cmd, doing string) error {
	res, err := box.Exec(ctx, cmd, sandbox.ExecOptions{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	if err != nil {
		return fmt.Errorf("executor: %s: %w", doing, err)
	}
	return nil
}

// shellQuoteAll is paths quoted for sh, one word each, space-separated.
func shellQuoteAll(paths []string) string {
	quoted := make([]string, len(paths))
	for i, path := range paths {
		quoted[i] = shellword.Quote(path)
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
	if len(base) > 0 {
		if err = box.WriteFile(ctx, paths.BaseResult(), base); err != nil {
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
	configPath, err := r.harness.WriteConfig(ctx, box, req, session, paths)
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
	script, err := wrapperScript(inner, out, r.harness.Pins(ctx, req, paths), paths, hooks, directHosts(req))
	if err != nil {
		return sandbox.RunHandle{}, err
	}
	if out.BriefOnStdin {
		brief, fresh := req.Brief, req.FreshBrief
		if _, ok := r.harness.(commandReader); ok {
			brief, fresh = briefNotACommand(brief), briefNotACommand(fresh)
		}
		if err = box.WriteFile(ctx, paths.Brief(), []byte(brief)); err != nil {
			return sandbox.RunHandle{}, fmt.Errorf("executor: writing the brief: %w", err)
		}
		// AND THE BRIEF A SESSION OF ITS OWN IS HANDED, for a run that
		// continues one: its script points its harness at it where it starts
		// a new session in the one's place ([sandbox.RunRequest.FreshBrief]).
		if session.Continues && fresh != "" {
			if err = box.WriteFile(ctx, paths.FreshBrief(), []byte(fresh)); err != nil {
				return sandbox.RunHandle{}, fmt.Errorf("executor: writing the brief a new session is handed: %w", err)
			}
		}
	}

	// A login shell, because a harness installed through nvm, asdf or similar
	// is on PATH only after a profile a login shell sources. So PATH inside is
	// the profile's, which is why the box's bin directory is prepended inside
	// the script ([withBoxBin]): an export around it would be replaced.
	pid, err := box.StartBackground(ctx, "sh -lc "+shellword.Quote(script), sandbox.ExecOptions{Env: req.Env})
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
func (r *Runner) baseResult(ctx context.Context, box sandbox.Sandbox, paths Paths, req sandbox.RunRequest) ([]byte, error) {
	if _, totals := r.harness.(sessionTotals); !totals || req.Resume == "" {
		return nil, nil
	}
	out := r.harness.Output(paths)
	if out.Result == "" {
		return nil, nil
	}
	raw, refusal, err := readWholeBytes(ctx, box, out.Result, "the result the job before it left")
	if err != nil {
		return nil, fmt.Errorf("executor: reading what the continued session had spent: %w", err)
	}
	if refusal == "" && r.statesTotalsOf(raw, req.Resume) {
		return raw, nil
	}
	blank := len(bytes.TrimSpace(raw)) == 0
	if refusal == "" && blank && req.Carried != nil {
		return nil, nil
	}
	if req.Carried == nil {
		kept, keptRefusal, err := readWholeBytes(ctx, box, paths.BaseResult(), "the base the job before it kept")
		if err != nil {
			return nil, fmt.Errorf("executor: reading what the continued session had spent: %w", err)
		}
		if keptRefusal == "" && r.statesTotalsOf(kept, req.Resume) {
			return kept, nil
		}
	}
	if refusal == "" && !blank {
		refusal = "it is no result of the session this run continues"
	}
	log.WarnContext(ctx, "harness_session_spend_unread", "harness", r.harness.Name(),
		"detail", "this run continues a session, and the result the job before it left "+
			"could not be read, so the run is charged the spend its harness states, which counts "+
			"the session's earlier jobs again", "refusal", refusal)
	return nil, nil
}

// statesTotalsOf reports whether raw is a harness result naming session, so
// what it states is that session's totals.
func (r *Runner) statesTotalsOf(raw []byte, session string) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
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
func (r *Runner) ownSpend(base *piece, result sandbox.Result, streamed *runSpend, rec sessionRecord,
	stated string) (sandbox.Result, error) {

	if streamed != nil {
		return streamed.on(result), nil
	}
	totals, ok := r.harness.(sessionTotals)
	if !ok || !rec.continued || base == nil {
		return result, nil
	}
	raw, refusal, err := base.whole()
	if err != nil {
		return sandbox.Result{}, err
	}
	if refusal != "" || len(bytes.TrimSpace(raw)) == 0 {
		// NO BASE: a run that started a session of its own, or one whose
		// base could not be kept ([Runner.baseResult] said so).
		return result, nil
	}
	if !totals.TotalsRestored(rec.release) {
		return result, nil
	}
	// Only a result naming the base's own session (stated) has them taken off:
	// one that stopped before its result states none.
	parsed := r.harness.Parse(raw)
	if !sandbox.ValidSessionID(stated) || stated != parsed.SessionID {
		return result, nil
	}
	return less(result, parsed), nil
}

// wrapperScript is the shell line a job runs under: the harness with its stdout
// and stderr redirected, then its result, its exit code and its done marker.
//
// The exit status is the harness's own: read into code before anything else
// runs, with the result line copied after the harness exits rather than piped
// beside it, since a plain `sh` has no pipefail. hooks is the directory the
// run's git takes its hooks from ([attributionExports]), or "" for a run that
// credits nothing; direct the hosts its harness reaches past any proxy
// ([noProxyExports]). A pin no assignment can carry
// ([envfile.FormatAssignment]) refuses the run before its job starts.
func wrapperScript(inner string, out Output, pins []Pinned, paths Paths, hooks string,
	direct []string) (string, error) {
	stdin := "/dev/null"
	if out.BriefOnStdin {
		stdin = shellword.Quote(paths.Brief())
	}
	switches, err := harnessSwitches(out, pins)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(switches)
	// AFTER THE PROFILE, BEFORE THE HARNESS, for the reason the unset is: an
	// entry exported around the shell would be replaced by a profile that
	// sets the count.
	if hooks != "" {
		b.WriteString(attributionExports(hooks))
	}
	b.WriteString(noProxyExports(direct))
	// THE JOB'S MEMORY DOMAIN EITHER SIDE OF ITS HARNESS, so a kill the kernel
	// made for memory meanwhile is told from any other end ([Paths.MemoryBefore]).
	fmt.Fprintf(&b, "%s\ncrewlet_memory > %s 2>/dev/null; ", sandbox.MemoryDomainScript,
		shellword.Quote(paths.MemoryBefore()))
	fmt.Fprintf(&b, "%s < %s > %s 2> %s; code=$?; crewlet_memory > %s 2>/dev/null; ",
		inner, stdin, shellword.Quote(out.Stdout), shellword.Quote(paths.Err()),
		shellword.Quote(paths.MemoryAfter()))
	if out.Result != "" && out.Result != out.Stdout {
		fmt.Fprintf(&b, "tail -n 1 %s > %s; ", shellword.Quote(out.Stdout), shellword.Quote(out.Result))
	}
	fmt.Fprintf(&b, "echo $code > %s; echo $code > %s",
		shellword.Quote(paths.ExitCode()), shellword.Quote(paths.Done()))
	return b.String(), nil
}

// harnessSwitches is the shell that sets a harness's environment as its
// launch does, after the login shell's profile: every [Output.Unset] name
// removed, then every pin exported over what any source gave it. The one
// text the wrapper and every read of the harness's release run
// ([releaseAsk]).
func harnessSwitches(out Output, pins []Pinned) (string, error) {
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
	return b.String(), nil
}

// releaseAsk is h's version command (version) run in a box as h's launch runs
// h: under [harnessSwitches] of h's [Harness.Output] and its pins for a request
// naming nothing, since a read of the release precedes any run and no pin a
// request moves picks the release (package doc, "A box's release is pinned").
// A subshell, so the switches end with the read and a script that goes on
// sees its own environment.
func releaseAsk(ctx context.Context, h Harness, paths Paths, version string) (string, error) {
	switches, err := harnessSwitches(h.Output(paths), h.Pins(ctx, sandbox.RunRequest{}, paths))
	if err != nil {
		return "", fmt.Errorf("executor: asking the box's %s for its release: %w", h.Name(), err)
	}
	return "(" + switches + "exec " + version + " < /dev/null)", nil
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
	return "PATH=" + shellword.Quote(paths.BinDir()) + `:"$PATH" ` + cmd
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
// decoded a line at a time ([eachLine]) and the error stream redacted from its
// start, its end kept ([Runner.readFailureTail]), so neither is refused for its size. A piece read
// whole (report, question, result line, exit code) past [sandbox.MaxFileBytes],
// or any piece whose path is not a regular file ([sandbox.ErrNotRegularFile],
// which a retry would read again), degrades only itself: described, the run
// not a success, its tokens, refs and transcript still collected and charged.
// A read that fails is an error. Everything is redacted whole before anything
// is bounded, since a secret cut in two survives as fragments; report and
// failure leave whole, and the transcript whole up to what the record keeps
// ([transcriptHoldBytes]), past it as the record keeps it, its redaction
// streamed ([transcriptLines]): the coordinator bounds what is sent, in one place.
func (r *Runner) Collect(ctx context.Context, box sandbox.Sandbox, handle sandbox.RunHandle,
	answers *queue.AnswerBudget) (sandbox.Result, error) {
	paths := PathsFor(box)
	out := r.harness.Output(paths)
	// EVERY PIECE OPENED, WEIGHED AND HELD before any is read (collectpieces.go).
	pieces, openErr := r.openCollection(ctx, box, paths, out)
	if openErr != nil {
		return sandbox.Result{}, openErr
	}
	defer pieces.close()
	release, holdErr := holdCollection(ctx, answers, pieces)
	if holdErr != nil {
		return sandbox.Result{}, holdErr
	}
	defer release()
	var refused []string

	var (
		result    sandbox.Result
		ended     bool
		spent     *runSpend
		request   contextRead
		window    int64
		limits    string
		requests  int
		unheld    string
		inFlight  int
		undecided int
	)
	if out.Events {
		streamed, refusal, err := r.decodeStream(pieces.stream, false)
		if err != nil {
			return sandbox.Result{}, err
		}
		if refusal != "" {
			refused = append(refused, refusal)
		}
		result, spent, request = streamed.result, streamed.spent, streamed.request
		window, limits, ended = streamed.window, streamed.limits, streamed.ended
		requests, unheld, inFlight = streamed.requests, streamed.unheld, streamed.inFlight
		undecided = streamed.undecided
	}
	// A HARNESS WHOSE OWN RECORD SHOWS ITS REQUESTS ([recordCollected]) is held to
	// its cap by that record, read as its live count reads it; one counted off
	// several ([recordsCounted]) by the largest, its stream's count of the
	// records it carries among them.
	if rc, ok := r.harness.(recordCollected); ok {
		count, err := r.collectedRecord(ctx, box, rc)
		if err != nil {
			return sandbox.Result{}, err
		}
		requests, undecided = count.Requests, count.Undecided
	}
	if rs, ok := r.harness.(recordsCounted); ok {
		count, unarmed, err := r.collectedRecords(ctx, box, rs, sandbox.RequestCount{Requests: requests,
			Undecided: undecided}, handle.TurnCap)
		if err != nil {
			return sandbox.Result{}, err
		}
		requests, undecided = count.Requests, count.Undecided
		if unheld == "" {
			unheld = unarmed
		}
	}
	// AND THE SIDE REQUESTS A RECORD KEEPS beside a stream that shows none of
	// them ([sideCounted]), added as its live count adds them.
	if sc, ok := r.harness.(sideCounted); ok {
		n, err := sc.sideCount().read(ctx, box, paths)
		if err != nil {
			return sandbox.Result{}, err
		}
		requests += n
	}
	// AND ONE WHOSE SESSION STORE DOES ([storeCounted]), asked as its live count
	// asks it.
	if sc, ok := r.harness.(storeCounted); ok {
		n, _, err := sc.storeRequests(ctx, box, paths)
		if err != nil {
			return sandbox.Result{}, err
		}
		requests, undecided = n, 0
	}
	// THE STREAM BEFORE THE RESULT FILE, keeping not its last line: what its
	// reading holds after is the record's halves, so the file is parsed beside
	// nothing; the line is read again only where it stands for the file.
	if out.Result != "" {
		raw, refusal, err := pieces.result.whole()
		if err != nil {
			return sandbox.Result{}, err
		}
		if out.lastLineStandsIn(refusal, raw) {
			again, err := r.redecodeStream(ctx, box, pieces.stream)
			if err != nil {
				return sandbox.Result{}, err
			}
			if again.last.said() {
				// The result file holds the stream's last line, copied by the
				// wrapper after the harness exits. Where the file is empty (the
				// process group died) or refused, that line is read in its
				// place, so a streamed session is not "no output" and its spend
				// is charged; a refused file is still reported. Only where the
				// stream said something: a harness that exited at once leaves
				// both empty.
				why := "was empty"
				if refusal != "" {
					refused = append(refused, refusal)
					why = "was refused"
				}
				raw, refusal = again.last.resultLine(out.Stdout)
				log.WarnContext(ctx, "harness_result_from_stream", "harness", r.harness.Name(),
					"detail", "the result file "+why+", so the event stream's last line, which the "+
						"wrapper copies into it, was read in its place")
			}
		}
		if refusal != "" {
			refused = append(refused, refusal)
		} else {
			parsed := r.harness.Parse(raw)
			if out.Events {
				parsed.Transcript = result.Transcript
				parsed.TranscriptElidedLines = result.TranscriptElidedLines
				parsed.TranscriptElidedBytes = result.TranscriptElidedBytes
			}
			result = parsed
			ended = ended || r.endsRun(raw)
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
	recorded, recordRefusal, err := pieces.record.text()
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
	// A MINTING HARNESS'S SESSION IS THE ONE ITS SCRIPT REPORTED, never one it
	// printed or its record names (minter.go): a refused report is a piece the
	// run did not report, so it collects no session and no capture, and the run
	// fails closed.
	if r.MintsSession() {
		minted, mintErr := r.reportedSession(ctx, box, paths, handle, rec)
		if errors.Is(mintErr, ErrSessionReport) {
			refused, minted = append(refused, mintErr.Error()), ""
		} else if mintErr != nil {
			return sandbox.Result{}, mintErr
		}
		result.SessionID, rec.id = minted, minted
	}
	result.Continued, result.HarnessVersion = rec.continued, releaseOf(rec.release)
	result.FellBack = sandbox.BoundFellBack(rec.why)
	if result.Captured, err = r.captured(ctx, box, paths, rec); err != nil {
		return sandbox.Result{}, fmt.Errorf("executor: reading the session the box packed: %w", err)
	}

	// WHAT THIS RUN SPENT, never its session's: a harness that continued a
	// session can state the session's spend, and charged as it is, every
	// continued run would pay for the jobs before it again ([Runner.ownSpend]).
	result, err = r.ownSpend(pieces.base, result, spent, rec, stated)
	if err != nil {
		return sandbox.Result{}, fmt.Errorf("executor: reading what the run itself spent: %w", err)
	}

	// The transcript is the observability surface of an agent with no
	// telemetry: the parser's from streamed events, else the error stream's
	// end, read only where it is shown, and once for the failure detail too.
	if refusal := pieces.stderr.refusal; refusal != "" {
		refused = append(refused, refusal)
	}
	stderrTail := sync.OnceValues(func() (failureTail, error) {
		return r.readFailureTail(ctx, pieces.stderr, r.failureBound, handle.FailureReadBy)
	})
	if result.Transcript == "" {
		tail, tailErr := stderrTail()
		if tailErr != nil {
			return sandbox.Result{}, tailErr
		}
		result.Transcript = tail.transcript()
	}

	codeText, refusal, err := pieces.exitCode.text()
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
	result, refused, err = memoryKills(pieces.memory, result, refused, code)
	if err != nil {
		return sandbox.Result{}, err
	}
	// A LIMIT ONLY AT THE HARNESS'S OWN API FAILURE ([limitGate]); a status
	// that does not read is no such failure.
	if gate, ok := r.harness.(limitGate); ok {
		if n, convErr := strconv.Atoi(code); convErr != nil || !gate.limitExit(n) {
			result.NoLimitExit = true
		}
	}

	// Whether the job was ended on request, as its script recorded
	// ([Paths.JobEnded]): such a run did not finish, whatever it wrote or exited
	// with (a harness may exit 0 on SIGTERM), so it is never a success and its
	// failure says so first. A word this build does not know is no end.
	endedText, refusal, err := pieces.ended.text()
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
	// for the text, and its presence means success unless the process crashed
	// or its harness never said how its run ended.
	findingsText, refusal, err := pieces.findings.text()
	if err != nil {
		return sandbox.Result{}, err
	}
	if refusal != "" {
		refused = append(refused, refusal)
	}
	findings := strings.TrimSpace(findingsText)
	// A HARNESS THAT NEVER SAID HOW ITS RUN ENDED did not finish it, whatever
	// it exited with or left behind ([Decoder.Ended]): a launcher an earlier
	// run replaced exits 0 having started nothing. It fails in its decoder's
	// words where they say why, else in [harnessNoResult], and a report it left
	// makes no success.
	if !ended {
		result.Success = false
		if strings.TrimSpace(result.Error) == "" {
			result.Error = harnessNoResult
		}
	}
	// What the failure says after any piece that could not be read: the
	// harness's own account where the run reported something, and where it
	// produced nothing at all, the exit status, the harness's error and the
	// error stream.
	var said []string
	errStream, unread := "", int64(0)
	switch {
	case findings != "":
		// The report's refs before the message's, by one rule over both
		// ([deliveredRefs]), so neither's named refs are outranked by a pull
		// request the other only mentions.
		result.DeliveredRefs = deliveredRefs(findings, result.Text)
		result.Text = findings
		// A RUN ITS GUARD STOPPED stays failed whatever it reported
		// ([sandbox.Result.Unbridged]).
		if !crashed && ended && !result.Unbridged {
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
		tail, tailErr := stderrTail()
		if tailErr != nil {
			return sandbox.Result{}, tailErr
		}
		said = append(said, result.Error, tail.note())
		errStream, unread = strings.TrimSpace(tail.shown), tail.before
	default:
		said = append(said, result.Error)
	}

	// A RUN ITS HARNESS ENDED WITHOUT CONNECTING ITS BRIDGE had none of its
	// seat's tools, so what it reported is no result ([bridgeUnconnected]); its
	// failure says so first. Not for a run ended on request or never ended,
	// whose own sentence says more of why nothing was listed.
	unbridged := handle.BridgeUnconnected && ended && result.JobEnd == ""
	if unbridged {
		result.Success = false
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
	if unbridged && handle.BridgeHolderChanged {
		sentences = append(sentences, bridgeHolderLost)
	} else if unbridged {
		sentences = append(sentences, bridgeUnconnected)
	}
	// A RUN PAST ITS CAP fails next, whatever it reported: decided here, after
	// the report's success above and nowhere else, so no runner can let a
	// defeated cap collect as a success ([capVerdict]).
	count := sandbox.RequestCount{Requests: requests, InFlight: inFlight, Undecided: undecided}
	if verdict, past := capVerdict(handle.TurnCap, count, result.RoundCapped); past {
		result.Success, result.TurnCapExceeded = false, verdict.exceeded
		result.RoundCapped = !verdict.exceeded
		sentences = append(sentences, verdict.sentence)
	}
	// AND A CAPPED RUN WHOSE STREAM MAY HIDE REQUESTS in lines past the bound
	// ([Decoder.Undecided]) that could take it past its cap: each such line
	// hides one request at most, so a run within its cap with all of them
	// counted is judged within it. ONE PREDICATE with the launching node's
	// watch, which ends a run as soon as this would fail it ([sandbox.RequestCount]).
	if count.Unjudged(handle.TurnCap) {
		result.Success = false
		sentences = append(sentences, fmt.Sprintf("the run's turn cap of %d could not be judged: %d line(s) of its "+
			"stream, past the %s a line is read within, may hold a model request whose marker neither end of the "+
			"line showed", handle.TurnCap, undecided, humanSize(int64(r.lineBound))))
	}
	// AND A RUN WHOSE CAP'S ENFORCER NEVER RAN, which a report made a success
	// above: a failure its decoder said is already among its sentences.
	if unheld != "" && result.Success {
		result.Success = false
		sentences = append(sentences, unheld)
	}
	sentences = append(append(sentences, refused...), said...)
	var once secretsOnce
	result.Error = failureDetail(sentences, errStream, unread, r.failureBound, once.of)

	result.Text = once.of(result.Text)
	result.Error = redact.Secrets(result.Error)
	// The refs too: a ref is whatever word the report put on its line,
	// and a word from inside the box can be a credential.
	for i, ref := range result.DeliveredRefs {
		result.DeliveredRefs[i] = redact.Secrets(ref)
	}
	// Whole up to the hold, as the report and the failure leave here: the
	// coordinator holds the transcript to the record's bound after redacting
	// it again ([sandbox.MaxRunTextBytes]).
	result.Transcript = redact.Secrets(result.Transcript)
	return result, nil
}

// capJudgment is [capVerdict]'s: which failure a run past its cap collects
// as, and the sentence that says so.
type capJudgment struct {
	exceeded bool
	sentence string
}

// capVerdict is the one verdict on a run whose stream shows more model
// requests than the cap its launch handed it ([sandbox.RunHandle.TurnCap]),
// past false for a run within it or with none. THE ENGINE'S CAP over THE
// STREAM'S COUNT ([Decoder.Requests]), never what the box says of either: the
// stream bounds a harness whose own enforcer died or ignored its cap, never a
// shell that rewrites the stream (docs/concepts/code-sandbox.md, "What ends a
// run"). A run past it is NEVER A SUCCESS: past by no more than its runner's
// in-flight requests ([Decoder.InFlight]), with its enforcer's stop in its
// stream (stopped), it collects as stopped at its cap; else as past it
// ([sandbox.RequestCount.Exceeds], the launching node's watch's predicate too).
func capVerdict(turnCap int, count sandbox.RequestCount, stopped bool) (capJudgment, bool) {
	if turnCap <= 0 || count.Requests <= turnCap {
		return capJudgment{}, false
	}
	if stopped && !count.Exceeds(turnCap) {
		return capJudgment{sentence: fmt.Sprintf("the run was stopped at its turn cap of %d: it made %d model "+
			"requests, the last already made as its harness was stopped", turnCap, count.Requests)}, true
	}
	return capJudgment{exceeded: true, sentence: fmt.Sprintf("the turn cap of %d was exceeded: the run made %d "+
		"model requests", turnCap, count.Requests)}, true
}

// bridgeUnconnected is a run's failure where its harness ended it without ever
// listing the tools of the engine's own server, its bridge (internal/api/mcpbridge,
// connected.go, measured eager on every runner): the likely causes, and where they
// are told apart.
const bridgeUnconnected = "the run's harness never connected its Crewlet bridge (it never listed the " +
	"tools of Crewlet's own MCP server, `crewlet`), so the run had none of its seat's tools and what it " +
	"reported is no result. Likely, a settings file in the box (its home's, the repository's, or a " +
	"system-wide or managed one), a plugin or an extension switched that server off or put one of its own " +
	"under its name; else the bridge did not answer the harness's startup in time. See Code Sandbox, " +
	"\"The tool bridge\", in Crewlet's documentation"

// bridgeHolderLost is bridgeUnconnected for a run collected for another node
// than the one that launched it ([sandbox.RunHandle.BridgeHolderChanged]): the
// node that saw its harness list may be gone with what it saw, so the failure
// says what is known, never that the harness did not connect.
const bridgeHolderLost = "the node that served the run's bridge was lost before it recorded the connection: " +
	"the run's seat moved to another node after its launch, and the run's record holds no word that its " +
	"harness connected its Crewlet bridge (listed the tools of Crewlet's own MCP server, `crewlet`), so " +
	"whether the run had its seat's tools is unknown and what it reported is no result. See Code Sandbox, " +
	"\"The tool bridge\", in Crewlet's documentation"

// harnessNoResult is a run's failure where its harness exited without saying
// how its run ended and its decoder gave no reason of its own.
const harnessNoResult = "the harness exited without reporting how its run ended, so it produced no result"

// endsRun reports whether a result file read apart from the stream says the
// run ended ([Decoder.Ended]), fed whole to a fresh decoder: the file is the
// stream's last line, copied, and read to [sandbox.MaxFileBytes], where a
// stream line past [Runner.lineBound] is skipped unread.
func (r *Runner) endsRun(raw []byte) bool {
	dec := r.harness.Events()
	for line := range bytes.Lines(raw) {
		dec.Line(bytes.TrimRight(line, "\r\n"))
	}
	return dec.Ended()
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
// line too long to keep whole. secrets redacts the sentences joined ([secretsOnce]).
func failureDetail(sentences []string, errStream string, unread int64, bound int, secrets func(string) string) string {
	var parts []string
	for _, s := range sentences {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	head := secrets(strings.Join(parts, ":\n"))
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

// secretsOnce is [redact.Secrets] of each text once a collection: a failed
// result's text is both its report and its harness's error, one string
// ([ClaudeCode.Parse]), and redacted for each it was copied twice.
type secretsOnce struct{ in, out []string }

// of is text redacted, as it was the first time where it was redacted before.
// A string compares to itself in constant time, so a text seen costs nothing.
func (s *secretsOnce) of(text string) string {
	for i, seen := range s.in {
		if seen == text {
			return s.out[i]
		}
	}
	redacted := redact.Secrets(text)
	s.in, s.out = append(s.in, text), append(s.out, redacted)
	return redacted
}

// unreadNote is the line standing where the start of the error stream was
// left out of a failure: none says nothing of it was shown at all.
func unreadNote(unread int64, none bool) string {
	if none {
		return fmt.Sprintf("(the error stream, %s, is not shown: the engine's own account of the "+
			"run fills what one condensation can take)", humanSize(unread))
	}
	return fmt.Sprintf("(the error stream's first %s are not shown: a run's failure shows the end of "+
		"its redaction, as much of it as one condensation can take beside the engine's own account)",
		humanSize(unread))
}

// decodeStream reads the event stream a collection opened through a fresh
// decoder, no further than the size it was weighed at, and keeps its last line
// where keepLast asks, which [Runner.Collect] reads again for where the line
// stands for the result file — or answers the refusal of a stream the box
// would not open because it is not a regular file ([notRegularPiece]).
func (r *Runner) decodeStream(stream *piece, keepLast bool) (decodedStream, string, error) {
	if stream.refusal != "" {
		return decodedStream{request: contextRead{why: "the run's event stream was not read: " +
			stream.refusal}}, stream.refusal, nil
	}
	defer stream.close()
	return r.decodeLines(io.LimitReader(stream.body, stream.size), keepLast)
}

// redecodeStream reads the stream a collection opened again, keeping its last
// line: opened anew, and read no further than the size the first open was
// weighed at, so a file renamed over it since adds nothing to what is held.
func (r *Runner) redecodeStream(ctx context.Context, box sandbox.Sandbox, stream *piece) (decodedStream, error) {
	if stream.refusal != "" || stream.size == 0 {
		return decodedStream{}, nil
	}
	body, _, err := box.OpenRange(ctx, stream.path, 0, stream.size)
	if err != nil {
		return decodedStream{}, fmt.Errorf("executor: opening the event stream again: %w", err)
	}
	defer func() { _ = body.Close() }()
	again, _, err := r.decodeLines(io.LimitReader(body, stream.size), true)
	return again, err
}

// decodeLines decodes an event stream's lines from stream through a fresh
// decoder.
func (r *Runner) decodeLines(stream io.Reader, keepLast bool) (decodedStream, string, error) {
	events := r.harness.Events()
	kept := &keepingLast{Decoder: events}
	dec := events
	if keepLast {
		dec = kept
	}
	if err := eachLine(stream, dec, r.lineBound); err != nil {
		return decodedStream{}, "", fmt.Errorf("executor: reading the event stream: %w", err)
	}
	out := decodedStream{result: events.Result(), last: kept.last, ended: events.Ended(),
		requests: events.Requests(), inFlight: events.InFlight(), undecided: events.Undecided()}
	if spender, ok := events.(runSpender); ok {
		if spent, stated := spender.runSpend(); stated {
			out.spent = &spent
		}
	}
	if reader, ok := events.(contextReader); ok {
		out.request = reader.request()
	}
	if reporter, ok := events.(windowReporter); ok {
		out.window = reporter.reportedWindow()
	}
	if reporter, ok := events.(limitReporter); ok {
		out.limits = reporter.limitReport()
	}
	if reporter, ok := events.(enforcerReporter); ok {
		out.unheld = reporter.unheld()
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
	// ended is whether the stream said how the run ended ([Decoder.Ended]).
	ended bool
	// requests is the model requests the stream shows ([Decoder.Requests]).
	requests int
	// unheld is why what holds the run to its cap never ran
	// ([enforcerReporter]), "" where it ran.
	unheld string
	// inFlight is the requests the runner's enforcer cannot stop
	// ([Decoder.InFlight]); undecided the skipped lines that may hold a
	// request the count could not read ([Decoder.Undecided]).
	inFlight, undecided int
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

// lastLineStandsIn reports whether the stream's last line is read in place of
// the result file ([Runner.Collect]): the file, read as raw or refused, is
// empty or refused, and is a file of its own beside a stream the wrapper copied
// the line from.
func (o Output) lastLineStandsIn(refusal string, raw []byte) bool {
	return o.Events && o.Result != "" && o.Result != o.Stdout && (refusal != "" || len(bytes.TrimSpace(raw)) == 0)
}

// resultLine is the stream's last line read in place of the result file: the
// line, or a refusal where it was past what one line of a stream may hold — as
// the copy itself would have been past what a result file is read to.
func (l streamLast) resultLine(stream string) ([]byte, string) {
	if l.skipped > 0 {
		return nil, fmt.Sprintf("the result its harness printed (the last line of %s, read in place of "+
			"the result file) is %s, past the %s one line of a run's output may hold, "+
			"so it was not read", stream, humanSize(l.skipped), humanSize(int64(l.bound)))
	}
	return l.line, ""
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

func (k *keepingLast) Skipped(line LongLine) {
	k.last.line, k.last.skipped, k.last.bound = k.last.line[:0], line.Size, line.Bound
	k.Decoder.Skipped(line)
}

// readWhole reads a file meant to be read whole, answering a file past
// [sandbox.MaxFileBytes], or a path that is not a regular file, with a
// REFUSAL — a sentence saying what it was — rather than an error, so the piece
// degrades and the collection does not.
func readWhole(ctx context.Context, box sandbox.Sandbox, path, what string) (string, string, error) {
	raw, refusal, err := readWholeBytes(ctx, box, path, what)
	return string(raw), refusal, err
}

// readWholeBytes is [readWhole] as the box read it, for a reader that decodes
// the bytes ([Harness.Parse]) rather than holding them as text beside a copy.
func readWholeBytes(ctx context.Context, box sandbox.Sandbox, path, what string) ([]byte, string, error) {
	raw, err := box.ReadFile(ctx, path)
	if refusal := notRegularPiece(err, path, what); refusal != "" {
		return nil, refusal, nil
	}
	var tooLarge *sandbox.FileTooLargeError
	switch {
	case errors.As(err, &tooLarge):
		return nil, refusedPiece(ctx, box, path, what, tooLarge.Limit), nil
	case err != nil:
		return nil, "", err
	}
	return raw, "", nil
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
func memoryKills(records [2]*piece, result sandbox.Result, refused []string,
	code string) (sandbox.Result, []string, error) {

	var read [2]sandbox.MemoryDomain
	for i, record := range records {
		text, refusal, err := record.text()
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
