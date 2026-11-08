package home

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/textcut"
)

// WorkerQueue is the merge queue's worker's name, as crewlet.code.home.passes
// counts its turns.
const WorkerQueue = "queue"

// Chains builds a chain of merges in one scratch on this node —
// internal/code/host's *Server satisfies it (BuildChain): the queue's
// candidates, built by the repository's home, never by this package, which
// imports no git host.
type Chains interface {
	BuildChain(ctx context.Context, b code.ChainBuild) (code.ChainResult, error)
}

// Tasks closes a landed change's work item AS THE ENTRY'S ENQUEUER — step 2
// of a queued merge that asked it (M5-D27): the engine wires the company's
// tracker, which decides the close as that principal, as it decides every
// write of theirs. The queue never closes a work item on its own account.
//
// CloseWorkItem answers nil for an item closed, or finished already; a
// [*NotClosed] for a close that will not be made however often it is asked,
// which the home settles not_closed and names on the change; and any other
// error for a close whose end is not known yet — the tracker unreachable,
// its answer unknown — which leaves the close OWED on the entry's row
// (code_queue.close_op), for the next turn on whichever home holds the
// repository.
type Tasks interface {
	CloseWorkItem(ctx context.Context, c TaskClose) error
}

// NotClosed is a close [Tasks] will not make however often it is asked: the
// item is a vendor's, which is closed where it lives; the enqueuer is bound
// to no seat, has left the chart, or may no longer update it; the tracker
// refuses the close for good (the item gone, or refusing to be set done). Why
// is the sentence the change's authors and its enqueuer read beside `merged`
// (code.SettledClose.Detail), and Err the cause, for errors.Is.
type NotClosed struct {
	Why string
	Err error
}

func (n *NotClosed) Error() string { return n.Why }

func (n *NotClosed) Unwrap() error { return n.Err }

// TaskClose is one landed change's work item to close: the step's operation
// id — derived from its landing's, so the step asked again is the same write,
// which the tracker's ledger collapses — the change, its work item and who
// closes it.
type TaskClose struct {
	OpID     string
	RepoID   string
	Change   code.ChainChange
	WorkItem types.WorkItem
	As       code.Principal
}

// queue is the home's MERGE QUEUE (M5-D12, M5-D13): for each queue target of
// a repository holding live entries, from ONE snapshot of its queue work
// (internal/code's QueueDue), the next step of its train:
//
//   - NO TRAIN, or one its target moved under — a person's override merge, a
//     person's push — or one testing an entry that is no longer live: a new
//     train BUILT onto the target's tip, its candidates min(B, ⌈W/D⌉) of the
//     W live entries each, up to depth D, as ONE chain in one scratch on this
//     node (Chains), and RECORDED (a candidate record, which abandons the
//     target's earlier train); an entry the chain skipped — a conflict, a
//     fast-forward that is none — EJECTED with its paths;
//   - a train testing: each candidate's tip DISPATCHED the required checks it
//     holds no attempt of, defined by the checks file at the train's base and
//     never by a candidate's own (M5-D15), and an attempt its infrastructure
//     ended queued again — up to the first candidate a check failed on, past
//     which every candidate holds the failure too;
//   - the LONGEST GREEN PREFIX landed in ONE land record: every candidate up
//     to the last whose tip passed every required check with none failed
//     before it — a candidate's tip holds every change before it, so its pass
//     is the whole prefix's — each entry landed as its ENQUEUER, and step 2
//     run for each that asked its work item closed (Tasks), the close OWED
//     on the entry's row from the landing's apply until a close_settled
//     record says how it ended, so a later turn on any home makes it where
//     this one could not ([queue.closeOwed]);
//   - the train's FIRST candidate red: its batch BISECTED over its entries'
//     own commits, each the batch's merge up to that entry — probed midway
//     between the last commit known good (the train's base, the target's tip,
//     before any) and the first known red, the failed checks queued there
//     (purpose bisect) — down to the culprit, EJECTED check_failed naming the
//     checks; the eject abandons the train, and the next turn builds again
//     without it;
//   - a PULL MIRROR's target (code.QueueDue.Mirrored), whose candidates and
//     landings are refused repo_mirrored: every live entry EJECTED
//     repo_mirrored, and no train built for it ([queue.shed]).
//
// Beside them, every attempt it queued for a train it has since GIVEN UP — by
// an eject, a dequeue, a target a person moved, a new train — still queued or
// running on a commit no train of its target tests or landed is CANCELLED,
// `dequeued` where a change its candidate merged left the queue and
// `abandoned` otherwise (cancelAbandoned), by any turn but one that recorded
// a new train (Work): the records that give a train up end its candidates and
// leave its checks, which are the checks writer's, and each measures a commit
// nothing will land while it holds a runner's cell ahead of the live train's.
// A commit a later train tests again, which a chain built from the same rows
// may, is queued again by that train's test (code.CheckReason.GivenUp).
//
// Each target's MERGE RATE is measured as its queue work is read — its
// enqueues within code.MergeRateWindow against what its depth and batch land
// at the rounds this node timed its trains at (mergerate.go) — and handed to
// the node's alarm reading ([queue.AlarmReading]), which raises
// code_merge_rate_over_capacity (M5-D28).
//
// A candidate and a landing state the repository's DROP EPOCH (M6-D4): the
// candidate the epoch of the copy its chain was built from (code.ChainResult),
// the landing the epoch its snapshot read (code.QueueDue) — so a train built
// before an expunge began is refused `expunging` and built again, never
// recorded over objects the expunge retires.
//
// Every write is a record decided where it lands — a candidate, an eject and
// a land by the repository's committer (code.queue), a check by its checks
// writer — each refusing what its own snapshot does not prove, so a step
// decided from a snapshot the log has moved past is refused and read again
// next turn rather than recorded wrong; every record it lands nudges the
// repository again, which is how a train moves from one step to the next.
type queue struct {
	estate Estate
	chains Chains
	tasks  Tasks
	// checks is the check work's own writer: a candidate's checks are
	// queued, and the checks file read, as a head's and a tip's are.
	checks *checks
	// rate is the queue's merge rate, measured from each target's queue
	// work and the rounds this node timed (mergerate.go).
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

// Due is a page of the repositories holding a live entry: the `queue_due`
// listing (internal/code's ReadQueueDue).
func (w *queue) Due(ctx context.Context, floor statelog.Position, cursor string) ([]string, string, error) {
	a, err := w.estate.Read(ctx, code.ReadQuery{Kind: code.ReadQueueDue, Cursor: cursor, Fresh: freshAt(floor),
		QueueDue: &code.QueueDueQuery{}})
	if err != nil {
		return nil, "", err
	}
	return a.DueRepos, a.Next, nil
}

// Work is one repository's queue work, a target a page: each target's next
// step, a failure of one left for the next pass while the others are worked.
// Every page counts its target's enqueues within the merge rate's window, and
// a turn that read every page drops the merge rate of each target it did not
// meet, whose queue has drained ([mergeRate]).
//
// The given-up trains' attempts the first page answers are cancelled once
// every target has taken its step — and NOT in a turn whose step recorded a
// new train: a chain built again from the same rows is the same commits byte
// for byte (code/host's BuildChain), so the new train may test commits a
// given-up attempt is measuring, and the next turn, which its candidate
// record nudges, reads them as the live train's and cancels only the rest.
// Cancelled one turn early, such a run would be cut and queued again
// (code.CheckReason.GivenUp), which costs its cell the run so far.
//
// The closes the first page answers OWED — landed entries whose work item is
// not closed yet, by a landing answered pending, a home that stopped or
// handed the repository on before its close, or a tracker that could not
// answer (M5-D27) — are closed and settled last ([queue.closeOwed]), so a
// close no turn could make is asked again by every turn after, on whichever
// home, until it ends.
func (w *queue) Work(ctx context.Context, repoID string, floor statelog.Position) error {
	q := code.ReadQuery{Kind: code.ReadQueueDue, Fresh: freshAt(floor), QueueDue: &code.QueueDueQuery{
		RepoID: repoID, EnqueuedSince: w.now().Add(-code.MergeRateWindow)}}
	memo := newDefsMemo()
	var (
		failed    []error
		met       []string
		abandoned []code.AbandonedCheck
		owed      []code.QueueWorkEntry
		readAt    statelog.Position
		built     bool
	)
	for {
		a, err := w.estate.Read(ctx, q)
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
func (w *queue) target(ctx context.Context, repoID string, d code.QueueDue, memo *defsMemo,
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
	case len(d.Required) > 0 && d.Tip == code.ZeroOID:
		// NO TIP TO BUILD ONTO: the branch was deleted under its queue; a
		// person's push creating it again gives its train a base.
		w.log.Debug("code_queue_no_target", "repo", repoID, "about", fmt.Sprintf("target %s at %s",
			d.TargetRef, short(d.Tip)))
	}
	return false, nil
}

// shed puts every live entry of d's target out, ejected repo_mirrored: its
// repository is a PULL MIRROR, which nothing merges into — a candidate and a
// landing on one are refused repo_mirrored — so a train built of them could
// never land, and a queue left standing would be built again on every turn, a
// scratch, a share unit and a stored pack each time, for nothing. Each
// entry's enqueuer and its change's authors are told why by the eject's
// announcement, and the record nudges the repository again, so a target
// holding more entries than one page names is shed on the next turn.
func (w *queue) shed(ctx context.Context, repoID string, d code.QueueDue, at statelog.Position) error {
	ejected := make([]code.Ejection, len(d.Entries))
	for i, e := range d.Entries {
		ejected[i] = code.Ejection{EntryID: e.EntryID, Reason: code.EjectRepoMirrored}
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
// reach within its time (code.ChainBuildDeadline) or its objects' bound
// (code.MaxChainObjectBytes) is absent from its answer, and stays queued for
// the train after this one.
func (w *queue) build(ctx context.Context, repoID string, d code.QueueDue, at statelog.Position) (bool, error) {
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
	train := code.QueueTrainID(opID)
	b := code.ChainBuild{RepoID: repoID, ChainID: train, TargetRef: d.TargetRef, Base: d.Tip, Seen: at}
	for _, e := range take {
		b.Entries = append(b.Entries, code.ChainEntry{EntryID: e.EntryID, Change: e.Change, Head: e.HeadOID,
			Method: e.Method, Merger: e.EnqueuedBy})
	}
	built, err := w.chains.BuildChain(ctx, b)
	switch {
	case err != nil:
		return false, fmt.Errorf("home: build the train of %s: %w", about, err)
	case built.Status != "":
		// THE REPOSITORY'S ROW REFUSES EVERY MERGE, as the copy the chain was
		// to be built over holds it — past this snapshot: nothing was built,
		// and the next turn reads the queue again (a pull mirror's, to shed).
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
		ejected []code.Ejection
		kept    []code.ChainCommit
	)
	for _, c := range built.Commits {
		switch {
		case c.Skipped == "":
			kept = append(kept, c)
		case c.Skipped == code.WriteConflict || c.Skipped == code.WriteTooLarge ||
			c.Skipped == code.WriteRejectedObject:
			// A MERGE THAT CANNOT BE BUILT is its entry's conflict: one
			// past a merge's own budget or pack bound, or past the objects a
			// chain may hold (code.MaxChainObjectBytes) alone, or one
			// writing an object git's check refuses, is as unbuildable as
			// one git marks, and named with no paths — left queued, every
			// train after this one would skip it again.
			x := code.Ejection{EntryID: c.EntryID, Reason: code.EjectConflict}
			for _, conflict := range c.Conflicts {
				if len(x.Paths) < code.MaxEjectedPaths && code.ConflictPathCarried(conflict.Path) &&
					code.ConflictsCost(append(slices.Clone(x.Paths), conflict.Path)) <= code.MaxConflictBytes {
					x.Paths = append(x.Paths, conflict.Path)
				}
			}
			ejected = append(ejected, x)
		case c.Skipped == code.WriteNotFastForward:
			ejected = append(ejected, code.Ejection{EntryID: c.EntryID, Reason: code.EjectNotFastForward})
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
	cand := code.Candidate{RepoID: repoID, TargetRef: d.TargetRef, Train: train, BaseOID: d.Tip, Pack: built.Pack}
	for start := 0; start < len(kept); start += n {
		batch := kept[start:min(start+n, len(kept))]
		ordinal := len(cand.Candidates) + 1
		tc := code.TrainCandidate{CandidateID: code.QueueCandidateID(train, ordinal), Ordinal: ordinal,
			TipOID: batch[len(batch)-1].Commit}
		for _, c := range batch {
			tc.Entries = append(tc.Entries, code.CandidateEntry{EntryID: c.EntryID, Commit: c.Commit})
		}
		cand.Candidates = append(cand.Candidates, tc)
	}
	// THE EPOCH OF THE COPY THE CHAIN WAS BUILT FROM: a train built over
	// objects an expunge begun since left dropping is refused `expunging`,
	// and the next turn builds it again from a copy past the begin.
	res, err := w.estate.Queue(ctx, code.QueueProposal{OpID: opID, Seen: at, DropEpoch: built.DropEpoch,
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

// test is a standing train's next step: its longest green prefix landed;
// failing that, its candidates' tips dispatched up to the first red one; and
// its first candidate, red, bisected.
func (w *queue) test(ctx context.Context, repoID string, d code.QueueDue, memo *defsMemo,
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
		if err := w.probe(ctx, repoID, d, c, c.Tip(), code.PurposeCandidate, s, defs, at); err != nil {
			return err
		}
	}
	return nil
}

// trainDefs is the checks file d's train is measured by: the one its base —
// the target's tip — holds (M5-D15). Where that file defines a required
// check in no form that runs, no candidate of the train can ever be green, so
// every entry the train tests is EJECTED check_failed naming those checks,
// and nil answered: until the target's own file defines them, nothing its
// queue tests can land.
func (w *queue) trainDefs(ctx context.Context, repoID string, d code.QueueDue, memo *defsMemo,
	at statelog.Position) (*checksDefs, error) {

	base := d.Train[0].BaseOID
	defs, err := memo.at(ctx, w.estate, repoID, base, at)
	if err != nil {
		return nil, fmt.Errorf("home: read the checks file of %s's train base %s: %w", d.TargetRef, short(base),
			err)
	}
	var undefined []string
	for _, name := range d.Required {
		if defs.invalid != "" || !slices.ContainsFunc(defs.file.Checks, func(c code.CheckDef) bool {
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
			"that runs, so no candidate can pass it: the train's entries are ejected check_failed")
	var ejected []code.Ejection
	for _, c := range d.Train {
		for _, commit := range c.Commits {
			ejected = append(ejected, code.Ejection{EntryID: commit.EntryID, Reason: code.EjectCheckFailed,
				Checks: undefined})
		}
	}
	return nil, w.eject(ctx, repoID, about, endOf(d, roundEjected), ejected, at)
}

// probe queues on commit of candidate c what s says it owes of the required
// checks, under defs, the train base's checks file: the first attempt of each
// it holds none of and the next of each its infrastructure ended.
func (w *queue) probe(ctx context.Context, repoID string, d code.QueueDue, c code.QueueWorkCandidate,
	commit code.QueueWorkCommit, purpose code.CheckPurpose, s landingStates, defs *checksDefs,
	at statelog.Position) error {

	about := fmt.Sprintf("target %s candidate %d at %s", d.TargetRef, c.Ordinal, short(commit.Commit))
	q := code.CheckQueued{CheckKey: code.CheckKey{RepoID: repoID, OID: commit.Commit, Target: d.TargetRef},
		Purpose: purpose, CandidateID: c.CandidateID, Base: c.BaseOID, DefsOID: defs.oid, Required: true}
	if purpose == code.PurposeBisect {
		q.EntryID = commit.EntryID
	}
	owe := func(name string, attempt int, reason code.QueueReason) error {
		i := slices.IndexFunc(defs.file.Checks, func(def code.CheckDef) bool { return def.Name == name })
		def := defs.file.Checks[i]
		a := q
		a.Name, a.Attempt, a.Reason, a.Def = name, attempt, reason, &def
		return w.checks.queue(ctx, about, a, at)
	}
	for _, name := range s.missing {
		if err := owe(name, 1, code.QueueDispatch); err != nil {
			return err
		}
	}
	for _, name := range s.requeue {
		if err := owe(name, s.latest[name]+1, code.QueueRequeue); err != nil {
			return err
		}
	}
	return nil
}

// land lands d's train's candidates up to and including green, in one land
// record, each entry as its enqueuer; an entry its decide would not land is
// ejected for why, and every entry landed that asked it has its work item
// closed as its enqueuer.
func (w *queue) land(ctx context.Context, repoID string, d code.QueueDue, green int, at statelog.Position) error {
	prefix := d.Train[:green+1]
	tip := prefix[len(prefix)-1].TipOID
	about := fmt.Sprintf("target %s at %s", d.TargetRef, short(tip))
	opID := statelog.NewOpID(w.now(), "")
	l := code.Land{RepoID: repoID, LandID: code.QueueLandID(opID), TargetRef: d.TargetRef,
		BaseOID: prefix[0].BaseOID, TipOID: tip, Train: prefix[0].Train,
		Node: w.node, ReceivedAt: w.now().UTC()}
	var landed []code.QueueWorkEntry
	for _, c := range prefix {
		for _, commit := range c.Commits {
			e := d.Entries[slices.IndexFunc(d.Entries, func(e code.QueueWorkEntry) bool {
				return e.EntryID == commit.EntryID
			})]
			l.Entries = append(l.Entries, code.LandEntry{EntryID: e.EntryID, ChangeID: e.Change.ID,
				Commit: commit.Commit})
			landed = append(landed, e)
		}
	}
	// THE EPOCH THE TRAIN WAS READ AT (code.QueueDue.DropEpoch): its
	// candidates were recorded at their chain's, and an expunge begun since
	// holds their tips live, so only one begun after this read refuses the
	// landing `expunging`, for the next turn to read the train again.
	res, err := w.estate.Queue(ctx, code.QueueProposal{OpID: opID, Seen: at, DropEpoch: d.DropEpoch, Land: &l})
	switch {
	case err != nil:
		return fmt.Errorf("home: land %s: %w", about, err)
	case res.Refusal != "":
		var ejected []code.Ejection
		for _, r := range res.Refused {
			switch r.Refusal {
			case code.MergeRefusalGateUnmet:
				ejected = append(ejected, code.Ejection{EntryID: r.EntryID, Reason: code.EjectGateUnmet})
			case code.MergeRefusalHeadMoved:
				ejected = append(ejected, code.Ejection{EntryID: r.EntryID, Reason: code.EjectHeadMoved})
			}
		}
		w.log.Debug("code_queue_record_refused", "repo", repoID, "about", about, "write", "land",
			"refusal", res.Refusal, "detail", res.Detail, "ejecting", len(ejected))
		if len(ejected) > 0 {
			return w.eject(ctx, repoID, about, endOf(d, roundEjected), ejected, at)
		}
		return nil
	case res.Outcome == statelog.OutcomeUnknown:
		// NOTHING ESTABLISHED HERE: the landing may be on no log at all, so
		// it is neither counted nor timed nor closed on its word; the next
		// turn reads its entries landed, their closes owed on their rows,
		// or its train still testing them.
		w.log.Debug("code_queue_close_owed", "repo", repoID, "about", about, "outcome", res.Outcome,
			"detail", "nothing is known here of the landing, so its work items are closed by the turn "+
				"that reads it applied")
		return nil
	}
	// DURABLE, applied here or pending — every node applies a pending
	// record — so the round ENDED at the landing's append, and is counted
	// and timed there: a landing answered pending and left uncounted would
	// read as its train abandoned on the next turn, its changes never
	// counted landed.
	took, timed := w.rate.landed(repoID, d.TargetRef, l.Train, w.now())
	w.count(metrics.CodeQueueLanded, uint64(len(l.Entries)), nil)
	w.round(roundLanded)
	if timed && w.metrics != nil {
		w.metrics.Observe(metrics.CodeQueueRoundDuration, took, nil)
	}
	if res.Outcome != statelog.OutcomeApplied {
		// NOT YET APPLIED HERE: its changes are not merged on this node's
		// rows, so no work item of theirs is closed on its word. The
		// landing's apply leaves each close it owes on its entry's row,
		// which the next turn's read answers ([queue.closeOwed]).
		w.log.Debug("code_queue_close_owed", "repo", repoID, "about", about, "outcome", res.Outcome,
			"detail", "the landing is appended and not applied here, so its work items are closed by the "+
				"turn that reads it applied")
		return nil
	}
	// THE CLOSES IT OWES AT ONCE, as the rows the landing wrote owe them —
	// the same operations and the same settling a later turn's read would
	// give them, so whichever runs first, the other is answered done.
	var owed []code.QueueWorkEntry
	for _, e := range landed {
		if e.CloseTask && e.WorkItem != nil {
			e.CloseOp = code.CloseWorkItemOpID(opID, e.EntryID)
			owed = append(owed, e)
		}
	}
	return w.closeOwed(ctx, repoID, owed, res.Position)
}

// closeOwed closes the work item each of owed — a landed entry still owing
// its close (code.ReadAnswer.QueueOwedCloses) — asked closed, AS ITS
// ENQUEUER under the operation its landing named (code.CloseWorkItemOpID,
// so a close a home asked before it stopped is the same tracker write
// asked again, which the tracker's ledger answers once), and settles every
// close that ENDED in ONE close_settled record: closed, or not_closed with
// the sentence its change's authors and enqueuer read. A close whose end is
// not known yet is left owed, for the next turn on whichever home holds the
// repository, and is this turn's failure — as is every close this turn
// ended whose settle did not append, each asked again under its landing's
// operation by the next turn, which the tracker answers done.
func (w *queue) closeOwed(ctx context.Context, repoID string, owed []code.QueueWorkEntry,
	at statelog.Position) error {

	var (
		failed  []error
		settled []code.SettledClose
		ended   []code.QueueWorkEntry
	)
	for _, e := range owed {
		if ctx.Err() != nil {
			break
		}
		s := code.SettledClose{EntryID: e.EntryID, Outcome: code.CloseClosed}
		if e.WorkItem == nil {
			// THE CHANGE NAMES NO WORK ITEM ANY MORE, or the repository
			// holds the change no more: nothing is left to close.
			s.Outcome, s.Detail = code.CloseNotClosed, "the change no longer names a work item to close"
		} else {
			err := w.tasks.CloseWorkItem(ctx, TaskClose{OpID: e.CloseOp, RepoID: repoID, Change: e.Change,
				WorkItem: *e.WorkItem, As: e.EnqueuedBy})
			var refused *NotClosed
			switch {
			case errors.As(err, &refused):
				s.Outcome, s.Detail = code.CloseNotClosed, refused.Why
			case err != nil:
				w.log.Warn("code_queue_close_failed", "repo", repoID, "change", e.Change.Number,
					"work_item", e.WorkItem.ID, "err", err,
					"detail", "the close stays owed, and the next turn asks it again")
				failed = append(failed, fmt.Errorf("home: close the work item of change %d: %w",
					e.Change.Number, err))
				continue
			}
		}
		s.Detail = textcut.Within(s.Detail, code.MaxCloseDetail)
		settled, ended = append(settled, s), append(ended, e)
	}
	// ONE RECORD: both callers hand at most code.MaxOwedCloses — a page's
	// owed closes, or one landing's entries (code.MaxLandEntries, the same
	// bound) — and a longer list would be cut at the record's own bound.
	for start := 0; start < len(settled); start += code.MaxOwedCloses {
		end := min(start+code.MaxOwedCloses, len(settled))
		if err := w.settle(ctx, repoID, settled[start:end], ended[start:end], at); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// settle records how the closes of entries ended — at most
// code.MaxOwedCloses, closes[i] entries[i]'s — in one close_settled record,
// which takes each out of every home's queue work. An entry another home's
// settle reached first is skipped by the committer, and a record of none
// still owing is refused stale; one appended and not yet applied is applied
// by the committer, and a turn that reads its closes still owed meanwhile
// asks the same closes again, which the tracker answers done.
func (w *queue) settle(ctx context.Context, repoID string, closes []code.SettledClose,
	entries []code.QueueWorkEntry, at statelog.Position) error {

	// A FRESH OPERATION, never one derived from a landing's: a settle is
	// guarded by the rows it clears, so asked twice the second skips them,
	// and an id minted at a landing's instant would be one the log's ledger
	// cannot vouch for once its retention passes it.
	res, err := w.estate.Queue(ctx, code.QueueProposal{OpID: statelog.NewOpID(w.now(), ""), Seen: at,
		CloseSettled: &code.CloseSettled{RepoID: repoID, Settled: closes}})
	switch {
	case err != nil:
		return fmt.Errorf("home: settle the closes of %d changes' work items: %w", len(closes), err)
	case res.Refusal != "":
		w.log.Debug("code_queue_record_refused", "repo", repoID, "write", "close_settled",
			"closes", len(closes), "refusal", res.Refusal, "detail", res.Detail)
		return nil
	}
	for i, s := range closes {
		if s.Outcome == code.CloseNotClosed {
			w.log.Info("code_queue_close_not_made", "repo", repoID, "change", entries[i].Change.Number,
				"detail", s.Detail)
		}
	}
	return nil
}

// bisect finds which entry of d's first candidate the checks named failed
// on, reading each entry's commit's own attempts from the rows — each the
// batch's merge up to that entry, so a commit's checks judge the entries up
// to it:
//
//   - red is the first commit one of failed FAILED on (the tip, at the
//     latest), good the last one before it every one of failed passed on —
//     the train's base, before the first, where none did: the base is the
//     target's tip, which its queue landed green;
//   - adjacent, red's entry is the culprit, EJECTED check_failed naming the
//     checks that failed on it;
//   - otherwise the commit midway between them is PROBED (purpose bisect).
//
// A probe still running, or one ended in a way only a person's rerun moves,
// waits for the record that ends it.
func (w *queue) bisect(ctx context.Context, repoID string, d code.QueueDue, failed []string, memo *defsMemo,
	at statelog.Position) error {

	c := d.Train[0]
	red := slices.IndexFunc(c.Commits, func(commit code.QueueWorkCommit) bool {
		return slices.ContainsFunc(failed, func(name string) bool {
			held, ok := commit.Check(name)
			return ok && held.State == code.CheckStateFailed
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
		x := code.Ejection{EntryID: culprit.EntryID, Reason: code.EjectCheckFailed}
		for _, name := range failed {
			if held, ok := culprit.Check(name); ok && held.State == code.CheckStateFailed {
				x.Checks = append(x.Checks, name)
			}
		}
		return w.eject(ctx, repoID, fmt.Sprintf("target %s candidate %d at %s", d.TargetRef, c.Ordinal,
			short(culprit.Commit)), endOf(d, roundBisected), []code.Ejection{x}, at)
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
	return w.probe(ctx, repoID, d, c, mid, code.PurposeBisect, s, defs, at)
}

// eject asks the repository's committer to put entries out of their queue —
// which abandons the train each was in — about what it is for: a refusal is
// the rows answering, read again next turn, and only an error of the write
// itself fails the turn. ends, where set, is the train the ejection ENDS and
// how ([endOf]): counted once its first record is appended, and its round
// left untimed ([mergeRate.ended]); nil for entries no train of the target's
// was testing — a chain's skipped merges, a pull mirror's entries.
func (w *queue) eject(ctx context.Context, repoID, about string, ends *trainEnd, entries []code.Ejection,
	at statelog.Position) error {

	for start := 0; start < len(entries); start += code.MaxLandEntries {
		part := entries[start:min(start+code.MaxLandEntries, len(entries))]
		res, err := w.estate.Queue(ctx, code.QueueProposal{OpID: statelog.NewOpID(w.now(), ""), Seen: at,
			Eject: &code.Eject{RepoID: repoID, Entries: part}})
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
func endOf(d code.QueueDue, outcome string) *trainEnd {
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

// cancelAbandoned ends each of attempts — an attempt its queue queued, of a
// train it has since given up (code.AbandonedCheck) — cancelled for its
// reason, abandoned or dequeued, one check write an attempt, as the check work
// ends a head's its change moved on from (checks.go's cancel): the commit it
// measures is one nothing will land, and a runner claims oldest first, so
// left alone it holds a cell — a Gitlet node's container or the company's e2b
// box time — ahead of the live train's and its bisect's. A box running one
// stops once its row has ended. A refusal is the rows answering — the attempt
// ended meanwhile — and only a write that decided nothing fails the turn, the
// repository then listed again; every attempt is asked either way.
func (w *queue) cancelAbandoned(ctx context.Context, repoID string, attempts []code.AbandonedCheck,
	at statelog.Position) error {

	var failed []error
	for _, x := range attempts {
		summary := "its merge queue gave up the train this candidate was in"
		if x.Reason == code.CheckReasonDequeued {
			summary = "a change this candidate merged left its merge queue"
		}
		if err := w.checks.write(ctx, fmt.Sprintf("target %s candidate %s at %s", x.Target, x.CandidateID,
			short(x.OID)), code.ChecksRequest{OpID: statelog.NewOpID(w.now(), ""), Seen: at,
			Finished: &code.CheckFinished{CheckKey: x.CheckKey, State: code.CheckStateCancelled, Reason: x.Reason,
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
func commitStates(commit code.QueueWorkCommit, names []string) landingStates {
	return statesOf(commit.Check, names)
}
