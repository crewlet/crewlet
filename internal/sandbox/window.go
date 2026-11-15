package sandbox

import (
	"regexp"
	"slices"
	"strings"
)

// ClaudeCodeHarness is the Claude Code runner's name, the one harness whose
// compaction window the engine pins ([ClaudeCodeWindow]).
const ClaudeCodeHarness = "claude-code"

// ExecutorWindow is the context window a Claude Code executor run is pinned to
// (CLAUDE_CODE_AUTO_COMPACT_WINDOW), the window of the model the run works
// under, and the least window the harness holds a run of that model to.
//
// THE HARNESS COMPACTS UNDER THE SMALLER OF THE PIN AND ITS OWN WINDOW FOR THE
// MODEL, and keeps its own headroom below it: a reply reserve, then a
// compaction buffer ([ExecutorWindow.SendsWhole]), so the engine adds none.
type ExecutorWindow struct {
	// Tokens is the pin: the model's own window at its largest, the most the
	// harness may hold the run to, which the launch floor judges a continued
	// session under ([ExecutorWindow.Carries]).
	Tokens int
	// Standard is the least window Claude Code 2.1.310 and 2.1.322 hold a
	// run of the model to, where nothing served to the account or opted into
	// raises it, on the route the run reaches it by ([ClaudeCodeWindowOn]):
	// what a continuation's telling is judged under
	// ([ExecutorWindow.SendsWhole]). Claude Code holds claude-opus-4-6 and
	// claude-sonnet-4-6 to 200,000 tokens unless the account is served more or
	// the argument opts in with `[1m]`, though the pin is their 1,000,000;
	// an alias can name an older model on a cloud platform or a gateway; and
	// no model, or `default`, is the account's default model, which may be
	// one of those. Zero, with Known true, is a window this build cannot say.
	Standard int
	// Known is false where this build does not know the model, and Tokens
	// and Standard are [FallbackExecutorWindow].
	Known bool
}

// FallbackExecutorWindow is the window of a model this build does not know: the
// standard window of every Claude model, so the harness's clamp never lowers it on
// a current model, and room for a run's first request several times over.
const FallbackExecutorWindow = 200_000

// claudeLongWindow is the window of every current Claude model and of a model
// argument that opts into the long window (`[1m]`).
const claudeLongWindow = 1_000_000

// claudeStandardWindow is the window Claude Code holds a model to that it does
// not serve its long window: claude-opus-4-6 and claude-sonnet-4-6 unless the
// account is served more or the argument opts in, every older model, and a
// model a release does not know (2.1.322's catalog and its default for a model
// it lacks).
const claudeStandardWindow = 200_000

// claudeReplyReserve is the most of a window Claude Code keeps back for a reply:
// min(maxOutputTokens, 20000), 2.1.310 and 2.1.322 alike, and every model in
// the table has a default output of at least 32,000 tokens, so 20,000 it is
// (a lower CLAUDE_CODE_MAX_OUTPUT_TOKENS only raises the compaction point). A
// request past the rest is one the harness cannot send whole: it reaches its
// blocked level and compacts first, dropping its oldest messages where even the
// summary's request cannot carry them. The launch floor keeps a continued
// session below it ([ExecutorWindow.Carries]), so one request can carry it and
// the budget holds it whole; the point the harness compacts at is lower
// ([claudeCompactBuffer]).
const claudeReplyReserve = 20_000

// claudeCompactBuffer is the room Claude Code keeps between its compaction
// point and the rest of its window less [claudeReplyReserve]: it compacts a
// conversation before a request where its own count of it reaches the window
// less the reserve less this (`let r=e-13000` in 2.1.310's and 2.1.322's
// threshold code; a percentage override can only lower it, and the run's
// environment and settings unset and blank that override). Measured: at a
// 200,000-token window the point is 167,000, a count of 165,922 sent whole and
// one of 167,834 compacted, on both releases (docs/concepts/code-sandbox.md).
const claudeCompactBuffer = 13_000

// claudeTurnAttachments is what a resumed run adds to its session beside its
// brief before its first request, at most, in tokens at [ClaudeCharge]: the
// records a resume writes that the session's thread does not hold yet, and its
// turn's attachments.
//
// TWENTY THOUSAND, measured: beside its brief a resume added at most 2,597
// bytes of records the archive does not hold (the synthetic result for its open
// call, the `<synthetic>` reply and the token reminder), 1,301 tokens at
// ClaudeCharge, and its turn's attachments alone came to at most 2,232 bytes (a
// deferred-tools reminder and the token reminder), 1,118 tokens, on 2.1.310 and
// 2.1.322 alike. NOT COVERED, stated: those runs' boxes were unchanged, and a
// box whose skills changed lists them again, up to 40,000 characters at a
// 1,000,000-token window (about 13,333 tokens at three a character) and up to
// 100,000 where a listing budget is served, beside other large deltas; a whole
// first turn's set was at most 39,064 bytes in the census, 19,532 tokens at
// ClaudeCharge.
const claudeTurnAttachments = 20_000

// ClaudeCharge is the most tokens n bytes of a Claude Code session can count
// for, by the harness's own estimate of what lies after its last counted
// reply: ⌈n/2⌉. The harness counts a text at its UTF-16 length over three or
// four (by the model), a JSON-titled text document at its length over two, a
// call at its JSON over three or four, so two bytes a token is at most every
// rate it charges, and a line's bytes are never fewer than its text's UTF-16
// length. A later release that counts text below three a character is still
// covered, which a tighter three bytes a token would not be.
func ClaudeCharge(n int) int64 {
	if n <= 0 {
		return 0
	}
	return (int64(n) + 1) / 2
}

// ClaudeRoute is how a Claude Code run reaches its model, as far as the windows
// it holds a model to differ by it ([ClaudeCodeWindowOn]): an alias names
// another model on a cloud platform or a gateway than on Anthropic's own API.
type ClaudeRoute string

const (
	// ClaudeRouteAnthropic is Anthropic's own API, or an endpoint the harness
	// reaches as it does that API (a base URL of its own and a token): what
	// an entry signed in with a login or a key reaches, and one whose env
	// names no cloud platform.
	ClaudeRouteAnthropic ClaudeRoute = "anthropic"
	// ClaudeRouteBedrock is Amazon Bedrock (CLAUDE_CODE_USE_BEDROCK).
	ClaudeRouteBedrock ClaudeRoute = "bedrock"
	// ClaudeRouteVertex is Google Cloud's Vertex AI (CLAUDE_CODE_USE_VERTEX).
	ClaudeRouteVertex ClaudeRoute = "vertex"
	// ClaudeRouteFoundry is Microsoft Foundry (CLAUDE_CODE_USE_FOUNDRY).
	ClaudeRouteFoundry ClaudeRoute = "foundry"
	// ClaudeRouteElsewhere is a route this build cannot name: a gateway the
	// harness's host signs into, another platform, or an entry whose route is
	// not read. Held to the least window of every route but Anthropic's own,
	// the smaller side.
	ClaudeRouteElsewhere ClaudeRoute = "elsewhere"
)

// ClaudeRoutes are the routes this build names.
var ClaudeRoutes = []ClaudeRoute{ClaudeRouteAnthropic, ClaudeRouteBedrock, ClaudeRouteVertex, ClaudeRouteFoundry,
	ClaudeRouteElsewhere}

// Valid reports whether this build knows r. [ClaudeCodeWindowOn] judges a
// route it does not know as [ClaudeRouteElsewhere].
func (r ClaudeRoute) Valid() bool { return slices.Contains(ClaudeRoutes, r) }

// claudeWindows is each model's own window at its largest, the pin, by the
// canonical id or the alias a providers.llm entry hands Claude Code.
var claudeWindows = map[string]int{
	"claude-fable-5-2":  claudeLongWindow,
	"claude-fable-5-1":  claudeLongWindow,
	"claude-fable-5":    claudeLongWindow,
	"claude-mythos-5-2": claudeLongWindow,
	"claude-mythos-5-1": claudeLongWindow,
	"claude-mythos-5":   claudeLongWindow,
	"claude-opus-5-5":   claudeLongWindow,
	"claude-opus-5":     claudeLongWindow,
	"claude-opus-4-8":   claudeLongWindow,
	"claude-opus-4-7":   claudeLongWindow,
	"claude-opus-4-6":   claudeLongWindow,
	"claude-sonnet-5-5": claudeLongWindow,
	"claude-sonnet-5":   claudeLongWindow,
	"claude-sonnet-4-6": claudeLongWindow,
	"claude-haiku-5-5":  claudeLongWindow,
	"claude-opus-4-5":   claudeStandardWindow,
	"claude-sonnet-4-5": claudeStandardWindow,
	"claude-haiku-4-5":  claudeStandardWindow,
	"default":           claudeLongWindow,
	"best":              claudeLongWindow,
	"opus":              claudeLongWindow,
	"opusplan":          claudeLongWindow,
	"sonnet":            claudeLongWindow,
	"haiku":             claudeLongWindow,
	"fable":             claudeLongWindow,
}

// claudeStandardWindows is, for each model argument whose least window
// ([ExecutorWindow.Standard]) is less than its pin on some route, that window
// by route; every other known argument's is its pin on every route. Read in the
// model catalogs and alias tables of Claude Code 2.1.310 and 2.1.322:
//
//   - claude-opus-4-6 and claude-sonnet-4-6 are held to 200,000 unless the
//     account is served a larger window or the argument opts in (`[1m]`);
//   - `opus` names claude-opus-5-5, but claude-opus-4-6 on Foundry, and `best`
//     names `opus` on 2.1.322 (`fable` on 2.1.310);
//   - `sonnet` names claude-sonnet-5-5 on Anthropic's API, and claude-sonnet-4-5
//     or claude-sonnet-4-6 on every other route, `haiku` claude-haiku-4-5 off
//     Anthropic's API, and `opusplan` plans on `opus` and works on `sonnet`;
//   - no model, and `default`, is the account's default, which an organization,
//     ANTHROPIC_DEFAULT_MODEL or the account's tier can make claude-opus-4-6 or
//     claude-sonnet-4-6 on any route;
//   - `fable` names a long-window model on every route.
var claudeStandardWindows = map[string]map[ClaudeRoute]int{
	"claude-opus-4-6":   allRoutes(claudeStandardWindow),
	"claude-sonnet-4-6": allRoutes(claudeStandardWindow),
	"default":           allRoutes(claudeStandardWindow),
	"opus":              offFoundry(claudeLongWindow),
	"best":              offFoundry(claudeLongWindow),
	"sonnet":            onAnthropicAlone(claudeLongWindow),
	"haiku":             onAnthropicAlone(claudeLongWindow),
	"opusplan":          onAnthropicAlone(claudeLongWindow),
}

// allRoutes is window on every route.
func allRoutes(window int) map[ClaudeRoute]int {
	out := map[ClaudeRoute]int{}
	for _, r := range ClaudeRoutes {
		out[r] = window
	}
	return out
}

// offFoundry is window on Anthropic's API, Bedrock and Vertex, and the standard
// window on Foundry and on a route this build cannot name.
func offFoundry(window int) map[ClaudeRoute]int {
	out := allRoutes(window)
	out[ClaudeRouteFoundry], out[ClaudeRouteElsewhere] = claudeStandardWindow, claudeStandardWindow
	return out
}

// onAnthropicAlone is window on Anthropic's API and the standard window on
// every other route.
func onAnthropicAlone(window int) map[ClaudeRoute]int {
	out := allRoutes(claudeStandardWindow)
	out[ClaudeRouteAnthropic] = window
	return out
}

// claudeModelDecorations are what a platform adds around a model's canonical id:
// a Bedrock region and vendor prefix, a Bedrock version suffix, and a dated
// snapshot written with `-` (Claude API, Bedrock) or `@` (Vertex AI).
var claudeModelDecorations = regexp.MustCompile(
	`^(?:(?:[a-z]{2,6}\.)?anthropic\.)?(.+?)(?:[-@]\d{8})?(?:-v\d+(?::\d+)?)?$`)

// ClaudeCodeWindow is the window a Claude Code run on model is pinned to: the
// model's own, or [FallbackExecutorWindow] where this build does not know it,
// with its least window on Anthropic's own API ([ClaudeCodeWindowOn]). An empty
// model is the harness's default model.
func ClaudeCodeWindow(model string) ExecutorWindow {
	return ClaudeCodeWindowOn(model, ClaudeRouteAnthropic)
}

// ClaudeCodeWindowOn is the window a Claude Code run on model, reaching it by
// route, is pinned to ([ExecutorWindow.Tokens], the same on every route), and
// the least window the harness holds it to there ([ExecutorWindow.Standard]):
// [FallbackExecutorWindow] for both where this build does not know the model,
// and a route it does not know judged as [ClaudeRouteElsewhere]. A model
// argument that opts into the long window (`[1m]`) is held to it on every
// route.
func ClaudeCodeWindowOn(model string, route ClaudeRoute) ExecutorWindow {
	if !route.Valid() {
		route = ClaudeRouteElsewhere
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.HasSuffix(m, "[1m]") {
		return ExecutorWindow{Tokens: claudeLongWindow, Standard: claudeLongWindow, Known: true}
	}
	if m == "" {
		m = "default"
	}
	if parts := claudeModelDecorations.FindStringSubmatch(m); parts != nil {
		m = parts[1]
	}
	tokens, ok := claudeWindows[m]
	if !ok {
		return ExecutorWindow{Tokens: FallbackExecutorWindow, Standard: FallbackExecutorWindow}
	}
	standard := tokens
	if routes, held := claudeStandardWindows[m]; held {
		standard = routes[route]
	}
	return ExecutorWindow{Tokens: tokens, Standard: standard, Known: true}
}

// Carries reports whether one request under w can carry a continued session whose
// last request carried context tokens, with a brief of about brief tokens: a
// request the harness can send, which leaves the budget the session whole. NOT
// that the harness sends it without compacting the session first: its
// compaction point is lower ([ExecutorWindow.SendsWhole]), and a continuation
// past it is told as one that may have been cut off, never refused for it.
func (w ExecutorWindow) Carries(context int64, brief int) bool {
	return context+int64(brief) <= int64(w.Tokens-claudeReplyReserve)
}

// SendsWhole reports whether Claude Code, holding a run to w, sends a resumed
// session's first request without compacting the session first: its count of
// the conversation it resumes, at most count's bound, with the brief and what
// the resume's own turn attaches ([claudeTurnAttachments]), below its
// threshold: the window it holds the run to less its reply reserve and its
// compaction buffer (Claude Code compacts where its count reaches that point,
// 2.1.310 and 2.1.322 alike). That window is w's standard one, or the window
// the run that left the copy reported where that is less ([ResumedCount.Window]).
// False where the count has no bound ([ResumedCount.Bound]) and under a window
// this build does not know: one of an unknown model, and a known one whose
// standard window is not stated (zero).
//
// STRICTLY BELOW: the harness compacts where its count is at least the
// threshold, and the bound is at least its count, so only a bound below the
// point proves it sends the session whole. Monotone in the brief: a longer
// brief never sends whole where a shorter one does not.
func (w ExecutorWindow) SendsWhole(count *ResumedCount, brief string) bool {
	bound, ok := count.Bound()
	if !ok || !w.Known || w.Standard <= 0 {
		return false
	}
	window := int64(w.Standard)
	if count.Window > 0 && count.Window < window {
		window = count.Window
	}
	return bound+ClaudeCharge(len(brief))+claudeTurnAttachments < CompactionPoint(window)
}

// CompactionPoint is the count at which Claude Code compacts a conversation
// under a window of window tokens: the window less its reply reserve and its
// compaction buffer ([ExecutorWindow.SendsWhole]).
func CompactionPoint(window int64) int64 {
	return window - claudeReplyReserve - claudeCompactBuffer
}
