package code

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REVISION'S FACTS, RECORDED: what one revision of a live change is against
// the branch it would merge into — the base it is measured from, the id of the
// change it makes, the files it touches — read once by the repository's HOME
// git node (internal/code/query's revision_facts, a function of two commits
// alone) and recorded on the change as a [Revise]: a record on the change's own
// subject ([KindChange]), which the repository's committing data node decides
// and publishes for the home, as it does every write of a change's
// conversation ([ChangeWriter]). A git node never publishes it. Every reader of
// a revision's facts after that reads the rows (code_revision_facts), never
// git.
//
// A HEAD ITS TARGET HOLDS IS NO REVISION TO MEASURE: whatever moved the target
// there merged the change, and there is nothing left of it to touch or to
// review. Its facts are never recorded — a revise whose base is its own head
// is refused before anything is read — because what the home owes such a
// change is its landing, a record on the repository's subject, where its
// lifecycle is decided; a facts row beside it would be a second record of one
// finding, and a due listing that had to tell a revision measured from one
// waiting to be landed.
//
// # Path owners, matched on the home
//
// The repository's PATH OWNERS ([PathOwner]) are asked to review a revision
// that touches their paths. Matching is the HOME's: a glob ([gitref.PathGlob])
// against every touched path — every owner, where the revision touches all —
// is up to MaxPathOwners × MaxPathOwnerPaths globs against
// MaxTouchedPaths paths, which no decide pays on every round it may take. So
// the home matches ([MatchOwners]) and sends the owners it matched beside the
// DIGEST of the owner rows it matched them against ([OwnersDigest]), and the
// decide recomputes the digest from its own snapshot's rows: another digest is
// owners that changed since the home read them, and the facts are SUPERSEDED,
// recorded nowhere, for the home's next pass to match again. What the decide
// does with the matched owners is a judgment of the chart, which no home
// makes: a unit owner is asked of its EFFECTIVE LEAD, a seat owner of itself;
// one who may not review the repository — a seat below review — or who is an
// author of the change, whose review of their own change counts for nothing,
// is SKIPPED, the record naming each skipped owner and why; an owner whose ask
// the change holds already — waiting, answered by their review, ended with the
// change or withdrawn by somebody's word on this change, which no rule
// overrides — is left as it is; and the change asks at
// most MaxReviewersPerChange at once, past which the rest are skipped too. A
// path owner GRANTS NOTHING: its ask is a review request, and whether its
// review counts is the reviewer's grant's, as any reviewer's.
//
// # The guard
//
// Facts are of the change's HEAD revision against its CURRENT target, and a
// revise is decided only while they still are: one of an earlier revision, of
// a head the change has moved from, against a branch the change no longer
// merges into, or no later than the facts it holds of that revision against
// that branch — the same tip read twice, by a home that ran its pass again or
// two homes a split presence view each called home — is SUPERSEDED. A retarget
// is new facts of the same revision against another branch, which replace the
// row.
//
// # Whom the facts concern, for the wakes
//
// A revise records whom its revision concerns beside the asks it makes, each
// stamped by the writer from the snapshot and the chart, for the wakes a later
// record's routing derives from it: the reviewers whose standing verdict asks
// for changes on an earlier revision — asked to look again (re_review) — on
// the revision's first facts; and, on the change's first facts, where nobody
// who may review it is asked, the lead of the repository's owner — its unit's
// effective lead, or the lead of the unit an owning seat sits in — so a change
// nobody is asked to review still reaches somebody (lead_fallback).
//
// # A revision's classes ride its facts
//
// What the home read of a revision includes its CLASSES, two kinds of them.
// The TREES of its base and its head ([Revise.BaseTree], [Revise.HeadTree]):
// two revisions holding the same pair hold the same code, byte for byte, so
// an approval of the one carries to the other however their histories differ
// and whatever their patch ids say — [ChangeClassNoCodeChange], a revision
// carry-forward kind ([ReadinessOf]), where an omitted patch id carries
// nothing. The trees are recorded rather than the class itself, because the
// class is a relation between two revisions: recorded, it would be judged by
// a home against the rows it read of the revision before, which a late
// record of that revision may replace after; compared, it is judged by
// whoever reads the two rows, in one snapshot, as two patch ids are. And the
// PATH classes of the paths it touches ([RevisionClasses]): what each touched
// Go file's change is to the language's own lexer ([ClassifyGo]), within the
// bounds a revision's classification keeps to ([MaxClassedBytes]) — what a
// merge gate's exemptions read ([MergeGate.ApprovalsUnless],
// [HumanPath.Unless]).

// SkipReason is why a revise asked no review of a path owner whose globs its
// revision touched (code_revision_facts.skipped). A closed set ([SkipReasons]).
type SkipReason string

const (
	// SkipNoLead is a unit owner with no effective lead: nobody leads it, so
	// nobody is asked for it.
	SkipNoLead SkipReason = "no_lead"
	// SkipNoSeat is an owner that resolved to no seat of the chart: a seat
	// owner the chart no longer holds, or a unit's lead it does not.
	SkipNoSeat SkipReason = "no_seat"
	// SkipBelowReview is a seat that may not review the repository: its
	// grant is below review, so its review would count for nothing.
	SkipBelowReview SkipReason = "below_review"
	// SkipAuthor is an author of the change, whose review of their own change
	// counts for nothing.
	SkipAuthor SkipReason = "author"
	// SkipTooManyReviewers is an owner past what one change asks at once
	// (MaxReviewersPerChange).
	SkipTooManyReviewers SkipReason = "too_many_reviewers"
)

// SkipReasons is the closed set, in the order the decide reaches them.
func SkipReasons() []SkipReason {
	return []SkipReason{SkipNoLead, SkipNoSeat, SkipBelowReview, SkipAuthor, SkipTooManyReviewers}
}

// Valid reports a reason this build knows.
func (r SkipReason) Valid() bool { return slices.Contains(SkipReasons(), r) }

// SkippedOwner is a path owner a revise asked no review of, the seat it
// resolved to — "" where it resolved to none — and why.
type SkippedOwner struct {
	Owner  Owner      `json:"owner"`
	Handle string     `json:"handle,omitempty"`
	Why    SkipReason `json:"why"`
}

// Revise records one revision's facts on its change: what the revision's head
// is against its target's tip, as the repository's home git node read them,
// and the path owners whose globs its touched paths match.
//
// Every field is in the base format, under the condition [OpRevise] states.
type Revise struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// N is the revision, and HeadOID its head: the change's head revision as
	// the home read it, which the decide holds it to.
	N       int64  `json:"n"`
	HeadOID string `json:"head_oid"`
	// TargetRef is the change's target branch, TargetOID the tip the facts
	// were read against and TargetPosition the position of the record that
	// moved the branch there.
	TargetRef      string            `json:"target_ref"`
	TargetOID      string            `json:"target_oid"`
	TargetPosition statelog.Position `json:"target_position"`
	// BaseOID is the merge base the change is measured from, "" for a history
	// that shares no commit with its target's — and never the head itself,
	// which its target would hold (this file's head).
	BaseOID string `json:"base_oid,omitempty"`
	// BaseTree and HeadTree are the trees of the base and of the head — the
	// empty tree's id for a history that shares no commit with its target's —
	// which a later revision holding the same pair holds the same code as
	// (this file's head, "A revision's classes ride its facts").
	//
	// BASE FORMAT, under the condition [OpRevise] states: fields the op
	// gained after it landed, so once a build without them has run they take
	// {Name: "Revise.BaseTree", Since: 2, Op: "revise", Path:
	// []string{"mutation", "base_tree"}} and {Name: "Revise.HeadTree", Since:
	// 2, Op: "revise", Path: []string{"mutation", "head_tree"}}, which an
	// older build defers the record by rather than record facts no approval
	// could be carried by.
	BaseTree string `json:"base_tree"`
	HeadTree string `json:"head_tree"`
	// PatchID is `git patch-id --verbatim` of the cumulative diff from the
	// base, "" for a revision that changes nothing and where PatchIDOmitted.
	PatchID        string `json:"patch_id,omitempty"`
	PatchIDOmitted bool   `json:"patch_id_omitted,omitempty"`
	// Touched is every file the change touches, unless TouchesAll — read as
	// every path, with no list.
	Touched    []string `json:"touched"`
	TouchesAll bool     `json:"touches_all,omitempty"`
	// Classes is the path classes of the touched files' changes, each named
	// by its place in Touched ([RevisionClasses]).
	//
	// BASE FORMAT, under the condition [OpRevise] states: a field the op
	// gained after it landed, so once a build without it has run it takes
	// {Name: "Revise.Classes", Since: 2, Op: "revise", Path:
	// []string{"mutation", "classes"}}, which an older build defers the
	// record by rather than record facts that class nothing. Omitted where
	// it is the zero value — nothing classed, the classification incomplete
	// — which an older build's facts mean already.
	Classes RevisionClasses `json:"classes,omitzero"`
	// Node is the git node that recorded the facts: the repository's home as
	// it took itself to be, which asked for them.
	Node string `json:"node"`
	// RequestedOwners is the path owners one of whose globs matched a touched
	// path — every owner, where the revision touches all — as [MatchOwners]
	// answers them, and OwnersDigest the digest of the owner rows they were
	// matched against ([OwnersDigest]), which the decide holds its own
	// snapshot's to.
	RequestedOwners []Owner `json:"requested_owners,omitempty"`
	OwnersDigest    string  `json:"owners_digest"`

	// Asked is the reviewers the record asks for the matched owners, each by
	// the seat it resolved to and its kind; SkippedOwners the matched owners
	// it asked no review of, and why. STAMPED BY THE WRITER from its snapshot
	// and its chart; the home sends neither.
	Asked         []Reviewer     `json:"asked,omitempty"`
	SkippedOwners []SkippedOwner `json:"skipped_owners,omitempty"`
	// ReReview is the reviewers whose standing verdict asks for changes on an
	// earlier revision, asked to look again; LeadFallback the lead of the
	// repository's owner a change nobody who may review it is asked reaches.
	// STAMPED BY THE WRITER, for the wakes a record's routing derives.
	ReReview     []Reviewer `json:"re_review,omitempty"`
	LeadFallback *Reviewer  `json:"lead_fallback,omitempty"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// WRITER from its snapshot and chart; an asker sends none.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// MaxReReviewers is the most reviewers one revise asks to look again: the
// reviewers whose standing verdict on an earlier revision asks for changes.
// SIXTY-FOUR, as many as a change's routing names watchers: a reviewer
// standing on request_changes is one a change has asked, or one who reviewed
// it unasked, and a change reaching more than sixty-four of either has an
// audience no wake is the way to — the rest read it in their listings.
const MaxReReviewers = 64

// RevisionClasses is what the changes of the paths a revision touches are to
// a lexer ([ClassifyGo]): each touched path a class holds of, in the list of
// its NARROWEST class — a whitespace_only change is a comment_only one too,
// and is listed as whitespace_only alone.
//
// A PATH IS NAMED BY ITS PLACE in the facts' touched list, counted from 0,
// never by its name again: the facts are one page beside a touched list of up
// to [MaxTouchedBytes] (MaxGitAnswerBytes), and a class list naming every
// path a second time — a thousand Go files reformatted — would take the
// page twice over, where a thousand places take some four kilobytes.
//
// COMPLETE IS WHAT LETS A LIST BE READ AS EVERY PATH. A path no list names
// has no class: one no lexer reads (another language, a file added, deleted
// or changed in its mode, a symbolic link, a submodule), one whose versions
// ClassifyGo gives none — and, where the classification is NOT complete, one
// it never reached: past [MaxClassedBytes] of Go a revision's classification
// lexes, past the read's budget of git, and every path of a revision that
// touches all, which lists none. So a reader exempting a change because every
// path it touches is of a class requires Complete: incomplete, an empty list
// would read as every path classed.
type RevisionClasses struct {
	WhitespaceOnly []int `json:"whitespace_only,omitempty"`
	CommentOnly    []int `json:"comment_only,omitempty"`
	Complete       bool  `json:"complete,omitempty"`
}

// IsZero reports classes that say nothing — no path classed, the
// classification incomplete — which a record, an answer and a row omit
// (encoding/json's omitzero asks it), an empty list and none alike.
func (c RevisionClasses) IsZero() bool {
	return len(c.WhitespaceOnly) == 0 && len(c.CommentOnly) == 0 && !c.Complete
}

// classesProblem refuses classes as facts touching touched — or all — carry
// them: each list ascending, each place once and of a touched path, no path
// in both lists, and neither a list nor completeness for a revision that
// touches all, which lists no path to name. Which paths a class may hold of
// is the lexer's, on the git node, and no rule here: a build that lexes
// another language classes its paths, and an older writer deciding its
// record must not refuse them.
func classesProblem(c RevisionClasses, touched []string, all bool) error {
	if all && (c.Complete || len(c.WhitespaceOnly) > 0 || len(c.CommentOnly) > 0) {
		return badField("revise.classes", "a revision that touches all lists no path, so its classes name none "+
			"and are never complete")
	}
	classed := make(map[int]bool, len(c.WhitespaceOnly)+len(c.CommentOnly))
	for _, list := range []struct {
		field  string
		places []int
	}{{"revise.classes.whitespace_only", c.WhitespaceOnly}, {"revise.classes.comment_only", c.CommentOnly}} {
		for i, at := range list.places {
			field := fmt.Sprintf("%s[%d]", list.field, i)
			switch {
			case at < 0 || at >= len(touched):
				return badField(field, "%d is no place in the touched list of %d paths", at, len(touched))
			case i > 0 && at <= list.places[i-1]:
				return badField(field, "%d is not after %d: a class names its paths in the touched list's order, "+
					"each once", at, list.places[i-1])
			case classed[at]:
				return badField(field, "%s is classed already: a path is listed under its narrowest class alone",
					touched[at])
			}
			classed[at] = true
		}
	}
	return nil
}

// OwnersDigest is the digest of a repository's path owners: the SHA-256, in
// lowercase hex, of their JSON in the ONE form a record carries them in
// ([CanonicalPathOwners]) — none at all is the digest of an empty list. The
// home sends the digest of the owners it matched a revision's touched paths
// against, and the decide recomputes it from its own snapshot's rows: two
// digests apart are owners that changed between the two reads.
func OwnersDigest(owners []PathOwner) string {
	canonical := CanonicalPathOwners(owners)
	if canonical == nil {
		canonical = []PathOwner{}
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		// A list of strings and named strings always encodes; reaching here
		// is a broken encoder, which no digest could be taken past.
		panic(fmt.Sprintf("code: encoding path owners: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// MatchOwners is which of a repository's path owners a revision touching
// touched owns — every owner one of whose globs matches a touched path, and
// every owner where the revision touches all — in the order owners holds them
// (the canonical one, [CanonicalPathOwners]), and the digest of owners
// ([OwnersDigest]) a revise sends beside them. A glob gitref cannot read — which
// no row holds, since every settings record is validated — owns nothing.
//
// ON THE HOME, NEVER IN A DECIDE: up to MaxPathOwners × MaxPathOwnerPaths globs
// against MaxTouchedPaths paths, each owner's match ending at its first hit.
func MatchOwners(owners []PathOwner, touched []string, touchesAll bool) ([]Owner, string) {
	canonical := CanonicalPathOwners(owners)
	var out []Owner
	for _, po := range canonical {
		if touchesAll || ownsAny(po.Paths, touched) {
			out = append(out, po.Owner)
		}
	}
	return out, OwnersDigest(canonical)
}

// ownsAny reports whether one of globs matches one of paths.
func ownsAny(globs, paths []string) bool {
	for _, spelled := range globs {
		glob, err := gitref.ParsePathGlob(spelled)
		if err != nil {
			continue
		}
		for _, p := range paths {
			if glob.Match(p) {
				return true
			}
		}
	}
	return false
}

// validFields holds a revise to [ConversationWrite.Validate]'s rules: a
// revision from 1; whole object ids for the head, the target's tip, the
// base's and the head's trees, and the base and the patch id where set; a
// base that is not the head, which its target would hold (this file's head);
// the target a branch a change may name, and its tip's position one on the
// code log; an omitted id with no id; a touched list within the bounds the
// facts keep it to, beside no word that it touches all, of clean paths; its
// classes naming its touched paths ([classesProblem]); the node a node id
// config admits; the matched owners each a unit or a seat, sorted and once,
// at most MaxPathOwners; a digest of SHA-256's width in lowercase hex; and no
// field the writer stamps.
func (r *Revise) validFields() error {
	if r.N < 1 {
		return badField("revise.n", "%d; a change's revisions count from 1", r.N)
	}
	for _, id := range []struct {
		field, id string
		empty     bool
	}{
		{"revise.head_oid", r.HeadOID, false}, {"revise.target_oid", r.TargetOID, false},
		{"revise.base_oid", r.BaseOID, true}, {"revise.patch_id", r.PatchID, true},
		{"revise.base_tree", r.BaseTree, false}, {"revise.head_tree", r.HeadTree, false},
	} {
		if (id.id != "" || !id.empty) && (!gitref.ValidOID(id.id) || id.id == ZeroOID) {
			return badField(id.field, "%q is not a whole object id", id.id)
		}
	}
	if why := branchProblem(r.TargetRef); why != "" {
		return badField("revise.target_ref", "%s", why)
	}
	switch p := r.TargetPosition; {
	case p.IsZero():
		return badField("revise.target_position", "the facts name the record that moved the target to the tip "+
			"they were read against")
	case p.Stream != topics.CodeLogStream:
		return badField("revise.target_position", "%s is a position on another log than the code log (%s)", p,
			topics.CodeLogStream)
	case p.Valid() != nil:
		return fmt.Errorf("%w: revise.target_position: %w", ErrInvalidProposal, p.Valid())
	}
	switch {
	case r.BaseOID == r.HeadOID:
		return badField("revise.base_oid", "%s is the head itself, which its target holds: the change is "+
			"merged, and its home lands it rather than recording facts of it", short(r.HeadOID))
	case r.PatchIDOmitted && r.PatchID != "":
		return badField("revise.patch_id", "the facts omitted the id, and name one")
	}
	if err := touchedProblem(r.Touched, r.TouchesAll); err != nil {
		return err
	}
	if err := classesProblem(r.Classes, r.Touched, r.TouchesAll); err != nil {
		return err
	}
	if !config.ValidNodeID(r.Node) {
		return badField("revise.node", "%q is not a node id config admits", r.Node)
	}
	if len(r.RequestedOwners) > MaxPathOwners {
		return badField("revise.requested_owners", "%d owners, past the %d a repository names (MaxPathOwners)",
			len(r.RequestedOwners), MaxPathOwners)
	}
	for i, o := range r.RequestedOwners {
		field := fmt.Sprintf("revise.requested_owners[%d]", i)
		switch {
		case !o.Kind.Valid():
			return badField(field+".kind", "%q is not one of %v", o.Kind, OwnerKinds())
		case strings.TrimSpace(o.Ref) == "":
			return badField(field+".ref", "a path owner names a unit or a seat")
		case i > 0 && compareOwners(r.RequestedOwners[i-1], o) >= 0:
			return badField(field, "%s %s is not after %s %s — matched owners are in the order the "+
				"repository's settings hold them, each once", o.Kind, o.Ref, r.RequestedOwners[i-1].Kind,
				r.RequestedOwners[i-1].Ref)
		}
	}
	if len(r.OwnersDigest) != sha256.Size*2 || strings.Trim(r.OwnersDigest, "0123456789abcdef") != "" {
		return badField("revise.owners_digest", "%q is not a SHA-256 in lowercase hex", r.OwnersDigest)
	}
	if len(r.Asked) > 0 || len(r.SkippedOwners) > 0 || len(r.ReReview) > 0 || r.LeadFallback != nil {
		return badField("revise", "the writer stamps whom the facts ask, skip and wake from its snapshot and its "+
			"chart, and the home sends none of them")
	}
	return stampedRouting("revise", r.Routing)
}

// touchedProblem refuses a revision's touched paths as a revise carries them:
// a list beside the word that it touches all, a list past either bound the
// facts hold one to, or a path no tree entry is named by — one of valid UTF-8,
// which a record carries exactly, holding no NUL, which separates them on the
// row.
func touchedProblem(touched []string, all bool) error {
	switch cost := TouchedCost(touched); {
	case all && len(touched) > 0:
		return badField("revise.touched", "a revision that touches all lists no paths")
	case len(touched) > MaxTouchedPaths:
		return badField("revise.touched", "%d paths, past the %d a revision's facts list (MaxTouchedPaths)",
			len(touched), MaxTouchedPaths)
	case cost > MaxTouchedBytes:
		return badField("revise.touched", "the list is %d bytes of JSON, past the %d a revision's facts list "+
			"(MaxTouchedBytes)", cost, MaxTouchedBytes)
	}
	for i, p := range touched {
		switch {
		case !utf8.ValidString(p):
			return badField(fmt.Sprintf("revise.touched[%d]", i), "not valid UTF-8")
		case validGitPath(p, false) != "":
			return badField(fmt.Sprintf("revise.touched[%d]", i), "%s", validGitPath(p, false))
		}
	}
	return nil
}

// compareOwners is the order a repository's path owners are held in — by kind,
// then reference ([comparePathOwners]) — as two owners alone.
func compareOwners(a, b Owner) int {
	return comparePathOwners(PathOwner{Owner: a}, PathOwner{Owner: b})
}

// stampedReviseBytes is what the writer may add to a revise's record beside
// what the home sent, at its widest: an ask or a skip for every matched owner —
// a skip naming the owner, the widest handle and the widest reason, which is
// wider than an ask — MaxReReviewers reviewers to look again and a lead, each
// at the widest handle a seat may have and a person's kind.
func stampedReviseBytes(r Revise) int {
	const (
		handle   = iam.MaxLogin // a seat's handle is no longer than a login (iam.ValidSeatHandle)
		reviewer = len(`{"handle":"","kind":"person"},`) + handle
		lists    = len(`,"asked":[],"skipped_owners":[],"re_review":[],"lead_fallback":`)
	)
	widest := 0
	for _, why := range SkipReasons() {
		widest = max(widest, len(why))
	}
	n := lists + MaxReReviewers*reviewer + reviewer
	for _, o := range r.RequestedOwners {
		encoded, err := json.Marshal(SkippedOwner{Owner: o, Handle: strings.Repeat("h", handle),
			Why: SkipReason(strings.Repeat("w", widest))})
		if err != nil {
			continue
		}
		n += len(encoded) + 1
	}
	return n
}

// factsRow is what a revise's decide and apply read of one revision's facts:
// the branch and the tip's position they are against, and what their wide
// columns weigh.
type factsRow struct {
	targetRef      string
	targetPosition int64
	bytes          int64
}

// factsIn reads revision n of changeID's facts, and false where it holds none.
func factsIn(ctx context.Context, tx *sql.Tx, changeID string, n int64) (factsRow, bool, error) {
	var f factsRow
	err := tx.QueryRowContext(ctx, factsSQL, changeID, n).Scan(&f.targetRef, &f.targetPosition, &f.bytes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return factsRow{}, false, nil
	case err != nil:
		return factsRow{}, false, fmt.Errorf("code: read revision %d of %s's facts: %w", n, changeID, err)
	}
	return f, true, nil
}

// decideRevise is a revise's rules over row — see this file's head: a LIVE
// change (change_closed, change_merged); its head revision and head and its
// current target (superseded); no facts of that revision against that branch
// at or past the tip read (superseded); the repository's path owners as the
// home matched them (superseded), each matched owner one of them; and then
// the chart's judgment of whom the facts ask, skip and wake, stamped onto the
// record. Pure over (the rows, w, roster): roster is the deciding epoch's
// chart, which a revise always reads ([ConversationWrite.JudgesChart]).
func decideRevise(ctx context.Context, tx *sql.Tx, w ConversationWrite, roster Roster, repo RepoRef,
	row changeRow, named string, d conversationDecision) (conversationDecision, error) {

	r := *w.Revise
	switch row.state {
	case ChangeStateClosed:
		return d.refuse(ConversationRefusalChangeClosed, fmt.Sprintf("%s is closed, and a closed change's "+
			"revisions take no facts", named))
	case ChangeStateMerged:
		return d.refuse(ConversationRefusalChangeMerged, fmt.Sprintf("%s is merged, and a merged change's "+
			"revisions take no facts", named))
	}
	switch {
	case r.N != row.revision || r.HeadOID != row.head:
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s is at revision %d, its head %s, and the "+
			"facts are of revision %d at %s: the home reads the head revision's", named, row.revision,
			short(row.head), r.N, short(r.HeadOID)))
	case r.TargetRef != row.target:
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s merges into %s, and the facts are "+
			"against %s: the home reads them against the change's target", named, row.target, r.TargetRef))
	}
	prior, held, err := factsIn(ctx, tx, row.id, r.N)
	if err != nil {
		return conversationDecision{}, err
	}
	if held && prior.targetRef == r.TargetRef && prior.targetPosition >= r.TargetPosition.Packed() {
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("revision %d of %s holds facts against %s "+
			"at %s already, and these were read at %s", r.N, named, r.TargetRef, unpacked(prior.targetPosition),
			r.TargetPosition))
	}
	owners, err := pathOwnersIn(ctx, tx, repo.ID)
	if err != nil {
		return conversationDecision{}, err
	}
	if digest := OwnersDigest(owners); digest != r.OwnersDigest {
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s's path owners are not the ones the "+
			"facts were matched against: the home matches the touched paths again", repo.Name))
	}
	isOwner := make(map[Owner]bool, len(owners))
	for _, po := range owners {
		isOwner[po.Owner] = true
	}
	for _, o := range r.RequestedOwners {
		if !isOwner[o] {
			return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s names no path owner %s %s, which "+
				"the facts were matched to: the home matches the touched paths again", repo.Name, o.Kind, o.Ref))
		}
	}
	j, err := judgeOwners(ctx, tx, r, roster, repo, row, !held)
	if err != nil {
		return conversationDecision{}, err
	}
	r.Asked, r.SkippedOwners, r.ReReview, r.LeadFallback = j.asked, j.skipped, j.reReview, j.fallback
	// WHOM THE FACTS CONCERN: the path owners they ask, the reviewers they
	// ask to look again, and the lead a change nobody may review reaches.
	d.concern = concerning{pathOwners: j.asked, reReview: j.reReview}
	if j.fallback != nil {
		d.concern.lead = *j.fallback
	}
	d.payload = r
	// THE FACTS, A ROW EACH REVIEWER ASKED AND A PARTICIPANT ROW EACH, the
	// ask of each reviewer it asks to look again, and the conversation's own.
	d.rows = 2*len(r.Asked) + len(r.ReReview) + 2
	return d, nil
}

// ownersJudged is what a revise's decide makes of the owners a revision's
// facts matched, and of whom else its facts concern.
type ownersJudged struct {
	asked    []Reviewer
	skipped  []SkippedOwner
	reReview []Reviewer
	fallback *Reviewer
}

// resolvedOwner is one matched owner as the chart resolves it: the seat it is
// asked of, the kind it is asked as, and why it is skipped where it is.
type resolvedOwner struct {
	owner  Owner
	handle string
	kind   PartyKind
	why    SkipReason
}

// judgeOwners is whom a revise of a live change asks, skips and wakes, over the
// rows tx holds and the chart roster — see this file's head. first is a
// revision's first facts: only those wake reviewers to look again, and only
// the change's first wake its lead where nobody who may review it is asked.
func judgeOwners(ctx context.Context, tx *sql.Tx, r Revise, roster Roster, repo RepoRef, row changeRow,
	first bool) (ownersJudged, error) {

	var j ownersJudged
	resolved := make([]resolvedOwner, 0, len(r.RequestedOwners))
	handles := []string{}
	for _, o := range r.RequestedOwners {
		ro := resolveOwner(o, roster, repo)
		resolved = append(resolved, ro)
		if ro.handle != "" {
			handles = append(handles, ro.handle)
		}
	}
	// WHOM ELSE THE FACTS CONCERN, read before the authors so one statement
	// asks which of everybody wrote the change: the reviewers standing on
	// request_changes, on a revision's first facts; the standing asks and
	// the owner's lead, on the change's first.
	var again []Reviewer
	if first && r.N > 1 {
		var err error
		if again, err = changesRequestedBefore(ctx, tx, row.id, r.N); err != nil {
			return ownersJudged{}, err
		}
		for _, rv := range again {
			handles = append(handles, rv.Handle)
		}
	}
	anyFacts := !first
	if first {
		var err error
		if anyFacts, err = factsOf(ctx, tx, row.id); err != nil {
			return ownersJudged{}, err
		}
	}
	var standing []Reviewer
	lead := Reviewer{}
	if !anyFacts {
		var err error
		if standing, err = standingAsks(ctx, tx, row.id); err != nil {
			return ownersJudged{}, err
		}
		for _, rv := range standing {
			handles = append(handles, rv.Handle)
		}
		if lead = ownersLead(repo, roster); lead.Handle != "" {
			handles = append(handles, lead.Handle)
		}
	}
	authors, err := authorsAmong(ctx, tx, row.id, handles)
	if err != nil {
		return ownersJudged{}, err
	}
	asks := []string{}
	for _, ro := range resolved {
		if ro.handle != "" {
			asks = append(asks, ro.handle)
		}
	}
	states, requested, err := reviewerStates(ctx, tx, row.id, asks)
	if err != nil {
		return ownersJudged{}, err
	}
	// EACH OWNER IN THE ORDER MATCHED, and a seat two owners resolve to — two
	// units one lead leads — judged once: asked once, or skipped for each
	// owner by the same reason.
	judged := map[string]SkipReason{}
	for _, ro := range resolved {
		why := ro.why
		if why == "" {
			if earlier, seen := judged[ro.handle]; seen {
				if earlier != "" {
					j.skipped = append(j.skipped, SkippedOwner{Owner: ro.owner, Handle: ro.handle, Why: earlier})
				}
				continue
			}
			switch {
			case authors[ro.handle] == ro.kind:
				why = SkipAuthor
			case states[ro.handle] != "":
				// ASKED ALREADY — the ask waits, was answered or ended, or
				// somebody withdrew it, which is their word on this change and
				// no rule's to undo.
			case requested+len(j.asked) >= MaxReviewersPerChange:
				why = SkipTooManyReviewers
			default:
				j.asked = append(j.asked, Reviewer{Handle: ro.handle, Kind: ro.kind})
			}
			judged[ro.handle] = why
		}
		if why != "" {
			j.skipped = append(j.skipped, SkippedOwner{Owner: ro.owner, Handle: ro.handle, Why: why})
		}
	}
	for _, rv := range again {
		if authors[rv.Handle] != rv.Kind {
			j.reReview = append(j.reReview, rv)
		}
	}
	if !anyFacts && lead.Handle != "" && authors[lead.Handle] != lead.Kind && len(j.asked) == 0 &&
		!slices.ContainsFunc(standing, func(rv Reviewer) bool { return mayReview(rv, authors, repo, roster) }) {
		j.fallback = &lead
	}
	return j, nil
}

// resolveOwner is o as the chart resolves it: a unit's effective lead or the
// seat itself, the kind it is asked as, and why it is skipped before anything
// of the change is read — no lead, no seat, or a seat that may not review the
// repository.
func resolveOwner(o Owner, roster Roster, repo RepoRef) resolvedOwner {
	ro := resolvedOwner{owner: o, handle: o.Ref}
	if o.Kind == OwnerUnit {
		if ro.handle = roster.Lead(o.Ref); ro.handle == "" {
			ro.why = SkipNoLead
			return ro
		}
	}
	seat, ok := roster.Seat(ro.handle)
	switch {
	case !ok:
		ro.why = SkipNoSeat
	case seat.Human:
		ro.kind = PartyPerson
	default:
		ro.kind = PartyAgent
		if GrantOf(Principal{Kind: PrincipalAgent, Seat: ro.handle}, repo, roster).Access < AccessReview {
			ro.why = SkipBelowReview
		}
	}
	return ro
}

// mayReview reports whether rv's review would count: a person, or a seat that
// reviews the repository, and in either case no author of the change.
func mayReview(rv Reviewer, authors map[string]PartyKind, repo RepoRef, roster Roster) bool {
	if authors[rv.Handle] == rv.Kind {
		return false
	}
	if rv.Kind == PartyPerson {
		return true
	}
	return GrantOf(Principal{Kind: PrincipalAgent, Seat: rv.Handle}, repo, roster).Access >= AccessReview
}

// ownersLead is the lead a change nobody who may review it is asked reaches:
// the repository's owning unit's effective lead, or the effective lead of the
// unit an owning seat sits in — as a reviewer of its kind, and none where the
// chart names nobody.
func ownersLead(repo RepoRef, roster Roster) Reviewer {
	var lead string
	switch repo.Owner.Kind {
	case OwnerUnit:
		lead = roster.Lead(repo.Owner.Ref)
	case OwnerSeat:
		if s, ok := roster.Seat(repo.Owner.Ref); ok {
			lead = roster.Lead(s.Unit)
		}
	}
	if lead == "" {
		return Reviewer{}
	}
	seat, ok := roster.Seat(lead)
	switch {
	case !ok:
		return Reviewer{}
	case seat.Human:
		return Reviewer{Handle: lead, Kind: PartyPerson}
	}
	return Reviewer{Handle: lead, Kind: PartyAgent}
}

// changesRequestedBefore is the reviewers of changeID whose standing verdict
// asks for changes on a revision before n, by handle, at most MaxReReviewers —
// one range of the participants' key.
func changesRequestedBefore(ctx context.Context, tx *sql.Tx, changeID string, n int64) ([]Reviewer, error) {
	rows, err := tx.QueryContext(ctx, changesRequestedBeforeSQL, changeID, n, MaxReReviewers)
	if err != nil {
		return nil, fmt.Errorf("code: read who asks for changes on %s: %w", changeID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Reviewer
	for rows.Next() {
		var rv Reviewer
		var kind string
		if err := rows.Scan(&rv.Handle, &kind); err != nil {
			return nil, fmt.Errorf("code: read who asks for changes on %s: %w", changeID, err)
		}
		rv.Kind = PartyKind(kind)
		out = append(out, rv)
	}
	return out, rows.Err()
}

// standingAsks is every reviewer changeID asks now, by handle — at most
// MaxReviewersPerChange, which no change asks past.
func standingAsks(ctx context.Context, tx *sql.Tx, changeID string) ([]Reviewer, error) {
	rows, err := tx.QueryContext(ctx, standingAsksSQL, changeID, MaxReviewersPerChange)
	if err != nil {
		return nil, fmt.Errorf("code: read %s's reviewers: %w", changeID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Reviewer
	for rows.Next() {
		var rv Reviewer
		var kind string
		if err := rows.Scan(&rv.Handle, &kind); err != nil {
			return nil, fmt.Errorf("code: read %s's reviewers: %w", changeID, err)
		}
		rv.Kind = PartyKind(kind)
		out = append(out, rv)
	}
	return out, rows.Err()
}

// factsOf reports whether changeID holds facts of any revision.
func factsOf(ctx context.Context, tx *sql.Tx, changeID string) (bool, error) {
	var n int64
	err := tx.QueryRowContext(ctx, anyFactsSQL, changeID).Scan(&n)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("code: read whether %s holds facts: %w", changeID, err)
	}
	return true, nil
}

// A revise's decide reads, each registered in plan_test.go: one revision's
// facts by their key; whether a change holds any, by the key's prefix; who
// stands on request_changes before a revision, a range of the participants'
// key in its order; and the reviewers a change asks now, a range of the
// reviewers' key in its order — the reviewers' key NAMED ([reviewersKey] says
// why).
const (
	factsSQL = `SELECT target_ref, target_position, bytes FROM code_revision_facts
		WHERE change_id = ? AND n = ?`
	anyFactsSQL               = `SELECT n FROM code_revision_facts WHERE change_id = ? LIMIT 1`
	changesRequestedBeforeSQL = `SELECT handle, kind FROM code_participants
		WHERE change_id = ? AND verdict = 'request_changes' AND verdict_revision < ? ORDER BY handle LIMIT ?`
	standingAsksSQL = `SELECT reviewer, reviewer_kind FROM code_reviewers INDEXED BY ` + reviewersKey + `
		WHERE change_id = ? AND state = 'requested' ORDER BY reviewer LIMIT ?`
)
