package executor

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN'S REASON FOR STARTING ANEW IS ONE SENTENCE, HELD AS ITS RESULT IS
// BUILT. The record's `why` line quotes whatever OpenCode answered, in a file
// the run's own agent can rewrite. A reason past [sandbox.MaxFellBackBytes]
// keeps its opening, cut on a character and marked inside the bound; a short
// one is kept whole; and the cut comes after the redaction, so a credential
// across it leaves no fragment behind.
//
// Mutation: keep the line whole, cut it on a byte, leave the mark out or past
// the bound, or cut it before redacting it, and a case goes red.
func TestARunsReasonForStartingAnewIsHeldToItsBound(t *testing.T) {
	t.Parallel()
	const id = "9a8b7c6d-0000-4000-8000-000000000002"
	runner := NewClaudeCode()
	b := sandbox.NewFakeSandbox("box-1")
	if err := runner.Install(t.Context(), b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	p := PathsFor(b)
	b.Put(p.Stream(), `{"type":"result","subtype":"success","is_error":false,"result":"done"}`+"\n")
	b.Put(p.Result(), `{"type":"result","subtype":"success","is_error":false,"result":"done"}`)
	b.Put(p.ExitCode(), "0")
	collected := func(why string) sandbox.Result {
		t.Helper()
		b.Put(p.SessionRecord(), "id "+id+"\ncontinued no\nwhy "+why+"\nrelease 2.1.310 (Claude Code)\n")
		res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		return res
	}

	const short = "OpenCode did not import it: Error: the export's format is not one this release reads"
	if got := collected(short).FellBack; got != short {
		t.Errorf("a short reason reads %q, want it whole", got)
	}

	// THREE BYTES A CHARACTER, after an opening that puts the bound inside
	// one, so a cut made on a byte splits it.
	long := "OpenCode answered:" + strings.Repeat("ア", sandbox.MaxFellBackBytes)
	if utf8.RuneStart(long[sandbox.MaxFellBackBytes-len("…")]) {
		t.Fatal("the case's bound falls between two characters, so it cannot see a cut inside one")
	}
	res := collected(long)
	switch got := res.FellBack; {
	case len(got) > sandbox.MaxFellBackBytes:
		t.Errorf("a reason of %d bytes reads %d, past the %d it is held to", len(long), len(got),
			sandbox.MaxFellBackBytes)
	case !utf8.ValidString(got):
		t.Errorf("the reason was cut inside a character: it ends %q", got[max(0, len(got)-8):])
	case !strings.HasPrefix(got, "OpenCode answered:アア") || !strings.HasSuffix(got, "…"):
		t.Errorf("the reason does not keep its opening, marked where it was cut: %.40q … %q", got,
			got[max(0, len(got)-8):])
	}
	if res.SessionID != id || res.Continued || res.HarnessVersion != "2.1.310" {
		t.Errorf("the record's other lines read session %q, continued %v, release %q", res.SessionID,
			res.Continued, res.HarnessVersion)
	}

	// A CREDENTIAL ACROSS THE CUT, at every offset that puts any of it there:
	// no run of its body long enough to identify it may survive.
	const body = "Q7wErTy9UiOp3AsDfGh5JkLzXcVbNm1PqRsT"
	token := "ghp_" + body
	for pad := sandbox.MaxFellBackBytes - len(token) - len("…") - 8; pad <= sandbox.MaxFellBackBytes; pad++ {
		got := collected(strings.Repeat("x", pad) + token + " and what OpenCode said after it").FellBack
		for i := 0; i+8 <= len(body); i++ {
			if strings.Contains(got, body[i:i+8]) {
				t.Fatalf("%d bytes before it, a fragment of a credential survived the cut: …%q", pad,
					got[max(0, len(got)-48):])
			}
		}
	}
}

// A SESSION RECORD IS READ WHERE IT LIES, AND ALLOCATES NOTHING: the record is
// agent-writable up to the 32 MiB the engine reads whole, and read on every
// collection and take. Its reason is handed on as the box wrote it, bounded
// only where the result is built ([TestARunsReasonForStartingAnewIsHeldToItsBound]).
// Not parallel, since an allocation count is the whole process's: over a
// hundred reads a stray allocation elsewhere divides to nothing, while a parse
// that allocates divides to one or more.
//
// Mutation: split the record into its lines, keep a set of the keys it named,
// or hold the reason to its bound as it is read, and the case goes red.
func TestASessionRecordIsReadWhereItLies(t *testing.T) {
	const id = "9a8b7c6d-0000-4000-8000-000000000003"
	// Past the reason's bound, so holding it as it is read cuts it.
	why := "OpenCode did not import it:" + strings.Repeat(" Error: the export's format is not one "+
		"this release reads.", 2*sandbox.MaxFellBackBytes/64)
	var b strings.Builder
	b.WriteString(strings.Repeat("\n", 64<<10))
	for i := range 4096 {
		fmt.Fprintf(&b, "key%d a line the record's agent wrote\n", i)
	}
	b.WriteString("id " + id + "\ncontinued no\nwhy " + why + "\nrelease 2.1.310 (Claude Code)\n")
	record := b.String()

	rec := parseSessionRecord(record)
	if rec.id != id || rec.continued || rec.why != why || rec.release != "2.1.310 (Claude Code)" {
		t.Fatalf("the record reads id %q, continued %v, a reason of %d bytes and release %q; want its "+
			"four lines, the reason as the box wrote it (%d bytes)", rec.id, rec.continued, len(rec.why),
			rec.release, len(why))
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = parseSessionRecord(record) }); allocs != 0 {
		t.Errorf("reading a record of %d bytes allocated %.0f times, where it allocates nothing: it is "+
			"split, its keys are kept, or its reason is held as it is read", len(record), allocs)
	}
}

// A RELEASE IS REFUSED PAST WHAT THE EXECUTOR LOG RECORDS, never cut: it is
// printed in the box, so a line-long run of digits must not read as one. A
// release at the log's bound is still one; past it, by digits at either end,
// it is none.
//
// Mutation: drop the bound, hold it with ">=", or cut the release to it, and a
// case goes red.
func TestAReleasePastTheLogsBoundIsNone(t *testing.T) {
	t.Parallel()
	at := strings.Repeat("1", executorkeep.HarnessVersionMaxBytes-len(".0.0")) + ".0.0"
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
