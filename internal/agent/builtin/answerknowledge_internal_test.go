package builtin

import (
	"fmt"
	"testing"
)

// THE CACHE HOLDS AT MOST ITS LIMIT, and what goes is what was used least
// recently: a question somebody keeps asking outlives a flood of one-offs.
func TestTheAnswerCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	c := NewAnswerCache(3)
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
	c := NewAnswerCache(2)
	c.put("q", KnowledgeAnswer{AnswerMD: "a", Sources: []AnswerSource{{Ref: "p-1"}}})
	hit, _ := c.get("q")
	hit.Cached, hit.Sources[0].Ref = true, "changed"
	again, _ := c.get("q")
	if again.Cached || again.Sources[0].Ref != "p-1" {
		t.Fatalf("the stored answer moved with a caller's copy: %+v", again)
	}
}

// THE PLAN'S SIZE, pinned: 256 answers per node, which is what a cache asked
// for no particular size holds.
func TestTheAnswerCacheHoldsTwoHundredFiftySixPerNode(t *testing.T) {
	t.Parallel()
	if AnswerCacheEntries != 256 {
		t.Fatalf("AnswerCacheEntries = %d, want 256", AnswerCacheEntries)
	}
	if got := NewAnswerCache(0).limit; got != AnswerCacheEntries {
		t.Fatalf("a cache of no stated size holds %d, want AnswerCacheEntries", got)
	}
}

// THE CACHE IS THE NODE'S, NOT THE CATALOGUE'S. A surface builds its catalogue
// per request, so a tool that built its own cache would answer every question
// afresh: two catalogues over one AnswerDeps must share what it carries.
func TestEveryCatalogueServesTheNodesOneCache(t *testing.T) {
	t.Parallel()
	cache := NewAnswerCache(2)
	deps := OperatorDeps{Answer: AnswerDeps{Cache: cache}}
	first, second := newAnswerKnowledge(deps), newAnswerKnowledge(deps)
	if first.deps.Cache != cache || second.deps.Cache != cache {
		t.Fatal("a catalogue's answer tool holds a cache other than the node's")
	}
}
