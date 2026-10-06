package search

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
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
// by its source and the digest of the exact text refused, and the selection
// PASSES OVER it ([Held]): it costs no request and no place in the selection,
// whose limit counts only what a tick may send. What it still costs is the
// scan stepping over its key each tick, one read of its row whenever it
// changes without its text changing, and the coverage gauge, which counts it
// stale for as long as it is refused — the one symptom it leaves.
//
// After [EmbedRefusalRetry] it is due, and offered again ALONE, in a request
// of its own — at most [EmbedRetriesPerTick] of one corpus's a tick, the
// longest refused first, so refusals falling due together take that many
// requests a tick rather than the corpus's whole share. Each retry costs one
// request and one log line naming it, the model, the bytes it was sent and the
// per-input bound the model's limits assume, which is the bound a misdeclared
// `max_input_tokens` would have got wrong. A changed text is a new digest and
// is offered at once, with its neighbours. A provider CONFIGURED differently
// starts with no memory, because a refusal is a fact about the provider as it
// was configured; one built again for the same configuration — an apply that
// changed something else, a rotated key — keeps it.

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
// ONE ENTRY A SOURCE, for the text it was refused as: a source has one text at
// a time, so an entry for what it used to say is replaced, never kept beside
// it. An entry is gone when its text is embedded (a retry accepted), when the
// text moves (the selection returns the source under another digest), and when
// a retry is offered and the selection no longer returns the source at all —
// it was removed, or has a vector by now — so the memory holds at most one
// entry per source the provider is still refusing, with no clock of its own
// deciding what to forget.
//
// THIS NODE'S ALONE, and in memory: a cache of what one provider told one duty
// holder, whose loss costs one more isolation of each input it held — fifteen
// requests at most — on whichever node holds the duty next. Nothing about it is
// a fact the fleet must agree on, so nothing about it is published.
//
// Safe for concurrent use; the duty's ticks run one at a time anyway.
type Refusals struct {
	mu sync.Mutex
	at map[refusalID]refusal
}

// refusalID is the source one refusal is about.
type refusalID struct {
	source Source
	id     string
}

// refusal is one input refused alone: the digest of the exact text refused,
// the source's version and title when it was last seen with that text — which
// is what lets a selection recognise it before reading its body ([Held]) — and
// when the provider last refused it.
type refusal struct {
	sha     string
	version uint64
	title   string
	at      time.Time
}

// NewRefusals is an empty memory, for one provider configuration.
func NewRefusals() *Refusals { return &Refusals{at: map[refusalID]refusal{}} }

// refusalPlan is what the memory says about one corpus for one tick: every
// refusal it holds of that corpus, which of them the selection passes over,
// and which are offered again this tick.
type refusalPlan struct {
	// entries is every refusal of the corpus, by source id, as the tick
	// began.
	entries map[string]refusal

	// held is the ones the selection passes over: every entry but the
	// retries.
	held Held

	// retry is the due ones offered again this tick — at most
	// [EmbedRetriesPerTick], the longest refused first.
	retry map[string]bool
}

// plan is what the memory says about source's refusals at now.
//
// THE RETRIES ARE CHOSEN HERE, BEFORE THE SELECTION, because the selection
// passes over everything else: a due refusal not offered this tick is held one
// more tick rather than read, so a thousand refusals falling due together
// cannot fill the selection — which is the failure the pass-over exists to
// remove — and cannot take the corpus's requests either, at one each.
func (r *Refusals) plan(source Source, now time.Time) refusalPlan {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := refusalPlan{
		entries: map[string]refusal{},
		held:    Held{byID: map[string]heldAs{}},
		retry:   map[string]bool{},
	}
	var due []string
	for k, entry := range r.at {
		if k.source != source {
			continue
		}
		out.entries[k.id] = entry
		if now.Sub(entry.at) >= EmbedRefusalRetry {
			due = append(due, k.id)
		}
	}
	// THE LONGEST REFUSED FIRST, and the id after it so the order is total:
	// an input offered and refused again goes to the back, so every due
	// refusal is offered in turn however many there are.
	slices.SortFunc(due, func(a, b string) int {
		if c := out.entries[a].at.Compare(out.entries[b].at); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	for _, id := range due[:min(len(due), EmbedRetriesPerTick)] {
		out.retry[id] = true
	}
	for id, entry := range out.entries {
		if !out.retry[id] {
			out.held.byID[id] = heldAs{version: entry.version, title: entry.title}
		}
	}
	return out
}

// refuse remembers p's text as refused alone at now.
func (r *Refusals) refuse(source Source, p pending, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.at[refusalID{source, p.doc.ID}] = refusal{
		sha: p.sha, version: p.doc.Version, title: p.doc.Title, at: now,
	}
}

// restate records that a held source was seen at another version or title
// with the text it was refused as — a status, a move, an assignee — so the
// next selection passes over it again rather than reading it once more.
func (r *Refusals) restate(source Source, doc Document) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := refusalID{source, doc.ID}
	if entry, ok := r.at[k]; ok {
		entry.version, entry.title = doc.Version, doc.Title
		r.at[k] = entry
	}
}

// forget drops the refusal of one source: its text was embedded after all, it
// moved, or the source is no longer selected.
func (r *Refusals) forget(source Source, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.at, refusalID{source, id})
}

// Len is how many refusals are remembered.
func (r *Refusals) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.at)
}

// Held is what one corpus's selection passes over: the sources whose text the
// provider refused alone and that this tick does not offer again, recognised
// by their version and title BEFORE their bodies are read ([Corpus.Stale]).
//
// # Why a selection passes over them, and is not merely filtered after
//
// The selection is oldest first and a refused source is never embedded, so it
// keeps its place at the front of every selection after it. Filtered after the
// selection's limit, every held source took a slot it would never use: with H
// of them a corpus embedded at most [EmbedSourcesPerTick] − H sources a tick,
// and once H reached the limit, nothing written to that corpus was selected
// again at all. Passed over inside the selection, the limit counts only what
// the tick may send.
//
// # Why by version and title, and not by the digest
//
// The digest is of the prepared opening, which needs the body — the read the
// pass-over exists to spare a source that will not be sent. A source's version
// moves whenever its body does ([Document.Version]), and its title is read with
// the key, so the two together say the text cannot have changed since it was
// refused; a source whose version or title moved is read, and its digest
// decides — a new text is offered at once, and an unchanged one (a status, a
// move) is held again under its new version ([Refusals.restate]), having cost
// one read.
//
// THE ZERO VALUE PASSES OVER NOTHING, which is what a caller with no refusals
// hands a corpus.
type Held struct {
	byID map[string]heldAs
}

// heldAs is the version and title one held source was refused at.
type heldAs struct {
	version uint64
	title   string
}

// Len is how many sources h can pass over — each names one source, so a
// selection that reads this many rows beyond its limit returns its limit
// whenever the corpus has that many to send.
func (h Held) Len() int { return len(h.byID) }

// Holds reports whether doc — its identity, version and title; a corpus asks
// before it reads the body — is passed over.
func (h Held) Holds(doc Document) bool {
	at, ok := h.byID[doc.ID]
	return ok && at.version == doc.Version && at.title == doc.Title
}
