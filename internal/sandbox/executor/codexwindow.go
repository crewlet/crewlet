package executor

import (
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

var _ windowHarness = Codex{}

// codexWindows is the raw window (the units carry's count reads,
// [codexRawWindow]) of each public model in Codex 0.171.0's bundled catalogue,
// none of which sets its own compaction limit (CODEX-MEASURE §17.2). A
// ChatGPT-login run may fetch a catalogue that differs: the limit the runner
// pins governs the run, and carry's count reads the window the run's rollout
// states at collection ([sandbox.Result.ReportedWindow], the least window).
var codexWindows = map[string]int{
	"gpt-6.2-sol": 500_000, "gpt-6.1-sol": 500_000, "gpt-6.1-terra": 500_000,
	"gpt-6.2-astra": 272_000, "gpt-6-sol": 272_000, "gpt-6-astra": 272_000, "gpt-6-luna": 272_000,
	"gpt-5.6-sol": 272_000, "gpt-5.6-terra": 272_000, "gpt-5.6-luna": 272_000, "gpt-5.5": 272_000,
}

// codexFallbackWindow is the raw window of a model the table does not list,
// and of a run naming no model (Codex's default may move): 272,000, the window
// Codex holds a model with no catalogue entry to (measured, §16.4).
const codexFallbackWindow = 272_000

// codexWindow is the window a Codex run on model is pinned to and held to
// ([sandbox.ExecutorWindow]): its raw window less 5% Codex keeps back from a
// request (its model_context_window reads 95%) and 5% more to the compaction
// point, 90% of the window, where Codex compacts by default (§16.4). Known for
// a listed model alone. SendsWhole adds the brief's charge, where Codex's own
// check before a turn counts only the newest request's total: stricter, so it
// errs safe.
func codexWindow(model string) sandbox.ExecutorWindow {
	tokens, known := codexWindows[strings.ToLower(strings.TrimSpace(model))]
	if !known {
		tokens = codexFallbackWindow
	}
	return sandbox.ExecutorWindow{Tokens: tokens, Standard: tokens, Known: known,
		Reserve: tokens / 20, Buffer: tokens / 20, Charge: codexChargeBytes}
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
