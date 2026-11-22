package modelpass

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// wordsReads names its reads in words, and fails token spend.
type wordsReads struct{}

func (wordsReads) Questions() []Question {
	return []Question{{Name: "tokens", Words: "token spend"}, {Name: "work_items", Words: "work items"}}
}

func (wordsReads) Answer(_ context.Context, name string, _ map[string]any) (any, error) {
	if name == "tokens" {
		return nil, errors.New("refused")
	}
	return map[string]any{"items": []any{}}, nil
}

// A DEEP RUN'S STEPS SAY WHAT IT READ IN WORDS, as the administrator reads
// them: what, whose and over when, never a read's name or its parameters as
// the model wrote them; a read that failed and a verdict say so in the same
// words. Mutation: write the read's name and parameters, and this fails.
func TestADeepRunsStepsSayWhatItReadInWords(t *testing.T) {
	t.Parallel()
	var lines []string
	s := &deepSurface{data: DeepData{Reads: wordsReads{}}, served: advisor.NewServed(), now: time.Now,
		asked: map[string]string{}, failed: map[string]bool{}, agenda: []AgendaItem{{ID: "A1"}},
		progress: func(p Progress) { lines = append(lines, p.Text) }}
	for _, raw := range []string{
		`{"name":"tokens","params":{"days":7,"previous":true}}`,
		`{"name":"work_items","params":{"assignee":"sre","status_group":"not_started","limit":30}}`,
	} {
		args, err := toolargs.Decode([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.read(t.Context(), args); err != nil {
			t.Fatal(err)
		}
	}
	if r := s.verdict(map[string]any{"id": "A1", "verdict": "unreadable", "note": "token spend did not answer",
		"read": "tokens"}); r.Failed {
		t.Fatalf("verdict refused: %s", r.Output)
	}
	want := []string{
		"Reading token spend, the 7 days before",
		"Could not read token spend, the 7 days before",
		"Reading work items for sre, not started",
		"Could not check: token spend did not answer",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("steps =\n%q\nwant\n%q", lines, want)
	}
}
