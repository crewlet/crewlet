package learning_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// --- fixtures -------------------------------------------------------------

// auxProvider is a scripted auxiliary model.
//
// It replays one completion per call and records what it was asked, including
// the DEADLINE on the call's context — the timeout is a real property of the
// request the decider makes and the only place it can be observed is here.
type auxProvider struct {
	mu      sync.Mutex
	replies []llm.Completion
	err     error
	seen    []llm.Request
	budget  []time.Duration
	calls   int
}

func (p *auxProvider) Model() string { return "aux-test" }

func (p *auxProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.seen = append(p.seen, req)
	if deadline, ok := ctx.Deadline(); ok {
		p.budget = append(p.budget, time.Until(deadline))
	} else {
		p.budget = append(p.budget, 0)
	}
	if p.err != nil {
		return nil, p.err
	}
	if len(p.replies) == 0 {
		return &llm.Completion{}, nil
	}
	c := p.replies[min(p.calls-1, len(p.replies)-1)]
	return &c, nil
}

func (p *auxProvider) request(t *testing.T, i int) llm.Request {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.seen) {
		t.Fatalf("provider saw %d requests, wanted #%d", len(p.seen), i)
	}
	return p.seen[i]
}

// prompt returns the user prompt of the i-th request.
func (p *auxProvider) prompt(t *testing.T, i int) string {
	t.Helper()
	for _, m := range p.request(t, i).Messages {
		if m.Role == llm.RoleUser {
			return m.Content
		}
	}
	t.Fatal("request carried no user message")
	return ""
}

func (p *auxProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func says(bodies ...string) *auxProvider {
	p := &auxProvider{}
	for _, b := range bodies {
		p.replies = append(p.replies, llm.Completion{Content: b})
	}
	return p
}

// stubModels resolves every phase to one provider.
//
// The mutex is not decoration: one decider is shared by every seat on a node
// and reflection runs off a queue consumer, so Head is called concurrently.
// An unguarded recording slice here fails -race for the FIXTURE's bug and
// buries whatever the run was meant to prove.
type stubModels struct {
	mu    sync.Mutex
	p     llm.Provider
	err   error
	asked []phase.Phase
}

func (m *stubModels) Head(_ *org.Role, ph phase.Phase) (chain.Member, error) {
	m.mu.Lock()
	m.asked = append(m.asked, ph)
	m.mu.Unlock()
	if m.err != nil {
		return chain.Member{}, m.err
	}
	return chain.Member{Key: "cheap", Provider: m.p}, nil
}

// fakeDiary is a diary whose halves can be broken independently.
type fakeDiary struct {
	mu        sync.Mutex
	writes    []learning.DiaryEntry
	writeErr  error
	recent    []learning.DiaryEntry
	recentErr error
	askedFor  int
}

func (d *fakeDiary) Write(_ context.Context, e learning.DiaryEntry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.writeErr != nil {
		return d.writeErr
	}
	d.writes = append(d.writes, e)
	return nil
}

func (d *fakeDiary) Recent(_ context.Context, _ string, _ time.Time, limit int) ([]learning.DiaryEntry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.askedFor = limit
	if d.recentErr != nil {
		return nil, d.recentErr
	}
	return d.recent, nil
}

func (d *fakeDiary) written() []learning.DiaryEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]learning.DiaryEntry(nil), d.writes...)
}

var pdClock = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// decider builds a decider over a scripted model and a diary, with the clock
// and the row id pinned so a written row is comparable.
func decider(t *testing.T, p llm.Provider, d learning.DiaryStore, opts ...func(*learning.PersistOptions)) *learning.PersistDecider {
	t.Helper()
	o := learning.PersistOptions{
		Now:   func() time.Time { return pdClock },
		NewID: func() string { return "row-1" },
	}
	for _, fn := range opts {
		fn(&o)
	}
	dec, err := learning.NewPersistDecider(&stubModels{p: p}, d, o)
	if err != nil {
		t.Fatalf("NewPersistDecider: %v", err)
	}
	return dec
}

// pdTurn is a settled turn with an identifiable requester.
func pdTurn() learning.Turn {
	return learning.Turn{
		Role: &org.Role{Name: "Dev"},
		Event: types.TurnCompleted{
			Agent: "agent-uuid", AgentHandle: "dev", RoleName: "Dev", TurnID: "t1",
			TaskSummary: "Help with reporting", PlanSummary: "Summarise",
			ToolSequence: []string{"search"}, ReviewOutcome: "done",
		},
	}
}

func mustDecide(t *testing.T, d *learning.PersistDecider, turn learning.Turn) learning.Decision {
	t.Helper()
	dec, err := d.Decide(context.Background(), turn)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	return dec
}

// --- the four tiers -------------------------------------------------------

func TestALongFactLandsAtAgentScopeWithNoDeadline(t *testing.T) {
	t.Parallel()
	store := diary(t)
	d := decider(t, says(`{"kind": "LONG", "content": "User Sam prefers weekly digests."}`), store)

	dec := mustDecide(t, d, pdTurn())
	if dec.Tier != types.PersistLong || !dec.Persisted() {
		t.Fatalf("decision = %+v, want a persisted LONG", dec)
	}
	if !dec.Entry.TTLUntil.IsZero() {
		t.Errorf("a LONG carries a deadline (%s); it must never expire", dec.Entry.TTLUntil)
	}

	rows, err := store.Recent(context.Background(), "agent-uuid", pdClock, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("diary holds %d rows, want the one that was classified", len(rows))
	}
	got := rows[0]
	if got.Kind != learning.DiaryLong {
		t.Errorf("kind = %q, want %q", got.Kind, learning.DiaryLong)
	}
	if got.Content != "User Sam prefers weekly digests." {
		t.Errorf("content = %q", got.Content)
	}
	// The literal, not the constant: an assertion that reads the constant
	// back passes for whatever the constant is changed to, and the whole
	// point of the stamp is telling a post-turn row from one the turn's own
	// model wrote through reflect_and_persist.
	if got.Source != "persist_decider" || got.TurnID != "t1" {
		t.Errorf("provenance = source %q turn %q", got.Source, got.TurnID)
	}
	if got.Metadata["review_outcome"] != "done" || got.Metadata["agent_handle"] != "dev" {
		t.Errorf("metadata = %v", got.Metadata)
	}
}

// The requirement this test exists for: a SHORT whose expiry the model did
// not propose must not become a LONG. The diary is the REAL one, so a zero
// deadline would not merely be mislabelled — the write would be refused and
// the decision would report nothing persisted.
func TestAShortWithNoProposedExpiryKeepsItsTierAndTakesTheDefaultBand(t *testing.T) {
	t.Parallel()
	store := diary(t)
	d := decider(t, says(`{"kind": "SHORT", "content": "Sarah is OOO."}`), store)

	dec := mustDecide(t, d, pdTurn())
	if dec.Tier != types.PersistShort {
		t.Fatalf("tier = %q, want SHORT — the model chose it", dec.Tier)
	}
	if !dec.Persisted() {
		t.Fatal("nothing was persisted; a SHORT with no ttl_days must still be stored")
	}
	if dec.Entry.Kind != learning.DiaryShort {
		t.Errorf("kind = %q, want %q — a missing expiry must not promote it",
			dec.Entry.Kind, learning.DiaryShort)
	}
	want := pdClock.Add(30 * 24 * time.Hour)
	if !dec.Entry.TTLUntil.Equal(want) {
		t.Errorf("deadline = %s, want the default band %s", dec.Entry.TTLUntil, want)
	}

	// Counterfactual: the same store refuses a short row with no deadline,
	// so "was written as a LONG in disguise" is not a state the code above
	// could have reached quietly.
	err := store.Write(context.Background(), learning.DiaryEntry{
		ID: "x", AgentID: "agent-uuid", Kind: learning.DiaryShort, Content: "no deadline",
	})
	if err == nil {
		t.Fatal("the diary accepted a short entry with no deadline; the guard behind this test is gone")
	}
}

func TestAProposedExpiryIsClampedToTheSupportedBand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		json string
		days int
	}{
		{"honoured", `{"kind":"SHORT","content":"c","ttl_days": 45}`, 45},
		{"absent", `{"kind":"SHORT","content":"c"}`, 30},
		{"null", `{"kind":"SHORT","content":"c","ttl_days": null}`, 30},
		{"zero", `{"kind":"SHORT","content":"c","ttl_days": 0}`, 30},
		{"negative", `{"kind":"SHORT","content":"c","ttl_days": -3}`, 30},
		{"word", `{"kind":"SHORT","content":"c","ttl_days": "soon"}`, 30},
		// A model that quotes its number has classified correctly and
		// formatted badly; demoting it to the default month would lose a
		// real expiry to a formatting choice.
		{"quoted", `{"kind":"SHORT","content":"c","ttl_days": "45"}`, 45},
		{"beyond the cap", `{"kind":"SHORT","content":"c","ttl_days": 900}`, 180},
		{"at the cap", `{"kind":"SHORT","content":"c","ttl_days": 180}`, 180},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeDiary{}
			dec := mustDecide(t, decider(t, says(tc.json), store), pdTurn())
			want := pdClock.Add(time.Duration(tc.days) * 24 * time.Hour)
			if !dec.Entry.TTLUntil.Equal(want) {
				t.Errorf("deadline = %s, want %s (%d days)", dec.Entry.TTLUntil, want, tc.days)
			}
		})
	}
}

func TestADocIsAnnouncedAndNeverMemorised(t *testing.T) {
	t.Parallel()
	store := &fakeDiary{}
	d := decider(t, says(`{"kind":"DOC","content":"Use semantic commit messages.",
		"target_hint":"Engineering / commit conventions","rationale":"Imperative for the whole team."}`), store)

	dec := mustDecide(t, d, pdTurn())
	if dec.Tier != types.PersistDoc {
		t.Fatalf("tier = %q, want DOC", dec.Tier)
	}
	if len(store.written()) != 0 {
		t.Errorf("DOC wrote %d diary rows; a standing rule is policy, not memory", len(store.written()))
	}
	if dec.Persisted() {
		t.Error("DOC reported a persisted row")
	}
	if dec.Directive.Content != "Use semantic commit messages." ||
		dec.Directive.TargetHint != "Engineering / commit conventions" ||
		!strings.Contains(dec.Directive.Rationale, "Imperative") {
		t.Errorf("directive = %+v", dec.Directive)
	}

	payloads, err := d.Reflect(context.Background(), pdTurn())
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	ev := onlyPayload(t, payloads).(types.PersistDeciderCompleted)
	if ev.Classification != types.PersistDoc || ev.Persisted || ev.Scope != "" || ev.DocID != "" {
		t.Errorf("event = %+v, want an unpersisted DOC with no scope", ev)
	}
}

func TestNothingDurableWritesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body string }{
		{"the JSON contract", `{"kind": "NOOP"}`},
		{"the bare sentinel", "NOOP"},
		{"the sentinel with prose", "NOOP - nothing worth keeping here"},
		{"lower case", "noop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeDiary{}
			p := says(tc.body)
			dec := mustDecide(t, decider(t, p, store), pdTurn())
			if dec.Tier != types.PersistNOOP || dec.Persisted() {
				t.Errorf("decision = %+v, want an unpersisted NOOP", dec)
			}
			if len(store.written()) != 0 {
				t.Errorf("NOOP wrote %d rows", len(store.written()))
			}
			if p.count() != 1 {
				t.Errorf("provider called %d times, want exactly one classification", p.count())
			}
		})
	}
}

// --- what a broken classifier may not do ----------------------------------

func TestAMalformedResponseWritesNothingAndFailsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"whitespace", "   \n "},
		{"prose only", "I think this turn was interesting but I am not sure."},
		{"a JSON array", "[1, 2, 3]"},
		{"a JSON string", `"LONG"`},
		{"JSON null", "null"},
		{"truncated object", `{"kind": "LONG", "content": `},
		{"a tier with no content", `{"kind": "LONG"}`},
		{"content but no tier", `{"content": "something durable"}`},
		{"an empty content string", `{"kind":"SHORT","content":"   "}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeDiary{}
			d := decider(t, says(tc.body), store)
			dec, err := d.Decide(context.Background(), pdTurn())
			if err != nil {
				t.Errorf("a malformed response failed the turn: %v", err)
			}
			if dec.Tier != types.PersistNOOP {
				t.Errorf("tier = %q, want NOOP", dec.Tier)
			}
			if len(store.written()) != 0 {
				t.Errorf("wrote %d rows from a malformed response", len(store.written()))
			}
		})
	}

	// Counterfactual: the same harness, given the contract, writes. Without
	// this every assertion above also passes for a decider that never writes.
	store := &fakeDiary{}
	dec := mustDecide(t, decider(t, says(`{"kind":"LONG","content":"a durable fact"}`), store), pdTurn())
	if !dec.Persisted() || len(store.written()) != 1 {
		t.Fatalf("the well-formed control wrote %d rows (%+v)", len(store.written()), dec)
	}
}

func TestAnUnknownTierIsTreatedAsNothingDurable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body string }{
		// The shape an older three-scope prompt asked for. "org" must not
		// be read as anything writable.
		{"a scope instead of a tier", `{"scope": "org", "content": "Anything."}`},
		{"an invented tier", `{"kind": "UNIT", "content": "Anything."}`},
		{"an empty tier", `{"kind": "", "content": "Anything."}`},
		{"a numeric tier", `{"kind": 3, "content": "Anything."}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeDiary{}
			dec := mustDecide(t, decider(t, says(tc.body), store), pdTurn())
			if dec.Tier != types.PersistNOOP {
				t.Errorf("tier = %q, want NOOP", dec.Tier)
			}
			if len(store.written()) != 0 {
				t.Errorf("an unknown tier wrote %d rows", len(store.written()))
			}
		})
	}
}

// A model that lower-cases its own tier has classified correctly and shouted
// wrongly; demoting that to NOOP loses a real fact to a capitalisation.
func TestATierIsReadWhateverCaseItArrivesIn(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"kind":"long","content":"a durable fact"}`,
		`{"kind":" Long ","content":"a durable fact"}`,
	} {
		store := &fakeDiary{}
		dec := mustDecide(t, decider(t, says(body), store), pdTurn())
		if dec.Tier != types.PersistLong || len(store.written()) != 1 {
			t.Errorf("%s -> %+v, wrote %d rows", body, dec, len(store.written()))
		}
	}
}

func TestJSONWrappedInProseIsStillRead(t *testing.T) {
	t.Parallel()
	store := &fakeDiary{}
	body := "Sure! Here is the classification:\n{\"kind\": \"LONG\", \"content\": \"Sam reviews on Mondays.\"}\nHope that helps."
	dec := mustDecide(t, decider(t, says(body), store), pdTurn())
	if dec.Tier != types.PersistLong || !dec.Persisted() {
		t.Fatalf("decision = %+v, want the wrapped LONG to be read", dec)
	}
	if store.written()[0].Content != "Sam reviews on Mondays." {
		t.Errorf("content = %q", store.written()[0].Content)
	}
}

// --- failures the caller must be able to tell apart -----------------------

func TestAFailedWriteReportsTheTierItFailedNotNothingToPersist(t *testing.T) {
	t.Parallel()
	broken := errors.New("disk is unhappy")
	store := &fakeDiary{writeErr: broken}
	d := decider(t, says(`{"kind":"LONG","content":"a durable fact"}`), store)

	dec, err := d.Decide(context.Background(), pdTurn())
	if !errors.Is(err, broken) {
		t.Fatalf("error = %v, want the store's own failure", err)
	}
	if dec.Tier != types.PersistLong {
		t.Errorf("tier = %q; the classification survived the write failure", dec.Tier)
	}
	if dec.Persisted() {
		t.Error("a failed write reported a persisted row")
	}

	payloads, err := d.Reflect(context.Background(), pdTurn())
	if err == nil {
		t.Error("Reflect swallowed the write failure; nothing would ever log it")
	}
	ev := onlyPayload(t, payloads).(types.PersistDeciderCompleted)
	if ev.Persisted || ev.DocID != "" || ev.Scope != "" {
		t.Errorf("event = %+v, want it to claim nothing landed", ev)
	}
	if ev.Classification != types.PersistLong {
		t.Errorf("classification = %q, want the tier that failed", ev.Classification)
	}
}

func TestAnUnreachableModelWritesNothing(t *testing.T) {
	t.Parallel()
	t.Run("the call fails", func(t *testing.T) {
		t.Parallel()
		store := &fakeDiary{}
		p := &auxProvider{err: errors.New("502 from the vendor")}
		dec, err := decider(t, p, store).Decide(context.Background(), pdTurn())
		if err == nil {
			t.Fatal("a failed aux call reported success")
		}
		if dec.Tier != types.PersistNOOP || len(store.written()) != 0 {
			t.Errorf("decision = %+v, wrote %d rows", dec, len(store.written()))
		}
	})
	t.Run("no model resolves", func(t *testing.T) {
		t.Parallel()
		store := &fakeDiary{}
		d, err := learning.NewPersistDecider(
			&stubModels{err: errors.New("no auxiliary chain")}, store, learning.PersistOptions{})
		if err != nil {
			t.Fatalf("NewPersistDecider: %v", err)
		}
		dec, err := d.Decide(context.Background(), pdTurn())
		if err == nil {
			t.Fatal("an unresolvable model reported success")
		}
		if dec.Tier != types.PersistNOOP || len(store.written()) != 0 {
			t.Errorf("decision = %+v, wrote %d rows", dec, len(store.written()))
		}
	})
}

func TestADecidersDependenciesAreRefusedWhenMissing(t *testing.T) {
	t.Parallel()
	if _, err := learning.NewPersistDecider(nil, &fakeDiary{}, learning.PersistOptions{}); err == nil {
		t.Error("a decider with no model registry was accepted")
	}
	if _, err := learning.NewPersistDecider(&stubModels{}, nil, learning.PersistOptions{}); err == nil {
		t.Error("a decider with no diary was accepted; it would classify and then drop every row")
	}
}

// --- the prompt -----------------------------------------------------------

func TestTheDedupBlockRendersWhatTheSeatAlreadyKnows(t *testing.T) {
	t.Parallel()
	expiry := pdClock.Add(72 * time.Hour)
	store := &fakeDiary{recent: []learning.DiaryEntry{
		{ID: "a", Content: "Sam prefers terse replies", Kind: learning.DiaryLong},
		{ID: "b", Content: "Sarah is OOO", Kind: learning.DiaryShort, TTLUntil: expiry},
		{ID: "c", Content: "   "},
		{ID: "d", Content: strings.Repeat("x", 400), Kind: learning.DiaryLong},
	}}
	p := says(`{"kind":"NOOP"}`)
	mustDecide(t, decider(t, p, store), pdTurn())

	prompt := p.prompt(t, 0)
	if !strings.Contains(prompt, "Already in your memory:") {
		t.Fatalf("no dedup block in the prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "0. Sam prefers terse replies") {
		t.Errorf("first memory not rendered:\n%s", prompt)
	}
	// The deadline is what separates "still known" from "knew it until
	// last week", so it has to reach the model.
	if !strings.Contains(prompt, "1. Sarah is OOO (until "+expiry.Format(time.RFC3339)+")") {
		t.Errorf("short memory rendered without its deadline:\n%s", prompt)
	}
	// One bloated row may not crowd out the others, and the empty row must
	// take no index: the numbers are what the read-side filter selects by,
	// so a gap in them is a memory the model cannot point at.
	if strings.Contains(prompt, strings.Repeat("x", 300)) {
		t.Errorf("an overlong memory was not truncated:\n%s", prompt)
	}
	if !strings.Contains(prompt, "2. "+strings.Repeat("x", 240)+"...") {
		t.Errorf("truncation did not land at the cap, or the empty row took index 2:\n%s", prompt)
	}
	if store.askedFor != 50 {
		t.Errorf("dedup pool = %d, want it bounded at 50", store.askedFor)
	}
}

func TestAFreshSeatGetsNoDedupHeader(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		d    *fakeDiary
	}{
		{"nothing remembered yet", &fakeDiary{}},
		{"every row empty", &fakeDiary{recent: []learning.DiaryEntry{{ID: "a"}, {ID: "b", Content: "  "}}}},
		// A store that cannot be read must degrade to the pre-dedup
		// prompt, not to skipping the turn: the alternative drops every
		// fact for as long as the store is unhappy.
		{"the store is unreadable", &fakeDiary{recentErr: errors.New("no route to host")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := says(`{"kind":"LONG","content":"a durable fact"}`)
			dec := mustDecide(t, decider(t, p, tc.d), pdTurn())
			if strings.Contains(p.prompt(t, 0), "Already in your memory") {
				t.Errorf("a bare header reached the prompt:\n%s", p.prompt(t, 0))
			}
			if !dec.Persisted() {
				t.Error("classification stopped because the dedup read produced nothing")
			}
		})
	}
}

func TestEveryCoalescedRequesterReachesThePrompt(t *testing.T) {
	t.Parallel()
	turn := pdTurn()
	turn.Event.Interactions = []types.InboundInteraction{
		{Sender: types.CanonicalIdentity{DisplayName: "Sam", ExternalID: "U0TESTUSER1", Platform: "slack"},
			Body: "please open replies with\nhey sam"},
		{Sender: types.CanonicalIdentity{Handle: "miles"}, Body: "and cc me"},
		// No identifier at all: a scheduled tick or an internal
		// assignment. Rendering "Requester: " for it would invite the
		// model to attribute a fact to nobody.
		{Body: "orphan body"},
	}
	p := says(`{"kind":"NOOP"}`)
	mustDecide(t, decider(t, p, &fakeDiary{}), turn)

	prompt := p.prompt(t, 0)
	for _, want := range []string{
		"- Requester: Sam (slack:U0TESTUSER1)",
		`- Inbound message: "please open replies with hey sam"`,
		"- Requester: miles",
		`- Inbound message: "and cc me"`,
		"- Task: Help with reporting",
		"- Plan: Summarise",
		"- Tools called: search",
		"- Outcome: done",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "orphan body") {
		t.Errorf("a senderless message was attributed to somebody:\n%s", prompt)
	}
	if n := strings.Count(prompt, "- Requester:"); n != 2 {
		t.Errorf("%d requester lines, want one per identifiable sender", n)
	}
}

func TestAnEmptyTurnStillDescribesItself(t *testing.T) {
	t.Parallel()
	turn := learning.Turn{Role: &org.Role{Name: "Dev"}, Event: types.TurnCompleted{
		Agent: "agent-uuid", ReviewOutcome: "failed",
	}}
	p := says(`{"kind":"NOOP"}`)
	mustDecide(t, decider(t, p, &fakeDiary{}), turn)
	prompt := p.prompt(t, 0)
	for _, want := range []string{"- Task: (no description)", "- Plan: (no plan)", "- Tools called: (none)"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

func TestDescribeSenderIsOneRendering(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		id   types.CanonicalIdentity
		want string
	}{
		{"named on a platform",
			types.CanonicalIdentity{DisplayName: "Sam", ExternalID: "U1", Platform: "slack"}, "Sam (slack:U1)"},
		{"named with no platform",
			types.CanonicalIdentity{DisplayName: "Sam", ExternalID: "U1"}, "Sam (U1)"},
		{"a seat", types.CanonicalIdentity{Handle: "dev"}, "dev"},
		{"a handle behind a display name",
			types.CanonicalIdentity{Handle: "dev", DisplayName: "Dev Bot"}, "Dev Bot"},
		{"an id on a platform", types.CanonicalIdentity{ExternalID: "U1", Platform: "slack"}, "slack:U1"},
		{"an id from nowhere", types.CanonicalIdentity{ExternalID: "U1"}, "unknown:U1"},
		{"nobody", types.CanonicalIdentity{}, ""},
		{"a name with no identifier at all",
			types.CanonicalIdentity{DisplayName: "Sam"}, "Sam"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := learning.DescribeSender(tc.id); got != tc.want {
				t.Errorf("DescribeSender(%+v) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

// --- how the call is made -------------------------------------------------

func TestTheClassifierCallIsToollessBoundedAndNearlyDeterministic(t *testing.T) {
	t.Parallel()
	p := says(`{"kind":"NOOP"}`)
	mustDecide(t, decider(t, p, &fakeDiary{}), pdTurn())

	req := p.request(t, 0)
	if len(req.Tools) != 0 {
		t.Errorf("%d tools offered; the contract is a JSON object in the content", len(req.Tools))
	}
	if req.Temperature == nil {
		t.Fatal("no temperature set; the provider default would make the tier drift between identical turns")
	}
	if got := *req.Temperature; got != 0.2 {
		t.Errorf("temperature = %v, want 0.2", got)
	}
	if req.MaxTokens != learning.DefaultAuxTokens {
		t.Errorf("max tokens = %d, want %d — a tighter cap is spent thinking and returns empty content",
			req.MaxTokens, learning.DefaultAuxTokens)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != llm.RoleSystem || req.Messages[1].Role != llm.RoleUser {
		t.Fatalf("messages = %+v, want one system and one user message", req.Messages)
	}
	if req.Messages[0].Content != learning.PersistSystemPrompt {
		t.Error("the system prompt was assembled per call; prompt caching keys on the prefix")
	}
	if !strings.Contains(learning.PersistSystemPrompt, "Declarative facts, not instructions") ||
		!strings.Contains(learning.PersistSystemPrompt, "Already in your memory") {
		t.Error("the system prompt no longer states the rules the write side depends on")
	}
	for _, tier := range []types.PersistClassification{
		types.PersistNOOP, types.PersistDoc, types.PersistLong, types.PersistShort} {
		if !strings.Contains(learning.PersistSystemPrompt, string(tier)) {
			t.Errorf("the system prompt never names the %s tier", tier)
		}
	}
}

// The defaults are asserted as BANDS with the failure behind each, not as the
// numbers themselves: an assertion that reads the constant back passes for
// any value the constant is ever changed to, which is exactly the mutation it
// is supposed to catch.
func TestTheAuxDefaultsStayInsideTheBandsTheyWereChosenFor(t *testing.T) {
	t.Parallel()
	// Under a thousand tokens an extended-thinking model spends the whole
	// cap reasoning and returns empty content: every turn then degrades to
	// NOOP with no error anywhere to say why. Above twenty thousand buys
	// nothing for an answer that is one small JSON object.
	if learning.DefaultAuxTokens < 1000 || learning.DefaultAuxTokens > 20000 {
		t.Errorf("DefaultAuxTokens = %d, outside the band it was chosen for",
			learning.DefaultAuxTokens)
	}
	// Under 30s an ordinary round trip to a small model is cut off and
	// reported as "nothing to persist". Over 5 minutes it stops being a
	// deadline for a dispatcher that consumes completed turns one at a
	// time — a hung provider would stall the company's whole reflection.
	if learning.DefaultAuxTimeout < 30*time.Second || learning.DefaultAuxTimeout > 5*time.Minute {
		t.Errorf("DefaultAuxTimeout = %s, outside the band it was chosen for",
			learning.DefaultAuxTimeout)
	}
}

func TestTheAuxCallCarriesADeadline(t *testing.T) {
	t.Parallel()
	t.Run("the shipped one", func(t *testing.T) {
		t.Parallel()
		p := says(`{"kind":"NOOP"}`)
		mustDecide(t, decider(t, p, &fakeDiary{}), pdTurn())
		got := p.budget[0]
		if got <= 0 || got > learning.DefaultAuxTimeout {
			t.Errorf("deadline in %s, want it inside the %s budget — a hung provider "+
				"stops the whole dispatcher, not just this turn", got, learning.DefaultAuxTimeout)
		}
	})
	t.Run("a raised one", func(t *testing.T) {
		t.Parallel()
		p := says(`{"kind":"NOOP"}`)
		d := decider(t, p, &fakeDiary{}, func(o *learning.PersistOptions) {
			o.CallTimeout = 8 * time.Minute
		})
		mustDecide(t, d, pdTurn())
		if got := p.budget[0]; got <= learning.DefaultAuxTimeout {
			t.Errorf("deadline in %s; a backend whose own budget is larger was cut off anyway", got)
		}
	})
	t.Run("the caller's own is not widened", func(t *testing.T) {
		t.Parallel()
		p := says(`{"kind":"NOOP"}`)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := decider(t, p, &fakeDiary{}).Decide(ctx, pdTurn()); err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if got := p.budget[0]; got > 2*time.Second {
			t.Errorf("deadline in %s, want the caller's shorter one to win", got)
		}
	})
}

func TestTheDeciderAsksForTheAuxiliaryModel(t *testing.T) {
	t.Parallel()
	m := &stubModels{p: says(`{"kind":"NOOP"}`)}
	d, err := learning.NewPersistDecider(m, &fakeDiary{}, learning.PersistOptions{})
	if err != nil {
		t.Fatalf("NewPersistDecider: %v", err)
	}
	if _, err := d.Decide(context.Background(), pdTurn()); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(m.asked) != 1 || m.asked[0] != phase.Auxiliary {
		t.Errorf("asked for %v, want the cheap auxiliary model reflection is meant to run on", m.asked)
	}
}

// --- the worker seam ------------------------------------------------------

func TestTheDeciderSkipsTheTurnsItMustNotClassify(t *testing.T) {
	t.Parallel()
	d := decider(t, says(`{"kind":"NOOP"}`), &fakeDiary{})
	for _, tc := range []struct {
		name string
		turn func(*learning.Turn)
		want string
	}{
		{"mid-turn", func(x *learning.Turn) { x.Event.ReviewOutcome = "self_iterate" }, "non_terminal"},
		{"no outcome at all", func(x *learning.Turn) { x.Event.ReviewOutcome = "" }, "non_terminal"},
		{"already self-persisted in Plan",
			func(x *learning.Turn) { x.Event.PlanToolSequence = []string{"reflect_and_persist"} }, "self_persisted"},
		{"already self-persisted in Execute",
			func(x *learning.Turn) { x.Event.ToolSequence = []string{"reflect_and_persist"} }, "self_persisted"},
		{"done", func(*learning.Turn) {}, ""},
		{"failed", func(x *learning.Turn) { x.Event.ReviewOutcome = "failed" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			turn := pdTurn()
			tc.turn(&turn)
			if got := d.Skip(turn); got != tc.want {
				t.Errorf("Skip = %q, want %q", got, tc.want)
			}
		})
	}
	if d.Name() != "persist_decider" {
		t.Errorf("Name = %q", d.Name())
	}
}

func TestAPersistedRowIsAnnouncedWithWhereToReadItBack(t *testing.T) {
	t.Parallel()
	store := &fakeDiary{}
	d := decider(t, says(`{"kind":"SHORT","content":"freeze until June","ttl_days":10}`), store)
	payloads, err := d.Reflect(context.Background(), pdTurn())
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	ev := onlyPayload(t, payloads).(types.PersistDeciderCompleted)
	if !ev.Persisted || ev.DocID != "row-1" {
		t.Errorf("event = %+v, want the row it wrote", ev)
	}
	if ev.Scope != types.MemoryScopeAgent {
		t.Errorf("scope = %q, want agent — the decider produces no other", ev.Scope)
	}
	if ev.Classification != types.PersistShort {
		t.Errorf("classification = %q", ev.Classification)
	}
	want := pdClock.Add(10 * 24 * time.Hour).Format(time.RFC3339)
	if ev.TTLUntil != want {
		t.Errorf("ttl_until = %q, want %q", ev.TTLUntil, want)
	}
	if ev.Agent != "agent-uuid" || ev.AgentHandle != "dev" || ev.RoleName != "Dev" ||
		ev.TurnID != "t1" || ev.ReviewOutcome != "done" {
		t.Errorf("event lost the turn it belongs to: %+v", ev)
	}
}

func TestALongIsAnnouncedWithNoDeadline(t *testing.T) {
	t.Parallel()
	d := decider(t, says(`{"kind":"LONG","content":"a durable fact"}`), &fakeDiary{})
	payloads, err := d.Reflect(context.Background(), pdTurn())
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if ev := onlyPayload(t, payloads).(types.PersistDeciderCompleted); ev.TTLUntil != "" {
		t.Errorf("ttl_until = %q on a LONG, which never expires", ev.TTLUntil)
	}
}

// A decider is shared by every seat on a node and reflection runs off a queue
// consumer, so two turns can classify at once. Nothing here is per-call state
// that a second caller could observe.
func TestConcurrentClassificationsDoNotCollide(t *testing.T) {
	t.Parallel()
	store := &fakeDiary{}
	d := decider(t, says(`{"kind":"LONG","content":"a durable fact"}`), store)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			turn := pdTurn()
			turn.Event.TurnID = fmt.Sprintf("t%d", i)
			if _, err := d.Decide(context.Background(), turn); err != nil {
				t.Errorf("Decide: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(store.written()) != 8 {
		t.Errorf("%d rows written, want one per turn", len(store.written()))
	}
}

// onlyPayload unwraps the single event a one-subject worker reports.
func onlyPayload(t *testing.T, payloads []events.Payload) events.Payload {
	t.Helper()
	if len(payloads) != 1 {
		t.Fatalf("worker reported %d events, want exactly 1", len(payloads))
	}
	return payloads[0]
}
