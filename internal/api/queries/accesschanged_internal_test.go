package queries

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessreview"
)

// reviewsFake is each seat's pending review: the SRE's change was told at
// toldAt, the PM's record cannot be read, and every read is counted.
type reviewsFake struct {
	toldAt time.Time
	reads  map[string]int
}

func (r *reviewsFake) PendingAccessReview(_ context.Context, handle string) (accessreview.Pending, bool, error) {
	r.reads[handle]++
	switch handle {
	case "pm":
		return accessreview.Pending{}, false, errors.New("the coordination store is out of service")
	case "sre":
		return accessreview.Pending{Baseline: "t-1", Digest: "d", ToldTurn: "t-review", ToldAt: r.toldAt,
			Lines: []string{"SSH: its level is now Read (was Operate)."}}, true, nil
	}
	return accessreview.Pending{}, false, nil
}

// ONE READ PER SEAT, AND THE QUESTIONS DECIDED IN MEMORY: a question asked
// before the change was told is noted, one after it is not, a seat with no
// pending review notes nothing, and one whose record cannot be read notes
// nothing either.
//
// Mutation: read the record per question and the reads count past one a seat.
func TestEachSeatsReviewIsReadOncePerAnswer(t *testing.T) {
	t.Parallel()
	toldAt := time.Date(2026, 11, 8, 9, 30, 0, 0, time.UTC)
	reviews := &reviewsFake{toldAt: toldAt, reads: map[string]int{}}
	changes := &accessChanges{reviews: reviews, detailed: true, read: map[string]*accessreview.Pending{}}
	if got := changes.of(t.Context(), "sre", toldAt.Add(-time.Hour)); got == nil || len(got.Lines) != 1 {
		t.Fatalf("a question asked before the change was told notes %+v", got)
	}
	if got := changes.of(t.Context(), "sre", toldAt.Add(time.Hour)); got != nil {
		t.Errorf("a question asked after the change was told notes %+v", got)
	}
	for _, handle := range []string{"pm", "pm", "cto"} {
		if got := changes.of(t.Context(), handle, toldAt); got != nil {
			t.Errorf("%s notes %+v", handle, got)
		}
	}
	if reviews.reads["sre"] != 1 || reviews.reads["pm"] != 1 || reviews.reads["cto"] != 1 {
		t.Errorf("the records were read %v times, want once a seat", reviews.reads)
	}
}
