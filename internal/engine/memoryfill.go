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
// A note or episode is embedded as it is written, but one written while the
// provider did not answer (or answered a vector with no direction), and every
// row written under a model the company has since moved off, has no vector
// recall compares (learning.RecallQuery.Model). Nothing else would give it
// one: the similarity half of `## Personal memory`, `## Similar prior work`
// and `query_episodes` would never reach it, and the 500-entry trim evicts it
// first. An episode's stored ask (node migration 0042) is what lets its vector
// be made again ([learning.Episodes.Unfilled]).

// Only the holder fills, and only once the seat may start a turn
// ([seat.Host.MayStart]), after acquisition hydrated its memory: every other
// node's copy is stale until the next hydration. A filled row takes a fresh
// change sequence, which carries it to the next holder
// (learning.Diary.FillEmbeddings).

// A tick is ONE [embeddings.Pass] over every seat it visits, bounded in what it
// sends: a rate limit, timeout, revoked key or unserved model ends the TICK,
// not the seat, or every held seat would send its full batch into the same
// answer every minute. A refused request is split until its input is alone,
// then held back for [embeddings.RefusalRetry] by a memory kept while the
// provider's configuration is unchanged ([embedConfiguration]); a row answered
// with a directionless vector is held back the same way, or it is paid for
// every tick. A row too long for one tick (an episode's ask is stored whole) is
// sent a request at a time, what each embedded kept in that memory and the rest
// sent next tick, its vector still of its whole text; sent as one call, a row
// that overran the tick kept nothing and stalled the fill for good.

// The fill runs in its own loop, apart from the memory sync, so a slow provider
// cannot widen the sync's crash window ([memorySyncInterval]). A filled row is
// stamped again, so the sync carries it like a new row in the seat's next own
// pass ([memorySync.resume]).

const (
	// memoryFillInterval is how often the holder looks for rows to fill: a
	// minute, the knowledge corpus's cadence (search.EmbedInterval). An idle
	// tick costs one indexed read per table per held seat, and a faster tick
	// fills nothing sooner: the provider is what is slow.
	memoryFillInterval = time.Minute

	// memoryFillPage is how many rows one read of a seat's table takes —
	// one full call of the pass ([embeddings.PassBatch]).
	memoryFillPage = embeddings.PassBatch

	// memoryFillRequestsPerTick bounds the provider requests one node's tick
	// sends, and so its wall clock. Sixteen isolates one refused input in a
	// full call within one tick: 1 + 2·log₂128 = 15, plus the canary that
	// judges a refusal that is the tick's first answer (embeddings.Pass). Per
	// node, since providers limit these models by the token, and sixteen a
	// minute is far inside any published request rate.
	memoryFillRequestsPerTick = 16

	// memoryFillBytesPerTick bounds the prepared bytes the COMPANY's holders
	// send in one tick between them, each node taking an equal share over the
	// live nodes that run seats ([seat.SweepResult.LiveNodes]): a provider
	// limits tokens a minute per account, and every node draws on the one.
	// A million bytes bounds a million tokens (a token is at least one byte,
	// config.EmbeddingModels), OpenAI's lowest paid tier's minute for these
	// models, so a model change alone cannot exhaust it, and fifty seats at the
	// 500-note cap still re-embed in under an hour. A JUDGEMENT CALL trading
	// catch-up time against the account share a model change takes (the corpus
	// duty draws on it too); a tick that meets a rate limit ends, and the next
	// is the retry.
	memoryFillBytesPerTick = 1_000_000
)

// memoryFill is the fill loop, the handle that stops it, and the state its
// ticks share.
type memoryFill struct {
	cancel context.CancelFunc
	done   sync.WaitGroup

	// refusals is what the provider refused, kept across ticks, and
	// refusalsFor the configuration it belongs to ([embedConfiguration], the
	// corpus duty's identity, so the node's two memories move together). A
	// provider configured otherwise starts with none, so a concluded refusal
	// is judged again at once; one an apply merely rebuilt the same (an
	// unrelated change, a rotated key) keeps its held-back rows and pause.
	// Touched only by the loop's goroutine.
	refusals    *embeddings.Refusals
	refusalsFor embedConfiguration

	// resume is the seat the last tick's pass ran out at, where the next
	// tick starts, so a budget spent early never leaves later seats behind.
	resume string
}

// startMemoryFill runs the fill loop until [Engine.stopMemoryFill].
//
// Detached from the signal context, like the memory sync, since the drain
// still releases seats after SIGTERM; but CANCELLED on stop rather than waited
// out: a tick mid-call holds only a provider request, and its transaction
// rolls back whole for a later holder to fill again.
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
// Bounded by the interval: a tick that overruns it has fallen behind whatever
// it does next.
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
	// The seat's derived id, never a roster lookup ([memoryAgentID]): a seat
	// still held after an apply removed it keeps an address for its rows.
	agentIDOf := func(handle string) string { return memoryAgentID(e.Company(), handle) }
	report := fillHeldMemory(ctx, host, memorySources(e.backends.Store, agentIDOf),
		provider, pass, loop.resume)
	loop.resume = report.resume
	report.log(ctx, provider)
}

// memoryFor is the refusal memory a tick embedding with provider sends under:
// the loop's while the provider's configuration is unchanged
// ([embedConfiguration]), else a new one. It is read off the very provider the
// tick embeds with, as [embedDuty.tick] does, so an apply landing between two
// reads cannot pair one provider's refusals with another's.
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
			// Still establishing (memory not hydrated) or no longer
			// renewing: not this tick's to fill.
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
				// A failed read costs this seat's table this tick, not
				// the others'.
				log.WarnContext(ctx, "memory_fill_read_failed", "seat", handle,
					"table", source.table, "error", err)
			}
			if !pass.Open() {
				// Spent or stopped here: the next tick starts at this
				// seat, which may have rows the pass never reached.
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

// reportLine is one line a loop over the seats this node holds says: a fill
// tick's ([fillReport.lines]) or a memory sync cycle's ([syncReport.lines]),
// or a released seat's last memory turn ([flushSeatMemory]).
type reportLine struct {
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
// A value, so a test reads what a tick says rather than the process's logger.
func (r fillReport) lines(provider embeddings.BatchEmbedder) []reportLine {
	model, pass := provider.Model(), r.pass
	var out []reportLine
	add := func(level slog.Level, msg string, args ...any) {
		out = append(out, reportLine{level: level, msg: msg, args: args})
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
	for _, left := range pass.Unfinished {
		add(slog.LevelInfo, "memory_fill_input_continued", "scope", left.Input.Scope,
			"id", left.Input.ID, "model", model, "pieces_embedded", left.Embedded,
			"pieces", left.Pieces, "bytes", left.Bytes,
			"detail", "this row's text is longer than what one tick sends; the pieces "+
				"embedded so far are kept on this node, and the next tick sends the "+
				"rest, from where this one stopped")
	}
	for _, unusable := range pass.Unusable {
		add(slog.LevelWarn, "memory_fill_vector_unusable", "scope", unusable.Input.Scope,
			"id", unusable.Input.ID, "model", model, "bytes", unusable.Bytes,
			"retry", unusable.Retry, "retry_in", embeddings.RefusalRetry.String(),
			"error", unusable.Err,
			"detail", "the provider accepted this row's request and answered its text "+
				"with a vector that has no direction to keep — all zeros or not "+
				"finite, for the text or one piece of it; the row keeps no vector of "+
				"this model and is held back, costing no request, until the retry is "+
				"due, when it is offered again from the piece that had none, and its "+
				"neighbours were filled")
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
		// Filled nothing and named no row, yet had requests refused: it is
		// narrowing multi-row refusals one tick cannot isolate, and silent
		// it would read as a tick that sent nothing.
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
// A provider that cannot batch does not fill, for the corpus duty's reason
// ([Engine.embedModel]); every provider this build ships batches.
func batchOf(slot *embeddings.Embedder) (embeddings.BatchEmbedder, bool) {
	if slot == nil || *slot == nil {
		return nil, false
	}
	batch, ok := (*slot).(embeddings.BatchEmbedder)
	return batch, ok
}
