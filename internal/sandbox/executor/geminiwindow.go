package executor

import "github.com/crewlet/crewlet/internal/sandbox"

var _ windowHarness = GeminiCLI{}

// Window is the window a Gemini CLI run of ask's model is held to: Gemini
// CLI's own table ([geminiWindow]), which it states for every model, so it is
// known whatever the model and the sign-in. No reserve, buffer or charge is
// stated: Gemini CLI's compression point was not measured, and the runner
// continues no session, so nothing judges a first request against them.
func (GeminiCLI) Window(ask WindowAsk) sandbox.ExecutorWindow {
	tokens := int(geminiWindow(ask.Model))
	return sandbox.ExecutorWindow{Tokens: tokens, Standard: tokens, Known: true}
}

// PinnedWindow is the window a Gemini CLI run on model is held to, as the
// collected record states it.
func (GeminiCLI) PinnedWindow(model string) int { return int(geminiWindow(model)) }

// geminiWindow is the context window Gemini CLI holds model to, as its own
// table states it: tokenLimit in 0.70.0's bundle, which `gemini models list
// -o json` prints as each model's contextLimits.inputTokens with no request
// and no sign-in. 256,000 for the two Gemma 4 models it names, and 1,048,576
// for every other, its listed Gemini models and its DEFAULT_TOKEN_LIMIT for a
// model it does not know alike. Gemini CLI's own assumption, not the API's:
// it is what the harness compresses its history against, and the stream
// reports no window of its own.
func geminiWindow(model string) int64 {
	switch model {
	case "gemma-4-31b-it", "gemma-4-26b-a4b-it":
		return geminiGemma4Window
	default:
		return geminiDefaultWindow
	}
}

// geminiGemma4Window and geminiDefaultWindow are Gemini CLI 0.70.0's
// GEMMA_4_TOKEN_LIMIT and DEFAULT_TOKEN_LIMIT ([geminiWindow]).
const (
	geminiGemma4Window  = 256_000
	geminiDefaultWindow = 1_048_576
)
