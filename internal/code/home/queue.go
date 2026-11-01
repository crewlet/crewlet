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
type Tasks interface {
	CloseWorkItem(ctx context.Context, c TaskClose) error
}

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

// queue is the home's MERGE QUEUE (M5-D12, M5-D13): for each pre_land target
// of a repository holding live entries, from ONE snapshot of its queue work
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
//     run for each that asked its work item closed (Tasks);
//   - the train's FIRST candidate red: its batch BISECTED over its entries'
//     own commits, each the batch's merge up to that entry — probed midway
//     between the last commit known good (the train's base, the target's tip,
//     before any) and the first known red, the failed checks queued there
//     (purpose bisect) — down to the culprit, EJECTED check_failed naming the
//     checks; the eject abandons the train, and the next turn builds again
//     without it.
//
// A post_land target is no train's: its queue lands no candidate.
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
	node   string
	log    *slog.Logger
	now    func() time.Time
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
func (w *queue) Work(ctx context.Context, repoID string, floor statelog.Position) error {
	q := code.ReadQuery{Kind: code.ReadQueueDue, Fresh: freshAt(floor), QueueDue: &code.QueueDueQuery{RepoID: repoID}}
	memo := newDefsMemo()
	var failed []error
	for {
		a, err := w.estate.Read(ctx, q)
		if err != nil {
			return errors.Join(append(failed, fmt.Errorf("home: read repository %s's queue work: %w", repoID,
				err))...)
		}
		if a.DueQueue != nil {
			if err := w.target(ctx, repoID, *a.DueQueue, memo, a.Position); err != nil {
				failed = append(failed, err)
			}
		}
		if a.Next == "" || ctx.Err() != nil {
			return errors.Join(append(failed, ctx.Err())...)
		}
		q.Cursor = a.Next
	}
}

// target is one target's next step, from the snapshot d was read in at.
func (w *queue) target(ctx context.Context, repoID string, d code.QueueDue, memo *defsMemo,
	at statelog.Position) error {

	switch {
	case d.Policy != code.LandingPreLand || len(d.Required) == 0 || len(d.Entries) == 0:
		return nil
	case len(d.Train) > 0 && trainStands(d):
		return w.test(ctx, repoID, d, memo, at)
	}
	return w.build(ctx, repoID, d, at)
}

// trainStands reports d's train still the one to test: built onto the
// target's tip, every entry it tests live in it.
func trainStands(d code.QueueDue) bool {
	if d.Train[0].BaseOID != d.Tip {
		return false
	}
	train := d.Train[0].Train
	for _, c := range d.Train {
		for _, commit := range c.Commits {
			i := slices.IndexFunc(d.Entries, func(e code.QueueWorkEntry) bool { return e.EntryID == commit.EntryID })
			if i < 0 || d.Entries[i].Train != train || d.Entries[i].State != code.QueueTesting {
				return false
			}
		}
	}
	return true
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
// records it, ejecting the entries the chain skipped.
func (w *queue) build(ctx context.Context, repoID string, d code.QueueDue, at statelog.Position) error {
	about := fmt.Sprintf("target %s at %s", d.TargetRef, short(d.Tip))
	if d.Tip == code.ZeroOID {
		// NO TIP TO BUILD ONTO: the branch was deleted under its queue; a
		// person's push creating it again gives its train a base.
		w.log.Debug("code_queue_no_target", "repo", repoID, "about", about)
		return nil
	}
	waiting := len(d.Entries)
	if d.More {
		waiting = code.MaxCandidateEntries + 1
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
		return fmt.Errorf("home: build the train of %s: %w", about, err)
	case built.Refusal != "":
		return fmt.Errorf("home: build the train of %s: %s: %s", about, built.Refusal, built.Detail)
	}
	applied := false
	defer func() {
		if err := built.Settle(context.WithoutCancel(ctx), applied); err != nil {
			w.log.Warn("code_queue_chain_not_settled", "repo", repoID, "about", about, "err", err)
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
		case c.Skipped == code.WriteConflict || c.Skipped == code.WriteTooLarge:
			// A MERGE THAT CANNOT BE BUILT is its entry's conflict: one
			// past its own build's budget is as unbuildable as one git
			// marks, and named with no paths.
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
		if err := w.eject(ctx, repoID, about, ejected, at); err != nil {
			return err
		}
	}
	if len(kept) == 0 {
		return nil
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
	res, err := w.estate.Queue(ctx, code.QueueProposal{OpID: opID, Seen: at, Candidate: &cand})
	if err != nil {
		return fmt.Errorf("home: record the train of %s: %w", about, err)
	}
	applied = res.Outcome == statelog.OutcomeApplied
	if res.Refusal != "" {
		w.log.Debug("code_queue_record_refused", "repo", repoID, "about", about, "write", "candidate",
			"refusal", res.Refusal, "detail", res.Detail)
	}
	return nil
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
	return nil, w.eject(ctx, repoID, about, ejected, at)
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
		BaseOID: prefix[0].BaseOID, TipOID: tip, Policy: code.LandingPreLand, Train: prefix[0].Train,
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
	res, err := w.estate.Queue(ctx, code.QueueProposal{OpID: opID, Seen: at, Land: &l})
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
			return w.eject(ctx, repoID, about, ejected, at)
		}
		return nil
	case res.Outcome != statelog.OutcomeApplied:
		// NOT YET APPLIED HERE: its changes are not merged on this node's
		// rows, so no work item of theirs is closed on its word.
		w.log.Warn("code_queue_close_not_run", "repo", repoID, "about", about, "outcome", res.Outcome,
			"detail", "the landing is appended and not applied here, so its work items are not closed")
		return nil
	}
	var failed []error
	for _, e := range landed {
		if !e.CloseTask || e.WorkItem == nil {
			continue
		}
		if err := w.tasks.CloseWorkItem(ctx, TaskClose{OpID: statelog.StepOpID(opID, "close-work-item",
			e.EntryID), RepoID: repoID, Change: e.Change, WorkItem: *e.WorkItem, As: e.EnqueuedBy}); err != nil {
			w.log.Warn("code_queue_close_failed", "repo", repoID, "about", about, "change", e.Change.Number,
				"work_item", e.WorkItem.ID, "err", err)
			failed = append(failed, fmt.Errorf("home: close the work item of change %d landed on %s: %w",
				e.Change.Number, d.TargetRef, err))
		}
	}
	return errors.Join(failed...)
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
			short(culprit.Commit)), []code.Ejection{x}, at)
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
// itself fails the turn.
func (w *queue) eject(ctx context.Context, repoID, about string, entries []code.Ejection,
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
		}
	}
	return nil
}

// commitStates is where each of names stands on commit by its latest
// attempt ([landingStates]).
func commitStates(commit code.QueueWorkCommit, names []string) landingStates {
	return statesOf(commit.Check, names)
}
