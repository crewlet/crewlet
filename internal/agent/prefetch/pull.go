package prefetch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
)

// The PULL side of the same two searches the turn-start prefetch pushes.
//
// Both blocks are the prefetch's own, re-run on demand (`## Similar prior work`
// and `## Personal memory`); a second implementation would drift from the
// block shown at turn start.
//
// # Why a pull exists at all
//
// The push runs once against the trigger, and a thin one ("PR #42 got a
// comment") is a pointer: similarity against it returns recent work, so both
// blocks render a hint telling the executor to look again after recon. These
// methods are what it looks with.

// RecallEpisodes returns the seat's past turns most similar to text.
//
// No fallback to recency, as in the block: recent turns read as precedent.
//
// Three answers, never two: a search (no hits means nothing searched
// resembles this), [ErrNoSimilarity] for a company with no embeddings, and an
// error wrapping [ErrSimilarityFailed] for a search that could not run (the
// embedder refused or missed [EmbedBudget], or the store is unreadable);
// conflating those sent a model away from a search that would answer next call.
// It also counts turns with no vector of the query's model
// ([learning.EpisodeSearch.Unsearched]): "nothing similar" over an unsearched
// history said its work was new.
func (f *Fetcher) RecallEpisodes(ctx context.Context, seat *org.Role, text string, limit int) (learning.EpisodeSearch, error) {
	if f == nil || f.src.Episodes == nil || seat == nil || seat.Handle() == "" {
		return learning.EpisodeSearch{}, fmt.Errorf("%w: this node holds no episode store for the seat", ErrSimilarityFailed)
	}
	handle := seat.Handle()
	if strings.TrimSpace(text) == "" {
		return learning.EpisodeSearch{}, nil
	}
	embedCtx, cancel := context.WithTimeout(ctx, EmbedBudget)
	vector, err := f.embed(embedCtx, text)
	cancel()
	if errors.Is(err, learning.ErrNoEmbeddings) {
		return learning.EpisodeSearch{}, ErrNoSimilarity
	}
	if err != nil {
		return learning.EpisodeSearch{}, fmt.Errorf("%w: embedding the query: %w", ErrSimilarityFailed, err)
	}
	hits, err := f.src.Episodes.Recall(ctx, learning.RecallQuery{
		Handle: handle, Embedding: vector.Values, Model: vector.Model, Limit: limit,
	})
	if err != nil {
		return learning.EpisodeSearch{}, fmt.Errorf("%w: reading %s's episodes: %w", ErrSimilarityFailed, handle, err)
	}
	unsearched, err := f.src.Episodes.Unsearchable(ctx, handle, vector.Model)
	if err != nil {
		return learning.EpisodeSearch{}, fmt.Errorf("%w: counting %s's episodes: %w", ErrSimilarityFailed, handle, err)
	}
	return learning.EpisodeSearch{Hits: hits, Unsearched: unsearched}, nil
}

// RecallMemories re-runs the personal-memory filter against a hint.
//
// The same pool, filter and refusal to fall back as the block: a failed filter
// gives empty, since "the most recent eight" would leak a memory about one
// person into another's turn. With the turn's senders, which the per-subject
// rule cannot judge "party to the task" without.
//
// With the turn's attribution (aux): the call is the turn's cost, filed under
// the turn and its tally like the turn-start filter's; unattributed, the seam
// refuses it and the tool recalls nothing.
func (f *Fetcher) RecallMemories(ctx context.Context, seat *org.Role, agentID, hint string,
	senders []learning.Subject, aux auxspend.Use,
) ([]learning.DiaryEntry, error) {
	if f == nil || f.src.Diary == nil || seat == nil || agentID == "" {
		return nil, nil
	}
	if strings.TrimSpace(hint) == "" {
		return nil, nil
	}
	// THE HINT IS THE ASK: it is the executor's own account of what the
	// task is about, written after recon, and the whole of what the filter
	// and the vector are judged against here.
	request := Request{Seat: seat, AgentID: agentID, Task: hint, Ask: PlainAsk(hint),
		Senders: senders, Aux: aux}
	if f.src.AccessTold != nil {
		// THE SAME HOLD AS THE TURN'S START: when the seat was last told its access.
		request.AccessChangedAt = f.src.AccessTold(ctx, seat.Handle())
	}
	candidates := f.memoryCandidates(ctx, request, f.vectorFor(ctx, request, nil))
	if len(candidates) == 0 {
		return nil, nil
	}
	return f.filterMemories(ctx, request, candidates), nil
}

// ErrNoSimilarity reports that this company configured no embeddings, so a
// similarity search cannot run at all. It wraps [learning.ErrNoEmbeddings],
// which is what a caller that does not import this package tests for.
//
// Its own error rather than an empty result, because the two send a model to
// opposite places: "nothing resembles this" is an answer it should act on, and
// "this company cannot search by meaning" is a reason to fall back to a
// conversation filter it can still use.
var ErrNoSimilarity = fmt.Errorf("prefetch: a similarity search cannot run: %w", learning.ErrNoEmbeddings)

// ErrSimilarityFailed reports a similarity search that could have run and did
// not: the embedder refused or did not answer in time, or the store could not
// be read. Distinct from [ErrNoSimilarity] because it is not how the company
// is set up. Whether the same call may answer later is the CAUSE's to say,
// and it is wrapped beside this one: an embedder's failure keeps its class
// (embeddings.ErrConfiguration, embeddings.ErrRefused,
// embeddings.ErrTransient) through the wrap, and only a transient one or a
// deadline is worth asking again.
var ErrSimilarityFailed = errors.New("prefetch: the similarity search could not run")
