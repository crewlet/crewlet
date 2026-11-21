package executor

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE STANDARD WINDOW IS THE HARNESS'S OWN, by model and route, as Claude Code
// 2.1.310 and 2.1.322 hold a run where nothing raises it: claude-opus-4-6 and
// claude-sonnet-4-6 at 200,000 on every route though pinned at 1,000,000; an
// alias at its model's window on the route (`opus` is claude-opus-4-6 on
// Foundry, `sonnet` and `haiku` older models off Anthropic's API); no model and
// `default` at 200,000 everywhere, the account's default possibly one of those;
// an unknown route as the least off Anthropic's. The pin never moves.
//
// Mutation: take the pin for the standard window, or read every route as
// Anthropic's, and a row goes red.
func TestTheStandardWindowIsTheHarnessesOwn(t *testing.T) {
	t.Parallel()
	type row struct {
		model    string
		route    claudeRoute
		tokens   int
		standard int
	}
	for _, r := range []row{
		{"claude-opus-4-6", claudeRouteAnthropic, 1_000_000, 200_000},
		{"claude-sonnet-4-6", claudeRouteBedrock, 1_000_000, 200_000},
		{"claude-opus-4-6[1m]", claudeRouteAnthropic, 1_000_000, 1_000_000},
		{"claude-sonnet-4-5", claudeRouteAnthropic, 200_000, 200_000},
		{"claude-fable-5-2", claudeRouteVertex, 1_000_000, 1_000_000},
		{"claude-mythos-5-2", claudeRouteAnthropic, 1_000_000, 1_000_000},
		{"claude-opus-5-5", claudeRouteFoundry, 1_000_000, 1_000_000},
		{"opus", claudeRouteAnthropic, 1_000_000, 1_000_000},
		{"opus", claudeRouteBedrock, 1_000_000, 1_000_000},
		{"opus", claudeRouteFoundry, 1_000_000, 200_000},
		{"opus", claudeRouteElsewhere, 1_000_000, 200_000},
		{"best", claudeRouteVertex, 1_000_000, 1_000_000},
		{"best", claudeRouteFoundry, 1_000_000, 200_000},
		{"sonnet", claudeRouteAnthropic, 1_000_000, 1_000_000},
		{"sonnet", claudeRouteBedrock, 1_000_000, 200_000},
		{"haiku", claudeRouteVertex, 1_000_000, 200_000},
		{"fable", claudeRouteElsewhere, 1_000_000, 1_000_000},
		{"opusplan", claudeRouteAnthropic, 1_000_000, 1_000_000},
		{"opusplan", claudeRouteBedrock, 1_000_000, 200_000},
		{"default", claudeRouteAnthropic, 1_000_000, 200_000},
		{"", claudeRouteBedrock, 1_000_000, 200_000},
		{"sonnet", claudeRoute("a-later-builds-route"), 1_000_000, 200_000},
	} {
		got := claudeCodeWindowOn(r.model, r.route)
		if got.Tokens != r.tokens || got.Standard != r.standard || !got.Known {
			t.Errorf("claudeCodeWindowOn(%q, %s) = %+v, want the pin %d and the standard window %d, known",
				r.model, r.route, got, r.tokens, r.standard)
		}
	}
}

// SENDS WHOLE IS THE HARNESS'S THRESHOLD: Claude Code compacts where its count of
// the conversation, the brief and the resume's attachments reaches its window
// less its reply reserve and compaction buffer, so a bound whose sum with the
// brief's charge and the attachments is below that point sends whole, and one
// at it does not. At a 200,000-token window the point is 167,000.
//
// Mutation: compare with <=, leave the brief's charge or the turn's attachments
// out, or judge under the pin, and a case goes red.
func TestSendsWholeIsTheHarnessesThreshold(t *testing.T) {
	t.Parallel()
	w := claudeCodeWindowOn("claude-opus-4-6", claudeRouteAnthropic)
	const point = 167_000
	if got := w.Point(200_000); got != point {
		t.Fatalf("the point at a 200,000-token window is %d, want %d", got, point)
	}
	brief := "0123456789" // ten bytes, five tokens at claudeChargeBytes
	below := &sandbox.ResumedCount{Tokens: point - claudeTurnAttachments - 5 - 1}
	at := &sandbox.ResumedCount{Tokens: point - claudeTurnAttachments - 5}
	if !w.SendsWhole(below, brief) {
		t.Errorf("a bound one token below the point with its brief compacts: %+v", below)
	}
	if w.SendsWhole(at, brief) {
		t.Errorf("a bound at the point with its brief sends whole: %+v", at)
	}
	if !w.SendsWhole(at, "") {
		t.Errorf("the same bound with no brief compacts: the brief's charge is not what tips it")
	}
}

// NO BOUND, OR A WINDOW THIS BUILD DOES NOT KNOW, MAY COMPACT: no count, a mark,
// a figure that is no count, an unknown model, and a known window with no
// standard one stated, whatever the brief.
//
// Mutation: read a mark as a bound of 0, or a standard window of 0 as one, and a
// case goes red.
func TestNoBoundOrAnUnknownWindowMayCompact(t *testing.T) {
	t.Parallel()
	known := claudeCodeWindow("claude-opus-5-5")
	for name, tc := range map[string]struct {
		w     sandbox.ExecutorWindow
		count *sandbox.ResumedCount
	}{
		"no count":               {w: known},
		"a mark":                 {w: known, count: &sandbox.ResumedCount{Mark: sandbox.CountMedia}},
		"a later build's mark":   {w: known, count: &sandbox.ResumedCount{Mark: "a-later-builds-mark"}},
		"no figure":              {w: known, count: &sandbox.ResumedCount{}},
		"an unknown model":       {w: claudeCodeWindow("claude-next-preview"), count: &sandbox.ResumedCount{Tokens: 10}},
		"no standard window":     {w: sandbox.ExecutorWindow{Tokens: 1_000_000, Known: true}, count: &sandbox.ResumedCount{Tokens: 10}},
		"a zero window, unknown": {count: &sandbox.ResumedCount{Tokens: 10}},
	} {
		if tc.w.SendsWhole(tc.count, "") {
			t.Errorf("%s: sends whole", name)
		}
	}
}

// A REPORTED WINDOW BELOW THE STANDARD DECIDES: a run that reported holding its
// model to 200,000 tokens (the account's default, an alias its box retargeted, a
// switch holding every model there) may compact at 150,000 under a standard
// window of 1,000,000.
//
// Mutation: ignore the reported window, and this goes red.
func TestAReportedWindowBelowTheStandardDecides(t *testing.T) {
	t.Parallel()
	w := claudeCodeWindow("claude-opus-5-5")
	if w.SendsWhole(&sandbox.ResumedCount{Tokens: 150_000, Window: 200_000}, "") {
		t.Errorf("a count of 150,000 its run reported a 200,000-token window for sends whole under %+v", w)
	}
}

// A REPORTED WINDOW NEVER RAISES THE STANDARD: a figure a box writes may make a
// continuation read as cut off, never as whole, so a run reporting 1,000,000 for
// a model the harness holds to 200,000 is judged at 200,000.
//
// Mutation: take the larger of the two, and this goes red.
func TestAReportedWindowNeverRaisesTheStandard(t *testing.T) {
	t.Parallel()
	w := claudeCodeWindow("claude-sonnet-4-5")
	if w.SendsWhole(&sandbox.ResumedCount{Tokens: 150_000, Window: 1_000_000}, "") {
		t.Errorf("a count of 150,000 reported under 1,000,000 sends whole under %+v", w)
	}
}

// THE WINDOW IS THE MODEL'S OWN, read through a platform's decorations on its
// id, with the fallback for a model this build does not know. Never a pin below
// a current model's window: the harness clamps a pin to the model's own, so
// overstating costs nothing and understating starves the run.
// [sandbox.ExecutorWindow.Standard] is the least window Claude Code holds the model to
// on Anthropic's API: no model (the account's default) may be held to 200,000.
//
// Mutation: return a fixed window, drop a decoration, mark a fallback known, or
// take the pin for the standard window, and a row goes red.
func TestTheWindowIsTheModelsOwn(t *testing.T) {
	t.Parallel()
	for model, want := range map[string]sandbox.ExecutorWindow{
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
		"claude-next-preview":                          {Tokens: claudeFallbackWindow, Standard: claudeFallbackWindow},
		"arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/abc": {
			Tokens: claudeFallbackWindow, Standard: claudeFallbackWindow},
	} {
		if got := claudeCodeWindow(model); got.Tokens != want.Tokens || got.Standard != want.Standard ||
			got.Known != want.Known {
			t.Errorf("claudeCodeWindow(%q) = %+v, want %+v", model, got, want)
		}
	}
}

// EVERY ROUTE THIS BUILD JUDGES A WINDOW ON IS VALID, and a word it does not
// know is not: it reads as the least window off Anthropic's API.
//
// Mutation: leave a route out of its list, and a case goes red.
func TestEveryClaudeRouteIsValid(t *testing.T) {
	t.Parallel()
	for _, r := range []claudeRoute{claudeRouteAnthropic, claudeRouteBedrock, claudeRouteVertex, claudeRouteFoundry,
		claudeRouteElsewhere} {
		if !r.Valid() {
			t.Errorf("%q is not valid", r)
		}
	}
	if claudeRoute("a-later-builds-route").Valid() {
		t.Error("a route this build does not know is valid")
	}
}

// A TELLING JUDGES AN ENTRY ON ITS COARSE ROUTE: Anthropic's own API for a
// login, a key or the host's environment, and the least window off it for an
// entry signed in by its own env or a mode this build does not know.
//
// Mutation: read an inherit-env entry as a route of its own, or an env entry
// as Anthropic's, and a case goes red.
func TestATellingJudgesAnEntryOnItsCoarseRoute(t *testing.T) {
	t.Parallel()
	for mode, want := range map[config.HarnessAuthMode]claudeRoute{
		config.AuthSubscription: claudeRouteAnthropic, config.AuthAPIKey: claudeRouteAnthropic,
		config.AuthInheritEnv: claudeRouteAnthropic, config.AuthEnv: claudeRouteElsewhere,
		config.HarnessAuthMode("a-later-builds-mode"): claudeRouteElsewhere,
	} {
		if got := claudeRouteOf(mode); got != want {
			t.Errorf("an entry signed in by %q is judged on the route %q, want %q", mode, got, want)
		}
	}
}
