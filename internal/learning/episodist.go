package learning

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// EpisodistSource names the worker in a pass result and in its logs.
const EpisodistSource = "episodist"

// episodeEmbedInput caps what is handed to the embedding provider.
//
// Every provider has a token limit and a task summary is not bounded: a
// coalesced trigger merges N messages, and a webhook body can be a whole
// diff. The cap is in CHARACTERS because that is what this layer can count
// without a tokenizer per provider, and 8000 is comfortably inside the
// ~8k-TOKEN window the third-generation OpenAI models offer — a summary long
// enough to be truncated here has already said what it is about many times
// over.
const episodeEmbedInput = 8000

// Embed turns text into a vector, or reports that it cannot.
//
// A FUNCTION rather than the provider interface: it is the one thing this
// worker wants from embeddings, and taking the interface would make every
// test that writes an episode implement a Width it never reads.
type Embed func(ctx context.Context, text string) ([]float32, error)

// Episodist records one completed turn as an episode.
//
// # Why this is a worker and not a write at the end of the turn
//
// An episode is what a seat DID, and the turn engine already knows that as
// the turn ends — so writing it there would be one fewer hop. It is a worker
// because the write must be gated on the same questions the other learning
// passes are gated on (did the turn settle, did the agent engage), and a
// gate that lives in the turn engine is a gate no operator can turn off and
// no pass result reports on. The episode table is the substrate the whole
// read side runs on; a company whose recall is empty needs to be able to see
// WHICH gate closed.
//
// # It writes even when there is no vector
//
// [Episode.Embedding] is nil when the embedder was unreachable, and the row
// still lands: recall skips such rows while the time-window and outcome
// queries still surface them, and the lifecycle worker's clustering falls
// back to its token overlap. A transient embedding outage must never cost an
// episode — the row cannot be reconstructed later, and the vector can, by
// nothing more than a re-embed.
type Episodist struct {
	episodes *Episodes
	embed    Embed
	timeout  time.Duration
	now      func() time.Time
	newID    func() string
}

// EpisodistOptions configures the worker.
type EpisodistOptions struct {
	// Embed is the vector backend, or nil for a company with none.
	Embed Embed

	// EmbedTimeout bounds the embedding call. Zero takes the default.
	EmbedTimeout time.Duration

	Now   func() time.Time
	NewID func() string
}

// DefaultEmbedTimeout bounds one episode's embedding call.
//
// The same 15 seconds the provider defaults to, applied again HERE because
// this caller has its own reason for it: an episode write must not sit on a
// slow provider while the reflect dispatcher's other workers wait behind it,
// and a nil vector is a supported outcome. The bound is what makes "no
// vector" reachable instead of "no episode".
const DefaultEmbedTimeout = 15 * time.Second

// NewEpisodist builds the worker over an episode store.
func NewEpisodist(e *Episodes, opts EpisodistOptions) (*Episodist, error) {
	if e == nil {
		return nil, fmt.Errorf("learning: the episodist needs an episode store to write to")
	}
	w := &Episodist{
		episodes: e, embed: opts.Embed,
		timeout: opts.EmbedTimeout, now: opts.Now, newID: opts.NewID,
	}
	if w.timeout <= 0 {
		w.timeout = DefaultEmbedTimeout
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	if w.newID == nil {
		w.newID = uuid.NewString
	}
	return w, nil
}

// Name implements [Worker].
func (w *Episodist) Name() string { return EpisodistSource }

// Skip implements [Worker].
//
// The two gates the persist decider does NOT share are the interesting part.
// It skips a self-persisted turn because the fact is already in the diary;
// an episode is a different record of a different thing — what the seat did,
// not what it concluded — so a turn that wrote its own memory still earns
// one. It runs on a FAILED turn for the same reason: an episode of work that
// did not land is exactly what recall should surface next time the seat is
// asked to do it again.
func (w *Episodist) Skip(t Turn) string {
	if !t.Settled() {
		// A self_iterate round is work the agent itself judged
		// incomplete; the turn will reattempt, and the reattempt is the
		// episode.
		return "non_terminal"
	}
	if !t.Engaged() {
		// The planner opted out, or was coerced to direct and called
		// nothing. There is no work here to remember, and a row saying
		// otherwise would weight every later recall with a turn in which
		// the seat did nothing.
		return "no_engagement"
	}
	if t.Event.AgentHandle == "" {
		// Episodes are keyed on the seat. An unkeyed row is one no
		// recall can ever find, so it is not worth writing.
		return "no_handle"
	}
	return ""
}

// Reflect implements [Worker].
func (w *Episodist) Reflect(ctx context.Context, t Turn) ([]events.Payload, error) {
	ep := w.episodeOf(t)
	ep.Embedding = w.vector(ctx, ep.TaskSummary)

	written, err := w.episodes.Append(ctx, ep)
	if err != nil {
		return nil, fmt.Errorf("learning: append episode for %s: %w", ep.Handle, err)
	}
	if !written {
		// A DUPLICATE IS NOT AN EVENT. The work key already had a row, so
		// nothing was written, and publishing episode_written for it
		// would report the same turn twice to every surface that counts
		// them — which is the exact miscount the unique index exists to
		// prevent.
		log.DebugContext(ctx, "episode_already_recorded", "seat", ep.Handle, "work_key", ep.WorkKey)
		return nil, nil
	}
	return []events.Payload{types.EpisodeWritten{
		Agent:         t.Event.Agent,
		AgentHandle:   t.Event.AgentHandle,
		RoleName:      t.Event.RoleName,
		TurnID:        t.Event.TurnID,
		ReviewOutcome: t.Event.ReviewOutcome,
		DurationMS:    t.Event.DurationMS,
		ToolCount:     len(t.Event.ToolSequence),
	}}, nil
}

// episodeOf projects a completed turn onto a raw episode row.
func (w *Episodist) episodeOf(t Turn) Episode {
	ended := t.Event.EndedAt
	if ended.IsZero() {
		ended = w.now()
	}
	started := t.Event.StartedAt
	if started.IsZero() {
		started = ended.Add(-time.Duration(t.Event.DurationMS) * time.Millisecond)
	}
	return Episode{
		ID:     w.newID(),
		Handle: t.Event.AgentHandle,
		Role:   t.Event.RoleName,
		TaskID: t.Event.TaskID,
		// TurnID and WorkKey carry the SAME value on a raw row, because
		// this engine mints one identity per unit of work and calls it
		// the turn id (see turnctx.Turn.ID). They are separate columns
		// because a COMPACTED row has a work key derived from the
		// episodes it folded and no turn of its own.
		TurnID:          t.Event.TurnID,
		WorkKey:         t.Event.TurnID,
		StartedAt:       started.UTC(),
		EndedAt:         ended.UTC(),
		Duration:        time.Duration(t.Event.DurationMS) * time.Millisecond,
		PlanSummary:     t.Event.PlanSummary,
		TaskSummary:     t.Event.TaskSummary,
		ToolSequence:    t.Event.ToolSequence,
		SkillsUsed:      t.Event.SkillsUsed,
		ReviewOutcome:   t.Event.ReviewOutcome,
		ConversationKey: t.Event.ConversationKey,
		Kind:            KindRaw,
		Count:           1,
	}
}

// vector embeds the task summary, or reports none.
//
// NEVER an error: see the type comment. The failure is logged where it
// happens and the row is written without a vector.
func (w *Episodist) vector(ctx context.Context, summary string) []float32 {
	if w.embed == nil || summary == "" {
		return nil
	}
	summary = clip(summary, episodeEmbedInput)
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	vector, err := w.embed(ctx, summary)
	if err != nil {
		log.WarnContext(ctx, "episode_embedding_failed", "error", err.Error(),
			"detail", "the episode is written without a vector; recall skips "+
				"it while the time-window queries still surface it")
		return nil
	}
	return vector
}
