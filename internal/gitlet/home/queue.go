package home

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/textcut"
)

// WorkerQueue is the merge queue's worker's name, as crewlet.code.home.passes
// counts its turns.
const WorkerQueue = "queue"

// Chains builds a chain of merges, the queue's candidates, in one scratch on
// this node. internal/gitlet/host's *Server satisfies it; this package
// imports no git host.
type Chains interface {
	BuildChain(ctx context.Context, b gitlet.ChainBuild) (gitlet.ChainResult, error)
}

// Tasks closes a landed change's work item AS THE ENTRY'S ENQUEUER, step 2 of
// a queued merge that asked it (M5-D27). The engine wires the company's
// tracker, which decides the close as that principal and, where the enqueue
// was made for somebody, for them and within what they may use: the
// enqueuing turn's own write made late (ADR-0109). The queue never closes a
// work item on its own account.
//
// CloseWorkItem answers nil for an item closed or already finished; a
// [*NotClosed] for a close that will never be made, which the home settles
// not_closed and names on the change; any other error for a close whose end
// is unknown (the tracker unreachable), which stays OWED on the entry's row
// (code_queue.close_op) for a later turn on whichever home holds the repository.
type Tasks interface {
	CloseWorkItem(ctx context.Context, c TaskClose) error
}

// NotClosed is a close [Tasks] will never make: the item is a vendor's
// (closed where it lives); the enqueuer holds no seat, left the chart or may
// no longer update it; or the tracker refuses for good (the item gone, or
// refusing done). Why is the sentence the change's authors and enqueuer read
// beside `merged` (gitlet.SettledClose.Detail); Err is the cause, for errors.Is.
type NotClosed struct {
	Why string
	Err error
}

func (n *NotClosed) Error() string { return n.Why }

func (n *NotClosed) Unwrap() error { return n.Err }

// TaskClose is one landed change's work item to close, by As. OpID derives
// from the landing's, so a close asked again is the same write, which the
// tracker's ledger collapses.
type TaskClose struct {
	OpID     string
	RepoID   string
	Change   gitlet.ChainChange
	WorkItem types.WorkItem
	As       gitlet.Principal
	// WorkingFor is whom As's enqueue was made for, as the entry's row keeps
	// it (gitlet.QueueWorkEntry.WorkingFor), nil for nobody: the close is
	// written for them and decided within what they may use, whether the
	// landing's own turn or a later turn's owed close makes it.
	WorkingFor *types.WorkingFor
}

// queue is the home's MERGE QUEUE (M5-D12, M5-D13; package doc, "What a turn
// of the merge queue does"): for each target of a repository with live
// entries, from ONE snapshot of its queue work (gitlet.QueueDue), the next
// step of its train: built ([queue.build]), tested and its longest green
// prefix landed ([queue.test]), a red first candidate bisected
// ([queue.bisect]), or a pull mirror's entries shed ([queue.shed]); beside
// them, a given-up train's attempts cancelled ([queue.cancelAbandoned]) and
// the closes landings owe made ([queue.closeOwed]). Every write is a record
// decided where it lands (a candidate, eject or land by the committer's
// code.queue, a check by its writer), refusing what its snapshot does not
// prove, so a stale step is refused and read again next turn; every record
// nudges the repository, which is how a train moves from step to step.
type queue struct {
	estate Estate
	chains Chains
	tasks  Tasks
	// checks is the check work's own writer: a candidate's checks are
	// queued, and the checks file read, as a head's and a tip's are.
	checks *checks
	// rate is each target's merge rate, measured as its queue work is read
	// against the rounds this node timed (mergerate.go), and read by
	// [queue.AlarmReading] for code_merge_rate_over_capacity (M5-D28).
	rate mergeRate
	// metrics is where its rounds, landings and ejections are counted
	// (crewlet.code.queue.*); nil records nothing.
	metrics *metrics.Recorder
	node    string
	log     *slog.Logger
	now     func() time.Time
}

// AlarmReading fills the reading's merge-rate half from the queues this node
// is home to ([mergeRate]): what raises code_merge_rate_over_capacity.
func (w *queue) AlarmReading(out *statelog.Reading) {
	w.rate.reading(w.now(), out)
}

// Name is [WorkerQueue].
func (w *queue) Name() string { return WorkerQueue }

// Due is a page of the repositories owing queue work (a live entry, a
// given-up train's attempt or a landed change's close): the due-queue
// listing, a gitlet.QueueDueQuery naming no repository.
func (w *queue) Due(ctx context.Context, floor statelog.Position, cursor string) ([]string, string, error) {
	a, err := w.estate.QueueDue(ctx, gitlet.QueueDueQuery{Cursor: cursor, Fresh: freshAt(floor)})
	if err != nil {
		return nil, "", err
	}
	return a.DueRepos, a.Next, nil
}

// Work is one repository's queue work, a target a page: each target's next
// step, one target's failure left for the next pass while the others are
// worked; a turn that read every page drops the merge rate of each target it
// did not meet, whose queue has drained ([mergeRate]). The closes the first
// page answers OWED are made last ([queue.closeOwed]).
//
// Its given-up attempts are cancelled once every target has stepped, and NOT
// in a turn that recorded a new train: a chain rebuilt from the same rows is the same commits
// (gitlet/host's BuildChain), so the new train may test a commit such an
// attempt measures, and the next turn, which its candidate record nudges,
// reads it as the live train's. Cancelled a turn early, the run would be cut
// and queued again (gitlet.CheckReason.GivenUp), costing its container the run.
func (w *queue) Work(ctx context.Context, repoID string, floor statelog.Position) error {
	q := gitlet.QueueDueQuery{RepoID: repoID, EnqueuedSince: w.now().Add(-gitlet.MergeRateWindow),
		Fresh: freshAt(floor)}
	memo := newDefsMemo()
	var (
		failed    []error
		met       []string
		abandoned []gitlet.AbandonedCheck
		owed      []gitlet.QueueWorkEntry
		readAt    statelog.Position
		built     bool
	)
	for {
		// EVERY PAGE IS READ WHOLE OR REFUSED (gitlet.QueueDueAnswer): each
		// entry is acted on as read, so a page naming a member this build does
		// not declare fails the turn, stepping on none of it, and a data node
		// too old to serve the read answers unserved; either way the queue
		// waits for a build that reads it whole.
		a, err := w.estate.QueueDue(ctx, q)
		if err != nil {
			return errors.Join(append(failed, fmt.Errorf("home: read repository %s's queue work: %w", repoID,
				err))...)
		}
		if q.Cursor == "" {
			abandoned, owed, readAt = a.QueueAbandoned, a.QueueOwedCloses, a.Position
		}
		if a.DueQueue != nil {
			met = append(met, a.DueQueue.TargetRef)
			recorded, err := w.target(ctx, repoID, *a.DueQueue, memo, a.Position)
			if err != nil {
				failed = append(failed, err)
			}
			built = built || recorded
		}
		if a.Next == "" {
			w.rate.seen(repoID, met)
		}
		if a.Next == "" || ctx.Err() != nil {
			break
		}
		q.Cursor = a.Next
	}
	if !built && ctx.Err() == nil {
		if err := w.cancelAbandoned(ctx, repoID, abandoned, readAt); err != nil {
			failed = append(failed, err)
		}
	}
	if err := w.closeOwed(ctx, repoID, owed, readAt); err != nil {
		failed = append(failed, err)
	}
	return errors.Join(append(failed, ctx.Err())...)
}

// target is one target's next step, from the snapshot d was read in at, and
// whether it recorded a new train.
func (w *queue) target(ctx context.Context, repoID string, d gitlet.QueueDue, memo *defsMemo,
	at statelog.Position) (bool, error) {

	if w.rate.measure(repoID, d, w.now()) {
		w.round(roundAbandoned)
	}
	switch {
	case len(d.Entries) == 0:
		return false, nil
	case d.Mirrored:
		return false, w.shed(ctx, repoID, d, at)
	case d.Builds():
		// THE ONE PREDICATE the reader answered the entries' descriptions
		// by: a build writes their commit messages from them.
		return w.build(ctx, repoID, d, at)
	case d.TrainStands():
		return false, w.test(ctx, repoID, d, memo, at)
	case len(d.Required) > 0 && d.Tip == gitlet.ZeroOID:
		// NO TIP TO BUILD ONTO: the branch was deleted under its queue; a
		// person's push creating it again gives its train a base.
		w.log.Debug("code_queue_no_target", "repo", repoID, "about", fmt.Sprintf("target %s at %s",
			d.TargetRef, short(d.Tip)))
	}
	return false, nil
}

// shed ejects every live entry of d's target repo_mirrored: a PULL MIRROR
// (gitlet.QueueDue.Mirrored) refuses candidates and landings, so its train
// could never land, and a queue left standing would be built again every turn
// (a scratch, a share unit, a stored pack) for nothing. The eject's
// announcement tells each enqueuer and author why, and its record nudges the
// repository, so entries past one page are shed next turn.
func (w *queue) shed(ctx context.Context, repoID string, d gitlet.QueueDue, at statelog.Position) error {
	ejected := make([]gitlet.Ejection, len(d.Entries))
	for i, e := range d.Entries {
		ejected[i] = gitlet.Ejection{EntryID: e.EntryID, Reason: gitlet.EjectRepoMirrored}
	}
	return w.eject(ctx, repoID, fmt.Sprintf("target %s of a pull mirror", d.TargetRef), nil, ejected, at)
}

// BatchOf is how many entries one candidate of a target's train takes, of
// waiting live entries at depth and batch: min(batch, ⌈waiting/depth⌉) — one
// while the queue keeps up, so a quiet queue tests each change alone and lands
// it at once, and up to batch as it falls behind (M5-D13). At least one.
func BatchOf(waiting, depth, batch int) int {
	if waiting < 1 || depth < 1 || batch < 1 {
		return 1
	}
	return max(1, min(batch, (waiting+depth-1)/depth))
}

// build builds a train of d's live entries onto its tip as one chain and
// records it, ejecting the entries the chain skipped; it answers whether the
// train was recorded. A chain is never refused whole: an entry it did not
// reach within its time (gitlet.ChainBuildDeadline) or its objects' bound
// (gitlet.MaxChainObjectBytes) is absent from its answer, and stays queued for
// the train after this one.
func (w *queue) build(ctx context.Context, repoID string, d gitlet.QueueDue, at statelog.Position) (bool, error) {
	about := fmt.Sprintf("target %s at %s", d.TargetRef, short(d.Tip))
	// MORE WAIT THAN THE PAGE NAMES: it names depth × batch, so one more
	// takes BatchOf to batch, as any number past depth × (batch − 1) does.
	waiting := len(d.Entries)
	if d.More {
		waiting++
	}
	n := BatchOf(waiting, d.Depth, d.Batch)
	take := d.Entries[:min(len(d.Entries), n*d.Depth)]
	opID := statelog.NewOpID(w.now(), "")
	train := gitlet.QueueTrainID(opID)
	b := gitlet.ChainBuild{RepoID: repoID, ChainID: train, TargetRef: d.TargetRef, Base: d.Tip, Seen: at}
	for _, e := range take {
		b.Entries = append(b.Entries, gitlet.ChainEntry{EntryID: e.EntryID, Change: e.Change, Head: e.HeadOID,
			Method: e.Method, Merger: e.EnqueuedBy})
	}
	built, err := w.chains.BuildChain(ctx, b)
	switch {
	case err != nil:
		return false, fmt.Errorf("home: build the train of %s: %w", about, err)
	case built.Status != "":
		// THE REPOSITORY'S ROW REFUSES EVERY MERGE in the copy the chain was
		// to be built over, newer than this snapshot: nothing was built, and
		// the next turn reads the queue again (a pull mirror's, to shed).
		w.log.Debug("code_queue_chain_refused", "repo", repoID, "about", about, "refusal", built.Status,
			"detail", built.Detail)
		return false, nil
	}
	applied := false
	defer func() {
		if settleErr := built.Settle(context.WithoutCancel(ctx), applied); settleErr != nil {
			w.log.Warn("code_queue_chain_not_settled", "repo", repoID, "about", about, "err", settleErr)
		}
	}()
	var (
		ejected []gitlet.Ejection
		kept    []gitlet.ChainCommit
	)
	for _, c := range built.Commits {
		switch {
		case c.Skipped == "":
			kept = append(kept, c)
		case c.Skipped == gitlet.WriteConflict || c.Skipped == gitlet.WriteTooLarge ||
			c.Skipped == gitlet.WriteRejectedObject:
			// A MERGE THAT CANNOT BE BUILT is its entry's conflict, named
			// with no paths: past a merge's budget or pack bound, past
			// gitlet.MaxChainObjectBytes alone, or writing an object git's
			// check refuses. Left queued, every later train would skip it.
			x := gitlet.Ejection{EntryID: c.EntryID, Reason: gitlet.EjectConflict}
			for _, conflict := range c.Conflicts {
				if len(x.Paths) < gitlet.MaxEjectedPaths && gitlet.ConflictPathCarried(conflict.Path) &&
					gitlet.ConflictsCost(append(slices.Clone(x.Paths), conflict.Path)) <= gitlet.MaxConflictBytes {
					x.Paths = append(x.Paths, conflict.Path)
				}
			}
			ejected = append(ejected, x)
		case c.Skipped == gitlet.WriteNotFastForward:
			ejected = append(ejected, gitlet.Ejection{EntryID: c.EntryID, Reason: gitlet.EjectNotFastForward})
		default:
			// A HEAD THE TARGET ALREADY HOLDS is its change landed, which
			// the change work records and which ends its entry.
			w.log.Debug("code_queue_entry_skipped", "repo", repoID, "about", about, "entry", c.EntryID,
				"skipped", c.Skipped, "detail", c.Detail)
		}
	}
	if len(ejected) > 0 {
		if err = w.eject(ctx, repoID, about, nil, ejected, at); err != nil {
			return false, err
		}
	}
	if len(kept) == 0 {
		return false, nil
	}
	cand := gitlet.Candidate{RepoID: repoID, TargetRef: d.TargetRef, Train: train, BaseOID: d.Tip, Pack: built.Pack}
	for start := 0; start < len(kept); start += n {
		batch := kept[start:min(start+n, len(kept))]
		ordinal := len(cand.Candidates) + 1
		tc := gitlet.TrainCandidate{CandidateID: gitlet.QueueCandidateID(train, ordinal), Ordinal: ordinal,
			TipOID: batch[len(batch)-1].Commit}
		for _, c := range batch {
			tc.Entries = append(tc.Entries, gitlet.CandidateEntry{EntryID: c.EntryID, Commit: c.Commit})
		}
		cand.Candidates = append(cand.Candidates, tc)
	}
	// THE DROP EPOCH OF THE COPY THE CHAIN WAS BUILT FROM (M6-D4): a train
	// built over objects an expunge begun since is dropping is refused
	// `expunging`, and the next turn builds it again past the begin.
	res, err := w.estate.Queue(ctx, gitlet.QueueProposal{OpID: opID, Seen: at, DropEpoch: built.DropEpoch,
		Candidate: &cand})
	if err != nil {
		// RECORDED OR NOT, NOBODY KNOWS: answered as recorded, so the
		// given-up attempts wait a turn rather than cut a run the train
		// may be testing.
		return true, fmt.Errorf("home: record the train of %s: %w", about, err)
	}
	applied = res.Outcome == statelog.OutcomeApplied
	if applied && res.Refusal == "" {
		if w.rate.built(repoID, d.TargetRef, train, w.now()) {
			w.round(roundAbandoned)
		}
	}
	if res.Refusal != "" {
		w.log.Debug("code_queue_record_refused", "repo", repoID, "about", about, "write", "candidate",
			"refusal", res.Refusal, "detail", res.Detail)
	}
	return res.Refusal == "", nil
}

// test is a standing train's next step: its longest green prefix landed (a
// candidate's tip holds every change before it, so its pass is the whole
// prefix's); else its tips dispatched up to the first red one, past which
// every tip holds the failure too; and a red first candidate bisected.
func (w *queue) test(ctx context.Context, repoID string, d gitlet.QueueDue, memo *defsMemo,
	at statelog.Position) error {

	green := -1
	for i, c := range d.Train {
		s := commitStates(c.Tip(), d.Required)
		if len(s.failed) > 0 {
			break
		}
		if s.passed() {
			green = i
		}
	}
	if green >= 0 {
		return w.land(ctx, repoID, d, green, at)
	}
	var defs *checksDefs
	for i, c := range d.Train {
		s := commitStates(c.Tip(), d.Required)
		if len(s.failed) > 0 {
			if i == 0 {
				return w.bisect(ctx, repoID, d, s.failed, memo, at)
			}
			return nil
		}
		if len(s.missing)+len(s.requeue) == 0 {
			continue
		}
		if defs == nil {
			var err error
			if defs, err = w.trainDefs(ctx, repoID, d, memo, at); err != nil || defs == nil {
				return err
			}
		}
		if err := w.probe(ctx, repoID, d, c, c.Tip(), gitlet.PurposeCandidate, s, defs, at); err != nil {
			return err
		}
	}
	return nil
}

// trainDefs is the checks file d's train is measured by: its base's (the
// target's tip), never a candidate's own (M5-D15). Where it defines a
// required check in no form that runs, no candidate can be green, so every
// entry the train tests is EJECTED checks_undefined naming those checks and
// nil answered: nothing the queue tests can land until the target defines
// them. Never check_failed, which says a run measured the change.
func (w *queue) trainDefs(ctx context.Context, repoID string, d gitlet.QueueDue, memo *defsMemo,
	at statelog.Position) (*checksDefs, error) {

	base := d.Train[0].BaseOID
	defs, err := memo.at(ctx, w.estate, repoID, base, at)
	if err != nil {
		return nil, fmt.Errorf("home: read the checks file of %s's train base %s: %w", d.TargetRef, short(base),
			err)
	}
	var undefined []string
	for _, name := range d.Required {
		if defs.invalid != "" || !slices.ContainsFunc(defs.file.Checks, func(c gitlet.CheckDef) bool {
			return c.Name == name
		}) {
			undefined = append(undefined, name)
		}
	}
	if len(undefined) == 0 {
		return defs, nil
	}
	about := fmt.Sprintf("target %s at %s", d.TargetRef, short(base))
	w.log.Warn("code_queue_checks_undefined", "repo", repoID, "about", about, "checks", undefined,
		"invalid", defs.invalid, "detail", "the target's checks file defines a required check in no form "+
			"that runs, so no candidate can pass it: the train's entries are ejected checks_undefined")
	var ejected []gitlet.Ejection
	for _, c := range d.Train {
		for _, commit := range c.Commits {
			ejected = append(ejected, gitlet.Ejection{EntryID: commit.EntryID,
				Reason: gitlet.EjectChecksUndefined, Checks: undefined})
		}
	}
	return nil, w.eject(ctx, repoID, about, endOf(d, roundEjected), ejected, at)
}

// probe queues on commit of candidate c what s says it owes of the required
// checks, under defs, the train base's checks file: the first attempt of each
// it holds none of and the next of each its infrastructure ended.
func (w *queue) probe(ctx context.Context, repoID string, d gitlet.QueueDue, c gitlet.QueueWorkCandidate,
	commit gitlet.QueueWorkCommit, purpose gitlet.CheckPurpose, s landingStates, defs *checksDefs,
	at statelog.Position) error {

	about := fmt.Sprintf("target %s candidate %d at %s", d.TargetRef, c.Ordinal, short(commit.Commit))
	q := gitlet.CheckQueued{CheckKey: gitlet.CheckKey{RepoID: repoID, OID: commit.Commit, Target: d.TargetRef},
		Purpose: purpose, CandidateID: c.CandidateID, Base: c.BaseOID, DefsOID: defs.oid, Required: true}
	if purpose == gitlet.PurposeBisect {
		q.EntryID = commit.EntryID
	}
	owe := func(name string, attempt int, reason gitlet.QueueReason) error {
		i := slices.IndexFunc(defs.file.Checks, func(def gitlet.CheckDef) bool { return def.Name == name })
		def := defs.file.Checks[i]
		a := q
		a.Name, a.Attempt, a.Reason, a.Def = name, attempt, reason, &def
		return w.checks.queue(ctx, about, a, at)
	}
	for _, name := range s.missing {
		if err := owe(name, 1, gitlet.QueueDispatch); err != nil {
			return err
		}
	}
	for _, name := range s.requeue {
		if err := owe(name, s.latest[name]+1, gitlet.QueueRequeue); err != nil {
			return err
		}
	}
	return nil
}

// land lands d's train's candidates up to and including green, in one land
// record, each entry as its enqueuer; an entry its decide would not land is
// ejected for why, and every entry landed that asked it has its work item
// closed as its enqueuer.
func (w *queue) land(ctx context.Context, repoID string, d gitlet.QueueDue, green int, at statelog.Position) error {
	prefix := d.Train[:green+1]
	tip := prefix[len(prefix)-1].TipOID
	about := fmt.Sprintf("target %s at %s", d.TargetRef, short(tip))
	opID := statelog.NewOpID(w.now(), "")
	l := gitlet.Land{RepoID: repoID, LandID: gitlet.QueueLandID(opID), TargetRef: d.TargetRef,
		BaseOID: prefix[0].BaseOID, TipOID: tip, Train: prefix[0].Train,
		Node: w.node, ReceivedAt: w.now().UTC()}
	var landed []gitlet.QueueWorkEntry
	for _, c := range prefix {
		for _, commit := range c.Commits {
			e := d.Entries[slices.IndexFunc(d.Entries, func(e gitlet.QueueWorkEntry) bool {
				return e.EntryID == commit.EntryID
			})]
			l.Entries = append(l.Entries, gitlet.LandEntry{EntryID: e.EntryID, ChangeID: e.Change.ID,
				Commit: commit.Commit})
			landed = append(landed, e)
		}
	}
	// THE DROP EPOCH THE TRAIN WAS READ AT (gitlet.QueueDue.DropEpoch, M6-D4):
	// its candidates were recorded at their chain's, and an expunge begun
	// since holds their tips live, so only one begun after this read refuses
	// the landing `expunging`, for the next turn to read the train again.
	res, err := w.estate.Queue(ctx, gitlet.QueueProposal{OpID: opID, Seen: at, DropEpoch: d.DropEpoch, Land: &l})
	switch {
	case err != nil:
		return fmt.Errorf("home: land %s: %w", about, err)
	case res.Refusal != "":
		var ejected []gitlet.Ejection
		for _, r := range res.Refused {
			switch r.Refusal {
			case gitlet.MergeRefusalGateUnmet:
				ejected = append(ejected, gitlet.Ejection{EntryID: r.EntryID, Reason: gitlet.EjectGateUnmet})
			case gitlet.MergeRefusalHeadMoved:
				ejected = append(ejected, gitlet.Ejection{EntryID: r.EntryID, Reason: gitlet.EjectHeadMoved})
			}
		}
		w.log.Debug("code_queue_record_refused", "repo", repoID, "about", about, "write", "land",
			"refusal", res.Refusal, "detail", res.Detail, "ejecting", len(ejected))
		if len(ejected) > 0 {
			return w.eject(ctx, repoID, about, endOf(d, roundEjected), ejected, at)
		}
		return nil
	case res.Outcome == statelog.OutcomeUnknown:
		// NOTHING ESTABLISHED: the landing may be on no log, so it is not
		// counted, timed or closed on its word; the next turn reads its
		// entries landed with their closes owed, or its train still testing.
		w.log.Debug("code_queue_close_owed", "repo", repoID, "about", about, "outcome", res.Outcome,
			"detail", "nothing is known here of the landing, so its work items are closed by the turn "+
				"that reads it applied")
		return nil
	}
	// DURABLE, applied here or pending (every node applies a pending record),
	// so the round ENDED at the append and is counted and timed here: left
	// uncounted, the next turn would read its train abandoned, its changes
	// never counted landed.
	took, timed := w.rate.landed(repoID, d.TargetRef, l.Train, w.now())
	w.count(metrics.CodeQueueLanded, uint64(len(l.Entries)), nil)
	w.round(roundLanded)
	if timed && w.metrics != nil {
		w.metrics.Observe(metrics.CodeQueueRoundDuration, took, nil)
	}
	if res.Outcome != statelog.OutcomeApplied {
		// NOT YET APPLIED HERE: its changes are not merged on this node's
		// rows, so no work item is closed on its word; the apply leaves each
		// close owed on its entry's row for a later turn ([queue.closeOwed]).
		w.log.Debug("code_queue_close_owed", "repo", repoID, "about", about, "outcome", res.Outcome,
			"detail", "the landing is appended and not applied here, so its work items are closed by the "+
				"turn that reads it applied")
		return nil
	}
	// THE CLOSES IT OWES AT ONCE, under the same operations and settling a
	// later turn's read would give them, so whichever runs second is
	// answered done.
	var owed []gitlet.QueueWorkEntry
	for _, e := range landed {
		if e.CloseTask && e.WorkItem != nil {
			e.CloseOp = gitlet.CloseWorkItemOpID(opID, e.EntryID)
			owed = append(owed, e)
		}
	}
	return w.closeOwed(ctx, repoID, owed, res.Position)
}

// closeOwed closes the work item of each of owed (landed entries still owing
// a close, gitlet.QueueDueAnswer.QueueOwedCloses) AS ITS ENQUEUER and for
// whom the enqueue was made (TaskClose.WorkingFor), under the operation its
// landing named (gitlet.CloseWorkItemOpID), so a close asked again, by any
// home, is the same tracker write, which the tracker's ledger answers once.
// Every close that ENDED is settled in ONE close_settled record: closed, or
// not_closed with the sentence its change's authors and enqueuer read. A
// close whose end is unknown stays owed for the next turn on whichever home
// holds the repository and fails this turn, as does every ended close whose
// settle did not append.
func (w *queue) closeOwed(ctx context.Context, repoID string, owed []gitlet.QueueWorkEntry,
	at statelog.Position) error {

	var (
		failed  []error
		settled []gitlet.SettledClose
		ended   []gitlet.QueueWorkEntry
	)
	for _, e := range owed {
		if ctx.Err() != nil {
			break
		}
		s := gitlet.SettledClose{EntryID: e.EntryID, Outcome: gitlet.CloseClosed}
		if e.WorkItem == nil {
			// THE CHANGE NAMES NO WORK ITEM ANY MORE, or the repository
			// holds the change no more: nothing is left to close.
			s.Outcome, s.Detail = gitlet.CloseNotClosed, "the change no longer names a work item to close"
		} else {
			err := w.tasks.CloseWorkItem(ctx, TaskClose{OpID: e.CloseOp, RepoID: repoID, Change: e.Change,
				WorkItem: *e.WorkItem, As: e.EnqueuedBy, WorkingFor: e.WorkingFor})
			var refused *NotClosed
			switch {
			case errors.As(err, &refused):
				s.Outcome, s.Detail = gitlet.CloseNotClosed, refused.Why
			case err != nil:
				w.log.Warn("code_queue_close_failed", "repo", repoID, "change", e.Change.Number,
					"work_item", e.WorkItem.ID, "err", err,
					"detail", "the close stays owed, and the next turn asks it again")
				failed = append(failed, fmt.Errorf("home: close the work item of change %d: %w",
					e.Change.Number, err))
				continue
			}
		}
		s.Detail = textcut.Within(s.Detail, gitlet.MaxCloseDetail)
		settled, ended = append(settled, s), append(ended, e)
	}
	// ONE RECORD: both callers hand at most gitlet.MaxOwedCloses (a page's
	// owed closes, or one landing's gitlet.MaxLandEntries, the same bound);
	// a longer list is cut at the record's bound.
	for start := 0; start < len(settled); start += gitlet.MaxOwedCloses {
		end := min(start+gitlet.MaxOwedCloses, len(settled))
		if err := w.settle(ctx, repoID, settled[start:end], ended[start:end], at); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// settle records how the closes of entries ended (at most
// gitlet.MaxOwedCloses, closes[i] entries[i]'s) in one close_settled record,
// which takes each out of every home's queue work. The committer skips an
// entry another home settled first and refuses stale a record of none still
// owing; a turn that reads the closes still owed before the record applies
// asks them again, which the tracker answers done.
func (w *queue) settle(ctx context.Context, repoID string, closes []gitlet.SettledClose,
	entries []gitlet.QueueWorkEntry, at statelog.Position) error {

	// A FRESH OPERATION, never one derived from a landing's: the rows it
	// clears guard a settle, so asked twice the second skips them, and an id
	// minted at a landing's instant falls out of the log ledger's retention.
	res, err := w.estate.Queue(ctx, gitlet.QueueProposal{OpID: statelog.NewOpID(w.now(), ""), Seen: at,
		CloseSettled: &gitlet.CloseSettled{RepoID: repoID, Settled: closes}})
	switch {
	case err != nil:
		return fmt.Errorf("home: settle the closes of %d changes' work items: %w", len(closes), err)
	case res.Refusal != "":
		w.log.Debug("code_queue_record_refused", "repo", repoID, "write", "close_settled",
			"closes", len(closes), "refusal", res.Refusal, "detail", res.Detail)
		return nil
	}
	for i, s := range closes {
		if s.Outcome == gitlet.CloseNotClosed {
			w.log.Info("code_queue_close_not_made", "repo", repoID, "change", entries[i].Change.Number,
				"detail", s.Detail)
		}
	}
	return nil
}

// bisect finds which entry of d's first candidate the failed checks failed
// on, from each entry's commit's own attempts; each commit is the batch's
// merge up to its entry, so its checks judge the entries up to it. red is the
// first commit one of failed FAILED on (the tip at the latest); good the last
// before it every one passed on, else the train's base (the target's tip,
// which its queue landed green). Adjacent, red's entry is the culprit,
// EJECTED check_failed naming the checks that failed on it and the train
// (candidate, base, red's commit); otherwise the commit midway is PROBED
// (purpose bisect). A probe still running, or ended in a way only a person's
// rerun moves, waits for the record that ends it.
func (w *queue) bisect(ctx context.Context, repoID string, d gitlet.QueueDue, failed []string, memo *defsMemo,
	at statelog.Position) error {

	c := d.Train[0]
	red := slices.IndexFunc(c.Commits, func(commit gitlet.QueueWorkCommit) bool {
		return slices.ContainsFunc(failed, func(name string) bool {
			held, ok := commit.Check(name)
			return ok && held.State == gitlet.CheckStateFailed
		})
	})
	good := -1
	for i := red - 1; i >= 0; i-- {
		if commitStates(c.Commits[i], failed).passed() {
			good = i
			break
		}
	}
	if red == good+1 {
		culprit := c.Commits[red]
		x := gitlet.Ejection{EntryID: culprit.EntryID, Reason: gitlet.EjectCheckFailed,
			TestedIn: &gitlet.EjectionTrain{CandidateID: c.CandidateID, BaseOID: c.BaseOID, CommitOID: culprit.Commit}}
		for _, name := range failed {
			if held, ok := culprit.Check(name); ok && held.State == gitlet.CheckStateFailed {
				x.Checks = append(x.Checks, name)
			}
		}
		return w.eject(ctx, repoID, fmt.Sprintf("target %s candidate %d at %s", d.TargetRef, c.Ordinal,
			short(culprit.Commit)), endOf(d, roundBisected), []gitlet.Ejection{x}, at)
	}
	mid := c.Commits[(good+red+1)/2]
	s := commitStates(mid, failed)
	if len(s.missing)+len(s.requeue) == 0 {
		return nil
	}
	defs, err := w.trainDefs(ctx, repoID, d, memo, at)
	if err != nil || defs == nil {
		return err
	}
	return w.probe(ctx, repoID, d, c, mid, gitlet.PurposeBisect, s, defs, at)
}

// eject asks the repository's committer to put entries out of their queue,
// abandoning each one's train; about names the target for errors and logs. A
// refusal is the rows answering, read again next turn; only an error of the
// write itself fails the turn. ends, where set, is the train the ejection
// ENDS and how ([endOf]): counted once its first record is appended, its
// round left untimed ([mergeRate.ended]); nil for entries no train was
// testing (a chain's skipped merges, a pull mirror's entries).
func (w *queue) eject(ctx context.Context, repoID, about string, ends *trainEnd, entries []gitlet.Ejection,
	at statelog.Position) error {

	for start := 0; start < len(entries); start += gitlet.MaxLandEntries {
		part := entries[start:min(start+gitlet.MaxLandEntries, len(entries))]
		res, err := w.estate.Queue(ctx, gitlet.QueueProposal{OpID: statelog.NewOpID(w.now(), ""), Seen: at,
			Eject: &gitlet.Eject{RepoID: repoID, Entries: part}})
		switch {
		case err != nil:
			return fmt.Errorf("home: eject %d entries of %s: %w", len(part), about, err)
		case res.Refusal != "":
			w.log.Debug("code_queue_record_refused", "repo", repoID, "about", about, "write", "eject",
				"refusal", res.Refusal, "detail", res.Detail)
			continue
		}
		// APPENDED, applied here or not yet: the log applies it in order,
		// and the next turn reads its entries gone.
		for _, x := range part {
			w.count(metrics.CodeQueueEjected, 1, metrics.Attrs{"reason": string(x.Reason)})
		}
		if ends != nil {
			w.rate.ended(repoID, ends.target, ends.train)
			w.round(ends.outcome)
			ends = nil
		}
	}
	return nil
}

// How a train of a target's queue ended, as crewlet.code.queue.rounds
// counts it.
const (
	roundLanded    = "landed"
	roundBisected  = "bisected"
	roundEjected   = "ejected"
	roundAbandoned = "abandoned"
)

// trainEnd is the train an eject ends, and how.
type trainEnd struct {
	target, train, outcome string
}

// endOf is the end of d's train an eject of its entries makes, outcome being
// how.
func endOf(d gitlet.QueueDue, outcome string) *trainEnd {
	e := &trainEnd{target: d.TargetRef, outcome: outcome}
	if len(d.Train) > 0 {
		e.train = d.Train[0].Train
	}
	return e
}

// round counts a train of a target's queue ended, outcome being how.
func (w *queue) round(outcome string) {
	w.count(metrics.CodeQueueRounds, 1, metrics.Attrs{"outcome": outcome})
}

// count adds n to the counter name under attrs, where the node records
// metrics at all.
func (w *queue) count(name string, n uint64, attrs metrics.Attrs) {
	if w.metrics != nil {
		w.metrics.Add(name, n, attrs)
	}
}

// cancelAbandoned cancels each of attempts (one the queue queued for a train
// since given up by an eject, a dequeue, a moved target or a new train,
// gitlet.AbandonedCheck) for its reason, `dequeued` or `abandoned`, one check
// write each, as checks.go's cancel ends a moved-on head's: nothing will land
// its commit, and a runner claims oldest first, so left alone it holds one of
// a Gitlet node's gitlet.max_checks containers ahead of the live train's. A
// box running one stops once its row ends. A refusal is the rows answering
// (the attempt ended meanwhile); only a write that decided nothing fails the
// turn, and every attempt is asked either way.
func (w *queue) cancelAbandoned(ctx context.Context, repoID string, attempts []gitlet.AbandonedCheck,
	at statelog.Position) error {

	var failed []error
	for _, x := range attempts {
		summary := "its merge queue gave up the train this candidate was in"
		if x.Reason == gitlet.CheckReasonDequeued {
			summary = "a change this candidate merged left its merge queue"
		}
		if err := w.checks.write(ctx, fmt.Sprintf("target %s candidate %s at %s", x.Target, x.CandidateID,
			short(x.OID)), gitlet.ChecksRequest{OpID: statelog.NewOpID(w.now(), ""), Seen: at,
			Finished: &gitlet.CheckFinished{CheckKey: x.CheckKey, State: gitlet.CheckStateCancelled, Reason: x.Reason,
				FinishedAt: w.now().UTC(), Summary: summary}},
			fmt.Sprintf("cancel check %s of %s for %s", x.Name, short(x.OID), x.Target)); err != nil {
			failed = append(failed, err)
		}
	}
	if len(attempts) > 0 {
		w.log.Debug("code_queue_checks_cancelled", "repo", repoID, "attempts", len(attempts),
			"failed", len(failed))
	}
	return errors.Join(failed...)
}

// commitStates is where each of names stands on commit by its latest
// attempt ([landingStates]).
func commitStates(commit gitlet.QueueWorkCommit, names []string) landingStates {
	return statesOf(commit.Check, names)
}
