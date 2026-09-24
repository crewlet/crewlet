package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The fleet's turn-level history, armed: this node answers every peer's
// history questions from its own event store, and asks every live peer when it
// is asked one (ADR-0021).
//
// # Every node serves, in every mode
//
// A node's events are in its own store and nowhere else, so a node that stopped
// answering would be a gap in every history screen of the company. That
// includes a node in maintenance: it publishes nothing, but it still holds
// everything it published, and the window an operator is investigating is
// exactly the one it must not go dark for. Answering is not publishing — the
// scatter rides the broker's ephemeral request and reply, which write no
// stream and no record — so the maintenance gate, which stops every publisher,
// has nothing here to stop.

// historyPartial and historyComplete are the two values of the history
// counter's `coverage` attribute — a closed set, spelled once for the writer
// and the alarm's reader.
const (
	historyComplete = "complete"
	historyPartial  = "partial"
)

// armHistory builds this node's fleet reader and makes the node an answerer.
func (e *Engine) armHistory(ctx context.Context) error {
	if e.backends == nil || e.backends.Store == nil {
		return nil
	}
	e.history = &eventfan.Fleet{
		Self:   e.id,
		Local:  e.backends.Store.Events(),
		Queue:  e.backends.Queue,
		Roster: e.liveNodes,
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

// History is the fleet's turn-level history, read from every live node — what
// the API answers `events`, `turns`, `trace` and their siblings from.
func (e *Engine) History() *eventfan.Fleet { return e.history }

// reportHistory counts what one history answer covered.
//
// THE ONLY THING THAT LETS `history_partial` FIRE. The alarm is a fraction of
// the answers this node gave, and an alarm whose input nobody records is
// permanently silent — which looks exactly like a fleet whose every node
// always answers.
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
