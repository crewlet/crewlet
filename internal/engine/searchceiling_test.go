package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/tracker"
)

// NO CALLER ASKS THE FAN-OUT FOR MORE THAN IT CAN ANSWER.
//
// A participant returns its top-[search.FuseN] PER METHOD, so the fusion sees
// at most that many of each however many the caller asks for, and
// [search.FanOut.Search] refuses a larger limit rather than answer it short.
// A caller past the ceiling would therefore meet that refusal — which a
// knowledge search, best effort by contract, turns into an empty answer and a
// log line on every call.
//
// Every caller's ask is an AGREEMENT BETWEEN CONSTANTS IN SEVERAL PACKAGES,
// and this is where it is held, because this package is where the searchers
// and their callers are wired together. The knowledge seam's callers reach the
// fan-out through the native page searcher, which over-fetches by
// [pages.SearchOverfetch] to leave room for what it drops after the ranking;
// the tracker's callers reach it through the tracker's searcher, which refuses
// any ask past [tracker.MaxSearchLimit].
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
		{"a knowledge query that names no limit",
			knowledge.DefaultLimit * pages.SearchOverfetch},
		{"the turn-start knowledge block",
			prefetch.KnowledgeHits * pages.SearchOverfetch},
		{"a seat's or an operator's search_knowledge",
			builtin.SearchKnowledgeHits * pages.SearchOverfetch},
		{"the dashboard's knowledge search",
			queries.KnowledgeHitLimit * pages.SearchOverfetch},
	} {
		if c.asks > search.FuseN {
			t.Errorf("%s asks for %d and a fan-out answers at most %d "+
				"per method: the ask is refused, and the caller's search "+
				"answers nothing", c.who, c.asks, search.FuseN)
		}
	}
}
