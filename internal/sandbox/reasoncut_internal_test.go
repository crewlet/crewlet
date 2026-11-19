package sandbox

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A REASON IS REDACTED WHOLE, THEN CUT: every reason and diagnostic a run's
// record carries answers exactly what cutting the whole text's redaction
// answers, with a credential straddling the cut, and past the length up to
// which a redaction reads its text whole (32 MiB), where it reads it as a
// stream.
//
// Mutation: cut a reason or a diagnostic before redacting it, and the
// credential's remnant is carried where the whole's marker stands.
func TestAReasonIsRedactedWholeThenCut(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("c", 36)
	reason := func(bound int) func(string) string {
		return func(redacted string) string { return textcut.Within(redacted, bound) }
	}
	sites := []struct {
		name  string
		bound int
		got   func(string) string
		want  func(redacted string) string
	}{
		{"BoundFellBack", MaxFellBackBytes, BoundFellBack, reason(MaxFellBackBytes)},
		{"stampReason", executorkeep.UnkeptReasonMaxBytes, stampReason, reason(executorkeep.UnkeptReasonMaxBytes)},
		{"PushedUnread", events.MaxDiagnosticBytes, func(s string) string {
			return (&Coordinator{}).fitText(t.Context(), PendingRun{}, Result{PushedUnread: s}).PushedUnread
		}, events.ClipDiagnostic},
		{"Unopened", events.MaxDiagnosticBytes, func(s string) string {
			return fitOpened([]OpenedChange{{Unopened: s}})[0].Unopened
		}, events.ClipDiagnostic},
	}
	// words is n bytes of a sentence, ending on a space.
	words := func(n int) string { return strings.Repeat("the run broke ", n/14+1)[:n-1] + " " }
	for _, site := range sites {
		for _, at := range []int{site.bound - len(secret), site.bound - len(secret)/2, site.bound - 1, site.bound,
			site.bound + 1} {
			text := words(at) + secret + " and then " + words(3*site.bound)
			if got, want := site.got(text), site.want(redact.Secrets(text)); got != want {
				t.Errorf("%s, a credential at byte %d: got %q, want %q", site.name, at,
					got[max(0, len(got)-120):], want[max(0, len(want)-120):])
			}
		}
	}
	// PAST 32 MiB, a credential at each bound's cut and the rest after it,
	// through the two rules every site above is one of (a reason's cut and a
	// diagnostic's clip); each read of the text costs seconds under -race.
	head := words(MaxFellBackBytes-len(secret)/2) + secret + " "
	head += words(events.MaxDiagnosticBytes-len(secret)/2-len(head)) + secret + " "
	text := head + words(33<<20)
	redacted := redact.Secrets(text)
	for _, site := range []int{0, 3} {
		site := sites[site]
		if got, want := site.got(text), site.want(redacted); got != want {
			t.Errorf("%s, a %d-byte text: got %q, want %q", site.name, len(text), got[max(0, len(got)-120):],
				want[max(0, len(want)-120):])
		}
	}
}

// A REWRITE IS MEASURED WHOLE: a model's rewrite is taken only where its whole
// redaction fits the budget, and then as that redaction, every byte of it; one
// whose redaction passes the budget by a byte, or grows past it, is refused.
//
// Mutation: measure only an opening budget bytes long, and a rewrite past the
// budget is taken cut.
func TestARewriteIsMeasuredWhole(t *testing.T) {
	t.Parallel()
	const budget = 4 << 10
	line := "the run did its work\n"
	fits := strings.Repeat(line, budget/len(line)+1)[:budget]
	piece := "pwd=" + strings.Repeat(`x\n`, 20) + "\n"
	grows := strings.Repeat(piece, budget/len(piece))
	if len(grows) > budget || len(redact.Secrets(grows)) <= budget {
		t.Fatalf("the growing rewrite is %d bytes and redacts to %d: it must fit %d as written and not redacted",
			len(grows), len(redact.Secrets(grows)), budget)
	}
	for name, tc := range map[string]struct {
		rewrite string
		taken   bool
	}{
		"exactly the budget":             {fits, true},
		"a byte past the budget":         {fits + "x", false},
		"within it until it is redacted": {grows, false},
	} {
		c := &Coordinator{condense: &fakeCondenser{answer: func(RunPart, string, int) (string, error) {
			return tc.rewrite, nil
		}}}
		var spent AuxTokens
		got, ok := c.condensed(t.Context(), PendingRun{}, PartReport, "a long report", budget, &spent)
		if ok != tc.taken {
			t.Errorf("%s: taken %v, want %v", name, ok, tc.taken)
		}
		if want := redact.Secrets(tc.rewrite); ok && got != want {
			t.Errorf("%s: took %d bytes, not the whole %d-byte redaction", name, len(got), len(want))
		}
	}
}
