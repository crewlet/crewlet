package gitlet

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// A REVISION'S FACTS (package doc, "A change's conversation is rows on its
// own subject"): what the repository's HOME Gitlet node read of one revision
// against its target (internal/gitlet/query's revision_facts, a function of
// two commits alone), recorded as a [Revise] on the change's subject, which
// the committing data node's [ChangeWriter] decides and publishes; a Gitlet
// node never does. Every later reader reads code_revision_facts, never git.
//
// A HEAD ITS TARGET HOLDS IS NO REVISION TO MEASURE: the change is merged, and
// the home owes it a landing, decided on the repository's subject. Facts
// beside it would be a second record of one finding, so a revise whose base
// is its own head is refused before anything is read.

// PATH OWNERS ARE MATCHED ON THE HOME ([MatchOwners]): up to MaxPathOwners ×
// MaxPathOwnerPaths globs against MaxTouchedPaths paths is more than a decide
// pays on every round it may take. The home sends the matched owners beside
// their [OwnersDigest], which the decide holds to code_repos.owners_digest:
// another digest is owners changed since, and the facts are SUPERSEDED for
// the home's next pass. Owners changed after the facts were recorded make the
// revision due again ([ReadChangesDue]), and the same tip's new match
// replaces them. Whom they ask is the chart's judgment, which no home makes
// ([judgeOwners], [SkipReason]); an owner whose ask the change holds already
// (waiting, answered, ended, or withdrawn by somebody's word, which no rule
// overrides) is left as it is. A path owner GRANTS NOTHING: whether its
// review counts is the reviewer's grant's, as any reviewer's.

// HUMAN PATHS are matched on the home the same way ([MatchTargetHumanPaths]),
// held to code_repos.human_paths_digest, each place checked against the
// target's gate ([humanPlacesIn]).

// THE GUARD: facts are of the change's HEAD revision against its CURRENT
// target, and a revise is decided only while they still are. One of an
// earlier revision or head, of a branch it no longer merges into, or no later than the facts
// held of that revision there (the same tip read twice, by a home that ran
// its pass again or two homes a split presence view each called home) is
// SUPERSEDED, unless the held facts were matched against owners or human
// paths since changed. A retarget is new facts of the same revision, which
// replace the row.

// A revise's ROUTING ([ChangeRouting]), stamped by the writer from the
// snapshot and the chart, is whom it concerns beside its asks: on a
// revision's first facts, the reviewers standing on request_changes from an
// earlier one (re_review); on the change's first, where nobody who may review
// it is asked, the lead of the repository's owner (lead_fallback), so it
// still reaches somebody. The routing is their ONE copy (the apply asks its
// re_review to look again, and only the wakes read lead_fallback): the
// payload restates neither.

// A REVISION'S CLASSES RIDE ITS FACTS (package doc, "A change's class is
// lexed, and a directive is code"). The TREES of its base and head
// ([Revise.BaseTree], [Revise.HeadTree]): a revision holding the same pair
// holds the same code, so an approval carries to it whatever its history or
// patch id ([ChangeClassNoCodeChange], [ReadinessOf]). The trees are recorded
// rather than the class, a relation between two revisions: recorded by a
// home, it would be judged against a row a late record may replace;
// compared, it is judged by whoever reads both rows in one snapshot. And the
// PATH classes of its touched Go files ([RevisionClasses], [ClassifyGo]),
// which a merge gate's exemptions read ([MergeGate.ApprovalsUnless],
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

// MatchedOwner is a path owner a revision's touched paths matched, and its globs that matched them
// in the order its settings hold them; none where the revision touches all, every glob matching.
type MatchedOwner struct {
	Owner
	// Paths is SENT BY THE HOME, at most MaxPathOwnerPaths; none from a home before [OwnedPathsVersion].
	Paths []string `json:"paths,omitempty"`
}

// OwnerAsk is a reviewer a revise asks for the path owners resolving to it, and the globs of
// theirs the revision touched: sorted, each once, cut at MaxPathOwnerPaths and MaxOwnedPathsBytes
// as JSON, PathsOmitted how many more. STAMPED BY THE WRITER.
type OwnerAsk struct {
	Reviewer
	Paths        []string `json:"paths,omitempty"`
	PathsOmitted int      `json:"paths_omitted,omitempty"`
}

// Revise records one revision's facts on its change: what the revision's head
// is against its target's tip, as the repository's home Gitlet node read them,
// and the path owners whose globs its touched paths match.
//
// Every field is in the base format, as [OpRevise] is, but MeasuredAt and the globs of the matched
// owners and the asks ([OwnedPathsVersion]).
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
	// (this file's head, "A revision's classes ride its facts"). BASE FORMAT,
	// as [OpRevise] is.
	BaseTree string `json:"base_tree"`
	HeadTree string `json:"head_tree"`
	// PatchID is `git patch-id --verbatim` of the cumulative diff from the
	// base, "" for a revision that changes nothing and where PatchIDOmitted:
	// the diff past the bound on one whose id is taken, the one omission a
	// home records (an omission for time is no final fact, and recorded by
	// nobody: [RevisionFactsAnswer.Final]).
	PatchID        string `json:"patch_id,omitempty"`
	PatchIDOmitted bool   `json:"patch_id_omitted,omitempty"`
	// Touched is every file the change touches, unless TouchesAll — read as
	// every path, with no list.
	Touched    []string `json:"touched"`
	TouchesAll bool     `json:"touches_all,omitempty"`
	// Classes is the path classes of the touched files' changes, each named
	// by its place in Touched ([RevisionClasses]). BASE FORMAT, as [OpRevise]
	// is; omitted at its zero: nothing classed, the classification incomplete.
	Classes RevisionClasses `json:"classes,omitzero"`
	// Node is the Gitlet node that recorded the facts: the repository's home as
	// it took itself to be, which asked for them.
	Node string `json:"node"`
	// MeasuredAt is when the home read these facts, by its clock and to the microsecond: SENT BY THE
	// HOME, zero from a home before [OwnedPathsVersion].
	MeasuredAt time.Time `json:"measured_at,omitzero"`
	// RequestedOwners is the path owners [MatchOwners] answered, and
	// OwnersDigest the [OwnersDigest] of the owner rows they were matched
	// against, which the decide holds its own snapshot's to.
	RequestedOwners []MatchedOwner `json:"requested_owners,omitempty"`
	OwnersDigest    string         `json:"owners_digest"`
	// HumanPaths is which of the human paths the change's target's merge gate
	// names hold the revision to a person and which their unless waives, each
	// by its place in the target's human paths, as [MatchTargetHumanPaths]
	// answers them; HumanPathsDigest the [HumanPathsDigest] of the rules' human
	// paths they were matched against, which the decide holds its snapshot's to.
	// BASE FORMAT, as [OpRevise] is.
	HumanPaths       HumanPathMatch `json:"human_paths,omitzero"`
	HumanPathsDigest string         `json:"human_paths_digest"`

	// Asked is the reviewers the record asks for the matched owners, each by
	// the seat it resolved to, its kind and the globs it is asked for;
	// SkippedOwners the matched owners it asked no review of, and why.
	// STAMPED BY THE WRITER from its snapshot and its chart; the home sends
	// neither.
	Asked         []OwnerAsk     `json:"asked,omitempty"`
	SkippedOwners []SkippedOwner `json:"skipped_owners,omitempty"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// WRITER; an asker sends none. nil where the facts concern nobody (this
	// file's head).
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// MaxReReviewers is the most reviewers one revise asks to look again: 64, as
// many as a change's routing names watchers. A change with more standing on
// request_changes has an audience no wake is the way to; the rest read it in
// their listings.
const MaxReReviewers = 64

// RevisionClasses is what the changes of a revision's touched paths are to a
// lexer ([ClassifyGo]): each classed path in the list of its NARROWEST class
// alone — a whitespace_only change is a comment_only one too.
//
// A PATH IS NAMED BY ITS PLACE in the facts' touched list, from 0, never by
// name: beside a touched list of up to [MaxTouchedBytes], naming a thousand
// reformatted Go files again would take the facts' page twice over.
//
// COMPLETE IS WHAT LETS A LIST BE READ AS EVERY PATH. A path no list names has
// no class, or, incomplete, was never reached (past [MaxClassedBytes], past
// the read's git budget, or every path of a revision touching all). So an
// exemption needing every touched path classed requires Complete.
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
// is the lexer's, on the Gitlet node, and no rule here: a build that lexes
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
		panic(fmt.Sprintf("gitlet: encoding path owners: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// MatchOwners is which of a repository's path owners a revision touching
// touched owns — every owner one of whose globs matches a touched path, and
// every owner where the revision touches all — in the order owners holds them
// (the canonical one, [CanonicalPathOwners]), each with its globs that matched,
// none where the revision touches all; and the digest of owners
// ([OwnersDigest]) a revise sends beside them. A glob gitref cannot read, which
// no row holds since every settings record is validated, owns nothing.
//
// ON THE HOME, NEVER IN A DECIDE: up to MaxPathOwners × MaxPathOwnerPaths globs
// against MaxTouchedPaths paths, each glob's match ending at its first hit.
func MatchOwners(owners []PathOwner, touched []string, touchesAll bool) ([]MatchedOwner, string) {
	canonical := CanonicalPathOwners(owners)
	var out []MatchedOwner
	for _, po := range canonical {
		if touchesAll {
			out = append(out, MatchedOwner{Owner: po.Owner})
			continue
		}
		if globs := matchingGlobs(po.Paths, touched); len(globs) > 0 {
			out = append(out, MatchedOwner{Owner: po.Owner, Paths: globs})
		}
	}
	return out, OwnersDigest(canonical)
}

// matchingGlobs is the globs of globs one of paths matches, in globs' order.
func matchingGlobs(globs, paths []string) []string {
	var out []string
	for _, spelled := range globs {
		glob, err := gitref.ParsePathGlob(spelled)
		if err != nil {
			continue
		}
		if slices.ContainsFunc(paths, glob.Match) {
			out = append(out, spelled)
		}
	}
	return out
}

// validFields holds a revise to [ConversationWrite.Validate]'s rules: whole
// object ids; a base that is not the head (this file's head); a branch target
// and a position on the code log; touched paths within the facts' bounds and
// classes naming them; a node id config admits; matched owners sorted and
// once, their globs named exactly where measured facts do not touch all
// ([matchedGlobsProblem]); digests of SHA-256's width in lowercase hex; and
// no field the writer stamps.
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
	named := !r.MeasuredAt.IsZero() && !r.TouchesAll
	for i, o := range r.RequestedOwners {
		field := fmt.Sprintf("revise.requested_owners[%d]", i)
		switch {
		case !o.Kind.Valid():
			return badField(field+".kind", "%q is not one of %s", o.Kind, words.Or(OwnerKinds()))
		case strings.TrimSpace(o.Ref) == "":
			return badField(field+".ref", "a path owner names a unit or a seat")
		case i > 0 && compareOwners(r.RequestedOwners[i-1].Owner, o.Owner) >= 0:
			return badField(field, "%s %s is not after %s %s — matched owners are in the order the "+
				"repository's settings hold them, each once", o.Kind, o.Ref, r.RequestedOwners[i-1].Kind,
				r.RequestedOwners[i-1].Ref)
		}
		if err := matchedGlobsProblem(field+".paths", o, named, r.TouchesAll); err != nil {
			return err
		}
	}
	if !r.MeasuredAt.IsZero() && r.MeasuredAt.UnixMicro() <= 0 {
		return badField("revise.measured_at", "%s: an instant before 1970 is no home's reading",
			r.MeasuredAt.Format(time.RFC3339Nano))
	}
	if len(r.OwnersDigest) != sha256.Size*2 || strings.Trim(r.OwnersDigest, "0123456789abcdef") != "" {
		return badField("revise.owners_digest", "%q is not a SHA-256 in lowercase hex", r.OwnersDigest)
	}
	if err := humanPathMatchProblem(r.HumanPaths); err != nil {
		return err
	}
	if len(r.HumanPathsDigest) != sha256.Size*2 || strings.Trim(r.HumanPathsDigest, "0123456789abcdef") != "" {
		return badField("revise.human_paths_digest", "%q is not a SHA-256 in lowercase hex", r.HumanPathsDigest)
	}
	if len(r.Asked) > 0 || len(r.SkippedOwners) > 0 {
		return badField("revise", "the writer stamps whom the facts ask, skip and wake from its snapshot and its "+
			"chart, and the home sends none of them")
	}
	return stampedRouting("revise", r.Routing)
}

// matchedGlobsProblem refuses a matched owner's globs as a revise carries them at field: named
// exactly where the facts name them (named), at most MaxPathOwnerPaths, each a path glob, sorted and once.
func matchedGlobsProblem(field string, o MatchedOwner, named, touchesAll bool) error {
	switch {
	case named && len(o.Paths) == 0:
		return badField(field, "the home names the globs of every owner it matched, and names none of %s %s",
			o.Kind, o.Ref)
	case !named && len(o.Paths) > 0 && touchesAll:
		return badField(field, "a revision that touches all names no glob: it touches every glob of every owner")
	case !named && len(o.Paths) > 0:
		return badField(field, "facts naming globs name when they were measured (measured_at)")
	case len(o.Paths) > MaxPathOwnerPaths:
		return badField(field, "%d globs, past the %d a path owner names (MaxPathOwnerPaths)", len(o.Paths),
			MaxPathOwnerPaths)
	}
	for k, glob := range o.Paths {
		if _, err := gitref.ParsePathGlob(glob); err != nil {
			return badField(fmt.Sprintf("%s[%d]", field, k), "%q is not a path glob: %v", glob, err)
		}
		if k > 0 && o.Paths[k-1] >= glob {
			return badField(fmt.Sprintf("%s[%d]", field, k), "%q is not after %q: an owner's globs are in the "+
				"order its settings hold them, each once", glob, o.Paths[k-1])
		}
	}
	return nil
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
// what the home sent, at its widest: an ask or a skip for every matched owner,
// each at the wider of a skip naming the owner, the widest handle and the
// widest reason, and an ask naming the widest handle and globs at their bound.
// Whom it asks to look again and the lead it falls back to ride its routing
// alone, which [MaxRoutingBytes] bounds.
func stampedReviseBytes(r Revise) int {
	const (
		handle = iam.MaxLogin // a seat's handle is no longer than a login (iam.ValidSeatHandle)
		lists  = len(`,"asked":[],"skipped_owners":[]`)
	)
	widest := 0
	for _, why := range SkipReasons() {
		widest = max(widest, len(why))
	}
	ask := len(`{"handle":"","kind":"","paths":,"paths_omitted":},`) + handle + len(PartyPerson) +
		MaxOwnedPathsBytes + len(strconv.Itoa(MaxPathOwners*MaxPathOwnerPaths))
	n := lists
	for _, o := range r.RequestedOwners {
		encoded, err := json.Marshal(SkippedOwner{Owner: o.Owner, Handle: strings.Repeat("h", handle),
			Why: SkipReason(strings.Repeat("w", widest))})
		if err != nil {
			continue
		}
		n += max(len(encoded)+1, ask)
	}
	return n
}

// factsRow is what a revise's decide and apply read of one revision's facts:
// the branch and the tip's position they are against, the digest of the path
// owners they were matched against, and what their wide columns weigh.
type factsRow struct {
	targetRef        string
	targetPosition   int64
	ownersDigest     string
	humanPathsDigest string
	bytes            int64
}

// factsIn reads revision n of changeID's facts, and false where it holds none.
func factsIn(ctx context.Context, tx *sql.Tx, changeID string, n int64) (factsRow, bool, error) {
	var f factsRow
	err := tx.QueryRowContext(ctx, factsSQL, changeID, n).Scan(&f.targetRef, &f.targetPosition, &f.ownersDigest,
		&f.humanPathsDigest, &f.bytes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return factsRow{}, false, nil
	case err != nil:
		return factsRow{}, false, fmt.Errorf("gitlet: read revision %d of %s's facts: %w", n, changeID, err)
	}
	return f, true, nil
}

// decideRevise is a revise's rules over row — see this file's head: a LIVE
// change (change_closed, change_merged); its head revision and head and its
// current target (superseded); no facts of that revision against that branch
// at or past the tip read and matched against the path owners the repository
// holds (superseded); the repository's path owners as the home matched them
// (superseded), each matched owner one of them; and then
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
	digests, err := matchDigestsIn(ctx, tx, repo.ID)
	if err != nil {
		return conversationDecision{}, err
	}
	// FACTS HELD AT OR PAST THE TIP READ SUPERSEDE THESE — unless a match
	// they hold is of owners or rules the repository no longer has: the
	// home's due listing names such a revision due its facts again
	// ([ReadChangesDue]), and the same tip read again is how it matches what
	// the repository holds now.
	if held && prior.targetRef == r.TargetRef && prior.ownersDigest == digests.owners &&
		prior.humanPathsDigest == digests.humanPaths && prior.targetPosition >= r.TargetPosition.Packed() {
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("revision %d of %s holds facts against %s "+
			"at %s already, matched against the path owners and the human paths it holds, and these were read "+
			"at %s", r.N, named, r.TargetRef, unpacked(prior.targetPosition), r.TargetPosition))
	}
	if r.OwnersDigest != digests.owners {
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s's path owners are not the ones the "+
			"facts were matched against: the home matches the touched paths again", repo.Name))
	}
	if r.HumanPathsDigest != digests.humanPaths {
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s's human paths are not the ones the "+
			"facts were matched against: the home matches the touched paths again", repo.Name))
	}
	if err = humanPlacesIn(ctx, tx, repo, r); err != nil {
		return conversationDecision{}, err
	}
	owners, err := pathOwnersIn(ctx, tx, repo.ID)
	if err != nil {
		return conversationDecision{}, err
	}
	globsOf := make(map[Owner][]string, len(owners))
	for _, po := range owners {
		globsOf[po.Owner] = po.Paths
	}
	// EACH OWNER'S MATCHED GLOBS: those the home names, and every glob it holds where a home of
	// this build measured facts touching all. An older home's name none, so its asks name none.
	matched := make([][]string, len(r.RequestedOwners))
	for i, o := range r.RequestedOwners {
		owns, ok := globsOf[o.Owner]
		if !ok {
			return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s names no path owner %s %s, which "+
				"the facts were matched to: the home matches the touched paths again", repo.Name, o.Kind, o.Ref))
		}
		for _, glob := range o.Paths {
			if !slices.Contains(owns, glob) {
				return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s's path owner %s %s owns no glob "+
					"%q, which the facts were matched to: the home matches the touched paths again", repo.Name,
					o.Kind, o.Ref, glob))
			}
		}
		matched[i] = o.Paths
		if r.TouchesAll && !r.MeasuredAt.IsZero() {
			matched[i] = owns
		}
	}
	j, err := judgeOwners(ctx, tx, r, matched, roster, repo, row, !held)
	if err != nil {
		return conversationDecision{}, err
	}
	r.Asked, r.SkippedOwners = j.asked, j.skipped
	// WHOM THE FACTS CONCERN: the path owners they ask, the reviewers they
	// ask to look again, and the lead a change nobody may review reaches.
	d.concern = concerning{pathOwners: asksReviewers(j.asked), reReview: j.reReview}
	if j.fallback != nil {
		d.concern.lead = *j.fallback
	}
	// AND WHOSE ERRAND THEY ARE: a revision is somebody's push (the opener's for
	// revision 1, whoever moved the head after), so the review it asks for is
	// that principal's request. Named as the record's actor, the wake states
	// them as its writer and the woken seat's reads are decided for both grants
	// (the requester ceiling); named as nobody, a seat that may push one
	// repository could have a reviewer read a `visibility: unit` one for it
	// unbounded. Whom a seat's push was made for rides the revision's row
	// (concerns.go's head, "Whom the actor acted for"), and the wake states them
	// too, so the woken seat reads for them. The pusher hears nothing of their own revision, as every actor.
	if d.concern.actor, d.concern.actorKind, d.concern.workingFor, err = revisionPusher(ctx, tx, row.id,
		r.N); err != nil {
		return conversationDecision{}, err
	}
	d.payload = r
	// THE FACTS, A ROW EACH REVIEWER ASKED AND A PARTICIPANT ROW EACH, the
	// ask of each reviewer it asks to look again, and the conversation's own.
	d.rows = 2*len(r.Asked) + len(j.reReview) + 2
	return d, nil
}

// ownersJudged is what a revise's decide makes of the owners a revision's
// facts matched, and of whom else its facts concern.
type ownersJudged struct {
	asked    []OwnerAsk
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
// matched is each requested owner's matched globs, in its order: an ask names
// the union of those of every owner resolving to its seat ([askedGlobs]).
func judgeOwners(ctx context.Context, tx *sql.Tx, r Revise, matched [][]string, roster Roster, repo RepoRef,
	row changeRow, first bool) (ownersJudged, error) {

	var j ownersJudged
	resolved := make([]resolvedOwner, 0, len(r.RequestedOwners))
	handles := []string{}
	globs := map[string][]string{}
	for i, o := range r.RequestedOwners {
		ro := resolveOwner(o.Owner, roster, repo)
		if ro.handle != "" && ro.why == "" {
			globs[ro.handle] = append(globs[ro.handle], matched[i]...)
		}
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
				j.asked = append(j.asked, OwnerAsk{Reviewer: Reviewer{Handle: ro.handle, Kind: ro.kind}})
			}
			judged[ro.handle] = why
		}
		if why != "" {
			j.skipped = append(j.skipped, SkippedOwner{Owner: ro.owner, Handle: ro.handle, Why: why})
		}
	}
	for i := range j.asked {
		j.asked[i].Paths, j.asked[i].PathsOmitted = askedGlobs(globs[j.asked[i].Handle])
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

// askedGlobs is the globs an ask names of globs: sorted, each once, at most MaxPathOwnerPaths and as
// many as MaxOwnedPathsBytes holds as a JSON array, and how many more.
func askedGlobs(globs []string) ([]string, int) {
	sorted := slices.Compact(slices.Sorted(slices.Values(globs)))
	weight, kept := len("[]"), 0
	for kept < len(sorted) && kept < MaxPathOwnerPaths {
		next := stringBytes(sorted[kept])
		if kept > 0 {
			next++
		}
		if weight+next > MaxOwnedPathsBytes {
			break
		}
		weight += next
		kept++
	}
	if kept == 0 {
		return nil, len(sorted)
	}
	return sorted[:kept:kept], len(sorted) - kept
}

// asksReviewers is the reviewers asks ask.
func asksReviewers(asks []OwnerAsk) []Reviewer {
	if len(asks) == 0 {
		return nil
	}
	out := make([]Reviewer, len(asks))
	for i, a := range asks {
		out[i] = a.Reviewer
	}
	return out
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
		return nil, fmt.Errorf("gitlet: read who asks for changes on %s: %w", changeID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Reviewer
	for rows.Next() {
		var rv Reviewer
		var kind string
		if err := rows.Scan(&rv.Handle, &kind); err != nil {
			return nil, fmt.Errorf("gitlet: read who asks for changes on %s: %w", changeID, err)
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
		return nil, fmt.Errorf("gitlet: read %s's reviewers: %w", changeID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Reviewer
	for rows.Next() {
		var rv Reviewer
		var kind string
		if err := rows.Scan(&rv.Handle, &kind); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's reviewers: %w", changeID, err)
		}
		rv.Kind = PartyKind(kind)
		out = append(out, rv)
	}
	return out, rows.Err()
}

// matchDigests is what a repository's row says a revision's touched paths are
// matched against now: the digest of its path owners
// (code_repos.owners_digest) and of its rules' human paths
// (code_repos.human_paths_digest), each written beside the rows it digests by
// every record that writes them.
type matchDigests struct {
	owners     string
	humanPaths string
}

// humanPlacesIn refuses r's human-path match where a place it names is past
// the human paths its target's gate names, as the rules of r's repository in
// tx combine them: under the digest the decide held the match to, a place
// names one entry of that list, and a place past it is a home's error that a
// judgment could only read as a match of other rules, for ever. Read only for
// a match naming a place, so a revision touching no human path reads no rule.
func humanPlacesIn(ctx context.Context, tx *sql.Tx, repo RepoRef, r Revise) error {
	if r.HumanPaths.IsZero() {
		return nil
	}
	rules, err := protectionsIn(ctx, tx, repo.ID)
	if err != nil {
		return err
	}
	gate, err := GateRulesOf(rules, r.TargetRef, "")
	if err != nil {
		return err
	}
	for _, list := range [][]int{r.HumanPaths.Held, r.HumanPaths.Waived} {
		if len(list) > 0 && list[len(list)-1] >= len(gate.HumanPaths) {
			return fmt.Errorf("%w: revise: human path %d is past the %d %s's gate names", ErrInvalidProposal,
				list[len(list)-1], len(gate.HumanPaths), r.TargetRef)
		}
	}
	return nil
}

// matchDigestsIn reads repoID's [matchDigests] — one seek of its key.
func matchDigestsIn(ctx context.Context, tx *sql.Tx, repoID string) (matchDigests, error) {
	var m matchDigests
	if err := tx.QueryRowContext(ctx, matchDigestsSQL, repoID).Scan(&m.owners, &m.humanPaths); err != nil {
		return matchDigests{}, fmt.Errorf("gitlet: read what repository %s's facts are matched against: %w", repoID,
			err)
	}
	return m, nil
}

// factsOf reports whether changeID holds facts of any revision.
func factsOf(ctx context.Context, tx *sql.Tx, changeID string) (bool, error) {
	var n int64
	err := tx.QueryRowContext(ctx, anyFactsSQL, changeID).Scan(&n)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("gitlet: read whether %s holds facts: %w", changeID, err)
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
	factsSQL = `SELECT target_ref, target_position, owners_digest, human_paths_digest, bytes
		FROM code_revision_facts WHERE change_id = ? AND n = ?`
	// matchDigestsSQL is a repository's digests of what a revision's touched
	// paths are matched against, by its key ([matchDigestsIn]).
	matchDigestsSQL           = `SELECT owners_digest, human_paths_digest FROM code_repos WHERE id = ?`
	anyFactsSQL               = `SELECT n FROM code_revision_facts WHERE change_id = ? LIMIT 1`
	changesRequestedBeforeSQL = `SELECT handle, kind FROM code_participants
		WHERE change_id = ? AND verdict = 'request_changes' AND verdict_revision < ? ORDER BY handle LIMIT ?`
	standingAsksSQL = `SELECT reviewer, reviewer_kind FROM code_reviewers INDEXED BY ` + reviewersKey + `
		WHERE change_id = ? AND state = 'requested' ORDER BY reviewer LIMIT ?`
)
