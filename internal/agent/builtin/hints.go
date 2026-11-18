package builtin

import (
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/learning"
)

// hintLedger remembers, per turn, which context hints the auxiliary filter
// call has answered and with what. A [turnctx.Turn] is immutable and the tool
// is shared by every seat, so it is keyed by turn id and bounded by
// HintLedgerTurns: the turn loop has no end-of-turn hook to clean it up.
//
// A repeated hint is answered from here, not re-run: charging it would teach
// the model to vary its wording, and a free re-run leaves the cap bounding
// nothing (alternating two hints spends forever). It caches the filtered
// rows, not the rendered text, so a repeat with a larger `limit` gets them
// all and the cap is not keyed per limit; a note persisted mid-turn is
// reached by a new hint, not a repeat. Hints compare case- and
// whitespace-normalised, since a model does not spell consistently.
type hintLedger struct {
	mu    sync.Mutex
	turns map[string]map[string]hintAnswer
	order []string
}

// hintAnswer is one hint's filtered rows. ready separates "the filter found
// nothing" (an answer for a repeat) from "slot reserved, nothing kept yet"
// (a call still to make).
type hintAnswer struct {
	ready   bool
	entries []learning.DiaryEntry
}

// hintTake is what one attempt at a hint resolves to: served from the cache,
// allowed to run, or refused by the cap, with the spent count a refusal
// quotes so the model learns the limit rather than that the tool is flaky.
type hintTake struct {
	// Cached is the rows an earlier call with this hint filtered to; Hit
	// says whether there was one, since a filter that found nothing
	// answers with no rows.
	Cached []learning.DiaryEntry
	Hit    bool

	// Spent is how many DISTINCT hints the turn has used.
	Spent int

	// Allowed is false only when the cap refuses a NEW hint.
	Allowed bool
}

// HintLedgerTurns bounds how many turns are remembered: nothing says when a
// turn ends, so the bound makes the map a cache rather than a leak. 256 is far
// more than a node runs at once, so eviction only drops finished turns.
const HintLedgerTurns = 256

// DefaultRefreshesPerTurn is learning.personal_memory.max_refreshes_per_turn's
// shipped value, for a registry built without a company.
const DefaultRefreshesPerTurn = 3

// take resolves one attempt at a hint: cached, allowed, or refused. A turn
// with no id (a surface built outside a turn) is never tracked or refused.
func (l *hintLedger) take(turnID, hint string, budget int) hintTake {
	if turnID == "" || budget <= 0 {
		return hintTake{Allowed: true}
	}
	key := normalizeHint(hint)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.turns == nil {
		l.turns = map[string]map[string]hintAnswer{}
	}
	spent, seen := l.turns[turnID]
	if !seen {
		spent = map[string]hintAnswer{}
		l.turns[turnID] = spent
		l.order = append(l.order, turnID)
		for len(l.order) > HintLedgerTurns {
			delete(l.turns, l.order[0])
			l.order = l.order[1:]
		}
	}
	if answer, ok := spent[key]; ok {
		// A slot taken but never answered (the filter failed) is allowed,
		// not a hit: an empty digest handed back as "your notes" is worse
		// than paying for the call again.
		return hintTake{Cached: answer.entries, Hit: answer.ready, Spent: len(spent), Allowed: true}
	}
	if len(spent) >= budget {
		return hintTake{Spent: len(spent), Allowed: false}
	}
	// Reserved before the call, so a failing filter still costs its slot;
	// otherwise an erroring hint is retryable without bound.
	spent[key] = hintAnswer{}
	return hintTake{Spent: len(spent), Allowed: true}
}

// keep stores what a hint filtered to, so a repeat is answered from here.
func (l *hintLedger) keep(turnID, hint string, entries []learning.DiaryEntry) {
	if turnID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	spent, seen := l.turns[turnID]
	if !seen {
		// Evicted between take and keep: re-creating it would revive a
		// turn eviction has decided is over.
		return
	}
	spent[normalizeHint(hint)] = hintAnswer{ready: true, entries: entries}
}

// normalizeHint is the comparison key: case- and whitespace-insensitive.
func normalizeHint(hint string) string {
	return strings.ToLower(strings.Join(strings.Fields(hint), " "))
}
