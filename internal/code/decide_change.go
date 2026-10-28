package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A CHANGE'S LIFECYCLE, DECIDED: an open, an edit of what it says of itself, a
// draft's state, a retarget, a close and a reopen are each a [ChangeProposal]
// its asker hands the repository's committer ([Committer.ProposeChange]),
// which decides it in ONE snapshot, alone, as a record of its own op
// ([ChangeOps]) on the REPOSITORY's subject — the subject every push that
// moves the change's branches is on too, so the broker arbitrates a change's
// lifecycle and its pushes together, and a change is never both merged and
// closed. Its number is no decision's: the apply that opens it reads and
// advances the repository's next_change, so numbers follow the log's order.
//
// # What the decide holds a write to
//
// Against the rows of one snapshot, in this order; the first rule that fails
// is the answer, and none of them writes anything.
//
// Every write first meets its repository: absent, deleted or being purged is
// repo_deleted, in the sentence of the state it is in ([RepoGone]); archived
// is repo_archived, since an archived repository is read-only and its changes
// are read and written nothing.
//
// AN OPEN then meets:
//
//  1. its asker's grant on the repository taking a fast-forward of the
//     source branch ([Grant.RefRule]) — else not_permitted, in the grant's
//     own sentence: a change proposes what its source holds, so who may open
//     one from a branch is who may push to it — a seat's own namespace for a
//     full_access seat outside the owning unit, any branch for one that
//     writes, and every branch for a person;
//  2. a source that is not its target — else same_ref;
//  3. a source and a target that are live branches — else no_source, then
//     no_target: a change is about branches that exist;
//  4. heads that differ — else no_difference, since a source at its target's
//     commit holds nothing to merge;
//  5. no live change already on the source — else change_exists, naming it
//     ([ChangeRef]): a branch heads ONE live change at a time, the invariant
//     the push apply's revisions rest on, which every write that makes a
//     change live — an open and a reopen — holds.
//
// EVERY OTHER WRITE meets:
//
//  1. its change, on this repository — else change_not_found;
//  2. its asker being the change's owner, a principal that maintains the
//     repository ([AccessMaintain]) or a person — else not_permitted
//     ([mayWrite]): a change's lifecycle is its author's to steer, its
//     repository's maintainers' to keep in order, and every person's;
//  3. the version it names being the change's ([ChangeProposal.ExpectedVersion])
//     — else stale: another write landed first, a push that moved the
//     change's head among them, and the asker reads the change again;
//  4. the op's own rule: an edit, a draft's state and a retarget take a LIVE
//     change — else change_closed or change_merged — and a retarget's target
//     is a live branch that is not its source (no_target, same_ref); a close
//     refuses a merged change (change_merged); a reopen refuses a merged one
//     too, and takes a closed change's source only where it is a live branch
//     (no_source) that heads no other live change (change_exists).
//
// A write that asks for what the change already says — an edit to its own
// values, a draft's state it is in, the target it has, a close of a closed
// change, a reopen of a live one — is ANSWERED UNCHANGED from the change's row,
// with no record: nothing moves, so nothing is appended, and a write asked
// again after its answer was lost, under a new operation id, lands on what the
// first did rather than refused.
//
// # A seat's write is decided only by a node that can judge it
//
// Every rule that judges a SEAT reads the chart of the deciding node's epoch
// ([SeatRules]), as a seat's push does: a node with no chart yet, or one
// confirmed behind the company's configuration, decides none of a seat's
// writes and says so about itself ([ErrNotServing]), so the router asks the
// repository's runner-up under the same operation id. A person's and a token's
// are decided as ever, since no rule of theirs reads the chart.

// Validate refuses a proposal no committer could decide ([ErrInvalidProposal],
// naming the field), before it is queued: anything but exactly one of the six
// records; an operation id this engine does not accept
// (statelog.CheckCallerOpID); an asker that is no principal, or that names a
// run; a Seen that is no position on the code log; a repository id that is no
// UUID in its canonical spelling; a change id that is no bare UUIDv7 — and an
// open's that is not [OpenChangeID] of its operation id; an open that names a
// version, or any other write that names none; a branch that is no branch a
// change may name ([branchProblem]); a title, a body, a close's reason, a work
// item, a turn or a run its bounds refuse; an edit that sets nothing, or sets
// a work item and clears it; a field the committer stamps — who wrote it, an
// open's or a reopen's head — set by the asker; and a record past what one
// proposal may carry ([EntryRoom]). What the repository and the change hold is
// the decide's to judge, from the rows.
func (p ChangeProposal) Validate() error {
	_, err := p.measure()
	return err
}

// measure validates p and answers its queued byte charge: its record's
// payload as the committer stamps it — its asker as who wrote it, and a head
// at an id's whole length — encoded, held under [EntryRoom].
func (p ChangeProposal) measure() (int, error) {
	if err := p.validFields(); err != nil {
		return 0, err
	}
	op, payload := p.stamped(strings.Repeat("f", 40))
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: encode it: %w", ErrInvalidProposal, op, err)
	}
	if len(encoded) > EntryRoom {
		return 0, fmt.Errorf("%w: %s: %d bytes, past the %d one proposal may take of its record",
			ErrInvalidProposal, op, len(encoded), EntryRoom)
	}
	return len(encoded), nil
}

// record is the op and the payload p carries, as its asker sent it — "" and
// nil for a proposal that carries none, which [ChangeProposal.validFields]
// refuses before anything reads this.
func (p ChangeProposal) record() (OpKind, any) {
	switch {
	case p.Open != nil:
		return OpChangeOpen, *p.Open
	case p.Edit != nil:
		return OpChangeEdit, *p.Edit
	case p.Draft != nil:
		return OpChangeDraft, *p.Draft
	case p.Retarget != nil:
		return OpChangeRetarget, *p.Retarget
	case p.Close != nil:
		return OpChangeClose, *p.Close
	case p.Reopen != nil:
		return OpChangeReopen, *p.Reopen
	}
	return "", nil
}

// stamped is the op and the payload of p's record as the committer forms it:
// its asker as the change's owner or as who wrote it, and head as an open's or
// a reopen's head — the fields no asker sends.
func (p ChangeProposal) stamped(head string) (OpKind, any) {
	op, payload := p.record()
	switch r := payload.(type) {
	case ChangeOpen:
		r.Owner, r.Head = p.Asker, head
		return op, r
	case ChangeEdit:
		r.By = p.Asker
		return op, r
	case ChangeDraft:
		r.By = p.Asker
		return op, r
	case ChangeRetarget:
		r.By = p.Asker
		return op, r
	case ChangeClose:
		r.By = p.Asker
		return op, r
	case ChangeReopen:
		r.By, r.Head = p.Asker, head
		return op, r
	}
	return op, payload
}

// RepoID is the repository p is about, by whichever record it carries — ""
// for one that carries none, which the estate's router refuses as it refuses
// a push naming no repository. It is the ONE answer the router keys p by, the
// committer queues it under and the estate's seam raises the repository's
// floor for, so the three never name different repositories.
func (p ChangeProposal) RepoID() string {
	switch {
	case p.Open != nil:
		return p.Open.RepoID
	case p.Edit != nil:
		return p.Edit.RepoID
	case p.Draft != nil:
		return p.Draft.RepoID
	case p.Retarget != nil:
		return p.Retarget.RepoID
	case p.Close != nil:
		return p.Close.RepoID
	case p.Reopen != nil:
		return p.Reopen.RepoID
	}
	return ""
}

// ChangeID is the change p is about, by whichever record it carries.
func (p ChangeProposal) ChangeID() string {
	switch {
	case p.Open != nil:
		return p.Open.ChangeID
	case p.Edit != nil:
		return p.Edit.ChangeID
	case p.Draft != nil:
		return p.Draft.ChangeID
	case p.Retarget != nil:
		return p.Retarget.ChangeID
	case p.Close != nil:
		return p.Close.ChangeID
	case p.Reopen != nil:
		return p.Reopen.ChangeID
	}
	return ""
}

// decidedRows is the most rows p's record makes its apply touch: an open's
// change, body, first author, first revision and the repository's row; an
// edit's change, body and repository; a draft's state's, a retarget's and a
// close's change and repository; a reopen's change, the revision a moved
// source makes, and the repository.
func (p ChangeProposal) decidedRows() int {
	switch {
	case p.Open != nil:
		return 5
	case p.Edit != nil, p.Reopen != nil:
		return 3
	}
	return 2
}

// validFields is every rule of [ChangeProposal.Validate] but the record's
// bytes.
func (p ChangeProposal) validFields() error {
	set := 0
	for _, isSet := range []bool{p.Open != nil, p.Edit != nil, p.Draft != nil, p.Retarget != nil,
		p.Close != nil, p.Reopen != nil} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%w: a change proposal is exactly one of an open, an edit, a draft's state, a "+
			"retarget, a close and a reopen, and this one sets %d", ErrInvalidProposal, set)
	}
	if err := statelog.CheckCallerOpID(p.OpID); err != nil {
		return badField("op_id", "%v", err)
	}
	switch why := p.Asker.invalid(); {
	case why != "":
		return fmt.Errorf("%w: asker: %s", ErrInvalidProposal, why)
	case !p.Asker.Run.IsZero():
		return badField("asker.run", "a change keeps the turn and the run that opened it in its own fields, "+
			"and its asker names none")
	}
	if !p.Seen.IsZero() {
		switch {
		case p.Seen.Stream != topics.CodeLogStream:
			return badField("seen", "%s is a position on another log than the code log (%s)", p.Seen,
				topics.CodeLogStream)
		case p.Seen.Valid() != nil:
			return fmt.Errorf("%w: seen: %w", ErrInvalidProposal, p.Seen.Valid())
		}
	}
	if err := validRepoID("repo_id", p.RepoID()); err != nil {
		return err
	}
	if err := bareUUIDv7("change_id", p.ChangeID()); err != nil {
		return err
	}
	if p.Open != nil {
		return p.validOpen()
	}
	if p.ExpectedVersion < 1 {
		return badField("expected_version", "%d; a write to a change names the version it was formed against, "+
			"1 at its open", p.ExpectedVersion)
	}
	switch {
	case p.Edit != nil:
		return p.Edit.validFields()
	case p.Draft != nil:
		return stampedBy("draft", p.Draft.By)
	case p.Retarget != nil:
		if why := branchProblem(p.Retarget.TargetRef); why != "" {
			return badField("retarget.target_ref", "%s", why)
		}
		return stampedBy("retarget", p.Retarget.By)
	case p.Close != nil:
		if why := textProblem(p.Close.Reason, MaxCloseReasonBytes, "close's reason"); why != "" {
			return badField("close.reason", "%s", why)
		}
		return stampedBy("close", p.Close.By)
	}
	if p.Reopen.Head != "" {
		return badField("reopen.head", "the committer stamps a reopen's head from its snapshot, and an asker "+
			"sends none")
	}
	return stampedBy("reopen", p.Reopen.By)
}

// validOpen holds an open to [ChangeProposal.Validate]'s rules.
func (p ChangeProposal) validOpen() error {
	o := p.Open
	if want := OpenChangeID(p.OpID); o.ChangeID != want {
		return badField("open.change_id", "%q, and the change an open under %s opens is %s", o.ChangeID, p.OpID,
			want)
	}
	if p.ExpectedVersion != 0 {
		return badField("expected_version", "%d; an open names no version — the change does not exist yet",
			p.ExpectedVersion)
	}
	for _, ref := range []struct{ field, ref string }{
		{"open.source_ref", o.SourceRef}, {"open.target_ref", o.TargetRef},
	} {
		if why := branchProblem(ref.ref); why != "" {
			return badField(ref.field, "%s", why)
		}
	}
	if why := titleProblem(o.Title); why != "" {
		return badField("open.title", "%s", why)
	}
	if why := textProblem(o.Body, MaxChangeBodyBytes, "change's body"); why != "" {
		return badField("open.body", "%s", why)
	}
	if o.WorkItem != nil {
		if why := workItemProblem(*o.WorkItem); why != "" {
			return badField("open.work_item", "%s", why)
		}
	}
	for _, label := range []struct{ field, value string }{
		{"open.turn_id", o.TurnID}, {"open.run_id", o.RunID},
	} {
		if why := labelProblem(label.value); why != "" {
			return badField(label.field, "%s", why)
		}
	}
	switch {
	case o.Head != "":
		return badField("open.head", "the committer stamps an open's head from its snapshot, and an asker sends none")
	case o.Owner != (Principal{}):
		return badField("open.owner", "the committer stamps the change's owner from the proposal's asker, and an "+
			"asker sends none")
	}
	return nil
}

// validFields holds an edit to [ChangeProposal.Validate]'s rules.
func (e *ChangeEdit) validFields() error {
	switch {
	case e.Title == nil && e.Body == nil && e.WorkItem == nil && !e.ClearWorkItem:
		return badField("edit", "an edit sets a title, a body or a work item, and this one sets none")
	case e.WorkItem != nil && e.ClearWorkItem:
		return badField("edit.clear_work_item", "an edit sets a work item or clears it, never both")
	}
	if e.Title != nil {
		if why := titleProblem(*e.Title); why != "" {
			return badField("edit.title", "%s", why)
		}
	}
	if e.Body != nil {
		if why := textProblem(*e.Body, MaxChangeBodyBytes, "change's body"); why != "" {
			return badField("edit.body", "%s", why)
		}
	}
	if e.WorkItem != nil {
		if why := workItemProblem(*e.WorkItem); why != "" {
			return badField("edit.work_item", "%s", why)
		}
	}
	return stampedBy("edit", e.By)
}

// stampedBy refuses a record's By an asker set: the node that decides the
// write stamps it from the write's asker — the committer a change's lifecycle,
// the change writer its conversation.
func stampedBy(what string, by Principal) error {
	if by != (Principal{}) {
		return badField(what+".by", "the deciding node stamps who wrote it from the write's asker, and an asker "+
			"sends none")
	}
	return nil
}

// again is what a proposal's asker does after a refusal that decided nothing.
const changeAgain = "send the write again"

// ProposeChange decides one write of a change's lifecycle as a batch of its own
// in the repository's queue and answers what became of it — the push's queue
// and bounds ([Committer.Propose]), as a compaction's are
// ([Committer.ProposeCompaction]): a full queue or a node holding its bytes is
// busy at once, a write whose caller's deadline leaves too little for a batch
// and its answer is busy undecided, a stopping committer answers
// [ErrNotServing] — as does one that cannot judge a seat's write, behind the
// company's configuration or holding no chart yet — and a proposal that is not
// one answers [ErrInvalidProposal]. ctx bounds only this caller's wait.
//
// ITS OPERATION ID IS THE PROPOSAL'S OWN, never one minted for the batch, so a
// write its asker hands the repository's runner-up after this node went
// silent is the same write there, collapsed by the ledger onto the record that
// landed — an open onto the change it opened, never a second. ITS SESSION IS
// ITS SEEN. Applied is answered FROM THE CHANGE'S ROW the record wrote, never
// from the decision: its number is the apply's to give.
func (c *Committer) ProposeChange(ctx context.Context, p ChangeProposal) (ChangeResult, error) {
	bytes, err := p.measure()
	if err != nil {
		c.countChange(p, ChangeResult{}, err)
		return ChangeResult{}, err
	}
	deadline, _ := ctx.Deadline()
	a := c.queue(ctx, &queued{change: &p, bytes: bytes, rows: p.decidedRows(), arrived: c.clock(),
		deadline: deadline, gone: ctx.Done(), answer: make(chan answered, 1)})
	c.countChange(p, a.change, a.err)
	return a.change, a.err
}

// countChange counts one proposal's answer under its op and what became of it
// ([changeOutcome]).
func (c *Committer) countChange(p ChangeProposal, res ChangeResult, err error) {
	if c.metrics == nil {
		return
	}
	op, _ := p.record()
	c.metrics.Add(metrics.CodeChangeProposals, 1, metrics.Attrs{"op": string(op), "outcome": changeOutcome(res, err)})
}

// changeOutcome is what became of a change proposal, as
// crewlet.code.change.proposals names it: `handed_on` for one this node
// decided nothing of about itself, which the router asks the repository's
// runner-up; `failed` for any other error; the refusal's own code; `unchanged`
// for one answered from the row with no record; and otherwise its outcome —
// applied, pending or unknown.
func changeOutcome(res ChangeResult, err error) string {
	switch {
	case errors.Is(err, ErrNotServing):
		return "handed_on"
	case err != nil:
		return "failed"
	case res.Refusal != "":
		return string(res.Refusal)
	case res.Unchanged:
		return "unchanged"
	}
	return string(res.Outcome)
}

// changeDecision is one round's decision of a change proposal: its refusal and
// sentence where it is refused; the change as the snapshot holds it where it
// is answered unchanged; the record's payload, stamped, where it forms one;
// and the version the repository's row was read at either way.
type changeDecision struct {
	refusal   ChangeRefusal
	detail    string
	unchanged *changeRow
	payload   any
	version   int64
}

// refuse is d refused under code with detail.
func (d changeDecision) refuse(code ChangeRefusal, detail string) (changeDecision, error) {
	d.refusal, d.detail = code, detail
	return d, nil
}

// decideChange is one round's decision of p over the rows tx holds — see this
// file's head for the rules, in their order. Pure over (the rows, p, seats,
// retention): seats is the deciding epoch's chart and calendar, which a seat's
// write is judged by and which a write by anybody else may leave unset;
// retention is code.deleted_retention_days on the deciding epoch, which a
// repo_deleted refusal names the purge's earliest instant by.
func decideChange(ctx context.Context, tx *sql.Tx, p ChangeProposal, seats SeatRules,
	retention time.Duration) (changeDecision, error) {

	repoID := p.RepoID()
	repo, found, err := pushRepoIn(ctx, tx, repoID)
	if err != nil {
		return changeDecision{}, err
	}
	d := changeDecision{version: repo.version}
	switch {
	case !found || repo.status == RepoStatusDeleted || repo.status == RepoStatusPurging:
		detail, err := goneDetail(ctx, tx, repoID, repo, found, retention)
		if err != nil {
			return changeDecision{}, err
		}
		return d.refuse(ChangeRefusalRepoDeleted, detail)
	case repo.status == RepoStatusArchived:
		return d.refuse(ChangeRefusalRepoArchived, fmt.Sprintf("repository %s is archived and read-only "+
			"(code.repos archived: true): its changes are read, and written nothing", repo.name))
	}
	if p.Asker.Kind.readsChart() && seats.Roster == nil {
		// THE CALLER'S BUG: the committer hands a seat's write on to the
		// repository's next committer before deciding it with no chart.
		return changeDecision{}, fmt.Errorf("code: a seat's change to %s reached the decide with no chart to "+
			"judge it by — the committer hands such a write to the repository's next committer", repoID)
	}
	grant := GrantOf(p.Asker, repo.grantRef(repoID), seats.Roster)
	if p.Open != nil {
		return decideOpen(ctx, tx, p, repo, grant, d)
	}
	row, found, err := changeRowIn(ctx, tx, p.ChangeID())
	if err != nil {
		return changeDecision{}, err
	}
	if !found || row.repoID != repoID {
		return d.refuse(ChangeRefusalChangeNotFound, fmt.Sprintf("repository %s holds no change %s", repo.name,
			p.ChangeID()))
	}
	named := ChangeRef(repo.name, row.number)
	if why := mayWrite(p.Asker, row, grant, named); why != "" {
		return d.refuse(ChangeRefusalNotPermitted, why)
	}
	if p.ExpectedVersion != row.version {
		return d.refuse(ChangeRefusalStale, fmt.Sprintf("%s is at version %d, and this write was formed "+
			"against version %d: another write landed first — read it again", named, row.version,
			p.ExpectedVersion))
	}
	switch {
	case p.Edit != nil:
		return decideEdit(ctx, tx, p, row, named, d)
	case p.Draft != nil:
		if why, code := notLive(row, named); code != "" {
			return d.refuse(code, why)
		}
		if (row.state == ChangeStateDraft) == p.Draft.Draft {
			d.unchanged = &row
			return d, nil
		}
	case p.Retarget != nil:
		return decideRetarget(ctx, tx, p, repo, row, named, d)
	case p.Close != nil:
		switch row.state {
		case ChangeStateMerged:
			return d.refuse(ChangeRefusalChangeMerged, fmt.Sprintf("%s is merged and stays merged", named))
		case ChangeStateClosed:
			d.unchanged = &row
			return d, nil
		}
	default:
		return decideReopen(ctx, tx, p, repo, row, named, d)
	}
	_, d.payload = p.stamped("")
	return d, nil
}

// decideOpen is an open's rules 1–5 over the repository's row as repo holds it
// and the rows tx holds, its asker's grant g.
func decideOpen(ctx context.Context, tx *sql.Tx, p ChangeProposal, repo pushRepo, g Grant,
	d changeDecision) (changeDecision, error) {

	o := p.Open
	if r, why := g.RefRule(fastForward(o.SourceRef)); r != "" {
		return d.refuse(ChangeRefusalNotPermitted, "a change is opened by whoever may push its source branch, "+
			"and "+why)
	}
	if o.SourceRef == o.TargetRef {
		return d.refuse(ChangeRefusalSameRef, fmt.Sprintf("%s is both the change's source and its target: a "+
			"change proposes one branch into another", o.SourceRef))
	}
	heads, err := refsNamed(ctx, tx, p.RepoID(), []string{o.SourceRef, o.TargetRef})
	if err != nil {
		return changeDecision{}, fmt.Errorf("code: read the branches change %s names: %w", o.ChangeID, err)
	}
	source, target := heads[o.SourceRef], heads[o.TargetRef]
	switch {
	case source == "":
		return d.refuse(ChangeRefusalNoSource, fmt.Sprintf("%s holds no live %s: push the branch first",
			repo.name, o.SourceRef))
	case target == "":
		return d.refuse(ChangeRefusalNoTarget, fmt.Sprintf("%s holds no live %s to merge into", repo.name,
			o.TargetRef))
	case source == target:
		return d.refuse(ChangeRefusalNoDifference, fmt.Sprintf("%s and %s are both at %s: the source holds "+
			"nothing its target does not", o.SourceRef, o.TargetRef, short(source)))
	}
	if live, found, err := liveChangeOn(ctx, tx, p.RepoID(), o.SourceRef); err != nil {
		return changeDecision{}, err
	} else if found {
		return d.refuse(ChangeRefusalChangeExists, fmt.Sprintf("%s already heads %s, and a branch heads one "+
			"live change at a time: write to that change rather than open another",
			o.SourceRef, ChangeRef(repo.name, live)))
	}
	_, d.payload = p.stamped(source)
	return d, nil
}

// decideEdit is an edit's rule over row — a live change — and its answer
// unchanged where every field it sets already holds what it sets.
func decideEdit(ctx context.Context, tx *sql.Tx, p ChangeProposal, row changeRow, named string,
	d changeDecision) (changeDecision, error) {

	if why, code := notLive(row, named); code != "" {
		return d.refuse(code, why)
	}
	e := p.Edit
	changed := (e.Title != nil && *e.Title != row.title) ||
		(e.WorkItem != nil && *e.WorkItem != row.work) ||
		(e.ClearWorkItem && row.work != types.WorkItem{})
	if !changed && e.Body != nil {
		// THE BODY IS READ ONLY WHERE ITS LENGTH CANNOT TELL: a body of
		// another length is another body, and one of none holds nothing.
		switch {
		case int64(len(*e.Body)) != row.bodyBytes:
			changed = true
		case row.bodyBytes > 0:
			body, err := bodyOf(ctx, tx, row.id)
			if err != nil {
				return changeDecision{}, err
			}
			changed = body != *e.Body
		}
	}
	if !changed {
		d.unchanged = &row
		return d, nil
	}
	_, d.payload = p.stamped("")
	return d, nil
}

// decideRetarget is a retarget's rule over row: a live change, and a target
// that is a live branch of the repository other than its source; unchanged
// for the target it has.
func decideRetarget(ctx context.Context, tx *sql.Tx, p ChangeProposal, repo pushRepo, row changeRow,
	named string, d changeDecision) (changeDecision, error) {

	if why, code := notLive(row, named); code != "" {
		return d.refuse(code, why)
	}
	target := p.Retarget.TargetRef
	switch target {
	case row.target:
		d.unchanged = &row
		return d, nil
	case row.source:
		return d.refuse(ChangeRefusalSameRef, fmt.Sprintf("%s is %s's source: a change proposes one branch "+
			"into another", target, named))
	}
	heads, err := refsNamed(ctx, tx, p.RepoID(), []string{target})
	if err != nil {
		return changeDecision{}, fmt.Errorf("code: read the target %s retargets onto: %w", named, err)
	}
	if heads[target] == "" {
		return d.refuse(ChangeRefusalNoTarget, fmt.Sprintf("%s holds no live %s to merge into", repo.name,
			target))
	}
	_, d.payload = p.stamped("")
	return d, nil
}

// decideReopen is a reopen's rule over row: a closed change whose source is a
// live branch heading no other live change — at that branch's head, which
// the record carries; unchanged for a live one.
func decideReopen(ctx context.Context, tx *sql.Tx, p ChangeProposal, repo pushRepo, row changeRow,
	named string, d changeDecision) (changeDecision, error) {

	switch row.state {
	case ChangeStateMerged:
		return d.refuse(ChangeRefusalChangeMerged, fmt.Sprintf("%s is merged and stays merged", named))
	case ChangeStateDraft, ChangeStateOpen:
		d.unchanged = &row
		return d, nil
	}
	heads, err := refsNamed(ctx, tx, p.RepoID(), []string{row.source})
	if err != nil {
		return changeDecision{}, fmt.Errorf("code: read the source %s reopens on: %w", named, err)
	}
	head := heads[row.source]
	if head == "" {
		return d.refuse(ChangeRefusalNoSource, fmt.Sprintf("%s holds no live %s, %s's source: push the "+
			"branch again before reopening it", repo.name, row.source, named))
	}
	if live, found, err := liveChangeOn(ctx, tx, p.RepoID(), row.source); err != nil {
		return changeDecision{}, err
	} else if found {
		return d.refuse(ChangeRefusalChangeExists, fmt.Sprintf("%s heads %s now, and a branch heads one live "+
			"change at a time: close that one first, or write to it", row.source, ChangeRef(repo.name, live)))
	}
	_, d.payload = p.stamped(head)
	return d, nil
}

// notLive is why row is no change a write that takes a live one may write,
// and its code, or "" for a live change.
func notLive(row changeRow, named string) (string, ChangeRefusal) {
	switch row.state {
	case ChangeStateClosed:
		return fmt.Sprintf("%s is closed: reopen it first", named), ChangeRefusalChangeClosed
	case ChangeStateMerged:
		return fmt.Sprintf("%s is merged and stays merged", named), ChangeRefusalChangeMerged
	}
	return "", ""
}

// mayWrite is why asker may not write row's lifecycle beyond its open, or ""
// where it may (rule D21): the change's OWNER — a seat by its handle, a person
// by the human seat they hold — a principal that MAINTAINS the repository on
// its grant g, and every PERSON, whom a grant puts at maintain anyway. A token
// bound to nobody reads every repository and writes no change; a seat that
// neither owns the change nor maintains the repository is told what it holds
// and who may.
func mayWrite(asker Principal, row changeRow, g Grant, named string) string {
	handle, kind := asker.party()
	switch {
	case kind == PartyPerson:
		return ""
	case kind != "" && handle == row.owner && kind == row.ownerKind:
		return ""
	case g.Access >= AccessMaintain:
		return ""
	case kind == "":
		return "this credential is bound to no seat: a change is written by a person, through a credential " +
			"bound to their human seat, or by a seat; a credential bound to nobody reads only"
	case g.none != "":
		return g.none
	}
	return fmt.Sprintf("seat %q holds %s on this repository (tier %s, relation %s) and did not open %s: a "+
		"change's lifecycle is its owner's, a maintainer's — %s — or a person's", handle, g.Access,
		g.Tier.spelled(), g.Relation, named, g.lead())
}

// fastForward is a fast-forward of ref, as [Grant.RefRule] judges one: the
// command an open's asker must be able to push to open a change from ref.
func fastForward(ref string) Command {
	return Command{Ref: ref, Old: strings.Repeat("1", 40), New: strings.Repeat("2", 40)}
}

// withinChange refuses a decision past a record's bytes or rows, which
// [ChangeProposal.Validate]'s bounds keep from ever forming — the check makes
// the invariant the record's own, as [withinRecord] does a push batch's.
func withinChange(d statelog.Decision, p ChangeProposal) error {
	switch rows := p.decidedRows(); {
	case len(d.Payload) > MaxRecordBytes:
		return fmt.Errorf("%w: %d bytes, past MaxRecordBytes (%d)", ErrRecordCap, len(d.Payload), MaxRecordBytes)
	case rows > MaxRecordRows:
		return fmt.Errorf("%w: up to %d rows, past MaxRecordRows (%d)", ErrRecordCap, rows, MaxRecordRows)
	}
	return nil
}

// change publishes one write of a change's lifecycle and answers it — the
// write's batch of its own, under the committer's own lifetime and one
// CommitBudget, as a compaction's ([Committer.compact]).
//
// A SEAT'S IS DECIDED WITH THE EPOCH'S CHART, read once, and on a node that
// holds none yet it is handed on, decided by nobody, as a seat's push is
// ([Committer.commit]); its posture was judged as it was queued and again as
// its batch formed ([Committer.admit]).
func (c *Committer) change(repoID string, q *queued) {
	p := *q.change
	var seats SeatRules
	if q.judgedByChart() {
		var held bool
		if seats, held = c.seatRules(); !held {
			q.answer <- answered{err: fmt.Errorf("%w: this node holds no chart yet to judge a seat's change "+
				"by; the repository's next committer decides it", ErrNotServing)}
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.life, CommitBudget)
	defer cancel()
	op, _ := p.record()
	retention := c.retention()
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	var (
		last    changeDecision
		decided bool
	)
	res, err := c.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: p.OpID, Pattern: statelog.PatternArbitrated, Session: p.Seen,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// RESET AT THE TOP OF EVERY ROUND: only the last round says
			// what the publish decided.
			last, decided = changeDecision{}, false
			d, err := decideChange(ctx, tx, p, seats, retention)
			if err != nil {
				return statelog.Decision{}, err
			}
			last, decided = d, true
			if d.payload == nil {
				return statelog.Decision{Version: d.version}, nil
			}
			decision, err := recordFor(stamp, subject, op, scope, p.OpID, Actor{ID: c.nodeID, Kind: ActorSystem},
				d.payload, c.now())
			if err != nil {
				return statelog.Decision{}, err
			}
			if err := withinChange(decision, p); err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = d.version
			return decision, nil
		},
	})
	out, answerErr := c.changeAnswer(ctx, p, last, decided, res, err)
	q.answer <- answered{change: out, err: answerErr}
}

// changeAnswer is a change proposal's answer from the publish's result and the
// last round's decision.
func (c *Committer) changeAnswer(ctx context.Context, p ChangeProposal, last changeDecision, decided bool,
	res statelog.Result, err error) (ChangeResult, error) {

	if err != nil {
		return c.refusedChange(ctx, p, err)
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		if !res.Position.IsZero() {
			return c.appliedChange(ctx, p, res.Position)
		}
		// NOTHING WAS APPENDED: the decide refused the write or found it
		// unchanged, and its last round says which. A publish that answered
		// with no record and no decision broke the publisher's contract,
		// and says so rather than answering from nothing.
		switch {
		case decided && last.refusal != "":
			return ChangeResult{ChangeID: p.ChangeID(), Refusal: last.refusal, Detail: last.detail}, nil
		case decided && last.unchanged != nil:
			row := last.unchanged
			return ChangeResult{Outcome: statelog.OutcomeApplied, ChangeID: row.id, Number: row.number,
				Version: row.version, Position: unpacked(row.position), Unchanged: true}, nil
		}
		return ChangeResult{}, fmt.Errorf("code: the publish of operation %s answered applied with no record, "+
			"and its decide neither refused nor found it unchanged", p.OpID)
	case statelog.OutcomePending:
		return ChangeResult{Outcome: statelog.OutcomePending, ChangeID: p.ChangeID(), Position: res.Position}, nil
	}
	return ChangeResult{Outcome: statelog.OutcomeUnknown, ChangeID: p.ChangeID()}, nil
}

// appliedChange answers a write whose record applied at at, FROM THE CHANGE'S
// ROW: its number — an open's, which only the apply gave — and its version
// where its row's last write is this record, and none where a later record
// has moved the change since. A change with no row is a record that applied
// and wrote what this node does not hold: no committer of this domain forms
// that, so it is logged at ERROR and answered unknown — never applied, which
// would hand its asker a change nobody can read.
func (c *Committer) appliedChange(ctx context.Context, p ChangeProposal, at statelog.Position) (ChangeResult,
	error) {

	var (
		row   changeRow
		found bool
	)
	if err := c.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		row, found, err = changeRowIn(ctx, tx, p.ChangeID())
		return err
	}); err != nil {
		failed := fmt.Errorf("code: the record of operation %s applied at %s and its change's row could not be "+
			"read: %w", p.OpID, at, err)
		c.log.WarnContext(ctx, "code_commit_failed", "kind", kindChange, "repo", p.RepoID(), "op_id", p.OpID,
			"error", failed)
		return ChangeResult{}, failed
	}
	if !found || row.repoID != p.RepoID() {
		c.log.ErrorContext(ctx, "code_change_unanswerable", "repo", p.RepoID(), "change", p.ChangeID(),
			"op_id", p.OpID, "position", at.String())
		return ChangeResult{Outcome: statelog.OutcomeUnknown, ChangeID: p.ChangeID(), Detail: fmt.Sprintf("the "+
			"write's record applied at %s and the change it names is not in the rows this node holds", at)}, nil
	}
	out := ChangeResult{Outcome: statelog.OutcomeApplied, ChangeID: row.id, Number: row.number, Position: at}
	if row.position == at.Packed() {
		out.Version = row.version
	}
	return out, nil
}

// refusedChange answers a change proposal whose publish failed — the push's
// mapping ([Committer.refused]) in a change's words, as a compaction's is
// ([Committer.refusedCompaction]).
func (c *Committer) refusedChange(ctx context.Context, p ChangeProposal, err error) (ChangeResult, error) {
	var refusal *statelog.Unavailable
	switch {
	case errors.Is(err, ErrInvalidProposal):
		c.log.ErrorContext(ctx, "code_change_invalid", "repo", p.RepoID(), "op_id", p.OpID, "error", err)
		return ChangeResult{}, err
	case errors.Is(err, statelog.ErrConflict):
		c.countBusy(kindChange, 1)
		return ChangeResult{ChangeID: p.ChangeID(), Refusal: ChangeRefusalBusy,
			Detail: "the repository kept changing for every round the write may take; " + changeAgain}, nil
	case errors.As(err, &refusal) && refusal.Reason == statelog.ReasonDeleted:
		// A FACT ABOUT THE REPOSITORY: its deletion marker drops every
		// record about it, and the position is named only where the marker
		// dropped THIS record.
		var at statelog.Position
		if refusal.Landed() {
			at = refusal.Position
		}
		return ChangeResult{ChangeID: p.ChangeID(), Refusal: ChangeRefusalRepoDeleted, Position: at,
			Detail: c.purgedDetail(ctx, p.RepoID(), p.OpID)}, nil
	case errors.As(err, &refusal) && refusal.Landed():
		return ChangeResult{ChangeID: p.ChangeID(), Refusal: ChangeRefusalNotApplied, Position: refusal.Position,
			Detail: fmt.Sprintf("the write applies nowhere: its record at %s was durable and the code log's %s "+
				"gate drops it on every node; %s", refusal.Position, refusal.Reason, changeAgain)}, nil
	case errors.As(err, &refusal) && nodeRefusals[refusal.Reason]:
		return ChangeResult{}, fmt.Errorf("%w: %w", ErrNotServing, refusal)
	case errors.As(err, &refusal):
		return ChangeResult{ChangeID: p.ChangeID(), Refusal: ChangeRefusalUnavailable,
			Detail: "the code log refused the write: " + strings.TrimPrefix(refusal.Error(), "statelog: ")}, nil
	}
	c.log.WarnContext(ctx, "code_commit_failed", "kind", kindChange, "repo", p.RepoID(), "op_id", p.OpID,
		"error", err)
	return ChangeResult{}, fmt.Errorf("code: change operation %s: %w", p.OpID, err)
}

// changeRow is one change's row of code_changes, as a decide, an apply and an
// answer read it — and a read of the change ([changeRow.wire]), which serves
// every column of it.
type changeRow struct {
	id, repoID string
	number     int64
	title      string
	bodyBytes  int64
	state      ChangeState
	source     string
	target     string
	head       string
	revision   int64
	owner      string
	ownerKind  PartyKind
	// operator is the credential a person opened it with, "" for a seat's.
	operator string
	work     types.WorkItem
	// turn and run are the turn and the sandbox run that opened it, each ""
	// where there was none.
	turn, run string
	// openedAt is the broker instant (store.EncodeTime) the change was opened
	// at, and opened the packed position of the record that opened it.
	openedAt, opened int64
	// closedAt and mergedAt are the broker instants (store.EncodeTime) the
	// change was closed and merged at, each set exactly while it is.
	closedAt    sql.NullInt64
	closeReason string
	mergedAt    sql.NullInt64
	mergedVia   MergedVia
	mergeOID    string
	version     int64
	// position is the packed position of the last record that wrote it.
	position int64
}

// scanChange reads one row [changeColumns] spells.
func scanChange(scan func(...any) error) (changeRow, error) {
	var r changeRow
	var state, ownerKind, backend, via string
	err := scan(&r.id, &r.repoID, &r.number, &r.title, &r.bodyBytes, &state, &r.source, &r.target, &r.head,
		&r.revision, &r.owner, &ownerKind, &r.operator, &backend, &r.work.ID, &r.work.Key, &r.work.Project,
		&r.turn, &r.run, &r.openedAt, &r.opened, &r.closedAt, &r.closeReason, &r.mergedAt, &via, &r.mergeOID,
		&r.version, &r.position)
	r.state, r.ownerKind, r.work.Backend, r.mergedVia = ChangeState(state), PartyKind(ownerKind),
		types.WorkBackend(backend), MergedVia(via)
	return r, err
}

// changeRowIn reads the change id names, and false where no change has it.
func changeRowIn(ctx context.Context, tx *sql.Tx, id string) (changeRow, bool, error) {
	row, err := scanChange(tx.QueryRowContext(ctx, changeRowSQL, id).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return changeRow{}, false, nil
	case err != nil:
		return changeRow{}, false, fmt.Errorf("code: read change %s: %w", id, err)
	}
	return row, true, nil
}

// liveChangeOn is the number of the live change source heads on repoID, and
// false where it heads none — one seek of code_changes_open_idx.
func liveChangeOn(ctx context.Context, tx *sql.Tx, repoID, source string) (int64, bool, error) {
	var number int64
	err := tx.QueryRowContext(ctx, liveChangeOnSQL, repoID, source).Scan(&number)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("code: read the live change %s heads: %w", source, err)
	}
	return number, true, nil
}

// bodyOf is the change's body, "" where it has none — read by its key.
func bodyOf(ctx context.Context, tx *sql.Tx, changeID string) (string, error) {
	var body string
	err := tx.QueryRowContext(ctx, changeBodySQL, changeID).Scan(&body)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("code: read the body of change %s: %w", changeID, err)
	}
	return body, nil
}

// The lifecycle's reads, each registered in plan_test.go: a change by its key —
// a decide's, an apply's and an answer's — the live change a source branch
// heads, by code_changes_open_idx with its predicate spelled as the index
// declares it ([liveChanges]), and a change's body by its key.
const (
	// changeColumns is every column of a change [scanChange] reads, in its
	// order: every column code_changes holds, so a decide, an apply and a read
	// of the change scan one row alike.
	changeColumns = `id, repo_id, number, title, body_bytes, state, source_ref, target_ref, head_oid, revision,
		owner, owner_kind, owner_operator_id, work_backend, work_id, work_key, work_project, created_turn,
		created_run, opened_at, opened_position, closed_at, close_reason, merged_at, merged_via, merge_oid,
		version, position`
	changeRowSQL    = `SELECT ` + changeColumns + ` FROM code_changes WHERE id = ?`
	liveChangeOnSQL = `SELECT number FROM code_changes WHERE repo_id = ? AND source_ref = ? AND ` +
		liveChanges + ` LIMIT 1`
	changeBodySQL = `SELECT body FROM code_change_bodies WHERE change_id = ?`
)
