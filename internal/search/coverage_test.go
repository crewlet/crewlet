package search_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
)

// countingCorpus answers a fixed coverage, and never a stale document.
type countingCorpus struct {
	source         search.Source
	current, total int
	err            error
}

func (c countingCorpus) Source() search.Source { return c.source }

func (countingCorpus) Stale(context.Context, string, int, int) ([]search.Document, []string, error) {
	return nil, nil, nil
}

func (c countingCorpus) Coverage(context.Context, string, int) (int, int, error) {
	return c.current, c.total, c.err
}

// A COMPANY THAT HAS WRITTEN NOTHING DOWN HAS NO COVERAGE, which is not the
// same fact as a company whose embeddings have stopped.
//
// Zero is the value `recall_below_floor` fires on. Reported for an empty
// corpus it would page every fleet on its first day, and the operator would
// learn that the alarm means nothing — which is how a real stalled backlog
// then goes unnoticed.
func TestAnEmptyCorpusHasNoCoverageRatherThanZeroCoverage(t *testing.T) {
	t.Parallel()
	_, known, err := search.Coverage(t.Context(),
		[]search.Corpus{countingCorpus{source: search.SourceTask}}, "m", 8)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if known {
		t.Error("an empty corpus reported a measurable coverage, so a company " +
			"on its first day fires the alarm a stalled backlog is for")
	}
}

// IT SUMS ACROSS THE CORPORA rather than averaging their fractions.
//
// What the alarm is about is how much of what a search can reach is reachable
// by MEANING. Averaging two fractions weights a twelve-page wiki equally with
// a hundred thousand tasks, so a fully-embedded handful of pages would mask a
// corpus that is barely covered at all.
func TestCoverageIsWeightedByCorpusSizeRatherThanAveraged(t *testing.T) {
	t.Parallel()
	got, known, err := search.Coverage(t.Context(), []search.Corpus{
		countingCorpus{source: search.SourceTask, current: 0, total: 990},
		countingCorpus{source: search.Source("page"), current: 10, total: 10},
	}, "m", 8)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if !known {
		t.Fatal("a thousand sources reported no measurable coverage")
	}
	if got != 0.01 {
		t.Errorf("coverage is %.4f; summed it is 10 of 1000, and the average "+
			"of the two fractions would be 0.5 — which is the reading that "+
			"hides an unembedded corpus behind a small complete one", got)
	}
}

// NEVER ABOVE ONE. A corpus free to answer from two statements can report a
// vector whose source was removed between them, and a fraction above one reads
// as a broken gauge rather than as the rounding it is.
func TestCoverageIsNeverBetterThanComplete(t *testing.T) {
	t.Parallel()
	got, _, err := search.Coverage(t.Context(), []search.Corpus{
		countingCorpus{source: search.SourceTask, current: 12, total: 10},
	}, "m", 8)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if got != 1 {
		t.Errorf("coverage is %.4f for twelve vectors over ten sources", got)
	}
}

// AN UNREADABLE CORPUS IS NOT AN UNCOVERED ONE. Returning a fraction here
// would report a database that could not be read as a company with no
// embeddings — the one reading whose remedy is the opposite.
func TestAnUnreadableCorpusRefusesRatherThanReportingZero(t *testing.T) {
	t.Parallel()
	boom := errors.New("the estate is closed")
	_, known, err := search.Coverage(t.Context(), []search.Corpus{
		countingCorpus{source: search.SourceTask, current: 9, total: 10},
		countingCorpus{source: search.Source("page"), err: boom},
	}, "m", 8)
	if !errors.Is(err, boom) {
		t.Errorf("an unreadable corpus answered %v", err)
	}
	if known {
		t.Error("an unreadable corpus reported a measurable coverage")
	}
}
