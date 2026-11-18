package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The fleet's turn-level history, armed: this node answers every peer's
// history questions from its own event store, and asks every live data node
// when it is asked one (ADR-0021).
//
// Every data node answers, in maintenance too: its store alone holds its events
// and the custody batches it keeps for nodes without `data`, so a silent one is
// a gap in every history screen, and maintenance is the window an operator is
// investigating. Answering is not publishing — the scatter rides the broker's
// ephemeral request and reply, writing no stream or record — so the
// maintenance gate has nothing here to stop.

// historyPartial and historyComplete are the history counter's `coverage`
// values, spelled once for the writer and the alarm's reader.
const (
	historyComplete = "complete"
	historyPartial  = "partial"
)

// armHistory builds this node's fleet reader and makes the node an answerer.
//
// Only on a node that holds data. A node without `data` has no history to
// answer from (its store is scratch; custody puts each event it publishes in one
// data node's log, internal/observe), and nothing asks there: the reader's
// callers are the API's history answers and the live projection's boot seed, and
// serving the API takes `data` (config refuses `ingress` without it). A reader
// there would name its empty scratch store as a node every answer was assembled
// from.
func (e *Engine) armHistory(ctx context.Context) error {
	if e.backends == nil || e.backends.Store == nil || e.local == nil {
		return nil
	}
	e.history = &eventfan.Fleet{
		Self:   e.id,
		Local:  e.backends.Store.Events(),
		Queue:  e.backends.Queue,
		Roster: e.dataRoster,
		Report: e.reportHistory,
	}
	if e.backends.Queue == nil {
		// A NODE WITH NO BROKER IS THE FLEET, which is a legal
		// deployment rather than a degradation: its reads are its store.
		return nil
	}
	stop, err := eventfan.Serve(ctx, e.backends.Queue, e.id, e.backends.Store.Events())
	if err != nil {
		return err
	}
	e.stopHistoryServe = stop
	return nil
}

// stopHistory withdraws this node as an answerer. Before the broker closes,
// so a request arriving during the teardown is declined rather than answered
// from a store that is closing.
func (e *Engine) stopHistory(ctx context.Context) {
	if e.stopHistoryServe == nil {
		return
	}
	if err := e.stopHistoryServe(context.WithoutCancel(ctx)); err != nil {
		log.WarnContext(ctx, "history_answerer_not_withdrawn", "error", err)
	}
	e.stopHistoryServe = nil
}

// History is the fleet's turn-level history, read from every live data node —
// what the API answers `events`, `turns`, `trace` and their siblings from. Nil
// on a node without `data`, which serves no API and holds no history (see
// [Engine.armHistory]).
func (e *Engine) History() *eventfan.Fleet { return e.history }

// reportHistory counts what one history answer covered.
//
// THE ONLY THING THAT LETS `history_partial` FIRE: the alarm is a fraction of
// the answers this node gave, and one whose input nobody records stays silent,
// looking exactly like a fleet whose every node always answers.
func (e *Engine) reportHistory(q eventfan.Question, c eventfan.Coverage, _ time.Duration) {
	if e.metrics == nil {
		return
	}
	coverage := historyComplete
	if !c.Complete {
		coverage = historyPartial
	}
	e.metrics.Add(metrics.HistoryAnswers, 1,
		metrics.Attrs{"question": string(q), "coverage": coverage})
}
