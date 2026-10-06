package search

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// A tick's provider REQUESTS: how a corpus's stale sources become requests the
// model accepts, and what becomes of one it refuses.
//
// # The duty forms every request itself
//
// The provider will pack a call into as many requests as the model's limits
// need, but a call that fails fails whole, and a caller handed one error for a
// hundred and twenty-eight inputs cannot tell the one the model will never
// accept from the rest. So the duty plans each request through the provider's
// own packing rule ([embeddings.Limits.Requests] — the one rule, never a second
// idea of it) and sends each through a call of its own: every request it
// forms is inside the model's input count and request total by construction,
// and a request that is refused all the same is ONE request whose inputs it
// knows.
//
// # A refused request is split until the input it refuses is alone
//
// A refusal ([embeddings.ErrRefused]) says the REQUEST is unacceptable and not
// which part of it, and only a smaller request can tell: a request of n inputs
// is split into halves, sent on the corpus's next turns before anything else it
// holds, and so on down to the one input still refused alone — at most
// 1 + 2·log₂n requests, fifteen for a full request of 128, which fits the
// share of a tick's requests each of two corpora is guaranteed. Every half that
// is accepted publishes its vectors as it goes, so a refused input costs its
// own vector and never its neighbours'. A refusal that was really the request's
// size or a setting the server rejects whatever the inputs are surfaces the
// same way and is told apart by the same means: halves that succeed blame no
// input at all, and halves that all fail end in a log line naming every input
// and, once a tick sees nothing but refusals, one line saying so.
//
// # And an input refused alone is held back, then offered again alone
//
// The selection is oldest first and an input nobody embeds keeps its place, so
// without a memory a refused input would be at the front of every later tick,
// isolated again at fifteen requests a tick for ever. [Refusals] remembers it,
// by its source and the digest of the exact text refused, and the next ticks
// skip it — so it costs NO request, and what a stale source costs the coverage
// gauge is the one symptom it leaves. After [EmbedRefusalRetry] it is offered
// again ALONE, in a request of its own: one request an hour, and one log line
// naming it, the model, the bytes it was sent and the per-input bound the
// model's limits assume, which is the bound a misdeclared `max_input_tokens`
// would have got wrong. A changed text is a new digest and is offered at once,
// with its neighbours; a provider rebuilt by a config apply starts with no
// memory, because a refusal is a fact about the provider as it was configured.

// pending is one source as a tick holds it: the document, the exact text that
// would be sent for it — its prepared opening ([Document.text]) — and that
// text's digest, which the vector record carries.
type pending struct {
	doc  Document
	text string
	sha  string

	// alone says the provider refused this exact text alone before, and
	// its retry is due: it is offered in a request of its own.
	alone bool
}

// pendingOf is a document as a tick at bound holds it.
func pendingOf(doc Document, bound int) pending {
	text := doc.text(bound)
	return pending{doc: doc, text: text, sha: digestText(text)}
}

// digestText is sha256 over text, hex — the digest a vector record carries,
// taken over [Document.text], which is byte for byte what the provider
// received.
//
// OF THE OPENING, not of the source, and that is what lets it answer the
// question it exists for: a source rewritten into the same words — a re-file,
// a label, a parent move — produces the same digest. A digest of the whole
// body would differ whenever anything past the window moved, which is text the
// provider never saw.
func digestText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// corpusQueue is one corpus's share of a tick: what it has left to do.
type corpusQueue struct {
	source Source

	// restamps are the sources whose stored vector is already their text's
	// vector, oldest first: republished under their current version and
	// container, never sent ([Embedder.restamp]).
	restamps []pending

	// split are the halves of the corpus's refused requests, sent before
	// anything else it holds, the first half first.
	split [][]pending

	// waiting is everything else the selection returned, oldest first.
	waiting []pending
}

// next is the corpus's next request — the longest run of its oldest sources
// that fits one request inside limits and room — or nil when it has none.
//
// A SOURCE WITH NOTHING TO EMBED is passed over without a request, and is
// selected again next tick, as it always was: an input with no text is never
// sent ([embeddings.Limits.Requests] files it in no request). A source offered
// ALONE ends the run before it and is a request by itself.
func (q *corpusQueue) next(limits embeddings.Limits, room int) ([]pending, error) {
	if room <= 0 {
		return nil, nil
	}
	if len(q.split) > 0 {
		group := q.split[0]
		q.split = q.split[1:]
		// A HALF LARGER THAN THE ROOM LEFT is sent at the room's size:
		// what does not fit is selected again next tick.
		return group[:min(len(group), room)], nil
	}
	for len(q.waiting) > 0 && q.waiting[0].text == "" {
		q.waiting = q.waiting[1:]
	}
	if len(q.waiting) == 0 {
		return nil, nil
	}
	if q.waiting[0].alone {
		group := q.waiting[:1]
		q.waiting = q.waiting[1:]
		return group, nil
	}
	run := q.waiting[:min(len(q.waiting), room)]
	for i, p := range run {
		if p.alone {
			run = run[:i]
			break
		}
	}
	texts := make([]string, len(run))
	for i, p := range run {
		texts[i] = p.text
	}
	groups, err := limits.Requests(texts)
	if err != nil {
		return nil, err
	}
	// run[0] has text, so the first request holds at least it.
	first := groups[0]
	group := make([]pending, len(first))
	for i, at := range first {
		group[i] = run[at]
	}
	q.waiting = q.waiting[first[len(first)-1]+1:]
	return group, nil
}

// halves splits a refused request in two, the first half the larger.
func halves(group []pending) ([]pending, []pending) {
	mid := (len(group) + 1) / 2
	return group[:mid], group[mid:]
}

// Refusals is what a provider has refused ALONE, held across the ticks of the
// duty that met it — see the section above [pending] for what it buys.
//
// THIS NODE'S ALONE, and in memory: a cache of what one provider told one duty
// holder, whose loss costs one more isolation of each input it held — fifteen
// requests at most — on whichever node holds the duty next. Nothing about it is
// a fact the fleet must agree on, so nothing about it is published.
//
// Safe for concurrent use; the duty's ticks run one at a time anyway.
type Refusals struct {
	mu sync.Mutex
	at map[refusalKey]time.Time
}

// refusalKey is what one refusal is remembered by: the source, and the digest
// of the exact text that was refused — so a source whose text changed is a
// different input and is offered at once.
type refusalKey struct {
	source Source
	id     string
	sha    string
}

// refusalStanding is what the memory says about one source's text now.
type refusalStanding int

const (
	// refusalNone is a text never refused alone: sent with its neighbours.
	refusalNone refusalStanding = iota

	// refusalHeld is a text refused alone inside the last
	// [EmbedRefusalRetry]: not sent.
	refusalHeld

	// refusalDue is a text refused alone longer ago than that: offered
	// again, alone.
	refusalDue
)

// NewRefusals is an empty memory, for one provider.
func NewRefusals() *Refusals { return &Refusals{at: map[refusalKey]time.Time{}} }

// standing answers what the memory says about k at now.
func (r *Refusals) standing(k refusalKey, now time.Time) refusalStanding {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, held := r.at[k]
	switch {
	case !held:
		return refusalNone
	case now.Sub(at) < EmbedRefusalRetry:
		return refusalHeld
	}
	return refusalDue
}

// refuse remembers k as refused alone at now.
func (r *Refusals) refuse(k refusalKey, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.at[k] = now
}

// accept forgets k: the provider embedded it after all.
func (r *Refusals) accept(k refusalKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.at, k)
}

// expire forgets every refusal older than twice [EmbedRefusalRetry]: one that
// was due an hour ago and was offered nowhere since is a text no selection
// returns any more — it was edited, so it carries another digest, or its
// source is gone — and keeping it would grow the memory for the life of the
// process.
func (r *Refusals) expire(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, at := range r.at {
		if now.Sub(at) >= 2*EmbedRefusalRetry {
			delete(r.at, k)
		}
	}
}

// Len is how many refusals are remembered.
func (r *Refusals) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.at)
}

// keyOf is the refusal key of one pending source.
func keyOf(source Source, p pending) refusalKey {
	return refusalKey{source: source, id: p.doc.ID, sha: p.sha}
}
