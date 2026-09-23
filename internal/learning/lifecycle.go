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
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/workkey"
)

// The episode compaction lifecycle: the pass that drains a seat's episode
// table.
//
// A seat that keeps working accumulates one raw row per turn forever, and
// nothing else in the system removes one. Recall is what pays for that, and it
// pays LINEARLY: there is still no ANN index reachable from the Go driver,
// re-measured at the pin, so recall visits every embedded row
// the seat owns, at the start of every turn.
//
// The constant shrank when the distance arithmetic moved into the database
// — the rows no longer cross the driver boundary to be decoded
// in Go, which at 5 000 rows of 1 536 dimensions was 144 ms and 35.8 MB
// against 34 ms and 35.8 KB for the same ranking. The SHAPE did not: it is
// still linear in the seat's row count, still with no ceiling, and this pass
// is still the only thing that puts one there. The measurement AFTER the move,
// which is the one this pass is now sized against — 1.9 ms at 100 rows, 3.9 ms
// at 500, 7.3 ms at 1000, 15.2 ms at 2000, 7.5 µs per row — is in
// BenchmarkRecallScan. The pre-move figures it replaces were 8× higher and
// described the Go loop this package deleted.
//
// Four actions, cheapest first, so an expensive one never runs over rows the
// cheap ones were about to delete:
//
//  1. drop mid-state rows — a self_iterate turn is half a turn. Skill
//     synthesis already excludes it, so recall is its only reader and there it
//     is noise.
//  2. drop rows a skill absorbed, past their grace — the skill carries the
//     learning; the grace is the window in which a bad consolidation can still
//     be audited against its sources.
//  3. optionally evict ancient summaries, for a deployment that wants a hard
//     storage cap rather than a decaying long tail — and their anchors, which
//     no other sweep can reach.
//  4. compact the rest — cluster by tool-sequence similarity and fold each
//     cluster into ONE summary row, keeping a couple of members as drill-down
//     anchors. This is the only step that costs an LLM call, which is why it
//     goes last: everything above it removes rows it would otherwise pay a
//     model to summarise.

// ErrPassInFlight reports that a pass for this seat is already running.
//
// A sentinel rather than a silent no-op, so a caller can tell a seat another
// pass is working on from a pass that found nothing to do.
//
// THE ENGINE NEVER MEETS IT: [Background] runs one pass at a time, from one
// loop. It stays because [Lifecycle.Pass] is exported and this is the only
// thing that stops two concurrent callers summarising one seat's clusters
// twice.
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

	// Embedding is the summary's vector, or nil when none could be made.
	//
	// It comes from the summarizer because that is the component already
	// talking to a model — and it is optional for the same reason the raw
	// rows' vectors are: a compacted row with no vector is skipped by
	// recall and still read by every time-window query. Losing the summary
	// because its vector could not be computed would be the worse trade.
	Embedding []float32
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
	// It exists because such a turn can never be COMPACTED: clustering
	// pools turns by tool-sequence overlap, Jaccard over an empty set is
	// undefined, and there is no other similarity signal here — so for a
	// chat-only seat the fold that bounds every other seat's raw rows
	// never fires and the table only grows. Every one of those rows is
	// scanned and cosined at the start of every turn, for ever.
	//
	// A horizon rather than a cap, because these rows have no cluster to
	// be a member of: there is no summary that would carry their content
	// forward, so the only question is how long a chat turn stays worth
	// scanning. See defaultToolFreeMaxAge.
	ToolFreeMaxAge time.Duration

	// CompactedMaxAge evicts summaries themselves. Zero keeps them
	// forever, which is the default: they are small, and losing one loses
	// the only record of a whole era of a seat's work.
	//
	// It is a LONG-TAIL knob — years, not weeks. A summary carries the
	// ended_at of the newest turn it covers, and the turns it covers are at
	// least MinAge old, so a horizon anywhere near MinAge produces
	// summaries that are already past it when they are written: the pass
	// spends an LLM call, writes the row, and the next pass deletes it.
	CompactedMaxAge time.Duration
}

const (
	day = 24 * time.Hour

	// defaultThreshold is the raw-row count that makes a pass due.
	//
	// MEASURED, and it is a latency budget rather than a storage one: recall
	// scans every embedded row a seat owns, once per turn, at 7.5 µs per
	// row at 1 536 dimensions (BenchmarkRecallScan, re-measured at the pin).
	// 500 rows is ~3.9 ms at the start of every turn, a rounding error
	// next to one LLM round. 2000 is ~15 ms and still climbing linearly,
	// because nothing else in the system bounds this number.
	//
	// THE 62 µs/row THIS REPLACES WAS 8× STALE, and it had already been
	// contradicted inside this package: the ORDER BY moved into the database
	// and only the rows surviving the LIMIT cross the driver boundary now.
	// The threshold does not move with the correction — it is a latency
	// budget and the budget is met by a wide margin — but the ANCHOR does,
	// because the next person to raise this number would have reasoned from
	// a per-row cost that has not been true since the Go loop went.
	//
	// It must stay in step with config.DefaultEpisodeLifecycle, which is
	// what an operator actually edits; the two agreeing is asserted by a
	// test rather than left to whoever edits one of them.
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
	// Ninety days, and the number is the same argument the chat-thread
	// follow retention makes: a quarter is the point past which a
	// conversation has stopped being live on every backend this engine
	// speaks to. A tool-free turn IS a conversation — it answered somebody
	// without doing anything — so its value as "similar prior work" decays
	// with the thread it belonged to, while its cost does not decay at all:
	// it is a row recall scans at every turn start at 7.5 µs, permanently.
	//
	// Deliberately far longer than the other two raw-row horizons here (14
	// and 30 days), because those drop rows that are half-finished or
	// already carried forward by something else, and this one drops the
	// only record of work that really happened. What keeps that honest is
	// the diary: a fact worth remembering past a quarter is one the seat
	// was supposed to persist with reflect_and_persist, which nothing here
	// touches.
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

	// walks is the per-seat state one pass leaves for the next. Every
	// Lifecycle is built with its own; a [Background] replaces it with the
	// one it keeps for the life of the process, which is what lets that
	// state outlive this Lifecycle. See [seatWalks].
	walks atomic.Pointer[seatWalks]
}

// seatWalks is what compaction keeps per seat between one pass and the next:
// which seats have a pass in flight, and where each seat's next window starts.
//
// ITS OWN VALUE, rather than fields of [Lifecycle], because a Lifecycle does
// not live as long as the walk it serves. Every config apply builds the passes
// again and hands them to [Background.Reconfigure], so a position the replaced
// Lifecycle held would send every seat's next window back to its oldest row
// on every apply — and in a company that applies config more often than a
// walk takes, [Lifecycle.compact] would keep reading the same first windows,
// which is the stuck window the walk exists to get out of. [Background] keeps
// one for as long as it runs and hands it to every Lifecycle it is given.
//
// THIS PROCESS'S AND IN MEMORY, and nothing has to agree on it: a position is
// a read position over this node's own copy of the seat's episodes, and
// losing it only makes the next pass start again from the oldest row. A
// restart loses it, and a seat this process has no entry for starts there.
type seatWalks struct {
	mu sync.Mutex

	// inflight is the per-seat single-flight; see [ErrPassInFlight] for
	// when it can fire. Two passes over one seat at once would fetch the
	// same candidates and summarise each cluster twice. The durable half of
	// that guard is the compacted row's work key (see
	// [Lifecycle.foldCluster]), which stops the second summary landing —
	// this half stops the second pass from spending the LLM call at all,
	// and from moving the seat's position under the first. Kept beside the
	// positions for that reason: two Lifecycles sharing positions must
	// share this too.
	inflight map[string]struct{}

	// resume is where each seat's next compaction window starts: the last
	// row of a window that did not reach the end of the eligible rows. See
	// [Lifecycle.compact] for why the windows walk rather than restart.
	resume map[string]windowEnd
}

func newSeatWalks() *seatWalks {
	return &seatWalks{inflight: map[string]struct{}{}, resume: map[string]windowEnd{}}
}

// windowEnd is the sort key of the last row a compaction window read. The
// window after it starts strictly past this (ended_at, id).
type windowEnd struct {
	endedAt time.Time
	id      string
}

// NewLifecycle wraps a database handle and the summarizer that folds
// clusters.
func NewLifecycle(db *store.DB, s Summarizer, o Options) *Lifecycle {
	l := &Lifecycle{db: db, sum: s, opts: o.withDefaults()}
	l.walks.Store(newSeatWalks())
	return l
}

// shareWalks makes this Lifecycle keep its per-seat state in w, so that state
// outlives it. See [seatWalks].
func (l *Lifecycle) shareWalks(w *seatWalks) { l.walks.Store(w) }

// Options returns the knobs in force, defaults applied.
func (l *Lifecycle) Options() Options { return l.opts }

// terminalOutcomes is [SettledOutcomes] as a SQL tuple.
//
// Used twice here, in complementary directions: a compaction candidate must be
// in it, and the mid-state sweep drops everything that is not. Two hand-written
// lists is how a row ends up being neither — a sweep naming 'self_iterate'
// explicitly while the candidate query takes only ('done','failed') leaves any
// other value undroppable AND uncompactable, and it stays in the seat's memory
// forever. That was written when this was a literal, and it was already wrong
// by one: cluster.go held a third copy it could not see.
//
// Spelled into the query rather than bound: these are compile-time constants
// of this package and never caller input, exactly like the state names in
// ListOptions.filter. The day one stops being a bare identifier, the tuple
// this builds would need quoting rules it does not have — which is why a test
// holds every value to that shape rather than trusting the next editor to
// remember it.
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

// PassResult is what one pass did. Every count is what THIS pass read, removed
// or wrote, never a total: two nodes sweeping one seat each report their own
// share, and summing them is the only way to get the total.
type PassResult struct {
	// Candidates is how many rows the compaction window read, and
	// CandidatesResumed says the window started where the seat's previous
	// one stopped rather than at its oldest eligible row.
	//
	// MoreCandidates says eligible rows lay past the window, so the next
	// pass reads on from its end — see [Lifecycle.compact]. It is what
	// tells a pass that folded nothing because nothing in its window could
	// fold from a pass that folded nothing because there was nothing.
	Candidates        int
	CandidatesResumed bool
	MoreCandidates    bool

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
	// They are left raw and tried again when the walk next reaches their
	// window — the rows are still there, so nothing is lost but the call.
	SummarizerFailures int

	CompactedEvicted int
	ExemplarsEvicted int
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
	// ONE seatWalks FOR THE WHOLE PASS, read once: a [Background] that
	// hands this Lifecycle its own mid-pass must not leave the claim in one
	// and the release, or the position, in the other.
	walks := l.walks.Load()
	if !walks.claim(handle) {
		return PassResult{}, ErrPassInFlight
	}
	defer walks.release(handle)

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

	if err := l.compact(ctx, walks, handle, now, &res); err != nil {
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
		"candidates", res.Candidates,
		"candidates_resumed", res.CandidatesResumed,
		"more_candidates", res.MoreCandidates,
		"clusters_compacted", res.ClustersCompacted,
		"raw_replaced", res.RawReplaced,
		"summarizer_failures", res.SummarizerFailures,
		"compacted_evicted", res.CompactedEvicted,
		"exemplars_evicted", res.ExemplarsEvicted)
	return res, nil
}

func (w *seatWalks) claim(handle string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, running := w.inflight[handle]; running {
		return false
	}
	w.inflight[handle] = struct{}{}
	return true
}

func (w *seatWalks) release(handle string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.inflight, handle)
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
		   AND `+toolFree+`
		   AND ended_at < ?`,
		handle, store.EncodeTime(cutoff))
}

// toolFree is the SQL test for a row that called no tools.
//
// ONE SPELLING, read by two queries in opposite directions: the sweep above
// deletes what it matches, and [Lifecycle.candidates] keeps only what it does
// not, because a turn with no tools is one [clusterByTools] can never place.
// Two spellings could disagree about a row, and the row they disagree on is
// one neither reaches: kept out of the fold's window as tool-free, and left
// by the sweep as not.
const toolFree = `(tool_sequence IS NULL OR tool_sequence IN ('[]', ''))`

// retiredExemplars selects every episode id one seat's summaries keep as a
// drill-down exemplar, for [Lifecycle.candidates] to leave out of its window.
// Its one argument is the seat's handle.
//
// A SUMMARY'S LIST RETIRES ITS TEXT ELEMENTS, and only when the list is a
// JSON array. Any other value in the column — not JSON, `null`, an object, a
// bare string — retires nothing, and neither does an element that is not
// text, such as a `null` or a number. The fold writes the column through
// [jsonList], which always produces an array of strings, so every other shape
// is one only a damaged or foreign row carries.
//
// THE TEXT TEST IS WHAT KEEPS THE WINDOW FROM EMPTYING. The candidates query
// reads this through NOT IN, and `id NOT IN (…)` is never true once the list
// holds a single NULL. json_each reports a JSON null as a NULL `value`, so
// without the test one summary whose list read `["a", null]` would stop
// compaction for the whole seat, while each pass logged a window of nothing
// and no more to read.
//
// THE ARRAY TEST IS NESTED IN A CASE rather than written as an AND beside the
// validity test: against the pinned driver, `json_valid(x) AND
// json_type(x) = 'array'` raised `malformed JSON` for an x that was not JSON,
// so the AND did not keep json_type from reading it. The CASE does.
//
// NOT IN, and not a correlated NOT EXISTS that would need no text test,
// because of what the correlated form cost: measured against the pinned
// driver on one seat holding 5 000 raw rows and 300 summaries, it took about
// 2.5 s to read one window, and this form about 0.15 s.
const retiredExemplars = `
	SELECT kept.value FROM episodes summary,
	       json_each(CASE WHEN json_valid(summary.exemplar_turn_ids)
	                      THEN CASE json_type(summary.exemplar_turn_ids)
	                           WHEN 'array' THEN summary.exemplar_turn_ids END
	                 END) kept
	WHERE summary.agent_handle = ? AND summary.kind = 'compacted'
	  AND kept.type = 'text'`

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
// FIRST STAMP WINS: a row already pointing at a skill is left alone. One turn
// can feed several drafts, and the column is the audit trail for which skill
// claimed it — overwriting would make the trail name the most recent claimant
// rather than the one whose grace window the row is serving out.
//
// Raw rows only. A summary is not something a skill absorbs: it has no tool
// arguments and no transcript, so nothing could have been drafted from it, and
// stamping one would hand it to a sweep that is scoped to raw rows anyway.
//
// Seat-scoped for the reason [deleteEpisodes] is: a stamp is a deletion on a
// 30-day fuse, so an id that belongs to another seat must land nowhere rather
// than quietly schedule that seat's turn for removal. A skill belongs to one
// seat, so the caller always has the handle.
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
//
// ONE WINDOW OF [Options.BatchSize] ROWS PER PASS, AND THE WINDOWS WALK. A
// window can hold rows the fold cannot take — a turn whose tool shape nothing
// else matched, a pair short of [Options.MinClusterSize] — and those stay
// where they are, oldest first. A window read from the oldest row every pass
// would find the same ones there every pass once BatchSize of them had piled
// up: it would fold nothing and never read a newer row, and its log line
// would look exactly like a seat with nothing to fold. So a window that did
// not reach the end of the eligible rows leaves the seat's position in
// [seatWalks] at its last row, and the next pass reads the window after it;
// the window that reaches the end clears it, and the pass after that starts
// from the oldest row again. Every eligible row is read once in each walk,
// so a row one pass left raw is read again by the next walk.
//
// A CLUSTER IS FORMED INSIDE ONE WINDOW, which is the cost of bounding one:
// two similar turns either side of a window's edge are not pooled by this
// pass. That is also what bounds the largest cluster, and so the largest
// summarisation prompt, at BatchSize members.
//
// THE POSITION MOVES ONLY WHEN THE WINDOW WAS WORKED THROUGH. A store error
// returns before it moves, so the next pass reads the same window again. A
// cluster the model could not summarise does not hold it back: that cluster
// is still raw, and the next walk reaches it again.
func (l *Lifecycle) compact(ctx context.Context, walks *seatWalks, handle string, now time.Time, res *PassResult) error {
	from, resumed := walks.resumeAt(handle)
	window, more, err := l.candidates(ctx, handle, now.Add(-l.opts.MinAge), from)
	if err != nil {
		return err
	}
	res.Candidates, res.CandidatesResumed, res.MoreCandidates = len(window), resumed, more
	anchors, err := l.anchors(ctx, handle)
	if err != nil {
		return err
	}

	live, orphans := splitOrphans(window, anchors, l.opts.JaccardThreshold)
	n, err := l.sweepOrphans(ctx, handle, orphans)
	res.OrphansDropped = int(n)
	if err != nil {
		return err
	}

	for _, cluster := range clusterByTools(live, l.opts.JaccardThreshold) {
		if len(cluster) < l.opts.MinClusterSize {
			continue
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

	if more {
		last := window[len(window)-1]
		walks.setResume(handle, &windowEnd{endedAt: last.EndedAt, id: last.ID})
	} else {
		walks.setResume(handle, nil)
	}
	return nil
}

// resumeAt reports where a seat's next compaction window starts, and whether
// that is anywhere but the oldest eligible row.
func (w *seatWalks) resumeAt(handle string) (*windowEnd, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	at, ok := w.resume[handle]
	if !ok {
		return nil, false
	}
	return &at, true
}

// setResume records where a seat's next window starts; nil starts it at the
// oldest eligible row.
func (w *seatWalks) setResume(handle string, at *windowEnd) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if at == nil {
		delete(w.resume, handle)
		return
	}
	w.resume[handle] = *at
}

// candidates returns one window of the raw rows eligible for folding, oldest
// first, starting strictly after `after` (nil starts at the oldest), and
// whether more eligible rows lie past it.
//
// Eligible is every row the fold could take and no sweep owns: terminal (see
// [terminalOutcomes]), unstamped, old enough that no other reader still wants
// the detail — and TWO KINDS THE FOLD CAN NEVER TAKE ARE LEFT OUT HERE, IN THE
// QUERY, so they do not occupy the window. A turn that called no tools (see
// [toolFree]) has no tool shape to be clustered by. And a RETIRED EXEMPLAR —
// a member a fold kept raw as its summary's drill-down anchor — must never be
// folded again: it is old and matches its own cluster's shape by
// construction, so it would join the next cluster over the same work, be
// counted a second time and be deleted, leaving the earlier summary pointing
// at a row that no longer exists. See [retiredExemplars] for which values of
// a summary's `exemplar_turn_ids` retire a row.
//
// READ PLUS ONE, and the extra row is not returned: it is the evidence that
// the window did not reach the end, which is what moves the seat's position
// on rather than back to the start.
//
// ORDERED BY (ended_at, id), and the id is load bearing rather than tidy.
// Clustering is greedy over this order, so two passes that disagree about the
// order of two rows stamped in the same microsecond build different clusters,
// derive different fold keys, and each write a summary claiming turns the
// other also claimed. SQL leaves the order of equal sort keys unspecified. It
// is also the key the window resumes on, so a row is in exactly one window of
// a walk even where a window's edge falls between two rows stamped alike.
func (l *Lifecycle) candidates(ctx context.Context, handle string, cutoff time.Time, after *windowEnd) ([]Episode, bool, error) {
	query := `SELECT ` + episodeColumns + ` FROM episodes
		 WHERE agent_handle = ? AND kind = 'raw'
		   AND consolidated_into_skill_id IS NULL
		   AND review_outcome IN ` + terminalOutcomes + `
		   AND ended_at < ?
		   AND NOT ` + toolFree + `
		   AND id NOT IN (` + retiredExemplars + `)`
	args := []any{handle, store.EncodeTime(cutoff), handle}
	if after != nil {
		// Strictly past (ended_at, id), spelled with the range bound
		// first so the index on (agent_handle, kind, ended_at) seeks to
		// it rather than filtering every row before it.
		query += `
		   AND ended_at >= ? AND (ended_at > ? OR id > ?)`
		at := store.EncodeTime(after.endedAt)
		args = append(args, at, at, after.id)
	}
	query += `
		 ORDER BY ended_at ASC, id ASC LIMIT ?`
	args = append(args, l.opts.BatchSize+1)

	rows, err := l.db.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("learning: compaction candidates for %s: %w", handle, err)
	}
	window, err := collectEpisodes(rows)
	if err != nil {
		return nil, false, err
	}
	if len(window) > l.opts.BatchSize {
		return window[:l.opts.BatchSize], true, nil
	}
	return window, false, nil
}

// anchor is one existing summary, reduced to what the recovery sweep needs:
// the span it claims and the tool shape it claims.
type anchor struct {
	startedAt time.Time
	endedAt   time.Time
	tools     []string
}

// covers reports whether an episode falls inside the span this summary claims.
//
// HALF-OPEN at the top, deliberately. A summary's end is the instant its
// newest member ended, and that member is an exemplar whenever any are kept —
// so nothing a crash left behind sits exactly on the boundary. What does sit
// there is a row the batch limit cut off: candidates come back oldest-first,
// so a tie at the last instant of a full batch leaves its twin for the next
// window, and a closed interval would sweep that twin as though a summary
// already counted it.
func (a anchor) covers(ep Episode) bool {
	return !ep.EndedAt.Before(a.startedAt) && ep.EndedAt.Before(a.endedAt)
}

// anchors loads every summary the seat holds.
//
// The whole per-seat set, unbounded by time: a seat accumulates at most one
// summary per cluster a pass folds, and each is read down to three small
// columns. CompactedMaxAge is the knob for a deployment where that stops being
// small.
func (l *Lifecycle) anchors(ctx context.Context, handle string) ([]anchor, error) {
	rows, err := l.db.SQL().QueryContext(ctx,
		`SELECT started_at, ended_at, tool_sequence
		 FROM episodes WHERE agent_handle = ? AND kind = 'compacted'`, handle)
	if err != nil {
		return nil, fmt.Errorf("learning: compaction anchors for %s: %w", handle, err)
	}
	defer rows.Close()
	var out []anchor
	for rows.Next() {
		var (
			started, ended int64
			toolsJSON      string
		)
		if err := rows.Scan(&started, &ended, &toolsJSON); err != nil {
			return nil, fmt.Errorf("learning: scan compaction anchor: %w", err)
		}
		out = append(out, anchor{
			startedAt: store.DecodeTime(started),
			endedAt:   store.DecodeTime(ended),
			tools:     parseList(toolsJSON),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: read compaction anchors: %w", err)
	}
	return out, nil
}

// splitOrphans divides a window into rows still to be folded and rows an
// existing summary already covers.
//
// An ORPHAN is a member the fold meant to delete and did not. Its summary
// already counts it, so folding it again writes a second summary over turns
// the first one claims — the double count this whole mechanism exists to
// prevent. See [Lifecycle.foldCluster] for why that state should be
// unreachable, and [Lifecycle.sweepOrphans] for why it is still handled.
//
// A RETIRED EXEMPLAR NEVER GETS HERE, and that is what keeps it from being
// taken for one: [Lifecycle.candidates] leaves every id a summary's exemplar
// list names out of the window (see [retiredExemplars] for which lists name
// one). Two summaries can claim overlapping spans with the same tool
// shape, so one summary's exemplar can sit inside the other's span, where
// this coverage test alone would call it an orphan and delete a live
// drill-down anchor.
func splitOrphans(window []Episode, anchors []anchor, threshold float64) (live, orphans []Episode) {
	for _, ep := range window {
		if coveredBySummary(anchors, ep, threshold) {
			orphans = append(orphans, ep)
			continue
		}
		live = append(live, ep)
	}
	return live, orphans
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
// The fold below writes both in ONE transaction, so a crash between them is
// not reachable through this code — proven by failing the delete statement
// under a fault-injecting driver and watching the summary roll back with it.
// The state is still reachable from OUTSIDE it: a restore from a backup taken
// mid-transaction, an operator re-inserting rows from an export, a future
// backend whose writes are not transactional.
//
// The alternative to deleting them is folding them again, which deletes them
// too — and mints a second summary over the same turns first. There is no
// third option that keeps the rows: they are already inside a span some
// summary claims.
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

// patternLogDetail is how much of a compacted row's pattern sentence reaches
// the pass's log line.
//
// ONE SHORT SENTENCE IS WHAT WAS ASKED FOR. [CompactorSystemPrompt] requires
// `common_task_pattern` to be "declarative and short", and 120 bytes is around
// twenty English words — so in the ordinary case this cuts nothing at all, and
// what it bounds is the answer that ignored the instruction. That is the whole
// job here: `episode_cluster_compacted` exists so an operator watching a pass
// can tell WHICH pattern was folded, beside the three counts on the same line,
// and a model that pasted a document into that field would bury every one of
// them.
//
// WHERE THE WHOLE SENTENCE IS, which is what makes a cut legitimate at all.
// The `episode_id` on the same log line says WHICH row is meant, and there
// are TWO copies of that row — named separately because they fail differently:
//
//   - The `common_task_pattern` column of the row this transaction just
//     committed, in this node's own store.
//   - The SEAT'S MEMORY CHANGELOG. `episodes` is one of the tables
//     internal/learning/memsync carries, and its registry names
//     `common_task_pattern` among the columns; a compacted row is an INSERT,
//     so the node's next publish cycle — or the flush a seat gets when it is
//     released — puts the whole row on the memory stream under its own
//     subject. That stream retains one message per subject and carries no age
//     bound, so what sits there is the row's current value for as long as the
//     stream lives, and a memory delete deliberately does not travel to
//     remove it. It is the better of the two routes back: it is readable from
//     a running fleet, while the column sits in a file the engine holds
//     exclusively while it runs. A node with no broker or no store publishes
//     nothing (memsync.New answers nil), and there the column is alone.
//
// NOTHING RENDERS THE COLUMN BACK TODAY, which is the honest other half and
// was checked rather than assumed. `query_episodes` prints an episode's
// `task_summary`, and a compacted row has none — [Lifecycle.buildCompacted]
// writes the pattern and leaves the turn-shaped prose columns empty, because
// this row is not a turn. Recall excludes compacted rows by default
// ([RecallQuery.Kinds]). The projection the dashboard's memory rows are built
// from leaves the column out too — it ships the turn-shaped fields and a
// `compacted` flag, so a folded row reads there as an empty summary with a
// count. So reading the sentence back means going to one of the two copies
// above rather than to a screen. That is a gap in the READ side rather than a
// reason to widen this budget — a log line sized to carry a value nobody can
// look up is a log line pretending to be a store.
//
// Contrast [modelAnswerDetail], which is larger for exactly the opposite
// reason — what that one quotes has no copy at all, which is why its own
// answer is a debug line carrying the value whole.
const patternLogDetail = 120

// patternForLog previews a compacted row's pattern for a log field.
//
// Through [textcut.Ellipsis]: bytes, cut on a rune boundary, and MARKED. The
// marker is the load-bearing part — an operator comparing this line against
// the row has to be able to tell a pattern the model wrote short from one this
// line shortened, and a silently clipped sentence reads as the whole claim.
//
// A function rather than the budget spelled at the call site, because the
// budget is a decision with a reason (see [patternLogDetail]) and a number
// typed into a log call is a decision nobody can find again.
func patternForLog(pattern string) string { return textcut.Ellipsis(pattern, patternLogDetail) }

// foldCluster summarises one cluster and replaces its members with the
// summary, keeping the exemplars.
//
// THREE OUTCOMES, THREE VALUES: the rows removed, whether the model answered,
// and a store error. A model that could not answer is not a failed pass — the
// cluster's rows are all still there and the next pass tries again — so it
// must not travel as an error the caller has to classify before deciding
// whether to abort.
//
// ONE TRANSACTION for the insert and the deletes. Two statements give two ways
// to disagree, and only one of them is survivable: deleting first and failing
// loses the turns outright, while writing first and failing leaves a summary
// counting rows that are still there — which every later pass then folds
// again — which is exactly what an insert followed by a separate delete
// does.
//
// THE SUMMARY'S WORK KEY NAMES THE FOLD, not a turn. It is derived from the
// member ids, so re-folding the same set collides with the unique index on
// (agent_handle, work_key) and does nothing instead of writing a second
// summary. That covers what the in-process single-flight cannot: a fold this
// node already made in an earlier pass, or one it makes concurrently in
// another goroutine. A peer folding the same seat is not the case — the
// episodes it folds are rows in ITS database, which this node never reads, so
// the two folds are two summaries of two separate copies of the memory.
// The "compact:" prefix keeps the two namespaces apart: a turn's key is 32 hex
// characters (internal/workkey), so no turn can ever mint this one.
//
// A COLLIDING INSERT STILL DELETES. That is not a fallthrough — it is the
// recovery: the summary exists, so its members are redundant, and leaving them
// would hand the next pass an orphaned cluster.
func (l *Lifecycle) foldCluster(ctx context.Context, handle string, cluster []Episode) (int64, bool, error) {
	exemplars, doomed := splitExemplars(cluster, l.opts.ExemplarCount)
	summary, err := l.sum.Summarize(ctx, Cluster{
		Handle: handle, Role: cluster[0].Role, Episodes: cluster,
	})
	if err != nil {
		log.WarnContext(ctx, "episode_cluster_not_summarised",
			"agent_handle", handle, "cluster_size", len(cluster), "error", err)
		// THE WHOLE ANSWER, ON ITS OWN LINE. The message above quotes
		// [modelAnswerDetail] bytes of it, and nothing else in this
		// system holds a model answer that failed to parse: the summary row
		// is never written, and no event carries a completion's content. So
		// without this line the quote in that message would be the value
		// being destroyed rather than shortened, which is the one thing a
		// cut here must not do.
		//
		// DEBUG, because it is prose in a log field: bounded only by the
		// call's own max_tokens (`compaction_budget_tokens`, 4000 by
		// default — on the order of 16 KB), which is far too much for the
		// line an operator has to SEE. The warning above is that line; this
		// is the one they turn on once they have seen it.
		var undecodable *UndecodableAnswerError
		if errors.As(err, &undecodable) {
			log.DebugContext(ctx, "episode_summary_undecodable",
				"agent_handle", handle, "cluster_size", len(cluster),
				"answer", undecodable.Answer)
		}
		return 0, false, nil
	}

	ids := make([]string, len(doomed))
	for i, ep := range doomed {
		ids[i] = ep.ID
	}
	row := l.buildCompacted(handle, cluster, exemplars, summary)
	deleted, err := l.tx(ctx, "fold episode cluster", func(tx *sql.Tx) (int64, error) {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := insertEpisodeTx(ctx, tx, l.db, row); err != nil {
			return 0, err
		}
		return deleteEpisodes(ctx, tx, handle, ids)
	})
	if err != nil {
		return 0, false, err
	}
	log.InfoContext(ctx, "episode_cluster_compacted",
		compactedLogFields(handle, row, len(cluster), len(exemplars), deleted)...)
	return deleted, true, nil
}

// compactedLogFields is the `episode_cluster_compacted` line, as values.
//
// A FUNCTION BECAUSE TWO OF THESE FIELDS ARE A PAIR. `pattern` is a preview
// (see [patternForLog]) and `episode_id` is the row it was previewed from, so
// a line carrying the first without the second is a shortened sentence with
// nowhere to read the rest — which is the one shape a cut must never take. An
// argument list assembled inside a log call is a pair no test can hold; this
// one is held by TestTheCompactedLogLineNamesTheRowItPreviews.
func compactedLogFields(handle string, row Episode, clusterSize, exemplarsKept int, rawDeleted int64) []any {
	return []any{
		"agent_handle", handle,
		"cluster_size", clusterSize,
		"exemplars_kept", exemplarsKept,
		"raw_deleted", rawDeleted,
		"episode_id", row.ID,
		"pattern", patternForLog(row.CommonTaskPattern),
	}
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
		ReviewOutcome: outcome,
		Duration:      total,
		// ONE WINDOW, and it is not a special case that got left behind:
		// this vector is over the compactor's own summary sentence, which
		// the prompt requires to be short, so there is nothing to window.
		// [Episode.Embeddings] holds a set because a TURN's summary is
		// unbounded; a summary of summaries is bounded by the model call
		// that produced it.
		Embeddings:        oneWindow(s.Embedding),
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
// The same algorithm the skill-drafting clustering uses, deliberately: a
// company where compaction and synthesis disagree about "the same work" folds
// away turns the other was still drafting from. The first member of a cluster
// is its representative and every later candidate is compared only against
// that one, so the result depends on the input order — which is why
// [Lifecycle.candidates] imposes a total one.
//
// A turn that called NO TOOLS is skipped entirely: Jaccard over an empty set
// is undefined, and there is no other similarity signal here. Such a turn is
// therefore never compacted, which for a chat-only seat means its raw rows
// only ever grow.
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
// ONE FUNCTION FOR ALL FIVE CALLERS — the two clusterers, the promoter, the
// near-duplicate check and the lifecycle fold. It was written twice with two
// names, and the copies could have disagreed on the threshold semantics
// silently: a seat's skills would then have been clustered by one rule and
// deduplicated by another, so a draft could be judged a duplicate of a skill
// it would never have been clustered with.
//
// Empty on either side scores 0 rather than 1. Two turns that called no tools
// have nothing in common that this function can see, and calling that a
// perfect match would pool every tool-free turn a seat ever ran into one
// cluster and summarise it as a pattern. That guard is also what makes the
// division below safe: with both sides non-empty the union is at least 1.
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

// insertEpisodeTx writes one episode inside a caller's transaction.
//
// It binds [episodeInsertSQL] through [episodeInsertArgs], the same statement
// and the same argument list Episodes.Append uses, so a summary row is written
// through exactly the columns every reader scans. That used to be a promise in
// this comment over a second hand-written list of twenty-five positional binds;
// it is one list now, which is what makes the promise checkable. The conflict
// clause on the statement is what makes a repeated fold a no-op.
func insertEpisodeTx(ctx context.Context, tx *sql.Tx, db *store.DB, ep Episode) error {
	blob, windows, err := (&Episodes{db: db}).encodeEmbedding(ep.Embeddings)
	if err != nil {
		// The summary is what the LLM call bought; its vector only
		// decides whether recall can reach the row. Refusing the row
		// here would spend the call again next pass and fail the same
		// way, so the row lands unembedded and the reason is logged.
		log.WarnContext(ctx, "compacted_episode_not_embedded",
			"episode", ep.ID, "error", err)
		blob, windows = nil, 0
	}
	_, err = tx.ExecContext(ctx, episodeInsertSQL, episodeInsertArgs(ep, blob, windows)...)
	return err
}

// oneWindow wraps a single vector as the one-window set [Episode.Embeddings]
// holds, and answers nil for an absent one.
//
// A named helper rather than a literal at each call site, because [][]float32
// has two different empty values — nil and a slice holding one empty vector —
// and only the first is what "there is no embedding" means to the writer and
// to recall.
func oneWindow(v []float32) [][]float32 {
	if len(v) == 0 {
		return nil
	}
	return [][]float32{v}
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

// CompleteFunc is one call to a model on one seat's behalf: the ROLE whose
// chain answers, a system prompt, a user prompt, and the text that came back.
//
// The narrowest thing that can stand for "an LLM" — no provider, no message
// types, no telemetry. Everything the engine wraps around a model call (the
// role's auxiliary provider chain, the token budget, the usage event) lives on
// the engine's side of this function, which is why this package still imports
// nothing but the store.
//
// The role is a NAME rather than a resolved provider for the same reason:
// resolving it is the engine's job, and a resolved chain here would put the
// provider types back in this package's imports.
type CompleteFunc func(ctx context.Context, role, system, user string) (string, error)

// NewSummarizer builds the model-backed [Summarizer]: it renders the cluster,
// makes one call, and parses what comes back.
//
// It attaches no embedding. The caller that has an embeddings provider can
// wrap this one and fill [Summary.Embedding]; a compacted row without it is
// read by every query except similarity recall.
func NewSummarizer(complete CompleteFunc) Summarizer { return modelSummarizer{complete: complete} }

type modelSummarizer struct{ complete CompleteFunc }

func (m modelSummarizer) Summarize(ctx context.Context, c Cluster) (Summary, error) {
	if m.complete == nil {
		return Summary{}, errors.New("learning: summarizer has no model")
	}
	if len(c.Episodes) == 0 {
		return Summary{}, errors.New("learning: summarizer got an empty cluster")
	}
	// THE ROLE IS PASSED THROUGH, which is what [Cluster.Role] is hoisted
	// out of the episodes for: it selects the seat's auxiliary provider
	// chain and attributes the token spend. Dropping it here would compact
	// every seat's memory on whichever single model the wiring happened to
	// close over, in a company whose whole point is that seats differ.
	raw, err := m.complete(ctx, c.Role, CompactorSystemPrompt, RenderCluster(c))
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
const CompactorSystemPrompt = `You are an agent-learning episode compactor.

You receive N similar agent turns (each a brief turn summary) and must
emit ONE JSON object summarising the recurring pattern.  The output
becomes a single compacted episode row that replaces the N originals
in the agent's episode store.

Output format (strict, JSON only -- no prose before or after):

{
  "common_task_pattern": "<one sentence describing the recurring task>",
  "common_outcome": "done" | "failed",
  "subjects_involved": ["<distinct counterparties / subjects>"],
  "notable_patterns": "<one to three sentences naming variations / edge cases>"
}

Rules:
- ` + "``common_task_pattern``" + ` is declarative and short; the agent reads
  it as "you've done this kind of work N times".
- ` + "``subjects_involved``" + ` lists distinct named counterparties (chat
  user labels, ticket reporter handles).  Empty if none consistent.
- ` + "``notable_patterns``" + ` captures variation across the cluster
  (e.g. "most replied within the thread; 2 handed off to manager
  when message contained 'urgent'").  Keep it useful, not exhaustive.
- Never invent facts not present in the turns.
- If the turns don't actually share a coherent pattern, emit
  ` + "``{\"common_task_pattern\": \"(heterogeneous)\"}``" + `; the rest of the
  fields can be empty -- the compactor will still write the row but
  the agent will see it as low-signal.
`

// toolsPerTurnShown clamps how much of one turn's tool sequence reaches the
// prompt.
//
// THE OPENING OF A PROCEDURE IS WHAT DISTINGUISHES IT. Clustering already
// pooled these turns on the Jaccard overlap of their whole sequences, so the
// model is not being asked which turns belong together — it is being asked
// what they have in common, and the first eight calls carry the shape of the
// work (search, read, comment, hand off) where the tail is usually the same
// tool repeated. A sequence is bounded only by the turn's round cap
// (`max_tool_rounds`), so a handful of long turns rendered whole would crowd
// out the prose in [perTurnDetail] on every other member of the cluster.
//
// THE COUNT OF THE REST IS PRINTED beside it — "(+N more)" — because the one
// thing the model must not read is a clipped procedure as a short one. And
// nothing is lost at the moment this renders: each member's whole sequence is
// its own `tool_sequence` column, which is what the fold about to happen is
// summarising away by design.
//
// Eight rather than a number typed into the loop: it was spelled twice, once
// as the slice bound and once as the subtrahend under it, which is a pair that
// can drift into a "(+N more)" naming the wrong N.
const toolsPerTurnShown = 8

// perTurnDetail clamps how much of one turn's prose reaches the prompt.
//
// A cluster can be as large as the batch (200 turns), and every turn renders
// four lines. At 280 BYTES each for the task and its summary that is roughly
// 120 KB, about 30k tokens, in a single call — affordable once a month per
// seat. Without the clamp one turn carrying a pasted stack trace sets the size
// of the call.
//
// BYTES AND NOT CHARACTERS, which is what this said until the unit was checked
// against the function it is handed to: [oneLine] spends it through
// [textcut.Ellipsis], whose budget is bytes (its package doc contrasts itself
// with ledger.Elide, which counts runes, for exactly this confusion). The
// arithmetic above was always a byte budget — 120 KB is what bounds the call —
// so the number is right and only the word was wrong. On prose that is not
// ASCII the clamp therefore falls sooner than a reader counting characters
// expects, and a summary in CJK reaches the compactor at about a third of the
// words an English one does. That is the honest trade for a bound this layer
// can actually count: a token budget needs a tokenizer per provider and a rune
// budget bounds nothing about the size of the call.
//
// WHERE THE WHOLE PROSE IS: in the `task_summary` and `plan_summary` columns
// of the very rows this pass is reading, for as long as those rows exist. What
// ends that is the fold itself, which deletes every member but the exemplars —
// deliberately, because deleting them IS compaction, and this render is the
// prompt asking for the summary that replaces them. So the clamp costs
// nothing: at the moment it happens the whole text is one column away, and
// what removes it afterwards is the operation, not the clamp. The cut is
// MARKED by Ellipsis, which is what stops the model reading a clipped stack
// trace as a short one and writing that down as the pattern.
const perTurnDetail = 280

// RenderCluster is the user half of the compaction prompt: the cluster as a
// numbered list of brief turn summaries.
func RenderCluster(c Cluster) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Cluster of %d similar agent turns from one agent.\n\n", len(c.Episodes))
	b.WriteString("Turns (numbered):\n")
	for i, ep := range c.Episodes {
		tools := "(none)"
		if len(ep.ToolSequence) > 0 {
			// SAYS WHEN IT CUTS. The compactor is inferring "how this
			// agent does this kind of work", and a silently shortened
			// tool sequence reads as a shorter procedure rather than a
			// clipped one — which is the pattern it then writes down.
			shown := ep.ToolSequence
			tools = strings.Join(shown[:min(toolsPerTurnShown, len(shown))], ", ")
			if dropped := len(shown) - toolsPerTurnShown; dropped > 0 {
				tools += fmt.Sprintf(" (+%d more)", dropped)
			}
		}
		fmt.Fprintf(&b, "%d. [%s] task: %s\n", i, ep.ReviewOutcome, oneLine(ep.TaskSummary))
		fmt.Fprintf(&b, "   plan: %s\n", oneLine(ep.PlanSummary))
		fmt.Fprintf(&b, "   tools: %s\n", tools)
		fmt.Fprintf(&b, "   when: %s\n", ep.EndedAt.UTC().Format(time.DateOnly))
	}
	b.WriteString("\nEmit one JSON object summarising the recurring pattern across " +
		"these turns.  Strict JSON, no prose.\n")
	return b.String()
}

// oneLine flattens and clamps one field of a turn.
//
// Newlines become spaces because the rendering is a numbered list and a turn
// summary containing its own newlines would look like more turns than there
// are — a model that miscounts the cluster writes the pattern of a cluster
// that does not exist.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return textcut.Ellipsis(s, perTurnDetail)
}

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
//
// A failure is an [UndecodableAnswerError] CARRYING THE ANSWER WHOLE; only its
// message is bounded. Nothing else keeps an answer that did not parse, so this
// return value is the only copy of it anywhere.
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
	return Summary{}, &UndecodableAnswerError{Answer: raw}
}

// UndecodableAnswerError reports an answer from the compactor that held no
// JSON object, and CARRIES THAT ANSWER WHOLE.
//
// The shortening is in the MESSAGE and nowhere else. Nothing in this system
// stores a model answer that failed to parse — the summary row is never
// written, and no event carries a completion's content — so this value is the
// only copy in existence, and an error that quoted 400 bytes and dropped the
// rest would be deleting the one thing a person debugging it needs. A caller
// reaches the rest with errors.As; [Lifecycle.foldCluster] is the caller that
// does.
type UndecodableAnswerError struct {
	// Answer is the model's reply exactly as it arrived, whitespace and all.
	Answer string
}

// Error quotes at most [modelAnswerDetail] bytes of the answer, marked where
// it was cut — the package's one budget for what a model's answer may spend
// in something a person reads, and its doc says why 400.
//
// It only has to CLASSIFY the failure — prose instead of JSON, an apology
// around a fence, an empty answer, an object that breaks halfway — because the
// answer itself is not lost to it: the error carries it whole, and
// [Lifecycle.foldCluster] logs that at debug.
//
// Trimmed for the quote only, never on [UndecodableAnswerError.Answer]: a
// model that answered with three hundred bytes of newlines would otherwise
// spend the whole budget proving it, while the field keeps what arrived.
func (e *UndecodableAnswerError) Error() string {
	return fmt.Sprintf("learning: no JSON object in the compactor's answer (%s)",
		textcut.Ellipsis(strings.TrimSpace(e.Answer), modelAnswerDetail))
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
