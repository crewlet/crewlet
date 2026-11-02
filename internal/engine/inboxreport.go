package engine

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// WHAT WAITS IN A SEAT'S INBOX, AS THE NODE HOLDING IT COUNTS.
//
// A seat parked on a provider limit, or held by its executor, can have a
// hundred wakes behind it and look idle. The count is read by the node that
// holds the seat, because only that node can leave out the mail its own turn
// is running on ([queue.EventQueue.Waiting]); the dashboard may be served by
// any node. Each node reports the seats it holds as one stream-only snapshot
// ([types.SeatInboxWaiting]), which the live projection folds per seat.

const (
	// InboxReportInterval is how often a node reads its seats' waiting
	// mail: the cadence the live projection already refreshes placement
	// on, so a count and the seat's state move together on Live.
	InboxReportInterval = 5 * time.Second

	// InboxReportHeartbeat is how often an unchanged report is published
	// again, so a dashboard that started since hears every seat within it;
	// the projection's expiry is read off the same constant.
	InboxReportHeartbeat = types.SeatInboxWaitingHeartbeat

	// inboxReadConcurrency caps the reads one tick has in flight: each is
	// two consumer-info requests to the broker, and a node holding thirty
	// seats would otherwise put sixty on the broker at once every tick.
	inboxReadConcurrency = 4

	// inboxReadBudget is one tick's deadline for all of its reads, under
	// the interval so a slow broker cannot stack one tick on the next.
	inboxReadBudget = 4 * time.Second
)

// inboxReporter is one node's waiting-mail loop.
type inboxReporter struct {
	engine *Engine

	// seq is the reorder guard within this incarnation's reports.
	seq atomic.Int64

	// last is each held seat's last count read, and sent what the last
	// published report said. A read that fails keeps last, so a broker
	// that did not answer leaves a seat's count as it was rather than
	// reporting it empty.
	last     map[uuid.UUID]queue.Waiting
	sent     map[uuid.UUID]queue.Waiting
	sentAt   time.Time
	reported bool

	stop context.CancelFunc
	done chan struct{}
}

// startInboxReports arms the loop on this node. Every node runs one, over the
// seats it holds; a node holding none publishes nothing.
func (e *Engine) startInboxReports(ctx context.Context) {
	if e.backends == nil || e.backends.Queue == nil || e.node == nil {
		return
	}
	r := &inboxReporter{engine: e, done: make(chan struct{})}
	// DETACHED, like every long-running loop here: bound to a signal
	// context it would stop at SIGTERM, while the drain still holds seats.
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	r.stop = stop
	e.inboxReports = r
	go r.run(loop)
}

// stopInboxReports ends the loop, waiting for an in-flight tick.
func (e *Engine) stopInboxReports() {
	if e.inboxReports == nil {
		return
	}
	e.inboxReports.stop()
	<-e.inboxReports.done
	e.inboxReports = nil
}

func (r *inboxReporter) run(ctx context.Context) {
	defer close(r.done)
	tick := time.NewTicker(InboxReportInterval)
	defer tick.Stop()
	for {
		r.tick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// tick reads every held seat and publishes when a count moved or the heartbeat
// is due.
func (r *inboxReporter) tick(ctx context.Context, now time.Time) {
	e := r.engine
	held := e.node.AttachedSeats()
	if len(held) == 0 && !r.reported {
		return
	}
	read := r.read(ctx, e.backends.Queue, held)
	if !r.due(read, now) {
		return
	}
	report := types.SeatInboxWaiting{
		// THE INCARNATION, for the reason [types.BudgetMeters.MeterID]
		// gives: a restarted node's reports are a reporter of their own.
		ReporterID: e.node.Owner(),
		Seq:        int(r.seq.Add(1)),
		Seats:      make([]types.SeatWaiting, 0, len(read)),
	}
	for _, id := range slices.SortedFunc(maps.Keys(read), uuidCompare) {
		w := read[id]
		report.Seats = append(report.Seats, types.SeatWaiting{
			AgentID: id.String(), Priority: w.Priority, Background: w.Background})
	}
	ev := events.New(report, tracing.TraceOf(ctx))
	ev.Source = e.node.ID()
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		if ctx.Err() == nil && !strings.Contains(err.Error(), "context canceled") {
			log.DebugContext(ctx, "inbox_report_not_published", "error", err.Error(),
				"detail", "the live counts keep the last report they had")
		}
		return
	}
	r.sent, r.sentAt, r.reported = read, now, len(read) > 0
}

// read is the count of every held seat, read with at most
// [inboxReadConcurrency] in flight under one [inboxReadBudget]. A seat whose
// read failed keeps its last count, and one never read successfully is left
// out rather than reported empty.
func (r *inboxReporter) read(ctx context.Context, q queue.EventQueue, held []uuid.UUID) map[uuid.UUID]queue.Waiting {
	readCtx, cancel := context.WithTimeout(ctx, inboxReadBudget)
	defer cancel()
	var (
		mu  sync.Mutex
		out = make(map[uuid.UUID]queue.Waiting, len(held))
		sem = make(chan struct{}, inboxReadConcurrency)
		wg  sync.WaitGroup
	)
	for _, id := range held {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-readCtx.Done():
				r.keep(&mu, out, id)
				return
			}
			defer func() { <-sem }()
			w, err := q.Waiting(readCtx, topics.AgentInbox(id), topics.AgentInboxGroup(id))
			if err != nil {
				log.DebugContext(ctx, "inbox_count_unread", "agent_id", id.String(),
					"error", err.Error(), "detail", "the seat keeps its last count")
				r.keep(&mu, out, id)
				return
			}
			mu.Lock()
			out[id] = w
			mu.Unlock()
		})
	}
	wg.Wait()
	r.last = out
	return out
}

// keep carries a seat's last count into this tick's read.
func (r *inboxReporter) keep(mu *sync.Mutex, out map[uuid.UUID]queue.Waiting, id uuid.UUID) {
	mu.Lock()
	defer mu.Unlock()
	if w, ok := r.last[id]; ok {
		out[id] = w
	}
}

// due reports whether this tick's read is worth a report: a count moved, a seat
// came or went, or the heartbeat is due.
func (r *inboxReporter) due(read map[uuid.UUID]queue.Waiting, now time.Time) bool {
	if !r.reported && len(read) == 0 {
		return false
	}
	return !maps.Equal(read, r.sent) || now.Sub(r.sentAt) >= InboxReportHeartbeat
}

// uuidCompare orders seat ids, so one node's reports list its seats in one
// order.
func uuidCompare(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) }
