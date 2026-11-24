package sandbox

import (
	"context"
	"slices"
	"strings"
)

// WHETHER A CONTINUATION'S FIRST REQUEST SENDS ITS SESSION WHOLE, for a
// harness whose point its box sets (OpenCode, Gemini CLI, Qwen Code): decided
// by the launch, in the box it gets, off that box's own point; the package doc
// says why the runner cannot decide it.

// PointMark is why a box's point could not be read for a launch, "" where it
// was ([BoxPoint.Mark]). Every mark is read as a session that may compact
// before its first request. A named string, so a word a later build writes
// reads as the value it is rather than a panic; marks never leave the launch
// that read them.
type PointMark string

// The marks, IN PRECEDENCE ORDER ([PointMarks]).
const (
	// PointUnread is a read that failed, ran past its wait, or answered
	// something this build cannot read.
	PointUnread PointMark = "unread"
	// PointRelease is a box whose harness is a release outside the set whose
	// rules were read and measured (internal/sandbox/executor's
	// openCodeCountRead, geminiRelease, qwenRelease).
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
	// PointBoxSources is a Gemini CLI box whose session context (its
	// workspace's tree and GEMINI.md files) is past what the read sizes or
	// the count allows for.
	PointBoxSources PointMark = "boxsources"
	// PointNoModel is a launch that names no `provider/model`, or a Gemini
	// CLI one that names no model, whose window was not measured.
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
	// PointSettings is a Gemini CLI or Qwen Code box whose settings, `.env`
	// files, environment or sign-in can move the point, its window or what
	// the first request masks or clears: found by the key's name in the
	// source's raw bytes, never parsed, so a false find only tells more.
	PointSettings PointMark = "settings"
)

// PointMarks are the marks this build writes, in precedence order: a read
// that finds several keeps the first.
var PointMarks = []PointMark{PointUnread, PointRelease, PointPair, PointPlugin, PointContextManagement,
	PointBoxSources, PointNoModel, PointAbsent, PointNative, PointDefaulted, PointNoLimit, PointSettings}

// Valid reports whether this build knows m.
func (m PointMark) Valid() bool { return slices.Contains(PointMarks, m) }

// BoxPoint is what a box's harness compacts a continued session's first
// request at, read in that box at the launch ([PointReader];
// [FirstRequestBox]).
type BoxPoint struct {
	// Release is the box's harness release, as it answered it.
	Release string
	// Tokens is the point: the harness compacts where its count of the
	// session (OpenCode's with the brief) is at least it. Meaningful only
	// where Mark is "" and Never is false; at or below zero, every request
	// compacts first.
	Tokens int64
	// Anchored says the harness anchors its count on a reply of the run's
	// current provider alone (OpenCode), so a count is judged only where its
	// provider is the launch's; unset, a count names no provider and is
	// judged whatever the launch's.
	Anchored bool
	// Never says the box's configuration turns compaction off
	// (`compaction.auto: false`), which skips every reason but a manual one,
	// the provider's refusal included: the session is sent whole whatever
	// its count.
	Never bool
	// Mark is why there is no point, "" where there is one.
	Mark PointMark
	// Why says Mark in a sentence, redacted.
	Why string
	// BoxBytes is what the box hands over beside the brief before the first
	// request, in bytes: Gemini CLI's session context, Qwen Code's dated
	// reminder; 0 for OpenCode, whose re-import hands over none.
	BoxBytes int64
	// Plugins is the plugins a box document names, by package, and the
	// entries of every plugin folder the read listed, by path: never a
	// plugin's options, which may hold a credential.
	Plugins []string
	// Charge is what the brief and BoxBytes count for in the harness's own
	// estimate, as the box's runner states it.
	Charge Charge
}

// SendsWhole reports whether the box's harness, at p, sends a continued
// session's first request without compacting it first: compaction off
// (Never), or count's bound, with the brief and what the box hands over
// before it (BoxBytes), STRICTLY BELOW the point (each harness compacts where
// its count is at least the point, and the bound is at least its count).
// OpenCode's box hands over nothing: a box that holds the copy re-imports it
// first, which tells the session none of the sources that changed
// (internal/sandbox/executor). False where p is marked, the count has no
// bound, or, where p is Anchored, its reply's provider (countProvider) is not
// the launch's (provider): OpenCode anchors its count on a reply of the run's
// current provider alone, so another provider's count bounds nothing.
func (p BoxPoint) SendsWhole(count *ResumedCount, countProvider, provider, brief string) bool {
	switch {
	case p.Mark != "":
		return false
	case p.Never:
		return true
	}
	bound, ok := count.Bound()
	if !ok || p.Anchored && (countProvider == "" || countProvider != provider) {
		return false
	}
	return bound+p.Charge.Tokens(int64(len(brief)))+p.Charge.Tokens(p.BoxBytes) < p.Tokens
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

// PointAsk is what a launch asks a box's point for ([PointReader]).
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
}

// PointReader is a [Runner] that reads a box's point before a run
// continues a session there (internal/sandbox/executor's Runner). OPTIONAL:
// a runner that reads none leaves every continuation judged in its box told as one
// that may be a summary. Three answers, as [SessionRestorer.CheckCarry]'s: a
// point, a mark, and an error where the box could not be asked.
type PointReader interface {
	ReadBoxPoint(ctx context.Context, box Sandbox, ask PointAsk) (BoxPoint, error)
}

// SummarizedTelling is a relaunch's briefs told as a continuation that may be
// a summary, beside the held telling, where the launch decides between them
// in its box ([BoxPoint.SendsWhole]): every call of the round from its
// start, every lead its record keeps, and a guard that arms nothing.
type SummarizedTelling struct {
	// Brief is the run's prompt in the box the session was last worked in,
	// Replaced in any other.
	Brief, Replaced string
	// Told is what Brief and Replaced told of the round ([BriefTold]).
	Told BriefTold
}
