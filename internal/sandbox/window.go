package sandbox

import (
	"regexp"
	"strings"
)

// ClaudeCodeHarness is the Claude Code runner's name, the one harness whose
// compaction window the engine pins ([ClaudeCodeWindow]).
const ClaudeCodeHarness = "claude-code"

// ExecutorWindow is the context window a Claude Code executor run is pinned to
// (CLAUDE_CODE_AUTO_COMPACT_WINDOW): the window of the model the run works under.
//
// The harness clamps the pin to the model's own window and keeps its own headroom
// below it (its reply reserve, then a compaction buffer), so the engine adds none.
type ExecutorWindow struct {
	// Tokens is the pinned window.
	Tokens int
	// Known is false where this build does not know the model, and Tokens is
	// [FallbackExecutorWindow].
	Known bool
}

// FallbackExecutorWindow is the window of a model this build does not know: the
// standard window of every Claude model, so the harness's clamp never lowers it on
// a current model, and room for a run's first request several times over.
const FallbackExecutorWindow = 200_000

// claudeLongWindow is the window of every current Claude model and of a model
// argument that opts into the long window (`[1m]`).
const claudeLongWindow = 1_000_000

// claudeReplyReserve is the most of a window Claude Code keeps back for a reply
// (min(maxOutputTokens, 20000) in 2.1.319): a request past the rest is one the
// harness cannot send, nor compact first.
const claudeReplyReserve = 20_000

// claudeWindows is each model's own window, by the canonical id or the alias a
// providers.llm entry hands Claude Code. The aliases name current models on
// Claude Code 2.1.319, all of them long-window ones; on a platform whose alias
// names an older model, the harness clamps the pin to that model's window.
var claudeWindows = map[string]int{
	"claude-fable-5-1":  claudeLongWindow,
	"claude-fable-5":    claudeLongWindow,
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
	"claude-opus-4-5":   200_000,
	"claude-sonnet-4-5": 200_000,
	"claude-haiku-4-5":  200_000,
	"default":           claudeLongWindow,
	"best":              claudeLongWindow,
	"opus":              claudeLongWindow,
	"opusplan":          claudeLongWindow,
	"sonnet":            claudeLongWindow,
	"haiku":             claudeLongWindow,
	"fable":             claudeLongWindow,
}

// claudeModelDecorations are what a platform adds around a model's canonical id:
// a Bedrock region and vendor prefix, a Bedrock version suffix, and a dated
// snapshot written with `-` (Claude API, Bedrock) or `@` (Vertex AI).
var claudeModelDecorations = regexp.MustCompile(
	`^(?:(?:[a-z]{2,6}\.)?anthropic\.)?(.+?)(?:[-@]\d{8})?(?:-v\d+(?::\d+)?)?$`)

// ClaudeCodeWindow is the window a Claude Code run on model is pinned to: the
// model's own, or [FallbackExecutorWindow] where this build does not know it.
// An empty model is the harness's default model.
func ClaudeCodeWindow(model string) ExecutorWindow {
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.HasSuffix(m, "[1m]") {
		return ExecutorWindow{Tokens: claudeLongWindow, Known: true}
	}
	if m == "" {
		m = "default"
	}
	if parts := claudeModelDecorations.FindStringSubmatch(m); parts != nil {
		m = parts[1]
	}
	if tokens, ok := claudeWindows[m]; ok {
		return ExecutorWindow{Tokens: tokens, Known: true}
	}
	return ExecutorWindow{Tokens: FallbackExecutorWindow}
}

// Carries reports whether one request under w can carry a continued session whose
// last request carried context tokens, with a brief of about brief tokens.
func (w ExecutorWindow) Carries(context int64, brief int) bool {
	return context+int64(brief) <= int64(w.Tokens-claudeReplyReserve)
}
