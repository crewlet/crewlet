package engine

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/rendezvous"
)

// THE PLAN READER reads every model sign-in's plan with no model call
// ([harness.Provider.ReadPlan]), so a member's state comes from its plan and no
// call is a probe. One node reads each sign-in: the first, in rendezvous order,
// of the live nodes signing it in with a login, and the node ranked k takes
// over once the newest reading is k read intervals old. Readings travel on the
// status records' model limits, which every node keeps ([harness.Plan.Heard]).
// A sign-in is read at once when first seen here and when a call hears a limit.
//
// Who has to agree: nobody. A reading is the vendor's word about an account.

// planReadInterval is how often a sign-in is read: the read's endpoint refused
// a burst past five reads and held one read every 30 s for minutes (measured),
// so one per sign-in every five minutes is a tenth of that, and keeps a reading
// fresh well inside [llm.LimitStaleAfter].
const planReadInterval = 5 * time.Minute

// planReadCeiling bounds the back-off after failed reads: [llm.LimitStaleAfter],
// by when the last reading has gone stale anyway.
const planReadCeiling = llm.LimitStaleAfter

// planReadJitter spreads each sign-in's reads a fifth either side of the
// interval, so the reads of many sign-ins, and of a restarted fleet, do not align.
const planReadJitter = 0.2

// planReadBeat is how often the reader looks at what is due.
const planReadBeat = 15 * time.Second

// planReadHeardGap is the least time between two reads a heard limit asks for,
// so several seats meeting one limit cost one read.
const planReadHeardGap = time.Minute

// planReads is the reader's state and its loop.
type planReads struct {
	mu   sync.Mutex
	rows map[harness.SignInID]*planReadRow

	cancel context.CancelFunc
	done   chan struct{}
}

// planReadRow is what this node keeps of one sign-in's reads.
type planReadRow struct {
	// first is when this node first saw the sign-in, next the earliest it
	// reads it again, asked the last read a heard limit asked for.
	first, next, asked time.Time
	failures           int
	// off is a read that ran a model turn: not read again by this build.
	off bool
}

// startPlanReads reads every sign-in now and keeps them read.
func (e *Engine) startPlanReads(ctx context.Context) {
	r := &e.planReads
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.cancel, r.done = cancel, make(chan struct{})
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(planReadBeat)
		defer ticker.Stop()
		for {
			e.readPlans(loopCtx)
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// stopPlanReads ends the loop and waits for it.
func (e *Engine) stopPlanReads() {
	if e.planReads.cancel == nil {
		return
	}
	e.planReads.cancel()
	<-e.planReads.done
}

// planEntries is one entry per sign-in of the active revision whose plan can be
// read here, the first by key.
func (e *Engine) planEntries() map[harness.SignInID]*harness.Provider {
	out := map[harness.SignInID]*harness.Provider{}
	entries := e.loginEntries()
	for _, key := range slices.Sorted(maps.Keys(entries)) {
		agent := entries[key]
		if id := agent.SignInID(); id != "" && agent.ReadsPlan() && out[id] == nil {
			out[id] = agent
		}
	}
	return out
}

// readPlans takes in what the other nodes read and reads each sign-in that is
// due here.
func (e *Engine) readPlans(ctx context.Context) {
	entries := e.planEntries()
	if len(entries) == 0 {
		e.planReads.mu.Lock()
		e.planReads.rows = nil
		e.planReads.mu.Unlock()
		return
	}
	nodes, err := e.liveModelRows(ctx)
	if err != nil {
		// Read as if alone: a store outage must not stop every node reading.
		nodes = nil
	}
	self := e.selfNode()
	now := e.now()
	r := &e.planReads
	r.mu.Lock()
	if r.rows == nil {
		r.rows = map[harness.SignInID]*planReadRow{}
	}
	for id := range r.rows {
		if entries[id] == nil {
			delete(r.rows, id)
		}
	}
	r.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(entries)) {
		if ctx.Err() != nil {
			return
		}
		agent := entries[id]
		rank := heardFromPeers(agent, id, nodes, self)
		r.mu.Lock()
		row := r.rows[id]
		if row == nil {
			row = &planReadRow{first: now}
			r.rows[id] = row
		}
		due := planReadDue(*row, rank, agent.Plan().LastRead(), agent.Plan().ReadWanted(), now)
		if due && agent.Plan().ReadWanted().After(agent.Plan().LastRead()) {
			row.asked = now
		}
		r.mu.Unlock()
		if due {
			e.readPlan(ctx, id, agent)
		}
	}
}

// heardFromPeers keeps on agent's plan every reading another live node
// published for sign-in id, and answers this node's rank among the nodes that
// read it: those signing it in with a login, this one included.
func heardFromPeers(agent *harness.Provider, id harness.SignInID, nodes map[string][]coord.ModelStatus,
	self string) int {
	short := id.Short()
	readers := []string{self}
	for node, rows := range nodes {
		if node == self {
			continue
		}
		reads := false
		for _, row := range rows {
			if row.Plan != short {
				continue
			}
			agent.Plan().Heard(limitsOf(row.Limits))
			reads = reads || (row.Resolves && row.SignIn == string(harness.RouteCredentials))
		}
		if reads {
			readers = append(readers, node)
		}
	}
	return slices.Index(rendezvous.Order(short, readers), self)
}

// limitsOf is a status record's limits in harness's words.
func limitsOf(in []coord.ModelLimit) []harness.Limit {
	out := make([]harness.Limit, 0, len(in))
	for _, l := range in {
		reading := llm.LimitReading{Window: llm.LimitWindow(l.Window), Hours: l.Hours, Model: l.Model,
			State: llm.LimitState(l.State), Used: l.Used, ResetsAt: l.ResetsAt}
		if !reading.Window.Valid() || !reading.State.Valid() || l.Seen.IsZero() {
			continue
		}
		out = append(out, harness.Limit{LimitReading: reading, Seen: l.Seen, Read: l.Read})
	}
	return out
}

// planReadDue reports whether this node, at rank among a sign-in's readers,
// reads it now: never before row.next, at once for a limit heard since the last
// reading, and otherwise once the newest reading (or this node's first sight
// of the sign-in) is rank intervals old, so rank 0 reads on its own schedule.
func planReadDue(row planReadRow, rank int, newest, wanted, now time.Time) bool {
	if row.off || rank < 0 || now.Before(row.next) {
		return false
	}
	if wanted.After(newest) && now.Sub(row.asked) >= planReadHeardGap {
		return true
	}
	since := row.first
	if newest.After(since) {
		since = newest
	}
	return rank == 0 || now.Sub(since) >= time.Duration(rank)*planReadInterval
}

// readPlan reads one sign-in now and schedules its next read: a jittered
// interval after a reading, a doubling back-off after a failure.
func (e *Engine) readPlan(ctx context.Context, id harness.SignInID, agent *harness.Provider) {
	before, wasOut := agent.SitOut()
	readings, err := agent.ReadPlan(ctx)
	now := e.now()
	r := &e.planReads
	r.mu.Lock()
	row := r.rows[id]
	if row == nil {
		row = &planReadRow{first: now}
		r.rows[id] = row
	}
	switch {
	case err == nil:
		row.failures = 0
		row.next = now.Add(backoff.Jitter(planReadInterval, planReadJitter))
	case errors.Is(err, harness.ErrPlanReadRanModel):
		row.off = true
	default:
		row.failures++
		row.next = now.Add(backoff.Doubling(row.failures, planReadInterval, planReadCeiling))
	}
	next := row.next
	r.mu.Unlock()
	if err != nil {
		log.WarnContext(ctx, "llm_plan_read_failed", "provider", agent.Key(), "plan", id.Short(),
			"error", err.Error(), "next", next.UTC().Format(time.RFC3339),
			"detail", "a failed read changes nothing: the sign-in keeps its last reading, and a "+
				"sit-out runs to its reset")
		return
	}
	after, isOut := agent.SitOut()
	switch {
	case isOut && (!wasOut || !after.Until.Equal(before.Until)):
		log.InfoContext(ctx, "llm_plan_read_sat_out", "provider", agent.Key(), "plan", id.Short(),
			"window", string(after.Window), "until", after.Until.UTC().Format(time.RFC3339))
	case wasOut && !isOut:
		log.InfoContext(ctx, "llm_plan_read_back", "provider", agent.Key(), "plan", id.Short(),
			"window", string(before.Window))
	default:
		log.DebugContext(ctx, "llm_plan_read", "provider", agent.Key(), "plan", id.Short(),
			"windows", len(readings))
	}
}
