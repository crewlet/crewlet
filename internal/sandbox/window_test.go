package sandbox

import "testing"

// THE WINDOW IS THE MODEL'S OWN, read through the decorations a platform writes
// around its id, and the fallback for a model this build does not know. Never a
// pin below a current model's window: the harness clamps a pin to the model's
// own window, so overstating costs nothing and understating starves the run.
// Beside the pin, the least window Claude Code holds a run of the model to on
// Anthropic's own API ([ExecutorWindow.Standard]): no model, the account's
// default, may be one it holds to 200,000.
//
// Mutation: return a fixed window, drop a decoration, mark a fallback known, or
// take the pin for the standard window, and a row goes red.
func TestTheWindowIsTheModelsOwn(t *testing.T) {
	t.Parallel()
	for model, want := range map[string]ExecutorWindow{
		"claude-opus-5-5":           {Tokens: 1_000_000, Standard: 1_000_000, Known: true},
		"Claude-Sonnet-5-5":         {Tokens: 1_000_000, Standard: 1_000_000, Known: true},
		"opus":                      {Tokens: 1_000_000, Standard: 1_000_000, Known: true},
		"":                          {Tokens: 1_000_000, Standard: 200_000, Known: true},
		"claude-haiku-4-5-20251001": {Tokens: 200_000, Standard: 200_000, Known: true},
		"claude-opus-4-5@20251101":  {Tokens: 200_000, Standard: 200_000, Known: true},
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0": {Tokens: 200_000, Standard: 200_000, Known: true},
		"anthropic.claude-opus-5-5":                    {Tokens: 1_000_000, Standard: 1_000_000, Known: true},
		"claude-sonnet-4-5[1m]":                        {Tokens: 1_000_000, Standard: 1_000_000, Known: true},
		"opus[1M]":                                     {Tokens: 1_000_000, Standard: 1_000_000, Known: true},
		"claude-next-preview":                          {Tokens: FallbackExecutorWindow, Standard: FallbackExecutorWindow},
		"arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/abc": {
			Tokens: FallbackExecutorWindow, Standard: FallbackExecutorWindow},
	} {
		if got := ClaudeCodeWindow(model); got != want {
			t.Errorf("ClaudeCodeWindow(%q) = %+v, want %+v", model, got, want)
		}
	}
}

// A WINDOW CARRIES A SESSION WHOSE FIRST REQUEST FITS BENEATH ITS REPLY RESERVE:
// the session's context and the brief that continues it, together.
//
// Mutation: compare the context alone, or against the whole window, and a row
// goes red.
func TestAWindowCarriesWhatFitsBeneathItsReplyReserve(t *testing.T) {
	t.Parallel()
	w := ExecutorWindow{Tokens: 200_000, Known: true}
	for name, c := range map[string]struct {
		context int64
		brief   int
		carries bool
	}{
		"well within":            {context: 150_000, brief: 5_000, carries: true},
		"at the reserve":         {context: 175_000, brief: 5_000, carries: true},
		"past it with the brief": {context: 178_000, brief: 5_000},
		"past the whole window":  {context: 210_000, brief: 1_000},
	} {
		if got := w.Carries(c.context, c.brief); got != c.carries {
			t.Errorf("%s: Carries(%d, %d) = %v, want %v", name, c.context, c.brief, got, c.carries)
		}
	}
}
