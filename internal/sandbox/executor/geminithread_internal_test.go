package executor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A GEMINI RUN'S RECEIPTS ARE READ OFF ITS RECORD OF TAKES AND ITS SESSION, Gemini
// CLI's case of the note contract's confirm path ([GeminiConfirmsATake]). In a
// box holding the runner's own capture of a run whose finish two takes of one
// note blocked (session-twin-end), beside another session sharing its id's
// eight characters, and the take script's record of three takes of that
// text: the first two are held, the third undecided while the job works and
// absent once it is over, as is a take asked that the script never recorded.
//
// Mutations, each red: answer Absent while the job works; hold a take the
// session shows no delivery for; read the session another session's first
// line names.
func geminiReceiptsAreReadOffItsTakesAndItsSession(t *testing.T) {
	t.Parallel()
	const session = "11111111-2222-4333-8444-555555555555"
	const text = "## A note from Lead, sent while this turn was running: CAP-TWIN-END"
	raw, err := os.ReadFile(filepath.Join("testdata", "gemini", "0.70.0", "session-twin-end.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	b := sandbox.NewFakeSandbox("box-1")
	p := PathsFor(b)
	files := map[string]string{
		p.GeminiChats() + "/session-2026-11-21T22-24-11111111.jsonl": string(raw),
		p.GeminiChats() + "/session-2026-11-21T22-20-11111111.jsonl": `{"sessionId":"11111111-0000-4000-8000-000000000000"}` +
			"\n" + `{"type":"user","content":[{"text":"` + text + `"}]}` + "\n" +
			`{"type":"user","content":[{"text":"` + text + `"}]}` + "\n" +
			`{"type":"user","content":[{"text":"` + text + `"}]}` + "\n",
		p.GeminiTakes(session): `{"take":"take-a","digest":"` + ItemDigest(text) + `"}` + "\n" +
			`{"take":"take-b","digest":"` + ItemDigest(text) + `"}` + "\n" +
			`{"take":"take-c","digest":"` + ItemDigest(text) + `"}` + "\n",
	}
	for path, content := range files {
		if err := b.WriteFile(t.Context(), path, []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	b.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if !strings.Contains(cmd, p.GeminiChats()) {
			return sandbox.ExecResult{ExitCode: 1}, nil
		}
		return sandbox.ExecResult{Stdout: "session-2026-11-21T22-20-11111111.jsonl\nsession-2026-11-21T22-24-11111111.jsonl\n"}, nil
	}
	asked := []sandbox.AskedTake{{Take: "take-a"}, {Take: "take-b"}, {Take: "take-c"}, {Take: "take-never"}}
	for _, ended := range []bool{false, true} {
		got := GeminiCLI{}.readMainThread(t.Context(), b, p, sandbox.MainThreadAsk{Session: session, Takes: asked,
			Ended: ended}).Receipts
		if !slices.Equal(got.Held, []string{"take-a", "take-b"}) {
			t.Errorf("ended %v: held %q, want take-a and take-b", ended, got.Held)
		}
		var absent []string
		if ended {
			absent = []string{"take-c", "take-never"}
		}
		if !slices.Equal(got.Absent, absent) {
			t.Errorf("ended %v: absent %q, want %q", ended, got.Absent, absent)
		}
	}
}

// AN EMPTY RECORD OF TAKES SETTLES, A MISSING ONE WAITS. The run's script leaves
// the record empty before the run, so a take the script never recorded (its
// hook killed between the bridge's answer and the record) reads absent once
// the job is over, and is served again; a box with no record at all lost it,
// and its takes stay undecided whether or not the job is over.
//
// Mutations, each red: read a missing record as empty; read an empty record
// as unreadable.
func TestAnEmptyRecordOfTakesSettlesAndAMissingOneWaits(t *testing.T) {
	t.Parallel()
	asked := []sandbox.AskedTake{{Take: "take-a"}}
	const session = "11111111-2222-4333-8444-555555555555"
	for name, tc := range map[string]struct {
		record       bool
		ended        bool
		absent       bool
		undecidedWhy bool
	}{
		"empty, the job working": {record: true, undecidedWhy: true},
		"empty, the job over":    {record: true, ended: true, absent: true},
		"missing, the job over":  {ended: true, undecidedWhy: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := sandbox.NewFakeSandbox("box-1")
			p := PathsFor(b)
			if tc.record {
				if err := b.WriteFile(t.Context(), p.GeminiTakes(session), nil); err != nil {
					t.Fatal(err)
				}
			}
			got := GeminiCLI{}.readMainThread(t.Context(), b, p, sandbox.MainThreadAsk{Session: session,
				Takes: asked, Ended: tc.ended}).Receipts
			if len(got.Held) != 0 || (len(got.Absent) == 1) != tc.absent || (got.Why != "") != tc.undecidedWhy {
				t.Errorf("held %q, absent %q, why %q; want absent %v, undecided %v",
					got.Held, got.Absent, got.Why, tc.absent, tc.undecidedWhy)
			}
		})
	}
}
