package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// Filling the vectors a seat's diary is missing, on the node that holds it.
//
// A note is embedded as it is written ([learning.Diary.Write]). Three kinds of
// note are left with no vector of the current model anyway, and nothing else
// would ever give them one:
//
//   - every note written before notes were embedded at all — which is every
//     note any build before this one wrote;
//   - a note written while the provider did not answer inside its budget;
//   - every note written under a model the company has since moved off,
//     which recall no longer compares (see learning.RecallQuery.Model).
//
// The similarity half of `## Personal memory` reaches none of those, so a
// durable fact older than the newest fifty is unreachable and — never
// selected — is the first thing the 500-entry trim evicts.
//
// # The holder, and only once the seat is established
//
// Only the node holding a seat writes its memory and carries it on the
// changelog; every other node's copy is a stale one the next hydration
// repairs. So a pass fills only seats this node HOLDS, and only once the seat
// may start a turn ([seat.Host.MayStart]) — which is after its acquisition
// hydrated its memory, so the pass fills the rows the seat actually has. A
// filled note takes a fresh change sequence, which is what carries it to the
// next holder (see learning.Diary.FillEmbeddings).
//
// # Its own loop, beside the memory sync rather than inside it
//
// The memory sync's cycle is the crash window for everything a seat learns,
// and a provider having a slow minute must not widen it: a fill waits on an
// embeddings API, a publish on the broker. So the two run apart, and a filled
// note reaches the changelog on the memory sync's next cycle.

const (
	// diaryBackfillInterval is how often the holder looks for notes to
	// fill.
	//
	// A MINUTE, the knowledge corpus's own embedding cadence
	// (search.EmbedInterval), for the same reason it gives: a tick with
	// nothing to fill costs one indexed read per held seat and stops, and
	// a faster tick would not fill anything sooner — what is slow is the
	// provider.
	diaryBackfillInterval = time.Minute

	// diaryBackfillBatch is how many notes one call fills.
	//
	// 128, the corpus duty's own batch (search.EmbedBatch): a note is at
	// most learning.MaxContentChars (2 000 bytes), so a full batch is at
	// most 256 000 bytes — one request inside OpenAI's 300 000-token
	// request total, since a byte bounds a token — and the provider packs
	// a model with smaller limits into as many requests as it needs.
	diaryBackfillBatch = 128

	// diaryBackfillPerTick bounds what one node fills in one tick, across
	// every seat it holds.
	//
	// THE BURST, and what it is sized against. A model change leaves
	// EVERY seat's diary in the old space at once, and an unbounded tick
	// would send all of it in the first minute: fifty seats at the 500-note
	// cap is 25 000 notes. 512 notes is at most about a million bytes a
	// minute — the 1 000 000 tokens a minute of OpenAI's lowest paid tier
	// for these models in the worst case of 2 000-byte notes, and a fraction
	// of that for notes of a typical few hundred bytes. A seat at the cap
	// catches up in one tick, and a node of fifty such seats in under an
	// hour. A JUDGEMENT CALL: a higher account tier could take more, and a
	// seat's similarity half reaches only its filled notes meanwhile, so
	// this trades the catch-up time against the burst a switch puts on the
	// account.
	diaryBackfillPerTick = 4 * diaryBackfillBatch
)

// diaryBackfill is the fill loop and the handle that stops it.
type diaryBackfill struct {
	cancel context.CancelFunc
	done   sync.WaitGroup
}

// startDiaryBackfill runs the fill loop until [Engine.stopDiaryBackfill].
//
// DETACHED from the signal context, like the memory sync: the drain still has
// seats to release when SIGTERM lands. Unlike the memory sync it is CANCELLED
// on stop rather than waited out: a tick mid-call holds a provider request,
// not a broker write, and abandoning it loses nothing a later holder will not
// fill again — its transaction rolls back whole.
func (e *Engine) startDiaryBackfill(ctx context.Context) {
	if e.backends == nil || e.backends.Store == nil || e.node == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	loop := &diaryBackfill{cancel: cancel}
	e.backfill = loop
	ticker := time.NewTicker(diaryBackfillInterval)
	loop.done.Go(func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.backfillDiaries(ctx)
			}
		}
	})
}

// stopDiaryBackfill ends the loop, abandoning a tick in flight.
func (e *Engine) stopDiaryBackfill() {
	if e.backfill == nil {
		return
	}
	e.backfill.cancel()
	e.backfill.done.Wait()
	e.backfill = nil
}

// backfillDiaries is one tick of the loop over the seats this node holds.
//
// BOUNDED BY THE INTERVAL ITSELF: a tick that cannot finish before the next is
// due has fallen behind whatever it does next.
func (e *Engine) backfillDiaries(ctx context.Context) {
	provider, ok := e.batchEmbedder()
	if !ok {
		return
	}
	host := e.node.Host()
	if host == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, diaryBackfillInterval)
	defer cancel()
	fillHeldDiaries(ctx, host, e.agentIDOf, learning.NewDiary(e.backends.Store), provider,
		diaryBackfillPerTick)
}

// heldSeats is the part of the seat host the fill reads: which seats this node
// holds, and whether each is established.
type heldSeats interface {
	Held() []string
	MayStart(handle string) (int64, bool)
}

// fillHeldDiaries fills, for every established seat in held, the notes with no
// vector of the provider's model — at most budget of them across the seats, in
// calls of at most diaryBackfillBatch — and reports how many it filled.
func fillHeldDiaries(ctx context.Context, held heldSeats, agentIDOf func(string) string,
	diary *learning.Diary, provider embeddings.BatchEmbedder, budget int,
) int {
	model := provider.Model()
	total := 0
	for _, handle := range held.Held() {
		if budget <= 0 || ctx.Err() != nil {
			break
		}
		if _, may := held.MayStart(handle); !may {
			// Still establishing — its memory is not hydrated yet — or
			// no longer renewing. Either way not this tick's to fill.
			continue
		}
		agentID := agentIDOf(handle)
		if agentID == "" {
			continue
		}
		filled, err := fillSeatDiary(ctx, diary, provider, agentID, model, budget)
		if filled > 0 {
			log.InfoContext(ctx, "diary_backfilled", "seat", handle,
				"model", model, "notes", filled)
		}
		if err != nil {
			// Logged and stepped over: one seat's notes, or a provider
			// refusing one batch, must not stop the others.
			log.WarnContext(ctx, "diary_backfill_failed", "seat", handle,
				"model", model, "error", err,
				"detail", "these notes stay out of the similarity half of the "+
					"seat's memory until a later tick fills them")
		}
		budget -= filled
		total += filled
	}
	return total
}

// fillSeatDiary fills one seat's notes, a batch at a time, until none are left,
// the budget is spent, or a batch fills nothing.
func fillSeatDiary(ctx context.Context, diary *learning.Diary, provider embeddings.BatchEmbedder,
	agentID, model string, budget int,
) (int, error) {
	total := 0
	for budget > 0 {
		limit := min(budget, diaryBackfillBatch)
		notes, err := diary.Unembedded(ctx, agentID, model, time.Now().UTC(), limit)
		if err != nil || len(notes) == 0 {
			return total, err
		}
		filled, err := fillNotes(ctx, diary, provider, model, notes)
		total += filled
		budget -= len(notes)
		if err != nil {
			return total, err
		}
		if filled < len(notes) || len(notes) < limit {
			// Nothing left, or notes this pass could not fill — which
			// the next tick asks about again rather than this one
			// spinning on.
			return total, nil
		}
	}
	return total, nil
}

// fillNotes embeds notes in one call and stores their vectors.
//
// A REFUSED BATCH IS RETRIED NOTE BY NOTE, because a provider refuses a whole
// request over one input it cannot take, and only a smaller request says which
// (embeddings.ErrRefused): one note the provider will never accept must not
// hold back every note batched beside it. Any other failure is the provider's,
// not a note's, and fails the call.
func fillNotes(ctx context.Context, diary *learning.Diary, provider embeddings.BatchEmbedder,
	model string, notes []learning.DiaryEntry,
) (int, error) {
	texts := make([]string, len(notes))
	for i, note := range notes {
		texts[i] = note.Content
	}
	vectors, err := embeddings.EmbedWholeBatch(ctx, provider, texts)
	if errors.Is(err, embeddings.ErrRefused) && len(notes) > 1 {
		filled := 0
		var refused error
		for _, note := range notes {
			n, alone := fillNotes(ctx, diary, provider, model, []learning.DiaryEntry{note})
			filled += n
			if alone != nil {
				refused = alone
			}
		}
		return filled, refused
	}
	if err != nil {
		return 0, err
	}
	fills := make([]learning.DiaryFill, 0, len(notes))
	for i, note := range notes {
		if len(vectors[i]) == 0 {
			continue
		}
		fills = append(fills, learning.DiaryFill{
			ID: note.ID, Vector: learning.Vector{Values: vectors[i], Model: model},
		})
	}
	return diary.FillEmbeddings(ctx, fills)
}

// batchEmbedder is the current embedder as a caller embedding many texts needs
// it, and false when the company configures none or its provider cannot batch.
//
// A PROVIDER THAT CANNOT BATCH DOES NOT FILL, for the corpus duty's reason
// (see [Engine.embedModel]): a seat's whole diary one note per round trip is
// the cost the batch interface exists to avoid, and every provider this build
// ships batches.
func (e *Engine) batchEmbedder() (embeddings.BatchEmbedder, bool) {
	held := e.embeddings.Load()
	if held == nil || *held == nil {
		return nil, false
	}
	batch, ok := (*held).(embeddings.BatchEmbedder)
	return batch, ok
}
