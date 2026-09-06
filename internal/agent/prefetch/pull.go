package prefetch

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
)

// The PULL side of the same two searches the turn-start prefetch pushes.
//
// Both blocks this file exposes are the prefetch's own, re-run on demand: the
// vector recall behind `## Relevant prior work`, and the auxiliary relevance
// filter behind `## What you have learned`. They are here rather than
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
// blocks deliberately render a hint instead. The hint tells the planner to
// look again once recon has made the task real. Without these two methods
// there was nothing for it to look with: `query_episodes` read recency and
// `refresh_memory` dumped the newest notes, so the documented escape hatch for
// the exact case the gate exists for was advertised and absent.

// RecallEpisodes returns the seat's past turns most similar to text.
//
// NO FALLBACK TO RECENCY, matching the block: episode recall's whole claim is
// "this resembles what you are doing now", the three most recent turns carry
// no such claim, and a planner told they are similar work treats them as
// precedent. A company with no embeddings gets (nil, nil) and the caller says
// so — which is a different sentence from "you have done nothing like this".
func (f *Fetcher) RecallEpisodes(ctx context.Context, seat *org.Role, text string, limit int) ([]learning.Hit, error) {
	if f == nil || f.src.Episodes == nil || seat == nil {
		return nil, nil
	}
	handle := seat.Handle()
	if handle == "" || strings.TrimSpace(text) == "" {
		return nil, nil
	}
	vector, ok := f.embed(ctx, text)
	if !ok {
		return nil, ErrNoSimilarity
	}
	hits, err := f.src.Episodes.Recall(ctx, learning.RecallQuery{
		Handle: handle, Embedding: vector, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("prefetch: recall episodes for %s: %w", handle, err)
	}
	return hits, nil
}

// RecallMemories re-runs the personal-memory filter against a hint.
//
// The SAME candidate pool and the SAME filter as the block: similarity union
// recency, judged by the auxiliary model. And the same refusal to fall back —
// when the filter is unavailable or its answer is unparseable the result is
// empty, because "the most recent eight" would leak a memory about one person
// into a turn about another, which is the failure the filter exists to
// prevent.
func (f *Fetcher) RecallMemories(ctx context.Context, seat *org.Role, agentID, hint string) ([]learning.DiaryEntry, error) {
	if f == nil || f.src.Diary == nil || seat == nil || agentID == "" {
		return nil, nil
	}
	if strings.TrimSpace(hint) == "" {
		return nil, nil
	}
	request := Request{Seat: seat, AgentID: agentID, Task: hint}
	candidates := f.memoryCandidates(ctx, request)
	if len(candidates) == 0 {
		return nil, nil
	}
	return f.filterMemories(ctx, request, candidates), nil
}

// ErrNoSimilarity reports that this company configured no embeddings, so a
// similarity search cannot run at all.
//
// Its own error rather than an empty result, because the two send a model to
// opposite places: "nothing resembles this" is an answer it should act on, and
// "this deployment cannot search by meaning" is a reason to fall back to a
// conversation filter it can still use.
var ErrNoSimilarity = fmt.Errorf("prefetch: no embeddings are configured, so " +
	"a similarity search cannot run")
