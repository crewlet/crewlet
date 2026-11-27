package learning

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/workkey"
)

// The episode compaction lifecycle: the pass that drains a seat's episodes.
// Raw rows pile up one per turn and recall scans them linearly (no ANN index
// is reachable from the Go driver; BenchmarkRecallScan sizes this pass).
//
// Four actions, cheapest first, so none runs over rows an earlier one deletes:
//  1. drop mid-state rows (a self_iterate turn is half a turn).
//  2. drop rows a skill absorbed, past their audit grace.
//  3. optionally evict ancient summaries and their anchors, for a hard cap.
//  4. fold the rest into ONE summary per tool-sequence cluster, keeping a few
//     drill-down anchors; the only LLM-costing step, so last.

// ErrPassInFlight reports that a pass for this seat is already running.
//
// A sentinel rather than a silent no-op: the caller publishes a
// CompactionCompleted carrying the skip reason, and an operator reading a
// stream of "compacted nothing" needs to tell "there was nothing to do" from
// "a burst of requests collapsed into one pass".
var ErrPassInFlight = errors.New("learning: a compaction pass for this seat is already running")

// Cluster is one group of similar turns, handed to a [Summarizer].
type Cluster struct {
	// Handle and Role are the seat the whole cluster belongs to, hoisted
	// out of the episodes rather than left to be read off Episodes[0]. A
	// model-backed summarizer needs both — the role selects the auxiliary
	// provider chain and the handle attributes the token usage — and making
	// each implementation reach into element zero spreads the
	// representative-first invariant into code that has no reason to know
	// about it.
	Handle string
	Role   string
	// Episodes are the cluster's members, in the order they were pooled —
	// oldest first, and the first element is the representative whose tool
	// sequence every other member was matched against.
	Episodes []Episode
}

// Summary is what a [Summarizer] made of a cluster.
//
// SUCCESS RATE IS ABSENT ON PURPOSE. The rate that reaches the row is computed
// from the outcomes of the rows just clustered, and a field here would invite
// a model-supplied number that contradicts the row's own members: a cluster
// that was 90% failed once surfaced under an outcome filter of "done" because
// the model mislabelled it. The data is right there; nothing needs to guess.
type Summary struct {
	// CommonTaskPattern is one declarative sentence. The agent reads it
	// as "you have done this kind of work N times".
	CommonTaskPattern string

	// CommonOutcome is the model's own word for how the pattern usually
	// ends. It is stored as prose and never filtered on — see
	// [Summary]'s note on the success rate for what happens when a derived
	// column trusts a model's label.
	CommonOutcome string

	// SubjectsInvolved are the distinct counterparties the pattern names.
	SubjectsInvolved []string

	// NotablePatterns is the variation across the cluster: the edge cases
	// and the handoffs, which is the part a single exemplar cannot show.
	NotablePatterns string

	// THERE IS NO VECTOR HERE, and a compacted row is never embedded. A
	// field for one sat here with nothing that filled it and nothing that
	// read it: similarity recall reads raw turns only, because a summary of
	// a cluster reads in a prompt like one turn that did all of them. Every
	// time-window and outcome query still reads a compacted row.
}

// Summarizer folds a cluster of similar turns into one summary.
//
// ONE METHOD, and the whole reason this interface exists: everything else in
// the pass — clustering, exemplar choice, the transaction, the recovery sweep
// — is decidable arithmetic over rows, and a test that has to stand up an LLM
// to reach any of it will not be written. A worker that takes a provider map,
// an org and an agent pool in its constructor and resolves the model three
// frames into the compaction routine needs a live provider to exercise a
// clustering rule.
type Summarizer interface {
	Summarize(ctx context.Context, c Cluster) (Summary, error)
}

// Options are the lifecycle's knobs. The zero value is the shipped default
// for every field; see [Options.withDefaults] for where each number comes
// from.
//
// Durations, not day counts: the day count is an artifact of the Postgres
// make_interval() call that used to enforce these, and every one of them is
// compared against a time.Time here.
type Options struct {
	// Threshold is the raw-row count that makes a pass due.
	Threshold int

	// NonTerminalMaxAge is when a mid-state row is dropped.
	NonTerminalMaxAge time.Duration

	// ConsolidatedGrace is how long a row a skill absorbed survives.
	ConsolidatedGrace time.Duration

	// MinAge keeps recent rows raw and out of the compactor.
	MinAge time.Duration

	// CapAge is how old a raw row must be to fold once the seat holds
	// Threshold of them: the cap binds whatever MinAge says, sparing only
	// the synthesizer's own window, whose turns it still drafts from.
	CapAge time.Duration

	// MinClusterSize is the smallest cluster worth summarising.
	MinClusterSize int

	// JaccardThreshold is the tool-sequence overlap that pools two turns.
	JaccardThreshold float64

	// BatchSize is how many raw rows one pass pulls.
	BatchSize int

	// ExemplarCount is how many members of a compacted cluster stay raw.
	ExemplarCount int

	// ToolFreeMaxAge is when a raw turn that called NO TOOLS is dropped.
	//
	// Such a turn can never be COMPACTED: clustering pools by tool-sequence
	// overlap, and Jaccard over an empty set is undefined, so for a chat-only
	// seat the fold never fires and every row is scanned at every turn start
	// for ever. A horizon rather than a cap, because no summary would carry
	// these rows' content forward. See defaultToolFreeMaxAge.
	ToolFreeMaxAge time.Duration

	// CompactedMaxAge evicts summaries themselves. Zero keeps them
	// forever, which is the default: they are small, and losing one loses
	// the only record of a whole era of a seat's work.
	//
	// A LONG-TAIL knob, years not weeks: a summary carries the ended_at of
	// the newest turn it covers, at least MinAge old, so a horizon near
	// MinAge writes summaries already past it, and the next pass deletes the
	// row an LLM call just paid for.
	CompactedMaxAge time.Duration

	// Budget is asked before a seat's first fold, the pass's first model
	// call ([BudgetGate]); nil never declines. The sweeps call no model and
	// run whatever it answers.
	Budget BudgetGate
}

const (
	day = 24 * time.Hour

	// defaultThreshold is the raw-row count that makes a pass due.
	//
	// A latency budget, not a storage one: recall scans every embedded row a
	// seat owns once per turn, at 7.5 µs per row at 1 536 dimensions
	// (BenchmarkRecallScan). 500 rows is ~3.9 ms at every turn start, a
	// rounding error next to one LLM round; 2000 is ~15 ms and climbs
	// linearly, since nothing else bounds this number.
	//
	// It must stay in step with config.DefaultEpisodeLifecycle, which is what
	// an operator edits; a test asserts the two agree.
	defaultThreshold = 500

	// defaultNonTerminalMaxAge is how long a self_iterate row survives.
	//
	// Two weeks rather than immediately: a mid-state turn is still the
	// honest record of what the seat did that day, and an operator
	// investigating a bad week needs the iterations, not just the outcome.
	// After that only recall reads it, and there it is a half-finished plan
	// competing with finished ones.
	defaultNonTerminalMaxAge = 14 * day

	// defaultToolFreeMaxAge is how long a raw turn that called no tools
	// stays recallable.
	//
	// Ninety days, as in the chat-thread follow retention: a quarter is when a
	// conversation stops being live on every backend. A tool-free turn is a
	// conversation, so its value as "similar prior work" decays with its thread
	// while its recall-scan cost (7.5 µs per turn start) never does.
	//
	// Far longer than the other raw-row horizons (14 and 30 days), which drop
	// rows half-finished or carried forward elsewhere; this drops the only
	// record of work that happened. A fact worth keeping past a quarter is one
	// the seat persists with reflect_and_persist, untouched here.
	defaultToolFreeMaxAge = 90 * day

	// defaultConsolidatedGrace is the audit window after a skill absorbs a
	// row. A month is one full review cycle: long enough that a bad
	// consolidation is noticed by somebody reading the skill in anger, and
	// bounded so the sources of a stable skill do not outlive it.
	defaultConsolidatedGrace = 30 * day

	// defaultMinAge keeps a row raw until every other reader has finished
	// with it. The clustering pass that drafts skills looks back 168 hours
	// (config.DefaultSkillSynthesis), so 30 days is four full passes of
	// headroom: compaction must never eat a turn the synthesizer would
	// still have drafted from, because the summary carries no tool
	// arguments and a skill cannot be written from it.
	defaultMinAge = 30 * day

	// defaultCapAge is the synthesizer's clustering window, the youngest a
	// row folds at once the seat is over its raw cap.
	defaultCapAge = 7 * day

	// defaultMinClusterSize leaves singletons and pairs alone. Three
	// occurrences is where "it happened" becomes "it recurs"; below that
	// the per-turn detail is worth more than an aggregate over it.
	defaultMinClusterSize = 3

	// defaultJaccard is the tool-sequence overlap that pools two turns.
	//
	// It is 0.6 because the skill-drafting clustering pass uses 0.6
	// (config.DefaultSkillSynthesis.ClusterJaccardThreshold). Two different
	// answers to "are these the same work" would let compaction fold turns
	// that synthesis had deliberately kept apart — and the fold is the
	// destructive one.
	defaultJaccard = 0.6

	// defaultBatchSize bounds one pass. 200 rows is the same batch the
	// clustering pass pulls, and it bounds the worst case that actually
	// costs money: one cluster of 200 turns renders to roughly 120 KB of
	// prompt, about 30k tokens, in a single summarisation call.
	defaultBatchSize = 200

	// defaultExemplarCount is how many members stay raw after a fold.
	//
	// Two, and it must stay below MinClusterSize or a fold keeps every row
	// AND writes a summary — the pass that exists to drain the table would
	// grow it. Two rather than one because the interesting thing about a
	// cluster is its VARIATION, and a single sample cannot show any.
	defaultExemplarCount = 2
)

// withDefaults fills the zero fields and clamps the ones a hand-built Options
// could set to a value that makes the pass destructive.
func (o Options) withDefaults() Options {
	if o.Threshold <= 0 {
		o.Threshold = defaultThreshold
	}
	if o.NonTerminalMaxAge <= 0 {
		o.NonTerminalMaxAge = defaultNonTerminalMaxAge
	}
	if o.ToolFreeMaxAge <= 0 {
		o.ToolFreeMaxAge = defaultToolFreeMaxAge
	}
	if o.ConsolidatedGrace <= 0 {
		o.ConsolidatedGrace = defaultConsolidatedGrace
	}
	if o.MinAge <= 0 {
		o.MinAge = defaultMinAge
	}
	if o.CapAge <= 0 || o.CapAge > o.MinAge {
		o.CapAge = min(defaultCapAge, o.MinAge)
	}
	if o.MinClusterSize <= 0 {
		o.MinClusterSize = defaultMinClusterSize
	}
	if o.MinClusterSize < 2 {
		// A "cluster" of one is a turn with a summary written over it: the
		// LLM call is spent, the detail is gone, and the count says 1.
		o.MinClusterSize = 2
	}
	if o.JaccardThreshold <= 0 || o.JaccardThreshold > 1 {
		o.JaccardThreshold = defaultJaccard
	}
	if o.BatchSize < o.MinClusterSize {
		// A batch that cannot hold one cluster makes every pass a no-op
		// that still costs four queries.
		o.BatchSize = max(defaultBatchSize, o.MinClusterSize)
	}
	if o.ExemplarCount < 1 {
		// BELOW ONE READS AS UNSET, so a fold always leaves something to
		// drill into. The alternative — honouring a zero — makes the zero
		// VALUE of this struct produce summaries with no anchors at all,
		// and a summary nobody can open is the failure this whole exemplar
		// mechanism exists to prevent. What it costs to keep them is two
		// rows per fold, which at the measured 7.5 µs of recall scan per
		// row is 15 µs a turn.
		o.ExemplarCount = defaultExemplarCount
	}
	return o
}

// Lifecycle runs compaction passes over one store.
type Lifecycle struct {
	db   *store.DB
	sum  Summarizer
	opts Options

	// inflight is the per-seat single-flight. A burst of writes crossing
	// the threshold publishes a request per write, and two passes over one
	// seat would fetch the same candidates, summarise them twice and write
	// two rows claiming the same turns. The durable half of that guard is
	// the compacted row's work key (see [Lifecycle.foldCluster]) — this
	// half stops the second pass from spending the LLM call at all.
	mu       sync.Mutex
	inflight map[string]struct{}
}

// NewLifecycle wraps a database handle and the summarizer that folds
// clusters.
func NewLifecycle(db *store.DB, s Summarizer, o Options) *Lifecycle {
	return &Lifecycle{db: db, sum: s, opts: o.withDefaults(), inflight: map[string]struct{}{}}
}

// Options returns the knobs in force, defaults applied.
func (l *Lifecycle) Options() Options { return l.opts }

// terminalOutcomes is [SettledOutcomes] as a SQL tuple.
//
// Used twice, in complementary directions: a compaction candidate must be in
// it, and the mid-state sweep drops everything that is not. Two hand-written
// lists leave a row neither droppable nor compactable, in the seat's memory
// forever.
//
// Spelled into the query rather than bound: these are compile-time constants,
// never caller input, like the state names in ListOptions.filter. A value that
// stopped being a bare identifier would need quoting this does not do, so a
// test holds every value to that shape.
var terminalOutcomes = sqlTuple(SettledOutcomes())

// sqlTuple renders values as a SQL tuple literal.
//
// No escaping, deliberately: see [terminalOutcomes] for why nothing here is
// ever caller input, and why the constraint that keeps it that way is a test
// rather than a quoting pass that would imply otherwise.
func sqlTuple(vals []string) string {
	quoted := make([]string, 0, len(vals))
	for _, v := range vals {
		quoted = append(quoted, "'"+v+"'")
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}

// RawCount reports how many raw rows a seat holds, and whether that is past
// the threshold.
//
// The count comes back even when the answer is no, because the request event
// carries it as telemetry.
//
// A store that cannot be read is NOT due. The polarity is worth stating: the
// pass reads the same store on its very next statement, so calling unknown
// "due" only converts a read failure into a pass failure, one per write check,
// for as long as the outage lasts.
func (l *Lifecycle) RawCount(ctx context.Context, handle string) (int, bool, error) {
	if handle == "" {
		return 0, false, fmt.Errorf("learning: a raw count needs a seat")
	}
	var n int
	if err := l.db.SQL().QueryRowContext(ctx,
		`SELECT count(*) FROM episodes WHERE agent_handle = ? AND kind = 'raw'`,
		handle).Scan(&n); err != nil {
		return 0, false, fmt.Errorf("learning: count raw episodes for %s: %w", handle, err)
	}
	return n, n >= l.opts.Threshold, nil
}

// PassResult is what one pass did. Every field is what THIS pass removed or
// wrote, never a total: two nodes sweeping one seat each report their own
// share, and summing them is the only way to get the total.
type PassResult struct {
	NonTerminalDropped  int
	ConsolidatedDropped int

	// ToolFreeDropped counts raw turns dropped for having called no tools
	// and aged past ToolFreeMaxAge. Counted separately from the others
	// because it is the only sweep here that removes work nothing else
	// carried forward — a summary replaces the rows it folds, and this
	// replaces its rows with nothing.
	//
	// Reported under its own key on `episode_lifecycle_pass`. The
	// CompactionCompleted event folds it into NonTerminalDropped instead,
	// deliberately, so the count reaches an operator on the surface that
	// can carry a new field without two builds on one stream having to
	// agree on it.
	ToolFreeDropped int

	// OrphansDropped counts rows an existing summary already covered. Any
	// value above zero means a previous pass wrote its summary and did not
	// remove its sources; see [Lifecycle.sweepOrphans].
	OrphansDropped int

	ClustersCompacted int
	RawReplaced       int

	// SummarizerFailures counts clusters the model could not summarise.
	// They are left raw and retried by the next pass — the rows are still
	// there, so nothing is lost but the call.
	SummarizerFailures int

	CompactedEvicted int
	ExemplarsEvicted int

	// BudgetSkip is why the folds did not start, "" when they did or had
	// nothing to fold: [SkipNoBudget] or [SkipBudgetUnknown]. Its clusters
	// stay raw for the next pass.
	BudgetSkip string
}

// Pass runs one full lifecycle pass over a seat.
//
// Partial results come back WITH the error. The actions are independent
// deletes and folds; the ones that committed are real, and a caller that threw
// the result away on error would publish a completion event claiming a pass
// removed nothing when it removed thousands of rows.
//
// A failing summarizer is the one error that does not abort: a model is flaky
// by nature and the other clusters are still foldable, so it is counted and
// the pass continues. A failing store aborts, because the next statement is
// against the same store.
func (l *Lifecycle) Pass(ctx context.Context, handle string, now time.Time) (PassResult, error) {
	if handle == "" {
		return PassResult{}, fmt.Errorf("learning: a compaction pass needs a seat")
	}
	if !l.claim(handle) {
		return PassResult{}, ErrPassInFlight
	}
	defer l.release(handle)

	var res PassResult
	n, err := l.dropNonTerminal(ctx, handle, now.Add(-l.opts.NonTerminalMaxAge))
	res.NonTerminalDropped = int(n)
	if err != nil {
		return res, err
	}

	n, err = l.dropConsolidated(ctx, handle, now.Add(-l.opts.ConsolidatedGrace))
	res.ConsolidatedDropped = int(n)
	if err != nil {
		return res, err
	}

	// The one bound on a chat-only seat's raw rows. Nothing below this can
	// reach them: the fold clusters by tool overlap and skips them
	// entirely, so without this the table grows for the life of the
	// deployment and every turn start pays for it.
	n, err = l.dropToolFree(ctx, handle, now.Add(-l.opts.ToolFreeMaxAge))
	res.ToolFreeDropped = int(n)
	if err != nil {
		return res, err
	}

	// BEFORE the fold, not after it. A summary is dated by the WORK it
	// covers, not by when it was written, so a cluster
	// of rows older than this horizon is born already past it — and evicting
	// after the fold deleted such a summary in the same pass that spent an
	// LLM call producing it. Going first costs nothing and leaves every
	// summary readable for at least one pass.
	if l.opts.CompactedMaxAge > 0 {
		rows, exemplars, err := l.evictCompacted(ctx, handle, now.Add(-l.opts.CompactedMaxAge))
		res.CompactedEvicted, res.ExemplarsEvicted = int(rows), int(exemplars)
		if err != nil {
			return res, err
		}
	}

	if err := l.compact(ctx, handle, now, &res); err != nil {
		return res, err
	}

	log.InfoContext(ctx, "episode_lifecycle_pass",
		"agent_handle", handle,
		"non_terminal_dropped", res.NonTerminalDropped,
		"consolidated_dropped", res.ConsolidatedDropped,
		// ON ITS OWN HERE, unlike on the event, where it is folded into
		// the non-terminal drop. This is the only sweep that removes work
		// nothing else carried forward — a summary REPLACES the rows it
		// folds, and this replaces its rows with nothing — so it is the
		// one whose volume an operator has to be able to see before
		// deciding whether ToolFreeMaxAge is too aggressive. The log is
		// the only surface that can carry it without changing an event
		// shape two builds on one stream have to agree on.
		"tool_free_dropped", res.ToolFreeDropped,
		"orphans_dropped", res.OrphansDropped,
		"clusters_compacted", res.ClustersCompacted,
		"budget_skip", res.BudgetSkip,
		"raw_replaced", res.RawReplaced,
		"summarizer_failures", res.SummarizerFailures,
		"compacted_evicted", res.CompactedEvicted,
		"exemplars_evicted", res.ExemplarsEvicted)
	return res, nil
}

func (l *Lifecycle) claim(handle string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, running := l.inflight[handle]; running {
		return false
	}
	l.inflight[handle] = struct{}{}
	return true
}

func (l *Lifecycle) release(handle string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.inflight, handle)
}

// dropNonTerminal removes mid-state raw rows that ended before cutoff.
//
// Scoped to raw rows even though a summary's outcome is derived from its
// members and is always terminal: that derivation lives in another function,
// and a sweep whose safety depends on what a different function computes is a
// sweep that deletes summaries the day someone changes it.
func (l *Lifecycle) dropNonTerminal(ctx context.Context, handle string, cutoff time.Time) (int64, error) {
	return l.exec(ctx, "drop non-terminal episodes",
		`DELETE FROM episodes WHERE agent_handle = ? AND kind = 'raw'
		   AND review_outcome NOT IN `+terminalOutcomes+` AND ended_at < ?`,
		handle, store.EncodeTime(cutoff))
}

// dropToolFree removes raw turns that called no tools and have aged out.
//
// Scoped to raw rows, like every other sweep here: a summary's tool sequence
// is the union of its members' and is never empty, but relying on that would
// make this sweep's safety depend on what another function computes.
//
// The empty sequence is stored as an empty JSON array, so the test is against
// both spellings a writer could leave: a row that never had one and a row
// whose list is present and empty.
func (l *Lifecycle) dropToolFree(ctx context.Context, handle string, cutoff time.Time) (int64, error) {
	return l.exec(ctx, "drop tool-free episodes",
		`DELETE FROM episodes WHERE agent_handle = ? AND kind = 'raw'
		   AND (tool_sequence IS NULL OR tool_sequence IN ('[]', ''))
		   AND ended_at < ?`,
		handle, store.EncodeTime(cutoff))
}

// dropConsolidated removes raw rows a skill absorbed, once the audit grace has
// elapsed.
//
// THE GRACE IS MEASURED FROM ended_at, NOT FROM THE STAMP, and that is a real
// limitation rather than a choice: there is no column recording when the stamp
// landed. A row consolidated later in its life than the grace is dropped by
// the very next pass with no audit window at all — with the shipped 30 days,
// any turn a skill absorbs more than a month after it ran. The index that
// serves this sweep is on (consolidated_into_skill_id, ended_at), so the
// alternative is a schema change, not a different WHERE clause.
func (l *Lifecycle) dropConsolidated(ctx context.Context, handle string, cutoff time.Time) (int64, error) {
	return l.exec(ctx, "drop consolidated episodes",
		`DELETE FROM episodes WHERE agent_handle = ? AND kind = 'raw'
		   AND consolidated_into_skill_id IS NOT NULL AND ended_at < ?`,
		handle, store.EncodeTime(cutoff))
}

// MarkConsolidated stamps rows a skill absorbed, so the sweep above can
// eventually drop them. Reports how many rows the stamp landed on.
//
// FIRST STAMP WINS: one turn can feed several drafts, and the column is the
// audit trail for which skill claimed it; overwriting would name the latest
// claimant, not the one whose grace window the row is serving out.
//
// Raw rows only: a summary has no tool arguments or transcript to draft from,
// and the sweep is scoped to raw rows anyway.
//
// Seat-scoped for the reason [deleteEpisodes] is: a stamp is a deletion on a
// 30-day fuse, so another seat's id must land nowhere.
func (l *Lifecycle) MarkConsolidated(ctx context.Context, handle, skillID string, episodeIDs []string) (int64, error) {
	if handle == "" || skillID == "" {
		return 0, fmt.Errorf("learning: a consolidation stamp needs a seat and a skill")
	}
	if len(episodeIDs) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(episodeIDs)+2)
	args = append(args, skillID, handle)
	for _, id := range episodeIDs {
		args = append(args, id)
	}
	return l.exec(ctx, "mark episodes consolidated",
		`UPDATE episodes SET consolidated_into_skill_id = ?
		 WHERE agent_handle = ? AND kind = 'raw'
		   AND consolidated_into_skill_id IS NULL
		   AND id IN (`+placeholders(len(episodeIDs))+`)`, args...)
}

// evictCompacted drops summaries older than cutoff, and the exemplars they
// anchor.
//
// The exemplars go WITH their summary, in the same transaction. They are the
// one kind of raw row no sweep can otherwise reach — retired from compaction
// (see [Lifecycle.candidates]), terminal so the mid-state sweep skips them,
// unstamped so the consolidation sweep skips them. Leaving them behind turns
// the one knob an operator sets for a hard storage cap into a slow leak of two
// context-free turns per evicted cluster, and what survives is the half with
// no summary attached.
func (l *Lifecycle) evictCompacted(ctx context.Context, handle string, cutoff time.Time) (rows, exemplars int64, err error) {
	err = l.db.Tx(ctx, func(tx *sql.Tx) error {
		// The read is its own scope so its Rows is closed before the
		// deletes run. A transaction holds ONE connection, and a statement
		// issued while a result set on it is still open deadlocks against
		// itself rather than erroring.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		ids, anchors, err := doomedSummaries(ctx, tx, handle, cutoff)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if exemplars, err = deleteEpisodes(ctx, tx, handle, anchors); err != nil {
			return err
		}
		rows, err = deleteEpisodes(ctx, tx, handle, ids)
		return err
	})
	if err != nil {
		return 0, 0, fmt.Errorf("learning: evict compacted episodes for %s: %w", handle, err)
	}
	return rows, exemplars, nil
}

// doomedSummaries reads the summaries past cutoff and the exemplars they
// anchor.
func doomedSummaries(ctx context.Context, tx *sql.Tx, handle string, cutoff time.Time) (ids, anchors []string, err error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, exemplar_turn_ids FROM episodes
		 WHERE agent_handle = ? AND kind = 'compacted' AND ended_at < ?`,
		handle, store.EncodeTime(cutoff))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, exemplarJSON string
		if err := rows.Scan(&id, &exemplarJSON); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		anchors = append(anchors, parseList(exemplarJSON)...)
	}
	return ids, anchors, rows.Err()
}

// compact is the LLM-driven step: fetch, recover, cluster, fold.
func (l *Lifecycle) compact(ctx context.Context, handle string, now time.Time, res *PassResult) error {
	// OVER THE CAP, THE CAP BINDS: rows younger than MinAge fold too, down
	// to the synthesizer's window, so a busy seat's raw rows stop growing.
	cutoff := now.Add(-l.opts.MinAge)
	if _, over, err := l.RawCount(ctx, handle); err != nil {
		return err
	} else if over {
		cutoff = now.Add(-l.opts.CapAge)
	}
	candidates, err := l.candidates(ctx, handle, cutoff)
	if err != nil {
		return err
	}
	anchors, err := l.anchors(ctx, handle)
	if err != nil {
		return err
	}

	live, orphans := splitOrphans(candidates, anchors, l.opts.JaccardThreshold)
	n, err := l.sweepOrphans(ctx, handle, orphans)
	res.OrphansDropped = int(n)
	if err != nil {
		return err
	}

	asked := false
	for _, cluster := range clusterByTools(live, l.opts.JaccardThreshold) {
		if len(cluster) < l.opts.MinClusterSize {
			continue
		}
		// ONCE, BEFORE THE FIRST FOLD: a seat with nothing to fold asks
		// nothing, so no refusal is stamped for a pass that made no call.
		if !asked {
			asked = true
			if res.BudgetSkip = budgetSkip(ctx, l.opts.Budget, "episode_compaction", handle); res.BudgetSkip != "" {
				return nil
			}
		}
		deleted, answered, err := l.foldCluster(ctx, handle, cluster)
		if err != nil {
			return err
		}
		if !answered {
			res.SummarizerFailures++
			continue
		}
		res.ClustersCompacted++
		res.RawReplaced += int(deleted)
	}
	return nil
}

// candidates returns the raw rows eligible for folding, oldest first.
//
// Eligible is terminal ([terminalOutcomes]), unstamped, old, and foldable:
// tool-free turns and retired exemplars ([splitOrphans]) are excluded BEFORE
// the limit, since neither leaves the OLDEST BatchSize window by folding and
// would fill it until compaction stopped for the seat. A tool-using turn no
// cluster reaches has no horizon and can still fill it.
//
// ORDERED BY (ended_at, id); the id is load bearing. Clustering is greedy over
// this order, so passes disagreeing on rows stamped in one microsecond build
// different clusters and fold keys and write summaries claiming each other's
// turns. SQL leaves the order of equal sort keys unspecified.
func (l *Lifecycle) candidates(ctx context.Context, handle string, cutoff time.Time) ([]Episode, error) {
	rows, err := l.db.SQL().QueryContext(ctx,
		`SELECT `+episodeColumns+` FROM episodes
		 WHERE agent_handle = ? AND kind = 'raw'
		   AND consolidated_into_skill_id IS NULL
		   AND review_outcome IN `+terminalOutcomes+`
		   AND ended_at < ?
		   AND tool_sequence IS NOT NULL AND tool_sequence NOT IN ('[]', '')
		   AND id NOT IN (
		       SELECT exemplar.value FROM episodes summary,
		              json_each(summary.exemplar_turn_ids) exemplar
		       WHERE summary.agent_handle = ? AND summary.kind = 'compacted'
		         AND json_valid(summary.exemplar_turn_ids))
		 ORDER BY ended_at ASC, id ASC LIMIT ?`,
		handle, store.EncodeTime(cutoff), handle, l.opts.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("learning: compaction candidates for %s: %w", handle, err)
	}
	return collectEpisodes(rows)
}

// anchor is one existing summary, reduced to what the recovery sweep needs:
// the span it claims, the tool shape it claims, and the rows it kept.
type anchor struct {
	startedAt time.Time
	endedAt   time.Time
	tools     []string
	exemplars []string
}

// covers reports whether an episode falls inside the span this summary claims.
//
// HALF-OPEN at the top, deliberately. A summary's end is the instant its
// newest member ended, and that member is an exemplar whenever any are kept —
// so nothing a crash left behind sits exactly on the boundary. What does sit
// there is a row the batch limit cut off: candidates come back oldest-first,
// so a tie at the last instant of a full batch leaves its twin for the next
// pass, and a closed interval would sweep that twin as though a summary
// already counted it.
func (a anchor) covers(ep Episode) bool {
	return !ep.EndedAt.Before(a.startedAt) && ep.EndedAt.Before(a.endedAt)
}

// anchors loads every summary the seat holds.
//
// The whole per-seat set, unbounded by time: a seat accumulates one summary
// per cluster per pass, which is at most BatchSize/MinClusterSize per pass and
// in practice a few dozen a year, and each row is read down to five small
// columns. CompactedMaxAge is the knob for a deployment where that stops being
// true.
func (l *Lifecycle) anchors(ctx context.Context, handle string) ([]anchor, error) {
	rows, err := l.db.SQL().QueryContext(ctx,
		`SELECT started_at, ended_at, tool_sequence, exemplar_turn_ids
		 FROM episodes WHERE agent_handle = ? AND kind = 'compacted'`, handle)
	if err != nil {
		return nil, fmt.Errorf("learning: compaction anchors for %s: %w", handle, err)
	}
	defer rows.Close()
	var out []anchor
	for rows.Next() {
		var (
			started, ended    int64
			toolsJSON, exJSON string
		)
		if err := rows.Scan(&started, &ended, &toolsJSON, &exJSON); err != nil {
			return nil, fmt.Errorf("learning: scan compaction anchor: %w", err)
		}
		out = append(out, anchor{
			startedAt: store.DecodeTime(started),
			endedAt:   store.DecodeTime(ended),
			tools:     parseList(toolsJSON),
			exemplars: parseList(exJSON),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: read compaction anchors: %w", err)
	}
	return out, nil
}

// splitOrphans divides candidates into rows still to be folded and rows an
// existing summary already covers.
//
// A RETIRED EXEMPLAR (kept by a fold) is dropped, or it would be counted twice
// and deleted, orphaning its summary's drill-down. An ORPHAN (meant to be
// deleted, was not) would get a second summary ([Lifecycle.sweepOrphans]).
//
// RETIREMENT IS CHECKED ACROSS EVERY ANCHOR BEFORE COVERAGE: overlapping
// summaries can put one's exemplar inside another's window, and deciding per
// anchor would depend on database row order, deleting a live anchor.
func splitOrphans(candidates []Episode, anchors []anchor, threshold float64) (live, orphans []Episode) {
	for _, ep := range candidates {
		switch {
		case retiredExemplar(anchors, ep.ID):
		case coveredBySummary(anchors, ep, threshold):
			orphans = append(orphans, ep)
		default:
			live = append(live, ep)
		}
	}
	return live, orphans
}

func retiredExemplar(anchors []anchor, id string) bool {
	for _, a := range anchors {
		if slices.Contains(a.exemplars, id) {
			return true
		}
	}
	return false
}

func coveredBySummary(anchors []anchor, ep Episode, threshold float64) bool {
	for _, a := range anchors {
		if a.covers(ep) && toolJaccard(ep.ToolSequence, a.tools) >= threshold {
			return true
		}
	}
	return false
}

// sweepOrphans deletes rows an existing summary already counts.
//
// This is the recovery half of "the summary landed and the deletes did not".
// The fold writes both in ONE transaction, so that state is unreachable through
// this code (proven by failing the delete under a fault-injecting driver), but
// still reachable from outside: a backup restored mid-transaction, an operator
// re-inserting exported rows, a non-transactional future backend.
//
// Folding them again would delete them too, after minting a second summary
// over the same turns; they already sit inside a span some summary claims.
func (l *Lifecycle) sweepOrphans(ctx context.Context, handle string, orphans []Episode) (int64, error) {
	if len(orphans) == 0 {
		return 0, nil
	}
	ids := make([]string, len(orphans))
	for i, ep := range orphans {
		ids[i] = ep.ID
	}
	n, err := l.tx(ctx, "sweep orphaned episodes", func(tx *sql.Tx) (int64, error) {
		return deleteEpisodes(ctx, tx, handle, ids)
	})
	if err != nil {
		return 0, err
	}
	// WARN, not INFO, and behind the early return above: on a transactional
	// store this cannot happen, so a line here means something outside the
	// engine put rows back. Firing it on every quiet pass with a count of
	// zero is how that signal stops meaning anything.
	log.WarnContext(ctx, "episode_compaction_orphans_dropped",
		"agent_handle", handle, "dropped", n)
	return n, nil
}

// foldCluster summarises one cluster and replaces its members with the
// summary, keeping the exemplars.
//
// Returns rows removed, whether the model answered, and a store error; no
// answer is not a failed pass (the rows remain).
//
// ONE TRANSACTION: deleting first and failing loses turns, writing first and
// failing leaves a summary counting rows every later pass folds again.
// The work key names the FOLD (from the member ids), so re-folding a set
// collides with the unique (agent_handle, work_key) index; the "compact:"
// prefix keeps it apart from a turn's key. A COLLIDING INSERT STILL DELETES:
// that is the recovery.
func (l *Lifecycle) foldCluster(ctx context.Context, handle string, cluster []Episode) (int64, bool, error) {
	exemplars, doomed := splitExemplars(cluster, l.opts.ExemplarCount)
	summary, err := l.sum.Summarize(ctx, Cluster{
		Handle: handle, Role: cluster[0].Role, Episodes: cluster,
	})
	if err != nil {
		log.WarnContext(ctx, "episode_cluster_not_summarised",
			"agent_handle", handle, "cluster_size", len(cluster), "error", err)
		return 0, false, nil
	}

	ids := make([]string, len(doomed))
	for i, ep := range doomed {
		ids[i] = ep.ID
	}
	row := l.buildCompacted(handle, cluster, exemplars, summary)
	deleted, err := l.tx(ctx, "fold episode cluster", func(tx *sql.Tx) (int64, error) {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := insertSummaryTx(ctx, tx, row); err != nil {
			return 0, err
		}
		return deleteEpisodes(ctx, tx, handle, ids)
	})
	if err != nil {
		return 0, false, err
	}
	log.InfoContext(ctx, "episode_cluster_compacted",
		"agent_handle", handle, "cluster_size", len(cluster),
		"exemplars_kept", len(exemplars), "raw_deleted", deleted,
		"pattern", row.CommonTaskPattern)
	return deleted, true, nil
}

// splitExemplars picks the members that stay raw and the ones that go.
//
// THE MOST RECENT members, so a drill-down shows what the seat does NOW rather
// than what it did when the pattern started. Ties break on the id descending,
// the same total order recall ranks by and for the same reason: two turns
// stamped in the same microsecond must not make the choice depend on the order
// the database happened to return them, or two nodes folding one cluster keep
// different rows and derive different fold keys.
//
// At most len-1, always: a fold that keeps every member is a summary with
// nothing under it.
func splitExemplars(cluster []Episode, want int) (exemplars, doomed []Episode) {
	if want > len(cluster)-1 {
		want = len(cluster) - 1
	}
	if want < 0 {
		want = 0
	}
	ordered := slices.Clone(cluster)
	// NEWEST FIRST, ties broken by the higher id — both keys descend, so
	// both compares take their arguments reversed.
	slices.SortFunc(ordered, func(a, b Episode) int {
		return cmp.Or(b.EndedAt.Compare(a.EndedAt), cmp.Compare(b.ID, a.ID))
	})
	exemplars = ordered[:want]
	kept := make([]string, len(exemplars))
	for i, ep := range exemplars {
		kept[i] = ep.ID
	}
	for _, ep := range cluster {
		if !slices.Contains(kept, ep.ID) {
			doomed = append(doomed, ep)
		}
	}
	return exemplars, doomed
}

// buildCompacted assembles the summary row.
//
// EVERY NUMBER ON IT COMES FROM THE DATA; only the prose comes from the model.
// The success rate is the members' own outcomes, and review_outcome is derived
// from that rate rather than from the model's word for it — the outcome column
// is what "my last N successful turns" filters on, and a cluster that was 90%
// failed once surfaced under "done" because the model called it done.
func (l *Lifecycle) buildCompacted(handle string, cluster, exemplars []Episode, s Summary) Episode {
	done, total := 0, time.Duration(0)
	started, ended := cluster[0].StartedAt, cluster[0].EndedAt
	for _, ep := range cluster {
		if ep.ReviewOutcome == "done" {
			done++
		}
		total += ep.Duration
		if ep.StartedAt.Before(started) {
			started = ep.StartedAt
		}
		if ep.EndedAt.After(ended) {
			ended = ep.EndedAt
		}
	}
	rate := float64(done) / float64(len(cluster))
	outcome := "failed"
	if rate >= 0.5 {
		outcome = "done"
	}
	ids := make([]string, len(cluster))
	exemplarIDs := make([]string, len(exemplars))
	for i, ep := range cluster {
		ids[i] = ep.ID
	}
	for i, ep := range exemplars {
		exemplarIDs[i] = ep.ID
	}
	pattern := strings.TrimSpace(s.CommonTaskPattern)
	if pattern == "" {
		// The agent reads this line as the whole reason the row exists.
		// An empty one renders as a count with no claim attached, which
		// reads like a bug in the dashboard rather than a thin summary.
		pattern = "(unspecified)"
	}
	return Episode{
		// RANDOM id, deriving nothing. The insert resolves a conflict on
		// (agent_handle, work_key) and only that one, so an id derived
		// from the same members would raise a primary-key violation on
		// exactly the re-fold the work key is there to absorb.
		ID:     uuid.NewString(),
		Handle: handle,
		Role:   cluster[0].Role,
		// turn_id is EMPTY, not a synthetic uuid: this row is not a turn,
		// and a fresh uuid there is an id that matches nothing anywhere
		// while looking exactly like one that does. The turns are in
		// ExemplarTurnIDs — which, despite the column's name, holds
		// EPISODE ids, because drilling in is a row lookup.
		StartedAt: started,
		EndedAt:   ended,
		// tool_sequence is the representative's, the shape every other
		// member was matched against.
		ToolSequence: slices.Clone(cluster[0].ToolSequence),
		// skills_used stays empty. A union over the members would make
		// one row claim every skill the pattern ever touched, and the
		// column is per-turn everywhere else that reads it.
		ReviewOutcome:     outcome,
		Duration:          total,
		Kind:              KindCompacted,
		Count:             len(cluster),
		WorkKey:           foldKey(ids),
		ExemplarTurnIDs:   exemplarIDs,
		CommonTaskPattern: pattern,
		CommonOutcome:     strings.TrimSpace(s.CommonOutcome),
		SuccessRate:       rate,
		SubjectsInvolved:  cleanStrings(s.SubjectsInvolved),
		NotablePatterns:   strings.TrimSpace(s.NotablePatterns),
	}
}

// foldKey is the stable identity of a fold over exactly these members.
//
// It reuses the turn-key grammar rather than hashing here: that function
// already sorts, deduplicates and answers "" for an empty set, and a second
// implementation of a key that a unique index depends on is a second way for
// two nodes to disagree about whether they did the same work.
func foldKey(episodeIDs []string) string {
	key := workkey.Derive(episodeIDs)
	if key == "" {
		return ""
	}
	return "compact:" + key
}

// clusterByTools pools turns by tool-sequence overlap, greedily and in one
// pass.
//
// The same algorithm as skill-drafting clustering, deliberately: if compaction
// and synthesis disagreed on "the same work", a fold would drop turns the other
// still drafts from. Each candidate is compared only against a cluster's first
// member, so the result depends on input order, which [Lifecycle.candidates]
// makes total.
//
// A turn with NO TOOLS is skipped (Jaccard over an empty set is undefined), so
// it is never compacted.
func clusterByTools(episodes []Episode, threshold float64) [][]Episode {
	var clusters [][]Episode
	for _, ep := range episodes {
		if len(ep.ToolSequence) == 0 {
			continue
		}
		placed := false
		for i, cluster := range clusters {
			if toolJaccard(ep.ToolSequence, cluster[0].ToolSequence) >= threshold {
				clusters[i] = append(cluster, ep)
				placed = true
				break
			}
		}
		if !placed {
			clusters = append(clusters, []Episode{ep})
		}
	}
	return clusters
}

// toolJaccard is set overlap between two tool sequences: |A∩B| / |A∪B| over
// the distinct tools, ignoring order and repetition.
//
// One function for all five callers (the two clusterers, the promoter, the
// near-duplicate check, the lifecycle fold), so skills are clustered and
// deduplicated by one threshold rule.
//
// Empty on either side scores 0, not 1: calling two tool-free turns a perfect
// match would pool every one a seat ran into one "pattern". That guard also
// makes the division safe, since the union is then at least 1.
func toolJaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	left := make(map[string]struct{}, len(a))
	for _, s := range a {
		left[s] = struct{}{}
	}
	right := make(map[string]struct{}, len(b))
	for _, s := range b {
		right[s] = struct{}{}
	}
	shared := 0
	for s := range left {
		if _, both := right[s]; both {
			shared++
		}
	}
	return float64(shared) / float64(len(left)+len(right)-shared)
}

// ---- storage helpers ------------------------------------------------- //

// insertSummaryTx writes one compacted row inside a caller's transaction.
//
// It binds [episodeInsertSQL], the same statement Episodes.Append uses, so a
// summary row is written through exactly the columns every reader scans. The
// conflict clause on that statement is what makes a repeated fold a no-op.
//
// WITH NO VECTOR, whatever the row carries: a summary is never embedded (see
// [Summary]), and similarity recall reads raw turns only.
func insertSummaryTx(ctx context.Context, tx *sql.Tx, ep Episode) error {
	_, err := tx.ExecContext(ctx, episodeInsertSQL, episodeInsertArgs(ep, nil, nil)...)
	return err
}

// deleteEpisodes removes rows by id, SCOPED TO THE SEAT.
//
// The scope is not redundant with where the ids come from. Every id list here
// is assembled in Go from several queries, and one delete statement standing
// between a bad list and another seat's memory is worth the clause: an id that
// does not belong to this seat deletes nothing instead of deleting somebody
// else's turns.
func deleteEpisodes(ctx context.Context, tx *sql.Tx, handle string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, handle)
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM episodes WHERE agent_handle = ? AND id IN (`+placeholders(len(ids))+`)`,
		args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// exec runs one statement and reports the rows it touched.
func (l *Lifecycle) exec(ctx context.Context, what, query string, args ...any) (int64, error) {
	res, err := l.db.SQL().ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("learning: %s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("learning: %s: %w", what, err)
	}
	return n, nil
}

// tx runs fn in a transaction and reports the count it produced. A failed
// transaction reports zero, never the count fn reached before it rolled back.
func (l *Lifecycle) tx(ctx context.Context, what string, fn func(*sql.Tx) (int64, error)) (int64, error) {
	var n int64
	err := l.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = fn(tx)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("learning: %s: %w", what, err)
	}
	return n, nil
}

// placeholders renders an IN list of n binds.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func cleanStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---- the model-backed summarizer ------------------------------------- //

// CompleteFunc is one call to a model on one seat's behalf: the HANDLE of the
// seat whose chain answers, a system prompt, a user prompt, and the text that
// came back.
//
// The narrowest stand-in for "an LLM": no provider, message types or
// telemetry. The auxiliary provider chain, token budget and usage event live on
// the engine's side, so this package imports nothing but the store.
//
// The HANDLE, not the display name, addresses a seat (inbox, agent id, org
// chart resolution) and is what the rows are keyed on (see the package doc); a
// name fails on companies whose seats declare handles. It is an identifier, not
// a resolved provider, which would pull provider types into this package.
type CompleteFunc func(ctx context.Context, handle, system, user string) (string, error)

// FitFunc rewrites one text that will not fit budget bytes, on one seat's
// behalf — the engine's compactor, behind a function for [CompleteFunc]'s
// reason: the compactor and the chain it runs on stay on the engine's side of
// it, metered there like any auxiliary call. The seat is named by its HANDLE,
// for [CompleteFunc]'s reason too. field says which of a turn's two accounts
// the text is, because what a rewrite must keep differs: a task keeps what was
// asked and where, an outcome what was delivered.
type FitFunc func(ctx context.Context, handle string, field EpisodeField, text string, budget int) (string, error)

// EpisodeField names one of a turn's prose fields a [FitFunc] is asked to
// condense.
type EpisodeField string

const (
	// FieldTask is [Episode.TaskSummary]: the label of the event that woke
	// the turn — the only account of what it was asked a stored episode has.
	FieldTask EpisodeField = "task"
	// FieldOutcome is [Episode.PlanSummary]: what the turn did.
	FieldOutcome EpisodeField = "outcome"
)

// Valid reports whether the field is one this package names.
func (f EpisodeField) Valid() bool { return f == FieldTask || f == FieldOutcome }

// NewSummarizer builds the model-backed [Summarizer]: it renders the cluster,
// makes one call, and parses what comes back.
//
// fit condenses a turn's field past [perTurnDetail] before the cluster is
// rendered — see [RenderCluster]. Nil condenses nothing, and such a field is
// carried whole.
func NewSummarizer(complete CompleteFunc, fit FitFunc) Summarizer {
	return modelSummarizer{complete: complete, fit: fit}
}

type modelSummarizer struct {
	complete CompleteFunc
	fit      FitFunc
}

func (m modelSummarizer) Summarize(ctx context.Context, c Cluster) (Summary, error) {
	if m.complete == nil {
		return Summary{}, errors.New("learning: summarizer has no model")
	}
	if len(c.Episodes) == 0 {
		return Summary{}, errors.New("learning: summarizer got an empty cluster")
	}
	// THE CLUSTER'S HANDLE, not its role: [Cluster] carries both because
	// they answer different questions, and this one is "which seat's
	// chain" — it selects the seat's auxiliary provider chain and
	// attributes the token spend. Dropping it here would compact every
	// seat's memory on whichever single model the wiring happened to close
	// over, in a company whose whole point is that seats differ. The role
	// is prose, and it reaches the model inside the rendered cluster where
	// prose belongs.
	//
	// A TOKEN FOR THE CALL, though it carries no fence: every field is a
	// label or a seat's account, neutralised under it ([RenderCluster]).
	raw, err := m.complete(ctx, c.Handle, CompactorSystemPrompt,
		RenderCluster(provenance.NewBoundary(), c, m.condense(ctx, c)))
	if err != nil {
		return Summary{}, err
	}
	return ParseSummary(raw)
}

// CompactorSystemPrompt instructs a model to fold a cluster into one summary.
//
// It does NOT ask for the success rate, and that is the one deliberate
// difference from the prompt this was ported from. The rate on the row is
// computed from the members' own outcomes, so asking for a number that is then
// discarded spends tokens to invite a claim contradicting the row it appears
// on.
//
// "(heterogeneous)" is no sentinel the code reads: [ParseSummary] stores it
// like any pattern. It is the words the seat later reads on the row — a fold
// whose turns shared nothing says so, rather than naming a pattern none of
// them had.
const CompactorSystemPrompt = `You are an agent-learning episode compactor. You receive N similar turns of
one agent and fold them into ONE summary, which replaces them in the agent's
episode store.

Answer with one JSON object and nothing else:

{
  "common_task_pattern": "<one short declarative sentence: the recurring task, which the agent reads as 'you have done this kind of work N times'>",
  "common_outcome": "done" | "failed",
  "subjects_involved": ["<the distinct named counterparties that recur: chat users, ticket reporters; empty if none>"],
  "notable_patterns": "<one to three sentences on how the turns varied: edge cases, hand-offs>"
}

If the turns share no coherent pattern, answer {"common_task_pattern": "(heterogeneous)"}.
`

// perTurnDetail is the budget one turn's task or plan has in the prompt, past
// which it is CONDENSED rather than carried — never cut.
//
// A cluster can be as large as the batch (200 turns), and every turn renders
// four lines. At 280 bytes each for the task and its plan that is roughly 120
// KB, about 30k tokens, in a single call — affordable once a month per seat.
// Without a bound one turn carrying a pasted stack trace sets the size of the
// call; with a CUT, the compactor inferred the pattern from that turn's first
// 280 bytes, which is the paste's opening and none of what the turn was for.
// So a field past it is rewritten by the seat's auxiliary model first — only
// the outliers, which is a handful of calls on a pass that runs monthly.
const perTurnDetail = 280

// condense rewrites every task and outcome of the cluster past
// [perTurnDetail], keyed by the flattened text so [RenderCluster] finds each
// one. A field that cannot be rewritten is left out of the map and rendered
// whole.
//
// [condenseParallel] at a time, because a long-lived seat's cluster is mostly
// outliers on its outcome field — a turn's account of what landed is rarely
// under 280 bytes — so a full batch is a few hundred rewrites, and one at a
// time each under its own call budget is a pass that holds its lease for
// hours.
func (m modelSummarizer) condense(ctx context.Context, c Cluster) map[string]string {
	if m.fit == nil {
		return nil
	}
	type job struct {
		field EpisodeField
		flat  string
	}
	var jobs []job
	seen := map[string]bool{}
	for _, ep := range c.Episodes {
		for _, f := range []struct {
			field EpisodeField
			text  string
		}{{FieldTask, ep.TaskSummary}, {FieldOutcome, ep.PlanSummary}} {
			flat := flatten(f.text)
			if len(flat) <= perTurnDetail || seen[flat] {
				continue
			}
			seen[flat] = true
			jobs = append(jobs, job{f.field, flat})
		}
	}
	var (
		mu  sync.Mutex
		out = make(map[string]string, len(jobs))
	)
	var group errgroup.Group
	group.SetLimit(condenseParallel)
	for _, j := range jobs {
		group.Go(func() error {
			rewritten, err := m.fit(ctx, c.Handle, j.field, j.flat, perTurnDetail)
			if err != nil {
				log.DebugContext(ctx, "episode_field_not_condensed", "agent_handle", c.Handle,
					"field", string(j.field), "bytes", len(j.flat), "error", err.Error())
				return nil
			}
			mu.Lock()
			out[j.flat] = flatten(rewritten)
			mu.Unlock()
			return nil
		})
	}
	_ = group.Wait() // every job answers nil: a failed rewrite is a whole field
	return out
}

// condenseParallel is how many of a cluster's rewrites run at once — the
// compactor's own chunk parallelism, so a pass asks a provider for no more
// at once than one long rewrite already does.
const condenseParallel = 4

// RenderCluster is the user half of the compaction prompt: the cluster as a
// numbered list of turn summaries.
//
// condensed is the rewrite of each task or plan past [perTurnDetail], keyed by
// its flattened text; a field past the budget that is not in it renders whole.
// NOTHING IS CUT, the tool sequence included.
//
// EVERY FIELD IS NEUTRALISED under b (ADR-0040): what woke a turn names its
// sender as they chose to be named, what it did is the seat's own account of
// work that read other people's words, and a rewrite is a model's answer —
// and what the compactor writes back is a pattern the seat reads as its own
// history, so nothing here may open or close a fence.
func RenderCluster(b provenance.Boundary, c Cluster, condensed map[string]string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Cluster of %d similar agent turns from one agent.\n\n", len(c.Episodes))
	out.WriteString("Turns (numbered):\n")
	for i, ep := range c.Episodes {
		tools := "(none)"
		if len(ep.ToolSequence) > 0 {
			// WHOLE. The compactor is inferring "how this agent does
			// this kind of work", and a shortened tool sequence reads as
			// a shorter procedure — which is the pattern it then writes
			// down. Names are short; a turn's whole sequence is cheaper
			// than the wrong procedure.
			tools = strings.Join(ep.ToolSequence, ", ")
		}
		// UNDER THE NAMES OF WHAT THEY ARE (see [describeTurn]): the
		// label of the event that woke the turn, and what the turn did.
		fmt.Fprintf(&out, "%d. [%s] woken by: %s\n", i, provenance.Neutralise(b, ep.ReviewOutcome),
			provenance.Neutralise(b, oneLine(ep.TaskSummary, condensed)))
		fmt.Fprintf(&out, "   did: %s\n", provenance.Neutralise(b, oneLine(ep.PlanSummary, condensed)))
		fmt.Fprintf(&out, "   tools: %s\n", tools)
		fmt.Fprintf(&out, "   when: %s\n", ep.EndedAt.UTC().Format(time.DateOnly))
	}
	out.WriteString("\nEmit one JSON object summarising the recurring pattern across " +
		"these turns.  Strict JSON, no prose.\n")
	return out.String()
}

// oneLine flattens one field of a turn and takes its rewrite where it has one.
//
// Newlines become spaces because the rendering is a numbered list and a turn
// summary containing its own newlines would look like more turns than there
// are — a model that miscounts the cluster writes the pattern of a cluster
// that does not exist.
func oneLine(s string, condensed map[string]string) string {
	flat := flatten(s)
	if rewritten, ok := condensed[flat]; ok && len(flat) > perTurnDetail {
		return "(condensed) " + rewritten
	}
	return flat
}

func flatten(s string) string { return strings.Join(strings.Fields(s), " ") }

// ParseSummary reads a model's answer, tolerating prose around the JSON.
//
// Down [modelJSONCandidates], so a fenced answer and one wrapped in an apology
// both recover, and a clean one parses exactly as sent.
//
// FIELD BY FIELD once an object is found, never all-or-nothing. The pattern
// sentence is the only part the agent actually reads, and decoding the whole
// object into one struct throws it away whenever any other field comes back
// the wrong shape — a model answering "subjects_involved": "nobody" instead of
// a list would cost the summary the LLM call just bought.
func ParseSummary(raw string) (Summary, error) {
	text := strings.TrimSpace(raw)
	for _, candidate := range modelJSONCandidates(text) {
		var obj map[string]json.RawMessage
		// A nil map with no error is `null`, which is valid JSON and not an
		// object. Without the check it would parse as a summary with every
		// field empty, and the row would land carrying nothing.
		if json.Unmarshal([]byte(candidate), &obj) != nil || obj == nil {
			continue
		}
		return Summary{
			CommonTaskPattern: jsonText(obj["common_task_pattern"]),
			CommonOutcome:     jsonText(obj["common_outcome"]),
			SubjectsInvolved:  jsonTexts(obj["subjects_involved"]),
			NotablePatterns:   jsonText(obj["notable_patterns"]),
		}, nil
	}
	// THE ANSWER WHOLE: the prompt asks for one JSON object, and the part
	// of an unparseable answer worth reading is rarely its opening.
	return Summary{}, fmt.Errorf("learning: no JSON object in the compactor's answer (%s)", text)
}

// jsonText reads one string field, answering "" for absent or wrong-typed.
func jsonText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// jsonTexts reads one string-list field, answering nil for absent or
// wrong-typed.
func jsonTexts(raw json.RawMessage) []string {
	var out []string
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return cleanStrings(out)
}
