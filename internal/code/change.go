package code

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A CHANGE'S CLOSED SETS — the values code_changes holds (replicated migration
// 0058, the table created again by 0066), each a named string with typed
// constants, Valid, and a function answering the set in order: the one list a
// validator, a schema and the dashboard's contract read.

// ChangeState is where a change (web!42) is in its life.
type ChangeState string

const (
	// ChangeStateDraft is opened and not yet asking to be merged: its rows,
	// its revisions and its reviews are a live change's.
	ChangeStateDraft ChangeState = "draft"
	// ChangeStateOpen asks to be merged into its target.
	ChangeStateOpen ChangeState = "open"
	// ChangeStateClosed was closed without being merged; it may be opened
	// again while its source branch is live.
	ChangeStateClosed ChangeState = "closed"
	// ChangeStateMerged reached its target, how its MergedVia says. Final.
	ChangeStateMerged ChangeState = "merged"
)

// ChangeStates is the closed set, in this order — a change's life, the live
// states first.
func ChangeStates() []ChangeState {
	return []ChangeState{ChangeStateDraft, ChangeStateOpen, ChangeStateClosed, ChangeStateMerged}
}

// Valid reports a state this build knows.
func (s ChangeState) Valid() bool { return slices.Contains(ChangeStates(), s) }

// Live reports a state a change still moves from — draft or open: the one a
// push to its source revises, a push of its target to its head merges, and an
// open on its source branch is refused beside. The two indexes partial on the
// live states (code_changes_open_idx, code_changes_target_idx) hold exactly
// these rows, which TestTheLiveIndexesHoldTheLiveStates holds their predicate
// to.
func (s ChangeState) Live() bool { return s == ChangeStateDraft || s == ChangeStateOpen }

// MergedVia is how a change reached its target.
type MergedVia string

const (
	// MergedViaPush is a change whose target came to hold its head by a
	// push: one that set the target to exactly the change's head, which the
	// push's own apply records, or one that moved the target past it, which
	// the repository's home git node finds and records ([ChangeLanded]).
	MergedViaPush MergedVia = "push"
	// MergedViaDirect is a change a MERGE RECORD merged ([Merge]): its
	// target moved onto the commit its merge built, by a record whose
	// decide held the change to its target's merge gate — or recorded a
	// person's override of it — from one snapshot (ADR-0066).
	//
	// BASE FORMAT, under the condition merge.go's head states: no build
	// that carries the code domain has run without it, and a change row is
	// read by the build that wrote it.
	MergedViaDirect MergedVia = "direct"
)

// MergedVias is the closed set, in the order the ways were added: a merge
// queue's two ([MergedViaQueue], [MergedViaLane]) after a merge record's.
func MergedVias() []MergedVia {
	return []MergedVia{MergedViaPush, MergedViaDirect, MergedViaQueue, MergedViaLane}
}

// Valid reports a value this build knows.
func (m MergedVia) Valid() bool { return slices.Contains(MergedVias(), m) }

// PartyKind is what a change's rows name a principal as, beside its handle —
// its owner (code_changes.owner_kind), an author (code_change_authors.author_kind)
// and who made a revision (code_revisions.pusher_kind): a SEAT by its handle,
// or a PERSON by the human seat they hold. Never a token bound to nobody,
// which opens no change and pushes nothing, so no row names one.
type PartyKind string

const (
	// PartyAgent is a seat of the chart, by its handle.
	PartyAgent PartyKind = "agent"
	// PartyPerson is a person, by the human seat they hold — whichever of
	// their credentials they acted with, which the change's own row keeps
	// beside it for its owner (owner_operator_id).
	PartyPerson PartyKind = "person"
)

// PartyKinds is the closed set, in this order.
func PartyKinds() []PartyKind { return []PartyKind{PartyAgent, PartyPerson} }

// Valid reports a kind this build knows.
func (k PartyKind) Valid() bool { return slices.Contains(PartyKinds(), k) }

// party is p as a change's rows name it: the seat's own handle and
// [PartyAgent], or the human seat a person holds and [PartyPerson] — and ""
// for a principal no row names, a token bound to nobody.
func (p Principal) party() (string, PartyKind) {
	switch {
	case p.Kind == PrincipalAgent && p.Seat != "":
		return p.Seat, PartyAgent
	case p.Kind == PrincipalOperator && p.Seat != "":
		return p.Seat, PartyPerson
	}
	return "", ""
}

// ChangeRefusal is why a repository's committer decided nothing of a write of a
// change's lifecycle ([ChangeProposal]) — a code its asker reads as "<code>:
// <sentence>", the sentence the decide's ([ChangeResult.Detail]) — and why a
// push's options opened no change ([ChangeAnswer], [PushChangeRefusals]). A
// named string, so a code this build does not know is a value rather than a
// panic. NONE IS EVER RECORDED: a refused write appends nothing, and a push
// whose change was refused lands as a push that asked for none, so no record
// of the code log carries one and none takes a versionedFields row.
type ChangeRefusal string

// The codes, in the order the decide's rules reach them (decide_change.go),
// then the two only a push's receiver answers (pushchange.go), and then the
// committer's own.
const (
	// ChangeRefusalRepoDeleted is a write to a repository that is deleted,
	// being purged, purged or not there at all — in the sentence of the
	// state it is in ([RepoGone]).
	ChangeRefusalRepoDeleted ChangeRefusal = "repo_deleted"
	// ChangeRefusalRepoArchived is a write to an archived repository, which
	// is read-only: a change on it is read and written nothing.
	ChangeRefusalRepoArchived ChangeRefusal = "repo_archived"
	// ChangeRefusalChangeNotFound is a write naming a change the repository
	// does not hold.
	ChangeRefusalChangeNotFound ChangeRefusal = "change_not_found"
	// ChangeRefusalNotPermitted is a write its asker may not make: an open
	// by one whose grant does not take a fast-forward of the source branch,
	// and every other by anybody but the change's owner, a principal that
	// maintains the repository and a person (the decide's rule D21).
	ChangeRefusalNotPermitted ChangeRefusal = "not_permitted"
	// ChangeRefusalStale is a write formed against a version of the change
	// it is no longer at ([ChangeProposal.ExpectedVersion]): another write
	// — an edit, or a push that moved its head — landed first. And a landing
	// ([ChangeLanded]) whose change's head, target branch or target's tip
	// moved since its home read them.
	ChangeRefusalStale ChangeRefusal = "stale"
	// ChangeRefusalSameRef is a change whose source and target are one
	// branch.
	ChangeRefusalSameRef ChangeRefusal = "same_ref"
	// ChangeRefusalNoSource is a source branch that is not a live branch of
	// the repository.
	ChangeRefusalNoSource ChangeRefusal = "no_source"
	// ChangeRefusalNoTarget is a target branch that is not a live branch of
	// the repository.
	ChangeRefusalNoTarget ChangeRefusal = "no_target"
	// ChangeRefusalNoDifference is a source branch at the commit its target
	// is at: it holds nothing the target does not.
	ChangeRefusalNoDifference ChangeRefusal = "no_difference"
	// ChangeRefusalChangeExists is a source branch that already heads a live
	// change, which the sentence names: a branch heads one live change at a
	// time, so a push to it revises exactly one.
	ChangeRefusalChangeExists ChangeRefusal = "change_exists"
	// ChangeRefusalChangeClosed is a write that only a live change takes,
	// to a closed one.
	ChangeRefusalChangeClosed ChangeRefusal = "change_closed"
	// ChangeRefusalChangeMerged is a write to a merged change, which stays
	// merged.
	ChangeRefusalChangeMerged ChangeRefusal = "change_merged"

	// ChangeRefusalAmbiguousSource is a push asking for a change that sets
	// more than one branch: a change proposes ONE branch, and which of the
	// push's is the asker's to say, by pushing it alone (pushchange.go). Its
	// receiver's answer, never its committer's.
	ChangeRefusalAmbiguousSource ChangeRefusal = "ambiguous_source"
	// ChangeRefusalBadOption is a push whose options are no change's — a
	// key of the change namespace this engine does not read, a value its
	// field refuses, a key given twice — or a change too wide for the
	// record of the push carrying it ([ParseChangeOptions]). Its receiver's
	// answer, never its committer's.
	ChangeRefusalBadOption ChangeRefusal = "bad_option"

	// ChangeRefusalBusy is a write the repository's committer did not reach
	// in time — its queue full, its asker's attempt run short, or the
	// repository changing under every round the write may take — and
	// decided nothing: it is sent again.
	ChangeRefusalBusy ChangeRefusal = "busy"
	// ChangeRefusalNotApplied is a write whose record was durable on the
	// code log and applied nowhere: a gate drops it on every node.
	ChangeRefusalNotApplied ChangeRefusal = "not_applied"
	// ChangeRefusalUnavailable is the code log refusing the write.
	ChangeRefusalUnavailable ChangeRefusal = "unavailable"
)

// ChangeRefusals is every code, in this order.
func ChangeRefusals() []ChangeRefusal {
	return []ChangeRefusal{
		ChangeRefusalRepoDeleted, ChangeRefusalRepoArchived, ChangeRefusalChangeNotFound,
		ChangeRefusalNotPermitted, ChangeRefusalStale, ChangeRefusalSameRef, ChangeRefusalNoSource,
		ChangeRefusalNoTarget, ChangeRefusalNoDifference, ChangeRefusalChangeExists, ChangeRefusalChangeClosed,
		ChangeRefusalChangeMerged, ChangeRefusalAmbiguousSource, ChangeRefusalBadOption, ChangeRefusalBusy,
		ChangeRefusalNotApplied, ChangeRefusalUnavailable,
	}
}

// Valid reports a code this build knows.
func (r ChangeRefusal) Valid() bool { return slices.Contains(ChangeRefusals(), r) }

// THE RECORDS OF A CHANGE'S LIFECYCLE, one payload an op ([ChangeOps]), each on
// the repository's own subject and decided by its committer alone
// ([Committer.ProposeChange]). Each carries its repository and its change, and
// the fields its asker chose; the committer STAMPS what no asker sends — who
// wrote it, from the proposal's asker, and an open's and a reopen's head, from
// the snapshot it decided in — so a record says what it did with no lookup.
//
// Every field is in the base format, under the condition [OpChangeOpen] states —
// Mentions and Routing among them, under the condition concerns.go states for
// a routing.

// ChangeOpen opens a change: its row, numbered by the apply from the
// repository's next_change, its owner as its first author, and its first
// revision, the source branch's head.
type ChangeOpen struct {
	RepoID string `json:"repo_id"`
	// ChangeID is [OpenChangeID] of the proposal's operation id.
	ChangeID string `json:"change_id"`
	// SourceRef is the branch whose head the change proposes, as a ref
	// (refs/heads/feature), and TargetRef the branch it proposes it into.
	SourceRef string `json:"source_ref"`
	TargetRef string `json:"target_ref"`
	// Head is the source branch's head as the deciding snapshot held it: the
	// change's first revision. STAMPED BY THE COMMITTER; an asker sends none.
	Head string `json:"head,omitempty"`
	// Title is one line, at most [MaxChangeTitleBytes]; Body the
	// description, at most [MaxChangeBodyBytes].
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	// Draft opens it as a draft, not yet asking to be merged.
	Draft bool `json:"draft,omitempty"`
	// Owner is who opened it: the change's owner and first author. STAMPED
	// BY THE COMMITTER from the proposal's asker; an asker sends none.
	Owner Principal `json:"owner,omitzero"`
	// WorkItem is the work item the change carries out, nil for none.
	WorkItem *types.WorkItem `json:"work_item,omitempty"`
	// TurnID and RunID are the turn and the sandbox run that opened it, each
	// "" where there was none — a person's open has neither.
	TurnID string `json:"turn_id,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	// Mentions is the handles the change's title and body name, as its
	// asker resolved them against the chart it acts in, at most
	// [MaxMentions]; the committer holds them to its own chart, and each
	// is told ([ReasonMention]).
	Mentions []string `json:"mentions,omitempty"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// COMMITTER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// ChangeEdit sets what a live change says of itself: its title, its body, the
// work item it carries — each only where set, at least one of them.
type ChangeEdit struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// By is who wrote it. STAMPED BY THE COMMITTER from the proposal's
	// asker; an asker sends none.
	By Principal `json:"by,omitzero"`
	// Title and Body, where set, replace the change's; an empty Body removes
	// its description.
	Title *string `json:"title,omitempty"`
	Body  *string `json:"body,omitempty"`
	// WorkItem, where set, replaces the item the change carries, and
	// ClearWorkItem leaves it carrying none — never both.
	WorkItem      *types.WorkItem `json:"work_item,omitempty"`
	ClearWorkItem bool            `json:"clear_work_item,omitempty"`
	// Mentions is the handles the title or the body it sets names, as
	// [ChangeOpen.Mentions] is.
	Mentions []string `json:"mentions,omitempty"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// COMMITTER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// ChangeDraft makes a live change a draft, or a draft a change asking to be
// merged.
type ChangeDraft struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// By is who wrote it, stamped as [ChangeEdit.By] is.
	By Principal `json:"by,omitzero"`
	// Draft is the state it leaves the change in: draft, or open.
	Draft bool `json:"draft"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// COMMITTER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// ChangeRetarget moves a live change's target to another live branch.
type ChangeRetarget struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// By is who wrote it, stamped as [ChangeEdit.By] is.
	By        Principal `json:"by,omitzero"`
	TargetRef string    `json:"target_ref"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// COMMITTER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// ChangeClose closes a live change without merging it.
type ChangeClose struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// By is who wrote it, stamped as [ChangeEdit.By] is.
	By Principal `json:"by,omitzero"`
	// Reason is why, at most [MaxCloseReasonBytes]; "" for none.
	Reason string `json:"reason,omitempty"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// COMMITTER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// ChangeReopen opens a closed change again, asking to be merged — at the
// source branch's head, which is a revision of its own where the branch moved
// while the change was closed.
type ChangeReopen struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// By is who wrote it, stamped as [ChangeEdit.By] is.
	By Principal `json:"by,omitzero"`
	// Head is the source branch's head as the deciding snapshot held it.
	// STAMPED BY THE COMMITTER; an asker sends none.
	Head string `json:"head,omitempty"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// COMMITTER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// ChangeLanded records a live change MERGED because its target came to hold
// its head: a push moved the target past the head rather than onto it, which
// no push's own apply can see (it would need git), so the repository's home
// git node finds it — the head inside the target's tip — and asks this
// record of the committer. The home's own write, naming no asker, guarded by
// what the home read: the change's revision and head, its target branch, and
// the tip that branch was at. A head that moved since, a retarget, or a target
// moved on is the landing decided against rows the home did not read — stale,
// and its next pass reads the rows again.
type ChangeLanded struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// Revision and HeadOID are the change's head revision the home found
	// inside its target, and its head.
	Revision int64  `json:"revision"`
	HeadOID  string `json:"head_oid"`
	// TargetRef is the change's target branch and TargetOID the tip it was
	// at, holding the head: the commit the change merged at (merge_oid).
	TargetRef string `json:"target_ref"`
	TargetOID string `json:"target_oid"`
	// Node is the git node that found it: the repository's home as it took
	// itself to be.
	Node string `json:"node"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// COMMITTER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// OpenChangeID is the id of the change an open under opID opens: DERIVED from
// the open's operation id (statelog.DeriveOpID, at the instant that id was
// minted), a bare UUIDv7 — so the open asked again under its own id, of the
// repository's runner-up after a committer went silent or by an asker whose
// answer was lost, names the same change, which the operation ledger collapses
// onto the record that landed, and no retry opens a second.
func OpenChangeID(opID string) string {
	at, _ := statelog.OpMintedAt(opID)
	return statelog.DeriveOpID(at, "", "code.change", opID)
}

// ChangeRef is how a change is named to a person: its repository's name, a
// bang and its number — web!42.
func ChangeRef(repo string, number int64) string { return fmt.Sprintf("%s!%d", repo, number) }

// AsTitle is s made a change's title, or "" where it cannot be one: valid
// UTF-8 — a subject in another encoding is no title — on one line, every run
// of spaces and control characters one space, and past [MaxChangeTitleBytes]
// cut on a rune boundary with a marker: a title nobody wrote is a preview of
// the subject, which its commit keeps whole. What an open that names no title
// of its own is titled from — the subject of the commit its branch was
// delivered at — for a push's options (internal/code/host) and for a coding
// run's ask at its collection (internal/engine): ONE RULE for both, so a
// change opened either way from one commit takes one title.
func AsTitle(s string) string {
	if !utf8.ValidString(s) {
		return ""
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return textcut.Within(strings.Join(strings.Fields(s), " "), MaxChangeTitleBytes)
}

// TitleProblem is why s is no change's title, or "": a title is one line of
// text — not empty, at most [MaxChangeTitleBytes] of valid UTF-8 holding no
// control character, so no line break. Exported for the launch that holds a
// coding run's ask to open a change to it before a box costs anything
// (internal/agent/builtin).
func TitleProblem(s string) string {
	switch {
	case strings.TrimSpace(s) == "":
		return "a change is named by a title"
	case len(s) > MaxChangeTitleBytes:
		return fmt.Sprintf("%d bytes, past the %d a title holds (MaxChangeTitleBytes)", len(s), MaxChangeTitleBytes)
	case !utf8.ValidString(s):
		return "not valid UTF-8"
	case strings.ContainsFunc(s, unicode.IsControl):
		return "holds a control character: a title is one line"
	}
	return ""
}

// textProblem is why s is no change's body (bound [MaxChangeBodyBytes]) or
// close reason (bound [MaxCloseReasonBytes]), or "": at most bound bytes of
// valid UTF-8 holding no NUL — line breaks and tabs are the text's own.
func textProblem(s string, bound int, name string) string {
	switch {
	case len(s) > bound:
		return fmt.Sprintf("%d bytes, past the %d a %s holds", len(s), bound, name)
	case !utf8.ValidString(s):
		return "not valid UTF-8"
	case strings.ContainsRune(s, 0):
		return "holds a NUL byte"
	}
	return ""
}

// workItemProblem is why w is no work item a change may carry, or "": a
// backend this build knows, an id — the item's identity — and every field at
// most [MaxProvenanceBytes] of valid UTF-8 with no control character, the
// bound a push's run holds the same labels to, so a stamping site fits them
// through [FitProvenance] as it fits a run's.
func workItemProblem(w types.WorkItem) string {
	if !w.Backend.Valid() {
		return fmt.Sprintf("backend: %q is no tracker a work item lives in", w.Backend)
	}
	if w.ID == "" {
		return "id: a work item is named by its id, its identity in its tracker"
	}
	for _, f := range []struct{ name, value string }{
		{"id", w.ID}, {"key", w.Key}, {"project", w.Project},
	} {
		if why := labelProblem(f.value); why != "" {
			return f.name + ": " + why
		}
	}
	return ""
}

// labelProblem is why s is no label a change's row keeps — a turn's or a run's
// id, a work item's field — or "": at most [MaxProvenanceBytes] of valid UTF-8
// with no control character, the bound a push's run holds its fields to.
func labelProblem(s string) string {
	switch {
	case len(s) > MaxProvenanceBytes:
		return fmt.Sprintf("%d bytes, past the %d one label may hold (MaxProvenanceBytes)", len(s),
			MaxProvenanceBytes)
	case !utf8.ValidString(s):
		return "not valid UTF-8"
	case strings.ContainsFunc(s, unicode.IsControl):
		return "holds a control character"
	}
	return ""
}

// branchProblem is why ref is no branch a change may name as its source or its
// target, or "": a ref a push may move ([ValidRef]), under refs/heads/, and none
// the engine reserves ([reservedRef]) — the branch spelled as the rows spell
// it, in full.
func branchProblem(ref string) string {
	switch {
	case !strings.HasPrefix(ref, branchPrefix):
		return fmt.Sprintf("%q is not a branch: a change names its branches in full, under %s", ref, branchPrefix)
	case !ValidRef(ref):
		return fmt.Sprintf("%q is not a ref", ref)
	case reservedRef(ref):
		return fmt.Sprintf("%q is a ref the engine reserves", ref)
	}
	return ""
}
