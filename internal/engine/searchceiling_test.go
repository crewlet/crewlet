package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/tracker"
)

// NOBODY MAY ASK THE FAN-OUT FOR MORE THAN IT CAN ANSWER.
//
// A participant returns its top-[search.FuseN] PER METHOD, so the fusion sees
// at most that many of each however many the caller asks for. A limit above
// it is answered short, and a short result set is indistinguishable from a
// short corpus — the exact confusion `buckets_missing` exists to prevent on
// the other axis.
//
// Today every caller sits under it: the tracker's ceiling is fifty and the
// knowledge seam's widest ask is its default times the page reader's
// over-fetch. That is an AGREEMENT BETWEEN CONSTANTS IN FOUR PACKAGES, none
// of which mentions the others, and it is the shape this repository keeps
// finding out about late. This is where it is held, because this package is
// where the searcher and its callers are wired together.
//
// If a ceiling legitimately has to rise, raise FuseN with it — and read
// [search.Stage1Depth] first, which is derived from it.
func TestNoSearchCallerAsksForMoreThanAFanOutCanAnswer(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		who  string
		asks int
	}{
		{"the tracker's own search ceiling", tracker.MaxSearchLimit},
		{"the knowledge seam, through the page reader's over-fetch",
			knowledge.DefaultLimit * pages.SearchOverfetch},
	} {
		if c.asks > search.FuseN {
			t.Errorf("%s asks for %d and a fan-out answers at most %d "+
				"per method: the extra would come back missing, "+
				"looking exactly like a corpus that holds no more",
				c.who, c.asks, search.FuseN)
		}
	}
}
