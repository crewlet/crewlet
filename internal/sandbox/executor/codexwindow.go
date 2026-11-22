package executor

import "github.com/crewlet/crewlet/internal/sandbox"

var _ windowHarness = Codex{}

// codexWindowTokens is the window a Codex run is pinned to, raw (the units
// carry's count reads, [codexRawWindow]): 272,000, the window Codex holds a model
// with no catalog entry to (measured on 0.171.0, CODEX-MEASURE §16.4). Every
// model is held to it, since the runner pins the compaction point below it
// ([codexCompactLimit]); a model whose own window is larger compacts earlier
// than it could, never later. That every model Codex drives has at least this
// window is inferred from its catalogue (gpt-6.2-sol reports 475,000 at 95%),
// not measured model by model.
const codexWindowTokens = 272_000

// codexReplyReserve is the share of the window Codex keeps back from a request:
// 5%, so the rollout's model_context_window reads 95% of the raw window
// (258,400 of 272,000; measured, §16.4).
const codexReplyReserve = codexWindowTokens / 20

// codexCompactBuffer is the room between Codex's compaction point and the
// window less [codexReplyReserve]: another 5%, so the point is 90% of the raw
// window, 244,800, where Codex compacts by default (measured, §16.4).
const codexCompactBuffer = codexWindowTokens / 20

// codexWindow is the window a Codex run on model is pinned to and held to
// ([sandbox.ExecutorWindow]): the same for every model ([codexWindowTokens]).
// SendsWhole adds the brief's charge, where Codex's own check before a turn
// counts only the newest request's total (§16.4): stricter, so it errs safe.
func codexWindow(string) sandbox.ExecutorWindow {
	return sandbox.ExecutorWindow{Tokens: codexWindowTokens, Standard: codexWindowTokens, Known: true,
		Reserve: codexReplyReserve, Buffer: codexCompactBuffer, Charge: codexChargeBytes}
}

// codexCompactLimit is the count a Codex run on model compacts at, pinned in
// its configuration as model_auto_compact_token_limit, which sets the limit
// directly (measured, §16.4): exactly the point the engine judges a
// continuation by ([sandbox.ExecutorWindow.Point]).
func codexCompactLimit(model string) int64 {
	w := codexWindow(model)
	return w.Point(int64(w.Tokens))
}

// Window is the window a Codex run of ask's member is pinned to and held to,
// whatever its sign-in: Codex holds every route to one window.
func (Codex) Window(ask WindowAsk) sandbox.ExecutorWindow { return codexWindow(ask.Model) }

// PinnedWindow is the window a Codex run on model is pinned to.
func (Codex) PinnedWindow(model string) int { return codexWindow(model).Tokens }
