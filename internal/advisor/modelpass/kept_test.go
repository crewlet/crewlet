package modelpass

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisordomain"
)

// A RUN'S RECORD KEEPS WHAT FITS, WHATEVER THE RUN DID: an investigation of a
// 23-item agenda with long verdicts, hundreds of long progress lines, a long
// reason and many unread sources keeps a summary within the log's bound, with
// its numbers, its last lines and its agenda as a tally. Mutation: keep the
// run whole, and its record is refused, so it never concludes.
func TestARunsRecordKeepsWhatFitsWhateverItDid(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 300)
	run := DeepRun{Pass: advisor.Pass{Status: advisor.PassDone, Model: strings.Repeat("m", 200), Ceiling: 2_000_000,
		InputTokens: 1_900_000, OutputTokens: 40_000, Listed: 10, Reason: strings.Repeat("why ", 2000),
		Dropped:  map[advisor.Drop]int{advisor.DropWords: 3, advisor.DropTokensUnsplit: 1, "a drop of its own": 9},
		Findings: make([]advisor.Finding, 10)}, StoppedBy: StoppedDone, Minutes: 19.5}
	run.Bounds.Tokens, run.Bounds.Minutes, run.Bounds.Rounds = 2_000_000, 20, 80
	for i := range 40 {
		run.Unread = append(run.Unread, fmt.Sprintf("source-%d-%s", i, long))
	}
	for i := range 23 {
		run.Agenda = append(run.Agenda, AgendaItem{ID: fmt.Sprintf("A%d", i+1), Source: AgendaFinding,
			Question: long, Verdict: []Verdict{VerdictConfirmed, VerdictRefuted, VerdictUnreadable, ""}[i%4],
			Note: strings.Repeat("n", maxVerdictNote), Numbers: []AgendaNumber{{Label: long, Value: 1, From: long}}})
	}
	for i := range maxRunLines {
		run.Progress = append(run.Progress, Progress{At: monday.Add(time.Duration(i) * time.Second),
			Text: fmt.Sprintf("A%d confirmed: %s", i, long)})
	}
	kept := run.Kept()
	raw, err := json.Marshal(kept)
	if err != nil || len(raw) > advisordomain.MaxModelBytes {
		t.Fatalf("the kept summary is %d bytes (%v), past the log's %d", len(raw), err, advisordomain.MaxModelBytes)
	}
	if kept.Tally == nil || *kept.Tally != (AgendaTally{Items: 23, Confirmed: 6, Refuted: 6, Unreadable: 6, Open: 5}) ||
		kept.Agenda != nil {
		t.Errorf("tally = %+v, agenda %d items; want the agenda counted, not kept", kept.Tally, len(kept.Agenda))
	}
	if len(kept.Progress) != keptLines || kept.Progress[keptLines-1].At != run.Progress[len(run.Progress)-1].At {
		t.Errorf("progress kept = %+v, want the last %d lines", kept.Progress, keptLines)
	}
	if kept.InputTokens != 1_900_000 || kept.Listed != 10 || kept.StoppedBy != StoppedDone || kept.Bounds.Tokens != 2_000_000 ||
		kept.Dropped[advisor.DropWords] != 3 || len(kept.Dropped) != 2 {
		t.Errorf("kept = %+v, want the run's numbers, bounds and the engine's own drops", kept)
	}
	var back DeepRun
	if err := json.Unmarshal(raw, &back); err != nil || back.Status != advisor.PassDone {
		t.Errorf("the kept summary reads back as %+v (%v)", back, err)
	}
}
