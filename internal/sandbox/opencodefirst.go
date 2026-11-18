package sandbox

import (
	"context"
	"slices"
	"strings"
)

// WHETHER AN OPENCODE CONTINUATION'S FIRST REQUEST SENDS ITS SESSION WHOLE:
// decided by the launch, in the box it gets, off that box's own point; the
// package doc says why the runner cannot decide it.

// PointMark is why a box's OpenCode point could not be read for a launch, ""
// where it was ([OpenCodePoint.Mark]). Every mark is read as a session that
// may compact before its first request. A named string, so a word a later
// build writes reads as the value it is rather than a panic; marks never leave
// the launch that read them.
type PointMark string

// The marks, IN PRECEDENCE ORDER ([PointMarks]).
const (
	// PointUnread is a read that failed, ran past its wait, or answered
	// something this build cannot read.
	PointUnread PointMark = "unread"
	// PointRelease is a box whose OpenCode is a release outside the set whose
	// rules were read and measured (internal/sandbox/executor's
	// openCodeCountRead).
	PointRelease PointMark = "release"
	// PointPair is a box whose release is not the one that wrote the session:
	// no pair of differing releases has been measured, so none is judged.
	PointPair PointMark = "pair"
	// PointPlugin is a plugin other than the engine's own, named by a box
	// document or found in a plugin folder: a plugin runs in the server and
	// can compact the session or add to it before the first request.
	PointPlugin PointMark = "plugin"
	// PointContextManagement is a document that sets a provider's
	// `contextManagement` option, which edits the conversation server-side.
	PointContextManagement PointMark = "contextmanagement"
	// PointBoxSources is, in the box that holds the session's copy, a box
	// document that adds a context source the read cannot size: an MCP
	// server, a skills path, an instructions entry, a reference or Code Mode.
	PointBoxSources PointMark = "boxsources"
	// PointNoModel is a launch that names no `provider/model`.
	PointNoModel PointMark = "nomodel"
	// PointAbsent is a model the box's catalog does not list (its provider's
	// credential not in the box among the causes).
	PointAbsent PointMark = "absent"
	// PointNative is a model whose compaction is its provider's own.
	PointNative PointMark = "native"
	// PointDefaulted is a model whose limit nothing states, so OpenCode holds
	// it to its default record's 200,000 tokens, which may not be the
	// server's window.
	PointDefaulted PointMark = "defaulted"
	// PointNoLimit is a model whose limit reads as zero with compaction on:
	// only the provider's refusal compacts it, at a size nothing here knows.
	PointNoLimit PointMark = "nolimit"
)

// PointMarks are the marks this build writes, in precedence order: a read
// that finds several keeps the first.
var PointMarks = []PointMark{PointUnread, PointRelease, PointPair, PointPlugin, PointContextManagement,
	PointBoxSources, PointNoModel, PointAbsent, PointNative, PointDefaulted, PointNoLimit}

// Valid reports whether this build knows m.
func (m PointMark) Valid() bool { return slices.Contains(PointMarks, m) }

// OpenCodePoint is what a box's OpenCode compacts a continued session's first
// request at, read in that box at the launch ([PointReader]).
type OpenCodePoint struct {
	// Release is the box's OpenCode release, as it answered it.
	Release string
	// Tokens is the point: OpenCode compacts where its count of the session
	// with the brief is at least it. Meaningful only where Mark is "" and
	// Never is false; at or below zero, every request compacts first.
	Tokens int64
	// Never says the box's configuration turns compaction off
	// (`compaction.auto: false`), which skips every reason but a manual one,
	// the provider's refusal included: the session is sent whole whatever
	// its count.
	Never bool
	// Mark is why there is no point, "" where there is one.
	Mark PointMark
	// Why says Mark in a sentence, redacted.
	Why string
	// BoxBytes is what the box hands over before the brief, in bytes, where
	// it holds the session's copy ([PointAsk.Sources]); 0 elsewhere.
	BoxBytes int64
	// Plugins is the plugins a box document names, by package, and the
	// entries of every plugin folder the read listed, by path: never a
	// plugin's options, which may hold a credential.
	Plugins []string
}

// OpenCodeCharge is OpenCode's estimate of n bytes of text, rounded up: at
// least its own round(UTF-16 length / 4) of any text of n UTF-8 bytes, since a
// string's UTF-8 bytes are never fewer than its UTF-16 units.
func OpenCodeCharge(n int64) int64 { return (max(n, 0) + 3) / 4 }

// SendsWhole reports whether OpenCode, at p, sends a continued session's
// first request without compacting it first: compaction off (Never), or
// count's bound, with the brief and what the box hands over before it,
// STRICTLY BELOW the point (OpenCode compacts where its count is at least the
// point, and the bound is at least its count). False where p is marked, the
// count has no bound, or its reply's provider (countProvider) is not the
// launch's (provider): OpenCode anchors its count on a reply of the run's
// current provider alone, so another provider's count bounds nothing.
func (p OpenCodePoint) SendsWhole(count *ResumedCount, countProvider, provider, brief string) bool {
	switch {
	case p.Mark != "":
		return false
	case p.Never:
		return true
	}
	bound, ok := count.Bound()
	if !ok || countProvider == "" || countProvider != provider {
		return false
	}
	return bound+OpenCodeCharge(int64(len(brief)))+OpenCodeCharge(p.BoxBytes) < p.Tokens
}

// ModelProvider is the provider a `--model` argument names, as OpenCode
// splits it at its first slash ("anthropic/claude-sonnet-5" is anthropic's;
// "openrouter/anthropic/claude-sonnet-5" is openrouter's), and the model, a
// `#variant` dropped; both "" where it names no provider.
func ModelProvider(model string) (provider, id string) {
	model, _, _ = strings.Cut(model, "#")
	provider, id, ok := strings.Cut(model, "/")
	if !ok || provider == "" || id == "" {
		return "", ""
	}
	return provider, id
}

// PointAsk is what a launch asks a box's OpenCode point for ([PointReader]).
type PointAsk struct {
	// Model is the launch's `--model` ([LaunchRequest.Model]).
	Model string
	// Release is the box's release where the launch already read it
	// ([CarryCheck.Version]), "" where it did not: the read asks.
	Release string
	// Writer is the release that wrote the session: a box of any other is
	// no point ([PointPair]).
	Writer string
	// Env is the environment the run is handed, but for its telemetry: the
	// documents and credentials the run's server reads are the ones the read
	// sees.
	Env map[string]string
	// Sources is what the run hands its server, read where the box holds the
	// session's copy (the copy continued in place, or the box the capture was
	// taken in); nil after an import into another box, which hands none.
	Sources *RunSources
}

// RunSources is the run's own context sources, as the launch hands them to its
// box: what OpenCode tells a session in the box that holds it before its
// first request, where they changed since its last ([PointAsk.Sources]).
type RunSources struct {
	// System is the run's own instructions ([buildSystem]), Reported the
	// repositories its report is told not to name ([RunRequest.ReportedRepos]),
	// which its runner adds to them, and Steered whether its harness is told
	// of notes beside them.
	System   string
	Reported []string
	Steered  bool
	// Skills is the run's own skills ([RunRequest.Skills]): their listing.
	Skills []Skill
	// Guidance is the instructions of each MCP server the run's bridge serves
	// it, by server ([LaunchRequest.MCPGuidance]).
	Guidance map[string]string
	// Unsized is every MCP server the run's box dials itself
	// ([LaunchRequest.MCPServers]), whose instructions it says to OpenCode
	// and nothing here knows: in the box that holds the copy, no point
	// ([PointBoxSources]).
	Unsized []string
}

// PointReader is a [Runner] that reads a box's OpenCode point before a run
// continues a session there (internal/sandbox/executor's Runner). OPTIONAL:
// a runner that reads none leaves every OpenCode continuation told as one
// that may be a summary. Three answers, as [SessionRestorer.CheckCarry]'s: a
// point, a mark, and an error where the box could not be asked.
type PointReader interface {
	ReadOpenCodePoint(ctx context.Context, box Sandbox, ask PointAsk) (OpenCodePoint, error)
}

// SummarizedTelling is a relaunch's briefs told as a continuation that may be
// a summary, beside the held telling, where the launch decides between them
// in its box ([OpenCodePoint.SendsWhole]): every call of the round from its
// start, every lead its record keeps, and a guard that arms nothing.
type SummarizedTelling struct {
	// Brief is the run's prompt in the box the session was last worked in,
	// Replaced in any other.
	Brief, Replaced string
	// Told is what Brief and Replaced told of the round ([BriefTold]).
	Told BriefTold
}
