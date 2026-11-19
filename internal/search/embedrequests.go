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
// The duty plans each request through [embeddings.Limits.Requests], the one
// packing rule, and sends each in its own call, so a refused request is ONE
// request whose inputs it knows. A refusal ([embeddings.ErrRefused]) does not
// say which input: the request is split into halves, sent first on the corpus's
// next turns, down to the input refused alone (1 + 2·log₂n requests at most).
// The isolation outlives its tick: unreached halves are kept ([Refusals.suspend],
// [corpusQueue.unanswered], [corpusQueue.resume]) and each corpus reserves its
// share of the tick's sources ([tickRequests.room]). A refusal of the request's
// size or a rejected setting ends in a log line naming every input ([Embedder.Tick]).

// An input refused alone is held, not retried every tick: left in the
// oldest-first selection it would be isolated again at fifteen requests a tick
// for ever. [Refusals] keys it by source and the digest of the text refused and
// the selection passes over it ([Held]), costing no request and no place. After
// [EmbedRefusalRetry] it is offered again ALONE, at most [EmbedRetriesPerTick]
// a corpus a tick, longest refused first; a changed text is a new digest and
// goes at once. A provider configured differently starts with no memory (a
// refusal is a fact about its configuration); one rebuilt for the same
// configuration keeps it. A vector the duty will not publish ([pack]: not
// finite, or all zero) is held the same way, else it would be the oldest stale
// source of every tick.

// pending is one source as a tick holds it: the document, the exact text that
// would be sent for it — its prepared opening ([Document.text]) — and that
// text's digest, which the vector record carries.
type pending struct {
	doc  Document
	text string
	sha  string

	// alone says the provider refused this exact text alone before, or
	// answered it with a vector the duty would not publish, and its retry
	// is due: it is offered in a request of its own.
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

	// used is how many of the tick's sources the corpus has spent — a
	// record published for each, embedded or restamped — which is what its
	// reserved share is measured against ([tickRequests.room]).
	used int
}

// canWork reports whether q has anything left this tick may do: a restamp,
// which costs no request, or a request while the tick may still send one.
//
// Within a tick it never turns true again once false: restamps and waiting
// sources only shrink, a split only grows from a request the corpus sent, and
// a tick that may not send stays that way.
func (q *corpusQueue) canWork(t *tickRequests) bool {
	return len(q.restamps) > 0 || (t.open() && q.hasRequests())
}

// hasRequests reports whether q holds a source a request would carry.
func (q *corpusQueue) hasRequests() bool {
	for len(q.waiting) > 0 && q.waiting[0].text == "" {
		q.waiting = q.waiting[1:]
	}
	return len(q.split) > 0 || len(q.waiting) > 0
}

// resume puts back the isolation the corpus's last tick did not finish: each
// remembered group, rebuilt from the sources this tick's selection returned
// that still carry the text they were suspended with, sent ahead of anything
// else and in the order the last tick would have sent them.
//
// A member the selection did not return — embedded since, removed, rewritten,
// or now held — is simply not in the group; a group left empty is dropped.
func (q *corpusQueue) resume(groups [][]frontierKey) {
	if len(groups) == 0 {
		return
	}
	at := make(map[frontierKey]int, len(q.waiting))
	for i, p := range q.waiting {
		if !p.alone {
			at[frontierKey{id: p.doc.ID, sha: p.sha}] = i
		}
	}
	taken := map[int]bool{}
	var split [][]pending
	for _, keys := range groups {
		var group []pending
		for _, k := range keys {
			if i, ok := at[k]; ok && !taken[i] {
				taken[i] = true
				group = append(group, q.waiting[i])
			}
		}
		if len(group) > 0 {
			split = append(split, group)
		}
	}
	if len(split) == 0 {
		return
	}
	waiting := q.waiting[:0:0]
	for i, p := range q.waiting {
		if !taken[i] {
			waiting = append(waiting, p)
		}
	}
	q.split, q.waiting = append(split, q.split...), waiting
}

// next is the corpus's next request — the longest run of its oldest sources
// that fits one request inside limits and room — or nil when it has none.
// isolating says the request was taken from the corpus's isolation
// ([corpusQueue.split]), which is where it goes back to if the provider gives
// no answer about it ([corpusQueue.unanswered]).
//
// A SOURCE WITH NOTHING TO EMBED is passed over without a request, and is
// selected again next tick, as it always was: an input with no text is never
// sent ([embeddings.Limits.Requests] files it in no request). A source offered
// ALONE ends the run before it and is a request by itself.
func (q *corpusQueue) next(limits embeddings.Limits, room int) (group []pending, isolating bool, err error) {
	if room <= 0 {
		return nil, false, nil
	}
	if len(q.split) > 0 {
		group = q.split[0]
		q.split = q.split[1:]
		if len(group) > room {
			// A HALF LARGER THAN THE ROOM LEFT is sent at the room's
			// size, and what does not fit stays at the front of the
			// split — still a suspect, so it is part of the isolation
			// the next tick resumes rather than a source sent again
			// with neighbours it may be refused beside.
			q.split = append([][]pending{group[room:]}, q.split...)
			group = group[:room]
		}
		return group, true, nil
	}
	for len(q.waiting) > 0 && q.waiting[0].text == "" {
		q.waiting = q.waiting[1:]
	}
	if len(q.waiting) == 0 {
		return nil, false, nil
	}
	if q.waiting[0].alone {
		group = q.waiting[:1]
		q.waiting = q.waiting[1:]
		return group, false, nil
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
		return nil, false, err
	}
	// run[0] has text, so the first request holds at least it.
	first := groups[0]
	group = make([]pending, len(first))
	for i, at := range first {
		group[i] = run[at]
	}
	q.waiting = q.waiting[first[len(first)-1]+1:]
	return group, false, nil
}

// unanswered puts back a request the provider gave NO ANSWER ABOUT ITS INPUTS
// (a failure not about an input, or an answer that cannot be matched to them),
// which ends the tick's requests ([Embedder.request]).
//
// A request from the isolation goes back to its FRONT: nothing cleared its
// sources, so they stay suspects and the tick's end suspends them
// ([Refusals.suspend]). Dropped, they rejoined neighbours in a fresh request of
// 128, and a rate-limiting gateway cut every tick at the same depth.
//
// One from the selection needs nothing: its sources are selected again next
// tick, and a due retry keeps its refusal, still due.
func (q *corpusQueue) unanswered(group []pending, isolating bool) {
	if isolating {
		q.split = append([][]pending{group}, q.split...)
	}
}

// halves splits a refused request in two, the first half the larger.
func halves(group []pending) ([]pending, []pending) {
	mid := (len(group) + 1) / 2
	return group[:mid], group[mid:]
}

// Refusals is what a provider has refused ALONE, or answered with a vector the
// duty will not publish, held across the duty's ticks (see the section above
// [pending]), and the isolation each corpus's last tick did not finish
// ([Refusals.suspend]).
//
// One entry a source, for the text it was refused as; an entry goes when its
// text is embedded, when the text moves, or when a retry is offered and the
// selection no longer returns the source, so no clock decides what to forget.
//
// THIS NODE'S ALONE, in memory: losing it costs one more isolation (fifteen
// requests at most) per input, and no fact in it needs the fleet's agreement.
// Safe for concurrent use; the duty's ticks run one at a time anyway.
type Refusals struct {
	mu sync.Mutex
	at map[refusalID]refusal

	// frontier is each corpus's isolation still under way when its last
	// tick ended: the groups it split a refused request into and did not
	// reach, in the order it would have sent them ([Refusals.suspend]).
	frontier map[Source][][]frontierKey

	// embedded says the provider has accepted a request since this memory
	// began: the evidence that whatever it refuses is a document's fault
	// and not its configuration's ([Embedder.Tick]).
	embedded bool
}

// frontierKey is one source of a suspended isolation: its id and the digest
// of the text it was being isolated as — so a source rewritten since is a new
// input and goes back among its neighbours rather than into the isolation.
type frontierKey struct {
	id, sha string
}

// refusalID is the source one refusal is about.
type refusalID struct {
	source Source
	id     string
}

// refusal is one input refused alone, or answered with a vector the duty would
// not publish: the digest of the exact text refused, the source's version and
// title when it was last seen with that text — which is what lets a selection
// recognise it before reading its body ([Held]) — and when the provider last
// refused it.
type refusal struct {
	sha     string
	version uint64
	title   string
	at      time.Time
}

// NewRefusals is an empty memory, for one provider configuration.
func NewRefusals() *Refusals {
	return &Refusals{at: map[refusalID]refusal{}, frontier: map[Source][][]frontierKey{}}
}

// accepted records that the provider embedded a request.
func (r *Refusals) accepted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.embedded = true
}

// everAccepted reports whether the provider has embedded a request since this
// memory began — under this configuration, on this node.
func (r *Refusals) everAccepted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.embedded
}

// suspend keeps what source's isolation has not reached at the end of a tick —
// the split groups still to be sent — for the next tick to resume, replacing
// whatever was kept before: at most one isolation is under way a corpus,
// since its halves go before anything else it holds, so this is at most one
// request's sources.
func (r *Refusals) suspend(source Source, split [][]pending) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(split) == 0 {
		delete(r.frontier, source)
		return
	}
	groups := make([][]frontierKey, 0, len(split))
	for _, group := range split {
		keys := make([]frontierKey, len(group))
		for i, p := range group {
			keys[i] = frontierKey{id: p.doc.ID, sha: p.sha}
		}
		groups = append(groups, keys)
	}
	r.frontier[source] = groups
}

// resume is what source's last tick suspended ([Refusals.suspend]).
func (r *Refusals) resume(source Source) [][]frontierKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.frontier[source])
}

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

// refuse remembers p's text as refused alone at now — or as answered with a
// vector the duty would not publish, which is held back the same way.
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

// Held is what one corpus's selection passes over: sources whose text the
// provider refused alone and that this tick does not offer again, recognised
// by version and title BEFORE their bodies are read ([Corpus.Stale]).
//
// It is passed over inside the selection, not filtered after its limit: held
// sources keep the front of the oldest-first order, so filtering after would
// let H of them take H slots a tick, and once H reached the limit nothing newer
// was selected. Version and title, not the digest, because the digest needs
// the body, the read the pass-over spares; a moved version or title is read and
// the digest decides ([Refusals.restate]).
//
// THE ZERO VALUE PASSES OVER NOTHING.
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
