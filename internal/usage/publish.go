package usage

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The PUBLISHER: one per data node, publishing that node's own days and
// nothing else. A node without `data` runs none: it keeps no event log, and
// its events are counted in the day of the data node that keeps them in
// custody (see the package doc).
//
// # Not a duty
//
// Every other writer of a derived domain in this engine is a fleet singleton,
// because every node could compute the same value and paying for it N times
// is waste. That is false here: a data node's event log holds only what its
// own seats did and what it keeps in custody for nodes without `data`, so
// each data node is the only one that can derive its day. The node in the
// subject is what makes N writers safe — no two nodes ever publish on one
// subject — and it is why a node that leaves the fleet takes nothing with it:
// what it published stays on the stream and in every peer's rows for
// [History].
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
// # And every older day whose kept rows changed
//
// A day counts the rows this node KEEPS ([store.DB.UsageForDay]): a custody
// batch it has written and not settled may be another data node's, and
// counted on both, every reader summing the fleet would bill it twice. So a
// batch settled as kept makes rows count on the days they fall on — and only
// on those, since every count of a day is read off rows inside it, a turn's
// off the one record that ends it (a turn whose end states no tally — ended by
// a build before the tally, or resumed from such a build's park — is read off
// its records, which reach back to wherever the turn started; see
// [store.DB.UsageForDay]) — and a batch can carry a stateless
// node's events from a partition that outlasted midnight by days. On today or
// yesterday the settle moves the fingerprint. On an older day it is the mark
// the settle leaves in its own transaction ([store.DB.UsageRederive]): each
// tick reads the marks first and derives every day from the oldest through the
// day before yesterday IN FULL, without a fingerprint, publishing each object
// whose content differs from what this process last published for it. Once
// every day the tick derived settled — published, or refused for good and
// said — it clears the marks up to the newest it read, so a settle that lands
// during the pass is stamped above it, survives the clear, and is derived on
// the next tick.
//
// # And every day it owes, across a restart
//
// A day this process could not settle — an unresolved publish of yesterday's
// last change — is derived again on later ticks however far the window has
// moved, and so is the day the node stopped in: the tail of it written after
// the last tick is in no published record. Both are held as ONE WATERMARK the
// store keeps ([store.DB.UsageWatermark]): after every tick, the start of
// yesterday, or of an older day still owed if there is one, written when it
// moves — about once a day. A fresh process reads it before anything else and
// derives every day from it again. That covers a crash, a long outage and a
// stop whose last flush failed, as far back as the floor below: a node down
// for longer than the event log is read back gets back nothing older than
// that, and a day older than the dispatch ledger's sweep without its
// schedules. Before the watermark a start re-derived only today and
// yesterday, so a node down across two midnights never published the tail of
// the day it stopped in; and the node's NEWEST ROW would not stand in for it,
// since the first custody batch the keeper writes at boot — or the first fire
// the scheduler's catch-up records — makes that today.
//
// # A day keeps the cut it was first given
//
// A record names its day by label alone, and a label is cut into instants on
// the company's clock, which an apply can move. A day derived again — an older
// one a mark or the watermark names, or one of the two a tick fingerprints at
// the tick the clock moves — keeps the instants its first derivation was cut
// on ([store.DB.PinUsageDay]): recut on the clock in force, a label replaced
// the record its old cut published while the objects the new cut no longer
// reached kept theirs, so a row either side of midnight counted in two
// published days or in none. A day cut for the first time abuts the cuts of
// the days either side of it, and is clamped by any farther recorded cut it
// would reach into, so the day a clock moved on is longer or shorter by the
// move and every instant is in exactly one day ([Publisher.windowOf]). The day
// an instant is in is the recorded cut that holds it, or the label next to its
// own whose first cut does ([Publisher.dayAt]) — so for the hours between the
// old clock's midnight and the new one's the label runs one ahead of the
// company's local date after a move west and one behind after a move east,
// and up to two either way after a move of more than a day, which leaves a
// label in between with no instants (east) or cuts a first day more than two
// days long (west).
//
// # Never below what the fleet holds, and not before the applier has caught up
//
// Inside the floor below a day only GROWS: a kept settle adds rows, a late row
// of this node's own adds one, a settle as not kept deletes rows no count
// held, a settled batch never goes back in flight, and a turn whose end states
// no tally gains segments rather than losing them. So a derivation that comes
// out SMALLER than what the fleet holds of the day in any figure proves the
// event log has lost rows that record counted: a store restored beside a usage
// log copied later (a backup takes the store first, and a fleet restore puts
// one node's store beside another's streams), one started empty, or an
// untallied turn whose earliest records the event log's sweep already removed.
// Published, the short record would replace the day on every node with what is
// left — and that is as true of today and yesterday, which a restore copied
// this morning most often names, as of any older day: the restored log never
// gets back the rows it lacks.
//
// So EVERY publish is the derivation JOINED with what the fleet holds of the
// object ([Applied], [Join]): figure by figure, the larger of the two. On the
// ordinary day the fleet's record is an earlier derivation of the same rows
// and the join is the derivation itself, byte for byte; after a loss it raises
// exactly the figures the fleet held more of and lets every other move — a
// day a restore cost one failed turn still publishes every later turn and its
// spend, where holding the short derivation back whole froze them all. What a
// join cannot do is recover what was lost: a figure the fleet held more of
// counts the node's new rows only once its own count passes the fleet's. That
// a derivation came out short is said once per run of short derivations of an
// object, as `usage_rederive_short` ([Publisher.noteShort]). The other side of
// the same rule is that a figure this node once published TOO HIGH is never
// lowered below it — which only the build before the kept-rows rule did, by
// counting a custody batch another node kept.
//
// What the fleet holds is read off the rows this node's applier wrote, so NO
// day — today and yesterday included — is derived until that applier has
// reached the end of the log since it started ([PublisherDeps.Caught]): right
// after a restore its rows are the copy's, older than what this node published
// after the copy was taken, and a short derivation joined with them would
// still replace the fleet's. Until then every day stays owed — an older one by
// the bound, the marks and the watermark, and today and yesterday as days the
// tick could not settle, which lowers the bound to them, so a node still
// catching up across a midnight does not lose the day it stopped in.
//
// # Never past the event log's floor
//
// A day is re-derived only while every row it was derived from is still held.
// The event log is read back [store.EventHistory] — its rows are swept a day
// after that — so a day that STARTS before now − EventHistory is never derived
// again: its rows may be partly gone, and the record a short derivation made
// would REPLACE the whole day every peer holds with less. Such a day keeps the
// value it was last published at; the tick says so once
// (`usage_rederive_past_floor`). Every row a day's counts are derived from
// falls inside the day, and a fire's turn label comes from its trace, which
// starts at the fire, so the floor is exact for them. The rows that can fall
// outside are the records of a turn whose end states no tally, read off its
// records — and those reach back to wherever the turn STARTED, which nothing
// bounds: a park lasts as long as its coding run, which is never stopped on a
// timer, or as long as a person takes to answer. So a day inside the floor can
// hold such a turn whose earliest records the sweep has removed, and derived
// again it reads that turn shorter, with fewer reviews; the join holds every
// figure of it at what the fleet was published (see above), and says so.
// The dispatch ledger keeps a fire for less than that
// ([PublisherDeps.FireHistory]), so a day older than IT is derived again
// without its schedules: a kept settle moves no schedule-day — a fire's label
// is read off rows in flight as well as kept ones, so it is set when the batch
// is written — and derived from a ledger that has swept part of the day it
// would shrink.
//
// # A refusal no retry changes is said once
//
// Two refusals are not a moment's trouble: a record larger than the log takes
// (`record_too_large`), which re-deriving the same content reproduces byte for
// byte, and a log at its byte ceiling (`log_full`), which only an operator's
// gesture or the horizon ageing a day out makes room in. Retried every tick,
// each would put the same warning and the same refused append on the log every
// fifteen seconds for as long as it lasted — on every node at once for a full
// log. So the object is logged ONCE, as `usage_publish_refused`, and skipped
// until its content changes, which is what makes a new record that might fit.
// A full log is a property of the LOG rather than of the object, though, so the
// first append that lands a subject NEW TO THE LOG afterwards — proof the log
// has room again — puts every object skipped for it back in line, and its day
// is derived again on the next tick: otherwise yesterday's last value, which
// nothing will ever change, would stay off the fleet until this process
// restarted. Whether a subject is new is asked of the replicated rows — the read
// of what the fleet holds that every publish takes first ([Applied]) — before
// the append, never of this process's memory: after a restart, or on an
// older day this process has not derived yet, every subject is one it has not
// published, and taken for new each would reopen every skip and have it
// refused, and said, again.
//
// # What this process remembers
//
// Each object's last published derivation, last refusal and whether its last
// derivation was short, for every day the floor still lets it derive — not
// only the two it fingerprints: an older day a mark names is derived in full,
// and remembered, its pass publishes only the objects whose derivation moved
// rather than every object of the day. An object whose derivation is unchanged
// costs no read of what the fleet holds. A fresh process remembers nothing,
// so its first pass over a day republishes it whole — joined, which is the
// fleet's own record wherever the derivation adds nothing — and that is
// harmless twice over: the op id is the content's digest, so the broker's
// duplicate window collapses a repeat, and past the window the apply replaces
// the rows with the same rows.
//
// # And at the stop
//
// An orderly stop flushes once more as [Publisher.Run] returns: today and
// yesterday, so the end of the day a node stops in is published even if it
// never starts again. That flush runs on a context of its own, the loop's
// having just ended, bounded by [FinalFlushBudget], and derives nothing older —
// what an older day is owed stays in the marks and the watermark for the next
// start.

// Store is this node's records, as the publisher reads them. Satisfied by
// [store.DB].
type Store interface {
	UsageMark(ctx context.Context, w store.UsageWindow) (store.UsageMark, error)
	UsageForDay(ctx context.Context, w store.UsageWindow) (store.UsageDay, error)

	// UsageRederive and ClearUsageRederive are the marks kept custody
	// settles leave: the oldest day they changed, and a clear of every mark
	// up to the newest the publisher acted on.
	UsageRederive(ctx context.Context) (store.UsageRederive, bool, error)
	ClearUsageRederive(ctx context.Context, seq int64) (bool, error)

	// UsageWatermark and SetUsageWatermark are the oldest day this
	// publisher owes a derivation of on its own account, kept across a
	// restart: the boot derives every day from it again.
	UsageWatermark(ctx context.Context) (time.Time, bool, error)
	SetUsageWatermark(ctx context.Context, since time.Time) error

	// UsageDayWindow, UsageDayContaining and PinUsageDay are the cut each
	// of this node's days was first given, which every later derivation of
	// the day keeps whatever the company's clock does since.
	UsageDayWindow(ctx context.Context, label string) (store.UsageWindow, bool, error)
	UsageDayContaining(ctx context.Context, t time.Time) (string, store.UsageWindow, bool, error)
	PinUsageDay(ctx context.Context, label string, w store.UsageWindow) (store.UsageWindow, error)
}

// FinalFlushBudget is how long the flush an orderly stop runs as
// [Publisher.Run] returns may take.
//
// THE STATE LOG'S OWN RESOLVE BUDGET ([statelog.DefaultResolveBudget]): the
// longest the log lets ONE write wait for this node's applier before it
// answers pending, and the line past which it calls the node one with a fault
// a caller cannot fix by waiting. The stop takes this flush before the trim
// and the logs come down (internal/engine's teardown), so its applier is
// running, and the flush is two fingerprints and the derivation and publish
// of whichever of today and yesterday moved since the last tick — it derives
// nothing older, whatever the marks name — so a healthy one needs a small
// fraction of the budget. One that has not finished inside it is meeting a
// log no stop can wait out. A node whose applier had not caught up with the
// usage log derives nothing at all ([PublisherDeps.Caught]), and its watermark
// keeps both days owed. What it did not send is published by this node's
// next start, whose watermark names the day it stopped in; it is lost only
// to a node that never starts again, or one that starts again after the event
// log's floor has passed that day.
const FinalFlushBudget = statelog.DefaultResolveBudget

// Log is the domain's write authority, as the publisher uses it. Satisfied
// by [statelog.Publisher].
type Log interface {
	Publish(ctx context.Context, req statelog.Request) (statelog.Result, error)
}

// PublisherDeps is everything a publisher needs that it does not own.
type PublisherDeps struct {
	Store Store
	Log   Log

	// Estate is the replicated estate this node's applier writes the
	// domain's rows into, which the publisher reads what the fleet holds of
	// an object from before every publish ([Applied]) — the record it joins
	// its derivation with, and whether the subject is new to the log.
	Estate Estate

	// NodeID is this node's id: the node in every subject it publishes.
	NodeID string

	// Caught reports whether this node's applier of the usage log has
	// reached the end of the log since it started
	// ([statelog.Runner.Drained]). No day is derived until it has, today
	// and yesterday included: what a derivation is published as is its join
	// with the rows the applier wrote ([Applied], [Join]), and before it has
	// replayed the log those are whatever a restore brought back — older
	// than records this node published after the copy was taken. Required.
	Caught func() bool

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

	// FireHistory is how long this node's dispatch ledger keeps a fire —
	// the maintenance sweep's retention for `scheduled_runs`
	// (maintenance.ScheduledRunRetention), which the engine hands in. A day
	// that starts before now − FireHistory is derived again without its
	// schedules: their fires may already be swept, and a schedule-day
	// derived from what is left would replace the whole one every peer
	// holds. Required: zero would say the ledger keeps nothing, which no
	// deployment's does.
	FireHistory time.Duration

	// Every is the cadence [Publisher.Run] ticks on. Zero is
	// [FlushInterval], which is what every production caller leaves it:
	// the cadence is the domain's decision, and a node publishing on
	// another would put its day behind the live meters it is read beside.
	// A test that must see a LIVE loop publish something written after
	// boot shortens it rather than waiting out fifteen seconds.
	Every time.Duration

	Logger *slog.Logger
}

// Publisher is one node's usage publisher.
type Publisher struct {
	deps PublisherDeps

	// mu serialises ticks: the loop runs one, and a test or a shutdown
	// flush may run another.
	mu sync.Mutex

	// marks is the fingerprint each of the two days a tick fingerprints had
	// when it was last derived and every one of its objects was confirmed
	// published.
	marks map[string]store.UsageMark

	// published is the digest of the derivation whose record this process
	// last published for each object — the derivation, not the record: what
	// went out is its join with the fleet's, which equals it on the ordinary
	// day — keyed by [objectKey], for every day the event log's floor still
	// lets it derive, so an unchanged derivation costs no read of the
	// fleet's rows. See the file's header.
	published map[string]string

	// refused is the content this process last saw refused for good for
	// each object, keyed as [Publisher.published] is: the object is skipped
	// while its digest is unchanged. See the file's header.
	refused map[string]refusal

	// short is every object, keyed as [Publisher.published] is, whose last
	// derivation came out SHORT of what the fleet holds of it — raised by
	// the join somewhere — so the line saying so is said when an object
	// becomes short rather than on every tick it stays so
	// ([Publisher.noteShort]).
	short map[string]bool

	// reopened is every day a landed publish put a full-log skip back in
	// line on: once the tick ends its fingerprint is forgotten and the
	// process's bound is lowered to it, so the next tick derives it again
	// whether it is still one of the two a tick fingerprints or not.
	reopened map[string]bool

	// since is the oldest instant this process still owes a derivation of
	// the day of, beside the store's marks: the watermark it read at boot,
	// a day a tick could not settle, a day a landed publish reopened —
	// each the START of a day as this node cuts it, which is what the
	// watermark is written as. Zero when there is none. What it names is
	// written to the store's watermark after every tick, so a restart owes
	// it too.
	since time.Time

	// caughtUp is whether the boot's read of the store's watermark has been
	// answered. Until it has, nothing writes the watermark either: written
	// over unread, the day it named would be gone.
	caughtUp bool

	// watermark is the value this process last wrote to — or read from —
	// the store's watermark, so a tick that would write the same writes
	// nothing. Zero when it has seen none.
	watermark time.Time

	// floorSaid is the range of days past the event log's floor this
	// process last said it would not derive, so a mark that stays put is
	// said once rather than every tick.
	floorSaid string
}

// refusal is one object's content the log refused, and why.
type refusal struct {
	opID   string
	reason statelog.Reason
}

// permanentRefusal reports the reason a publish was refused for good — one no
// retry of the same content changes — or false for every other failure, which
// the next tick retries. See the file's header for the two.
func permanentRefusal(err error) (statelog.Reason, bool) {
	var refused *statelog.Unavailable
	if !errors.As(err, &refused) {
		return "", false
	}
	switch refused.Reason {
	case statelog.ReasonRecordTooLarge, statelog.ReasonLogFull:
		return refused.Reason, true
	}
	return "", false
}

// transient is what is left of a flush's failures once the permanent refusals
// are taken out — nil when there were only those. Each permanent refusal has
// already been logged by the publish that met it, and saying "re-derived on the
// next tick" about it would be untrue.
func transient(err error) error {
	if err == nil {
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		if _, permanent := permanentRefusal(err); permanent {
			return nil
		}
		return err
	}
	var rest []error
	for _, one := range joined.Unwrap() {
		if left := transient(one); left != nil {
			rest = append(rest, left)
		}
	}
	return errors.Join(rest...)
}

// NewPublisher builds a node's publisher, refusing a dependency set that
// could not publish a correct record.
func NewPublisher(d PublisherDeps) (*Publisher, error) {
	switch {
	case d.Store == nil:
		return nil, fmt.Errorf("usage: a publisher needs this node's store")
	case d.Log == nil:
		return nil, fmt.Errorf("usage: a publisher needs the domain's write authority")
	case d.Estate == nil:
		return nil, fmt.Errorf("usage: a publisher needs the replicated estate its " +
			"records are applied into — it is what says a subject is new to the log, " +
			"which is the one proof a full log has room again")
	case strings.TrimSpace(d.NodeID) == "":
		return nil, fmt.Errorf("usage: a publisher needs this node's id — it is " +
			"half of every subject it writes and the only writer allowed on them")
	case d.Caught == nil:
		return nil, fmt.Errorf("usage: a publisher needs to know whether this node's " +
			"usage applier has caught up with the log — every day it publishes is " +
			"joined with the rows it applied, which before then may be a restore's")
	case d.Zone == nil:
		return nil, fmt.Errorf("usage: a publisher needs the company's clock, " +
			"which is what a day is cut on")
	case d.Every < 0:
		return nil, fmt.Errorf("usage: a publisher's cadence of %v is negative; "+
			"leave it zero for %v", d.Every, FlushInterval)
	case d.FireHistory <= 0:
		return nil, fmt.Errorf("usage: a publisher needs how long this node's "+
			"dispatch ledger keeps a fire (got %v) — a day older than that is "+
			"derived again without the schedules it may already have swept", d.FireHistory)
	}
	if d.Every == 0 {
		d.Every = FlushInterval
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		// THE PACKAGE'S OWN LOGGER, never a discarding one — see [log].
		d.Logger = log
	}
	return &Publisher{deps: d, marks: map[string]store.UsageMark{},
		published: map[string]string{}, refused: map[string]refusal{},
		short: map[string]bool{}, reopened: map[string]bool{}}, nil
}

// Run flushes at once and then every [PublisherDeps.Every] — [FlushInterval]
// in production — until ctx ends, and then once more, so an orderly stop
// publishes what today and yesterday gained since the last tick.
//
// THE LAST FLUSH IS DETACHED from ctx, which has just ended, and bounded by
// [FinalFlushBudget] instead. Without it a node that stops and never starts
// again — the one case the next start cannot cover — takes up to a tick of its
// last day with it.
func (p *Publisher) Run(ctx context.Context) {
	ticker := time.NewTicker(p.deps.Every)
	defer ticker.Stop()
	for {
		if err := transient(p.Flush(ctx)); err != nil && ctx.Err() == nil {
			p.deps.Logger.WarnContext(ctx, "usage_flush_failed", "err", err,
				"detail", "the day is re-derived and republished on the next tick")
		}
		select {
		case <-ctx.Done():
			p.finalFlush(ctx)
			return
		case <-ticker.C:
		}
	}
}

// finalFlush is the stop's last tick. See [Run].
func (p *Publisher) finalFlush(ctx context.Context) {
	final, cancel := context.WithTimeout(context.WithoutCancel(ctx), FinalFlushBudget)
	defer cancel()
	if err := transient(p.flush(final, true)); err != nil {
		p.deps.Logger.WarnContext(final, "usage_final_flush_failed", "err", err,
			"budget", FinalFlushBudget.String(),
			"detail", "what this node's day gained since its last tick is published "+
				"when it next starts, whose watermark names the day it stopped in; a "+
				"node that never starts again — or starts after the event log's read "+
				"floor has passed that day — leaves it unpublished")
	}
}

// Flush is one tick: re-derive what moved, publish what changed.
//
// It answers EVERY failure the tick met, joined — a permanent refusal among
// them, which [permanentRefusal] recognises and the tick has already logged.
func (p *Publisher) Flush(ctx context.Context) error { return p.flush(ctx, false) }

// flush is one tick, or — final — the stop's last: today and yesterday alone,
// with every older day the marks or the watermark owe left owed. See the
// file's header.
func (p *Publisher) flush(ctx context.Context, final bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now, loc := p.deps.Now(), p.deps.Zone()
	var failed []error

	// WHAT A RESTART OWES, read once per process before anything that could
	// write over it.
	if !p.caughtUp && !final {
		since, ok, err := p.deps.Store.UsageWatermark(ctx)
		if err != nil {
			failed = append(failed, fmt.Errorf("usage: read the day this node last "+
				"owed a derivation of: %w", err))
		} else {
			p.caughtUp = true
			if ok {
				p.watermark = since
				p.lower(since)
			}
		}
	}

	// THE TWO DAYS A TICK FINGERPRINTS, each on the cut it was first given.
	// A tick that cannot say which instants they are derives nothing: cut
	// afresh, a day would count other instants than its record did.
	window, err := p.window(ctx, now, loc)
	if err != nil {
		failed = append(failed, err)
		return errors.Join(failed...)
	}

	// THE MARKS ARE READ BEFORE ANY DAY IS DERIVED, so a settle that lands
	// after the read is either in a derivation below or stamped above the
	// seq the clear compares against — never neither. On an error the tick
	// derives what it already knows of and clears nothing.
	var (
		mark   store.UsageRederive
		marked bool
		older  []period.Window
	)
	olderSettled := true
	if !final {
		var err error
		mark, marked, err = p.deps.Store.UsageRederive(ctx)
		if err != nil {
			failed = append(failed, fmt.Errorf("usage: read the re-derive marks: %w", err))
		}
		from := p.since
		if marked && (from.IsZero() || mark.Since.Before(from)) {
			from = mark.Since
		}
		if older, err = p.olderDays(ctx, from, window[0], now, loc); err != nil {
			failed = append(failed, err)
			olderSettled = false
		}
	}
	p.forget(now, window, loc)

	// NOT BEFORE THE APPLIER HAS CAUGHT UP — no day, today and yesterday
	// included. What a day is published as is joined with the rows the
	// applier wrote ([Join]), and until it has replayed the log those can be
	// a restore's: older than what this node published after the copy was
	// taken, so a derivation from a restored event log that stops at the
	// copy, joined with them, would still be short of the fleet's record and
	// replace it. Every day stays owed — an older one by the bound, the
	// marks and the watermark, a window day as one this tick could not
	// settle, which lowers the bound below — for a tick after it has. A
	// window day skipped without that would be lost to a node still
	// catching up across a midnight: the next tick's window no longer holds
	// it, and nothing else names it.
	caught := p.deps.Caught()
	if len(older) > 0 && !caught {
		older, olderSettled = nil, false
	}
	for _, day := range older {
		settled, err := p.rederiveDay(ctx, day, now)
		failed = append(failed, err)
		olderSettled = olderSettled && settled
	}
	var unsettled []period.Window
	for _, day := range window {
		if !caught {
			unsettled = append(unsettled, day)
			continue
		}
		settled, err := p.flushDay(ctx, day)
		failed = append(failed, err)
		if !settled {
			unsettled = append(unsettled, day)
		}
	}

	if !final && olderSettled {
		// EVERY OLDER DAY THIS PROCESS OWED IS PUBLISHED.
		p.since = time.Time{}
	}
	// A DAY THIS TICK COULD NOT SETTLE is derived again however far the
	// window has moved by the next tick — yesterday's unresolved last change
	// is otherwise out of sight at the next midnight.
	for _, day := range unsettled {
		p.lower(day.Start)
	}
	// A DAY A LANDED PUBLISH REOPENED is derived again however far the
	// window has moved by the next tick: the objects it put back in line
	// were skipped as settled on this one. Its fingerprint is forgotten, so
	// a window day is derived again next tick, and the process's bound is
	// lowered to it, so a day the window leaves at midnight — yesterday,
	// reopened on the last tick before it — is derived as an older day
	// rather than out of sight with its skips already gone from `refused`.
	// A day whose cut cannot be read stays reopened for the next tick.
	for label := range p.reopened {
		delete(p.marks, label)
		natural, err := period.Parse(period.Day, label, loc)
		if err != nil {
			delete(p.reopened, label)
			continue
		}
		day, ok, err := p.windowOf(ctx, natural)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		delete(p.reopened, label)
		if ok {
			p.lower(day.Start)
		}
	}
	// WHAT THIS PROCESS STILL OWES IS WRITTEN DOWN before any mark is
	// cleared, so no crash between the two leaves a day owed by neither.
	watermarked := true
	if p.caughtUp {
		if err := p.keepWatermark(ctx, window[0]); err != nil {
			failed = append(failed, err)
			watermarked = false
		}
	}
	// THE MARKS ARE CLEARED once every day this tick derived settled — the
	// older ones the marks asked for, and the two a tick fingerprints, which
	// a settle that lands on them moves — up to the newest seq this tick
	// read. A settle since then is stamped above it, and the clear leaves
	// its mark.
	if marked && olderSettled && len(unsettled) == 0 && watermarked {
		if _, err := p.deps.Store.ClearUsageRederive(ctx, mark.Seq); err != nil {
			failed = append(failed, fmt.Errorf("usage: clear the re-derive marks: %w", err))
		}
	}
	return errors.Join(failed...)
}

// keepWatermark writes the store's watermark if it moved: the start of
// yesterday, or of an older day this process still owes — already a day's
// start as this node cuts it (see [Publisher.since]).
func (p *Publisher) keepWatermark(ctx context.Context, yesterday period.Window) error {
	want := yesterday.Start
	if !p.since.IsZero() && p.since.Before(want) {
		want = p.since
	}
	if want.Equal(p.watermark) {
		return nil
	}
	if err := p.deps.Store.SetUsageWatermark(ctx, want); err != nil {
		return fmt.Errorf("usage: write the day this node owes a derivation from: %w", err)
	}
	p.watermark = want
	return nil
}

// window is the two days a tick fingerprints — yesterday and today — each as
// this node cuts it.
func (p *Publisher) window(ctx context.Context, now time.Time, loc *time.Location) (
	[]period.Window, error) {

	today, err := p.dayAt(ctx, now, loc)
	if err != nil {
		return nil, err
	}
	yesterday, err := p.step(ctx, today, -1)
	if err != nil {
		return nil, err
	}
	return []period.Window{yesterday, today}, nil
}

// dayAt is the day this node cuts t into: the recorded cut that holds it, or —
// where none does — the first cut of the label next to it that does, cut and
// recorded by [Publisher.windowOf].
//
// THAT LABEL NEED NOT BE t's DATE ON THE COMPANY'S CLOCK. A label recorded on
// an earlier clock keeps the instants it was first cut on, so after a move WEST
// the label t falls on can be one whose cut already ENDED before t — the hours
// between the old clock's midnight and the new one's belong to no recorded cut
// — and after a move EAST one whose cut STARTS after t. Either way t belongs to
// the label beside it: cut for the first time, a label abuts its recorded
// neighbours ([Publisher.windowOf]), so the one after a pin that ended starts
// where it ended, and the one before a pin that starts late ends where it
// starts. So the walk steps from t's own label toward t until a cut holds it.
// For the hours of a westward move that makes a node's day label run one ahead
// of the company's local date, and for an eastward move one behind — two, for
// the first or last hours of a move of more than a day, which is the one case
// that takes a second step: moved twenty-six hours west, t's own label and the
// one after it can both have ENDED before t. A label the clock left EMPTY is
// stepped past the same way, in the direction t lies from where its empty cut
// would sit. No zone is two days from another, so three steps reach it.
func (p *Publisher) dayAt(ctx context.Context, t time.Time, loc *time.Location) (period.Window, error) {
	label, w, ok, err := p.deps.Store.UsageDayContaining(ctx, t)
	if err != nil {
		return period.Window{}, fmt.Errorf("usage: read the day this node cut %s into: %w",
			t.UTC().Format(time.RFC3339), err)
	}
	if ok {
		return period.Window{Period: period.Day, Label: label,
			Start: w.Start.In(loc), End: w.End.In(loc)}, nil
	}
	day, ok, err := p.windowOf(ctx, period.At(period.Day, t, loc))
	if err != nil {
		return period.Window{}, err
	}
	for range 3 {
		if ok && day.Contains(t) {
			return day, nil
		}
		n := 1
		if t.Before(day.Start) {
			n = -1
		}
		if day, err = p.step(ctx, day, n); err != nil {
			return period.Window{}, err
		}
		ok = true
	}
	if day.Contains(t) {
		return day, nil
	}
	// ONLY A CLOCK MOVED BY MORE THAN A DAY leaves an instant outside every
	// cut it abuts, and no day of this node's can count it.
	return period.Window{}, fmt.Errorf("usage: no day this node cuts holds %s — the "+
		"company's clock moved by more than a day across it",
		t.UTC().Format(time.RFC3339))
}

// step is the day n days from `day` as this node cuts it — n is ±1 — passing
// over a day the company's clock left empty.
func (p *Publisher) step(ctx context.Context, day period.Window, n int) (period.Window, error) {
	natural := day
	// AT MOST TWO EMPTY DAYS IN A ROW: an empty one is a clock moved by a
	// day or more, and no zone is two days from another.
	for range 3 {
		natural = natural.Shift(n)
		next, ok, err := p.windowOf(ctx, natural)
		if err != nil {
			return period.Window{}, err
		}
		if ok {
			return next, nil
		}
	}
	return period.Window{}, fmt.Errorf("usage: the days either side of %s are all empty", day.Label)
}

// windowOf is the window this node cuts the day `natural` names on — the
// company's clock's own cut of it, which carries the label — and false for a
// day it cuts EMPTY, which is then answered as the empty cut it would have
// been, so a caller can tell which side of it an instant lies.
//
// THE FIRST CUT WINS ([store.DB.PinUsageDay]): a label recorded already keeps
// the instants it was first cut on, so a day derived again after an apply
// moved the company's clock counts what its record counted — recut, it would
// replace the record its old cut published while the objects the new cut no
// longer reaches kept theirs, and a row either side of midnight would count in
// two published days or in none. A label cut for the FIRST time ABUTS its
// adjacent labels' recorded cuts — it starts where the day before it ended and
// ends where the day after it starts — so the day a clock moved on is longer
// or shorter by the move. And it is CLAMPED by a farther recorded cut that
// reaches into it: a move of more than a day leaves an adjacent label uncut
// and empty, and a cut that looked only next door would take its natural
// instants from under a recorded day two labels away, counting their rows in
// both. A farther cut that ends before this day starts is the other case — a
// day nobody has cut yet in between, an outage rather than a clock move — and
// is left alone, or a day cut after a node was down two days would swallow the
// uncut day before it. So every instant is in exactly one day. A move of a day
// or more can leave a day with no instants at all, which is answered false
// rather than recorded.
func (p *Publisher) windowOf(ctx context.Context, natural period.Window) (period.Window, bool, error) {
	loc := natural.Start.Location()
	as := func(w store.UsageWindow) period.Window {
		return period.Window{Period: period.Day, Label: natural.Label,
			Start: w.Start.In(loc), End: w.End.In(loc)}
	}
	read := func(label string) (store.UsageWindow, bool, error) {
		w, ok, err := p.deps.Store.UsageDayWindow(ctx, label)
		if err != nil {
			return store.UsageWindow{}, false, fmt.Errorf("usage: read the cut of %s: %w", label, err)
		}
		return w, ok, nil
	}
	pinned, ok, err := read(natural.Label)
	if err != nil || ok {
		return as(pinned), ok, err
	}
	cut := store.UsageWindow{Start: natural.Start, End: natural.End}
	// THE NEAREST RECORDED CUT EACH SIDE, as far as [Publisher.step] passes
	// empty days: no zone is two days from another.
	for k := 1; k <= 3; k++ {
		prev, recorded, readErr := read(natural.Shift(-k).Label)
		if readErr != nil {
			return period.Window{}, false, readErr
		}
		if !recorded {
			continue
		}
		if k == 1 || prev.End.After(cut.Start) {
			cut.Start = prev.End
		}
		break
	}
	for k := 1; k <= 3; k++ {
		next, recorded, readErr := read(natural.Shift(k).Label)
		if readErr != nil {
			return period.Window{}, false, readErr
		}
		if !recorded {
			continue
		}
		if k == 1 || next.Start.Before(cut.End) {
			cut.End = next.Start
		}
		break
	}
	if !cut.Start.Before(cut.End) {
		return as(cut), false, nil
	}
	if pinned, err = p.deps.Store.PinUsageDay(ctx, natural.Label, cut); err != nil {
		return period.Window{}, false, fmt.Errorf("usage: record the cut of %s: %w", natural.Label, err)
	}
	return as(pinned), true, nil
}

// forget drops what this process remembers of days it will never derive
// again: a fingerprint outside the two a tick takes, and an object's last
// publish and refusal on a day past the event log's floor.
func (p *Publisher) forget(now time.Time, window []period.Window, loc *time.Location) {
	floor := now.Add(-store.EventHistory)
	for day := range p.marks {
		if day != window[0].Label && day != window[1].Label {
			delete(p.marks, day)
		}
	}
	past := func(key string) bool {
		day, err := period.Parse(period.Day, dayOf(key), loc)
		return err != nil || day.Start.Before(floor)
	}
	for key := range p.published {
		if past(key) {
			delete(p.published, key)
		}
	}
	for key := range p.refused {
		if past(key) {
			delete(p.refused, key)
		}
	}
	for key := range p.short {
		if past(key) {
			delete(p.short, key)
		}
	}
}

// lower moves the process's own bound back to t if t is older.
func (p *Publisher) lower(t time.Time) {
	if p.since.IsZero() || t.Before(p.since) {
		p.since = t
	}
}

// olderDays is every day from the one `from` falls in through the day before
// yesterday that is not past the event log's floor — see the file's header —
// each as this node cuts it, saying once which days the floor kept it from.
func (p *Publisher) olderDays(ctx context.Context, from time.Time, yesterday period.Window,
	now time.Time, loc *time.Location) ([]period.Window, error) {

	if from.IsZero() || !from.Before(yesterday.Start) {
		return nil, nil
	}
	floor := now.Add(-store.EventHistory)
	var (
		kept period.Window
		err  error
	)
	if !from.Before(floor) {
		if kept, err = p.dayAt(ctx, from, loc); err != nil {
			return nil, err
		}
	}
	if from.Before(floor) || kept.Start.Before(floor) {
		// THE FIRST DAY THAT STARTS AT OR AFTER THE FLOOR, found by the
		// calendar rather than by walking from a mark that may be years
		// back on a clock nobody trusted — and never by cutting the days
		// before it, which nothing will derive.
		first := period.At(period.Day, from, loc).Label
		if kept, err = p.dayAt(ctx, floor, loc); err != nil {
			return nil, err
		}
		if kept.Start.Before(floor) {
			if kept, err = p.step(ctx, kept, 1); err != nil {
				return nil, err
			}
		}
		through := kept.Shift(-1).Label
		said := first + ".." + through
		if p.floorSaid != said {
			p.floorSaid = said
			p.deps.Logger.WarnContext(ctx, "usage_rederive_past_floor",
				"from", first, "through", through,
				"history", store.EventHistory.String(),
				"detail", "days this node owes a derivation of — a custody settle "+
					"changed them, or the node stopped in them and was down since — "+
					"are older than the event log is read back; they keep the value "+
					"last published, because their rows may already be swept and a "+
					"derivation of what is left would replace each whole day on "+
					"every node with less")
		}
	}
	var out []period.Window
	// BY LABEL, which a day's cut cannot move: the labels sort as their
	// dates do.
	for day := kept; day.Label < yesterday.Label; {
		out = append(out, day)
		if day, err = p.step(ctx, day, 1); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// usageWindow is a day as the store's reads take it, with the two review
// decisions a turn whose ending record states no tally is counted in — the
// turn loop's own words, which the store cannot import.
func usageWindow(day period.Window) store.UsageWindow {
	return store.UsageWindow{Start: day.Start, End: day.End,
		Accepted: string(phase.Done), SentBack: string(phase.SelfIterate)}
}

// flushDay re-derives one day of the two a tick fingerprints if it moved, and
// publishes each object that changed — reporting whether the day settled.
func (p *Publisher) flushDay(ctx context.Context, day period.Window) (bool, error) {
	w := usageWindow(day)
	mark, err := p.deps.Store.UsageMark(ctx, w)
	if err != nil {
		return false, fmt.Errorf("usage: fingerprint %s: %w", day.Label, err)
	}
	if last, seen := p.marks[day.Label]; seen && last == mark {
		return true, nil
	}
	derived, err := p.deps.Store.UsageForDay(ctx, w)
	if err != nil {
		return false, fmt.Errorf("usage: derive %s: %w", day.Label, err)
	}
	settled, err := p.publishDay(ctx, day.Label, derived)
	if settled {
		// EVERY OBJECT LANDED, OR WAS REFUSED FOR GOOD AND SAID SO: the
		// mark is kept, so an unchanged day costs the next tick two counts.
		p.marks[day.Label] = mark
	}
	// OTHERWISE THE MARK IS NOT KEPT, so the next tick derives this day
	// again and retries whatever did not land. An object that did land is
	// remembered by its digest and is not sent twice.
	return settled, err
}

// rederiveDay derives an older day IN FULL — no fingerprint, since nothing in
// this process saw the day move — and publishes each object whose derivation
// differs from what this process last published for it, reporting whether the
// day settled. A process
// that has not published the day yet publishes every object of it (see the
// file's header). A day older than the dispatch ledger keeps a fire is derived
// without its schedules; see the file's header.
func (p *Publisher) rederiveDay(ctx context.Context, day period.Window, now time.Time) (bool, error) {
	derived, err := p.deps.Store.UsageForDay(ctx, usageWindow(day))
	if err != nil {
		return false, fmt.Errorf("usage: derive %s again: %w", day.Label, err)
	}
	if day.Start.Before(now.Add(-p.deps.FireHistory)) {
		derived.Schedules = nil
	}
	return p.publishDay(ctx, day.Label, derived)
}

// publishDay publishes each object of one derived day whose derivation
// changed, joined with what the fleet holds of it ([Publisher.publish]),
// reporting whether every one settled — landed, unchanged, or refused for good
// and said.
func (p *Publisher) publishDay(ctx context.Context, day string, derived store.UsageDay) (bool, error) {

	records, err := p.Records(day, derived)
	if err != nil {
		return false, err
	}
	var failed []error
	settled := true
	for _, rec := range records {
		sent, err := p.publish(ctx, day, rec)
		failed = append(failed, err)
		settled = settled && sent
	}
	return settled, errors.Join(failed...)
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
			// NONE FOR A DAY WITH NO CHANGE OR REVIEW, so such a day stays
			// the base format every build reads.
			Code: codeDayOf(CodeDay{ChangesOpened: s.Code.ChangesOpened, ReviewsGiven: s.Code.ReviewsGiven,
				FindingsReceived: s.Code.FindingsReceived, ChecksFailed: s.Code.ChecksFailed,
				Ejections: s.Code.Ejections}),
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
		reads := make([]Read, 0, len(s.Reads))
		for _, r := range s.Reads {
			reads = append(reads, Read{PageID: r.PageID, Backend: r.Backend, Via: r.Via,
				Count: r.Count, LastAt: r.LastAt, LastTurnID: r.LastTurnID,
				LastWorkKey: r.LastWorkKey, LastQuery: r.LastQuery})
		}
		rec.Reads, rec.ReadsElided = capReads(reads)
		out = append(out, rec)
	}
	for _, person := range d.People {
		rec := Record{
			RecordEnvelope: RecordEnvelope{
				Subject: Subject{Kind: KindPerson, Node: p.deps.NodeID, Day: day,
					Person: person.Handle},
				Writer: p.deps.NodeID,
			},
			Role: person.Role,
		}
		for _, t := range person.Tokens {
			rec.Tokens = append(rec.Tokens, Tokens{
				Phase: t.Phase, Worker: t.Worker, Model: t.Model,
				ProviderKey: t.ProviderKey, Input: t.Input, Output: t.Output,
				CacheRead: t.CacheRead, CacheWrite: t.CacheWrite, Total: t.Total,
				Calls: t.Calls,
			})
		}
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
	out = append(out, p.repoReadRecords(day, d.RepoReads)...)
	for i := range out {
		// IN THE ORDER [Join] WRITES THEM, which is the order the store
		// derives them in — so the join of a day with an earlier derivation
		// of it is that day byte for byte.
		slices.SortFunc(out[i].Tokens, compareCell)
		slices.SortStableFunc(out[i].Fires, compareFire)
		if err := out[i].Subject.Validate(); err != nil {
			return nil, fmt.Errorf("usage: this node derived an object it cannot "+
				"address on %s: %w", day, err)
		}
	}
	return out, nil
}

// repoReadRecords is one record per unit repository the day's reads name — its
// readers, the most-read [ReadersPerRepoDay] of them, and its name as the
// day's newest read gave it — from the store's entries, which come in
// (repository, reader) order.
//
// A READER THIS BUILD CANNOT NAME — a principal kind or a way a newer peer's
// code host recorded in a custody batch this node keeps — is counted with the
// elided rather than written: this build cannot stamp a record carrying it at
// the version that holds back the builds it would be lost on, and an answer
// that names fewer readers than the day had must say so.
func (p *Publisher) repoReadRecords(day string, reads []store.UsageRepoRead) []Record {
	var out []Record
	for i := 0; i < len(reads); {
		repo := reads[i].RepoID
		rec := Record{
			RecordEnvelope: RecordEnvelope{
				Subject: Subject{Kind: KindRepoRead, Node: p.deps.NodeID, Day: day, Repo: repo},
				Writer:  p.deps.NodeID,
			},
			RepoName: reads[i].RepoName,
		}
		unnamed := 0
		for ; i < len(reads) && reads[i].RepoID == repo; i++ {
			r := reads[i]
			if !types.CodeReaderKind(r.PrincipalKind).Valid() || !types.CodeFetchedVia(r.Via).Valid() ||
				len(r.Principal) > maxReaderBytes || len(r.Seat) > maxSeatBytes {
				unnamed++
				continue
			}
			rec.RepoReads = append(rec.RepoReads, RepoRead{PrincipalKind: r.PrincipalKind,
				Principal: r.Principal, Seat: r.Seat, Via: r.Via, Reads: r.Reads, Bytes: r.Bytes,
				LastAt: r.LastAt})
		}
		if len(rec.RepoReads) == 0 {
			// EVERY READER UNNAMED: no record this build can write, and a
			// day of none is no record (validateRepoReads).
			continue
		}
		rec.RepoReads, rec.RepoReadersElided = capRepoReads(rec.RepoReads)
		rec.RepoReadersElided += unnamed
		out = append(out, rec)
	}
	return out
}

// capRepoReads keeps the [ReadersPerRepoDay] most-read readers and counts the
// rest.
//
// THE MOST-READ SURVIVE, a tie broken by the reader's key, so two derivations
// of one day are equal value for value — the publisher's no-change rule — and
// the survivors are put back in reader order, so a record's bytes do not move
// when two readers swap rank. It takes the slice it is handed as its own.
func capRepoReads(reads []RepoRead) ([]RepoRead, int) {
	elided := 0
	if len(reads) > ReadersPerRepoDay {
		slices.SortStableFunc(reads, func(a, b RepoRead) int {
			if c := cmp.Compare(b.Reads, a.Reads); c != 0 {
				return c
			}
			return compareRepoReader(a, b)
		})
		elided = len(reads) - ReadersPerRepoDay
		reads = reads[:ReadersPerRepoDay]
	}
	slices.SortFunc(reads, compareRepoReader)
	return reads, elided
}

// capReads keeps the [ReadsPerSeatDay] most-read entries and counts the rest.
//
// THE MOST-READ SURVIVE, the newest breaking a tie, because the question the
// entries answer is "what does this seat lean on" — and the survivors are then
// put back in key order, so a record's bytes do not move when two entries
// swap rank. It takes the slice it is handed as its own.
func capReads(reads []Read) ([]Read, int) {
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

// publish sends one object if its derivation changed since this process last
// published it, reporting whether the object is settled — published and
// confirmed, unchanged, or refused for good and logged — so the day's
// fingerprint may be kept.
//
// WHAT IT SENDS IS THE JOIN of the derivation and what the fleet already holds
// of the object ([Applied], [Join]): never below the fleet's record in any
// figure, and — the ordinary case — the derivation itself. See join.go.
func (p *Publisher) publish(ctx context.Context, day string, rec Record) (bool, error) {
	derived, err := rec.Encode()
	if err != nil {
		return false, fmt.Errorf("usage: encode %s: %w", rec.Subject, err)
	}
	derivedSum := contentDigest(derived)
	key := objectKey(day, rec.Subject)
	if p.published[key] == derivedSum {
		return true, nil
	}
	// WHAT THE FLEET HOLDS OF IT, read before anything is sent: the join is
	// taken against it, and whether it holds the subject at all is the one
	// proof a full log has room again (below).
	applied, held, err := Applied(ctx, p.deps.Estate, rec.Subject)
	if err != nil {
		return false, fmt.Errorf("usage: read what the fleet holds of %s before "+
			"publishing it: %w", rec.Subject, err)
	}
	body := derived
	if held {
		joined := Join(rec, applied)
		if body, err = joined.Encode(); err != nil {
			return false, fmt.Errorf("usage: encode %s: %w", rec.Subject, err)
		}
		// SHORT IS A FIGURE, NEVER A LABEL: a handle or a role the join
		// carried from the fleet's record where the derivation states none
		// is no row this node's log lost, so the join is compared with the
		// derivation under the join's labels. What this process remembers
		// stays the bare derivation's digest, so an unchanged derivation is
		// still skipped on the next tick.
		labelled := rec
		labelled.Handle, labelled.Role, labelled.RepoName = joined.Handle, joined.Role, joined.RepoName
		var plain []byte
		if plain, err = labelled.Encode(); err != nil {
			return false, fmt.Errorf("usage: encode %s: %w", rec.Subject, err)
		}
		p.noteShort(ctx, key, rec, applied, joined, !bytes.Equal(body, plain))
		rec = joined
	} else {
		delete(p.short, key)
	}
	// THE OP ID IS THE CONTENT'S DIGEST, taken over the record before the
	// id is stamped on it: an unchanged object carries the same id on
	// every derivation, which is what lets the broker collapse a repeat,
	// and a changed one carries a new id, which is what stops the broker
	// collapsing a real update into the one before it.
	opID := "usage-" + contentDigest(body)
	if p.refused[key].opID == opID {
		// REFUSED FOR GOOD AT THIS CONTENT, and logged when it was: see
		// the file's header.
		return true, nil
	}
	rec.OpID = opID
	// THE REQUEST IS FORMED FROM THE RECORD'S OWN ENVELOPE, encoded once
	// here before the stamp exists, so the subject, the scope and the op id
	// are the record's rather than a second spelling of them. The stamp
	// moves none of the three.
	if body, err = rec.Encode(); err != nil {
		return false, fmt.Errorf("usage: encode %s: %w", rec.Subject, err)
	}
	env, err := Domain{}.Envelope(body)
	if err != nil {
		return false, fmt.Errorf("usage: the record this node just wrote for %s "+
			"does not decode: %w", rec.Subject, err)
	}
	// WHETHER THE SUBJECT IS NEW TO THE LOG is the replicated rows' answer
	// from before the append — see the file's header.
	fresh := !held
	// THE OP ID CARRIES NO MINT INSTANT, and needs none: the instant is what
	// an operation LEDGER is vouched by ([statelog.OpMintedAt]), and this
	// domain keeps none — so the digest stays a pure function of the
	// content, which is what lets a republish of an unchanged object
	// collapse in the broker's window.
	res, err := p.deps.Log.Publish(ctx, statelog.Request{
		Subject: env.Subject,
		Scope:   env.Scope,
		OpID:    opID,
		// ADDITIVE: exactly one writer publishes on this subject — this
		// node — so there is nothing to arbitrate, and the apply's
		// position guard is what makes a redelivery harmless.
		Pattern: statelog.PatternAdditive,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// THE FRAMEWORK'S STAMP, on this record as on every other
			// domain's: the publisher refuses a record that does not
			// carry the writer and generation it was decided under. The
			// writer is this node either way — the subject's own — and
			// the generation is the snapshot's, which the digest above
			// deliberately does not cover: the same day re-published
			// after a reanchor is the same content.
			stamped := rec
			stamped.Gen, stamped.Writer = stamp.Gen, stamp.Writer
			payload, encodeErr := stamped.Encode()
			if encodeErr != nil {
				return statelog.Decision{}, fmt.Errorf("usage: encode %s: %w",
					rec.Subject, encodeErr)
			}
			return statelog.Decision{Payload: payload}, nil
		},
	})
	if err != nil {
		if reason, permanent := permanentRefusal(err); permanent {
			p.deps.Logger.WarnContext(ctx, "usage_publish_refused",
				"subject", rec.Subject.String(), "op_id", opID, "reason", string(reason),
				"err", err, "detail", "this object is not sent again until its "+
					"content changes"+refusalRemedy(reason))
			p.refused[key] = refusal{opID: opID, reason: reason}
			return true, fmt.Errorf("usage: publish %s: %w", rec.Subject, err)
		}
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
	p.published[key] = derivedSum
	delete(p.refused, key)
	if !fresh {
		return true, nil
	}
	// THE LOG TOOK A NEW SUBJECT, so it is not full any more: every object
	// skipped for a full log goes back in line. Only a NEW subject proves
	// it — a compacted log at its ceiling still takes a record that
	// replaces its subject's last one when it does not grow it, so
	// reopening on any landed record would retry every skipped object on
	// every tick of a log that is still full. New is the replicated rows'
	// answer from before the append, never "this process had not published
	// it": a restart, or an older day's first pass, publishes subjects the
	// log has held for days.
	for skipped, r := range p.refused {
		if r.reason != statelog.ReasonLogFull {
			continue
		}
		delete(p.refused, skipped)
		p.reopened[dayOf(skipped)] = true
	}
	return true, nil
}

// contentDigest is the digest a record's bytes are known by: its op id once
// published, and what this process remembers a derivation by.
func contentDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:16])
}

// noteShort says once, when an object's derivation first comes out SHORT of
// what the fleet holds of it — the join raised one of its figures, never a
// label alone — that this node's event log no longer holds every row the
// fleet's record counted, and forgets it once a derivation is short no longer,
// so the next time it is says so again. See join.go for what does and does
// not follow.
//
// ONCE PER RUN OF SHORT DERIVATIONS, not once per derivation: a day still being
// written changes every tick, and the condition — the log lost rows the fleet's
// record counted — holds across all of them until the node's own count passes
// the fleet's.
func (p *Publisher) noteShort(ctx context.Context, key string, derived, applied, joined Record,
	short bool) {

	if !short {
		delete(p.short, key)
		return
	}
	if p.short[key] {
		return
	}
	p.short[key] = true
	p.deps.Logger.WarnContext(ctx, "usage_rederive_short",
		"subject", derived.Subject.String(),
		"derived", summary(derived), "fleet", summary(applied), "published", summary(joined),
		"detail", "this node's event log counts less of this object than the record "+
			"the fleet holds of it — a store restored beside a newer usage log, one "+
			"started empty, or a turn whose earlier records the event log's sweep "+
			"removed — so each figure is published at the larger of the two: the "+
			"day never shrinks, the rows the log lost are not recovered, and a "+
			"figure the fleet held more of counts this node's new rows only once "+
			"its own count passes the fleet's")
}

// summary is what a log line says one record amounts to.
func summary(r Record) string {
	var total, calls int64
	for _, t := range r.Tokens {
		total += t.Total
		calls += t.Calls
	}
	out := fmt.Sprintf("total=%d calls=%d", total, calls)
	if r.Turns != nil {
		out += fmt.Sprintf(" turns=%d failed=%d reviewed=%d first_pass=%d sent_back=%d",
			r.Turns.Count, r.Turns.Failed, r.Turns.Reviewed, r.Turns.FirstPass, r.Turns.SentBack)
	}
	if len(r.Reads) > 0 || r.ReadsElided > 0 {
		out += fmt.Sprintf(" reads=%d", len(r.Reads)+r.ReadsElided)
	}
	if len(r.Fires) > 0 {
		out += fmt.Sprintf(" fires=%d", len(r.Fires))
	}
	if len(r.RepoReads) > 0 || r.RepoReadersElided > 0 {
		var reads, bytes int64
		for _, rd := range r.RepoReads {
			reads, bytes = reads+rd.Reads, bytes+rd.Bytes
		}
		out += fmt.Sprintf(" readers=%d repo_reads=%d repo_bytes=%d",
			len(r.RepoReads)+r.RepoReadersElided, reads, bytes)
	}
	return out
}

// objectKey is how this process remembers one object's last publish: its day,
// then its subject, so a day's entries are dropped by the day alone.
func objectKey(day string, s Subject) string { return day + "\x00" + s.String() }

// dayOf is the day an [objectKey] was made for.
func dayOf(key string) string {
	day, _, _ := strings.Cut(key, "\x00")
	return day
}

// refusalRemedy is what the refused-publish line adds about what makes room.
func refusalRemedy(reason statelog.Reason) string {
	if reason == statelog.ReasonLogFull {
		return ", or until a publish lands again — the usage log is at its byte " +
			"ceiling, so raise `stream.usage_log_max_bytes` and restart, or " +
			"wait for the horizon to age a day out"
	}
	return " — re-deriving the same day reproduces the same record, so no " +
		"retry places it"
}
