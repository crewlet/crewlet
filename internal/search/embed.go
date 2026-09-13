package search

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The embedding duty: ONE fleet singleton, one provider bill, N copies.
//
// # Why this is a duty and not a hook on the write path
//
// An embedding costs a provider call, and a write path that made one would put
// a third-party HTTP round trip inside the transaction that saves somebody's
// page. It would also charge the company once per NODE, for a value every node
// needs and nobody can compute differently. So it is a fleet singleton that
// publishes a record, and the record is what every node applies.
//
// # What it costs, from the constants below
//
// EmbedBatchesPerTick × EmbedBatch = 1 024 sources a tick, on a one-minute
// tick — across EVERY corpus rather than each, divided between them round
// robin by [Embedder.Tick], so the figures here are the company's and never
// one corpus's. A cold fill of 110 000 sources is ≈ 108 minutes and ≈ 860
// batched requests covering 110 000 inputs, billed once for the whole fleet —
// and those three numbers do NOT move with the configured width, because the
// provider bills per input TOKEN and `dimensions` is a truncation parameter it
// already receives. The write rate IS a function of the width: 1 024 records ×
// (4·D + envelope) per minute is ≈ 283 KB/s at 3 072, a factor of five under
// the pace the walk paths are held to.

const (
	// EmbedBatch is how many texts go in one provider call.
	//
	// 128, which is where the round-trip amortisation has essentially
	// flattened while the request stays comfortably inside every
	// provider's own input-count ceiling and inside a single HTTP body a
	// proxy will not refuse. At 8 KiB of input apiece that is a 1 MiB
	// request in the worst case.
	EmbedBatch = 128

	// EmbedBatchesPerTick bounds one tick's provider calls.
	//
	// EIGHT, so a tick spends at most eight round trips and the duty
	// cannot monopolise the singleton lease it holds. It is the knob that
	// trades cold-fill time against how much of a minute this job owns:
	// eight puts a 110 000-source fill at under two hours while leaving
	// the tick's wall clock dominated by the provider rather than by us.
	//
	// It is also the FAIRNESS FLOOR. [Embedder.Tick] hands these calls out
	// one at a time, round robin over the corpora, so each of N corpora is
	// guaranteed ⌊8/N⌋ of them however far behind its neighbours are — and
	// [NewEmbedder] refuses a wiring with more corpora than there are calls
	// to go round, because below one call apiece there is no per-tick share
	// left to guarantee.
	EmbedBatchesPerTick = 8

	// EmbedInterval is how often the duty ticks.
	//
	// ONE MINUTE, which is the tick the whole arithmetic above is written
	// against: 8 batches x 128 sources is 1 024 sources a tick, so a cold
	// fill of 110 000 sources is about 108 minutes and a steady company's
	// backlog is emptied within a minute of the write that created it. It
	// is also what bounds the WRITE rate this duty puts on the vector
	// log — 1 024 records x (4*D + envelope) per minute is about 283 KB/s
	// at a width of 3 072, a factor of five under the pace the walk paths
	// are held to.
	//
	// Faster would not embed anything sooner on a company that is caught
	// up (a tick with no stale source costs one indexed count and stops),
	// and on one that is behind it would raise the publish rate without
	// raising the provider's, which is the half that is actually slow.
	EmbedInterval = time.Minute

	// EmbedInputBytes caps what ONE source contributes to a request.
	//
	// 8 KiB. Past it the text is CUT rather than refused, and that is the
	// deliberate opposite of what this engine does with a vendor-limited
	// field elsewhere: a page's first eight kibibytes are what a semantic
	// search is about, so cutting keeps the document findable where
	// refusing would remove it from the corpus entirely with nothing to
	// say so.
	EmbedInputBytes = 8 << 10
)

// THERE IS NO STALL WINDOW HERE, deliberately, and the constant that used to
// declare one is gone rather than wired up.
//
// It named thirty minutes of no progress as the point an alarm fires, and no
// alarm read it — there is no progress timer on this duty at all, so the value
// promised a surface that did not exist. Wiring one is what the alarm table's
// own rule forbids: `recall_below_floor` already reports a duty that has
// stopped, from the coverage the corpora themselves are counted at, and a
// second threshold over the same event is a second opinion that drifts from
// the first. A duty that embeds nothing shows up as coverage that stops
// rising; a duty with nothing to embed shows up as coverage at one. A stall
// window cannot tell those apart, which is why the reading is the fraction and
// not the clock.

// # Two corpora, and the seam is why there was ever one
//
// [Corpus] had one implementation — the tracker's tasks — because
// `tracker_tasks` and `kb_vectors` are both in the replicated estate and the
// selection is one anti-join, while pages were this node's own projection and
// no statement may name a table in both. A page arm then would have been a
// bounded cursor sweep whose whole shape existed to work around that boundary.
// The pages domain removed it: `pages_heads` is written by an applier into the
// replicated estate, so [PageCorpus] is the same single anti-join and the duty
// itself did not change by a line — which is what this seam was for.

// Corpus is where the sources this duty embeds come from.
//
// DECLARED BY THE CONSUMER, which is this file, and kept to what the duty
// actually needs: the next batch of documents whose text has moved since the
// vector was computed. Each source kind implements it over its own tables, and
// both are one anti-join in one file today — the page arm was the one that had
// to cross the estate boundary, and the pages domain removed the boundary
// rather than the seam. Neither shape reaches the duty, which is what let that
// change cost this file nothing.
type Corpus interface {
	// Source is the kind this corpus supplies.
	Source() Source

	// Stale returns up to limit documents whose vector is missing or
	// computed from an older version, oldest first, together with the
	// sources whose vectors must be FORGOTTEN because the document is
	// gone.
	Stale(ctx context.Context, model string, dim, limit int) (stale []Document, gone []string, err error)

	// Coverage is how many of this corpus's sources carry a CURRENT
	// vector, and how many sources there are.
	//
	// THE SAME PREDICATE AS [Corpus.Stale], COUNTED RATHER THAN SELECTED,
	// and that is the whole requirement: a coverage number derived from a
	// second idea of what "current" means would disagree with the backlog
	// the duty is working through, and the alarm would fire against a
	// denominator nothing was ever going to fill.
	Coverage(ctx context.Context, model string, dim int) (current, total int, err error)
}

// Coverage is the fraction of every corpus's sources carrying a current
// vector, and whether there was anything to measure.
//
// FALSE RATHER THAN ZERO ON AN EMPTY CORPUS. A company that has written
// nothing down has no coverage, which is not the same fact as a company whose
// embeddings have stopped — and zero is the value the alarm fires on, so
// collapsing the two would page every fleet on its first day.
//
// IT SUMS ACROSS THE CORPORA rather than reporting the worst, because what the
// alarm is about is how much of what a search can reach is reachable by
// MEANING: a company whose tasks are fully embedded and whose pages are not is
// one whose searches are half-degraded, and the worst-of reading would call it
// wholly degraded while the average of two fractions would weight a
// twelve-page wiki equally with a hundred thousand tasks.
func Coverage(ctx context.Context, corpora []Corpus, model string, dim int) (float64, bool, error) {
	var current, total int
	for _, corpus := range corpora {
		have, all, err := corpus.Coverage(ctx, model, dim)
		if err != nil {
			return 0, false, err
		}
		current += have
		total += all
	}
	if total <= 0 {
		return 0, false, nil
	}
	// NEVER ABOVE ONE. The two counts come from one statement here, but a
	// corpus is free to answer from two — and a vector whose source was
	// removed between them would otherwise report a company as better than
	// completely covered, which reads as a broken gauge rather than as the
	// rounding it is.
	return min(float64(current)/float64(total), 1), true, nil
}

// Document is one source as the duty embeds it.
type Document struct {
	ID        string
	Container string

	// Version is the source's own version, stored beside the vector so
	// the next pass can tell a stale row from a current one without
	// re-embedding anything.
	Version uint64

	// Title and Body are the text. They are joined with a blank line
	// rather than concatenated, because a title running into a body is a
	// sentence the model has to disentangle before it can represent it.
	//
	// A TASK'S BODY IS NOT A COLUMN — it lives inside the encoded document,
	// deliberately, because nothing indexes it and a duplicate column costs
	// ≈ 200 MB a year. This duty reads it out with json_extract, which is
	// the one reader that needs the body without needing the rest of the
	// document: decoding every row to embed it would be the same 200 MB in
	// allocations per pass.
	Title string
	Body  string
}

// text is what is actually sent, cut to the per-source ceiling.
func (d Document) text() string {
	joined := strings.TrimSpace(d.Title) + "\n\n" + strings.TrimSpace(d.Body)
	return cut(strings.TrimSpace(joined), EmbedInputBytes)
}

// sha is the digest of the exact text that was embedded.
//
// OF THE CUT TEXT, not of the source, and that is what makes it able to answer
// the question it exists for: a source rewritten into the same words — a
// re-file, a label, a parent move — produces the same digest and is not paid
// for again. A digest of the whole body would differ whenever anything below
// the cut moved, which is text the provider never saw.
func (d Document) sha() string {
	sum := sha256.Sum256([]byte(d.text()))
	return hex.EncodeToString(sum[:])
}

// cut shortens a string at a rune boundary.
//
// BYTES, and the boundary matters: a plain slice is invalid UTF-8 whenever a
// multi-byte rune straddles the cut, which a JSON encoder substitutes and a
// vendor rejects.
func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !isRuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// EmbedDeps is what the duty needs.
type EmbedDeps struct {
	// Publisher is the vector domain's write authority.
	Publisher *statelog.Publisher

	// Embedder is the provider. A batch embedder is required rather than
	// preferred: one source at a time is 110 000 round trips for a cold
	// fill, which does not fit in the tick it runs on.
	Embedder embeddings.BatchEmbedder

	// Model is the embedding model id, which rides on every row so a
	// same-width model change can exclude the space it replaces.
	Model string

	// Corpora are the source kinds to embed.
	Corpora []Corpus

	// Logger is where a provider failure is reported. A failure is
	// LOGGED AND CARRIED, never propagated: the tick's other batches and
	// the tick after it are the retry, and a duty that failed the whole
	// tick on one bad batch would stop embedding the corpus because of
	// one document in it.
	Logger *slog.Logger

	// Now is the clock, injected so a test can hold it.
	Now func() time.Time
}

// Embedder is the duty.
type Embedder struct {
	deps EmbedDeps
}

// NewEmbedder builds the duty, refusing an incomplete wiring rather than
// starting and embedding nothing.
func NewEmbedder(d EmbedDeps) (*Embedder, error) {
	switch {
	case d.Publisher == nil:
		return nil, fmt.Errorf("search: the embed duty has no publisher")
	case d.Embedder == nil:
		return nil, fmt.Errorf("search: the embed duty has no embedder")
	case d.Model == "":
		return nil, fmt.Errorf("search: the embed duty has no model id — it " +
			"rides on every row, and rows written without one cannot be " +
			"excluded when the model changes at the same width")
	case d.Embedder.Width() <= 0:
		return nil, fmt.Errorf("search: the embed duty's provider reports "+
			"width %d", d.Embedder.Width())
	case len(d.Corpora) == 0:
		return nil, fmt.Errorf("search: the embed duty has no corpus")
	case len(d.Corpora) > EmbedBatchesPerTick:
		// REFUSED RATHER THAN SERVED UNFAIRLY. A tick divides
		// EmbedBatchesPerTick provider calls round robin, so with more
		// corpora than calls the ones past the eighth get none — not
		// this tick and not any tick, because every tick starts the
		// round at the front. That is the failure this whole file is
		// written against: a source kind silently unsearchable by
		// meaning, with a coverage gauge that sums the corpora
		// reporting the company as merely behind. Refusing says so at
		// the wiring, which is where the extra corpus was added.
		//
		// A CORPUS UNDER THE CEILING IS NOT FREE EITHER, and the bill
		// is in [Embedder.Tick]: every corpus is selected at the whole
		// tick's ceiling, so N of them read N x 1 024 whole documents
		// — untruncated bodies and all — to embed 1 024. Read that
		// paragraph before adding the third.
		return nil, fmt.Errorf("search: the embed duty has %d corpora and a "+
			"ceiling of %d provider calls a tick — every corpus must be "+
			"guaranteed at least one call or the ones at the back of "+
			"EmbedDeps.Corpora are never embedded at all; raise "+
			"EmbedBatchesPerTick or embed fewer source kinds",
			len(d.Corpora), EmbedBatchesPerTick)
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Embedder{deps: d}, nil
}

// Tick embeds one tick's worth of sources and reports how many records it
// published.
//
// # Why a failure is counted rather than returned
//
// The tick's unit of work is a BATCH, and the failures this meets are per
// batch: a rate limit, a timeout, one document the provider refuses. Returning
// the first one would abandon the other seven batches and every corpus after
// this one — so the corpus stops being embedded because of one document in it,
// and the symptom is a search that quietly stops improving.
//
// # How the tick's provider calls are divided between the corpora
//
// ROUND ROBIN, ONE BATCH AT A TIME, and the shape it replaced is why: a single
// budget spent corpus by corpus in order hands the whole tick to whoever is
// first in [EmbedDeps.Corpora] whenever that corpus has [EmbedBatchesPerTick]
// batches of backlog. A company writing tasks faster than 1 024 a minute — or
// simply one still cold-filling a hundred thousand of them — never reached its
// pages at all: not one page embedded, not one trashed page's vector
// withdrawn, for as long as the task backlog refilled. And the symptom is the
// one this file exists to prevent: page searches silently answering with the
// lexical half only, while a coverage gauge that SUMS the corpora reports a
// company that is merely behind.
//
// Round robin IS a reserved share plus the redistribution of what nobody used,
// expressed as one rule rather than two. With every corpus behind, each gets
// ⌊budget/N⌋ calls and the remainder goes to the ones at the front — an
// advantage bounded by a single call. With any of them caught up or empty, its
// turn is skipped and the rest take the calls it did not need, so the global
// ceiling is still spent in full and a corpus with nothing to do wastes
// nothing.
//
// The two alternatives, and what each costs:
//
//   - PROPORTIONAL TO BACKLOG is the other honest reading of "fair", and it
//     re-creates the starvation with arithmetic in place of ordering: at a
//     hundred tasks to one page, a page written this minute waits out the
//     entire task backlog. Equal shares bound a corpus's worst-case staleness
//     by ITS OWN size, which is the property a founder searching the wiki
//     actually has to be able to reason about.
//   - A ROTATING START INDEX fixes the starvation too, and needs memory across
//     ticks that this duty does not have: the engine rebuilds it every tick
//     because the provider and model are re-read on every config apply. It
//     also makes each corpus's progress bursty — a whole tick for one, then a
//     whole tick for the other — for identical throughput and a worse
//     worst-case latency on both.
func (e *Embedder) Tick(ctx context.Context) (int, error) {
	dim := e.deps.Embedder.Width()
	published := 0

	// ONE SELECTION PER CORPUS PER TICK, ASKED FOR THE WHOLE TICK'S
	// CEILING. Every batch a corpus is granted is sliced out of this one
	// list and the corpus is never re-queried, because a record this tick
	// published is not applied yet: a second selection inside the same tick
	// returns the documents just embedded and pays the provider for them
	// again. Asking for the ceiling rather than for the fair share is what
	// lets a corpus take the calls its neighbours did not use — how many
	// that is cannot be known until every corpus has answered.
	//
	// THE PRICE IS N SELECTIONS' WORTH OF ROWS TO SPEND ONE, and it is
	// written as a function of N rather than of today's corpora because N
	// is the term a later source kind moves: each corpus is asked for
	// EmbedBatchesPerTick*EmbedBatch rows while the tick can embed that
	// many in TOTAL, so with every corpus behind at once (N-1)/N of what
	// was read is discarded — 1 024 documents at the two corpora shipped,
	// 7 168 at the eight [NewEmbedder] allows. It is not only SQL work:
	// [Document.Body] holds the UNTRUNCATED body, since the cut to
	// EmbedInputBytes happens in [Document.text] at send time, so N x 1 024
	// whole documents are live for the length of the tick and a third
	// corpus is a 50 % rise in this duty's peak footprint before it embeds
	// anything new. That is the figure to weigh when adding one. None of it
	// is lost WORK — the selection is derived from the rows, so the next
	// tick asks again.
	//
	// TWO SHAPES THAT WOULD BOUND THE READ WERE WEIGHED AND NOT TAKEN,
	// because each buys it back with something load-bearing:
	//
	//   - A FAIR-SHARE FIRST PASS — ask each corpus for ceil(budget/N)
	//     batches and top up whoever came back full — rations the FORGET
	//     path by the same limit, because `gone` rides on this one
	//     selection. Withdrawals are deliberately OUTSIDE the provider
	//     budget (see the loop below), so rationing them by 1/N is exactly
	//     the trade this file refuses: a page somebody deleted would stay
	//     findable by meaning N times as long, to save rows on a tick whose
	//     wall clock is eight provider round trips.
	//   - A TOP-UP SELECTION for a corpus that exhausted its slice needs a
	//     cursor the [Corpus] seam does not have. It would re-read the rows
	//     it already took and filter them against an id set, since the
	//     selection order is not promised to be total — a smaller bounded
	//     waste plus a second query and an invariant nothing else here
	//     depends on, rather than no waste.
	stale := make([][]Document, len(e.deps.Corpora))
	var failed []error
	for i, corpus := range e.deps.Corpora {
		docs, gone, err := corpus.Stale(ctx, e.deps.Model, dim,
			EmbedBatchesPerTick*EmbedBatch)
		if err != nil {
			// A SELECTION FAILURE COSTS ITS OWN CORPUS AND NOT THE
			// TICK, for the same reason a batch failure costs its own
			// batch — and here the reason is sharper: the corpora are
			// visited in order, so returning on the first failure
			// would let one corpus whose query cannot run starve every
			// corpus behind it, tick after tick. That is the same
			// starvation the round robin below exists to remove, and
			// an ordering hazard is not less of one for arriving as an
			// error. It is still reported: the tick returns every
			// failure it carried.
			e.deps.Logger.WarnContext(ctx, "search_embed_select_failed",
				"source", string(corpus.Source()), "error", err.Error())
			failed = append(failed, fmt.Errorf(
				"search: select stale %s sources: %w", corpus.Source(), err))
			continue
		}
		stale[i] = docs

		// A FORGET COSTS NO PROVIDER CALL, so it is outside the budget
		// the calls are rationed by: withdrawing the vector of a task
		// somebody deleted is a correction the company has already paid
		// for, and rationing it would leave deleted work findable by
		// meaning for exactly as long as the corpus stayed behind. It is
		// bounded all the same, by the selection limit above.
		for _, id := range gone {
			if err := e.forget(ctx, corpus.Source(), id); err != nil {
				// AND THIS ONE DOES STOP THE TICK, because it is
				// not a fact about the corpus: an evicted node, an
				// unreachable broker or a store that refuses the
				// snapshot fails the next corpus's publishes
				// identically, so carrying it would buy nothing
				// and spend eight provider calls to find out.
				failed = append(failed, err)
				return published, errors.Join(failed...)
			}
			published++
		}
	}

	for budget := EmbedBatchesPerTick; budget > 0; {
		// ONE PASS OVER THE CORPORA, ONE BATCH EACH. A pass that hands
		// out nothing means no corpus has work left, which is the only
		// way out of this loop other than the budget: a caught-up
		// company must cost one selection per corpus and stop.
		spent := false
		for i, corpus := range e.deps.Corpora {
			if budget == 0 {
				break
			}
			if len(stale[i]) == 0 {
				continue
			}
			batch := stale[i][:min(EmbedBatch, len(stale[i]))]
			stale[i] = stale[i][len(batch):]
			budget--
			spent = true
			n, err := e.embed(ctx, corpus.Source(), dim, batch)
			published += n
			if err != nil {
				// LOGGED AND CARRIED. The next batch and the next
				// tick are the retry, and nothing here is lost:
				// the selection is derived from the rows
				// themselves, so an un-embedded source is simply
				// selected again.
				e.deps.Logger.WarnContext(ctx, "search_embed_batch_failed",
					"source", string(corpus.Source()),
					"documents", len(batch), "error", err.Error())
			}
		}
		if !spent {
			break
		}
	}
	return published, errors.Join(failed...)
}

// embed sends one batch and publishes a record per vector it got back.
func (e *Embedder) embed(ctx context.Context, source Source, dim int, batch []Document) (int, error) {
	texts := make([]string, len(batch))
	for i, doc := range batch {
		texts[i] = doc.text()
	}
	vectors, err := e.deps.Embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return 0, err
	}
	if len(vectors) != len(batch) {
		return 0, fmt.Errorf("search: the provider returned %d vectors for %d "+
			"documents — they are matched positionally, so a short answer is a "+
			"re-filing rather than a partial result", len(vectors), len(batch))
	}
	published := 0
	for i, vector := range vectors {
		if len(vector) == 0 {
			// A DOCUMENT WITH NOTHING TO EMBED, which is a real state
			// — an empty page, a task that is a title somebody
			// deleted — and not a failure. It keeps no vector and is
			// selected again next pass, which costs one slot in a
			// batch and never a provider call, because an empty
			// input is not sent.
			continue
		}
		if err := e.publish(ctx, source, dim, batch[i], vector); err != nil {
			// A VECTOR THIS DUTY REFUSES COSTS ITS OWN DOCUMENT AND
			// NOT THE BATCH, which is the same rule one level down
			// from the tick's: the refusal is about one vector, and
			// dropping the other 127 would make one poisoned
			// component cost a hundred provider calls' worth of
			// work. It is not silent — the log names the document —
			// and it is not lost, because the selection is over the
			// rows and picks it up again.
			if errors.Is(err, errUnusableVector) {
				e.deps.Logger.WarnContext(ctx, "search_embed_vector_refused",
					"source", string(source), "id", batch[i].ID,
					"error", err.Error())
				continue
			}
			return published, err
		}
		published++
	}
	return published, nil
}

// publish writes one embed record.
func (e *Embedder) publish(ctx context.Context, source Source, dim int, doc Document, vector []float32) error {
	subject := Subject{Source: source, ID: doc.ID}
	packed, err := pack(vector, dim)
	if err != nil {
		return fmt.Errorf("search: %s: %w: %w", subject, errUnusableVector, err)
	}
	rec := VectorRecord{
		RecordEnvelope: RecordEnvelope{
			Subject:   subject,
			Op:        OpEmbed,
			CreatedAt: e.deps.Now().UTC(),
			Scope: statelog.ScopeSet{
				Paths: []string{ScopePath(doc.Container, subject)},
			},
		},
		Container: doc.Container,
		Model:     e.deps.Model,
		Dim:       dim,
		SourceRev: doc.Version,
		TextSHA:   doc.sha(),
		Embedding: packed,
	}
	return e.append(ctx, subject, rec)
}

// forget withdraws a vector whose source is gone.
func (e *Embedder) forget(ctx context.Context, source Source, id string) error {
	subject := Subject{Source: source, ID: id}
	return e.append(ctx, subject, VectorRecord{
		RecordEnvelope: RecordEnvelope{
			Subject:   subject,
			Op:        OpForget,
			CreatedAt: e.deps.Now().UTC(),
			Scope: statelog.ScopeSet{
				// THE DOMAIN ROOT'S CONTAINER LEVEL IS UNKNOWN
				// HERE, because the source row is gone — so the
				// scope is the widest honest one for this
				// document rather than a container guessed from a
				// row nobody can read.
				Paths: []string{ScopePath(UnfiledContainer, subject)},
			},
		},
	})
}

// append publishes one record additively.
//
// PatternAdditive, and it is the pattern rather than a shortcut: this domain
// has exactly one writer per subject — a fleet singleton — so there is nothing
// to arbitrate against, and an expectation would be a per-subject anchor row
// written for a race that cannot happen. What makes a redelivery safe is the
// applier's own position guard.
func (e *Embedder) append(ctx context.Context, subject Subject, rec VectorRecord) error {
	// THE OP ID GOES ON THE RECORD BEFORE IT IS ENCODED, not beside it in
	// the request. The broker's duplicate window keys on the message id the
	// framework takes from the envelope, and this domain has no operation
	// ledger behind it — so an id that lived only in the request would
	// travel on the message and be absent from every copy of the record any
	// node ever reads back.
	rec.OpID = opIDFor(rec)
	payload, err := rec.Encode()
	if err != nil {
		return fmt.Errorf("search: encode the vector record for %s: %w", subject, err)
	}
	env, err := Domain{}.Envelope(payload)
	if err != nil {
		return fmt.Errorf("search: the record this duty just wrote for %s does "+
			"not decode: %w", subject, err)
	}
	opID := env.OpID
	res, err := e.deps.Publisher.Publish(ctx, statelog.Request{
		Subject:  env.Subject,
		Scope:    env.Scope,
		OpID:     opID,
		MintedAt: e.deps.Now().UTC(),
		Pattern:  statelog.PatternAdditive,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return statelog.Decision{Payload: payload, Envelope: env}, nil
		},
	})
	if err != nil {
		return fmt.Errorf("search: publish the vector record for %s: %w", subject, err)
	}
	// AN UNKNOWN OUTCOME IS NOT AN ERROR HERE, and this is the one domain
	// where that is honestly true: nothing is waiting on the answer, the
	// apply is idempotent under its position guard, and the selection that
	// produced this document is derived from the rows — so a record that
	// landed is not re-sent and one that did not is simply selected again.
	if res.Outcome == statelog.OutcomeUnknown {
		e.deps.Logger.InfoContext(ctx, "search_embed_unresolved",
			"subject", subject.String(), "op_id", opID)
	}
	return nil
}

// opIDFor is the record's own idempotency key, DERIVED rather than minted.
//
// A uuid7 per attempt would make every retry a new operation, and the broker's
// duplicate window — the only dedupe this domain has, since it keeps no
// operation ledger — would collapse nothing. Derived from what the record
// says, a retry of the same work carries the same id and is collapsed; a
// genuinely newer vector for the same source carries a different one, because
// the source version and the text digest are both in it.
func opIDFor(rec VectorRecord) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		rec.Subject.String(), string(rec.Op), rec.Model,
		fmt.Sprint(rec.Dim), fmt.Sprint(rec.SourceRev), rec.TextSHA,
	}, "\x00")))
	return "vec-" + hex.EncodeToString(sum[:16])
}

// errUnusableVector marks a vector this duty will not publish, which its
// caller treats as costing that document and not the batch it arrived in.
var errUnusableVector = errors.New("the provider's vector cannot be published")

// pack renders a vector in the layout the column holds.
//
// THE FINITENESS CHECK IS HERE rather than at the store's boundary, and it is
// not duplication: this value is published to every node in the fleet before
// any of them writes it, so a NaN refused at the write boundary would be a
// record every applier fails on for ever — a poison message on a stream with
// no dead-letter path. A vector_distance_cos of NaN answers 0, a PERFECT
// match, so one poisoned row outranks every genuine hit in every search.
func pack(v []float32, dim int) ([]byte, error) {
	if len(v) != dim {
		return nil, fmt.Errorf("the provider returned a %d-wide vector and the "+
			"corpus is embedded at %d", len(v), dim)
	}
	out := make([]byte, 4*len(v))
	for i, f := range v {
		f64 := float64(f)
		if math.IsNaN(f64) || math.IsInf(f64, 0) {
			return nil, fmt.Errorf("component %d of %d is %v — a non-finite "+
				"component scores as a perfect match against everything",
				i, len(v), f)
		}
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(f))
	}
	return out, nil
}

// TaskCorpus embeds the native tracker's tasks.
//
// ONE STATEMENT, because `tracker_tasks` and `kb_vectors` are both in the
// replicated estate: the selection is an anti-join between the rows and their
// own vectors, and there is no second read to keep in step.
type TaskCorpus struct{ DB *store.DB }

// Source implements [Corpus].
func (TaskCorpus) Source() Source { return SourceTask }

// Stale implements [Corpus].
func (c TaskCorpus) Stale(ctx context.Context, model string, dim, limit int) ([]Document, []string, error) {
	var stale []Document
	var gone []string
	err := c.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT t.id, t.project_key, t.version, t.title,
			       COALESCE(json_extract(t.document, '$.body'), '')
			FROM tracker_tasks t
			LEFT JOIN kb_vectors v
			  ON v.source = 'task' AND v.source_id = t.id
			WHERE t.removed_at IS NULL
			  AND (v.source_id IS NULL
			       OR v.source_rev <> t.version
			       OR v.model <> ? OR v.dim <> ?)
			ORDER BY t.updated_at
			LIMIT ?`, model, dim, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var doc Document
			var version int64
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := rows.Scan(&doc.ID, &doc.Container, &version,
				&doc.Title, &doc.Body); err != nil {
				return err
			}
			doc.Version = uint64(version)
			stale = append(stale, doc)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := rows.Err(); err != nil {
			return err
		}

		// AND THE OTHER DIRECTION: a vector whose task has been removed
		// or purged. Without it a deleted task stays findable by meaning
		// for ever — the row it came from is gone, so nothing else will
		// ever select it.
		dead, err := tx.QueryContext(ctx, `
			SELECT v.source_id
			FROM kb_vectors v
			LEFT JOIN tracker_tasks t
			  ON t.id = v.source_id AND t.removed_at IS NULL
			WHERE v.source = 'task' AND t.id IS NULL
			LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer func() { _ = dead.Close() }()
		for dead.Next() {
			var id string
			if err := dead.Scan(&id); err != nil {
				return err
			}
			gone = append(gone, id)
		}
		return dead.Err()
	})
	if err != nil {
		return nil, nil, err
	}
	return stale, gone, nil
}

// Coverage implements [Corpus].
//
// ONE STATEMENT, so the numerator and the denominator describe the same
// instant. Two counts would let a write land between them and report a
// coverage this corpus never had — which on a company writing tasks steadily
// is not a rare race but the ordinary case.
//
// THE PREDICATE IS [TaskCorpus.Stale]'s, INVERTED. A source is covered when it
// has a vector at this model and width whose `source_rev` still matches the
// task's version; every other row is exactly what the duty would select next.
func (c TaskCorpus) Coverage(ctx context.Context, model string, dim int) (int, int, error) {
	var current, total int
	err := c.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COUNT(*),
			       COUNT(CASE WHEN v.source_id IS NOT NULL
			                   AND v.source_rev = t.version
			                   AND v.model = ? AND v.dim = ?
			                  THEN 1 END)
			FROM tracker_tasks t
			LEFT JOIN kb_vectors v
			  ON v.source = 'task' AND v.source_id = t.id
			WHERE t.removed_at IS NULL`, model, dim).Scan(&total, &current)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("search: count the task corpus's vector "+
			"coverage: %w", err)
	}
	return current, total, nil
}
