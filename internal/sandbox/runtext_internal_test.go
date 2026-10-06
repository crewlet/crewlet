package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type condenseCall struct {
	// handle and turn are the run's: a rewrite is a model call the engine
	// charges to the seat and files under the turn the run belongs to.
	handle string
	turn   string
	part   RunPart
	bytes  int
	budget int
}

type fakeCondenser struct {
	calls  []condenseCall
	answer func(part RunPart, text string, budget int) (string, error)
	// cost is what every call reports it spent, an answer or not.
	cost AuxTokens
}

func (f *fakeCondenser) Condense(_ context.Context, run PendingRun, part RunPart, text string,
	budget int) (string, AuxTokens, error) {
	f.calls = append(f.calls, condenseCall{run.AgentHandle, run.TurnID, part, len(text), budget})
	answer, err := f.answer(part, text, budget)
	return answer, f.cost, err
}

func lines(prefix string, n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(prefix)
		b.WriteString(strings.Repeat("-", 40))
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString("\n")
	}
	return b.String()
}

// A report or failure within the record's bound is carried as itself, with no
// model asked; one past it is condensed by the seat's auxiliary model, and the
// rewrite is what every later reader carries.
func TestARunsAccountPastTheRecordIsCondensedNotCut(t *testing.T) {
	t.Parallel()
	condenser := &fakeCondenser{answer: func(part RunPart, _ string, _ int) (string, error) {
		return "(condensed) the " + string(part), nil
	}}
	c := &Coordinator{condense: condenser}
	run := PendingRun{AgentHandle: "dev", TurnID: "t1"}

	short := Result{Text: "Outcome: succeeded", Error: "a warning"}
	if got := c.fitResult(t.Context(), run, short); got.Text != short.Text || got.Error != short.Error {
		t.Errorf("an account within the bound changed: %+v", got)
	}
	if len(condenser.calls) != 0 {
		t.Fatalf("a model was asked about text that fits: %+v", condenser.calls)
	}

	long := Result{Text: lines("report", 8000), Error: lines("stderr", 8000)}
	got := c.fitResult(t.Context(), run, long)
	if got.Text != "(condensed) the report" || got.Error != "(condensed) the failure" {
		t.Errorf("the rewrites were not carried: %q / %q", got.Text, got.Error)
	}
	want := []condenseCall{
		{"dev", "t1", PartReport, len(long.Text), MaxRunTextBytes},
		{"dev", "t1", PartFailure, len(long.Error), MaxRunTextBytes},
	}
	if len(condenser.calls) != 2 || condenser.calls[0] != want[0] || condenser.calls[1] != want[1] {
		t.Errorf("condense calls = %+v, want %+v", condenser.calls, want)
	}
}

// NO MODEL, NO CUT. A piece no model could condense keeps WHOLE lines up to
// the bound and says how many it left out — the report its opening, where its
// summary is, and the failure BOTH ENDS: its end, where the line naming what
// broke is, and its start, where the engine states the exit status and the
// CLI's own error before the error stream. Kept from its end alone, a failure
// with a long error stream lost those. A rewrite that came back past the
// bound is no rewrite.
//
// Mutation: keep a failure's end alone, as it was, and the exit status is
// gone.
func TestAnUncondensedAccountDropsWholeLinesAndSaysSo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		condense  Condenser
		wantCalls bool
	}{
		{"no condenser", nil, false},
		{"the model failed", &fakeCondenser{answer: func(RunPart, string, int) (string, error) {
			return "", errors.New("aux unavailable")
		}}, true},
		{"the rewrite came back too long", &fakeCondenser{answer: func(_ RunPart, _ string, budget int) (string, error) {
			return strings.Repeat("z", budget+1), nil
		}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &Coordinator{condense: tc.condense}
			report := "Outcome: succeeded\n" + lines("report", 8000)
			const status = "the coding agent exited with status 1:\n"
			failure := status + lines("noise", 8000) + "FATAL: migrations/0007.sql is missing"
			got := c.fitResult(t.Context(), PendingRun{AgentHandle: "dev"}, Result{Text: report, Error: failure})

			head, note, ok := strings.Cut(got.Text, "\n(")
			if !ok || !strings.Contains(note, "later line(s)") {
				t.Fatalf("the report's left-out lines are not announced: …%q", got.Text[max(0, len(got.Text)-200):])
			}
			if !strings.HasSuffix(head, "\n") || !strings.HasPrefix(report, head) || !strings.HasPrefix(head, "Outcome: succeeded\n") {
				t.Error("the report did not keep whole lines from its opening")
			}
			if len(head) > MaxRunTextBytes {
				t.Errorf("the report kept %d bytes, past the bound", len(head))
			}

			start, rest, ok := strings.Cut(got.Error, "(")
			note, tail, ok2 := strings.Cut(rest, ")\n")
			if !ok || !ok2 || !strings.Contains(note, "intervening line(s)") {
				t.Fatalf("the failure's left-out lines are not announced: %.200q", got.Error)
			}
			if !strings.HasPrefix(start, status) || !strings.HasPrefix(failure, start) || !strings.HasSuffix(start, "\n") {
				t.Errorf("the failure did not keep whole lines from its start, the exit status first: %.120q", start)
			}
			if len(start) > MaxRunTextBytes/failureHeadShare {
				t.Errorf("the failure's start kept %d bytes, past its share", len(start))
			}
			if !strings.HasSuffix(failure, "\n"+tail) || !strings.HasSuffix(tail, "FATAL: migrations/0007.sql is missing") {
				t.Error("the failure did not keep whole lines from its end")
			}
			if len(start)+len(tail) > MaxRunTextBytes {
				t.Errorf("the failure kept %d bytes, past the bound", len(start)+len(tail))
			}
			if f, ok := tc.condense.(*fakeCondenser); ok && len(f.calls) != 2 {
				t.Errorf("condense asked %d times, want once per piece", len(f.calls))
			}
		})
	}
}

// A single line past the whole budget leaves nothing to keep, and the note
// says so rather than showing a fragment of it.
func TestALineLongerThanTheRecordIsLeftOutWhole(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("x", MaxRunTextBytes+1)
	got := wholeLines(PartReport, line, MaxRunTextBytes)
	if strings.Contains(got, "x") || !strings.Contains(got, "1 later line(s)") {
		t.Errorf("an over-long line was cut rather than left out: %.120q", got)
	}
}

// EVERY BACKEND REFUSES A FILE PAST THE CAP. A reader that stops at its cap
// reports a clean end of file, so a clipped report reads as a finished one;
// and the local backend read whatever the job wrote, whole, into the memory of
// the host it shares.
func TestAFilePastTheCapIsRefusedNotClipped(t *testing.T) {
	t.Parallel()
	exact, err := readCapped(io.LimitReader(zeros{}, MaxFileBytes), "findings")
	if err != nil || len(exact) != MaxFileBytes {
		t.Fatalf("a file of exactly the cap: %d bytes, %v", len(exact), err)
	}
	if _, err := readCapped(io.LimitReader(zeros{}, MaxFileBytes+1), "findings"); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("a file past the cap: %v, want ErrFileTooLarge", err)
	}

	dir := t.TempDir()
	if got, err := readHostFile(filepath.Join(dir, "absent"), "absent"); got != nil || err != nil {
		t.Errorf("a missing file = %q, %v; want empty, the poll's contract", got, err)
	}
	big := filepath.Join(dir, "stderr")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readHostFile(big, "stderr"); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("a local file past the cap: %v, want ErrFileTooLarge", err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
