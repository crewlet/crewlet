package executor

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN'S REASON FOR STARTING ANEW IS ONE SENTENCE, HELD AS IT IS READ. The
// session record's `why` line quotes whatever OpenCode answered, and the
// record is a file the run's own agent can rewrite, so read whole the line was
// as long as the file, and the launch's record carried it past the queue's
// ceiling. A reason past [sandbox.MaxFellBackBytes] keeps its opening, cut on
// a character and marked inside the bound; a short one is kept whole; and the
// cut comes after the redaction, so a credential across it leaves no fragment
// of itself behind.
//
// Mutation: keep the line whole, cut it on a byte, leave the mark out or past
// the bound, or cut it before redacting it, and a case goes red.
func TestASessionRecordsReasonIsHeldToItsBound(t *testing.T) {
	t.Parallel()
	const id = "9a8b7c6d-0000-4000-8000-000000000002"
	record := func(why string) string {
		return "id " + id + "\ncontinued no\nwhy " + why + "\nrelease 2.1.310 (Claude Code)\n"
	}

	const short = "OpenCode did not import it: Error: the export's format is not one this release reads"
	if got := parseSessionRecord(record(short)).why; got != short {
		t.Errorf("a short reason reads %q, want it whole", got)
	}

	// THREE BYTES A CHARACTER, after an opening that puts the bound inside
	// one, so a cut made on a byte splits it.
	long := "OpenCode answered:" + strings.Repeat("ア", sandbox.MaxFellBackBytes)
	if utf8.RuneStart(long[sandbox.MaxFellBackBytes-len("…")]) {
		t.Fatal("the case's bound falls between two characters, so it cannot see a cut inside one")
	}
	rec := parseSessionRecord(record(long))
	switch got := rec.why; {
	case len(got) > sandbox.MaxFellBackBytes:
		t.Errorf("a reason of %d bytes reads %d, past the %d it is held to", len(long), len(got),
			sandbox.MaxFellBackBytes)
	case !utf8.ValidString(got):
		t.Errorf("the reason was cut inside a character: it ends %q", got[max(0, len(got)-8):])
	case !strings.HasPrefix(got, "OpenCode answered:アア") || !strings.HasSuffix(got, "…"):
		t.Errorf("the reason does not keep its opening, marked where it was cut: %.40q … %q", got,
			got[max(0, len(got)-8):])
	}
	if rec.id != id || rec.continued || releaseOf(rec.release) != "2.1.310" {
		t.Errorf("the record's other lines read id %q, continued %v, release %q", rec.id, rec.continued,
			rec.release)
	}

	// A CREDENTIAL ACROSS THE CUT, at every offset that puts any of it there:
	// no run of its body long enough to identify it may survive.
	const body = "Q7wErTy9UiOp3AsDfGh5JkLzXcVbNm1PqRsT"
	token := "ghp_" + body
	for pad := sandbox.MaxFellBackBytes - len(token) - len("…") - 8; pad <= sandbox.MaxFellBackBytes; pad++ {
		got := parseSessionRecord(record(strings.Repeat("x", pad) + token + " and what OpenCode said after it")).why
		for i := 0; i+8 <= len(body); i++ {
			if strings.Contains(got, body[i:i+8]) {
				t.Fatalf("%d bytes before it, a fragment of a credential survived the cut: …%q", pad,
					got[max(0, len(got)-48):])
			}
		}
	}
}

// A RELEASE IS REFUSED PAST WHAT THE EXECUTOR LOG RECORDS, never cut: its
// numbers are runs of digits and what it is read from was printed in the box,
// so a run of them as long as the line read as a release the launch's record
// carried whole. A release at the log's bound is still one; past it, and
// past it by digits at either end, it is none.
//
// Mutation: drop the bound, hold it with ">=", or cut the release to it, and a
// case goes red.
func TestAReleasePastTheLogsBoundIsNone(t *testing.T) {
	t.Parallel()
	at := strings.Repeat("1", executordomain.HarnessVersionMaxBytes-len(".0.0")) + ".0.0"
	line := strings.Repeat("2", 1<<20) + ".1.310 (Claude Code)"
	for printed, want := range map[string]string{
		"2.1.310 (Claude Code)": "2.1.310",
		"opencode v2.0.32":      "2.0.32",
		at + " (Claude Code)":   at,
		"9" + at:                "",
		at + "9 (Claude Code)":  "",
		line:                    "",
	} {
		if got := releaseOf(printed); got != want {
			t.Errorf("releaseOf(%.40q, %d bytes) = %.40q, want %q", printed, len(printed), got, want)
		}
	}
}
