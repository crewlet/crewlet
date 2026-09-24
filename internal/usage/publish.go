package usage

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The PUBLISHER: one per node, publishing that node's own days and nothing
// else.
//
// # Not a duty
//
// Every other writer of a derived domain in this engine is a fleet singleton,
// because every node could compute the same value and paying for it N times
// is waste. That is false here: each node's event log holds only what ITS
// seats did, so each node is the only one that can derive its own day. The
// node in the subject is what makes N writers safe — no two nodes ever
// publish on one subject — and it is why a node that leaves the fleet takes
// nothing with it: what it published stays on the stream and in every peer's
// rows for [History].
//
// # What a tick does
//
// Every [FlushInterval] it fingerprints today and yesterday — yesterday
// because a turn that ended at 23:59:58 is written a moment after midnight —
// and re-derives only a day whose fingerprint moved. Each derived object is
// compared with what this process last published for it, and only an object
// whose content changed is published. A record carries the object's whole
// cumulative value, so a publish that failed is simply repeated by the next
// tick that finds the day still unconfirmed.
//
// # And at boot
//
// A fresh process has published nothing, so its first tick re-derives and
// republishes today and yesterday in full. That is what covers a node that was
// down across midnight: the day it was in when it stopped is yesterday when it
// comes back, and its last flush of that day may never have happened. A
// republish of an unchanged object is harmless twice over — the op id is the
// content's digest, so the broker's duplicate window collapses it, and past
// the window the apply replaces the rows with the same rows.

// Store is this node's records, as the publisher reads them. Satisfied by
// [store.DB].
type Store interface {
	UsageMark(ctx context.Context, w store.UsageWindow) (store.UsageMark, error)
	UsageForDay(ctx context.Context, w store.UsageWindow) (store.UsageDay, error)
}

// Log is the domain's write authority, as the publisher uses it. Satisfied
// by [statelog.Publisher].
type Log interface {
	Publish(ctx context.Context, req statelog.Request) (statelog.Result, error)
}

// PublisherDeps is everything a publisher needs that it does not own.
type PublisherDeps struct {
	Store Store
	Log   Log

	// NodeID is this node's id: the node in every subject it publishes.
	NodeID string

	// Zone is the company's clock, read on every tick because a config
	// apply can move it. A day is cut on it.
	Zone func() *time.Location

	// Handle names a seat by its id from the organisation this node is
	// running, and false when it has none — a seat removed since, whose
	// day's own records are then the only name there is. Nil answers
	// false for every seat.
	Handle func(agentID string) (string, bool)

	// Now is the clock. Nil is [time.Now].
	Now func() time.Time

	Logger *slog.Logger
}

// Publisher is one node's usage publisher.
type Publisher struct {
	deps PublisherDeps

	// mu serialises ticks: the loop runs one, and a test or a shutdown
	// flush may run another.
	mu sync.Mutex

	// marks is the fingerprint each day had when it was last derived and
	// every one of its objects was confirmed published.
	marks map[string]store.UsageMark

	// published is the op id — the content digest — this process last
	// published for each object, keyed by the object's subject.
	published map[string]string
}

// NewPublisher builds a node's publisher, refusing a dependency set that
// could not publish a correct record.
func NewPublisher(d PublisherDeps) (*Publisher, error) {
	switch {
	case d.Store == nil:
		return nil, fmt.Errorf("usage: a publisher needs this node's store")
	case d.Log == nil:
		return nil, fmt.Errorf("usage: a publisher needs the domain's write authority")
	case strings.TrimSpace(d.NodeID) == "":
		return nil, fmt.Errorf("usage: a publisher needs this node's id — it is " +
			"half of every subject it writes and the only writer allowed on them")
	case d.Zone == nil:
		return nil, fmt.Errorf("usage: a publisher needs the company's clock, " +
			"which is what a day is cut on")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &Publisher{deps: d, marks: map[string]store.UsageMark{},
		published: map[string]string{}}, nil
}

// Run flushes at once and then every [FlushInterval] until ctx ends.
func (p *Publisher) Run(ctx context.Context) {
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()
	for {
		if err := p.Flush(ctx); err != nil && ctx.Err() == nil {
			p.deps.Logger.WarnContext(ctx, "usage_flush_failed", "err", err,
				"detail", "the day is re-derived and republished on the next tick")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Flush is one tick: re-derive what moved, publish what changed.
func (p *Publisher) Flush(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	loc := p.deps.Zone()
	today := period.At(period.Day, p.deps.Now(), loc)
	days := []period.Window{today.Shift(-1), today}

	// A DAY OLDER THAN YESTERDAY WILL NEVER BE LOOKED AT AGAIN by this
	// process, so what it remembers about one is dropped — which is what
	// bounds both maps to two days of objects.
	keep := map[string]bool{days[0].Label: true, days[1].Label: true}
	for day := range p.marks {
		if !keep[day] {
			delete(p.marks, day)
		}
	}
	for key := range p.published {
		if day, _, _ := strings.Cut(key, "\x00"); !keep[day] {
			delete(p.published, key)
		}
	}

	var failed error
	for _, day := range days {
		if err := p.flushDay(ctx, day); err != nil {
			failed = err
		}
	}
	return failed
}

// flushDay re-derives one day if it moved and publishes each object that
// changed.
func (p *Publisher) flushDay(ctx context.Context, day period.Window) error {
	w := store.UsageWindow{Start: day.Start, End: day.End,
		Accepted: string(phase.Done), SentBack: string(phase.SelfIterate)}
	mark, err := p.deps.Store.UsageMark(ctx, w)
	if err != nil {
		return fmt.Errorf("usage: fingerprint %s: %w", day.Label, err)
	}
	if last, seen := p.marks[day.Label]; seen && last == mark {
		return nil
	}
	derived, err := p.deps.Store.UsageForDay(ctx, w)
	if err != nil {
		return fmt.Errorf("usage: derive %s: %w", day.Label, err)
	}
	records, err := p.Records(day.Label, derived)
	if err != nil {
		return err
	}
	var failed error
	settled := true
	for _, rec := range records {
		sent, err := p.publish(ctx, day.Label, rec)
		if err != nil {
			failed = err
		}
		settled = settled && sent
	}
	if !settled {
		// THE MARK IS NOT KEPT, so the next tick derives this day again
		// and retries whatever did not land. An object that did land is
		// remembered by its digest and is not sent twice.
		return failed
	}
	p.marks[day.Label] = mark
	return nil
}

// Records turns one derived day into this node's records for it, in subject
// order.
//
// DETERMINISTIC, which is the property everything above leans on: an
// unchanged day yields byte-identical records, so its digest is unchanged and
// nothing is published.
func (p *Publisher) Records(day string, d store.UsageDay) ([]Record, error) {
	var out []Record
	for _, s := range d.Seats {
		rec := Record{
			RecordEnvelope: RecordEnvelope{
				Subject: Subject{Kind: KindSeat, Node: p.deps.NodeID, Day: day, Seat: s.AgentID},
				Writer:  p.deps.NodeID,
			},
			Handle: s.Handle,
			Role:   s.Role,
			Turns: &Turns{
				Count: s.Turns.Count, Failed: s.Turns.Failed,
				Reviewed: s.Turns.Reviewed, FirstPass: s.Turns.FirstPass,
				SentBack: s.Turns.SentBack, LastEndedAt: s.Turns.LastEndedAt,
			},
		}
		if p.deps.Handle != nil {
			// THE ORGANISATION NAMES A SEAT IT STILL HOLDS, which is
			// the name every reader resolves a handle filter against;
			// the day's own records are the fallback for a seat that
			// has since left it.
			if handle, ok := p.deps.Handle(s.AgentID); ok && handle != "" {
				rec.Handle = handle
			}
		}
		for _, d := range s.Turns.Durations {
			rec.Turns.Durations.Add(d)
		}
		for _, t := range s.Tokens {
			rec.Tokens = append(rec.Tokens, Tokens{
				Phase: t.Phase, Worker: t.Worker, Model: t.Model,
				ProviderKey: t.ProviderKey, Input: t.Input, Output: t.Output,
				CacheRead: t.CacheRead, CacheWrite: t.CacheWrite, Total: t.Total,
				Calls: t.Calls,
			})
		}
		rec.Reads, rec.ReadsElided = capReads(s.Reads)
		out = append(out, rec)
	}
	for _, s := range d.Schedules {
		rec := Record{
			RecordEnvelope: RecordEnvelope{
				Subject: Subject{Kind: KindSchedule, Node: p.deps.NodeID, Day: day,
					ScopeType: s.ScopeType, ScopeID: s.ScopeID, Schedule: s.Name},
				Writer: p.deps.NodeID,
			},
		}
		for _, f := range s.Fires {
			rec.Fires = append(rec.Fires, Fire{At: f.At, Target: f.Target,
				Outcome: f.Outcome, TraceID: f.TraceID, TurnID: f.TurnID})
		}
		out = append(out, rec)
	}
	for i := range out {
		if err := out[i].Subject.Validate(); err != nil {
			return nil, fmt.Errorf("usage: this node derived an object it cannot "+
				"address on %s: %w", day, err)
		}
	}
	return out, nil
}

// capReads keeps the [ReadsPerSeatDay] most-read entries and counts the rest.
//
// THE MOST-READ SURVIVE, the newest breaking a tie, because the question the
// entries answer is "what does this seat lean on" — and the survivors are then
// put back in key order, so a record's bytes do not move when two entries
// swap rank.
func capReads(in []store.UsageRead) ([]Read, int) {
	reads := make([]Read, 0, len(in))
	for _, r := range in {
		reads = append(reads, Read{PageID: r.PageID, Backend: r.Backend, Via: r.Via,
			Count: r.Count, LastAt: r.LastAt, LastTurnID: r.LastTurnID,
			LastWorkKey: r.LastWorkKey, LastQuery: r.LastQuery})
	}
	elided := 0
	if len(reads) > ReadsPerSeatDay {
		slices.SortStableFunc(reads, func(a, b Read) int {
			if c := cmp.Compare(b.Count, a.Count); c != 0 {
				return c
			}
			if c := b.LastAt.Compare(a.LastAt); c != 0 {
				return c
			}
			return compareReadKey(a, b)
		})
		elided = len(reads) - ReadsPerSeatDay
		reads = reads[:ReadsPerSeatDay]
	}
	slices.SortFunc(reads, compareReadKey)
	return reads, elided
}

func compareReadKey(a, b Read) int {
	return cmp.Or(cmp.Compare(a.Backend, b.Backend), cmp.Compare(a.PageID, b.PageID),
		cmp.Compare(a.Via, b.Via))
}

// publish sends one object if its content changed since this process last
// did, reporting whether the object is settled — published and confirmed, or
// unchanged — so the day's fingerprint may be kept.
func (p *Publisher) publish(ctx context.Context, day string, rec Record) (bool, error) {
	body, err := rec.Encode()
	if err != nil {
		return false, fmt.Errorf("usage: encode %s: %w", rec.Subject, err)
	}
	// THE OP ID IS THE CONTENT'S DIGEST, taken over the record before the
	// id is stamped on it: an unchanged object carries the same id on
	// every derivation, which is what lets the broker collapse a repeat,
	// and a changed one carries a new id, which is what stops the broker
	// collapsing a real update into the one before it.
	sum := sha256.Sum256(body)
	opID := "usage-" + hex.EncodeToString(sum[:16])
	key := day + "\x00" + rec.Subject.String()
	if p.published[key] == opID {
		return true, nil
	}
	rec.OpID = opID
	if body, err = rec.Encode(); err != nil {
		return false, fmt.Errorf("usage: encode %s: %w", rec.Subject, err)
	}
	env, err := Domain{}.Envelope(body)
	if err != nil {
		return false, fmt.Errorf("usage: the record this node just wrote for %s "+
			"does not decode: %w", rec.Subject, err)
	}
	res, err := p.deps.Log.Publish(ctx, statelog.Request{
		Subject:  env.Subject,
		Scope:    env.Scope,
		OpID:     opID,
		MintedAt: p.deps.Now().UTC(),
		// ADDITIVE: exactly one writer publishes on this subject — this
		// node — so there is nothing to arbitrate, and the apply's
		// position guard is what makes a redelivery harmless.
		Pattern: statelog.PatternAdditive,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return statelog.Decision{Payload: body, Envelope: env}, nil
		},
	})
	if err != nil {
		return false, fmt.Errorf("usage: publish %s: %w", rec.Subject, err)
	}
	if res.Outcome == statelog.OutcomeUnknown {
		// UNRESOLVED IS NOT SENT. The object's day is derived again next
		// tick and the same bytes go out under the same op id: inside
		// the broker's duplicate window a copy that did land collapses,
		// and past it the apply replaces the rows with the same rows.
		// Counting it as sent instead would lose the day's last change
		// for good whenever nothing moved after it — which is exactly
		// the end of every day.
		p.deps.Logger.InfoContext(ctx, "usage_publish_unresolved",
			"subject", rec.Subject.String(), "op_id", opID)
		return false, nil
	}
	p.published[key] = opID
	return true, nil
}
