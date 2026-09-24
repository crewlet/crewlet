package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A SEARCH'S DURATION IS FILED UNDER WHO ASKED AND HOW IT RANKED.
//
// `search_slow` is a promise to whoever waits on a deliberate search, and a
// turn's prefetch is held to a figure of its own; a hybrid search runs the
// vector scan beside the lexical one. One series labelled the same way for
// all four would report a mix of them as the number somebody's target is read
// against. Each answer here is reported once, so each label pair must hold
// exactly the one observation its answer made.
func TestASearchDurationIsFiledUnderWhoAskedAndHowItRanked(t *testing.T) {
	t.Parallel()
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	e := &Engine{metrics: recorder}
	e.reportSearch(search.Answer{BucketsAnswered: 64, Hybrid: true}, time.Millisecond)
	e.reportSearch(search.Answer{BucketsAnswered: 64}, time.Millisecond)
	e.reportSearch(search.Answer{BucketsAnswered: 64, Hybrid: true, Prefetch: true},
		time.Millisecond)
	e.reportSearch(search.Answer{BucketsAnswered: 64, Prefetch: true}, time.Millisecond)

	got := map[[2]string]uint64{}
	for _, snapshot := range recorder.Read() {
		if snapshot.Name != metrics.TrackerSearchScanDuration {
			continue
		}
		got[[2]string{snapshot.Attrs["path"], snapshot.Attrs["rung"]}] += snapshot.Count
	}
	for _, want := range [][2]string{
		{"interactive", "hybrid"}, {"interactive", "lexical"},
		{"prefetch", "hybrid"}, {"prefetch", "lexical"},
	} {
		if got[want] != 1 {
			t.Errorf("path=%s rung=%s holds %d observations, want the one "+
				"search that was reported that way — the series are %v",
				want[0], want[1], got[want], got)
		}
	}
	if len(got) != 4 {
		t.Errorf("four kinds of search were filed under %d label pairs: %v", len(got), got)
	}
}
