package executor

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY HARNESS THAT CONTINUES A SESSION STATES HOW ITS FIRST REQUEST IS
// JUDGED: a resumer that states none reads as FirstRequestNone, held whatever
// its brief, which a harness that compresses at its first request turns into
// calls never told again (Gemini CLI and Qwen Code did, on main).
//
// Mutation: delete Gemini CLI's sessionTraits, and its row goes red.
func TestEveryHarnessThatContinuesASessionStatesHowItsFirstRequestIsJudged(t *testing.T) {
	t.Parallel()
	for _, h := range Registered() {
		c, ok := h.(resumer)
		if !ok || !c.Resumes() {
			continue
		}
		if got := SessionTraits(h.Name()).FirstRequest; got == sandbox.FirstRequestNone || !got.Valid() {
			t.Errorf("%s continues a session and states its first request as %q: it is held whatever its "+
				"brief", h.Name(), got)
		}
	}
}

// A GEMINI CLI OR QWEN CODE SESSION IS JUDGED IN ITS BOX: a box's settings can
// move either's point, so only the launch can judge it; no reading keys its
// calls yet; a brief is charged a token a byte, at or above either's estimate.
//
// Mutation: state FirstRequestWindow, or a charge of 4, and a row goes red.
func TestAGeminiOrQwenSessionIsJudgedInItsBox(t *testing.T) {
	t.Parallel()
	want := sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingNone,
		Charge: 1}
	for _, name := range []string{GeminiCLIName, QwenCodeName} {
		if got := SessionTraits(name); got != want {
			t.Errorf("%s is judged as %+v, want %+v", name, got, want)
		}
	}
}

// A GEMINI CLI SESSION WHOSE TOOL OUTPUT ITS FIRST REQUEST WOULD MASK IS
// MARKED, whatever its size: 50,000 estimated tokens of answers (the least
// that masks is 50,001) is masked, and one fewer is a bound, in a history far
// below the size it collapses at.
//
// Mutation: raise geminiMaskProtect, or compare strictly, and the first row
// goes red; drop the synthetic answers from the sum, and the second does.
func TestAGeminiCountItsAnswersWouldMaskIsMarked(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		answers int64
		calls   map[string]int
		masked  bool
	}{
		"answers at the line":            {answers: 50_000, masked: true},
		"a synthetic answer reaching it": {answers: 49_999, calls: map[string]int{"c": 4}, masked: true},
		"answers just under it":          {answers: 49_999},
	} {
		c := newGeminiCounting()
		c.order, c.quarters, c.bytes = []string{"r"}, map[string]int64{"r": 4}, map[string]int64{"r": 100}
		c.answers["a"] = &geminiAnswer{tokens: tc.answers, carriers: []string{"r"}}
		for id, n := range tc.calls {
			c.calls[id] = n
		}
		count := c.count(0)
		if masked := count.Mark == sandbox.CountMasked; masked != tc.masked {
			t.Errorf("%s: answers of %d tokens count as %+v, want masked %v", name, tc.answers, count, tc.masked)
		}
	}
}
