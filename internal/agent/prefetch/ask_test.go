package prefetch_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
)

// What every relevance judgement is made against: the turn's ASK, never the
// brief the executor is handed.

// countingEmbed records every text it was asked to embed.
type countingEmbed struct {
	mu    sync.Mutex
	texts []string
}

func (c *countingEmbed) embed(_ context.Context, text string) (learning.Vector, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.texts = append(c.texts, text)
	return learning.Vector{Values: []float32{0.1, 0.2}, Model: "m"}, nil
}

func (c *countingEmbed) asked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.texts...)
}

// ONE VECTOR A TURN, OF WHAT IT WAS ASKED. The memory and episode searches
// are judged against the same text, and each embedding it was two billed
// round trips for one vector; and the text is the ask, because the brief's
// scaffolding is the same on every turn of its surface and dominated the
// vector of every one.
func TestATurnEmbedsWhatItWasAskedOnceForBothSearches(t *testing.T) {
	t.Parallel()
	embedder := &countingEmbed{}
	model := &aux{answers: []string{"[0]"}}
	fetch(t, prefetch.Sources{
		Diary:    diary{recent: []learning.DiaryEntry{memory("m1", "a memory")}},
		Episodes: episodes{hits: []learning.Hit{{Episode: learning.Episode{TaskSummary: "a past turn"}}}},
		Models:   models{provider: model},
		Embed:    embedder.embed,
	}, request(t))

	got := embedder.asked()
	if len(got) != 1 {
		t.Fatalf("the turn was embedded %d times (%q), want once for both searches", len(got), got)
	}
	if got[0] != theAsk {
		t.Fatalf("the turn's vector is of %q, want what it was asked (%q)", got[0], theAsk)
	}
}

// THE THREE AUXILIARY JUDGEMENTS READ THE ASK. The memory filter would have
// matched the roles the scaffolding's worked examples name, the query writer
// would have been handed those examples as search terms, and the episode
// summary would have judged past work against boilerplate.
func TestTheAuxiliaryJudgementsReadWhatWasAskedNotTheBrief(t *testing.T) {
	t.Parallel()
	model := &aux{answers: []string{"[0]"}}
	fetch(t, prefetch.Sources{
		Diary:             diary{recent: []learning.DiaryEntry{memory("m1", "a memory")}},
		Knowledge:         &searcher{hits: []knowledge.Hit{{Title: "Staging runbook"}}},
		Episodes:          episodes{hits: []learning.Hit{{Episode: learning.Episode{TaskSummary: "a past turn"}}}},
		Models:            models{provider: model},
		Embed:             embeds,
		SummarizeEpisodes: true,
	}, request(t))

	prompts := model.prompts()
	if len(prompts) != 3 {
		t.Fatalf("%d auxiliary calls, want the filter, the query and the summary", len(prompts))
	}
	for _, prompt := range prompts {
		if !strings.Contains(prompt, theAsk) {
			t.Errorf("a judgement was not shown what the turn was asked:\n%s", prompt)
		}
		if strings.Contains(prompt, "## Triage") || strings.Contains(prompt, "@SWE") {
			t.Errorf("a judgement read the executor's scaffolding:\n%s", prompt)
		}
	}
}

// AN ASK WITH NOTHING IN IT IS A THIN TRIGGER: there is nothing to judge
// relevance against, so no search runs, nothing is embedded, and each block
// says to look again — exactly as for a pointer.
func TestAnEmptyAskIsJudgedLikeAThinTrigger(t *testing.T) {
	t.Parallel()
	embedder := &countingEmbed{}
	model := &aux{answers: []string{"[0]", "a query"}}
	pages := &searcher{hits: []knowledge.Hit{{Title: "Staging runbook"}}}
	r := request(t)
	r.Ask = "  \n "
	blocks := fetch(t, prefetch.Sources{
		Diary:     diary{recent: []learning.DiaryEntry{memory("m1", "a memory")}},
		Knowledge: pages,
		Episodes:  episodes{hits: []learning.Hit{{Episode: learning.Episode{TaskSummary: "a past turn"}}}},
		Models:    models{provider: model},
		Embed:     embedder.embed,
	}, r)
	if blocks.PersonalMemory != prefetch.EmptyMemoryHint ||
		blocks.RelevantKnowledge != prefetch.EmptyKnowledgeHint ||
		blocks.EpisodeRecall != prefetch.EmptyRecallHint {
		t.Fatalf("blocks = %+v, want every search's hint", blocks)
	}
	if model.calls() != 0 || len(embedder.asked()) != 0 || len(pages.asked()) != 0 {
		t.Fatalf("an empty ask spent %d auxiliary calls, %d embeddings and %d searches",
			model.calls(), len(embedder.asked()), len(pages.asked()))
	}
}

// A THIN TRIGGER EMBEDS NOTHING, the memory block's similarity half included:
// a vector of a pointer is a search keyed on nothing, and the gate the docs
// promised for it was skipped — every pointer turn paid a provider call for a
// pool the gate then threw away.
func TestAThinTriggerEmbedsNothing(t *testing.T) {
	t.Parallel()
	embedder := &countingEmbed{}
	r := request(t)
	r.RequiresRecon = true
	blocks := fetch(t, prefetch.Sources{
		Diary:    diary{recent: []learning.DiaryEntry{memory("m1", "a memory")}},
		Episodes: episodes{hits: []learning.Hit{{Episode: learning.Episode{TaskSummary: "a past turn"}}}},
		Embed:    embedder.embed,
	}, r)
	if got := embedder.asked(); len(got) != 0 {
		t.Fatalf("a thin trigger embedded %q", got)
	}
	if blocks.PersonalMemory != prefetch.EmptyMemoryHint {
		t.Fatalf("memory = %q, want the hint for a seat that has memories", blocks.PersonalMemory)
	}
	// And a thin trigger on a seat with NO memories renders nothing: there is
	// no filter to re-run and nothing for it to find.
	none := fetch(t, prefetch.Sources{Diary: diary{}, Embed: embedder.embed}, r)
	if none.PersonalMemory != "" {
		t.Fatalf("a seat with no memories was told to refresh them: %q", none.PersonalMemory)
	}
}

// THE TURN'S VECTOR IS BOUNDED BY ITS OWN BUDGET, not by the provider's: a
// provider having a bad minute costs the turn its similarity half, never its
// start.
func TestATurnWaitsNoLongerThanItsBudgetForItsVector(t *testing.T) {
	t.Parallel()
	slow := func(ctx context.Context, _ string) (learning.Vector, error) {
		<-ctx.Done()
		return learning.Vector{}, ctx.Err()
	}
	began := time.Now()
	blocks := fetch(t, prefetch.Sources{
		Episodes: episodes{hits: []learning.Hit{{Episode: learning.Episode{TaskSummary: "a past turn"}}}},
		Embed:    slow,
	}, request(t))
	if took := time.Since(began); took > prefetch.EmbedBudget+time.Second {
		t.Fatalf("the turn start waited %v on its vector, past the %v budget", took, prefetch.EmbedBudget)
	}
	if blocks.EpisodeRecall != "" {
		t.Fatalf("recall = %q with no vector, want nothing", blocks.EpisodeRecall)
	}
}

// THE PULL'S RE-FILTER IS TOLD WHO IS ASKING. refresh_memory built its filter
// request with no senders, so the "Current sender:" line the turn-start filter
// judges its per-subject rule by was missing from every re-filter.
func TestTheMemoryReFilterIsToldWhoIsAsking(t *testing.T) {
	t.Parallel()
	model := &aux{answers: []string{"[0]"}}
	f := prefetch.New(prefetch.Sources{
		Diary:  diary{recent: []learning.DiaryEntry{memory("m1", "Sam prefers short replies")}},
		Models: models{provider: model},
		Embed:  embeds,
	})
	_, seat := company(t)
	if _, err := f.RecallMemories(t.Context(), seat, "agent-1", "the deploy freeze",
		[]learning.Subject{{ExternalID: "U2", Platform: "slack", Name: "Miles"}}); err != nil {
		t.Fatalf("RecallMemories: %v", err)
	}
	prompts := model.prompts()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "Current sender: Miles") {
		t.Fatalf("the re-filter was not told who is asking:\n%v", prompts)
	}
}
