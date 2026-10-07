package engine

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/store"
)

// Filling the vectors a seat's memory is missing, on the node that holds it.
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
// An EPISODE is the same story with a worse ending. Its vector is of the turn
// whole — label, ask and what it did — and is made as the episode is written;
// a turn written while the provider did not answer, and after a model change
// every turn the seat ever took, has none that recall compares, and before the
// row stored its ask nothing could make one again. Now it does (node migration
// 0042), so the raw episodes are filled beside the notes
// ([learning.Episodes.Unfilled]), and `## Similar prior work` and
// `query_episodes` reach a seat's history again rather than only what it did
// since the change.
//
// # The holder, and only once the seat is established
//
// Only the node holding a seat writes its memory and carries it on the
// changelog; every other node's copy is a stale one the next hydration
// repairs. So a tick fills only seats this node HOLDS, and only once the seat
// may start a turn ([seat.Host.MayStart]) — which is after its acquisition
// hydrated its memory, so the tick fills the rows the seat actually has. A
// filled row takes a fresh change sequence, which is what carries it to the
// next holder (see learning.Diary.FillEmbeddings).
//
// # Every request under one discipline, across every seat
//
// A tick is ONE [embeddings.Pass] over every seat it visits, so what the
// provider answers about one seat's request governs the next seat's too: a
// rate limit, a timeout, a revoked key or a model the endpoint does not serve
// ends the TICK, not the seat — every held seat would otherwise send its own
// full batch into the same answer, every minute — and a refusal is split until
// the input it refuses is alone, then held back for the hour
// ([embeddings.RefusalRetry]) by a memory kept across ticks for as long as the
// provider is configured the same ([embedConfiguration]). The pass is bounded
// in what it SENDS, refused requests included, never in what came of it.
//
// # Its own loop, beside the memory sync rather than inside it
//
// The memory sync's cycle is the crash window for everything a seat learns,
// and a provider having a slow minute must not widen it: a fill waits on an
// embeddings API, a publish on the broker. So the two run apart, and a filled
// row reaches the changelog on the memory sync's next cycle.

const (
	// memoryFillInterval is how often the holder looks for rows to fill.
	//
	// A MINUTE, the knowledge corpus's own embedding cadence
	// (search.EmbedInterval), for the same reason it gives: a tick with
	// nothing to fill costs one indexed read per table per held seat and
	// stops, and a faster tick would not fill anything sooner — what is
	// slow is the provider.
	memoryFillInterval = time.Minute

	// memoryFillPage is how many rows one read of a seat's table takes —
	// one full call of the pass ([embeddings.PassBatch]).
	memoryFillPage = embeddings.PassBatch

	// memoryFillRequestsPerTick bounds the provider requests one node's
	// tick sends.
	//
	// SIXTEEN: exactly what isolating one refused input among a full call
	// costs inside the tick that meets it — 1 + 2·log₂128 = 15, and the one
	// canary that judges the refusal when it is the tick's first answer
	// (embeddings.Pass) — and what bounds the tick's wall clock. A provider
	// refusing its configuration costs two of them (the refused call and the
	// canary) before the tick concludes so. Per node rather than shared: a
	// provider limits these models by the token rather than by the request,
	// and sixteen a minute is far inside any request rate a vendor publishes.
	memoryFillRequestsPerTick = 16

	// memoryFillBytesPerTick bounds what the COMPANY's holders send in one
	// tick between them, in prepared bytes: each node takes an equal share
	// of it, divided by the live nodes that run seats
	// ([seat.SweepResult.LiveNodes]).
	//
	// AN ACCOUNT'S BUDGET, because the rate it is sized against is one: a
	// provider limits tokens a minute per ACCOUNT, and every node's fill
	// draws on the one account the company configured. Stated per node, as
	// it was, the burst a model change puts on the account multiplied with
	// the fleet. A MILLION BYTES bounds a million tokens — a token is at
	// least one byte (config.EmbeddingModels) — which is the tokens a minute
	// of OpenAI's lowest paid tier for these models, so a model change alone
	// cannot exhaust such an account's minute: about 500 notes at the
	// 2 000-byte note bound a minute, and many more of a typical few hundred
	// bytes, so a company of fifty seats at the 500-note cap re-embeds its
	// diaries in under an hour. A JUDGEMENT CALL: a higher tier could take
	// more, the knowledge corpus's own duty draws on the same account, and a
	// seat's similarity search reaches only its filled rows meanwhile — so
	// this trades the catch-up time against the share of the account's
	// minute a model change takes. A tick that meets a rate limit anyway
	// ends there, and the next is the retry.
	memoryFillBytesPerTick = 1_000_000
)

// memoryFill is the fill loop, the handle that stops it, and the state its
// ticks share.
type memoryFill struct {
	cancel context.CancelFunc
	done   sync.WaitGroup

	// refusals is what the provider refused, kept across ticks, and
	// refusalsFor the configuration it belongs to ([embedConfiguration]) —
	// the corpus duty's identity, so the two memories this node keeps of
	// one provider move together. A refusal is a fact about the provider
	// as it was configured, so a provider configured otherwise starts with
	// none and a configuration concluded refused is judged again at once;
	// one an apply merely built again the same — an unrelated change, a
	// re-activation that rotated the key — keeps the rows it holds back
	// and the pause it is in. Touched only by the loop's own goroutine.
	refusals    *embeddings.Refusals
	refusalsFor embedConfiguration

	// resume is the seat the last tick's pass ran out at, where the next
	// tick starts — so a budget spent before the last seat does not leave
	// the seats after it for ever behind the ones before.
	resume string
}

// startMemoryFill runs the fill loop until [Engine.stopMemoryFill].
//
// DETACHED from the signal context, like the memory sync: the drain still has
// seats to release when SIGTERM lands. Unlike the memory sync it is CANCELLED
// on stop rather than waited out: a tick mid-call holds a provider request,
// not a broker write, and abandoning it loses nothing a later holder will not
// fill again — its transaction rolls back whole.
func (e *Engine) startMemoryFill(ctx context.Context) {
	if e.backends == nil || e.backends.Store == nil || e.node == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	loop := &memoryFill{cancel: cancel}
	e.fill = loop
	ticker := time.NewTicker(memoryFillInterval)
	loop.done.Go(func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.fillMemory(ctx, loop)
			}
		}
	})
}

// stopMemoryFill ends the loop, abandoning a tick in flight.
func (e *Engine) stopMemoryFill() {
	if e.fill == nil {
		return
	}
	e.fill.cancel()
	e.fill.done.Wait()
	e.fill = nil
}

// fillMemory is one tick of the loop over the seats this node holds.
//
// BOUNDED BY THE INTERVAL ITSELF: a tick that cannot finish before the next is
// due has fallen behind whatever it does next.
func (e *Engine) fillMemory(ctx context.Context, loop *memoryFill) {
	provider, ok := batchOf(e.embeddings.Load())
	if !ok {
		return
	}
	host := e.node.Host()
	if host == nil {
		return
	}
	memory := loop.memoryFor(provider)
	liveNodes := 1
	if sweep, swept := host.LastSweep(); swept && sweep.LiveNodes > 1 {
		liveNodes = sweep.LiveNodes
	}
	ctx, cancel := context.WithTimeout(ctx, memoryFillInterval)
	defer cancel()
	pass := embeddings.NewPass(memory, time.Now().UTC(), memoryFillRequestsPerTick,
		shareOf(memoryFillBytesPerTick, liveNodes))
	report := fillHeldMemory(ctx, host, memorySources(e.backends.Store, e.agentIDOf),
		provider, pass, loop.resume)
	loop.resume = report.resume
	report.log(ctx, provider)
}

// memoryFor is the refusal memory a tick embedding with provider sends under:
// the one the loop holds while the provider is configured as it was, and a new
// one once it is configured otherwise ([embedConfiguration]).
//
// THE MEMORY FOLLOWS THE CONFIGURATION, read off the very provider the tick
// embeds with — the corpus duty's rule ([embedDuty.tick]) — so no apply
// landing between two reads can pair one provider's refusals with another's.
func (loop *memoryFill) memoryFor(provider embeddings.Embedder) *embeddings.Refusals {
	if configuration := configurationOf(provider); loop.refusals == nil ||
		loop.refusalsFor != configuration {
		loop.refusals, loop.refusalsFor = embeddings.NewRefusals(), configuration
	}
	return loop.refusals
}

// shareOf is one of n equal shares of total, rounded up so no share is zero;
// no nodes at all is one, this one.
func shareOf(total, n int) int {
	n = max(n, 1)
	return (total + n - 1) / n
}

// heldSeats is the part of the seat host the fill reads: which seats this node
// holds, and whether each is established.
type heldSeats interface {
	Held() []string
	MayStart(handle string) (int64, bool)
}

// fillSource is one table of a seat's memory as the fill walks it.
type fillSource struct {
	// table names it in the refusal memory's scope and the log lines.
	table string

	// seat is the table's name for a seat — a handle, or the derived agent
	// id the diary is keyed on — and "" for a seat it cannot name.
	seat func(handle string) string

	// unfilled is one page of the seat's rows with no vector of model at
	// the store's width, strictly after the cursor; fill stores vectors.
	unfilled func(ctx context.Context, seat, model string, after learning.FillCursor,
		limit int) ([]learning.Unfilled, error)
	fill func(ctx context.Context, fills []learning.VectorFill) (int, error)
}

// memorySources are the tables of a seat's memory the fill walks, in order:
// the diary, keyed on the seat's derived agent id, and its raw episodes,
// keyed on its handle.
func memorySources(db *store.DB, agentIDOf func(string) string) []fillSource {
	diary, episodes := learning.NewDiary(db), learning.NewEpisodes(db)
	return []fillSource{{
		table: "diary",
		seat:  agentIDOf,
		unfilled: func(ctx context.Context, seat, model string, after learning.FillCursor,
			limit int,
		) ([]learning.Unfilled, error) {
			return diary.Unfilled(ctx, seat, model, time.Now().UTC(), after, limit)
		},
		fill: diary.FillEmbeddings,
	}, {
		table:    "episodes",
		seat:     func(handle string) string { return handle },
		unfilled: episodes.Unfilled,
		fill:     episodes.FillEmbeddings,
	}}
}

// fillReport is what one tick did.
type fillReport struct {
	pass *embeddings.Pass

	// filled is the rows stored, by table.
	filled map[string]int

	// resume is the seat the pass ran out at, or "" for a tick that
	// visited every seat it holds.
	resume string

	// storeErr is the write that stopped the tick, if one did.
	storeErr error
}

// fillHeldMemory fills, for every established seat in held, the rows with no
// vector of the provider's model — starting at resume, under one pass — and
// reports what it did.
func fillHeldMemory(ctx context.Context, held heldSeats, sources []fillSource,
	provider embeddings.BatchEmbedder, pass *embeddings.Pass, resume string,
) fillReport {
	report := fillReport{pass: pass, filled: map[string]int{}}
	seats := held.Held()
	slices.Sort(seats)
	if at, _ := slices.BinarySearch(seats, resume); at < len(seats) {
		seats = slices.Concat(seats[at:], seats[:at])
	}
	model := provider.Model()
	for _, handle := range seats {
		if !pass.Open() || ctx.Err() != nil {
			report.resume = handle
			return report
		}
		if _, may := held.MayStart(handle); !may {
			// Still establishing — its memory is not hydrated yet — or
			// no longer renewing. Either way not this tick's to fill.
			continue
		}
		for _, source := range sources {
			seat := source.seat(handle)
			if seat == "" {
				continue
			}
			filled, err := fillSeat(ctx, source, seat, model, provider, pass)
			report.filled[source.table] += filled
			var stored storeError
			switch {
			case errors.As(err, &stored):
				report.storeErr, report.resume = stored.err, handle
				return report
			case err != nil:
				// A READ OF ONE SEAT'S ROWS that failed costs that
				// seat's table this tick, not the others'.
				log.WarnContext(ctx, "memory_fill_read_failed", "seat", handle,
					"table", source.table, "error", err)
			}
			if !pass.Open() {
				// SPENT OR STOPPED HERE, so the next tick starts at this
				// seat: it may have rows left that the pass never reached.
				report.resume = handle
				return report
			}
		}
	}
	return report
}

// storeError is a fill's write that failed, told apart from a read.
type storeError struct{ err error }

func (e storeError) Error() string { return e.err.Error() }

// fillSeat walks one table of one seat, a page at a time, until it is
// exhausted or the pass is spent, and reports how many rows it filled.
func fillSeat(ctx context.Context, source fillSource, seat, model string,
	provider embeddings.BatchEmbedder, pass *embeddings.Pass,
) (int, error) {
	scope := source.table + "/" + seat
	filled := 0
	var after learning.FillCursor
	for pass.Open() {
		page, err := source.unfilled(ctx, seat, model, after, memoryFillPage)
		if err != nil {
			return filled, err
		}
		if len(page) == 0 {
			return filled, nil
		}
		after = page[len(page)-1].Cursor
		inputs := make([]embeddings.PassInput, len(page))
		for i, row := range page {
			inputs[i] = embeddings.PassInput{Scope: scope, ID: row.ID, Text: row.Text}
		}
		if err := pass.Embed(ctx, provider, inputs,
			func(rows []embeddings.PassInput, vectors [][]float32) error {
				fills := make([]learning.VectorFill, 0, len(rows))
				for i, row := range rows {
					if vectors[i] == nil {
						continue
					}
					fills = append(fills, learning.VectorFill{ID: row.ID,
						Vector: learning.Vector{Values: vectors[i], Model: model}})
				}
				n, err := source.fill(ctx, fills)
				filled += n
				return err
			}); err != nil {
			return filled, storeError{err}
		}
		if len(page) < memoryFillPage {
			return filled, nil
		}
	}
	return filled, nil
}

// fillLine is one line a tick reports.
type fillLine struct {
	level slog.Level
	msg   string
	args  []any
}

// log reports a tick ([fillReport.lines]).
func (r fillReport) log(ctx context.Context, provider embeddings.BatchEmbedder) {
	for _, line := range r.lines(provider) {
		log.Log(ctx, line.level, line.msg, line.args...)
	}
}

// lines is what a tick reports: what it filled, every input the provider
// refused alone, the requests it had refused while it was still narrowing them
// down, and why it stopped where it did.
//
// A VALUE, so what a tick says is a fact a test reads rather than a side
// effect on the process's logger.
func (r fillReport) lines(provider embeddings.BatchEmbedder) []fillLine {
	model, pass := provider.Model(), r.pass
	var out []fillLine
	add := func(level slog.Level, msg string, args ...any) {
		out = append(out, fillLine{level: level, msg: msg, args: args})
	}
	for _, refused := range pass.RefusedAlone {
		add(slog.LevelWarn, "memory_fill_input_refused", "scope", refused.Input.Scope,
			"id", refused.Input.ID, "model", model, "bytes", refused.Bytes,
			"input_limit_bytes", provider.Limits().InputBytes, "retry", refused.Retry,
			"retry_in", embeddings.RefusalRetry.String(), "error", refused.Err,
			"detail", "the provider refused this row's text sent alone; it keeps no "+
				"vector of this model and is offered again alone when the retry is "+
				"due — a refusal of text inside input_limit_bytes says the model's "+
				"limits are declared wider than the endpoint enforces "+
				"(providers.embeddings.max_input_tokens), or that the endpoint "+
				"refuses this text for what it says")
	}
	if pass.Unusable > 0 {
		add(slog.LevelWarn, "memory_fill_vector_unusable", "model", model,
			"rows", pass.Unusable,
			"detail", "the provider answered these rows' requests with vectors that "+
				"have no direction to keep; each stays unfilled and is sent again "+
				"next tick, and its neighbours were filled")
	}
	total := 0
	for _, n := range r.filled {
		total += n
	}
	if total > 0 {
		add(slog.LevelInfo, "memory_filled", "model", model, "notes", r.filled["diary"],
			"episodes", r.filled["episodes"], "requests", pass.Requests, "bytes", pass.Bytes,
			"refused", pass.Refused)
	}
	if isolating := pass.Refused - len(pass.RefusedAlone); isolating > 0 && total == 0 &&
		!pass.Concluded {
		// A TICK THAT FILLED NOTHING AND NAMED NO ROW, and still had
		// requests refused, is narrowing down rows a refusal concerned —
		// several to a call, more than one tick's requests can isolate.
		// Silent, it read as a tick that sent nothing at all.
		add(slog.LevelInfo, "memory_fill_refusals_isolating", "model", model,
			"requests", pass.Requests, "refused", pass.Refused,
			"refused_alone", len(pass.RefusedAlone),
			"detail", "the provider refused requests of several rows each and this "+
				"tick filled none; the next tick halves them again where this one "+
				"stopped, and each row the provider refuses is named once it is "+
				"refused alone")
	}
	switch err := pass.Err(); {
	case r.storeErr != nil:
		add(slog.LevelWarn, "memory_fill_store_failed", "model", model, "error", r.storeErr,
			"detail", "this tick stores nothing more; the rows it could not store are "+
				"selected again by the next")
	case pass.Concluded:
		add(slog.LevelWarn, "memory_fill_configuration_refused", "model", model,
			"requests", pass.Requests, "pause", embeddings.RefusalRetry.String(),
			"error", err,
			"detail", "the provider refused this tick's first request and then one "+
				"plain word sent alone to judge it — the refusal is "+
				"providers.embeddings', not any row's; the fill sends nothing for "+
				"the pause and judges the next refusal again after it, and an "+
				"apply that changes the embeddings' model, width, limits or "+
				"endpoint judges it again at once — one that changes anything "+
				"else, or only the key, does not")
	case pass.Paused:
		add(slog.LevelDebug, "memory_fill_paused", "model", model)
	case err != nil:
		add(slog.LevelWarn, "memory_fill_stopped", "model", model, "error", err,
			"detail", "this tick sends no more requests, for any seat; the next "+
				"tick asks again, and nothing is lost — the rows are selected again")
	}
	return out
}

// batchOf is the embedder in slot as a caller embedding many texts needs it,
// and false when the company configures none or its provider cannot batch.
//
// A PROVIDER THAT CANNOT BATCH DOES NOT FILL, for the corpus duty's reason
// (see [Engine.embedModel]): a seat's whole memory one row per round trip is
// the cost the batch interface exists to avoid, and every provider this build
// ships batches.
func batchOf(slot *embeddings.Embedder) (embeddings.BatchEmbedder, bool) {
	if slot == nil || *slot == nil {
		return nil, false
	}
	batch, ok := (*slot).(embeddings.BatchEmbedder)
	return batch, ok
}
