package builtin

import (
	"fmt"
	"testing"
)

// THE CACHE HOLDS AT MOST ITS LIMIT, and what goes is what was used least
// recently: a question somebody keeps asking outlives a flood of one-offs.
func TestTheAnswerCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	c := newAnswerCache(3)
	for i := range 3 {
		c.put(fmt.Sprint(i), KnowledgeAnswer{AnswerMD: fmt.Sprint(i)})
	}
	if _, ok := c.get("0"); !ok {
		t.Fatal("an entry inside the limit is gone")
	}
	c.put("3", KnowledgeAnswer{AnswerMD: "3"})
	if c.len() != 3 {
		t.Fatalf("the cache holds %d, past its limit of 3", c.len())
	}
	if _, ok := c.get("1"); ok {
		t.Error("the least recently used entry survived an eviction")
	}
	for _, key := range []string{"0", "2", "3"} {
		if _, ok := c.get(key); !ok {
			t.Errorf("entry %s was evicted ahead of the least recently used", key)
		}
	}
}

// A HIT IS A COPY: stamping `cached` on what a caller got, or editing its
// sources, changes nothing the next caller is served.
func TestACacheHitIsACopy(t *testing.T) {
	t.Parallel()
	c := newAnswerCache(2)
	c.put("q", KnowledgeAnswer{AnswerMD: "a", Sources: []AnswerSource{{Ref: "p-1"}}})
	hit, _ := c.get("q")
	hit.Cached, hit.Sources[0].Ref = true, "changed"
	again, _ := c.get("q")
	if again.Cached || again.Sources[0].Ref != "p-1" {
		t.Fatalf("the stored answer moved with a caller's copy: %+v", again)
	}
}

// THE PLAN'S SIZE, pinned: 256 answers per node.
func TestTheAnswerCacheHoldsTwoHundredFiftySixPerNode(t *testing.T) {
	t.Parallel()
	if AnswerCacheEntries != 256 {
		t.Fatalf("AnswerCacheEntries = %d, want 256", AnswerCacheEntries)
	}
	if got := newAnswerKnowledge(OperatorDeps{}).cache.limit; got != AnswerCacheEntries {
		t.Fatalf("the tool's cache holds %d, want AnswerCacheEntries", got)
	}
}
