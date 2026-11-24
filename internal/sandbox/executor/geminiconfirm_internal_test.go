package executor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A GEMINI TAKE IS CONFIRMED BY ITS TEXT AS A USER MESSAGE, ONCE PER DELIVERY,
// IN ORDER. On 0.70.0's own sessions: an AfterAgent take's text is a user
// message; two takes of one note, each delivered at a finish, confirm as two,
// a third as nothing; a take the session lacks leaves the next to confirm.
// Through compressions (r6): a note sent plain, later folded and dropped by a
// `$patch`, confirms once; the summary's text confirms nothing. A delivery
// before the take's run started (an earlier job's) confirms nothing, one on
// its first line does: the twin's deliveries are its 0-based lines 19 and 35.
//
// Mutations, each red: match from the first take, not past the last confirmed;
// let one delivery confirm every take of its text; read a compression's copy;
// confirm a take by a line before its run's start, or not on it (`from < at`).
func TestAGeminiTakeIsConfirmedByItsTextAsAUserMessage(t *testing.T) {
	t.Parallel()
	const (
		aaNote  = "NOTE-END ## A note from Lead, sent while this turn was running: CONF-AA-70-nokill"
		twinEnd = "## A note from Lead, sent while this turn was running: CAP-TWIN-END"
		folded  = "NOTE-END ## A note from Lead, sent while this turn was running: R6-NOTE-second"
	)
	for name, tc := range map[string]struct {
		session string
		takes   []string
		want    int
		// froms are the takes' runs' starts, a line each; none is every
		// take's run starting with its session.
		froms []int
	}{
		"a take at the finish": {"session-afteragent-note.jsonl", []string{aaNote}, 1, nil},
		"two identical takes, each delivered": {"session-twin-end.jsonl",
			[]string{twinEnd, twinEnd}, 2, nil},
		"three identical takes, two delivered": {"session-twin-end.jsonl",
			[]string{twinEnd, twinEnd, twinEnd}, 2, nil},
		"a take the session lacks first":       {"session-afteragent-note.jsonl", []string{"another note", aaNote}, 1, nil},
		"a prefix of the delivered text":       {"session-afteragent-note.jsonl", []string{"NOTE-END"}, 0, nil},
		"no take":                              {"session-afteragent-note.jsonl", nil, 0, nil},
		"a note folded by a later compression": {"session-compressed.jsonl", []string{folded}, 1, nil},
		"a folded note and its twin, delivered once": {"session-compressed.jsonl",
			[]string{folded, folded}, 1, nil},
		"a compression's summary": {"session-compressed.jsonl",
			[]string{"<state_snapshot>SUMMARY-MARK sawNote=false</state_snapshot>"}, 0, nil},
		"a take whose run started on the second delivery's line": {"session-twin-end.jsonl",
			[]string{twinEnd}, 1, []int{35}},
		"a take whose run started past both deliveries": {"session-twin-end.jsonl",
			[]string{twinEnd}, 0, []int{36}},
		"a second take whose run started past both deliveries": {"session-twin-end.jsonl",
			[]string{twinEnd, twinEnd}, 1, []int{0, 36}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(filepath.Join("testdata", "gemini", "0.70.0", tc.session))
			if err != nil {
				t.Fatal(err)
			}
			asked := make([]geminiAskedTake, len(tc.takes))
			for i, take := range tc.takes {
				asked[i] = geminiAskedTake{digest: ItemDigest(take), until: -1}
				if tc.froms != nil {
					asked[i].from = tc.froms[i]
				}
			}
			confirmed, err := geminiConfirmed(t.Context(), bytes.NewReader(raw), asked, maxLineBytes)
			got := 0
			for _, ok := range confirmed {
				if ok {
					got++
				}
			}
			if err != nil || got != tc.want {
				t.Fatalf("geminiConfirmed = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

// A READ GEMINI NOTE KEEPS ITS PLACE: the take script records every take,
// read ones included, and that record, not the ask's served order, is what
// the session is matched against. So a read note's entry is spent on it and
// settles no later take of the same words whose entry the session never got
// (the run killed before Gemini CLI wrote it); B's own entry holds B, no reply
// needed, since Gemini CLI's resume hands back a trailing entry (ADR-0177 §3).
//
// Mutation: match the session against the asked takes alone, and B reads held
// off A's entry.
func TestAReadGeminiNoteKeepsItsPlace(t *testing.T) {
	t.Parallel()
	const session = "11111111-2222-4333-8444-555555555555"
	const note = "## A note from Lead, sent while this turn was running: use staging"
	const name = "session-2026-11-23T09-00-11111111.jsonl"
	entry := `{"type":"user","content":[{"text":"` + note + `"}]}` + "\n"
	b := sandbox.AskedTake{Take: "take-b", Digest: ItemDigest(note)}
	for caseName, tc := range map[string]struct {
		session string
		held    bool
	}{
		"B never written": {entry + `{"type":"gemini","content":"On it."}` + "\n", false},
		"B written":       {entry + `{"type":"gemini","content":"On it."}` + "\n" + entry, true},
	} {
		box := sandbox.NewFakeSandbox("gemini-read")
		p := PathsFor(box)
		box.Put(p.GeminiChats()+"/"+name, `{"sessionId":"`+session+`"}`+"\n"+
			`{"type":"user","content":[{"text":"Fix the flake."}]}`+"\n"+tc.session)
		box.Put(p.GeminiTakes(session), `{"from":0}`+"\n"+`{"take":"take-a","digest":"`+ItemDigest(note)+`"}`+"\n"+
			`{"take":"take-b","digest":"`+ItemDigest(note)+`"}`+"\n")
		box.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{Stdout: name + "\n"}, nil
		}
		got := GeminiCLI{}.readMainThread(t.Context(), box, p, sandbox.MainThreadAsk{Session: session,
			Takes: []sandbox.AskedTake{b}}).Receipts
		if held := slices.Equal(got.Held, []string{"take-b"}); held != tc.held {
			t.Errorf("%s: B reads %+v, want held %v", caseName, got, tc.held)
		}
	}
}
