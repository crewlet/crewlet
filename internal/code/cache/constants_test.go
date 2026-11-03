package cache_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
)

// A SYNC LEAVES HALF THE NODE'S FETCHES: one repository's round takes at most
// half of what the node reads at once, so a second repository's sync — a
// person's clone, a push's own step — starts with half the node's slots free
// rather than waiting behind the first's longest downloads. And the node's
// bound is the warm connections to a host the bucket's transport keeps
// (s3obj's clone of httpx's, over HTTP/1.1, httpx.MaxIdleConnsPerHost of
// them), so every GET of a burst of single-part bundles rides one.
func TestASyncLeavesHalfTheNodesFetches(t *testing.T) {
	t.Parallel()
	if 2*cache.FetchConcurrency != cache.NodeFetches {
		t.Errorf("FetchConcurrency %d is not half of NodeFetches %d: a second repository's sync "+
			"must start with half the node's fetches free", cache.FetchConcurrency, cache.NodeFetches)
	}
	if cache.NodeFetches != httpx.MaxIdleConnsPerHost {
		t.Errorf("NodeFetches %d is not httpx.MaxIdleConnsPerHost %d: the node's reads at once are "+
			"what the bucket's transport keeps warm connections for", cache.NodeFetches, httpx.MaxIdleConnsPerHost)
	}
}

// FOUR COLD NODES HOLD AT MOST HALF OF A DATA NODE'S ANSWERS: every page of a
// full read holds one of the answers the data node it reads gives its shared
// lane (queue.ConcurrentAnswers, estate's laneShared), which every read of
// the fleet shares, as well as one of its readers. The node's full reads at
// once (MaxFullRounds) are the smallest data node's readers, so it is the
// readers that fill first, never the lane, while four nodes join together;
// a reader floor (store.MinReaderConns) raised past eight fails here, to be
// measured again — BenchmarkColdJoin's other reads broke the rule at sixteen
// a node.
func TestFourColdNodesHoldAtMostHalfADataNodesAnswers(t *testing.T) {
	t.Parallel()
	if 4*cache.MaxFullRounds > queue.ConcurrentAnswers/2 {
		t.Errorf("MaxFullRounds %d: four nodes cold at once would hold %d of a data node's %d answers, "+
			"past the half the rest of the fleet's reads keep", cache.MaxFullRounds, 4*cache.MaxFullRounds,
			queue.ConcurrentAnswers)
	}
}

// A SECOND REPOSITORY IS NEVER STARVED OF FETCHES, at the production ratio:
// while one repository's round holds every download its share allows — held
// open by the store — a second repository's sync downloads and completes in
// the half left to it.
func TestASecondRepositoryIsNeverStarvedOfFetches(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	a, b := w.repo(t, 1), w.repo(t, 2)
	held := map[objstore.Key]bool{}
	for range 6 {
		_, row := w.push(t, a, "refs/heads/main", nil)
		held[row.Object] = true
		w.push(t, b, "refs/heads/main", nil)
	}
	c := w.open(t)
	cache.SetNodeFetches(c, 4)
	cache.SetFetchConcurrency(c, 2)
	release := make(chan struct{})
	w.objects.hold = func(k objstore.Key) <-chan struct{} {
		if held[k] {
			return release
		}
		return nil
	}
	first := make(chan error, 1)
	go func() {
		s, err := c.Sync(context.Background(), a, linearizable)
		if err == nil {
			s.Release()
		}
		first <- err
	}()
	eventually(t, "the first repository's downloads held", func() bool { return w.objects.open() >= 2 })

	ctx, cancel := context.WithTimeout(t.Context(), hangDetector)
	defer cancel()
	s, err := c.Sync(ctx, b, linearizable)
	if err != nil {
		t.Fatalf("a second repository's sync while the first holds its share: %v", err)
	}
	s.Release()
	close(release)
	if err = <-first; err != nil {
		t.Fatal(err)
	}
}
