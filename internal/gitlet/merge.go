package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/words"
)

// A CHANGE'S MERGE IS ONE RECORD ITS GATE DECIDES (ADR-0096): a [Merge] on the
// repository's own subject, decided by its committer alone
// ([Committer.ProposeMerge]), so a change's merge, its close, its queue entry and
// the pushes moving either branch have one writer and contend on one subject: a
// record landing after a decide's snapshot fails its append, and the decide runs
// again on the rows it left.

// # Decided from one snapshot
//
// A merge is built on the repository's home Gitlet node onto the target tip it
// read and proposed with that tip (BaseOID) and the change's head (HeadOID). Its
// decide proves in ONE snapshot: the repository active; the build's copy at its
// drop epoch (else expunging: an expunge begun since may leave an object the
// merge reaches dropping); the change open, not a draft, into the target, at the
// head merged; the target AT BaseOID by the snapshot's refs, never the proposal
// (else target_moved); and the gate ([JudgeGate]) met by the merger, all but
// `mergeable`, which the build proved, or a person's recorded OVERRIDE, a gate
// requiring a check only through the queue ([GateRules.QueueTarget], else
// queue_target). Builders answer expunging and target_moved by building again.

// # Applied as one record
//
// [Applier.applyMerge] does all of a merge in one apply: the change MERGED FIRST
// (`merged_via direct`, the commit, the merger, any override's reason), so the
// push it then lands finds it merged, not live (M4's merge by push and close on
// a deleted source would fire); the target moved base to commit; a push row
// `via merge` (no commit for a fast-forward); the source deleted where asked (Q4:
// a seat's branch, by default), its head a ref drop an expunge keeps for its
// grace ([leavesObject]); a queued change's live entry ended; and the target's
// landing and green rows. Every guard is a skip, never a refusal: a record whose
// decide's facts no longer hold (only one the ledger would have collapsed)
// applies as nothing, logged code_merge_not_applied.

// # Base format
//
// The op, its payload and every field are in the base format ([RecordVersion]),
// as are `merged_via direct` and `via merge` in their closed sets: the domain
// landed with them. Only [Merge.WorkingFor] and [ChangeRouting.WorkingFor] came
// with version 2. It is no gate.

// Merge moves a change's target onto the commit its merge built: the record of
// op merge on the repository's subject, in the base format but for
// [Merge.WorkingFor].
type Merge struct {
	RepoID string `json:"repo_id"`
	// MergeID is the push the merge lands as, [WritePushID] of the proposal's
	// operation id, so a merge asked again under its operation is the same push.
	MergeID  string `json:"merge_id"`
	ChangeID string `json:"change_id"`
	// Method is how the change merged: a merge commit, one squashed commit,
	// or a fast-forward of the target to the change's head.
	Method MergeMethod `json:"method"`
	// TargetRef is the branch merged into; BaseOID its tip the merge was built
	// onto and HeadOID the change's head it took, which the decide holds the
	// branch and the change to; CommitOID what the target moves to (HeadOID
	// itself for a fast-forward).
	TargetRef string `json:"target_ref"`
	BaseOID   string `json:"base_oid"`
	HeadOID   string `json:"head_oid"`
	CommitOID string `json:"commit_oid"`
	// Pack holds the objects the merge built, stored before it was proposed;
	// nil for a fast-forward, which builds none.
	Pack *PackRef `json:"pack,omitempty"`
	// DeleteSource is whether the merge deletes the change's source branch
	// (Q4: a seat's, by default); SourceRef is that branch, STAMPED BY THE
	// DECIDE from the change it read, "" where it deletes none.
	DeleteSource bool   `json:"delete_source,omitempty"`
	SourceRef    string `json:"source_ref,omitempty"`
	// Override is a person's reason for merging past the target's merge
	// gate, at most [MaxOverrideReasonBytes]; "" for a merge its gate holds.
	Override string `json:"override,omitempty"`
	// Node is the Gitlet node that built the merge and ReceivedAt when it was
	// asked to: the push row's node and received_at.
	Node       string    `json:"node"`
	ReceivedAt time.Time `json:"received_at"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// COMMITTER: the change's authors but the merger.
	Routing *ChangeRouting `json:"routing,omitempty"`
	// Principal is the merger, the proposal's asker STAMPED by the committer:
	// whom the gate judged and the push and change rows name.
	Principal Principal `json:"principal"`
	// WorkingFor is whom an agent's merge was made for (ADR-0109), STAMPED BY
	// THE COMMITTER from [MergeProposal.WrittenFor]; nil for a person's merge
	// or one made for nobody. The apply keeps it on the push row
	// (code_pushes.working_for), where [landingPusher] reads it back.
	//
	// VERSION 2, as every payload's working_for is ([versionedFields]).
	WorkingFor *types.WorkingFor `json:"working_for,omitempty"`
	// Protected and Green are what the decide's snapshot held of the target,
	// STAMPED BY THE DECIDE so the apply matches no pattern: Protected writes
	// its landing row; Green (no check required besides) lands it green and
	// moves the target's green row to it.
	Protected bool `json:"protected,omitempty"`
	Green     bool `json:"green,omitempty"`
}

// MergeProposal is one merge of a change as its builder hands it to the
// repository's committer ([Committer.ProposeMerge]): the record, who asks and
// the floor they read at. OpID is the record's operation id, which the push id
// derives from ([WritePushID]).
type MergeProposal struct {
	OpID string `json:"op_id"`
	// Asker is who merges: a person, or a seat whose merge authority, like
	// every approval its gate counts, is judged by the deciding node's chart.
	// A token bound to nobody merges nothing. A seat's merge may name its
	// turn, which the push row records, but no executor: a merge is a tool
	// call's, never a box's.
	Asker Principal `json:"asker"`
	// Seen is the code log's position the merge was built at, zero for none:
	// the publish's session floor. The committer DECIDES AT OR PAST IT, or
	// refuses `behind` about itself so the runner-up decides.
	Seen statelog.Position `json:"seen,omitzero"`
	// DropEpoch is the repository's drop epoch in the copy the merge was built
	// from (internal/gitlet/cache's Synced.DropEpoch), as [Proposal.DropEpoch]
	// is a push's. Where the repository's is another, an expunge begun since
	// may have left dropping objects that copy held live, which the merge's
	// commit may reach and the expunge's commit retires, so the committer
	// refuses it [MergeRefusalExpunging] and the builder builds again. NEVER
	// RECORDED, as Seen is not: only the decide reads it, so the record's
	// format does not move for it.
	DropEpoch int64 `json:"drop_epoch,omitempty"`
	// WrittenFor is whom a SEAT'S merge is made for (ADR-0109), as
	// [ChangeProposal.WrittenFor] is: the asking turn's carrier, handed on from
	// the engine write the building node was asked ([EngineWrite.WrittenFor]),
	// or stated nobody; a person's states nothing. [writtenForInvalid] holds it
	// to the asker, ceiling-free: the building node held the write to one.
	//
	// No versionedFields row, a proposal being no record ([Merge.WorkingFor]
	// and [ChangeRouting.WorkingFor] take theirs). A data node whose build lacks
	// it, or its carrier's kind or apps, refuses a merge carrying somebody,
	// naming args.written_for (.kind, .apps[]) ([unknownToThisBuild]), so the
	// walk asks one that reads it; one made for nobody sends no member, and an
	// older build's arrives stated nobody ([MergeProposal.UnmarshalJSONFrom]).
	WrittenFor types.WrittenFor `json:"written_for,omitzero"`
	Merge      Merge            `json:"merge"`
}

// MergeResult is what became of one merge proposal.
type MergeResult struct {
	// Outcome is applied, pending or unknown for a proposal whose record was
	// appended, empty for one refused; applied is read from the change's row.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// MergeID is the push the merge lands as; MergeOID the commit its target
	// moved to, once its record applied.
	MergeID  string `json:"merge_id,omitempty"`
	MergeOID string `json:"merge_oid,omitempty"`
	// Position is where the record landed (for a retry, the one the ledger
	// found), or, for a not_applied or repo_deleted refusal after the append,
	// where it lies applying nowhere.
	Position statelog.Position `json:"position,omitzero"`
	// Refusal is why it was refused with no record, Detail its one sentence,
	// and Unmet, for gate_unmet, every precondition not met, each naming whom
	// it waits on.
	Refusal MergeRefusal   `json:"refusal,omitempty"`
	Detail  string         `json:"detail,omitempty"`
	Unmet   []Precondition `json:"unmet,omitempty"`
}

// Validate refuses, before it is queued, a proposal no committer could decide
// ([ErrInvalidProposal], naming the field): a field malformed or past its bound;
// an asker naming an executor or stating whom it works for as it may not
// ([writtenForInvalid]); a merge id not [WritePushID] of the operation id; a
// commit that is the base; a fast-forward to anything but the head or carrying
// a pack; a merge or squash carrying none or committing the head; a pack past
// [MaxWritePackBytes]; a field the committer stamps set by the asker; and a
// record past [EntryRoom].
//
// First, a written_for value this build does not know is answered as its decoder
// answers it on the wire, never ErrInvalidProposal ([unknownToThisBuild]): this
// build cannot judge the merge, and a committer that knows the value can.
func (p MergeProposal) Validate() error {
	_, err := p.measure()
	return err
}

// ValidateUnstored is [MergeProposal.Validate] of a merge whose pack is still
// uploading, as [Proposal.ValidateUnstored] is a push's: the pack is named by
// the widest object key and hash a stored one could take, so a merge passing
// here passes Validate once stored. A pack already stored is the caller's
// mistake.
func (p MergeProposal) ValidateUnstored() error {
	if p.Merge.Pack != nil {
		if !p.Merge.Pack.Object.IsZero() || p.Merge.Pack.Hash != "" {
			return fmt.Errorf("%w: merge.pack: already stored as %s — Validate checks a stored pack",
				ErrInvalidProposal, p.Merge.Pack.Object)
		}
		pack := *p.Merge.Pack
		pack.Object, pack.Hash = largestEntry().Pack.Object, largestEntry().Pack.Hash
		p.Merge.Pack = &pack
	}
	return p.Validate()
}

// measure validates p and answers its queued byte charge: its record's payload
// as the committer stamps it, held under [EntryRoom]. The source, which the
// decide stamps, counts at the longest a ref may encode to: every byte past
// its prefix one JSON escapes as six.
func (p MergeProposal) measure() (int, error) {
	if err := p.validFields(); err != nil {
		return 0, err
	}
	m := p.stamped()
	if m.DeleteSource {
		m.SourceRef = branchPrefix + strings.Repeat("<", MaxRefNameBytes-len(branchPrefix))
	}
	m.Protected, m.Green = true, true
	encoded, err := json.Marshal(m)
	if err != nil {
		return 0, fmt.Errorf("%w: merge: encode it: %w", ErrInvalidProposal, err)
	}
	// Plus the room of the routing its committer stamps.
	if bytes := len(encoded) + MaxRoutingBytes; bytes > EntryRoom {
		return 0, fmt.Errorf("%w: merge: %d bytes with the routing it carries, past the %d one proposal may "+
			"take of its record", ErrInvalidProposal, bytes, EntryRoom)
	}
	return len(encoded) + MaxRoutingBytes, nil
}

// validFields is every rule of [MergeProposal.Validate] but the record's bytes.
func (p MergeProposal) validFields() error {
	// FIRST, A VALUE THIS BUILD DOES NOT KNOW, before any rule judges the
	// merge (writtenfor.go's head).
	if err := unknownToThisBuild(writtenForAt, p.WrittenFor); err != nil {
		return err
	}
	if err := statelog.CheckCallerOpID(p.OpID); err != nil {
		return badField("op_id", "%v", err)
	}
	if err := stampedRouting("merge", p.Merge.Routing); err != nil {
		return err
	}
	switch why := p.Asker.invalid(); {
	case why != "":
		return fmt.Errorf("%w: asker: %s", ErrInvalidProposal, why)
	case p.Asker.Run.RunID != "":
		return badField("asker.run.run_id", "a merge is a tool call's, whose run is its turn: it names no "+
			"executor")
	}
	if err := writtenForInvalid(p.Asker, p.WrittenFor); err != nil {
		return refusedCarrier(ErrInvalidProposal, err)
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
	if p.DropEpoch < 0 {
		return badField("drop_epoch", "%d is no drop epoch: it counts the expunges begun on the repository, "+
			"from 0", p.DropEpoch)
	}
	m := p.Merge
	if err := validRepoID("merge.repo_id", m.RepoID); err != nil {
		return err
	}
	if want := WritePushID(p.OpID); m.MergeID != want {
		return badField("merge.merge_id", "%q, and the push a merge under %s lands as is %s", m.MergeID,
			p.OpID, want)
	}
	if err := bareUUIDv7("merge.change_id", m.ChangeID); err != nil {
		return err
	}
	if why := branchProblem(m.TargetRef); why != "" {
		return badField("merge.target_ref", "%s", why)
	}
	if !m.Method.Valid() {
		return badField("merge.method", "%q is not one of %s", m.Method, words.Or(MergeMethods()))
	}
	for _, oid := range []struct{ field, value string }{
		{"merge.base_oid", m.BaseOID}, {"merge.head_oid", m.HeadOID}, {"merge.commit_oid", m.CommitOID},
	} {
		if !gitref.ValidOID(oid.value) || oid.value == ZeroOID {
			return badField(oid.field, "%q is not a whole object id", oid.value)
		}
	}
	switch {
	case m.CommitOID == m.BaseOID:
		return badField("merge.commit_oid", "the merge's commit is its base, %s: a merge moves its target",
			short(m.BaseOID))
	case m.Method == MergeMethodFastForward && m.CommitOID != m.HeadOID:
		return badField("merge.commit_oid", "a fast-forward moves its target to the change's head %s, and "+
			"this one names %s", short(m.HeadOID), short(m.CommitOID))
	case m.Method == MergeMethodFastForward && m.Pack != nil:
		return badField("merge.pack", "a fast-forward builds no object, so it stores no pack")
	case m.Method != MergeMethodFastForward && m.CommitOID == m.HeadOID:
		return badField("merge.commit_oid", "a %s builds a commit of its own, and this one names the "+
			"change's head", m.Method)
	case m.Method != MergeMethodFastForward && m.Pack == nil:
		return badField("merge.pack", "a %s builds a commit of its own, which a pack carries", m.Method)
	}
	if m.Pack != nil {
		if err := m.Pack.Check(); err != nil {
			return fmt.Errorf("%w: merge.pack.%w", ErrInvalidProposal, err)
		}
		if m.Pack.PackBytes() > MaxWritePackBytes {
			// THE BUILD'S OWN BOUND, an engine write's: a Gitlet node answers a
			// merge whose pack is past it before anything is uploaded.
			return badField("merge.pack", "%d bytes of pack, past the %d an engine-built pack may store "+
				"(MaxWritePackBytes)", m.Pack.PackBytes(), MaxWritePackBytes)
		}
	}
	if why := textProblem(m.Override, MaxOverrideReasonBytes, "merge's override reason"); why != "" {
		return badField("merge.override", "%s", why)
	}
	switch {
	case strings.TrimSpace(m.Node) == "":
		return badField("merge.node", "a merge names the Gitlet node that built it")
	case m.ReceivedAt.IsZero():
		return badField("merge.received_at", "a merge names when its builder was asked for it")
	case m.Principal != (Principal{}):
		return badField("merge.principal", "the committer stamps who merged it from the asker, and an asker "+
			"sends none")
	case m.WorkingFor != nil:
		return badField("merge.working_for", "the committer stamps whom a merge was made for from the "+
			"proposal's written_for, and an asker sends none")
	case m.SourceRef != "":
		return badField("merge.source_ref", "the committer stamps the source a merge deletes from the "+
			"change it reads, and an asker sends none")
	case m.Protected || m.Green:
		return badField("merge.protected", "the committer stamps what its snapshot holds of the target, and "+
			"an asker sends none")
	}
	return nil
}

// stamped is p's record as the committer forms it before its decide: its
// asker as the record's principal, and whom p is made for beside it.
func (p MergeProposal) stamped() Merge {
	m := p.Merge
	m.Principal, m.WorkingFor = p.Asker, p.WrittenFor.Carrier()
	return m
}

// RepoID is the repository p is about — the ONE answer the router keys p by,
// the committer queues it under and the estate's seam raises the repository's
// floor for.
func (p MergeProposal) RepoID() string { return p.Merge.RepoID }

// JudgesChart reports that p's decide reads the deciding node's chart, so a
// node holding none, or confirmed behind the company's configuration, never
// decides it: EVERY MERGE, since its gate judges approvals, owners' lead chains
// and a seat's merge authority by the chart ([JudgeGate]), as
// [QueueProposal.JudgesChart] does. ONE ANSWER, which the committer queues and
// decides by and the estate declares the operation's grant by.
func (p MergeProposal) JudgesChart() bool { return true }

// mergeDecidedRows is the most rows a merge's OWN writes make its apply touch,
// beside the push it lands ([Applier.applyMerge]): the change's row, its live
// queue entry, its stored readiness ([clearReadiness]), the target's landing
// row, its green row and the tip work its green row pays ([advanceGreen]), and
// the tally row its merge moves (apply_counts.go).
const mergeDecidedRows = 6 + tallyRows + routingNoticeRows

// mergePushRows is the most rows a merge's push makes its apply write besides
// the changes it moves: its push's row, its two commands (the target's move,
// and the source's deletion with the drop it leaves and the old tombstone its
// sweep may take: [commandRows]), its pack, Gitlet's pusher day
// ([pusherDayRows]), and a seat's day ([seatDayRows]), which entryRows counts
// for any agent's entry.
const mergePushRows = 1 + 1 + 3 + 1 + seatDayRows + pusherDayRows

// mergeRecordRows is the most rows one merge record makes its apply write
// before its horizons: its own ([mergeDecidedRows]); its push's
// ([mergePushRows]); and others' changes its push moves ([changeBook]): the
// live change the target heads as a source (a revision, an author, its queue
// entry ended), and up to [MaxPushMerges] live changes into the target at the
// merge's commit (each merged, queue entry ended, readiness cleared), plus
// their tally row. Its own change is merged first, so the push moves it no
// further. Far under MaxRecordRows with the horizons' room ([recordRows]),
// which [TestAMergeRecordFitsItsRows] holds, so a merge's decide counts no
// change of its push's, as a push batch's must.
const mergeRecordRows = mergeDecidedRows + mergePushRows + 4 + 3*MaxPushMerges + tallyRows

// ProposeMerge decides one merge as a batch of its own in the repository's
// queue, under a push's queue and bounds ([Committer.Propose]) as
// [Committer.ProposeQueue] is: busy at once for a full queue or a node holding
// its bytes, busy undecided for a deadline too short for a batch;
// [ErrNotServing] from a stopping committer or one holding no current chart,
// which judges every merge; [ErrInvalidProposal] for an invalid one. ctx bounds
// only this caller's wait.
//
// ITS OPERATION ID IS THE PROPOSAL'S OWN, so the same merge handed to the
// runner-up collapses onto the record that landed; ITS SESSION IS ITS SEEN.
// Applied is read FROM THE CHANGE'S ROW, and only the committer that landed
// the record, never a collapsed retry, announces it (`change_merged`).
func (c *Committer) ProposeMerge(ctx context.Context, p MergeProposal) (MergeResult, error) {
	bytes, err := p.measure()
	if err != nil {
		c.countMerge(MergeResult{}, err)
		return MergeResult{}, err
	}
	deadline, _ := ctx.Deadline()
	a := c.queue(ctx, &queued{merge: &p, bytes: bytes, rows: mergeRecordRows, arrived: c.clock(),
		deadline: deadline, gone: ctx.Done(), answer: make(chan answered, 1)})
	c.countMerge(a.merge, a.err)
	return a.merge, a.err
}

// countMerge counts one merge's answer under what became of it, as
// [Committer.countQueue] counts a queue's write.
func (c *Committer) countMerge(res MergeResult, err error) {
	if c.metrics == nil {
		return
	}
	outcome := string(res.Outcome)
	switch {
	case handedOn(err):
		outcome = "handed_on"
	case err != nil:
		outcome = "failed"
	case res.Refusal != "":
		outcome = string(res.Refusal)
	}
	c.metrics.Add(metrics.CodeMergeOutcomes, 1, metrics.Attrs{"outcome": outcome})
}

// mergeDecision is one round's decision of a merge: its refusal, its sentence
// and the preconditions a gate_unmet names where it is refused; the record's
// payload, stamped, where it forms one; and the version the repository's row
// was read at either way.
type mergeDecision struct {
	refusal MergeRefusal
	detail  string
	unmet   []Precondition
	payload *Merge
	version int64
}

// refuse is d refused under code with detail.
func (d mergeDecision) refuse(code MergeRefusal, detail string) (mergeDecision, error) {
	d.refusal, d.detail = code, detail
	return d, nil
}

// decideMerge is one round's decision of p over the rows tx holds, its rules in
// the order this file's head reads them. Pure over (the rows, p, seats,
// limits, retention, now): seats is the deciding epoch's chart the gate judges
// by; limits its code.limits (the merge_by floor, and the pack's size bound,
// as a push's); retention its code.deleted_retention_days, which a
// repo_deleted refusal names; now the clock an upload's age is judged by.
func decideMerge(ctx context.Context, tx *sql.Tx, p MergeProposal, seats SeatRules, limits Limits,
	retention time.Duration, now time.Time) (mergeDecision, error) {

	m := p.Merge
	repo, found, err := pushRepoIn(ctx, tx, m.RepoID)
	if err != nil {
		return mergeDecision{}, err
	}
	d := mergeDecision{version: repo.version}
	switch {
	case !found || repo.status == RepoStatusDeleted || repo.status == RepoStatusPurging:
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		detail, err := goneDetail(ctx, tx, m.RepoID, repo, found, retention)
		if err != nil {
			return mergeDecision{}, err
		}
		return d.refuse(MergeRefusalRepoDeleted, detail)
	case repo.status == RepoStatusArchived:
		return d.refuse(MergeRefusalRepoArchived, fmt.Sprintf("repository %s is archived and read-only "+
			"(code.repos archived: true): nothing merges into it", repo.name))
	case repo.status == RepoStatusImporting:
		return d.refuse(MergeRefusalRepoImporting, ImportingDetail(repo.name))
	case repo.mirrored:
		// A pull mirror moves only by its syncs, which record a merge its
		// source made as a landing.
		return d.refuse(MergeRefusalRepoMirrored, MirroredDetail(repo.name, repo.managedBy))
	case p.DropEpoch != repo.dropEpoch:
		// THE DROP EPOCH, before every rule of the merge's own, as a push's:
		// built on a copy at another epoch, its commit may reach an object an
		// expunge begun since left dropping.
		return d.refuse(MergeRefusalExpunging, ExpungingBuiltDetail(repo.name, "merge", "its builder",
			p.DropEpoch, repo.dropEpoch))
	case seats.Roster == nil:
		// THE CALLER'S BUG: the committer hands a merge to the repository's
		// next committer before deciding it with no chart.
		return mergeDecision{}, fmt.Errorf("gitlet: a merge on %s reached the decide with no chart to judge it "+
			"by — the committer hands such a write to the repository's next committer", m.RepoID)
	}
	f, found, err := LoadGateFacts(ctx, tx, m.RepoID, m.ChangeID)
	if err != nil {
		return mergeDecision{}, err
	}
	if !found {
		return d.refuse(MergeRefusalChangeNotFound, fmt.Sprintf("repository %s holds no change %s", repo.name,
			m.ChangeID))
	}
	c := f.Rows.Change
	named := ChangeRef(repo.name, c.Number)
	switch {
	case c.State == ChangeStateClosed || c.State == ChangeStateMerged:
		return d.refuse(MergeRefusalChangeNotOpen, fmt.Sprintf("%s is %s, and only a live change merges", named,
			c.State))
	case c.State == ChangeStateDraft:
		return d.refuse(MergeRefusalChangeDraft, fmt.Sprintf("%s is a draft: its owner, a principal that "+
			"maintains the repository or a person marks it ready for review first", named))
	case c.TargetRef != m.TargetRef:
		return d.refuse(MergeRefusalStale, fmt.Sprintf("%s merges into %s, and this merge was built into %s: "+
			"read it again", named, c.TargetRef, m.TargetRef))
	case c.Head != m.HeadOID:
		return d.refuse(MergeRefusalHeadMoved, fmt.Sprintf("%s's head is %s, and this merge was built from %s: "+
			"read it again", named, short(c.Head), short(m.HeadOID)))
	}
	// THE TARGET AS THIS SNAPSHOT HOLDS IT, never as the proposal says: the
	// commit merges its base, so the branch moves only if still there.
	refs, err := refsNamed(ctx, tx, m.RepoID, []string{c.TargetRef, c.SourceRef})
	if err != nil {
		return mergeDecision{}, fmt.Errorf("gitlet: read %s's and %s's tips on %s: %w", c.TargetRef, c.SourceRef,
			repo.name, err)
	}
	tip := refTip(refs, c.TargetRef)
	if tip != m.BaseOID {
		return d.refuse(MergeRefusalTargetMoved, fmt.Sprintf("%s is at %s, and this merge of %s was built onto "+
			"%s: build it again onto the tip the branch holds", c.TargetRef, short(tip), named, short(m.BaseOID)))
	}
	_, kind := p.Asker.party()
	if m.Override != "" && kind != PartyPerson {
		return d.refuse(MergeRefusalNotPerson, fmt.Sprintf("only a person merges past a merge gate, and this "+
			"merge of %s is overridden by %s", named, askerNamed(p.Asker)))
	}
	f.Roster, f.Floor, f.Merger = seats.Roster, limits.MergeByFloor, p.Asker
	gate, err := f.Gate()
	if err != nil {
		return mergeDecision{}, err
	}
	if m.Override == "" {
		if gate.QueueTarget() {
			return d.refuse(MergeRefusalQueueTarget, fmt.Sprintf("%s's merge gate requires a check, so a change "+
				"merges into it through its queue: enqueue %s, or a person overrides the gate", c.TargetRef, named))
		}
		// THE GATE, all but `mergeable`, which the build proved on its base.
		r := JudgeGate(f, gate)
		for _, pre := range r.Preconditions {
			if pre.State != PreconditionMet && pre.Kind != PreconditionMergeable {
				d.unmet = append(d.unmet, pre)
			}
		}
		if len(d.unmet) > 0 {
			return d.refuse(MergeRefusalGateUnmet, fmt.Sprintf("%s does not meet %s's merge gate: %s", named,
				c.TargetRef, d.unmet[0].Detail))
		}
	}
	grant := GrantOf(p.Asker, repo.grantRef(m.RepoID), seats.Roster)
	if m.DeleteSource {
		if refusal, why := sourceDeletion(f.Rules, repo, c.SourceRef, refTip(refs, c.SourceRef), p.Asker, grant); refusal != "" {
			return d.refuse(MergeRefusalSourceRefused, fmt.Sprintf("this merge of %s deletes its source %s, "+
				"and a push deleting it is refused %s: %s — merge it keeping its source", named, c.SourceRef,
				refusal, why))
		}
	}
	// THE PACK, held to a push's rules ([working.decide], [working.behind]):
	// its upload young enough to name, the repository's size with it within
	// bound ([OutsideSeatShare] of it for a seat outside the owners), and the
	// live packs under compaction's bound, judged last as a push's is.
	if m.Pack != nil {
		bound := limits.RepoMaxBytes
		if grant.outside() {
			bound = limits.RepoMaxBytes / OutsideSeatShare
		}
		if detail, expired := uploadExpired("the merge's pack", m.Pack.Object, now,
			"its builder builds it again"); expired {
			return d.refuse(MergeRefusalUploadExpired, detail)
		}
		if would := repo.liveBytes + m.Pack.Size; would > bound {
			return d.refuse(MergeRefusalRepoSize, sizeDetail(repo.name, would, bound, limits, grant))
		}
		if repo.livePacks >= MaxPacksPerRepo {
			return d.refuse(MergeRefusalCompactionBehind, BehindDetail(repo.name, repo.livePacks))
		}
	}
	payload := p.stamped()
	if m.DeleteSource {
		payload.SourceRef = c.SourceRef
	}
	payload.Protected = gate.Protected()
	payload.Green = gate.Protected() && len(gate.Checks) == 0
	// WHOM IT CONCERNS: the change's authors but the merger, the record's
	// actor, beside whom the merge was made for.
	row, _, err := changeRowIn(ctx, tx, m.ChangeID)
	if err != nil {
		return mergeDecision{}, err
	}
	merger, mergerKind := p.Asker.party()
	if payload.Routing, err = routingFor(ctx, tx, concerning{repoName: repo.name, change: row, actor: merger,
		actorKind: mergerKind, workingFor: p.WrittenFor.Carrier(), authors: true, merged: true}); err != nil {
		return mergeDecision{}, err
	}
	d.payload = &payload
	return d, nil
}

// refTip is ref's target as refs holds it, the zero id for a ref the
// repository holds no live row of.
func refTip(refs map[string]string, ref string) string {
	if target := refs[ref]; target != "" {
		return target
	}
	return ZeroOID
}

// sourceDeletion is the refusal and sentence a push deleting the change's
// source, at tip, would meet from merger, or "": [StatelessRefusal]; the
// merger's grant ([Grant.RefRule]) on a source OUTSIDE the seats' namespace,
// since a seat's own branch is the merge's to delete whoever merges (Q4: what
// carried the change ends with it); every matching protection
// ([ProtectionRefusal]); and [DefaultBranchRefusal], so a change proposing
// main into a release branch merges keeping main.
func sourceDeletion(rules []Protection, repo pushRepo, source, tip string, merger Principal,
	grant Grant) (Refusal, string) {
	del := Command{Ref: source, Old: tip, New: ZeroOID}
	if r := StatelessRefusal(del); r != "" {
		return r, statelessDetail(del, r)
	}
	if _, seats := seatNamespaceRef(source); !seats {
		if r, why := grant.RefRule(del); r != "" {
			return r, why
		}
	}
	protections, err := ParseProtections(pushRules(rules))
	if err != nil {
		// FAIL CLOSED: every stored rule passed validation before a record
		// carried it, so one this build cannot read keeps the branch.
		return RefusalProtectedDelete, err.Error()
	}
	if r, rule := ProtectionRefusal(del, protections, merger.Kind); r != "" {
		return r, source + " " + ProtectionDetail(rule, r)
	}
	if r, why := DefaultBranchRefusal(del, repo.name, repo.defaultBranch); r != "" {
		return r, why
	}
	return "", ""
}

// askerNamed is p in a sentence: a seat by its handle, a person by the human
// seat they hold, a token bound to nobody as one.
func askerNamed(p Principal) string {
	if handle, kind := p.party(); kind != "" {
		return Party{Handle: handle, Kind: kind}.named()
	}
	return "a credential bound to nobody"
}

// mergeWrite publishes one merge and answers it, a batch of its own under the
// committer's lifetime and one CommitBudget, as [Committer.queueWrite] is.
func (c *Committer) mergeWrite(repoID string, q *queued) {
	p := *q.merge
	seats, held := c.seatRules()
	if !held {
		q.answer <- answered{err: fmt.Errorf("%w: this node holds no chart yet to judge the merge by; the "+
			"repository's next committer decides it", ErrNotServing)}
		return
	}
	ctx, cancel := context.WithTimeout(c.life, CommitBudget)
	defer cancel()
	retention, limits := c.retention(), c.limits()
	subject := RepoSubject(repoID)
	// THE SCOPE NAMES WHAT THE DECIDE READS (M5-D33): the gate reads the
	// change's conversation and facts and the repository's checks, so a node
	// holding a deferred record of any of them decides no merge.
	scope := []string{RepoPath(repoID), ChecksPath(repoID), ChangePath(p.Merge.ChangeID)}
	var (
		last    mergeDecision
		decided bool
	)
	res, err := c.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: p.OpID, Pattern: statelog.PatternArbitrated, Session: p.Seen,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			last, decided = mergeDecision{}, false
			now := c.now()
			d, err := decideMerge(ctx, tx, p, seats, limits, retention, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			last, decided = d, true
			if d.payload == nil {
				return statelog.Decision{Version: d.version}, nil
			}
			decision, err := recordFor(stamp, subject, OpMerge, scope, p.OpID,
				Actor{ID: c.nodeID, Kind: ActorSystem}, *d.payload, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			if len(decision.Payload) > MaxRecordBytes {
				return statelog.Decision{}, fmt.Errorf("%w: %d bytes, past MaxRecordBytes (%d)", ErrRecordCap,
					len(decision.Payload), MaxRecordBytes)
			}
			decision.Version = d.version
			return decision, nil
		},
	})
	out, landed, answerErr := c.mergeAnswer(ctx, p, last, decided, res, err)
	q.answer <- answered{merge: out, err: answerErr}
	if landed != nil {
		c.announceLater(announcement{kind: "change_merged", repoID: landed.RepoID, id: landed.ChangeID,
			publish: func(ctx context.Context, when time.Time) { c.announceMerged(ctx, when, landed) }})
	}
}

// mergeAnswer is a merge's answer from the publish's result and the last
// round's decision, and the event its own fresh landing announces — nil for
// any other answer.
func (c *Committer) mergeAnswer(ctx context.Context, p MergeProposal, last mergeDecision, decided bool,
	res statelog.Result, err error) (MergeResult, *types.ChangeMerged, error) {

	if err != nil {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		out, err := c.refusedMerge(ctx, p, err)
		return out, nil, err
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		if !res.Position.IsZero() {
			return c.appliedMerge(ctx, p, res)
		}
		if decided && last.refusal != "" {
			return MergeResult{MergeID: p.Merge.MergeID, Refusal: last.refusal, Detail: last.detail,
				Unmet: last.unmet}, nil, nil
		}
		return MergeResult{}, nil, fmt.Errorf("gitlet: the publish of operation %s answered applied with no "+
			"record, and its decide refused nothing", p.OpID)
	case statelog.OutcomePending:
		return MergeResult{Outcome: statelog.OutcomePending, MergeID: p.Merge.MergeID, Position: res.Position},
			nil, nil
	}
	return MergeResult{Outcome: statelog.OutcomeUnknown, MergeID: p.Merge.MergeID}, nil, nil
}

// appliedMerge answers a merge whose record applied at res.Position FROM THE
// CHANGE'S ROW: merged at that position and commit, or unknown, logged at
// ERROR, where the row does not carry it. Only a fresh landing, never a retry
// the ledger collapsed, answers its event beside.
func (c *Committer) appliedMerge(ctx context.Context, p MergeProposal, res statelog.Result) (MergeResult,
	*types.ChangeMerged, error) {

	at, m := res.Position, p.Merge
	var (
		row      changeRow
		found    bool
		repoName string
	)
	if err := c.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		if row, found, err = changeRowIn(ctx, tx, m.ChangeID); err != nil || !found {
			return err
		}
		switch err := tx.QueryRowContext(ctx, repoNameSQL, m.RepoID).Scan(&repoName); {
		case errors.Is(err, sql.ErrNoRows):
			repoName = m.RepoID
		case err != nil:
			return err
		}
		return nil
	}); err != nil {
		failed := fmt.Errorf("gitlet: the record of operation %s applied at %s and its change's row could not be "+
			"read: %w", p.OpID, at, err)
		c.log.WarnContext(ctx, "code_commit_failed", "kind", kindMerge, "repo", m.RepoID, "op_id", p.OpID,
			"error", failed)
		return MergeResult{}, nil, failed
	}
	if !found || row.state != ChangeStateMerged || row.mergedVia != MergedViaDirect || row.position != at.Packed() {
		c.log.ErrorContext(ctx, "code_merge_unanswerable", "repo", m.RepoID, "change", m.ChangeID,
			"op_id", p.OpID, "position", at.String())
		return MergeResult{Outcome: statelog.OutcomeUnknown, MergeID: m.MergeID, Detail: fmt.Sprintf("the "+
			"merge's record applied at %s and the change's row this node holds does not carry it", at)}, nil, nil
	}
	out := MergeResult{Outcome: statelog.OutcomeApplied, MergeID: m.MergeID, MergeOID: row.mergeOID, Position: at}
	if res.Collapsed {
		// A collapsed retry: the earlier landing's call announced it.
		return out, nil, nil
	}
	return out, &types.ChangeMerged{RepoID: m.RepoID, RepoName: repoName, ChangeID: m.ChangeID,
		Number: row.number, Target: row.target, Method: string(m.Method), Commit: row.mergeOID,
		MergedBy: row.mergedBy, MergedByKind: string(row.mergedByKind), Override: row.overrideReason}, nil
}

// announceMerged publishes a merge's event, stamped when, under ctx, the
// announcer's one [checkEventBudget]: a failure is logged, the change's row
// still saying it merged. It runs after the answer is handed back, so neither
// the asker nor the repository's next write waits on it (announce.go).
func (c *Committer) announceMerged(ctx context.Context, when time.Time, e *types.ChangeMerged) {
	if err := announceOn(ctx, c.events, c.nodeID, when, *e); err != nil {
		c.log.WarnContext(ctx, "change_merged_unpublished", "repo", e.RepoID, "change", e.ChangeID,
			"error", err.Error(), "detail", "the activity feed and the event log do not hold this merge; the "+
				"change's row and its push row on the code log still do")
	}
}

// refusedMerge answers a merge whose publish failed: [Committer.refusedQueue]'s
// mapping in a merge's words.
func (c *Committer) refusedMerge(ctx context.Context, p MergeProposal, err error) (MergeResult, error) {
	var refusal *statelog.Unavailable
	switch {
	case errors.Is(err, ErrInvalidProposal):
		c.log.ErrorContext(ctx, "code_merge_invalid", "repo", p.RepoID(), "op_id", p.OpID, "error", err)
		return MergeResult{}, err
	case errors.Is(err, statelog.ErrConflict):
		c.countBusy(p.RepoID(), kindMerge, busyContended, 1)
		return MergeResult{MergeID: p.Merge.MergeID, Refusal: MergeRefusalBusy,
			Detail: "the repository kept changing for every round the merge may take; " + changeAgain}, nil
	case errors.As(err, &refusal) && refusal.Reason == statelog.ReasonDeleted:
		var at statelog.Position
		if refusal.Landed() {
			at = refusal.Position
		}
		return MergeResult{MergeID: p.Merge.MergeID, Refusal: MergeRefusalRepoDeleted, Position: at,
			Detail: c.purgedDetail(ctx, p.RepoID(), p.OpID)}, nil
	case errors.As(err, &refusal) && refusal.Landed():
		return MergeResult{MergeID: p.Merge.MergeID, Refusal: MergeRefusalNotApplied, Position: refusal.Position,
			Detail: fmt.Sprintf("the merge applies nowhere: its record at %s was durable and the code log's %s "+
				"gate drops it on every node; %s", refusal.Position, refusal.Reason, changeAgain)}, nil
	case errors.As(err, &refusal) && nodeRefusals[refusal.Reason]:
		return MergeResult{}, fmt.Errorf("%w: %w", ErrNotServing, refusal)
	case errors.As(err, &refusal):
		return MergeResult{MergeID: p.Merge.MergeID, Refusal: MergeRefusalUnavailable,
			Detail: "the code log refused the merge: " + strings.TrimPrefix(refusal.Error(), "statelog: ")}, nil
	}
	c.log.WarnContext(ctx, "code_commit_failed", "kind", kindMerge, "repo", p.RepoID(), "op_id", p.OpID,
		"error", err)
	return MergeResult{}, fmt.Errorf("gitlet: merge operation %s: %w", p.OpID, err)
}
