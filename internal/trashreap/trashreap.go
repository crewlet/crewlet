package trashreap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Name is the reaper's one name: the fleet duty's (`worker:trash-reaper`) and
// the author every purge it makes is recorded under. One name for both, so an
// operator who finds a purge in a page's history finds the duty that made it in
// the fleet view under the same word.
const Name = "trash-reaper"

// The schedule and the bounds of one pass.
const (
	// PassInterval is how often a pass runs, and the width of the slot a
	// pass's cutoff and operation ids are derived from.
	//
	// AN HOUR. The term is thirty days, so an item purged up to an hour
	// after its term ran out is late by a fraction of a percent of a
	// promise nobody reads to the minute; and a pass over a trash with
	// nothing due is one indexed listing per source, so an hour costs
	// nothing either. A wider slot only makes the lateness longer, and a
	// narrower one buys nothing a person could notice.
	PassInterval = time.Hour

	// RetryInterval is how soon a pass that failed is run again, inside
	// the same slot when the slot is long enough.
	//
	// TEN MINUTES, the object collector's figure and for its reason: what
	// stops a pass (a log at its ceiling, a broker re-electing, a store
	// that did not answer) is usually over in minutes, and a retry every
	// poll against a failing dependency would add load to the one thing
	// least able to take it. A retry in the same slot derives the same
	// operation ids, so what the failed pass did land collapses.
	RetryInterval = 10 * time.Minute

	// BatchSize is how many items one listing asks a source for.
	//
	// A HUNDRED: each item listed is one purge record published and waited
	// for, so the listing is never the cost; a hundred keeps each read
	// small while a pass at its cap lists ten times, not a thousand.
	BatchSize = 100

	// MaxPerPass is the most purges one pass attempts against one source.
	// A pass that reaches it says so ([SourceReport.More]) and the next
	// pass is due at once rather than a slot later.
	//
	// A THOUSAND: every purge is a record on the domain's log, published
	// and applied one after another, and a thousand at the publisher's
	// slow path of a few milliseconds each keeps a pass to seconds, far
	// inside the duty's lease, which is renewed between passes and never
	// during one. A trash that piled up while no node held the duty, or a
	// mass trash a person made a month ago, drains at a thousand a poll
	// rather than in one burst that would compete with every seat's
	// writes on the same log.
	MaxPerPass = 1000

	// MinTerm is the shortest term a reaper accepts. The engine passes
	// thirty days; anything under a day is a term nobody could restore
	// within, which can only be a mistake, and a mistake here destroys
	// what people trashed expecting to be able to take it back.
	MinTerm = 24 * time.Hour
)

// ErrNotEligible is what a [Source] answers for an item that is no longer in
// its trash since before the cutoff: restored, trashed again since it was
// listed (which starts its term over), or purged already. The reaper counts it
// as skipped, never as a failure.
var ErrNotEligible = errors.New("trashreap: the item is no longer in the trash past its term")

// Source is one domain's trash, as the reaper reaches it. The engine adapts
// each domain's write authority to it; the reaper knows no domain's rows.
type Source interface {
	// Name names the source in reports, logs and operation ids: a short
	// lowercase word, `pages` or `artifacts`, with no dot (a dot in an
	// operation id begins a gesture's step, statelog.StepOpID).
	Name() string

	// TrashedBefore lists the ids of items in the trash since before
	// cutoff, oldest first, at most limit of them. It may read a copy
	// that is behind: the list only nominates.
	TrashedBefore(ctx context.Context, cutoff time.Time, limit int) ([]string, error)

	// PurgeTrashed purges one item through the domain's own purge record,
	// as the engine, under opID, and only if the item is still in the
	// trash since before cutoff: decided again in the snapshot the record
	// is decided in, so a restore between the listing and the purge
	// refuses it. It answers [ErrNotEligible] (wrapped) for an item it did
	// not purge for that reason or because it is gone, nil for an item it
	// purged (applied or durable and pending), and any other error for a
	// purge whose outcome is unknown or refused.
	PurgeTrashed(ctx context.Context, id string, cutoff time.Time, opID string) error
}

// Termed is a [Source] whose items keep a term of their own rather than the
// reaper's: the assistant's conversations, kept `assistant.retention_days` after
// they were last used, beside a trash every other source keeps thirty days.
//
// OPTIONAL, and asked at every pass rather than once, because the term is
// company configuration that an apply may change while the duty runs. A term
// under [MinTerm] fails that source's pass and purges nothing: the floor is
// the reaper's guard against a mistake destroying what people expected to keep,
// whichever source states the term.
//
// A TERM MAY BE UNKNOWN, and Term then answers why. The assistant's is the
// term of the revision the fleet's activation pointer names, which a node that
// does not run that very revision cannot know, and a term guessed in its
// place is one the company may have lengthened: every item between the two
// would be purged for good. An unknown term fails that source's pass the way a
// term under the floor does, and purges nothing of it, so what it would have
// purged waits for a pass that can read the term. The other sources keep the
// reaper's term and are reaped as ever.
type Termed interface {
	Term(ctx context.Context) (time.Duration, error)
}

// Reports is where the reaper keeps its last report: the coordination store's
// trash reap record (coord.TrashReaps), narrowed to the two calls made here.
type Reports interface {
	RecordTrashReap(ctx context.Context, value []byte) error
	TrashReap(ctx context.Context) ([]byte, bool, error)
}

// Deps is what a [Reaper] is built from.
type Deps struct {
	// Sources are the trashes the reaper empties, in the order a pass
	// visits them. At least one, each named once.
	Sources []Source

	// Term is how long an item stays in the trash before it is purged:
	// artifacts.TrashRetention, which is also the knowledge base's
	// promise. At least [MinTerm].
	Term time.Duration

	// Reports is where each pass is recorded and the schedule resumed
	// from. Required: a reaper with nowhere to record would run every
	// pass again each time the duty moved.
	Reports Reports

	// Node is the node running the reaper, named in each report.
	Node string

	// Now is the clock; nil is time.Now.
	Now func() time.Time

	// Logger is where a pass is logged; nil is the package's own logger.
	Logger *slog.Logger
}

// Reaper empties the trash of every source it was given. A pass may be asked
// for directly ([Reaper.Pass]); the duty's loop asks through [Reaper.Turn],
// which claims the duty and keeps the schedule.
type Reaper struct {
	sources []Source
	term    time.Duration
	reports Reports
	node    string
	now     func() time.Time
	log     *slog.Logger

	// mu guards the schedule below and serialises passes: the duty's loop
	// is one goroutine, and a pass asked for beside it waits rather than
	// running against the same items at once.
	mu sync.Mutex
	// next is when the next pass is due; the zero instant is now.
	next time.Time
	// resumed is whether the schedule has been read back from the fleet's
	// report since this node took the duty.
	resumed bool
}

// New builds a reaper, refusing a deps it could not run honestly.
func New(deps Deps) (*Reaper, error) {
	if len(deps.Sources) == 0 {
		return nil, errors.New("trashreap: a reaper needs at least one source; " +
			"a duty that empties no trash would hold a lease for nothing")
	}
	seen := map[string]bool{}
	for i, src := range deps.Sources {
		if src == nil {
			return nil, fmt.Errorf("trashreap: source %d is nil", i)
		}
		name := src.Name()
		if err := validName(name); err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("trashreap: two sources are named %q, and a source's "+
				"name is what its operation ids and its report row are keyed on", name)
		}
		seen[name] = true
	}
	if deps.Term < MinTerm {
		return nil, fmt.Errorf("trashreap: a term of %v is under the %v minimum; "+
			"pass artifacts.TrashRetention, the term the trash promises", deps.Term, MinTerm)
	}
	if deps.Reports == nil {
		return nil, errors.New("trashreap: a reaper needs the coordination store's " +
			"trash reap record to report to and resume from")
	}
	if strings.TrimSpace(deps.Node) == "" {
		return nil, errors.New("trashreap: a reaper needs the id of the node running it, " +
			"which every report names")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	logger := deps.Logger
	if logger == nil {
		logger = log
	}
	return &Reaper{
		sources: append([]Source(nil), deps.Sources...),
		term:    deps.Term, reports: deps.Reports, node: deps.Node,
		now: now, log: logger,
	}, nil
}

var log = logging.Get("trashreap")

// validName holds a source's name to what an operation id's name segment and a
// report row may carry.
func validName(name string) error {
	if name == "" {
		return errors.New("trashreap: a source needs a name")
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("trashreap: source name %q may hold only lowercase "+
				"letters, digits and '-': it is part of every operation id the "+
				"reaper derives, where a dot begins a step and a space is refused", name)
		}
	}
	return nil
}

// OpID is the operation id the purge of item id from source, in the pass of
// slot, is published under.
//
// DERIVED, so a second reaper in the same slot and a pass retried inside it
// publish each purge under the id the first attempt used, and the operation
// ledger and the broker's duplicate window collapse them into one record. The
// slot's start is the instant: the unit of work is the pass, and its start is
// what every attempt at it reproduces, never the attempt's own clock.
func OpID(slot time.Time, source, id string) string {
	return statelog.DeriveOpID(slot, "trashreap",
		"crewlet.trashreap", source, id, slot.UTC().Format(time.RFC3339Nano))
}

// Report is the account of one pass, recorded in the coordination store.
type Report struct {
	// Node is the node that ran the pass.
	Node string `json:"node"`

	// Slot is the start of the slot the pass ran in, and Cutoff the
	// instant an item had to be trashed before to be purged: the slot's
	// start less the term.
	Slot   time.Time `json:"slot"`
	Cutoff time.Time `json:"cutoff"`

	// Started and Finished bound the pass on the running node's clock.
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`

	// Next is when the following pass is due: the next slot after a clean
	// pass, at once after one that reached its cap, and [RetryInterval]
	// later after one that failed. The node that takes the duty next
	// resumes from it.
	Next time.Time `json:"next"`

	// Sources is what each source gave up, in pass order.
	Sources []SourceReport `json:"sources"`
}

// SourceReport is what one source gave up in one pass.
type SourceReport struct {
	Source string `json:"source"`

	// Purged counts the items purged, Skipped the items no longer eligible
	// when their purge was decided, and Failed the purges that failed or
	// whose outcome is unknown, which the next pass tries again.
	Purged  int `json:"purged"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`

	// Cutoff is the instant this source's items had to be untouched since
	// for its term ([Termed]), and zero where it keeps the reaper's.
	Cutoff time.Time `json:"cutoff,omitzero"`

	// More says the pass stopped at [MaxPerPass] with items still due.
	More bool `json:"more,omitempty"`

	// Error is the first failure the pass met on this source, a listing
	// that failed included, and empty when it met none.
	Error string `json:"error,omitempty"`
}

// acted reports whether the pass purged, skipped or failed anything.
func (r Report) acted() bool {
	for _, s := range r.Sources {
		if s.Purged > 0 || s.Skipped > 0 || s.failed() || s.More {
			return true
		}
	}
	return false
}

// failed reports whether the source met any failure in the pass.
func (s SourceReport) failed() bool { return s.Failed > 0 || s.Error != "" }

// DecodeReport reads a report the reaper recorded.
func DecodeReport(raw []byte) (Report, error) {
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return Report{}, fmt.Errorf("trashreap: the recorded trash reap is unreadable: %w", err)
	}
	return r, nil
}

// LastReport is the last pass the fleet recorded, false when none was.
func LastReport(ctx context.Context, reports Reports) (Report, bool, error) {
	raw, found, err := reports.TrashReap(ctx)
	if err != nil || !found {
		return Report{}, false, err
	}
	r, err := DecodeReport(raw)
	if err != nil {
		return Report{}, false, err
	}
	return r, true, nil
}

// Turn is one turn of the duty: claim it, and run a pass when one is due.
//
// claim is the duty's lease claim; nil is a node with nobody to share the duty
// with, which runs it alone.
//
// AN UNANSWERED CLAIM IS NEITHER ANSWER: this node runs no pass, since it
// cannot say the duty is its own, and it forgets the resume, since another node
// may take the duty and record a newer pass while nobody can say who holds it.
func (r *Reaper) Turn(ctx context.Context, claim func(context.Context) (bool, error)) {
	mine := claim == nil
	if claim != nil {
		held, err := claim(ctx)
		if err != nil {
			r.mu.Lock()
			r.resumed = false
			r.mu.Unlock()
			if ctx.Err() == nil {
				r.log.WarnContext(ctx, "trash_reaper_duty_unclaimed", "error", err)
			}
			return
		}
		mine = held
	}
	if !mine {
		r.mu.Lock()
		r.resumed = false
		r.mu.Unlock()
		return
	}
	r.resume(ctx)
	r.mu.Lock()
	due := !r.now().Before(r.next)
	r.mu.Unlock()
	if !due {
		return
	}
	if _, err := r.Pass(ctx); err != nil && ctx.Err() == nil {
		r.log.WarnContext(ctx, "trash_reap_incomplete", "error", err,
			"detail", "the items not purged stay in the trash for the next pass")
	}
}

// resume reads the schedule back from the fleet's last report, once per
// holding of the duty, so a duty that moved runs its next pass when its last
// holder said, not at once.
//
// AN UNREADABLE REPORT RUNS THE PASS rather than waiting on a record nobody
// can read: a pass in a slot that already had one derives the same operation
// ids and finds the trash already emptied, so the cost is a listing.
func (r *Reaper) resume(ctx context.Context) {
	r.mu.Lock()
	resumed := r.resumed
	r.mu.Unlock()
	if resumed {
		return
	}
	last, found, err := LastReport(ctx, r.reports)
	if err != nil {
		r.log.WarnContext(ctx, "trash_reaper_schedule_unread", "error", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		// NOT RESUMED: the next turn asks again, and this one runs
		// whatever this node's own schedule says.
		return
	}
	r.resumed = true
	if found && last.Next.After(r.next) {
		r.next = last.Next
	}
}

// Pass runs one pass now, records it, and answers its report.
//
// The error joins every source's first failure and a report that could not be
// recorded; the report is whole either way. A pass whose context ended records
// nothing: the node is stopping, and the next holder resumes from the last
// pass that finished.
func (r *Reaper) Pass(ctx context.Context) (Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	started := r.now().UTC()
	slot := started.Truncate(PassInterval)
	report := Report{
		Node: r.node, Slot: slot, Cutoff: slot.Add(-r.term), Started: started,
	}
	var failures []error
	for _, src := range r.sources {
		cutoff, own, err := sourceCutoff(ctx, src, slot, report.Cutoff)
		if err != nil {
			report.Sources = append(report.Sources, SourceReport{Source: src.Name(),
				Error: err.Error()})
			failures = append(failures, fmt.Errorf("trashreap: %s: %w", src.Name(), err))
			continue
		}
		got, err := r.reap(ctx, src, slot, cutoff)
		if own {
			got.Cutoff = cutoff
		}
		report.Sources = append(report.Sources, got)
		if err != nil {
			failures = append(failures, fmt.Errorf("trashreap: %s: %w", src.Name(), err))
		}
	}
	report.Finished = r.now().UTC()
	report.Next = r.schedule(report)
	r.next = report.Next

	// AT INFO ONLY WHEN THE PASS DID SOMETHING: an empty trash is the
	// ordinary hour, and a line for it every hour on the holding node would
	// bury the passes that purged.
	level := slog.LevelDebug
	if report.acted() {
		level = slog.LevelInfo
	}
	r.log.Log(ctx, level, "trash_reap_pass", "slot", slot, "cutoff", report.Cutoff,
		"sources", summary(report.Sources), "next", report.Next)
	if ctx.Err() != nil {
		return report, errors.Join(append(failures, ctx.Err())...)
	}
	raw, err := json.Marshal(report)
	if err == nil {
		err = r.reports.RecordTrashReap(ctx, raw)
	}
	if err != nil {
		failures = append(failures, fmt.Errorf("trashreap: record the pass: %w", err))
	}
	return report, errors.Join(failures...)
}

// sourceCutoff is the cutoff src is reaped at in the pass of slot: its own
// term's ([Termed]), or the reaper's, and whether it is its own. A term the
// source cannot state is an error, and so no cutoff at all.
func sourceCutoff(ctx context.Context, src Source, slot,
	reaperCutoff time.Time) (time.Time, bool, error) {

	termed, ok := src.(Termed)
	if !ok {
		return reaperCutoff, false, nil
	}
	term, err := termed.Term(ctx)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("the source's term is unknown, so nothing "+
			"of it is purged until a pass can read it: %w", err)
	}
	if term < MinTerm {
		return time.Time{}, false, fmt.Errorf("the source's term of %v is under the %v "+
			"minimum, so nothing is purged until it states one a person could take an "+
			"item back within", term, MinTerm)
	}
	return slot.Add(-term), true, nil
}

// schedule is when the pass after report is due.
func (r *Reaper) schedule(report Report) time.Time {
	nextSlot := report.Slot.Add(PassInterval)
	more, failed := false, false
	for _, s := range report.Sources {
		more = more || s.More
		failed = failed || s.failed()
	}
	switch {
	case more:
		// A BACKLOG DRAINS AT ONCE, a cap at a time: the cap bounds a
		// burst, not the rate a month's trash is emptied at.
		return report.Finished
	case failed:
		retry := report.Finished.Add(RetryInterval)
		if retry.After(nextSlot) {
			return nextSlot
		}
		return retry
	}
	return nextSlot
}

// reap empties one source of what was trashed before cutoff, at most
// [MaxPerPass] purges, and answers what it gave up and its first failure.
//
// # Why every attempted id is remembered for the pass
//
// The listing reads the node's copy. An item purged with a pending outcome is
// durable but may not be applied here yet, and an item that failed or was
// refused stays in the trash; either would be listed again and attempted again
// for the rest of the pass, so each id is attempted once per pass and the
// listing is widened by the ids it has to step over.
func (r *Reaper) reap(ctx context.Context, src Source, slot, cutoff time.Time) (SourceReport, error) {
	out := SourceReport{Source: src.Name()}
	var first error
	fail := func(err error) {
		if first == nil {
			first = err
			out.Error = err.Error()
		}
	}
	attempted := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			fail(err)
			return out, first
		}
		want := BatchSize + len(attempted)
		ids, err := src.TrashedBefore(ctx, cutoff, want)
		if err != nil {
			fail(fmt.Errorf("list the trash: %w", err))
			return out, first
		}
		fresh := 0
		for _, id := range ids {
			if attempted[id] {
				continue
			}
			if len(attempted) == MaxPerPass {
				out.More = true
				return out, first
			}
			fresh++
			attempted[id] = true
			err := src.PurgeTrashed(ctx, id, cutoff, OpID(slot, src.Name(), id))
			switch {
			case err == nil:
				out.Purged++
			case errors.Is(err, ErrNotEligible):
				out.Skipped++
			default:
				out.Failed++
				fail(fmt.Errorf("purge %s: %w", id, err))
				if ctx.Err() == nil {
					r.log.WarnContext(ctx, "trash_reap_purge_failed", "source", src.Name(),
						"item", id, "error", err)
				}
			}
		}
		if fresh == 0 || len(ids) < want {
			return out, first
		}
	}
}

// summary renders the per-source counts on one log line.
func summary(sources []SourceReport) string {
	parts := make([]string, 0, len(sources))
	for _, s := range sources {
		part := fmt.Sprintf("%s purged=%d skipped=%d failed=%d", s.Source, s.Purged,
			s.Skipped, s.Failed)
		if s.More {
			part += " more"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}
