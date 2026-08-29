package builtin

import (
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/learning"
)

// hintLedger remembers which context hints a turn has already filtered on.
//
// # Why it lives on the tool
//
// A [turnctx.Turn] is IMMUTABLE and passed rather than stored — that is what
// makes every authorization decision downstream a fact rather than a
// suggestion — so there is nowhere on it to keep per-turn scratch. The tool
// instance lives in the epoch's registry and is shared by every seat, so this
// is keyed by turn id and bounded: entries fall out as turns rotate rather
// than being cleaned up by a hook the turn loop does not have.
//
// # Why an idempotency cache at all
//
// The filter is an auxiliary model call. A model that re-hints on every round
// spends a completion per round for answers that converge after the second,
// which is what the cap stops — but a model that repeats a hint it has already
// used is asking a question this ledger already has the answer to, and
// charging it against the cap would teach it to vary its wording instead.
//
// So a repeat is free BECAUSE IT IS ANSWERED FROM HERE, not merely uncharged.
// Re-running the filter for free would leave the cap bounding nothing: a model
// that alternated two hints could spend completions for the rest of the turn
// without ever taking a third slot.
//
// The answer it hands back is the one computed earlier in the SAME turn, which
// is the whole window a hint's answer has to be stable over. A note the seat
// persists mid-turn is reached by asking a new question, not by asking the old
// one again.
//
// What it caches is the FILTERED ROWS, not the rendered text: the auxiliary
// call is the expensive half and `limit` only decides how many of its rows are
// printed. Caching the text would answer a repeat asking for ten notes with
// the three the first call rendered — and keying the cap on the limit instead
// would let a model spend a completion per integer.
//
// Hints are compared with case and surrounding whitespace normalised, because
// "the search indexing bug" and "The search indexing bug " are the same
// question and a model does not spell consistently.
type hintLedger struct {
	mu    sync.Mutex
	turns map[string]map[string]hintAnswer
	order []string
}

// hintAnswer is one hint's filtered rows, once the filter has produced them.
//
// Ready distinguishes "the filter found nothing" from "the slot is reserved
// and nothing has been kept against it" — the first is an answer a repeat
// should be given, the second is a call still to make.
type hintAnswer struct {
	ready   bool
	entries []learning.DiaryEntry
}

// hintTake is what one attempt at a hint resolves to.
//
// FOUR ANSWERS, not a bool: served from the cache, allowed to run, refused by
// the cap — and the spent count the refusal has to quote, so the model learns
// the shape of the limit rather than that the tool became unreliable.
type hintTake struct {
	// Cached is the rows a previous call with this hint filtered to, and
	// Hit says whether there was such a call. Separate, because a filter
	// that found nothing legitimately answers with no rows, and an empty
	// slice would otherwise read as a miss.
	Cached []learning.DiaryEntry
	Hit    bool

	// Spent is how many DISTINCT hints the turn has used.
	Spent int

	// Allowed is false only when the cap refuses a NEW hint.
	Allowed bool
}

// HintLedgerTurns bounds how many turns are remembered.
//
// A turn's entries are dead the moment it ends and nothing tells this when
// that happened, so the bound is what makes the map a cache rather than a
// leak. Two hundred and fifty-six is far more than a node runs concurrently —
// the eviction is for turns that have long finished, not for pressure.
const HintLedgerTurns = 256

// DefaultRefreshesPerTurn is learning.personal_memory.max_refreshes_per_turn's
// shipped value, for a registry built without a company.
const DefaultRefreshesPerTurn = 3

// take resolves one attempt at a hint: cached, allowed, or refused.
//
// A turn with no id — a tool surface built outside a turn — is not tracked and
// never refused: there is no turn for a per-turn cap to bound.
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
		// A slot taken but not yet answered (the filter failed, and
		// nothing was kept) reads as allowed rather than a hit: handing
		// back an empty digest as "your notes" would be a worse answer
		// than paying for the call again.
		return hintTake{Cached: answer.entries, Hit: answer.ready, Spent: len(spent), Allowed: true}
	}
	if len(spent) >= budget {
		return hintTake{Spent: len(spent), Allowed: false}
	}
	// RESERVED BEFORE THE CALL, so a filter that fails still costs its
	// slot. Otherwise a hint whose call errors is retryable without bound,
	// which is the same unbounded spend the cap exists to stop.
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
		// The turn fell out of the bound between take and keep. Nothing
		// to attach the answer to, and re-creating the entry would put a
		// turn back that eviction has already decided is over.
		return
	}
	spent[normalizeHint(hint)] = hintAnswer{ready: true, entries: entries}
}

// normalizeHint is the comparison key: case- and whitespace-insensitive.
func normalizeHint(hint string) string {
	return strings.ToLower(strings.Join(strings.Fields(hint), " "))
}
