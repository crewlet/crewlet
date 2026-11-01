package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/words"
)

// A CHANGE'S MERGE, AS ONE RECORD ITS GATE DECIDES (ADR-0089): a change merges
// into its target by a MERGE RECORD ([Merge]) on the REPOSITORY's own subject,
// the one its pushes, its changes' lifecycles and its merge queues are on,
// decided by its committer alone ([Committer.ProposeMerge]) — so a change's
// merge, its close, its entry in a queue and the pushes that move either branch
// have one writer, and contend on one subject.
//
// # Decided from one snapshot
//
// A merge is built elsewhere — its commit and its pack written by the
// repository's home git node onto the tip it read of the target — and proposed
// with what it was built from: the target's tip (BaseOID) and the change's head
// (HeadOID). Its decide reads ONE snapshot and proves every precondition in
// it: the repository active; the change open, not a draft, into the target and
// at the head its builder merged; the target AT THE TIP THE MERGE WAS BUILT
// ONTO, read from the snapshot's refs and never from the proposal, since a
// merge built onto a tip the target has moved from merges a branch that is no
// longer there (target_moved, which its builder answers by building again);
// and the change's merge gate ([JudgeGate]) met with the record's principal as
// the merger — every precondition but `mergeable`, which the merge's own build
// proved against the base it names — or a person's OVERRIDE of it, the reason
// recorded. A target whose gate requires a check lands its merges through its
// queue ([GateRules.QueueTarget]), so a merge asked of it directly is refused
// queue_target unless a person overrides. Between the snapshot and the record
// that moves the branch, the race is the broker's: the record is arbitrated on
// the repository's subject, so a record that landed after the snapshot fails
// the append and the decide runs again on the rows it left.
//
// # Applied as one record
//
// The record does everything a merge is, in one apply ([Applier.applyMerge]):
// the change MERGED FIRST — `merged_via direct`, the commit, who merged it and
// any override's reason — so that the push the record then lands finds it
// merged, as a push that set its target to its head (M4's merge by push) and
// one that deleted its source (M4's close on a deleted source) would otherwise
// find it live; the target moved from its base onto the merge's commit; a push
// row `via merge` naming the merger, the commit it introduced (none for a
// fast-forward, whose head its own pushes brought) and the pack; the source
// branch deleted where the merge asks it (Q4: a seat's branch, by default); the
// change's live queue entry ended, where a person's override merged a queued
// change directly; the target's landing row, where a rule protects it; and its
// green row, where its gate requires no check. Every guard of the apply is a
// skip, never a refusal: a record that finds what its decide held untrue —
// which no correct log carries but a record the ledger would have collapsed —
// applies as nothing, logged (code_merge_not_applied).
//
// # Base format, as a stated condition
//
// The op, its payload and every field here are in the base format: no build
// that carries the code domain has run without them — none of the domain is on
// main, so no image and no release holds it — and while that is so, every
// build that meets a merge reads it. Once a build without them has run, the op
// takes {Name: "MutationRecord.Op merge", Since: 2, Path: []string{"op"},
// Equals: "merge"} in versionedFields, with RecordVersion at 2, its line under
// "What each version added", a Carrying record in the statelogtest candidate
// and the op excepted from the stamp test's base-format loop — and so do the
// values a merge adds to closed sets older builds read: `merged_via direct` on a
// change's row and `via merge` on a push's, which an older reader of either
// refuses as a way it does not know (scanPush). Neither is a gate.

// Merge moves a change's target onto the commit its merge built: the record of
// op merge on the repository's subject.
//
// BASE FORMAT, under the condition this file's head states; once a build
// without it has run it takes {Name: "MutationRecord.Op merge", Since: 2,
// Path: []string{"op"}, Equals: "merge"}.
type Merge struct {
	RepoID string `json:"repo_id"`
	// MergeID is the push the merge lands as — [WritePushID] of the
	// proposal's operation id, the id its builder's scratch and its push row
	// are named by — so a merge asked again under its operation is the same
	// push.
	MergeID  string `json:"merge_id"`
	ChangeID string `json:"change_id"`
	// Method is how the change merged: a merge commit, one squashed commit,
	// or a fast-forward of the target to the change's head.
	Method MergeMethod `json:"method"`
	// TargetRef is the branch the change merges into; BaseOID the tip of it
	// the merge was built onto, which the decide holds the branch to; HeadOID
	// the change's head the merge took, which it holds the change to; and
	// CommitOID what the target moves to — the merge commit, the squashed
	// one, or HeadOID itself for a fast-forward.
	TargetRef string `json:"target_ref"`
	BaseOID   string `json:"base_oid"`
	HeadOID   string `json:"head_oid"`
	CommitOID string `json:"commit_oid"`
	// Pack is the pack of the objects the merge built — its commit and the
	// trees and blobs the merge wrote — stored before it was proposed, and
	// nil for a fast-forward, which builds none.
	Pack *PackRef `json:"pack,omitempty"`
	// DeleteSource is whether the merge deletes the change's source branch
	// (Q4: a seat's branch, by default, its `delete_source`); SourceRef is
	// the branch it deletes, STAMPED BY THE DECIDE from the change it read
	// and "" where it deletes none.
	DeleteSource bool   `json:"delete_source,omitempty"`
	SourceRef    string `json:"source_ref,omitempty"`
	// Override is a person's reason for merging past the target's merge
	// gate, at most [MaxOverrideReasonBytes]; "" for a merge its gate holds.
	Override string `json:"override,omitempty"`
	// Node is the git node that built the merge and ReceivedAt when it was
	// asked to — the push row's node and received_at, as an engine write's
	// are.
	Node       string    `json:"node"`
	ReceivedAt time.Time `json:"received_at"`
	// Principal is who merged it — the proposal's asker, STAMPED by the
	// committer: the merger its gate judged, and whom its push row and the
	// change's row name.
	Principal Principal `json:"principal"`
	// Protected and Green are what the decide's snapshot held of the target,
	// STAMPED BY THE DECIDE so the apply matches no pattern: Protected that a
	// rule protects it, so the merge writes its landing row; Green that its
	// gate requires no check besides, so the landing is green as it lands
	// and the target's green row moves to it.
	Protected bool `json:"protected,omitempty"`
	Green     bool `json:"green,omitempty"`
}

// MergeProposal is one merge of a change as its builder hands it to the
// repository's committer ([Committer.ProposeMerge]): the record, beside who
// asks and the floor they read at. Its OpID is the record's operation id — the
// merge's push id is derived from it ([WritePushID]) — and its Seen the
// publish's session floor.
type MergeProposal struct {
	OpID string `json:"op_id"`
	// Asker is who merges: a person, or a seat — whose merge authority, like
	// every approval its gate counts, is judged from the chart of the
	// deciding node's epoch. A token bound to nobody merges nothing. A seat's
	// merge may name the turn that asked it, which its push row records; it
	// names no coding run, since a merge is a tool call's, never a box's.
	Asker Principal `json:"asker"`
	// Seen is the code log's position the merge was built at — zero for one
	// that read none. The committer DECIDES AT OR PAST IT, or refuses
	// `behind` about itself, so the runner-up decides.
	Seen  statelog.Position `json:"seen,omitzero"`
	Merge Merge             `json:"merge"`
}

// MergeResult is what became of one merge proposal.
type MergeResult struct {
	// Outcome is applied, pending or unknown for a proposal whose record was
	// appended, and empty for one refused. Applied is answered from the
	// change's row the record wrote.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// MergeID is the push the merge lands as; MergeOID the commit its target
	// moved to, once its record applied.
	MergeID  string `json:"merge_id,omitempty"`
	MergeOID string `json:"merge_oid,omitempty"`
	// Position is where the record landed — for one asked again under its
	// own operation id, the record the ledger found — and, for a not_applied
	// or repo_deleted refusal the log made after the append, where that
	// record lies applying nowhere.
	Position statelog.Position `json:"position,omitzero"`
	// Refusal is why it was refused with no record, Detail its one sentence,
	// and Unmet every precondition of the change's merge gate a merge refused
	// gate_unmet did not meet, each naming whom it waits on.
	Refusal MergeRefusal   `json:"refusal,omitempty"`
	Detail  string         `json:"detail,omitempty"`
	Unmet   []Precondition `json:"unmet,omitempty"`
}

// Validate refuses a proposal no committer could decide ([ErrInvalidProposal],
// naming the field), before it is queued: an operation id this engine does not
// accept; an asker that is no principal, or names a coding run; a Seen that is
// no position on the code log; a repository id that is no UUID in its
// canonical spelling; a merge id that is not [WritePushID] of the operation
// id; a change id that is no bare UUIDv7; a target that is no branch a change
// may name; a method this build does not know; a base, a head or a commit
// that is no whole object id, or a commit that is the base, which moves
// nothing; a fast-forward to anything but the head, or carrying a pack; a merge
// or a squash carrying none, or a commit that is the head; a pack whose row is
// no bundle's ([PackRef.Check]) or past what an engine-built pack may store
// ([MaxWritePackBytes]); an override past [MaxOverrideReasonBytes] or not
// valid text; no node, or no instant it was asked at; a field the committer
// stamps (the principal, the source, Protected and Green) set by the asker;
// and a record past what one proposal may carry ([EntryRoom]).
func (p MergeProposal) Validate() error {
	_, err := p.measure()
	return err
}

// ValidateUnstored is [MergeProposal.Validate] of a merge whose pack is not
// stored yet — its builder's check while the bundle uploads, as
// [Proposal.ValidateUnstored] is a push's: the pack named by the widest
// object key and hash a stored one could take, so a merge that passes here
// passes Validate once its bundle is stored. A pack already stored is the
// caller's mistake: Validate checks one.
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
// as the committer stamps it — its source at the longest a ref's encoding may
// be, every byte past its prefix one JSON writes as six, since the decide
// stamps one the proposal does not carry — encoded, held under [EntryRoom].
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
	if len(encoded) > EntryRoom {
		return 0, fmt.Errorf("%w: merge: %d bytes, past the %d one proposal may take of its record",
			ErrInvalidProposal, len(encoded), EntryRoom)
	}
	return len(encoded), nil
}

// validFields is every rule of [MergeProposal.Validate] but the record's bytes.
func (p MergeProposal) validFields() error {
	if err := statelog.CheckCallerOpID(p.OpID); err != nil {
		return badField("op_id", "%v", err)
	}
	switch why := p.Asker.invalid(); {
	case why != "":
		return fmt.Errorf("%w: asker: %s", ErrInvalidProposal, why)
	case p.Asker.Run.RunID != "":
		return badField("asker.run.run_id", "a merge is a tool call's, whose run is its turn: it names no "+
			"coding run")
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
			// THE BUILD'S OWN BOUND, an engine write's: a git node answers a
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
		return badField("merge.node", "a merge names the git node that built it")
	case m.ReceivedAt.IsZero():
		return badField("merge.received_at", "a merge names when its builder was asked for it")
	case m.Principal != (Principal{}):
		return badField("merge.principal", "the committer stamps who merged it from the asker, and an asker "+
			"sends none")
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
// asker as the record's principal.
func (p MergeProposal) stamped() Merge {
	m := p.Merge
	m.Principal = p.Asker
	return m
}

// RepoID is the repository p is about — the ONE answer the router keys p by,
// the committer queues it under and the estate's seam raises the repository's
// floor for.
func (p MergeProposal) RepoID() string { return p.Merge.RepoID }

// JudgesChart reports a proposal whose decide reads the deciding node's chart,
// which a node holding none, or one confirmed behind the company's
// configuration, never decides: EVERY MERGE, whoever asks — its gate counts an
// approval by who may approve the change, an owner's by the lead chain above
// it and a seat's merge authority by its grant, each the chart's
// ([JudgeGate]), as an enqueue's is ([QueueProposal.JudgesChart]). ONE ANSWER,
// which the committer queues and decides by and the estate declares the
// operation's grant by.
func (p MergeProposal) JudgesChart() bool { return true }

// mergeDecidedRows is the most rows a merge's OWN writes make its apply touch,
// beside the push it lands ([Applier.applyMerge]): the change's row, its live
// queue entry, the target's landing row and its green row.
const mergeDecidedRows = 4

// mergeRecordRows is the most rows one merge record makes its apply write
// before its horizons: its own ([mergeDecidedRows]); its push's row, its two
// commands — the target's move, and the source's deletion with the drop it
// leaves and the old tombstone its sweep may take ([commandRows]) — its pack,
// and a seat's day ([seatDayRows]), which entryRows counts for any agent's
// entry; and the changes its push moves of others' ([changeBook]) — the live
// change the target heads as a source, revised with a revision and an author
// and its queue entry ended, and up to [MaxPushMerges] live changes into the
// target at the merge's commit, each merged with its queue entry ended. Its
// own change the record merges first, so the push finds it merged and moves
// it no further. Far under MaxRecordRows with the horizons' room
// ([recordRows]), which [TestAMergeRecordFitsItsRows] holds — so a merge's
// decide counts no change of its push's, as a push batch's must.
const mergeRecordRows = mergeDecidedRows + 1 + 1 + 3 + 1 + seatDayRows + 4 + 2*MaxPushMerges

// ProposeMerge decides one merge as a batch of its own in the repository's
// queue — the push's queue and bounds ([Committer.Propose]), as a queue's
// write's are ([Committer.ProposeQueue]): a full queue or a node holding its
// bytes is busy at once, a merge whose caller's deadline leaves too little for
// a batch and its answer is busy undecided, a stopping committer answers
// [ErrNotServing] — as does one that cannot judge it, behind the company's
// configuration or holding no chart yet, since the chart judges every merge —
// and a proposal that is not one answers [ErrInvalidProposal]. ctx bounds only
// this caller's wait.
//
// ITS OPERATION ID IS THE PROPOSAL'S OWN, so a merge its builder hands the
// repository's runner-up after this node went silent is the same merge there,
// collapsed by the ledger onto the record that landed. ITS SESSION IS ITS
// SEEN. Applied is answered FROM THE CHANGE'S ROW the record wrote, never from
// the decision; and the node whose committer landed the record — never a
// retry the ledger collapsed onto it — announces it (`change_merged`).
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
	case errors.Is(err, ErrNotServing):
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

// decideMerge is one round's decision of p over the rows tx holds — see this
// file's head, in the order its rules are read. Pure over (the rows, p, seats,
// limits, retention, now): seats is the deciding epoch's chart, which the
// gate judges by; limits its code.limits — the merge_by floor the gate holds
// a merger to, and the size a merge's pack is held to as a push's is;
// retention its code.deleted_retention_days, which a repo_deleted refusal
// names; now the decide's clock, which an upload's age is judged by.
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
		detail, err := goneDetail(ctx, tx, m.RepoID, repo, found, retention)
		if err != nil {
			return mergeDecision{}, err
		}
		return d.refuse(MergeRefusalRepoDeleted, detail)
	case repo.status == RepoStatusArchived:
		return d.refuse(MergeRefusalRepoArchived, fmt.Sprintf("repository %s is archived and read-only "+
			"(code.repos archived: true): nothing merges into it", repo.name))
	case seats.Roster == nil:
		// THE CALLER'S BUG: the committer hands a merge to the repository's
		// next committer before deciding it with no chart.
		return mergeDecision{}, fmt.Errorf("code: a merge on %s reached the decide with no chart to judge it "+
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
	// THE TARGET AS THIS SNAPSHOT HOLDS IT, never as the proposal says it
	// was: the merge's commit is a merge of its base, and the branch moves
	// onto it only where the branch is still there.
	refs, err := refsNamed(ctx, tx, m.RepoID, []string{c.TargetRef, c.SourceRef})
	if err != nil {
		return mergeDecision{}, fmt.Errorf("code: read %s's and %s's tips on %s: %w", c.TargetRef, c.SourceRef,
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
		// THE GATE, every precondition but the change's merge into its
		// target, which the merge's own build proved against its base.
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
		if refusal, why := sourceDeletion(f.Rules, c.SourceRef, refTip(refs, c.SourceRef), p.Asker, grant); refusal != "" {
			return d.refuse(MergeRefusalSourceRefused, fmt.Sprintf("this merge of %s deletes its source %s, "+
				"and a push deleting it is refused %s: %s — merge it keeping its source", named, c.SourceRef,
				refusal, why))
		}
	}
	// THE PACK, held to a push's rules ([working.decide], [working.behind]):
	// its upload young enough for a record to name, the repository's size
	// past it within its bound — half of it for a seat outside the
	// repository's owners — and the repository's live packs under the
	// bound compaction keeps them to, judged last as a push's is.
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

// sourceDeletion is why a merge may not delete its change's source, at tip,
// for merger — the push refusal a push deleting it would meet and its
// sentence — or "": the rules no state decides ([StatelessRefusal]); the
// merger's grant on a source OUTSIDE the seats' namespace ([Grant.RefRule]) —
// a seat's own branch is the merge's to delete whoever merges it (Q4: what
// carried the change ends with it), and a branch anybody else named is
// deleted only by whoever may delete it; and every protection matching it
// ([ProtectionRefusal]), which binds a merge's deletion as it binds a push's.
func sourceDeletion(rules []Protection, source, tip string, merger Principal, grant Grant) (Refusal, string) {
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
		// A RULE THIS BUILD CANNOT READ keeps the branch, FAIL CLOSED:
		// every stored rule passed validation before a record carried it.
		return RefusalProtectedDelete, err.Error()
	}
	if r, rule := ProtectionRefusal(del, protections, merger.Kind); r != "" {
		return r, source + " " + ProtectionDetail(rule, r)
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

// mergeWrite publishes one merge and answers it — the merge's batch of its own,
// under the committer's own lifetime and one CommitBudget, as a queue's write
// is ([Committer.queueWrite]).
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
	// THE SCOPE NAMES WHAT THE DECIDE READS (M5-D33), stated before it runs:
	// the gate reads the change's conversation and facts and the repository's
	// checks beside the repository, so a node holding a deferred record of
	// any of them decides no merge.
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
		c.announce(landed)
	}
}

// mergeAnswer is a merge's answer from the publish's result and the last
// round's decision, and the event its own fresh landing announces — nil for
// any other answer.
func (c *Committer) mergeAnswer(ctx context.Context, p MergeProposal, last mergeDecision, decided bool,
	res statelog.Result, err error) (MergeResult, *types.ChangeMerged, error) {

	if err != nil {
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
		return MergeResult{}, nil, fmt.Errorf("code: the publish of operation %s answered applied with no "+
			"record, and its decide refused nothing", p.OpID)
	case statelog.OutcomePending:
		return MergeResult{Outcome: statelog.OutcomePending, MergeID: p.Merge.MergeID, Position: res.Position},
			nil, nil
	}
	return MergeResult{Outcome: statelog.OutcomeUnknown, MergeID: p.Merge.MergeID}, nil, nil
}

// appliedMerge answers a merge whose record applied at res.Position, FROM THE
// CHANGE'S ROW: merged by that record — at its position, at the commit the
// merge names — or the record wrote what this node does not hold, logged at
// ERROR and answered unknown. Its own fresh landing, never a retry the ledger
// collapsed onto an earlier one, is the event it answers beside.
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
		failed := fmt.Errorf("code: the record of operation %s applied at %s and its change's row could not be "+
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
		// A RETRY COLLAPSED ONTO AN EARLIER LANDING is that landing's, whose
		// call announced it.
		return out, nil, nil
	}
	return out, &types.ChangeMerged{RepoID: m.RepoID, RepoName: repoName, ChangeID: m.ChangeID,
		Number: row.number, Target: row.target, Method: string(m.Method), Commit: row.mergeOID,
		MergedBy: row.mergedBy, MergedByKind: string(row.mergedByKind), Override: row.overrideReason}, nil
}

// announce publishes a merge's event on this node's event stream, bounded by
// [checkEventBudget] under the committer's own lifetime: a stream that does
// not take it is logged, and the change's row still says it merged. Called
// once the merge's answer is handed back, so its asker waits on none of it;
// the repository's next batch waits on it at most that budget, as a checks
// writer's next write of the repository waits on its own announcement — a
// merge is one record a change, and a stream that cannot take an event in
// five seconds is one the code log behind it is no faster on.
func (c *Committer) announce(e *types.ChangeMerged) {
	if c.events == nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.life, checkEventBudget)
	defer cancel()
	ev := events.New(*e, events.TraceContext{})
	ev.Source = c.nodeID
	if err := c.events.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		c.log.WarnContext(ctx, "change_merged_unpublished", "repo", e.RepoID, "change", e.ChangeID,
			"error", err.Error(), "detail", "the activity feed and the event log do not hold this merge; the "+
				"change's row and its push row on the code log still do")
	}
}

// refusedMerge answers a merge whose publish failed — a queue's write's
// mapping ([Committer.refusedQueue]) in a merge's words.
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
	return MergeResult{}, fmt.Errorf("code: merge operation %s: %w", p.OpID, err)
}
