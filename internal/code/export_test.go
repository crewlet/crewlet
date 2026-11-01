package code

import (
	"context"
	"encoding/json"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Quiet is quiet, for this package's external benchmarks.
var Quiet = quiet

// RoutingNoticeRows is the notice rows one routing of the gate's records is
// charged — the bound a check ending's routing is held to beside the rows its
// apply writes.
const RoutingNoticeRows = routingNoticeRows

// ChangeCeilingRefusal is why a seat's write of a change may not be made for
// whoever c is, for the case that judges the requester ceiling of a change's
// writes apart from the decides that reach it.
func ChangeCeilingRefusal(c *WriteCeiling, asker Principal, repo RepoRef, own bool, need Access,
	roster Roster) string {
	return c.changeRefusal(asker, repo, own, need, "write it", roster)
}

// SetShareWait scales how long s's takers wait for a unit — production's
// GitShareWait — so a case waits it out in milliseconds.
func SetShareWait(s *GitShare, d time.Duration) { s.wait = d }

// SetPageLimits has r end every page at rows rows or bytes of encoded rows —
// the cases that read a snapshot across several pages without seeding tens of
// thousands of rows. The shipped limits are MaxSnapshotItems and MaxPageBytes.
func SetPageLimits(r *Reader, rows, bytes int) { r.pageRows, r.pageBytes = rows, bytes }

// SetTipWindow has r read at most landings landings above a target's green
// for its tip work — the cases that reach a window's bound without landing
// MaxTipLandings pushes.
func SetTipWindow(r *Reader, landings int) { r.tipWindow = landings }

// SetRetentionPace has r measure and pace its steps by clock and sleep, take
// steps for budget, and read the listing page repositories at a time, rather
// than the monotonic clock, a timer, RetentionPassBudget and retentionPage —
// the cases that stage a pass's budget, its pacing and a listing across pages
// without waiting out a minute. A zero leaves that one as shipped.
func SetRetentionPace(r *Retention, clock func() time.Time, sleep func(context.Context, time.Duration) error,
	budget time.Duration, page int) {
	r.clock, r.sleep, r.budget, r.page = clock, sleep, budget, page
}

// RetentionIdlePerStep is how many times a step's duration a pass waits after
// it.
const RetentionIdlePerStep = retentionIdlePerStep

// RetiredDuePages is every page of the retention pass's retired listing over
// r, read limit entries a page as the pass reads it past cutoff: the
// repositories each page names, in order.
func RetiredDuePages(ctx context.Context, r *Reader, cutoff time.Time, limit int) ([][]string, error) {
	q := retiredQuery{cutoff: cutoff, limit: limit}
	var pages [][]string
	for {
		page, err := r.retiredDue(ctx, q)
		if err != nil {
			return pages, err
		}
		pages = append(pages, page.repos)
		if page.next == nil {
			return pages, nil
		}
		q.after = page.next
	}
}

// IdleScratchView is the first page of the scratch expiry's listing over r at
// cutoff, as a pass reads it: the ids it lists, and the view — the cutoff, the
// position the page was read at and its stream's identity — the pass decides
// each of them against ([Store.ExpireScratch]).
func IdleScratchView(ctx context.Context, r *Reader, cutoff time.Time, limit int) ([]string, ScratchExpiry,
	error) {

	page, err := r.idleScratch(ctx, idleQuery{cutoff: cutoff, limit: limit})
	if err != nil {
		return nil, ScratchExpiry{}, err
	}
	ids := make([]string, 0, len(page.repos))
	for _, repo := range page.repos {
		ids = append(ids, repo.id)
	}
	return ids, ScratchExpiry{Cutoff: cutoff, Through: page.through, KeyedTo: page.keyedTo}, nil
}

// ExpiryPage is the first page of the ref expiry's listing over r at cutoff,
// as a pass reads it: each ref it lists, as "<repository id> <ref>", oldest
// first, and the position it was read at — the Seen every proposal formed from
// it carries.
func ExpiryPage(ctx context.Context, r *Reader, cutoff time.Time) ([]string, statelog.Position, error) {
	page, err := r.idleSeatRefs(ctx, idleRefsQuery{cutoff: cutoff, limit: MaxCommandsPerPush})
	if err != nil {
		return nil, statelog.Position{}, err
	}
	refs := make([]string, 0, len(page.refs))
	for _, ref := range page.refs {
		refs = append(refs, ref.repoID+" "+ref.ref)
	}
	return refs, page.through, nil
}

// ExpiryListing reads every page of the ref expiry's listing over r at
// cutoff, as a pass reads them, and answers how many pages and refs it read.
func ExpiryListing(ctx context.Context, r *Reader, cutoff time.Time) (pages, refs int, err error) {
	q := idleRefsQuery{cutoff: cutoff, limit: MaxCommandsPerPush}
	for {
		page, err := r.idleSeatRefs(ctx, q)
		if err != nil {
			return pages, refs, err
		}
		pages, refs = pages+1, refs+len(page.refs)
		if len(page.refs) < q.limit {
			return pages, refs, nil
		}
		q.after = &page.refs[len(page.refs)-1]
	}
}

// SetCommitterIdle has c's repository goroutines exit after d with nothing
// queued, rather than CommitterIdle — set before c's first proposal.
func SetCommitterIdle(c *Committer, d time.Duration) { c.idle = d }

// SetCommitterClock has c measure how long a push has waited in its queue by
// clock rather than the wall clock — set before c's first proposal.
func SetCommitterClock(c *Committer, clock func() time.Time) { c.clock = clock }

// SetCommitterNodeBytes has c hold at most n bytes of proposals across its
// repositories, rather than CommitterNodeBytes — the case that fills a node
// across two repositories without four repositories' queues of maximal
// pushes. Set before c's first proposal.
func SetCommitterNodeBytes(c *Committer, n int) { c.nodeBytes = n }

// SetQueueEntries holds c's merge queues to n waiting entries a target, in
// place of MaxQueueEntries.
func SetQueueEntries(c *Committer, n int) { c.queueEntries = n }

// CommitterHeld is the bytes of proposals c holds across its repositories.
func CommitterHeld(c *Committer) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held
}

// ObserveDecideRounds has c tell observe how many protection verdicts each
// decide round formed inside the publisher's snapshot — set before c's first
// proposal.
func ObserveDecideRounds(c *Committer, observe func(formed int)) { c.formedInRound = observe }

// CommitterRepos is how many repositories c holds a goroutine for.
func CommitterRepos(c *Committer) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.repos)
}

// CommitterStopping reports a committer Stop has been called on.
func CommitterStopping(c *Committer) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

// AwaitAnsweredAndGone is a queued push's wait on c when its answer, err, was
// given and its caller's context ended before the wait looked — both ready at
// once, as a stopping committer's refusals and the serving withdrawal's
// cancellation of every answer in flight are.
func AwaitAnsweredAndGone(c *Committer, err error) (ProposeResult, error) {
	q := &queued{arrived: c.clock(), answer: make(chan answered, 1)}
	q.answer <- answered{err: err}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := c.await(ctx, q)
	return a.res, a.err
}

// CommitterQueued is how many proposals wait in repoID's queue on c.
func CommitterQueued(c *Committer, repoID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rc := c.repos[repoID]; rc != nil {
		return len(rc.inbox)
	}
	return 0
}

// ChangeWriterHolders is how many writes of changeID hold or wait for its turn
// on w.
func ChangeWriterHolders(w *ChangeWriter, changeID string) int { return w.flights.holders(changeID) }

// ChecksWriterHolders is how many writes of repoID's checks hold or wait for
// its turn on w.
func ChecksWriterHolders(w *ChecksWriter, repoID string) int { return w.flights.holders(repoID) }

// SetChangeWriterBounds has w admit at most key writes of one change, node of
// every change and bytes of their weight, rather than its constants — the
// cases that fill a node's share without a thousand writes or sixty-four
// maximal reviews. Writes already held stay held.
func SetChangeWriterBounds(w *ChangeWriter, key, node, bytes int) {
	w.flights.setBounds(key, node, bytes)
}

// SetChecksWriterBounds is [SetChangeWriterBounds] for a checks writer, a
// repository's check writes in place of a change's conversation.
func SetChecksWriterBounds(w *ChecksWriter, key, node, bytes int) {
	w.flights.setBounds(key, node, bytes)
}

// setBounds is a test's bounds in place of f's constants, under its lock.
func (f *flights) setBounds(key, node, bytes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bounds.key, f.bounds.node, f.bounds.bytes = key, node, bytes
}

// SetCommitterNodeProposals has c hold at most n proposals across its
// repositories, rather than CommitterNodeProposals — the case that fills a
// node's count across two repositories without a thousand pushes. Set before
// c's first proposal.
func SetCommitterNodeProposals(c *Committer, n int) { c.nodeProposals = n }

// ConversationWeight is what cw weighs in its writer's node bytes: the bytes
// its record carries as the writer stamps it.
func ConversationWeight(cw ConversationWrite) (int, error) { return cw.measure() }

// ChecksWeight is what r weighs in its writer's node bytes.
func ChecksWeight(r ChecksRequest) (int, error) { return r.weight() }

// StandingStates is the list of states a raiser's standing findings are read
// by, as the decide's statement is driven from it.
func StandingStates() []string {
	var states []string
	if err := json.Unmarshal([]byte(standingStates), &states); err != nil {
		panic("code: standingStates is no JSON list of strings: " + err.Error())
	}
	return states
}

// The committer's batching bounds, for the limits test.
const (
	RecordOverheadBytes = recordOverheadBytes
	CommandResultBound  = commandResultBound
)

// EntryBound is the most bytes p's entry takes in a record.
func EntryBound(p Proposal) (int, error) { return entryBound(p) }

// RowBound is the most rows p's entry makes its record write.
func RowBound(p Proposal) int { return rowBound(p) }

// RecordRows is the row bound of a record whose entries bound rows rows.
func RecordRows(rows int) int { return recordRows(rows) }

// LandRecordRows is the most rows a land record of n entries makes its apply
// write but the changes its pushes move.
func LandRecordRows(n int) int { return landRecordRows(n) }

// QueueScope is the scope p's record names.
func QueueScope(p QueueProposal) []string { return p.scope() }

// MergeRecordRows is the most rows one merge record makes its apply write
// before its horizons.
const MergeRecordRows = mergeRecordRows

// EntryOf is the record entry p becomes with its commands as decided — a
// seat's counted against day, as its committer stamps it.
func EntryOf(p Proposal, day string) PushEntry { return p.entryWith(p.Commands, day) }

// LongestDay is a company day's label at its longest, which a seat's entry is
// measured carrying.
const LongestDay = longestDay

// CostliestRules is n distinct rules of gitref's costliest shape — the worst
// asking a rule about a ref can be (decide_bench_test.go), for the cases that
// pin what that worst allocates, as a push is judged by them.
func CostliestRules(n int) []PushRule { return pushRules(costliestRules(n)) }

// WidestRef is the 255-byte ref BenchmarkDecideABatch's worst arms name for a
// push's command, the one that costs each of CostliestRules most.
func WidestRef(push, command int) string { return widestRef(push, command) }

// WidestComponent is one component of WidestRef.
const WidestComponent = widestComponent

// ReconcileInFlight is how many repositories a reconcile decides at once.
const ReconcileInFlight = reconcileInFlight

// DecideEach is the reconcile's closed loop of decisions.
var DecideEach = decideEach

// MaxDetailBytes is the longest Detail a receiver may fill in on a command it
// refused — the bound a grant's refusal sentence is held under, since the
// receiver carries it into a proposal.
const MaxDetailBytes = maxDetailBytes

// LayerCut is where part i of a layer whose three files are sections bytes
// lies — the one cut StoreLayer reads a part by and FetchLayerPart lays it
// back by: its first byte in the layer, its bytes, and each run as its file
// (0 the multi-pack-index, 1 its bitmap, 2 the commit-graph layer), its
// offset in that file and its bytes.
func LayerCut(sections [3]int64, i int) (start, size int64, runs [][3]int64) {
	cut := layerPart(sections, i)
	for _, s := range cut.spans {
		runs = append(runs, [3]int64{int64(s.section), s.offset, s.bytes})
	}
	return cut.start, cut.size, runs
}
