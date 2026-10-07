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
// Both blocks this file exposes are the prefetch's own, re-run on demand: the
// vector recall behind `## Similar prior work`, and the auxiliary relevance
// filter behind `## Personal memory`. They are here rather than
// reimplemented in a builtin because a second implementation of "which of this
// seat's memories bear on this text" is a second answer to it, and the two
// would drift in exactly the direction nobody looks — the tool would quietly
// stop matching the block the model was shown at turn start.
//
// # Why a pull exists at all
//
// The push happens once, against the TRIGGER, and a thin trigger — "PR #42 got
// a comment" — is a pointer with no content: a similarity search against it
// returns the seat's most recent work rather than its most relevant, so both
// blocks deliberately render a hint instead. The hint tells the executor to
// look again once recon has made the task real. Without these two methods
// there was nothing for it to look with: `query_episodes` read recency and
// `refresh_memory` dumped the newest notes, so the documented escape hatch for
// the exact case the gate exists for was advertised and absent.

// RecallEpisodes returns the seat's past turns most similar to text.
//
// NO FALLBACK TO RECENCY, matching the block: episode recall's whole claim is
// "this resembles what you are doing now", the three most recent turns carry
// no such claim, and an executor told they are similar work treats them as
// precedent.
//
// THREE ANSWERS, never two: a search (no hits is "nothing that could be
// searched resembles this"), [ErrNoSimilarity] for a company with no
// embeddings, and an error wrapping [ErrSimilarityFailed] for a search that
// could not run — an embedder that refused or did not answer inside
// [EmbedBudget], or an episode store that could not be read. The last two used
// to be one: every embed failure answered "no embeddings are configured",
// which sent a model away from a search that would have answered on the next
// call.
//
// AND WHAT IT COULD NOT SEARCH: the search counts the seat's turns with no
// vector of the query's model ([learning.EpisodeSearch.Unsearched]) — after a
// model change, until the holder's fill has reached them, that is most of a
// seat's history — because "nothing similar" over a history nobody searched
// told a seat its work was new.
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
// The SAME candidate pool and the SAME filter as the block: similarity union
// recency, judged by the auxiliary model. And the same refusal to fall back —
// when the filter is unavailable or its answer is unparseable the result is
// empty, because "the most recent eight" would leak a memory about one person
// into a turn about another, which is the failure the filter exists to
// prevent.
//
// WITH THE TURN'S SENDERS, which the block's own request carries: the filter's
// per-subject rule — a preference about somebody not party to the task does
// not apply — has nothing to judge "party to the task" by without them, and a
// re-filter built without them was a filter that could not tell the person
// asking from anybody else.
//
// AND WITH THE TURN'S ATTRIBUTION (aux): the filter's call is the turn's own
// cost, spent mid-turn on its behalf, so it is filed under the turn and its
// tally like the turn-start filter's — a re-filter attributed to nothing would
// be refused by the seam, and the tool would find nothing it could recall.
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
	request := Request{Seat: seat, AgentID: agentID, Task: hint, Ask: hint, Senders: senders,
		Aux: aux}
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
// is set up — the same call may answer a moment later.
var ErrSimilarityFailed = errors.New("prefetch: the similarity search could not run")
