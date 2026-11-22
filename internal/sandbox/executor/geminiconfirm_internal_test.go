package executor

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A GEMINI TAKE IS CONFIRMED BY ITS TEXT AS A USER MESSAGE, ONCE PER DELIVERY,
// IN ORDER. On 0.70.0's own sessions: an AfterAgent take's text is a user
// message; two takes of one note, each delivered at a finish, confirm as two,
// a third as nothing; a take the session lacks leaves the next to confirm.
// Through compressions (r6): a note
// sent plain, later folded and dropped by a `$patch`, confirms once, though
// the compression kept a copy of it; the summary's own text confirms nothing.
//
// Mutations, each red: match from the first take rather than after the last
// confirmed (the twin confirms as one); let one delivery confirm every take of
// its text; read a compression's restatement (its copy confirms a twin).
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
	}{
		"a take at the finish": {"session-afteragent-note.jsonl", []string{aaNote}, 1},
		"two identical takes, each delivered": {"session-twin-end.jsonl",
			[]string{twinEnd, twinEnd}, 2},
		"three identical takes, two delivered": {"session-twin-end.jsonl",
			[]string{twinEnd, twinEnd, twinEnd}, 2},
		"a take the session lacks first":       {"session-afteragent-note.jsonl", []string{"another note", aaNote}, 1},
		"a prefix of the delivered text":       {"session-afteragent-note.jsonl", []string{"NOTE-END"}, 0},
		"no take":                              {"session-afteragent-note.jsonl", nil, 0},
		"a note folded by a later compression": {"session-compressed.jsonl", []string{folded}, 1},
		"a folded note and its twin, delivered once": {"session-compressed.jsonl",
			[]string{folded, folded}, 1},
		"a compression's summary": {"session-compressed.jsonl",
			[]string{"<state_snapshot>SUMMARY-MARK sawNote=false</state_snapshot>"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(filepath.Join("testdata", "gemini", "0.70.0", tc.session))
			if err != nil {
				t.Fatal(err)
			}
			digests := make([]string, len(tc.takes))
			for i, take := range tc.takes {
				digests[i] = ItemDigest(take)
			}
			confirmed, err := geminiConfirmed(bytes.NewReader(raw), digests, maxLineBytes)
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
