package learning_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/workkey"
)

func episodist(t *testing.T, e *learning.Episodes,
	opts ...func(*learning.EpisodistOptions),
) *learning.Episodist {
	t.Helper()
	o := learning.EpisodistOptions{NewID: func() string { return "row-1" }}
	for _, fn := range opts {
		fn(&o)
	}
	w, err := learning.NewEpisodist(e, o)
	if err != nil {
		t.Fatalf("NewEpisodist: %v", err)
	}
	return w
}

// epTurn is a settled turn that engaged with its trigger.
func epTurn() learning.Turn {
	return learning.Turn{
		Role: &org.Role{Name: "Dev"},
		Event: types.TurnCompleted{
			Agent: "agent-uuid", AgentHandle: "dev", RoleName: "Dev",
			// TWO IDENTITIES, as a post-split turn carries them: the run
			// that produced the record, and the unit of work it did.
			TurnID: "run-1", WorkKey: "work-1", TaskID: "task-9",
			StartedAt: base, EndedAt: base.Add(3 * time.Second), DurationMS: 3000,
			TaskSummary:   "the staging deploy keeps failing",
			PlanSummary:   "read the pipeline, then reply",
			ToolSequence:  []string{"read_pipeline", "reply"},
			SkillsUsed:    []string{"skill-a"},
			ReviewOutcome: "done", ConversationKey: "chat:general",
		},
	}
}

func reflectEpisode(t *testing.T, w *learning.Episodist, turn learning.Turn) []events.Payload {
	t.Helper()
	if reason := w.Skip(turn); reason != "" {
		t.Fatalf("the worker skipped a turn it should record: %s", reason)
	}
	payloads, err := w.Reflect(context.Background(), turn)
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	return payloads
}

func TestACompletedTurnBecomesAnEpisode(t *testing.T) {
	t.Parallel()
	store := episodes(t)
	reflectEpisode(t, episodist(t, store), epTurn())

	got, err := store.Recent(context.Background(), "dev", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("wrote %d episodes, want 1", len(got))
	}
	ep := got[0]
	switch {
	case ep.TaskSummary != "the staging deploy keeps failing":
		t.Errorf("task summary = %q", ep.TaskSummary)
	case ep.PlanSummary != "read the pipeline, then reply":
		t.Errorf("plan summary = %q", ep.PlanSummary)
	case strings.Join(ep.ToolSequence, ",") != "read_pipeline,reply":
		t.Errorf("tool sequence = %v", ep.ToolSequence)
	case ep.ReviewOutcome != "done":
		t.Errorf("review outcome = %q", ep.ReviewOutcome)
	case ep.ConversationKey != "chat:general":
		t.Errorf("conversation key = %q", ep.ConversationKey)
	case ep.Duration != 3*time.Second:
		t.Errorf("duration = %s", ep.Duration)
	case ep.Kind != learning.KindRaw:
		t.Errorf("kind = %q, want a raw row", ep.Kind)
	}
}

// THE WORK KEY IS WHAT DEDUPES, so it has to reach the row: two nodes can
// complete one trigger, and an episode keyed on nothing lands twice and then
// weights every later recall with work that happened once.
func TestTheWorkKeyReachesTheRow(t *testing.T) {
	t.Parallel()
	store := episodes(t)
	w := episodist(t, store)
	reflectEpisode(t, w, epTurn())

	got, _ := store.Recent(context.Background(), "dev", 10)
	if got[0].WorkKey != "work-1" {
		t.Fatalf("work key = %q, want the turn's", got[0].WorkKey)
	}

	// The same turn again — a redelivery, or a peer that raced it.
	payloads, err := w.Reflect(context.Background(), epTurn())
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(payloads) != 0 {
		t.Errorf("a collapsed duplicate announced %d events, want none — "+
			"nothing was written to announce", len(payloads))
	}
	again, _ := store.Recent(context.Background(), "dev", 10)
	if len(again) != 1 {
		t.Fatalf("the redelivery wrote a second row: %d rows", len(again))
	}
}

// TWO RUNS OF ONE TRIGGER ARE ONE EPISODE, and this is the guard that stopped
// holding when a turn id started naming a run rather than a unit of work.
//
// A turn that fails without acting is NAK'd and redelivered, so the same
// trigger legitimately runs again — under a new turn id. Keyed on that, the
// unique index has nothing to collide with and the retry writes a SECOND row:
// the seat then recalls one piece of work twice, and every later skill
// synthesis is weighted by a duplicate. The dedupe has to key on the identity
// a redelivery reproduces, which is the work key. See ADR-0017.
func TestTwoRunsOfOneTriggerCollapseToOneEpisode(t *testing.T) {
	t.Parallel()
	store := episodes(t)
	w := episodist(t, store)

	first := epTurn()
	first.Event.TurnID, first.Event.WorkKey = "run-1", "wk-1"
	reflectEpisode(t, w, first)

	// The retry: a different run of the same unit of work.
	second := epTurn()
	second.Event.TurnID, second.Event.WorkKey = "run-2", "wk-1"
	payloads, err := w.Reflect(context.Background(), second)
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(payloads) != 0 {
		t.Errorf("the retry announced %d events, want none — nothing was written",
			len(payloads))
	}
	got, _ := store.Recent(context.Background(), "dev", 10)
	if len(got) != 1 {
		t.Fatalf("two runs of one trigger wrote %d episodes, want 1", len(got))
	}
	if got[0].WorkKey != "wk-1" {
		t.Errorf("work key = %q, want the trigger's", got[0].WorkKey)
	}
	// AND THE ROW STILL NAMES THE RUN IT CAME FROM, which is a different
	// fact: the work key says what was done, the turn id says which
	// execution produced the record.
	if got[0].TurnID != "run-1" {
		t.Errorf("turn id = %q, want the run that actually wrote it", got[0].TurnID)
	}
}

// AND THE TWO ERAS ARE TOLD APART BY SHAPE, because the wire cannot tell them
// apart at all.
//
// A `turn_completed` from a build before the split carries no work key and its
// turn id IS one; a post-split turn with no ledgerable trigger carries no work
// key either, and its turn id is a RUN. The field is `omitempty`, so both
// arrive as absent — and the two want opposite answers. Falling back for both
// arms the counterparty guard with a value that means nothing, which is the
// exact disarming [Counterparties.Record] keeps the last KEYED unit of work to
// avoid.
func TestTheWorkKeyFallbackReadsTheGrammarNotTheAbsence(t *testing.T) {
	t.Parallel()
	// An older build's event: the turn id is a derived key, so it IS one.
	old := epTurn()
	old.Event.TurnID, old.Event.WorkKey = workkey.Derive([]string{"evt-a"}), ""
	if got := old.WorkKey(); got != old.Event.TurnID {
		t.Errorf("work key = %q, want the turn id an older build carried it in", got)
	}

	// A post-split turn with no trigger key: the turn id is a run, and
	// there is no unit of work to answer with.
	unkeyed := epTurn()
	unkeyed.Event.TurnID, unkeyed.Event.WorkKey = uuid.NewString(), ""
	if got := unkeyed.WorkKey(); got != "" {
		t.Errorf("work key = %q, want empty — a fabricated key disarms the "+
			"counterparty dedupe for the next real one", got)
	}
	// Its DELIVERY still has an identity, or every unkeyed turn in the
	// company would collapse onto one mark and reflect exactly once.
	if got := unkeyed.DedupeKey(); got != unkeyed.Event.TurnID {
		t.Errorf("dedupe key = %q, want the run", got)
	}

	fresh := epTurn()
	fresh.Event.TurnID, fresh.Event.WorkKey = "run-1", "wk-1"
	if got := fresh.WorkKey(); got != "wk-1" {
		t.Errorf("work key = %q, want the event's own", got)
	}
}

// A FAILED TURN IS AN EPISODE. Work that did not land is exactly what recall
// should surface the next time this seat is asked to do it again.
func TestAFailedTurnIsStillRecorded(t *testing.T) {
	t.Parallel()
	store := episodes(t)
	turn := epTurn()
	turn.Event.ReviewOutcome = "failed"
	reflectEpisode(t, episodist(t, store), turn)

	got, _ := store.Recent(context.Background(), "dev", 10)
	if len(got) != 1 || got[0].ReviewOutcome != "failed" {
		t.Fatalf("episodes = %+v, want one failed row", got)
	}
}

// A SELF-PERSISTED TURN IS STILL AN EPISODE, unlike for the persist decider:
// the diary holds what the agent CONCLUDED, an episode holds what it DID,
// and one is not the other.
func TestASelfPersistedTurnIsStillRecorded(t *testing.T) {
	t.Parallel()
	turn := epTurn()
	turn.Event.PlanToolSequence = []string{learning.ReflectTool}
	if reason := episodist(t, episodes(t)).Skip(turn); reason != "" {
		t.Fatalf("skipped a self-persisted turn: %s", reason)
	}
}

func TestTheGatesThatKeepEpisodesHonest(t *testing.T) {
	t.Parallel()
	w := episodist(t, episodes(t))
	for _, tc := range []struct {
		name, want string
		mutate     func(*learning.Turn)
	}{
		{
			// A self_iterate round is work the agent judged incomplete;
			// the reattempt is the episode.
			name: "a mid-iteration round", want: "non_terminal",
			mutate: func(tn *learning.Turn) { tn.Event.ReviewOutcome = "self_iterate" },
		},
		{
			// The executor recognised the trigger was for somebody else.
			name: "an explicit skip", want: "no_engagement",
			mutate: func(tn *learning.Turn) { tn.Event.PlanDecision = types.PlanDecisionSkip },
		},
		{
			// Finished done having called nothing: it did not touch the
			// trigger, so there is no work to remember.
			name: "a done turn that called nothing", want: "no_engagement",
			mutate: func(tn *learning.Turn) { tn.Event.ToolSequence = nil },
		},
		{
			name: "a turn with no seat", want: "no_handle",
			mutate: func(tn *learning.Turn) { tn.Event.AgentHandle = "" },
		},
	} {
		turn := epTurn()
		tc.mutate(&turn)
		if got := w.Skip(turn); got != tc.want {
			t.Errorf("%s: skip = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A TRANSIENT EMBEDDING OUTAGE MUST NEVER COST AN EPISODE. The row cannot be
// reconstructed later; the vector can, by nothing more than a re-embed.
func TestAnUnreachableEmbedderStillWritesTheEpisode(t *testing.T) {
	t.Parallel()
	store := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	w := episodist(t, store, func(o *learning.EpisodistOptions) {
		o.Embed = embedderOf(func(context.Context, string) ([]float32, error) {
			return nil, errors.New("the provider is down")
		})
	})
	reflectEpisode(t, w, epTurn())

	got, _ := store.Recent(context.Background(), "dev", 10)
	if len(got) != 1 {
		t.Fatalf("wrote %d episodes, want 1 even with no vector", len(got))
	}
	if got[0].Embeddings != nil {
		t.Errorf("embeddings = %v, want none", got[0].Embeddings)
	}
	if got[0].EmbeddingModel != "" {
		t.Errorf("a row with no vector names the model %q", got[0].EmbeddingModel)
	}
}

func TestAReachableEmbedderStampsTheVector(t *testing.T) {
	t.Parallel()
	store := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	var embedded string
	w := episodist(t, store, func(o *learning.EpisodistOptions) {
		o.Embed = embedderOf(func(_ context.Context, text string) ([]float32, error) {
			embedded = text
			return []float32{0.5, 0.5, 0.5, 0.5}, nil
		})
	})
	reflectEpisode(t, w, epTurn())

	if embedded != "the staging deploy keeps failing" {
		t.Errorf("embedded %q, want the task summary", embedded)
	}
	got, _ := store.Recent(context.Background(), "dev", 10)
	if len(got[0].Embeddings) != 1 || len(got[0].Embeddings[0]) != 4 {
		t.Fatalf("embeddings = %v, want one 4-wide vector", got[0].Embeddings)
	}
	// AND THE MODEL THAT MADE IT, which is what a recall compares it under:
	// a vector filed under no model is recalled by nothing.
	if got[0].EmbeddingModel != testModel {
		t.Errorf("the vector is filed under model %q, want the embedder's %q",
			got[0].EmbeddingModel, testModel)
	}
}

// A SLOW EMBEDDER IS A MISSING VECTOR, not a stalled pass: the write must not
// sit behind a provider while the dispatcher's other workers wait.
func TestASlowEmbedderIsBoundedAndYieldsNoVector(t *testing.T) {
	t.Parallel()
	store := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	w := episodist(t, store, func(o *learning.EpisodistOptions) {
		o.EmbedTimeout = 10 * time.Millisecond
		o.Embed = embedderOf(func(ctx context.Context, _ string) ([]float32, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	})
	reflectEpisode(t, w, epTurn())

	got, _ := store.Recent(context.Background(), "dev", 10)
	if len(got) != 1 || got[0].Embeddings != nil {
		t.Fatalf("episodes = %+v, want one row with no vector", got)
	}
}

// The announcement is what a dashboard counts, so it has to describe the row
// that landed rather than the turn that produced it.
func TestTheWrittenEpisodeIsAnnounced(t *testing.T) {
	t.Parallel()
	payloads := reflectEpisode(t, episodist(t, episodes(t)), epTurn())
	if len(payloads) != 1 {
		t.Fatalf("announced %d events, want 1", len(payloads))
	}
	ev, ok := payloads[0].(types.EpisodeWritten)
	if !ok {
		t.Fatalf("event = %T, want EpisodeWritten", payloads[0])
	}
	if ev.AgentHandle != "dev" || ev.ReviewOutcome != "done" || ev.ToolCount != 2 {
		t.Errorf("event = %+v", ev)
	}
}

func TestAnEpisodistNeedsAStore(t *testing.T) {
	t.Parallel()
	if _, err := learning.NewEpisodist(nil, learning.EpisodistOptions{}); err == nil {
		t.Fatal("an episodist with nowhere to write was accepted")
	}
}

// AN EMPTY TASK SUMMARY IS NOT SENT to the provider. There is nothing to
// embed, the provider would answer ErrEmpty, and the round trip is spent for
// a vector that could not exist — on a pass that runs after every turn.
func TestAnEmptySummaryNeverReachesTheEmbedder(t *testing.T) {
	t.Parallel()
	store := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	var calls int
	w := episodist(t, store, func(o *learning.EpisodistOptions) {
		o.Embed = embedderOf(func(context.Context, string) ([]float32, error) {
			calls++
			return []float32{1, 0, 0, 0}, nil
		})
	})
	turn := epTurn()
	turn.Event.TaskSummary = ""
	reflectEpisode(t, w, turn)

	if calls != 0 {
		t.Fatalf("the embedder was called %d times for an empty summary", calls)
	}
	got, _ := store.Recent(context.Background(), "dev", 10)
	if len(got) != 1 {
		t.Fatalf("wrote %d episodes, want the row anyway", len(got))
	}
}

// wordySummary is a summary of DISTINCT tokens, long enough to need several
// windows.
//
// Distinct is the point: every token is a needle, so "did this word reach a
// vector" is a question a test can ask of any position in the text, and the
// answer for the LAST one is precisely what the 8 000-byte cut used to get
// wrong.
func wordySummary(words int) (string, []string) {
	tokens := make([]string, words)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("tok%05d", i)
	}
	return strings.Join(tokens, " "), tokens
}

// NOTHING IS CUT: every word of a summary too long for one window reaches some
// vector.
//
// This is the invariant the whole change exists for, stated over the text
// rather than over the arithmetic that produces it. Before windowing, the
// summary was handed to textcut.Bytes at 8 000 bytes and everything past that
// reached no vector, no query_episodes result and no prefetch — silently,
// because the cut appended no marker and wrote nothing to the row.
func TestEveryWordOfALongSummaryReachesAVector(t *testing.T) {
	t.Parallel()
	store := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	var sent []string
	w := episodist(t, store, func(o *learning.EpisodistOptions) {
		o.Embed = embedderOf(func(_ context.Context, text string) ([]float32, error) {
			sent = append(sent, text)
			return []float32{1, 0, 0, 0}, nil
		})
	})
	turn := epTurn()
	// ~36 KB, comfortably past the old 8 000-byte cut and past one window.
	summary, tokens := wordySummary(4000)
	turn.Event.TaskSummary = summary
	reflectEpisode(t, w, turn)

	if len(sent) < 2 {
		t.Fatalf("a %d-byte summary produced %d window(s), want several",
			len(summary), len(sent))
	}
	embedded := strings.Join(sent, "\n")
	for _, token := range tokens {
		if !strings.Contains(embedded, token) {
			t.Fatalf("%q reached no window, so no query can find it", token)
		}
	}
	for at, window := range sent {
		if !utf8.ValidString(window) {
			t.Errorf("window %d is not valid UTF-8", at)
		}
	}
}

// A WINDOW IS VALID UTF-8 AT BOTH ENDS. The stride is a byte count, so a
// window's START is as likely to land inside a multi-byte character as its
// end — and text sliced from there reaches the provider as a replacement
// character inside the text it is meant to represent.
func TestNoWindowSplitsARune(t *testing.T) {
	t.Parallel()
	store := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	var sent []string
	w := episodist(t, store, func(o *learning.EpisodistOptions) {
		o.Embed = embedderOf(func(_ context.Context, text string) ([]float32, error) {
			sent = append(sent, text)
			return []float32{1, 0, 0, 0}, nil
		})
	})
	turn := epTurn()
	// Three-byte runes and NO whitespace: nothing for the word-edge walk
	// to find, so every boundary falls wherever the byte arithmetic puts
	// it. 60 000 bytes is not a multiple of the window or of the stride,
	// so the boundaries land inside a rune rather than beside one.
	turn.Event.TaskSummary = strings.Repeat("→", 20000)
	reflectEpisode(t, w, turn)

	if len(sent) < 2 {
		t.Fatalf("produced %d window(s), want several", len(sent))
	}
	joined := 0
	for at, window := range sent {
		if !utf8.ValidString(window) {
			t.Errorf("window %d splits a rune", at)
		}
		joined += utf8.RuneCountInString(window)
	}
	// Overlap means the windows repeat some runes, so the total is more
	// than the input — never less, which is what a gap would look like.
	if joined < 20000 {
		t.Errorf("the windows hold %d runes of a %d-rune summary, so some "+
			"of it reached no vector", joined, 20000)
	}
}

// A SHORT SUMMARY IS ONE WINDOW, VERBATIM. Almost every summary is short, and
// windowing must not change what those rows have always sent: a second window,
// a marker, or a trimmed byte would be a change to every seat's recall in
// exchange for nothing.
func TestAShortSummaryIsOneUntouchedWindow(t *testing.T) {
	t.Parallel()
	store := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	var sent []string
	w := episodist(t, store, func(o *learning.EpisodistOptions) {
		o.Embed = embedderOf(func(_ context.Context, text string) ([]float32, error) {
			sent = append(sent, text)
			return []float32{1, 0, 0, 0}, nil
		})
	})
	turn := epTurn()
	reflectEpisode(t, w, turn)

	if len(sent) != 1 || sent[0] != turn.Event.TaskSummary {
		t.Fatalf("sent %q, want the summary whole in one window", sent)
	}
	got, _ := store.Recent(context.Background(), "dev", 10)
	if len(got[0].Embeddings) != 1 {
		t.Fatalf("stored %d windows, want 1", len(got[0].Embeddings))
	}
}

// THE DEFECT, END TO END: a query that matches only the TAIL of a long
// summary finds the episode.
//
// This is what the 8 000-byte cut cost and what nothing anywhere reported.
// Everything past it reached no vector, so a seat asked to do again what it
// had already done — where "what it had already done" was described in the
// second half of a coalesced trigger — recalled nothing, from its own memory
// of doing it. The needle here is the LAST word of a summary several windows
// long, and the provider puts a window holding it on an axis of its own, so
// only a vector for the tail can answer this query.
func TestARecallMatchingOnlyTheTailFindsTheEpisode(t *testing.T) {
	t.Parallel()
	store := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	const needle = "tailneedle"
	head, _ := wordySummary(4000)
	summary := head + " " + needle
	w := episodist(t, store, func(o *learning.EpisodistOptions) {
		o.Embed = embedderOf(func(_ context.Context, text string) ([]float32, error) {
			if strings.Contains(text, needle) {
				return []float32{1, 0, 0, 0}, nil
			}
			return []float32{0, 1, 0, 0}, nil
		})
	})
	turn := epTurn()
	turn.Event.TaskSummary = summary
	reflectEpisode(t, w, turn)

	hits, err := store.Recall(context.Background(), learning.RecallQuery{
		Handle: "dev", Model: testModel, Embedding: []float32{1, 0, 0, 0},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("a query matching the summary's tail returned %d hits, "+
			"want the episode", len(hits))
	}
	// THE NEAREST WINDOW, not a mean over all of them: averaging would
	// score this at about 1/N and put it under the relevance floor, which
	// is the same invisibility by a different route.
	if hits[0].Similarity < 0.99 {
		t.Errorf("similarity = %v, want the matching window's own score",
			hits[0].Similarity)
	}
	if hits[0].Episode.TaskSummary != summary {
		t.Error("the row does not hold the whole summary")
	}

	// AND THE HEAD IS STILL REACHABLE. Windowing must not trade one end of
	// a summary for the other.
	fromHead, err := store.Recall(context.Background(), learning.RecallQuery{
		Handle: "dev", Model: testModel, Embedding: []float32{0, 1, 0, 0},
	})
	if err != nil || len(fromHead) != 1 {
		t.Fatalf("a query matching the head returned %d hits (%v), want the "+
			"episode", len(fromHead), err)
	}
}

// A FAILED WINDOW COSTS ITS WINDOW, NEVER THE EPISODE — and never the windows
// already paid for. The provider fails per call and those failures are
// transient, so abandoning the set on the first one would throw away vectors
// that were bought and hand the row the same empty recall the cut did.
func TestOneFailedWindowKeepsTheRest(t *testing.T) {
	t.Parallel()
	store := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	calls := 0
	w := episodist(t, store, func(o *learning.EpisodistOptions) {
		o.Embed = embedderOf(func(context.Context, string) ([]float32, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("rate limited")
			}
			return []float32{1, 0, 0, 0}, nil
		})
	})
	turn := epTurn()
	summary, _ := wordySummary(4000)
	turn.Event.TaskSummary = summary
	reflectEpisode(t, w, turn)

	got, _ := store.Recent(context.Background(), "dev", 10)
	if len(got) != 1 {
		t.Fatalf("wrote %d episodes, want 1", len(got))
	}
	// SEVERAL WINDOWS WERE ATTEMPTED, which is what makes the count below
	// mean anything: with one window the "keeps the rest" assertion reads
	// 0 == 0 and holds for a summary that was cut instead of windowed.
	if calls < 3 {
		t.Fatalf("a %d-byte summary made %d embedding call(s), so there was no "+
			"rest to keep", len(summary), calls)
	}
	if want := calls - 1; len(got[0].Embeddings) != want {
		t.Fatalf("stored %d windows, want the %d that succeeded",
			len(got[0].Embeddings), want)
	}
	// The whole summary is still on the row: what a failed window costs is
	// searchability of its slice, never the text.
	if got[0].TaskSummary != summary {
		t.Error("the summary itself was not kept whole")
	}
}
