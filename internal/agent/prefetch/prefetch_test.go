package prefetch_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// ── the fakes ──

// aux answers the auxiliary calls, recording what it was asked.
type aux struct {
	mu      sync.Mutex
	answers []string
	asked   []llm.Request
	err     error
}

func (a *aux) Complete(_ context.Context, r llm.Request) (*llm.Completion, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, r)
	if a.err != nil {
		return nil, a.err
	}
	if len(a.answers) == 0 {
		return &llm.Completion{}, nil
	}
	answer := a.answers[0]
	if len(a.answers) > 1 {
		a.answers = a.answers[1:]
	}
	return &llm.Completion{Content: answer}, nil
}

func (a *aux) Model() string { return "aux-model" }

func (a *aux) prompts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.asked))
	for _, r := range a.asked {
		var b strings.Builder
		for _, m := range r.Messages {
			b.WriteString(m.Content + "\n")
		}
		out = append(out, b.String())
	}
	return out
}

func (a *aux) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.asked)
}

type models struct{ provider llm.Provider }

func (m models) Head(*org.Role, phase.Phase) (chain.Member, error) {
	if m.provider == nil {
		return chain.Member{}, errors.New("no auxiliary model")
	}
	return chain.Member{Key: "aux", Provider: m.provider}, nil
}

type diary struct {
	hits   []learning.DiaryHit
	recent []learning.DiaryEntry
	err    error

	// marked collects the ids handed to MarkRetrieved. A POINTER because
	// the fake is used as a value in struct literals throughout, so a
	// plain slice field would record into a copy the test cannot see.
	marked *retrievalLog
}

func (d diary) Recall(context.Context, string, learning.RecallQuery, time.Time) ([]learning.DiaryHit, error) {
	return d.hits, d.err
}

func (d diary) Recent(context.Context, string, time.Time, int) ([]learning.DiaryEntry, error) {
	return d.recent, d.err
}

// MarkRetrieved honours ctx, like the real one: store.ExecContext refuses a
// dead context, so a fake that recorded regardless would make the detach
// untestable — the mark would "land" whether or not it was detached.
func (d diary) MarkRetrieved(ctx context.Context, ids []string, at time.Time) {
	if d.marked == nil || ctx.Err() != nil {
		return
	}
	d.marked.record(ids, at)
}

// retrievalLog is what the fake diary saw marked, across goroutines: the
// mark is a DETACHED write, so it does not run on the caller's goroutine
// ordering and a plain slice would be a race under -race.
type retrievalLog struct {
	mu   sync.Mutex
	ids  []string
	at   time.Time
	call int
}

func (l *retrievalLog) record(ids []string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ids = append(l.ids, ids...)
	l.at = at
	l.call++
}

func (l *retrievalLog) seen() ([]string, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.ids), l.call
}

type episodes struct {
	hits []learning.Hit
	err  error
}

func (e episodes) Recall(context.Context, learning.RecallQuery) ([]learning.Hit, error) {
	return e.hits, e.err
}

type counterparties struct {
	byExternal map[string]learning.Profile
	err        error
}

func (c counterparties) Get(_ context.Context, _ string, s learning.Subject) (learning.Profile, bool, error) {
	if c.err != nil {
		return learning.Profile{}, false, c.err
	}
	p, ok := c.byExternal[s.ExternalID]
	return p, ok, nil
}

type skills struct {
	rows []learning.Skill
	err  error
}

func (s skills) List(context.Context, string, learning.ListOptions) ([]learning.Skill, error) {
	return s.rows, s.err
}

type onboarding struct {
	done bool
	err  error
}

func (o onboarding) Onboarded(context.Context, string, string) (bool, error) {
	return o.done, o.err
}

// searcher is a knowledge backend that records the query it was given.
type searcher struct {
	mu      sync.Mutex
	hits    []knowledge.Hit
	queries []knowledge.Query
	cannot  bool
}

func (s *searcher) Backend() string { return "fake" }

func (s *searcher) CanSearch(*org.Role, *org.Organization) bool { return !s.cannot }

func (s *searcher) Search(_ context.Context, q knowledge.Query) []knowledge.Hit {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, q)
	return s.hits
}

func (s *searcher) asked() []knowledge.Query {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]knowledge.Query(nil), s.queries...)
}

// ── the fixture ──

func company(t *testing.T) (*org.Organization, *org.Role) {
	t.Helper()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		Name: "Engineering", Lead: "Tech Lead",
		Roles: []*org.Role{{Name: "Tech Lead", DeclaredHandle: "lead"}},
	}}}
	o.Normalize()
	return o, o.Role("Tech Lead")
}

func request(t *testing.T) prefetch.Request {
	t.Helper()
	o, seat := company(t)
	return prefetch.Request{
		Seat: seat, AgentID: "agent-1", Org: o,
		Task:   "fix the login redirect loop on staging",
		TurnID: "turn-1",
		Senders: []learning.Subject{
			{ExternalID: "U1", Platform: "chat", Name: "Ana Ruiz"},
		},
	}
}

func memory(id, content string) learning.DiaryEntry {
	return learning.DiaryEntry{
		ID: id, AgentID: "agent-1", Kind: learning.DiaryLong, Content: content,
		CreatedAt: time.Now().Add(-time.Hour),
	}
}

func embeds(context.Context, string) ([]float32, error) { return []float32{0.1, 0.2}, nil }

func fetch(t *testing.T, src prefetch.Sources, r prefetch.Request) prefetch.Blocks {
	t.Helper()
	return prefetch.New(src).Fetch(t.Context(), r)
}

// ── everything degrades to nothing ──

// A NIL SOURCE IS A SUPPORTED CONFIGURATION, not a degraded one: a company
// with reflection off, no knowledge backend, or no database has exactly
// this, and a turn must still start.
func TestTheZeroSourcesRenderNothingAndDoNotPanic(t *testing.T) {
	t.Parallel()
	blocks := fetch(t, prefetch.Sources{}, request(t))
	if !blocks.Empty() {
		t.Fatalf("blocks = %+v, want all empty", blocks)
	}
}

func TestASeatlessRequestRendersNothing(t *testing.T) {
	t.Parallel()
	blocks := fetch(t, prefetch.Sources{Diary: diary{recent: []learning.DiaryEntry{
		memory("m1", "always use semantic commits")}}}, prefetch.Request{})
	if !blocks.Empty() {
		t.Fatalf("blocks = %+v, want all empty", blocks)
	}
}

// A STORE THAT RAISES COSTS ITS BLOCK, never the turn.
func TestEveryStoreFailureCostsOnlyItsOwnBlock(t *testing.T) {
	t.Parallel()
	failing := errors.New("the database is unhappy")
	blocks := fetch(t, prefetch.Sources{
		Diary:          diary{err: failing},
		Episodes:       episodes{err: failing},
		Counterparties: counterparties{err: failing},
		Skills:         skills{err: failing},
		Onboarding:     onboarding{err: failing},
		Models:         models{provider: &aux{answers: []string{"[0]"}}},
		Embed:          embeds,
	}, request(t))
	if !blocks.Empty() {
		t.Fatalf("blocks = %+v, want all empty", blocks)
	}
}

// The six run CONCURRENTLY, so a panic in one would take the process down
// rather than the turn — and these renderers decode model output and index
// with numbers an LLM produced.
func TestAPanickingSourceCostsOnlyItsOwnBlock(t *testing.T) {
	t.Parallel()
	blocks := fetch(t, prefetch.Sources{
		Counterparties: panicking{},
		Skills: skills{rows: []learning.Skill{
			{Name: "ship-a-fix", Description: "the release checklist"}}},
	}, request(t))
	if blocks.CounterpartyProfile != "" {
		t.Fatalf("the panicking source rendered %q", blocks.CounterpartyProfile)
	}
	if !strings.Contains(blocks.SynthesizedSkills, "ship-a-fix") {
		t.Fatalf("a sibling block was lost: %q", blocks.SynthesizedSkills)
	}
}

type panicking struct{}

func (panicking) Get(context.Context, string, learning.Subject) (learning.Profile, bool, error) {
	panic("a malformed profile")
}

// ── personal memory ──

func TestTheFilterDecidesWhichMemoriesReachThePrompt(t *testing.T) {
	t.Parallel()
	model := &aux{answers: []string{"[1]"}}
	blocks := fetch(t, prefetch.Sources{
		Diary: diary{recent: []learning.DiaryEntry{
			memory("m1", "Sam prefers very short replies"),
			memory("m2", "always use semantic commit messages"),
		}},
		Models: models{provider: model},
	}, request(t))

	if !strings.Contains(blocks.PersonalMemory, "semantic commit") {
		t.Fatalf("the selected memory is missing:\n%s", blocks.PersonalMemory)
	}
	if strings.Contains(blocks.PersonalMemory, "Sam prefers") {
		t.Fatalf("an unselected memory reached the prompt:\n%s", blocks.PersonalMemory)
	}
}

// THE SENDER IS NAMED IN THE FILTER PROMPT. Without it the filter has only
// whatever platform ids appear in the task body, and rule 3 — the one hard
// subject filter — becomes unenforceable.
func TestTheFilterIsToldWhoTriggeredTheTurn(t *testing.T) {
	t.Parallel()
	model := &aux{answers: []string{"[]"}}
	fetch(t, prefetch.Sources{
		Diary:  diary{recent: []learning.DiaryEntry{memory("m1", "a memory")}},
		Models: models{provider: model},
	}, request(t))

	prompts := model.prompts()
	if len(prompts) == 0 {
		t.Fatal("the filter was never asked")
	}
	if !strings.Contains(prompts[0], "Current sender: Ana Ruiz") {
		t.Fatalf("the filter was not told the sender:\n%s", prompts[0])
	}
}

// NO UNFILTERED FALLBACK. Falling back to "the most recent eight" would leak
// a memory about one person into a turn triggered by another — the exact
// failure the filter exists to prevent, reappearing on the transient
// outages nobody watches for.
func TestAnUnavailableFilterSurfacesNoMemoryAtAll(t *testing.T) {
	t.Parallel()
	stored := diary{recent: []learning.DiaryEntry{
		memory("m1", "Sam prefers very short replies")}}

	for _, tc := range []struct {
		name string
		src  prefetch.Sources
	}{
		{"no model configured", prefetch.Sources{Diary: stored}},
		{"the model refused", prefetch.Sources{Diary: stored,
			Models: models{provider: &aux{err: errors.New("503")}}}},
		{"the model answered nothing", prefetch.Sources{Diary: stored,
			Models: models{provider: &aux{answers: []string{""}}}}},
		{"the model answered nonsense", prefetch.Sources{Diary: stored,
			Models: models{provider: &aux{answers: []string{"I think memory 1"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := fetch(t, tc.src, request(t)).PersonalMemory
			if strings.Contains(got, "Sam prefers") {
				t.Fatalf("an unfiltered memory reached the prompt:\n%s", got)
			}
		})
	}
}

// A seat with memories and nothing selected gets the HINT; a seat with no
// memories gets nothing, because there is no filter to re-run.
func TestTheEmptyHintSeparatesNothingSelectedFromNothingStored(t *testing.T) {
	t.Parallel()
	selected := fetch(t, prefetch.Sources{
		Diary:  diary{recent: []learning.DiaryEntry{memory("m1", "a memory")}},
		Models: models{provider: &aux{answers: []string{"[]"}}},
	}, request(t)).PersonalMemory
	if selected != prefetch.EmptyMemoryHint {
		t.Fatalf("a seat with memories and none selected got %q", selected)
	}

	fresh := fetch(t, prefetch.Sources{
		Diary:  diary{},
		Models: models{provider: &aux{answers: []string{"[]"}}},
	}, request(t)).PersonalMemory
	if fresh != "" {
		t.Fatalf("a seat with no memories got %q", fresh)
	}
}

// AN INDEX OUTSIDE THE POOL IS NOT AN ANSWER. Acting on it would surface an
// unrelated memory or panic, both worse than an empty block.
func TestAnOutOfRangeSelectionIsRefused(t *testing.T) {
	t.Parallel()
	got := fetch(t, prefetch.Sources{
		Diary: diary{recent: []learning.DiaryEntry{
			memory("m1", "the only memory")}},
		Models: models{provider: &aux{answers: []string{"[0, 7, -1, 0]"}}},
	}, request(t)).PersonalMemory

	if strings.Count(got, "the only memory") != 1 {
		t.Fatalf("a duplicate or phantom selection rendered:\n%s", got)
	}
}

// A SHORT MEMORY RENDERS ITS EXPIRY. Read as permanent it is worse than not
// having it: an agent still honouring last quarter's deploy freeze is
// confidently wrong.
func TestAShortLivedMemoryRendersItsExpiry(t *testing.T) {
	t.Parallel()
	expiring := learning.DiaryEntry{
		ID: "m1", AgentID: "agent-1", Kind: learning.DiaryShort,
		Content: "we are in a deploy freeze", CreatedAt: time.Now(),
		TTLUntil: time.Now().Add(72 * time.Hour),
	}
	model := &aux{answers: []string{"[0]"}}
	got := fetch(t, prefetch.Sources{
		Diary:  diary{recent: []learning.DiaryEntry{expiring}},
		Models: models{provider: model},
	}, request(t)).PersonalMemory

	if !strings.Contains(got, "short-lived") || !strings.Contains(got, "expires") {
		t.Fatalf("a short memory rendered without its expiry:\n%s", got)
	}
	if !strings.Contains(model.prompts()[0], "[short-lived]") {
		t.Fatalf("the filter was not told the memory expires:\n%s", model.prompts()[0])
	}
}

// An expired memory is not a candidate at all — the store may still hold it
// until the sweep runs.
func TestAnExpiredMemoryIsNeverACandidate(t *testing.T) {
	t.Parallel()
	expired := learning.DiaryEntry{
		ID: "m1", AgentID: "agent-1", Kind: learning.DiaryShort,
		Content: "last quarter's freeze", CreatedAt: time.Now().Add(-90 * 24 * time.Hour),
		TTLUntil: time.Now().Add(-24 * time.Hour),
	}
	model := &aux{answers: []string{"[0]"}}
	got := fetch(t, prefetch.Sources{
		Diary:  diary{recent: []learning.DiaryEntry{expired}},
		Models: models{provider: model},
	}, request(t)).PersonalMemory

	if strings.Contains(got, "last quarter") {
		t.Fatalf("an expired memory reached the prompt:\n%s", got)
	}
	if model.calls() != 0 {
		t.Fatal("the filter was asked about a pool with nothing in it")
	}
}

// THE POOL IS A UNION: similarity finds what the task is about, recency
// finds the standing rules that match no particular task. Either alone
// misses a whole category.
func TestTheCandidatePoolUnionsSimilarityWithRecency(t *testing.T) {
	t.Parallel()
	// THREE MEMORIES AND THREE ROUTES: one only similarity finds, one only
	// recency finds, one both do. Anything less and dropping a half of the
	// union costs nothing observable.
	both := memory("m1", "the staging redirect is behind the new proxy")
	vectorOnly := memory("m3", "the old redirect rule was retired in March")
	recentOnly := memory("m2", "always use semantic commit messages")

	model := &aux{answers: []string{"[0, 1, 2]"}}
	got := fetch(t, prefetch.Sources{
		Diary: diary{
			hits:   []learning.DiaryHit{{Entry: both}, {Entry: vectorOnly}},
			recent: []learning.DiaryEntry{both, recentOnly},
		},
		Models: models{provider: model},
		Embed:  embeds,
	}, request(t)).PersonalMemory

	for _, want := range []string{"new proxy", "retired in March", "semantic commit"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the pool missed %q:\n%s", want, got)
		}
	}
	// DEDUPED: a memory found both ways is one memory, and rendering it
	// twice would spend the budget saying it twice.
	if strings.Count(got, "new proxy") != 1 {
		t.Fatalf("a memory found both ways rendered twice:\n%s", got)
	}
	// And the filter was shown three candidates, not four: the memory
	// found both ways appears once in the pool it judges.
	if got := strings.Count(model.prompts()[0], "new proxy"); got != 1 {
		t.Fatalf("the filter saw the same memory %d times:\n%s",
			got, model.prompts()[0])
	}
}

// ── the thin-trigger gate ──

// A POINTER IS NOT A TASK. Judging relevance against "PR #42 got a comment"
// spends a call to filter against a sentence with no content, so the three
// searches skip and say re-searching later is worth it — which is true
// precisely because recon makes the trigger real.
func TestAThinTriggerSkipsTheSearchesAndSaysWhy(t *testing.T) {
	t.Parallel()
	model := &aux{answers: []string{"[0]", "redirect staging"}}
	pages := &searcher{hits: []knowledge.Hit{{Title: "Staging runbook"}}}

	r := request(t)
	r.RequiresRecon = true
	blocks := fetch(t, prefetch.Sources{
		Diary:     diary{recent: []learning.DiaryEntry{memory("m1", "a memory")}},
		Knowledge: pages,
		Episodes:  episodes{hits: []learning.Hit{{Episode: learning.Episode{TaskSummary: "a past turn"}}}},
		Models:    models{provider: model},
		Embed:     embeds,
	}, r)

	if blocks.PersonalMemory != prefetch.EmptyMemoryHint {
		t.Fatalf("memory = %q", blocks.PersonalMemory)
	}
	if blocks.RelevantKnowledge != prefetch.EmptyKnowledgeHint {
		t.Fatalf("knowledge = %q", blocks.RelevantKnowledge)
	}
	if blocks.EpisodeRecall != prefetch.EmptyRecallHint {
		t.Fatalf("recall = %q", blocks.EpisodeRecall)
	}
	if model.calls() != 0 {
		t.Fatalf("a thin trigger spent %d auxiliary calls", model.calls())
	}
	if len(pages.asked()) != 0 {
		t.Fatal("a thin trigger searched the knowledge base")
	}
}

// ── relevant knowledge ──

func TestTheModelWritesTheSearchQuery(t *testing.T) {
	t.Parallel()
	model := &aux{answers: []string{"login redirect loop staging proxy"}}
	pages := &searcher{hits: []knowledge.Hit{
		{Title: "Staging runbook", Snippet: "how the proxy is wired"}}}

	got := fetch(t, prefetch.Sources{
		Knowledge: pages, Models: models{provider: model},
	}, request(t)).RelevantKnowledge

	asked := pages.asked()
	if len(asked) != 1 || asked[0].Text != "login redirect loop staging proxy" {
		t.Fatalf("the search ran as %+v", asked)
	}
	if !strings.Contains(got, "Staging runbook") ||
		!strings.Contains(got, "how the proxy is wired") {
		t.Fatalf("the hit did not render:\n%s", got)
	}
	// THE POINTER IS THE POINT: a seat acting on a snippet would be acting
	// on the first two hundred characters of a runbook.
	if !strings.Contains(got, "look it up by title") {
		t.Fatalf("the block does not say to open the page:\n%s", got)
	}
}

// A chatty model prefixes an explanation or wraps the query in quotes, and
// both would be searched verbatim — a quoted query matches nothing.
func TestAChattyQueryIsReducedToItsFirstRealLine(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{
		"\n\n\"login redirect staging\"\n",
		"login redirect staging\nThat should find the runbook.",
		"'login redirect staging'",
	} {
		pages := &searcher{}
		fetch(t, prefetch.Sources{
			Knowledge: pages,
			Models:    models{provider: &aux{answers: []string{answer}}},
		}, request(t))
		asked := pages.asked()
		if len(asked) != 1 || asked[0].Text != "login redirect staging" {
			t.Fatalf("%q searched as %+v", answer, asked)
		}
	}
}

// THE CHEAP GATE FIRST: CanSearch does no I/O, and its whole job is to let
// the query call be skipped when the search is a guaranteed no-op.
func TestAGuaranteedEmptySearchSpendsNoModelCall(t *testing.T) {
	t.Parallel()
	model := &aux{answers: []string{"a query"}}
	got := fetch(t, prefetch.Sources{
		Knowledge: &searcher{cannot: true}, Models: models{provider: model},
	}, request(t)).RelevantKnowledge

	if got != "" {
		t.Fatalf("knowledge = %q", got)
	}
	if model.calls() != 0 {
		t.Fatalf("a gated search spent %d model calls", model.calls())
	}
}

// AUTO-DRAFTS ARE HIDDEN. Those pages are unreviewed proposals a synthesis
// pass wrote; a planner cannot tell one from a ratified runbook, and
// following one is how a draft becomes policy without anyone agreeing to it.
func TestTheSearchHidesUnreviewedDrafts(t *testing.T) {
	t.Parallel()
	pages := &searcher{}
	fetch(t, prefetch.Sources{
		Knowledge: pages, Models: models{provider: &aux{answers: []string{"q"}}},
	}, request(t))

	asked := pages.asked()
	if len(asked) != 1 ||
		!strings.Contains(strings.Join(asked[0].ExcludeAncestors, ","),
			knowledge.AutoDraftedParent) {
		t.Fatalf("the search did not exclude drafts: %+v", asked)
	}
}

func TestASearchThatFindsNothingSaysToLookAgain(t *testing.T) {
	t.Parallel()
	got := fetch(t, prefetch.Sources{
		Knowledge: &searcher{}, Models: models{provider: &aux{answers: []string{"q"}}},
	}, request(t)).RelevantKnowledge
	if got != prefetch.EmptyKnowledgeHint {
		t.Fatalf("knowledge = %q", got)
	}
}

// ── episode recall ──

func TestRecallRendersWhatAPastTurnWasAndHowItWent(t *testing.T) {
	t.Parallel()
	got := fetch(t, prefetch.Sources{
		Episodes: episodes{hits: []learning.Hit{{Episode: learning.Episode{
			TaskSummary:   "fixed a redirect loop on staging",
			ToolSequence:  []string{"read_file", "run_sandbox"},
			ReviewOutcome: "done",
		}}}},
		Embed: embeds,
	}, request(t)).EpisodeRecall

	for _, want := range []string{"redirect loop", "outcome: done",
		"read_file → run_sandbox"} {
		if !strings.Contains(got, want) {
			t.Fatalf("recall is missing %q:\n%s", want, got)
		}
	}
}

// NO FALLBACK TO RECENCY. Recall's whole claim is "this resembles what you
// are doing now"; the three most recent turns carry no such claim, and a
// planner told they are similar work will treat them as precedent.
func TestRecallWithNoEmbeddingSurfacesNothing(t *testing.T) {
	t.Parallel()
	got := fetch(t, prefetch.Sources{
		Episodes: episodes{hits: []learning.Hit{{Episode: learning.Episode{
			TaskSummary: "something recent"}}}},
	}, request(t)).EpisodeRecall
	if got != "" {
		t.Fatalf("recall = %q, want nothing without a similarity search", got)
	}
}

// The summary is OPTIONAL and its failure is free: the raw bullets are
// already usable, so a slow model costs verbosity rather than the block.
func TestTheEpisodeSummaryIsOptionalAndFailsSoft(t *testing.T) {
	t.Parallel()
	hits := episodes{hits: []learning.Hit{{Episode: learning.Episode{
		TaskSummary: "fixed a redirect loop on staging"}}}}

	off := fetch(t, prefetch.Sources{Episodes: hits, Embed: embeds,
		Models: models{provider: &aux{answers: []string{"a summary"}}},
	}, request(t)).EpisodeRecall
	if !strings.Contains(off, "redirect loop") {
		t.Fatalf("with summarising off, recall = %q", off)
	}

	on := fetch(t, prefetch.Sources{Episodes: hits, Embed: embeds,
		SummarizeEpisodes: true,
		Models:            models{provider: &aux{answers: []string{"- fixed staging redirects before"}}},
	}, request(t)).EpisodeRecall
	if !strings.Contains(on, "fixed staging redirects before") {
		t.Fatalf("with summarising on, recall = %q", on)
	}

	failed := fetch(t, prefetch.Sources{Episodes: hits, Embed: embeds,
		SummarizeEpisodes: true,
		Models:            models{provider: &aux{err: errors.New("503")}},
	}, request(t)).EpisodeRecall
	if !strings.Contains(failed, "redirect loop") {
		t.Fatalf("a failed summary lost the block: %q", failed)
	}
}

// ── counterparty ──

// ONE BLOCK PER DISTINCT SENDER: a coalesced trigger is several people
// speaking, and profiling only the latest hands the planner a profile of
// whoever spoke last while it answers all of them.
func TestEverySenderWithAProfileIsRendered(t *testing.T) {
	t.Parallel()
	r := request(t)
	r.Senders = []learning.Subject{
		{ExternalID: "U1", Platform: "chat", Name: "Ana Ruiz"},
		{ExternalID: "U2", Platform: "chat", Name: "Miles Okafor"},
		{ExternalID: "U1", Platform: "chat", Name: "Ana Ruiz"},
		{ExternalID: "U3", Platform: "chat", Name: "A Stranger"},
	}
	// TWENTY TRAITS EACH, not one. With one trait apiece both blocks fit
	// inside any plausible character budget, so this guard passed for as
	// long as the budget was silently rendering only the FIRST sender —
	// a test that cannot fail on the bug it names.
	traits := func(prefix string) map[string]any {
		out := map[string]any{}
		for i := range 20 {
			out[prefix+"-trait-"+strconv.Itoa(i)] = strings.Repeat("a wordy observed value ", 4)
		}
		return out
	}
	u1 := traits("ana")
	u1["tone"] = "terse"
	u2 := traits("miles")
	u2["timezone"] = "CET"
	got := fetch(t, prefetch.Sources{Counterparties: counterparties{
		byExternal: map[string]learning.Profile{
			"U1": {Subject: r.Senders[0], InteractionCount: 12, Traits: u1},
			"U2": {Subject: r.Senders[1], InteractionCount: 3, Traits: u2},
		}}}, r).CounterpartyProfile

	for _, want := range []string{"Ana Ruiz", "Miles Okafor", "tone: terse", "timezone: CET"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the profile block is missing %q:\n%s", want, got)
		}
	}
	// A sender with no profile is silent — a first interaction is the
	// ordinary case and not worth a line saying so.
	if strings.Contains(got, "A Stranger") {
		t.Fatalf("an unprofiled sender rendered:\n%s", got)
	}
	// And a repeated sender is one block.
	if strings.Count(got, "Ana Ruiz") != 1 {
		t.Fatalf("a repeated sender rendered twice:\n%s", got)
	}
	// SEPARATED BY A BLANK LINE. Each profile is several lines, so joining
	// on a single newline runs the second sender's opening straight onto
	// the end of the first sender's last trait.
	if !strings.Contains(got, "\n\n") {
		t.Fatalf("two multi-line profiles were joined without a blank line:\n%s", got)
	}
}

// THE SUBJECT HEADER IS NOT OPTIONAL: this block arrives with no
// conversational context, so traits with no name attached invite applying
// them to whoever is asking.
func TestAProfileAlwaysNamesItsSubject(t *testing.T) {
	t.Parallel()
	r := request(t)
	got := fetch(t, prefetch.Sources{Counterparties: counterparties{
		byExternal: map[string]learning.Profile{
			"U1": {Subject: r.Senders[0], Traits: map[string]any{"tone": "terse"}},
		}}}, r).CounterpartyProfile

	if !strings.HasPrefix(got, "Subject: Ana Ruiz") {
		t.Fatalf("the profile does not lead with its subject:\n%s", got)
	}
}

// Traits render in a STABLE order: a map's order is randomised, and a
// profile that reorders itself between two otherwise-identical turns breaks
// the provider's prompt cache for nothing.
func TestTraitsRenderInAStableOrder(t *testing.T) {
	t.Parallel()
	r := request(t)
	src := prefetch.Sources{Counterparties: counterparties{
		byExternal: map[string]learning.Profile{
			"U1": {Subject: r.Senders[0], Traits: map[string]any{
				"tone": "terse", "timezone": "CET", "areas": []any{"auth", "billing"},
				"prefers": "bullet points", "notes": "reviews on Fridays",
			}},
		}}}
	first := fetch(t, src, r).CounterpartyProfile
	for range 6 {
		if got := fetch(t, src, r).CounterpartyProfile; got != first {
			t.Fatalf("the profile reordered:\n%s\n---\n%s", first, got)
		}
	}
	if !strings.Contains(first, "areas: auth, billing") {
		t.Fatalf("a list trait did not render:\n%s", first)
	}
}

// ── skills ──

func TestTheSkillsBlockIsAMenuWithItsVerb(t *testing.T) {
	t.Parallel()
	got := fetch(t, prefetch.Sources{Skills: skills{rows: []learning.Skill{
		{Name: "ship-a-fix", Description: "the release checklist", State: learning.SkillActive},
		{Name: "triage-a-page", Description: "what to do when paged", State: learning.SkillStale},
	}}}, request(t)).SynthesizedSkills

	for _, want := range []string{"ship-a-fix", "the release checklist",
		"triage-a-page", "Load any of these by name"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the skills block is missing %q:\n%s", want, got)
		}
	}
	// STALE IS MARKED, NOT HIDDEN: it still works and loading revives it,
	// and knowing it has gone unused is the context for deciding whether
	// to.
	if !strings.Contains(got, "_(unused lately)_") {
		t.Fatalf("a stale skill rendered without its marker:\n%s", got)
	}
}

// ── onboarding ──

func TestTheOnboardingHintRendersOnlyForASeatThatNeedsIt(t *testing.T) {
	t.Parallel()
	r := request(t)
	pending := fetch(t, prefetch.Sources{Onboarding: onboarding{done: false}}, r).OnboardingHint
	if !strings.Contains(pending, "not yet completed onboarding") {
		t.Fatalf("a seat that has not onboarded got %q", pending)
	}
	settled := fetch(t, prefetch.Sources{Onboarding: onboarding{done: true}}, r).OnboardingHint
	if settled != "" {
		t.Fatalf("a settled seat got %q", settled)
	}
}

// FAIL CLOSED on an unreadable marker: the alternative nags a seat that
// onboarded months ago on every turn a database blip lasts, and a missed
// hint costs one turn of context while a false one costs a paragraph of
// every prompt.
func TestAnUnreadableOnboardingMarkerRendersNoHint(t *testing.T) {
	t.Parallel()
	got := fetch(t, prefetch.Sources{
		Onboarding: onboarding{err: errors.New("the database is unhappy")},
	}, request(t)).OnboardingHint
	if got != "" {
		t.Fatalf("an unreadable marker rendered %q", got)
	}
}

// ── block assembly ──

// EVERY SKILL IS LISTED. The block is a menu of names and one-line
// descriptions, and the ids it reports back are what reset a skill's
// staleness clock — so a skill dropped from the menu is never offered, never
// used, and ages out of the catalogue on its own. It used to be cut twice:
// a 12-entry list cap and a character budget on top of it.
func TestEverySynthesizedSkillReachesTheMenu(t *testing.T) {
	t.Parallel()
	var rows []learning.Skill
	for i := range 40 {
		rows = append(rows, learning.Skill{
			ID:          "id-" + strconv.Itoa(i),
			Name:        "skill-" + string(rune('a'+i%26)) + strconv.Itoa(i),
			Description: strings.Repeat("a long description that used to eat budget ", 4),
		})
	}
	blocks := fetch(t, prefetch.Sources{Skills: skills{rows: rows}}, request(t))
	got := blocks.SynthesizedSkills
	for _, row := range rows {
		if !strings.Contains(got, row.Name) {
			t.Fatalf("the menu dropped %q:\n%s", row.Name, got)
		}
	}
	// The ids reported as OFFERED must match what actually rendered, or a
	// skill the model never saw has its staleness clock reset.
	if len(blocks.SkillIDs) != len(rows) {
		t.Fatalf("offered ids = %d, want %d — the reported set and the rendered "+
			"menu disagree", len(blocks.SkillIDs), len(rows))
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "before doing the work it covers.") {
		t.Fatalf("the block did not end on a whole line:\n%s", got[max(0, len(got)-200):])
	}
	// Every rendered line is a WHOLE bullet: entries are dropped or kept,
	// never cut in half.
	for line := range strings.SplitSeq(got, "\n") {
		if strings.HasPrefix(line, "- ") && !strings.Contains(line, "**") {
			t.Fatalf("a bullet was cut mid-render: %q", line)
		}
	}
}

// A provider answering (nil, nil) violates its contract, and the recover is
// for what nobody foresaw — reaching it here would report a panic where the
// honest answer is "this backend returned nothing".
func TestAProviderThatAnswersNothingAtAllIsHandled(t *testing.T) {
	t.Parallel()
	got := fetch(t, prefetch.Sources{
		Diary:  diary{recent: []learning.DiaryEntry{memory("m1", "a memory")}},
		Models: models{provider: nilProvider{}},
	}, request(t)).PersonalMemory
	if got != prefetch.EmptyMemoryHint {
		t.Fatalf("memory = %q", got)
	}
}

type nilProvider struct{}

func (nilProvider) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return nil, nil
}

func (nilProvider) Model() string { return "nil-provider" }

// WHAT THE FILTER PICKED IS A USE, and it is recorded.
//
// The trim that bounds a seat's durable memory orders by retrieval count
// first, so an unmarked recall makes it evict the OLDEST entries — exactly
// what a cap on worth rather than age exists to avoid. Nothing marked a
// retrieval before this, so retrieval_count was permanently zero in every
// deployment and the ordering was decorative.
func TestTheMemoriesTheFilterPickedAreMarkedRetrieved(t *testing.T) {
	t.Parallel()
	log := &retrievalLog{}
	picked := memory("m1", "the release train is Thursdays")
	passedOver := memory("m2", "the old redirect rule was retired in March")

	got := fetch(t, prefetch.Sources{
		Diary:  diary{recent: []learning.DiaryEntry{picked, passedOver}, marked: log},
		Models: models{provider: &aux{answers: []string{"[0]"}}},
	}, request(t)).PersonalMemory

	if !strings.Contains(got, "release train") {
		t.Fatalf("the filter's pick never reached the block:\n%s", got)
	}
	ids, calls := log.seen()
	if calls == 0 {
		t.Fatal("the recall marked nothing: the trim will evict by age instead of by use")
	}
	if !slices.Contains(ids, "m1") {
		t.Errorf("marked %v, want the entry the filter picked", ids)
	}
	// A CANDIDATE IS NOT A USE. The pool is similarity union recency, so
	// counting candidates would move the counter for every entry a seat
	// owns on every turn and make the ordering meaningless the other way.
	if slices.Contains(ids, "m2") {
		t.Errorf("marked %v, which includes a candidate the filter passed over", ids)
	}
}

// AND THE MARK OUTLIVES THE TURN'S CONTEXT.
//
// The write happens after the seat already had the benefit of the recall,
// and a turn's context is routinely cancelled the moment its phase ends.
// Inheriting that cancellation would drop the count on exactly the turns
// that ran longest.
func TestARecallIsMarkedEvenWhenTheTurnsContextIsDone(t *testing.T) {
	t.Parallel()
	log := &retrievalLog{}
	ctx, cancel := context.WithCancel(t.Context())
	src := prefetch.Sources{
		Diary:  diary{recent: []learning.DiaryEntry{memory("m1", "a memory")}, marked: log},
		Models: models{provider: &aux{answers: []string{"[0]"}}},
	}
	// Cancelled between the fetch's reads and the mark is not something a
	// test can time; cancelling before proves the stronger property, since
	// the mark must still land on an already-dead context.
	cancel()
	prefetch.New(src).Fetch(ctx, request(t))

	ids, calls := log.seen()
	if calls == 0 {
		t.Fatal("the mark inherited the turn's cancellation, so a recall on a " +
			"turn that is already finishing never moves the counter")
	}
	if !slices.Contains(ids, "m1") {
		t.Errorf("marked %v, want the entry the filter picked", ids)
	}
}
