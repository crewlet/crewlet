package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/workkey"
)

// The episode compaction lifecycle: the pass that drains a seat's episode
// table.
//
// A seat that keeps working accumulates one raw row per turn forever, and
// nothing else in the system removes one. Recall is what pays for that, and it
// pays LINEARLY: there is still no ANN index reachable from the Go driver,
// re-measured at the pin, so recall visits every embedded row
// the seat owns, in the Plan phase of every turn.
//
// The constant shrank when the distance arithmetic moved into the database
// — the rows no longer cross the driver boundary to be decoded
// in Go, which at 5 000 rows of 1 536 dimensions was 144 ms and 35.8 MB
// against 34 ms and 35.8 KB for the same ranking. The SHAPE did not: it is
// still linear in the seat's row count, still with no ceiling, and this pass
// is still the only thing that puts one there. The original measurement,
// before the move — 6 ms at 100 rows, 32 ms at 500, 63 ms at 1000, 122 ms at
// 2000, ~62 µs per row — is what the pass was sized against.
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
	// CommonTaskPattern is one declarative sentence. The planner reads it
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
	// scanned and cosined on the Plan phase of every turn, for ever.
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
	// scans and cosines every embedded row a seat owns, once per turn, at
	// ~62 µs per row (see the package block above). 500 rows is ~32 ms on
	// the Plan phase of every turn — a rounding error next to one LLM round.
	// 2000 would be ~122 ms and still climbing linearly, because nothing
	// else in the system bounds this number.
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
	// it is a row recall scans on every Plan phase at ~62 µs, permanently.
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
		// rows per fold, which at the measured ~62 µs of recall scan per
		// row is 124 µs a turn.
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

// terminalOutcomes are the Review decisions that END a turn, as a SQL tuple.
//
// ONE definition used twice, in complementary directions: a compaction
// candidate must be in it, and the mid-state sweep drops everything that is
// not. Two hand-written lists is how a row ends up being neither: a sweep
// naming 'self_iterate' explicitly while the candidate query takes only
// ('done','failed') leaves any other value undroppable AND uncompactable, and
// it stays in the seat's memory forever.
//
// Spelled inline rather than bound: these are compile-time constants of this
// package and never caller input, exactly like the state names in
// ListOptions.filter.
const terminalOutcomes = `('done', 'failed')`

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
	// deployment and every Plan phase pays for it.
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
func (l *Lifecycle) compact(ctx context.Context, handle string, now time.Time, res *PassResult) error {
	candidates, err := l.candidates(ctx, handle, now.Add(-l.opts.MinAge))
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
	return nil
}

// candidates returns the raw rows eligible for folding, oldest first.
//
// Eligible is the exact complement of what the earlier sweeps drop: terminal
// (see [terminalOutcomes]), unstamped, and old enough that no other reader
// still wants the detail.
//
// ORDERED BY (ended_at, id), and the id is load bearing rather than tidy.
// Clustering is greedy over this order, so two passes that disagree about the
// order of two rows stamped in the same microsecond build different clusters,
// derive different fold keys, and each write a summary claiming turns the
// other also claimed. SQL leaves the order of equal sort keys unspecified.
func (l *Lifecycle) candidates(ctx context.Context, handle string, cutoff time.Time) ([]Episode, error) {
	rows, err := l.db.SQL().QueryContext(ctx,
		`SELECT `+episodeColumns+` FROM episodes
		 WHERE agent_handle = ? AND kind = 'raw'
		   AND consolidated_into_skill_id IS NULL
		   AND review_outcome IN `+terminalOutcomes+`
		   AND ended_at < ?
		 ORDER BY ended_at ASC, id ASC LIMIT ?`,
		handle, store.EncodeTime(cutoff), l.opts.BatchSize)
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
// Two rules, and they are the same rule seen from both ends:
//
// A RETIRED EXEMPLAR is a member a fold deliberately kept. It is dropped from
// the candidate list entirely — not folded, not counted, not deleted. Letting
// it back in is what makes drill-down rot: it is old, it matches its own
// cluster's tool shape by construction, so it joins the next cluster over the
// same work, gets counted a second time, and is deleted — leaving the earlier
// summary pointing at rows that no longer exist. Every pass after that costs
// the same two rows again.
//
// An ORPHAN is a member the fold meant to delete and did not. Its summary
// already counts it, so folding it again writes a second summary over turns
// the first one claims — the double count this whole mechanism exists to
// prevent. See [Lifecycle.foldCluster] for why that state should be
// unreachable, and [Lifecycle.sweepOrphans] for why it is still handled.
// RETIREMENT IS CHECKED ACROSS EVERY ANCHOR BEFORE COVERAGE IS CHECKED AT ALL,
// and that order is the whole reason these are two loops rather than one. Two
// summaries can claim overlapping spans with the same tool shape, so one
// summary's exemplar can sit inside another's window — and deciding per anchor
// in one pass would make the outcome depend on which row the database happened
// to return first, with a live drill-down anchor deleted on half the orderings.
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
		"agent_handle", handle, "cluster_size", len(cluster),
		"exemplars_kept", len(exemplars), "raw_deleted", deleted,
		"pattern", truncate(row.CommonTaskPattern, 120))
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
	slices.SortFunc(ordered, func(a, b Episode) int {
		switch {
		case a.EndedAt.After(b.EndedAt):
			return -1
		case a.EndedAt.Before(b.EndedAt):
			return 1
		default:
			return -1 * compareStrings(a.ID, b.ID)
		}
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
		// The planner reads this line as the whole reason the row exists.
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
		Embedding:         s.Embedding,
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
// It binds [episodeInsertSQL], the same statement Episodes.Append uses, so a
// summary row is written through exactly the columns every reader scans. The
// conflict clause on that statement is what makes a repeated fold a no-op.
func insertEpisodeTx(ctx context.Context, tx *sql.Tx, db *store.DB, ep Episode) error {
	var blob any
	if len(ep.Embedding) > 0 {
		packed, err := db.EncodeVector(ep.Embedding)
		if err != nil {
			// The summary is what the LLM call bought; its vector only
			// decides whether recall can reach the row. Refusing the row
			// here would spend the call again next pass and fail the same
			// way, so the row lands unembedded and the reason is logged.
			log.WarnContext(ctx, "compacted_episode_not_embedded",
				"episode", ep.ID, "error", err)
		} else {
			blob = packed
		}
	}
	_, err := tx.ExecContext(ctx, episodeInsertSQL,
		ep.ID, ep.Handle, ep.Role, ep.TaskID, ep.TurnID,
		store.EncodeTime(ep.StartedAt), store.EncodeTime(ep.EndedAt),
		ep.PlanSummary, ep.TaskSummary, jsonList(ep.ToolSequence), jsonList(ep.SkillsUsed),
		ep.ReviewOutcome, ep.Duration.Milliseconds(), blob,
		string(ep.Kind), ep.Count, jsonList(ep.ExemplarTurnIDs),
		store.NullText(ep.ConsolidatedInto), ep.CommonTaskPattern, ep.CommonOutcome,
		ep.SuccessRate, jsonList(ep.SubjectsInvolved), ep.NotablePatterns,
		store.NullText(ep.WorkKey), store.NullText(ep.ConversationKey),
	)
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return clip(s, n) + "..."
}

// clip cuts s to at most n bytes, never through a rune.
//
// A plain s[:n] splits whatever multi-byte character straddles the cut and
// yields invalid UTF-8 — which reaches a model as a replacement character,
// an embedding API as a rejected body, and a JSON encoder as an escaped
// substitution. Every one of those is a bug that only appears once the text
// stops being ASCII, which in a company's traffic is a matter of when.
func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
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
- ` + "``common_task_pattern``" + ` is declarative and short; the planner reads
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
  the planner will see it as low-signal.
`

// perTurnDetail clamps how much of one turn's prose reaches the prompt.
//
// A cluster can be as large as the batch (200 turns), and every turn renders
// four lines. At 280 characters each for the task and the plan that is roughly
// 120 KB, about 30k tokens, in a single call — affordable once a month per
// seat. Without the clamp one turn carrying a pasted stack trace sets the size
// of the call.
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
			tools = strings.Join(shown[:min(8, len(shown))], ", ")
			if dropped := len(shown) - 8; dropped > 0 {
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
	return truncate(s, perTurnDetail)
}

// ParseSummary reads a model's answer, tolerating prose around the JSON.
//
// Down [modelJSONCandidates], so a fenced answer and one wrapped in an apology
// both recover, and a clean one parses exactly as sent.
//
// FIELD BY FIELD once an object is found, never all-or-nothing. The pattern
// sentence is the only part the planner actually reads, and decoding the whole
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
	return Summary{}, fmt.Errorf("learning: no JSON object in the compactor's answer (%s)",
		truncate(text, 200))
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
