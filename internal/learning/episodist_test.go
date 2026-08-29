package learning_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
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
			TurnID: "work-1", TaskID: "task-9",
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
			// The planner recognised the trigger was for somebody else.
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
		o.Embed = func(context.Context, string) ([]float32, error) {
			return nil, errors.New("the provider is down")
		}
	})
	reflectEpisode(t, w, epTurn())

	got, _ := store.Recent(context.Background(), "dev", 10)
	if len(got) != 1 {
		t.Fatalf("wrote %d episodes, want 1 even with no vector", len(got))
	}
	if got[0].Embedding != nil {
		t.Errorf("embedding = %v, want none", got[0].Embedding)
	}
}

func TestAReachableEmbedderStampsTheVector(t *testing.T) {
	t.Parallel()
	store := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	var embedded string
	w := episodist(t, store, func(o *learning.EpisodistOptions) {
		o.Embed = func(_ context.Context, text string) ([]float32, error) {
			embedded = text
			return []float32{0.5, 0.5, 0.5, 0.5}, nil
		}
	})
	reflectEpisode(t, w, epTurn())

	if embedded != "the staging deploy keeps failing" {
		t.Errorf("embedded %q, want the task summary", embedded)
	}
	got, _ := store.Recent(context.Background(), "dev", 10)
	if len(got[0].Embedding) != 4 {
		t.Fatalf("embedding = %v, want the 4-wide vector", got[0].Embedding)
	}
}

// A SLOW EMBEDDER IS A MISSING VECTOR, not a stalled pass: the write must not
// sit behind a provider while the dispatcher's other workers wait.
func TestASlowEmbedderIsBoundedAndYieldsNoVector(t *testing.T) {
	t.Parallel()
	store := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	w := episodist(t, store, func(o *learning.EpisodistOptions) {
		o.EmbedTimeout = 10 * time.Millisecond
		o.Embed = func(ctx context.Context, _ string) ([]float32, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	reflectEpisode(t, w, epTurn())

	got, _ := store.Recent(context.Background(), "dev", 10)
	if len(got) != 1 || got[0].Embedding != nil {
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
		o.Embed = func(context.Context, string) ([]float32, error) {
			calls++
			return []float32{1, 0, 0, 0}, nil
		}
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

// THE EMBED INPUT IS CAPPED, and on a rune boundary: a coalesced trigger
// merges N messages and a webhook body can be a whole diff, so an uncapped
// summary is a request the provider refuses on length — and a byte-sliced
// one is a request it refuses on invalid UTF-8.
func TestALongSummaryIsCappedOnARuneBoundary(t *testing.T) {
	t.Parallel()
	store := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	var sent string
	w := episodist(t, store, func(o *learning.EpisodistOptions) {
		o.Embed = func(_ context.Context, text string) ([]float32, error) {
			sent = text
			return []float32{1, 0, 0, 0}, nil
		}
	})
	turn := epTurn()
	// Three-byte runes, so a byte-aligned cut lands mid-character.
	turn.Event.TaskSummary = strings.Repeat("→", 20000)
	reflectEpisode(t, w, turn)

	if len(sent) >= len(turn.Event.TaskSummary) {
		t.Fatalf("sent %d bytes uncapped", len(sent))
	}
	if !utf8.ValidString(sent) {
		t.Fatal("the cap split a rune, so the provider gets invalid UTF-8")
	}
}
