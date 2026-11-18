package gitlet

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A CHANGE'S CLOSED SETS: the values code_changes holds (replicated migrations
// 0086 and 0094), each a named string with typed constants, Valid, and a
// function answering the set in order, the one list a validator, a schema and
// the dashboard's contract read.

// ChangeState is where a change (web!42) is in its life.
type ChangeState string

const (
	// ChangeStateDraft is opened but not yet asking to be merged; otherwise a
	// live change.
	ChangeStateDraft ChangeState = "draft"
	// ChangeStateOpen asks to be merged into its target.
	ChangeStateOpen ChangeState = "open"
	// ChangeStateClosed was closed without being merged; it may be opened
	// again while its source branch is live.
	ChangeStateClosed ChangeState = "closed"
	// ChangeStateMerged reached its target, how its MergedVia says. Final.
	ChangeStateMerged ChangeState = "merged"
)

// ChangeStates is the closed set in life order, the live states first.
func ChangeStates() []ChangeState {
	return []ChangeState{ChangeStateDraft, ChangeStateOpen, ChangeStateClosed, ChangeStateMerged}
}

// Valid reports a state this build knows.
func (s ChangeState) Valid() bool { return slices.Contains(ChangeStates(), s) }

// Live reports a state a change still moves from, draft or open: a push to
// its source revises it, its target reaching its head merges it, and an open
// on its source branch is refused beside it. The partial indexes
// code_changes_open_idx and code_changes_target_idx hold exactly these rows
// (TestTheLiveIndexesHoldTheLiveStates).
func (s ChangeState) Live() bool { return s == ChangeStateDraft || s == ChangeStateOpen }

// MergedVia is how a change reached its target.
type MergedVia string

const (
	// MergedViaPush is a change whose target came to hold its head by a
	// push: set onto the head (recorded by the push's own apply) or moved past
	// it (found and recorded by the home Gitlet node, [ChangeLanded]).
	MergedViaPush MergedVia = "push"
	// MergedViaDirect is a change a MERGE RECORD merged ([Merge]): its
	// target moved onto the merge's commit, decided against the target's
	// merge gate, or a person's override of it, from one snapshot (ADR-0096).
	//
	// BASE FORMAT, as merge.go's head says: a change row is read by the build
	// that wrote it.
	MergedViaDirect MergedVia = "direct"
)

// MergedVias is the closed set, in the order the ways were added: a merge
// queue's ([MergedViaQueue]) after a merge record's.
func MergedVias() []MergedVia { return []MergedVia{MergedViaPush, MergedViaDirect, MergedViaQueue} }

// Valid reports a value this build knows.
func (m MergedVia) Valid() bool { return slices.Contains(MergedVias(), m) }

// PartyKind is how a change's rows name a principal beside its handle
// (code_changes.owner_kind, code_change_authors.author_kind,
// code_revisions.pusher_kind): a SEAT by its handle, or a PERSON by the human
// seat they hold. Never a token bound to nobody, which opens and pushes nothing.
type PartyKind string

const (
	// PartyAgent is a seat of the chart, by its handle.
	PartyAgent PartyKind = "agent"
	// PartyPerson is a person, by the human seat they hold whichever
	// credential they acted with; the change's row keeps its owner's
	// (owner_operator_id).
	PartyPerson PartyKind = "person"
)

// PartyKinds is the closed set, in this order.
func PartyKinds() []PartyKind { return []PartyKind{PartyAgent, PartyPerson} }

// Valid reports a kind this build knows.
func (k PartyKind) Valid() bool { return slices.Contains(PartyKinds(), k) }

// party is p as a change's rows name it, or "" for a token bound to nobody,
// which no row names.
func (p Principal) party() (string, PartyKind) {
	switch {
	case p.Kind == PrincipalAgent && p.Seat != "":
		return p.Seat, PartyAgent
	case p.Kind == PrincipalOperator && p.Seat != "":
		return p.Seat, PartyPerson
	}
	return "", ""
}

// ChangeRefusal is why a repository's committer decided nothing of a change
// lifecycle write ([ChangeProposal]), read as "<code>: <sentence>" with the
// sentence in [ChangeResult.Detail], and why a push's options opened no change
// ([ChangeAnswer], [PushChangeRefusals]). NONE IS EVER RECORDED: a refused
// write appends nothing and a push whose change was refused lands as one that
// asked for none, so no code log record carries one and none takes a
// versionedFields row.
type ChangeRefusal string

// The codes, in the order the decide's rules reach them (decide_change.go),
// then the two only a push's receiver answers (pushchange.go), and then the
// committer's own.
const (
	// ChangeRefusalRepoDeleted is a write to a repository deleted, being
	// purged, purged or absent, in the sentence of its state ([RepoGone]).
	ChangeRefusalRepoDeleted ChangeRefusal = "repo_deleted"
	// ChangeRefusalRepoArchived is a write to an archived, read-only
	// repository.
	ChangeRefusalRepoArchived ChangeRefusal = "repo_archived"
	// ChangeRefusalRepoImporting is a write to a repository whose import is
	// not done ([ImportingDetail]).
	ChangeRefusalRepoImporting ChangeRefusal = "repo_importing"
	// ChangeRefusalRepoMirrored is a write to a PULL MIRROR, which moves only
	// by syncs ([MirroredDetail]): every write but a close and a landing,
	// which move no ref.
	ChangeRefusalRepoMirrored ChangeRefusal = "repo_mirrored"
	// ChangeRefusalChangeNotFound is a write naming a change the repository
	// does not hold.
	ChangeRefusalChangeNotFound ChangeRefusal = "change_not_found"
	// ChangeRefusalNotPermitted is a write its asker may not make: an open
	// by one whose grant does not take a fast-forward of the source branch,
	// and any other by anybody but the change's owner, a maintainer of the
	// repository or a person (the decide's rule D21).
	ChangeRefusalNotPermitted ChangeRefusal = "not_permitted"
	// ChangeRefusalStale is a write formed against a version the change has
	// left ([ChangeProposal.ExpectedVersion]): an edit or a head-moving push
	// landed first. Also a landing ([ChangeLanded]) whose head, target branch
	// or target tip moved since its home read them.
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
	// ChangeRefusalNoDifference is a source branch at its target's commit.
	ChangeRefusalNoDifference ChangeRefusal = "no_difference"
	// ChangeRefusalChangeExists is a source branch already heading a live
	// change, which the sentence names: a branch heads one live change at a
	// time, so a push to it revises exactly one.
	ChangeRefusalChangeExists ChangeRefusal = "change_exists"
	// ChangeRefusalChangeClosed is a write only a live change takes, to a
	// closed one.
	ChangeRefusalChangeClosed ChangeRefusal = "change_closed"
	// ChangeRefusalChangeMerged is a write to a merged change, which stays
	// merged.
	ChangeRefusalChangeMerged ChangeRefusal = "change_merged"

	// ChangeRefusalAmbiguousSource is a push asking for a change while
	// setting several branches: a change proposes ONE, which the asker names
	// by pushing it alone (pushchange.go). The receiver's answer, never the
	// committer's.
	ChangeRefusalAmbiguousSource ChangeRefusal = "ambiguous_source"
	// ChangeRefusalBadOption is a push whose options are no change's (an
	// unread key of the change namespace, a value its field refuses, a key
	// given twice) or a change too wide for its push's record
	// ([ParseChangeOptions]). The receiver's answer, never the committer's.
	ChangeRefusalBadOption ChangeRefusal = "bad_option"

	// ChangeRefusalBusy is a write the committer did not reach in time (queue
	// full, the asker's attempt run short, or the repository changing under
	// every round) and decided nothing: it is sent again.
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
		ChangeRefusalRepoDeleted, ChangeRefusalRepoArchived, ChangeRefusalRepoImporting,
		ChangeRefusalRepoMirrored, ChangeRefusalChangeNotFound,
		ChangeRefusalNotPermitted, ChangeRefusalStale, ChangeRefusalSameRef, ChangeRefusalNoSource,
		ChangeRefusalNoTarget, ChangeRefusalNoDifference, ChangeRefusalChangeExists, ChangeRefusalChangeClosed,
		ChangeRefusalChangeMerged, ChangeRefusalAmbiguousSource, ChangeRefusalBadOption, ChangeRefusalBusy,
		ChangeRefusalNotApplied, ChangeRefusalUnavailable,
	}
}

// Valid reports a code this build knows.
func (r ChangeRefusal) Valid() bool { return slices.Contains(ChangeRefusals(), r) }

// THE RECORDS OF A CHANGE'S LIFECYCLE, one payload per op ([ChangeOps]), each
// on the repository's subject and decided by its committer alone
// ([Committer.ProposeChange]). The committer STAMPS what no asker sends (the
// writer, from the proposal's asker; an open's and a reopen's head, from the
// deciding snapshot; whom they were made for, from the proposal), so a record
// says what it did with no lookup.
//
// Every field is base format, as [OpChangeOpen] is (Mentions and Routing too,
// per concerns.go's head), but the version-2 WorkingFor of an open, a reopen
// and a routing ([ChangeRouting.WorkingFor]).

// ChangeOpen opens a change: its row, numbered by the apply from the
// repository's next_change, its owner as first author, and its first revision
// at the source branch's head.
type ChangeOpen struct {
	RepoID string `json:"repo_id"`
	// ChangeID is [OpenChangeID] of the proposal's operation id.
	ChangeID string `json:"change_id"`
	// SourceRef is the branch whose head the change proposes, as a ref
	// (refs/heads/feature), and TargetRef the branch it proposes it into.
	SourceRef string `json:"source_ref"`
	TargetRef string `json:"target_ref"`
	// Head is the source branch's head in the deciding snapshot, the first
	// revision. STAMPED BY THE COMMITTER; an asker sends none.
	Head string `json:"head,omitempty"`
	// Title is one line, at most [MaxChangeTitleBytes]; Body the
	// description, at most [MaxChangeBodyBytes].
	Title string `json:"title" untrusted:"name"`
	Body  string `json:"body,omitempty" untrusted:"fence"`
	// Draft opens it as a draft, not yet asking to be merged.
	Draft bool `json:"draft,omitempty"`
	// Owner is who opened it: the change's owner and first author. STAMPED
	// BY THE COMMITTER from the proposal's asker; an asker sends none.
	Owner Principal `json:"owner,omitzero"`
	// WorkingFor is whom an agent's open was made for (ADR-0109), whatever
	// the record wakes: its proposal's carrier ([ChangeProposal.WrittenFor]),
	// STAMPED BY THE COMMITTER; an asker sends none. nil for the seat's own
	// work and for every open no agent made. The apply keeps it on revision
	// 1's row (code_revisions.working_for), where that revision's fact records
	// and a failed check of its head read it back ([revisionPusher]).
	//
	// VERSION 2, as every payload's working_for is ([versionedFields]).
	WorkingFor *types.WorkingFor `json:"working_for,omitempty"`
	// WorkItem is the work item the change carries out, nil for none.
	WorkItem *types.WorkItem `json:"work_item,omitempty"`
	// TurnID and RunID are the turn and the executor's launch that opened
	// it, each "" where there was none (a person's open has neither).
	TurnID string `json:"turn_id,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	// Mentions is the handles the title and body name, as the asker resolved
	// them against its chart, at most [MaxMentions]; the committer holds them
	// to its own chart and tells each ([ReasonMention]). THE ASKER'S, never
	// the record's: the committer moves them into the routing's mentions,
	// their one copy, and strips them from the record (withRouting).
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
	// Mentions is the handles the title or body it sets names, as
	// [ChangeOpen.Mentions] is.
	Mentions []string `json:"mentions,omitempty"`
	// Routing is stamped as [ChangeOpen.Routing] is.
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
	// Routing is stamped as [ChangeOpen.Routing] is.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// ChangeRetarget moves a live change's target to another live branch.
type ChangeRetarget struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// By is who wrote it, stamped as [ChangeEdit.By] is.
	By        Principal `json:"by,omitzero"`
	TargetRef string    `json:"target_ref"`
	// Routing is stamped as [ChangeOpen.Routing] is.
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
	// Routing is stamped as [ChangeOpen.Routing] is.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// ChangeReopen opens a closed change again, asking to be merged, at the
// source branch's head: a revision of its own where the branch moved while
// the change was closed.
type ChangeReopen struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// By is who wrote it, stamped as [ChangeEdit.By] is.
	By Principal `json:"by,omitzero"`
	// Head is stamped as [ChangeOpen.Head] is.
	Head string `json:"head,omitempty"`
	// WorkingFor is whom an agent's reopen was made for, as
	// [ChangeOpen.WorkingFor] is, kept on the revision a reopen adds where the
	// source moved while the change was closed and read back from it the same.
	//
	// VERSION 2, as every payload's working_for is ([versionedFields]).
	WorkingFor *types.WorkingFor `json:"working_for,omitempty"`
	// Routing is stamped as [ChangeOpen.Routing] is.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// ChangeLanded records a live change MERGED because a push moved its target
// past its head rather than onto it, which no push's apply can see (it needs
// git): the repository's home Gitlet node finds the head inside the target's
// tip and asks the committer for this record. The home's own write, naming no
// asker, guarded by what it read (the change's revision and head, its target
// branch and that branch's tip): any of them moved since is stale, and the
// home's next pass reads the rows again. Its routing's actor is whoever pushed
// the move that MERGED the change (PushID), never whoever last moved the
// target, which may move again, in the same group-commit record or a later
// one, before the home reads it.
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
	// Node is the Gitlet node that found it, the repository's home as it took
	// itself to be.
	Node string `json:"node"`
	// PushID and PushPosition name the push that MERGED the change, found by
	// the home's walk of TargetRef's moves (internal/gitlet/home's merger):
	// the latest move at or below TargetOID's record whose old commit did not
	// hold HeadOID, so the target held the head from it through the tip.
	// Containment is git's to say, so the home finds the push
	// and the committer holds it to the rows ([landingPusher]); its pusher is
	// the landing's actor, and the routing carries whom the push's row says it
	// was made for. Both zero where the walk named no push: the head predates
	// every move the rows keep, or the walk's bound.
	//
	// BASE FORMAT, as [OpChangeLanded] is.
	PushID       string            `json:"push_id,omitempty"`
	PushPosition statelog.Position `json:"push_position,omitzero"`
	// Routing is stamped as [ChangeOpen.Routing] is.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// OpenChangeID is the id of the change an open under opID opens, a bare
// UUIDv7 DERIVED from the operation id (statelog.DeriveOpID at its mint
// instant): an open retried under its own id, by the runner-up committer or an
// asker whose answer was lost, names the same change, which the operation
// ledger collapses onto the record that landed, so no retry opens a second.
func OpenChangeID(opID string) string {
	at, _ := statelog.OpMintedAt(opID)
	return statelog.DeriveOpID(at, "", "code.change", opID)
}

// ChangeRef is how a change is named to a person: its repository's name, a
// bang and its number — web!42.
func ChangeRef(repo string, number int64) string { return fmt.Sprintf("%s!%d", repo, number) }

// changeAddress is the dashboard's address of a change, the route's own
// placeholders standing for the repository's name and the change's number.
//
// ONE SPELLING, WRITTEN AS THE ROUTE: dashboard/src/app/links.test.ts holds
// every dashboard address literal in the engine's Go to the dashboard's
// router, so a moved or renamed change page fails that suite instead of git
// printing a Not Found link under every push that opened a change.
const changeAddress = "#/gitlet/repos/{name}/changes/{n}"

// ChangeAddress is the dashboard's address of change number on repo
// (`#/gitlet/repos/web/changes/42`), the page web!42 is the short form of. The
// name is path-escaped, which the dashboard's router undoes, so the address
// holds whatever the repository-name grammar comes to admit.
func ChangeAddress(repo string, number int64) string {
	return strings.NewReplacer("{name}", url.PathEscape(repo), "{n}", strconv.FormatInt(number, 10)).
		Replace(changeAddress)
}

// DeliveredTitle is the title of a change opened from branch (a short name)
// without one of its own: subject (head's, "" where unread) made a title
// ([AsTitle]); failing that, the branch's name made one the same way; failing
// that, "commit" and head's abbreviated id. ALWAYS A TITLE ([TitleProblem]
// finds nothing in it), because neither opener is refused for its title: a
// push's options (internal/gitlet/host), whose change never fails its push,
// and an executor's ask at collection (internal/engine). One rule for both,
// so one commit takes one title either way.
//
// THE BRANCH'S NAME IS MADE A TITLE TOO, never taken raw: git and
// gitref.ValidRef admit names past [MaxChangeTitleBytes] and C1 controls
// (U+0085, a line break to a title), and a name of only those is no title.
func DeliveredTitle(subject, branch, head string) string {
	if title := AsTitle(subject); title != "" {
		return title
	}
	if title := AsTitle(branch); title != "" {
		return title
	}
	return "commit " + head[:min(len(head), 7)]
}

// AsTitle is s made a change's title, or "" where it cannot be one: valid
// UTF-8 (a subject in another encoding is none), one line, every run of
// spaces and control characters one space, and past [MaxChangeTitleBytes] cut
// on a rune boundary with a marker, since an unwritten title previews a
// subject its commit keeps whole. Exported for revert_change's "Revert
// <repo>!<n>" (internal/gitlet/host), whose repository name has no bound
// below the title's.
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

// TitleProblem is why s is no change's title, or "": one line, not empty, at
// most [MaxChangeTitleBytes] of valid UTF-8 with no control character.
// Exported for internal/agent/builtin, which holds an executor's ask to open a
// change to it before a box costs anything.
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

// workItemProblem is why w is no work item a change may carry, or "": a known
// backend, an id, and every field a label ([labelProblem]), the bound a push's
// run holds its labels to, so a stamping site fits them through
// [FitProvenance] as it fits a run's.
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
