package gitlet

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/words"
)

// WHO MAY DO WHAT TO A REPOSITORY: a grant derived from the chart, never
// stored (ADR-0088), since a stored list would be a second chart that drifts
// the day a seat moves units. Every judge computes it through [GrantOf]:
// Gitlet's admission, the push decide (rule 2b, decide.go, on the deciding
// node's chart), the read tools, and a coding box's admission ([Grant.Within]).
//
// A person maintains every repository and a token bound to nobody reads every
// one (ADR-0085), with no chart. A seat holds the minimum of [Tier.Cap] and its
// chart relation; visibility narrows seats only. A full_access seat pushes
// under its own [SeatNamespace] where its relation is below write: the seat's
// own branches replace forks. Protection rules are judged after the grant, by
// their own code, and a grant never skips one.

// Access is how much of a repository a principal may do, ORDERED: each level
// includes every level below it, which is why a seat's standing is the minimum
// of two of them.
type Access int

// The levels, least first.
const (
	// AccessNone reads nothing.
	AccessNone Access = iota
	// AccessRead fetches, clones and reads through every read tool.
	AccessRead
	// AccessReview is read, and reviewing changes; it moves no ref.
	AccessReview
	// AccessWrite creates and fast-forwards branches, and creates tags.
	AccessWrite
	// AccessMaintain also forces and deletes branches, deletes tags, and
	// changes a tool-managed repository's settings.
	AccessMaintain
)

// Accesses is the closed set, least first.
func Accesses() []Access {
	return []Access{AccessNone, AccessRead, AccessReview, AccessWrite, AccessMaintain}
}

// accessNames is each level's name, in [Accesses]' order.
var accessNames = [...]string{"none", "read", "review", "write", "maintain"}

// Valid reports a level this build knows.
func (a Access) Valid() bool { return a >= AccessNone && a <= AccessMaintain }

// String names the level: none, read, review, write or maintain.
func (a Access) String() string {
	if !a.Valid() {
		return fmt.Sprintf("access(%d)", int(a))
	}
	return accessNames[a]
}

// Tier is a seat's role.code.access: the constants are configuration's, and
// config.TestRoleCodeAccessIsTheOneVocabulary holds the lists equal because
// internal/github spells its own.
type Tier string

// The tiers, least first.
const (
	TierReadOnly   Tier = config.TierReadOnly
	TierReview     Tier = config.TierReview
	TierFullAccess Tier = config.TierFullAccess
)

// Tiers is the closed set, least first.
func Tiers() []Tier { return []Tier{TierReadOnly, TierReview, TierFullAccess} }

// Valid reports a tier this build knows. An unset tier ("") is not one: it is
// the absence of a setting, which [Tier.Cap] reads as read_only.
func (t Tier) Valid() bool { return slices.Contains(Tiers(), t) }

// Cap is the most the tier lets a seat do on any repository. Unset reads, as
// configuration's default does (config.RoleCode.AccessOrDefault); an unknown
// value caps at nothing, the narrow direction, rather than a guessed level.
func (t Tier) Cap() Access {
	switch t {
	case TierReadOnly, "":
		return AccessRead
	case TierReview:
		return AccessReview
	case TierFullAccess:
		return AccessMaintain
	}
	return AccessNone
}

// String is the tier as configuration spells it, and an unset one as the tier
// it runs at.
func (t Tier) String() string {
	if t == "" {
		return string(TierReadOnly) + " (unset)"
	}
	return string(t)
}

// spelled is the tier as a refusal sentence names it: an unknown value is
// "unknown", so no document's value can lengthen a sentence that travels in a
// proposal.
func (t Tier) spelled() string {
	if t != "" && !t.Valid() {
		return "unknown"
	}
	return t.String()
}

// Roster is the chart as the grant rules read it: the serving or deciding
// node's current epoch, adapted by the engine. This package never reads the
// organization itself, so a grant is a pure function of a repository row and
// these four answers.
//
// Every unit argument is a durable REFERENCE ([Owner]'s Ref, [RosterSeat]'s
// Unit), resolved as internal/org's Index.UnitByRef does (folded, an id before
// a name), never compared as a current key: a unit that gained an id after a
// repository named it is still that unit, and its lead and members keep their
// access. A reference the chart does not resolve ("" included, a root seat's
// unit) has no lead and no members, and is within nothing.
type Roster interface {
	// Seat is a seat by handle; false for none. A HUMAN seat is a person.
	Seat(handle string) (RosterSeat, bool)
	// Lead is a unit's EFFECTIVE lead's handle — its own lead, else the
	// nearest ancestor's — and "" for none or a unit the chart lacks.
	Lead(unit string) string
	// Leads is the lead chain over a unit, nearest first: the effective lead
	// of the unit and of each unit above it, each handle once.
	Leads(unit string) []string
	// Within reports whether unit is ancestor or a unit below it.
	Within(unit, ancestor string) bool
}

// RosterSeat is one seat as the grant rules read it.
type RosterSeat struct {
	Handle string
	// Human is a person's seat: a person acts through their own token
	// (ADR-0085), never as a seat, so a human seat asked as an agent gets none.
	Human bool
	// Tier is the seat's role.code.access as written, "" where the document
	// says nothing.
	Tier Tier
	// Unit is a reference to the unit holding the seat — its key in this
	// epoch — and "" for a seat at the chart's root.
	Unit string
}

// Grant is one principal's standing on one repository, and why.
type Grant struct {
	Principal Principal
	RepoID    string
	RepoName  string
	// Owner is the repository's owner: whoever leads it is who a refusal's
	// remedy says to ask.
	Owner Owner
	// Visibility is the repository's, read for an agent only, kept so a read
	// refusal can say why a seat reads nothing.
	Visibility Visibility
	// Tier is the seat's tier, an agent's only.
	Tier Tier
	// Relation is a seat's from the chart (relation); a bound person's is
	// maintain, and a token bound to nobody's read.
	Relation Access
	// Access is what the principal may do OUTSIDE its own namespace: the
	// minimum of Tier.Cap() and Relation for a seat, Relation otherwise.
	Access Access

	// none is why the principal stands nowhere at all, "" for one holding a
	// level. It names no identifier, because [Grant.RefRule] answers it as a
	// command's refusal ([Grant.refused] says why that bounds it).
	none string

	// limit is the grant of the executor whose box asks ([Grant.Within]),
	// and limited whether one does; the zero value is unlimited.
	limit   Access
	limited bool

	// ceiling is the grant of the colleague seat whose ask woke the turn
	// ([Grant.Under]), which every read and every command outside the
	// principal's own namespace is held to; nil for none.
	ceiling *Grant
}

// Within is g held to an executor's grant, what the run's checkout was given
// (read or write): the box does no more than both allow. Outside the seat's
// namespace access is capped at run; where run is below write, no command at
// all, the seat's namespace included, so a box given the repository to read
// cannot borrow the seat's tier. Within(AccessMaintain) is g.
//
// The decide never reads it: a run's grant is coordination state, judged at
// Gitlet's box admission alone; the seat's grant is judged at Gitlet's
// admission and again by the decide (rule 2b).
func (g Grant) Within(run Access) Grant {
	if run >= AccessMaintain {
		return g
	}
	g.limit, g.limited = run, true
	return g
}

// Under is g held to ceiling: the grant ([GrantOf] of an agent principal) of
// the colleague seat whose ask woke g's holder's turn, judged as that seat
// would be asking (the requester ceiling, M3-decisions U40 (a)). A read needs
// both grants; a command needs both RefRules, except the holder's own work
// ([Grant.OwnWork]), which whoever can message the seat steers, as
// authz.DecideWithin spares a steerable verb on what an agent owns.
//
// The ceiling only refuses; one standing nowhere ([WriteCeilingNobody]
// included) refuses all but the holder's own work. A person bounds nothing
// (ADR-0085), so is never a ceiling. The decide never reads it: a box's
// admission and the launch's precheck judge it (PendingRun.CodeCeiling), and
// the Gitlet node building an engine write judges [EngineWrite.Ceiling].
func (g Grant) Under(ceiling Grant) Grant {
	ceiling.ceiling = nil
	g.ceiling = &ceiling
	return g
}

// effective is what the grant lets its holder do outside its own namespace:
// Access, held to a run's grant where one limits it.
func (g Grant) effective() Access {
	if g.limited {
		return min(g.Access, g.limit)
	}
	return g.Access
}

// RunReadDetail refuses every command of a coding box whose run was given the
// repository to read; Gitlet's box admission refuses it first, at the
// advertisement, in words naming the repository.
const RunReadDetail = "this coding box's run was given this repository to read: a push needs access: " +
	"write in its " + toolname.CodeCheckout + " call"

// GrantOf is p's standing on repo, read from r.
//
// It never consults r for an operator (ADR-0085), so a nil roster answers
// every operator and gives every agent none. An agent naming no seat of the
// roster, or a human seat, stands nowhere.
func GrantOf(p Principal, repo RepoRef, r Roster) Grant {
	g := Grant{Principal: p, RepoID: repo.ID, RepoName: repo.Name, Owner: repo.Owner}
	switch p.Kind {
	case PrincipalOperator:
		g.Relation = AccessRead
		if p.Seat != "" {
			g.Relation = AccessMaintain
		}
		g.Access = g.Relation
		return g
	case PrincipalAgent:
	default:
		g.none = "this principal is of a kind this build grants nothing"
		return g
	}
	if r == nil {
		g.none = "this node holds no chart to read a seat's grant from"
		return g
	}
	s, ok := r.Seat(p.Seat)
	switch {
	case !ok:
		g.none = "this seat is no seat of the chart"
		return g
	case s.Human:
		g.none = "this is a human seat: a person acts through their own token, never as a seat"
		return g
	}
	g.Tier = s.Tier
	g.Visibility = repo.Visibility
	g.Relation = relation(s, repo, r)
	g.Access = min(g.Tier.Cap(), g.Relation)
	return g
}

// CredentialPrincipal is a credential acting as no seat, by login: [GrantOf]
// lets it read every repository and push to none. A coding box and its
// checkout's precheck are held [Grant.Under] it where the run's turn works for
// such a credential ([WriteCeilingNobody], ADR-0110); it is the one spelling
// both use, so they cannot judge the run by two grants.
func CredentialPrincipal(login string) Principal {
	return Principal{Kind: PrincipalOperator, OperatorID: login}
}

// AccessOf is GrantOf(p, repo, r).Access.
func AccessOf(p Principal, repo RepoRef, r Roster) Access { return GrantOf(p, repo, r).Access }

// JudgeRead is whether asker may read repo and, where the read is made for a
// seat ([ReadQuery.CeilingSeat]; "" for none), whether that seat could read it
// itself. It answers the asker's grant and [ErrNotPermitted] wrapping the
// asker's [Grant.ReadRefusal], else [ErrAboveCeiling] wrapping the ceiling
// seat's, else nil.
//
// THE ONE JUDGMENT OF A READ, every reader's (the data node's listing, detail,
// history, refs and resolve, and the Gitlet node's read by object id), so none
// can judge the asker and forget whom it reads for. The asker comes first: its
// own refusal is the remedy it can act on. The ceiling is the seat's [GrantOf];
// a human seat stands nowhere there, so the caller never sends one.
func JudgeRead(asker Principal, ceilingSeat string, repo RepoRef, r Roster) (Grant, error) {
	g := GrantOf(asker, repo, r)
	if !g.MayRead() {
		return g, fmt.Errorf("%w: %s", ErrNotPermitted, g.ReadRefusal())
	}
	if ceilingSeat == "" {
		return g, nil
	}
	if c := GrantOf(Principal{Kind: PrincipalAgent, Seat: ceilingSeat}, repo, r); !c.MayRead() {
		return g, fmt.Errorf("%w: %s", ErrAboveCeiling, c.ReadRefusal())
	}
	return g, nil
}

// relation is what the chart relates seat s to repo by, the first row that holds:
//
//	owner unit U:  s is U's effective lead              maintain
//	               s sits in U or a unit below it        write
//	               s leads a unit above U                read
//	owner seat H:  s is H                                maintain
//	               s leads H's unit or a unit above it   read
//	any other seat, visibility company                   read
//	any other seat, visibility unit                      none
//
// Units compare through the roster, never as strings ([Roster]). An unknown
// visibility reads as unit, the narrow direction: [RepoSettings.Validate] records none.
func relation(s RosterSeat, repo RepoRef, r Roster) Access {
	o := repo.Owner
	switch o.Kind {
	case OwnerUnit:
		switch {
		case s.Handle == r.Lead(o.Ref):
			return AccessMaintain
		case s.Unit != "" && r.Within(s.Unit, o.Ref):
			return AccessWrite
		case slices.Contains(r.Leads(o.Ref), s.Handle):
			return AccessRead
		}
	case OwnerSeat:
		if s.Handle == o.Ref {
			return AccessMaintain
		}
		if owner, ok := r.Seat(o.Ref); ok && owner.Unit != "" && slices.Contains(r.Leads(owner.Unit), s.Handle) {
			return AccessRead
		}
	}
	if repo.Visibility == VisibilityCompany {
		return AccessRead
	}
	return AccessNone
}

// MayRead reports whether the principal reads the repository at all — and,
// under a colleague's ceiling ([Grant.Under]), whether that colleague does.
func (g Grant) MayRead() bool {
	return g.effective() >= AccessRead && (g.ceiling == nil || g.ceiling.MayRead())
}

// MayPush reports whether any command could pass [Grant.RefRule]: write or
// above, or a full_access seat that reads the repository (its own namespace),
// and never a box whose run was given it to read ([Grant.Within]). Gitlet
// refuses a push that may not at its advertisement, before any pack is sent.
func (g Grant) MayPush() bool {
	if g.limited && g.limit < AccessWrite {
		return false
	}
	return g.effective() >= AccessWrite || g.ownsNamespace()
}

// PushRefusal is the sentence refusing, at its advertisement, a principal
// that may push nothing here ([Grant.MayPush] false), and "" otherwise. A
// token bound to nobody is told [UnboundDetail], and a box whose run reads
// [RunReadDetail], as each of their commands would be.
func (g Grant) PushRefusal() string {
	switch {
	case g.MayPush():
		return ""
	case g.none != "":
		return g.none
	case g.Principal.Kind == PrincipalOperator:
		return UnboundDetail
	case g.limited && g.limit < AccessWrite:
		return RunReadDetail
	}
	return g.refused("pushing", AccessWrite.String(), g.wouldPass(AccessWrite))
}

// ownsNamespace reports a seat that may push under its own namespace on this
// repository: a full_access tier, and a relation that reads it.
func (g Grant) ownsNamespace() bool {
	return g.Principal.Kind == PrincipalAgent && g.none == "" && g.Tier == TierFullAccess &&
		g.Relation >= AccessRead
}

// outside reports a seat outside the repository's owners (relation below
// write), which pushes only its own namespace there and which the push decide
// holds to [OutsideSeatShare] of the repository's refs and bytes. Persons,
// tokens and the owners' seats are held to the repository's whole bounds.
func (g Grant) outside() bool { return g.Principal.Kind == PrincipalAgent && g.Relation < AccessWrite }

// ReadRefusal is the sentence a read below read is refused with
// ([ErrNotPermitted]): who asked, which repository, and why. A seat the
// visibility shuts out is told which seats do read it. It names the owner as
// the row records it: a read refusal never rides into a proposal, so nothing
// bounds its length as RefRule's are. A principal refused only by its
// colleague's ceiling ([Grant.Under]) is told the colleague's own refusal, as
// [JudgeRead]'s [ErrAboveCeiling] carries it.
func (g Grant) ReadRefusal() string {
	if g.effective() >= AccessRead && g.ceiling != nil && !g.ceiling.MayRead() {
		return fmt.Sprintf("%s reads repository %q, and this work is for the seat whose ask started its "+
			"turn, which could not read it itself: %s", g.who(), g.RepoName, g.ceiling.ReadRefusal())
	}
	why := g.none
	switch {
	case why != "":
	case g.limited && g.limit < AccessRead && g.Access >= AccessRead:
		why = "the executor its box acts for was given no access to it"
	case g.Principal.Kind == PrincipalAgent && g.Relation == AccessNone:
		// The row's own visibility: an unknown one reads as unit, and the
		// sentence says so rather than misstate the row.
		v := string(g.Visibility)
		if g.Visibility != VisibilityUnit {
			v = fmt.Sprintf("%q, which this build reads as %s", g.Visibility, VisibilityUnit)
		}
		why = "it is visibility: " + v + ", read by " + g.unitReaders() + " and by persons"
	default:
		why = fmt.Sprintf("the chart relates it to this repository by %s, and its tier %s allows %s",
			g.Relation, g.Tier, g.Access)
	}
	return fmt.Sprintf("%s holds no access to repository %q: %s", g.who(), g.RepoName, why)
}

// unitReaders names who among the seats reads a `visibility: unit` repository
// of the grant's owner: the members of the owning unit and the leads above it,
// or the owning seat and the leads above its unit.
func (g Grant) unitReaders() string {
	if g.Owner.Kind == OwnerSeat {
		return fmt.Sprintf("seat %q, its owner, and the leads above that seat", g.Owner.Ref)
	}
	return fmt.Sprintf("the seats of unit %q and the leads above it", g.Owner.Ref)
}

// who names the principal in a sentence about it.
func (g Grant) who() string {
	switch g.Principal.Kind {
	case PrincipalOperator:
		return fmt.Sprintf("credential %q", g.Principal.OperatorID)
	case PrincipalAgent:
		return fmt.Sprintf("seat %q", g.Principal.Seat)
	case PrincipalSystem:
		return "the engine's ref expiry"
	}
	return fmt.Sprintf("a principal of kind %q", g.Principal.Kind)
}

// SeatNamespaceRoot is where every seat's own branches live, refs/heads/seats/
// ([gitref.SeatsRoot], whose doc says why), by the domain's name for it.
const SeatNamespaceRoot = gitref.SeatsRoot

// SeatNamespace is the refs a seat owns on every repository it can read:
// SeatNamespaceRoot, its handle and a slash. The ONE spelling every judge of a
// seat's own ref reads, so no two can disagree about whose a ref is.
func SeatNamespace(handle string) string { return SeatNamespaceRoot + handle + "/" }

// seatNamespaceRef reports a ref under some seat's namespace, and whose: the
// first component past [SeatNamespaceRoot], when a component follows it.
func seatNamespaceRef(ref string) (handle string, ok bool) {
	rest, under := strings.CutPrefix(ref, SeatNamespaceRoot)
	if !under {
		return "", false
	}
	handle, _, ok = strings.Cut(rest, "/")
	return handle, ok && handle != ""
}

// reservedSeatRef reports the two kinds of ref under refs/heads/seats that
// are nobody's — refs/heads/seats itself, and one seat's namespace as a
// branch ([gitref.ReservedSeatBranch], which says why).
func reservedSeatRef(ref string) bool {
	branch, ok := strings.CutPrefix(ref, branchPrefix)
	return ok && gitref.ReservedSeatBranch(branch)
}

// UnboundDetail refuses a push by a token bound to nobody: the one spelling,
// printed at the advertisement and on every ref of a push that skipped it. Its
// remedies are the two the directory admits: a person's own machine token, or
// a binding on a human seat nobody holds, never a Tier A token on the person's
// own seat, which the directory refuses since that seat is theirs alone.
const UnboundDetail = "this token is not bound to a person: a person pushes with their own machine " +
	"token, and a pipeline with a Tier A token or service account the identity directory binds to a " +
	"human seat nobody holds; a credential bound to no seat fetches only"

// RefRule judges one command: "" and "" where the grant permits it, else
// [RefusalNotPermitted] and one sentence, naming no ref (git prints it, and a
// proposal's Detail is bounded): what the principal holds, needs, and a remedy.
//
//	a ref StatelessRefusal reserves                 nobody
//	under the principal's own seat namespace        a read relation and full_access
//	under another seat's namespace                  maintain
//	create a branch or a tag, fast-forward          write
//	force or delete a branch, move or delete a tag  maintain
//
// A bound person passes all but the first row; an unbound token is refused with
// [UnboundDetail]. A box is held to [Grant.Within], a ceiling to [Grant.Under].
func (g Grant) RefRule(c Command) (Refusal, string) {
	refusal, sentence := g.ownRefRule(c)
	if refusal != "" || g.ceiling == nil {
		return refusal, sentence
	}
	if g.OwnWork(c) {
		// THE SEAT'S OWN WORK, which whoever messages it steers.
		return "", ""
	}
	if refusal, _ := g.ceiling.RefRule(c); refusal != "" {
		action, need := commandNeeds(c)
		if g.ceiling.Principal.Kind != PrincipalAgent {
			return RefusalNotPermitted, fmt.Sprintf("this work is for whoever started its turn, who may "+
				"change nothing of this company's code; %s needs %s, and they steer only this seat's own "+
				"work — its own namespace (%s<its handle>/) and the repositories it owns", action, need,
				SeatNamespaceRoot)
		}
		return RefusalNotPermitted, fmt.Sprintf("this work is for the seat whose ask started its turn, which "+
			"holds %s on this repository; %s needs %s, which that seat does not hold — push under this "+
			"seat's own namespace (%s<its handle>/) instead", g.ceiling.Access, action, need, SeatNamespaceRoot)
	}
	return "", ""
}

// OwnWork reports whether c is the holder's own work, which whoever its turn
// works for steers ([Grant.Under]): a seat's command under its own namespace,
// or any command in a repository its seat owns, judged on the grant's row (one
// since handed to a unit is steered no more). Exported for internal/gitlet/host's
// merge.go, which holds a seat's merge to its ceiling itself: a merge moves its
// target by a merge record, never a command [Grant.RefRule] judges.
func (g Grant) OwnWork(c Command) bool {
	if g.Principal.Kind != PrincipalAgent {
		return false
	}
	if owner, ok := seatNamespaceRef(c.Ref); ok && owner == g.Principal.Seat {
		return true
	}
	return g.Owner == Owner{Kind: OwnerSeat, Ref: g.Principal.Seat}
}

// WriteCeilingKind is whom a seat's write is made for inside a turn somebody
// woke ([WriteCeiling]): a closed set.
type WriteCeilingKind string

const (
	// WriteCeilingSeat is a colleague seat whose ask woke the turn: the write
	// is decided again as that seat would be asking it itself.
	WriteCeilingSeat WriteCeilingKind = "seat"
	// WriteCeilingNobody is a requester who may change nothing of the
	// company's code: a stranger, a person authz.DecideWithin refused the
	// tool's own row, or a credential acting as no seat, which reads alone
	// ([GrantOf]). Only the seat's own work is steered for them.
	WriteCeilingNobody WriteCeilingKind = "nobody"
)

// WriteCeilingKinds is the closed set.
func WriteCeilingKinds() []WriteCeilingKind {
	return []WriteCeilingKind{WriteCeilingSeat, WriteCeilingNobody}
}

// Valid reports a kind this build knows.
func (k WriteCeilingKind) Valid() bool { return slices.Contains(WriteCeilingKinds(), k) }

// WriteCeiling is the requester ceiling of a seat's write (M3-decisions U40
// (a)): whom the asking turn works for, whose standing bounds the seat's as
// authz.DecideWithin bounds an artifact verb. A write carries it
// ([EngineWrite.Ceiling], [LifecycleRequest.Ceiling], [ChangeProposal.Ceiling],
// [ConversationWrite.Ceiling]); the node deciding the write judges it from the
// seat's grant's chart and row, never the turn's node, which would keep writing
// for a colleague a later activation shut out ([JudgeRead]'s rule).
//
// No ceiling is a write for its seat alone or a person's. Whoever woke the turn
// steers the seat's own work ([Grant.OwnWork], ceilingLifecycleRefusal,
// [WriteCeiling.changeRefusal]); a repository's visibility, owner, archive,
// delete, restore or rename never is, as sharing an artifact is not.
type WriteCeiling struct {
	Kind WriteCeilingKind `json:"kind"`
	// Seat is a [WriteCeilingSeat] ceiling's colleague, by handle; "" for
	// [WriteCeilingNobody].
	Seat string `json:"seat,omitempty"`
}

// invalid is why c may not bound a write asker makes, or "": only a seat's
// write is made for somebody else, a kind this build knows, and a colleague
// named by a handle that is not the asker's own — a seat's own voice is its
// own work, never a ceiling.
func (c WriteCeiling) invalid(asker Principal) string {
	switch {
	case asker.Kind != PrincipalAgent:
		return "ceiling: only a seat's write is made for somebody else — a person's write is their own, " +
			"which no ceiling bounds"
	case !c.Kind.Valid():
		return fmt.Sprintf("ceiling.kind: %q is not one of %s", c.Kind, words.Or(WriteCeilingKinds()))
	case c.Kind == WriteCeilingNobody && c.Seat != "":
		return "ceiling.seat: a ceiling of nobody names no seat"
	case c.Kind == WriteCeilingSeat && !org.ValidHandle(c.Seat):
		return fmt.Sprintf("ceiling.seat: %q is not a seat's handle", c.Seat)
	case c.Kind == WriteCeilingSeat && c.Seat == asker.Seat:
		return "ceiling.seat: a seat's write is never made for the seat itself"
	}
	return ""
}

// writtenForInvalid is why a request asker makes may not state f as whom it
// is made for (ADR-0109), or nil: every code-write request's Validate holds
// it, so every node holding the request does, each wrapping it ([refusedCarrier]).
//
//   - first, a kind or app unknown here is a newer build's, never malformed:
//     [unknownToThisBuild] answers it as on the wire, asking a node that knows;
//   - an agent's request states a carrier or nobody, any other carries nobody
//     ([Principal.WriterKind], [types.WrittenFor.For]);
//   - a carrier is a seat's handle, or a machine login (iam.ValidLoginFor) with
//     its turn's apps, known and distinct, so at most [MaxCarrierBytes];
//   - never a stranger's (ADR-0110), nor a credential's naming no app, which
//     every reader takes for one (internal/agent/builtin's credentialCeiling).
func writtenForInvalid(asker Principal, f types.WrittenFor) error {
	if err := unknownToThisBuild(writtenForAt, f); err != nil {
		return err
	}
	carrier, err := f.For(asker.WriterKind())
	switch {
	case err != nil:
		return fmt.Errorf("written_for: %w", err)
	case carrier == nil:
		return nil
	}
	switch carrier.Kind {
	case types.WorkingForSeat:
		switch {
		case !org.ValidHandle(carrier.Seat):
			return fmt.Errorf("written_for.seat: %q is not a seat's handle", carrier.Seat)
		case carrier.Login != "" || len(carrier.Apps) > 0:
			return errors.New("written_for: a seat's carrier names the seat alone, with no login and no app")
		}
		return nil
	case types.WorkingForCredential:
		switch {
		case !iam.ValidLoginFor(iam.KindMachine, carrier.Login):
			return fmt.Errorf("written_for.login: %q is no credential's login: a credential acting as no seat "+
				"is named by its coloned handle", carrier.Login)
		case carrier.Seat != "":
			return errors.New("written_for.seat: a credential's carrier names no seat")
		case len(carrier.Apps) == 0:
			return errors.New("written_for.apps: a credential's carrier names the apps its turn was held to, " +
				"and one naming none is a stranger's to every reader, for whom no code write is made")
		}
		for i, app := range carrier.Apps {
			switch {
			case !app.IsApp():
				return fmt.Errorf("written_for.apps: %q is no app: a credential's carrier names the apps its "+
					"turn was held to", app)
			case slices.Contains(carrier.Apps[:i], app):
				return fmt.Errorf("written_for.apps: %q is named twice", app)
			}
		}
		return nil
	case types.WorkingForStranger:
		return errors.New("written_for: no code write is made for a stranger: the turn's own gate refuses " +
			"every code tool to whoever the company does not know (ADR-0110)")
	}
	return errors.New("written_for.kind: a carrier names whom a write is made for by its kind, and this one " +
		"names none: a code write is made for a seat or a credential")
}

// writtenForUnder is [writtenForInvalid] of a request a ceiling rides on (a
// change's lifecycle and conversation, an engine write, a dequeue), its carrier
// held to c (nil for none). The engine composes both from one bound ceiling
// (internal/agent/builtin's writeCaller, writtenForTurn); a write whose two
// disagreed would be decided for one requester and wake another's turns. A
// colleague seat's carrier pairs with its ceiling, a credential's with
// nobody's, a person's seat with none ([WriteCeiling]); a nil carrier, an older
// build's ([ChangeProposal.UnmarshalJSONFrom]), is held to none (ADR-0109's
// roll). Only roster, the deciding chart (nil in a Validate), tells a person's
// seat from a colleague's; a seat it does not place is held as a colleague's,
// lest its write escape its ceiling, and its refusal says the chart does not
// place it. The chart's refusals are typed [ChartRefusal] ([invalidLevel]).
func writtenForUnder(asker Principal, f types.WrittenFor, c *WriteCeiling, roster Roster) error {
	if err := writtenForInvalid(asker, f); err != nil {
		return err
	}
	carrier := f.Carrier()
	if carrier == nil {
		return nil
	}
	var seat RosterSeat
	placed := false
	if roster != nil && carrier.Kind == types.WorkingForSeat {
		seat, placed = roster.Seat(carrier.Seat)
	}
	person := placed && seat.Human
	switch {
	case c == nil && carrier.Kind == types.WorkingForCredential:
		return fmt.Errorf("written_for: a write made for credential %s carries the ceiling of nobody, and this "+
			"one carries none", carrier.Login)
	case c == nil && roster != nil && !placed:
		return &ChartRefusal{Why: fmt.Sprintf("written_for: this node's chart does not place seat %s, so a write "+
			"made for it is held as a colleague's, which carries that seat's ceiling, and this one carries none",
			carrier.Seat)}
	case c == nil && roster != nil && !person:
		return &ChartRefusal{Why: fmt.Sprintf("written_for: a write made for seat %s, which this node's chart holds "+
			"as an agent's, carries that seat's ceiling, and this one carries none", carrier.Seat)}
	case c == nil:
		return nil
	case c.Kind == WriteCeilingSeat && (carrier.Kind != types.WorkingForSeat || carrier.Seat != c.Seat):
		return fmt.Errorf("written_for: a write held to seat %s's ceiling is made for that seat, and this one is "+
			"made for %s", c.Seat, carried(carrier))
	case c.Kind == WriteCeilingSeat && person:
		return &ChartRefusal{Why: fmt.Sprintf("written_for: seat %s is a person's, who bounds nothing, so a write "+
			"made for them carries no ceiling, and this one carries theirs", carrier.Seat)}
	case c.Kind == WriteCeilingNobody && carrier.Kind != types.WorkingForCredential:
		return fmt.Errorf("written_for: a write held to the ceiling of nobody is made for a credential acting "+
			"as no seat, and this one is made for %s", carried(carrier))
	}
	return nil
}

// carried names whom a carrier says a write is made for, in a refusal.
func carried(w *types.WorkingFor) string {
	if w.Kind == types.WorkingForCredential {
		return "credential " + w.Login
	}
	return "seat " + w.Seat
}

// ChartRefusal is a refusal of whom a write is made for that the deciding
// chart alone makes ([writtenForUnder]). No sender erred: the turn composed
// carrier and ceiling from its own node's chart, so they disagree only where
// the chart moved under the turn or the deciding node's lags, an expected race
// logged at WARN ([invalidLevel]). Its class stays [ErrInvalidProposal]: the
// deciding chart refuses the request as it stands, so resending it unchanged
// is no remedy. Exported because every error type of this package crosses the
// estate's wire whole (internal/estate's typed kinds), so an asker's errors.As
// finds it.
type ChartRefusal struct {
	// Why is the refusal's sentence: the seat whom the write is made for, and
	// what the deciding chart holds of it.
	Why string
}

func (r *ChartRefusal) Error() string { return r.Why }

// invalidLevel is the level the deciding node logs an [ErrInvalidProposal]
// refusal at: WARN for a [ChartRefusal], a chart that moved under the turn,
// and ERROR for every other, which no chart makes right and only a sender's
// bug forms.
func invalidLevel(err error) slog.Level {
	if errors.As(err, new(*ChartRefusal)) {
		return slog.LevelWarn
	}
	return slog.LevelError
}

// Grant is the ceiling's own standing on repo, read from r — the grant
// [Grant.Under] holds a seat's to: a colleague's as [GrantOf] reads that
// seat, and nobody's standing nowhere.
func (c WriteCeiling) Grant(repo RepoRef, r Roster) Grant {
	if c.Kind == WriteCeilingSeat {
		return GrantOf(Principal{Kind: PrincipalAgent, Seat: c.Seat}, repo, r)
	}
	return Grant{RepoID: repo.ID, RepoName: repo.Name, Owner: repo.Owner,
		none: "whoever this turn works for may change nothing of this company's code"}
}

// changeRefusal is why a seat's write of a change ([ChangeProposal] or
// [ConversationWrite]) may not be made for whoever its turn works for, or "":
// judged by the committing data node from the same chart and snapshot, once
// the seat itself may write (M4-decisions D30). Anybody steers the seat's own
// work (own: a change it owns or opens from its own namespace, or any change
// in a repository it owns, as [Grant.OwnWork]); nobody steers nothing else; a
// colleague needs only need, the least access the decide admits at
// (internal/agent/builtin's changeToolNeeds). The rest of the relation is the
// seat's alone to meet: judging it for the colleague would refuse the very ask
// a ceiling exists for, an author asking a seat to review. A nil c is a write
// for its seat alone, or a person's.
func (c *WriteCeiling) changeRefusal(asker Principal, repo RepoRef, own bool, need Access, what string,
	roster Roster) string {

	if c == nil || own || repo.Owner == (Owner{Kind: OwnerSeat, Ref: asker.Seat}) {
		return ""
	}
	if c.Kind != WriteCeilingSeat {
		return fmt.Sprintf("this work is for whoever started this turn, who may change nothing of this company's "+
			"code, so seat %s may not %s for them: they steer only its own work — its own changes, a change of "+
			"a branch under its own namespace (%s<its handle>/), and the changes of a repository it owns",
			asker.Seat, what, SeatNamespaceRoot)
	}
	g := GrantOf(Principal{Kind: PrincipalAgent, Seat: c.Seat}, repo, roster)
	if g.Access >= need {
		return ""
	}
	return fmt.Sprintf("this work is for seat %s, whose ask started this turn, which holds %s on this repository, "+
		"so seat %s may not %s for it: that needs %s, which %s does not hold", c.Seat, g.Access, asker.Seat, what,
		need, c.Seat)
}

// ownRefRule is [Grant.RefRule] for the principal's own grant, held to its
// run's. The reserved-ref row holds even for a caller that skipped
// [StatelessRefusal], and [PrincipalSystem] may only delete a seat's branch,
// all [Proposal.Validate] lets the expiry's commands do.
func (g Grant) ownRefRule(c Command) (Refusal, string) {
	if reservedRef(c.Ref) {
		return RefusalNotPermitted, "no principal pushes a ref the engine reserves: " +
			RefusalReservedRef.Sentence()
	}
	if g.Principal.Kind == PrincipalSystem {
		if _, ok := seatNamespaceRef(c.Ref); ok && c.New == ZeroOID {
			return "", ""
		}
		return RefusalNotPermitted, "the engine's ref expiry deletes a seat's branch and does nothing else"
	}
	switch {
	case g.none != "":
		return RefusalNotPermitted, g.none
	case g.Principal.Kind == PrincipalOperator && g.Access < AccessMaintain:
		return RefusalNotPermitted, UnboundDetail
	case g.limited && g.limit < AccessWrite:
		return RefusalNotPermitted, RunReadDetail
	}
	if owner, ok := seatNamespaceRef(c.Ref); ok && g.Principal.Kind == PrincipalAgent &&
		owner == g.Principal.Seat {
		if g.ownsNamespace() {
			return "", ""
		}
		return RefusalNotPermitted, g.refused("pushing under its own namespace",
			"a read relation and the full_access tier", false)
	}
	action, need := commandNeeds(c)
	switch {
	case g.effective() >= need:
		return "", ""
	case g.Access >= need:
		// THE RUN'S GRANT, not the seat's, is what stands in the way: the
		// seat may, and its box may not.
		return RefusalNotPermitted, fmt.Sprintf("this coding box holds %s on this repository, as its run "+
			"was given it; %s needs %s, which its seat holds and its box does not", g.effective(), action, need)
	}
	return RefusalNotPermitted, g.refused(action, need.String(), g.wouldPass(need))
}

// commandNeeds is what a command does, as a sentence names it, and the level
// it needs (RefRule's table).
func commandNeeds(c Command) (string, Access) {
	if _, ok := seatNamespaceRef(c.Ref); ok {
		return "pushing under another seat's namespace", AccessMaintain
	}
	tag := strings.HasPrefix(c.Ref, tagPrefix)
	switch {
	case c.New == ZeroOID && tag:
		return "deleting a tag", AccessMaintain
	case c.New == ZeroOID:
		return "deleting a branch", AccessMaintain
	case c.Old == ZeroOID && tag:
		return "creating a tag", AccessWrite
	case c.Old == ZeroOID:
		return "creating a branch", AccessWrite
	case tag:
		return "moving a tag", AccessMaintain
	case c.Forced:
		return "force-pushing a branch", AccessMaintain
	}
	return "updating a branch", AccessWrite
}

// wouldPass reports whether the full_access tier alone would let the seat do
// what needs need: whether its relation, uncapped, reaches it.
func (g Grant) wouldPass(need Access) bool {
	return min(TierFullAccess.Cap(), g.Relation) >= need
}

// refused is an agent's refusal sentence: what it holds and why, what the
// command needs, and a remedy that would admit it (its own namespace, the
// full_access tier, or whoever leads the repository's owner). It names nothing
// the chart or client spells (no handle, ref or repository), so its length is
// the same everywhere: a proposal's Detail is bounded (maxDetailBytes), and a
// handle only by a ref's room. The namespace is named by its pattern.
func (g Grant) refused(action, need string, fullAccessPasses bool) string {
	held := fmt.Sprintf("this seat holds %s on this repository (tier %s, relation %s); %s needs %s",
		g.Access, g.Tier.spelled(), g.Relation, action, need)
	ns := "its own namespace (" + SeatNamespaceRoot + "<its handle>/)"
	var remedy string
	switch {
	case g.ownsNamespace():
		remedy = fmt.Sprintf("push under %s instead, or ask %s", ns, g.lead())
	case g.Tier == TierFullAccess:
		remedy = "ask " + g.lead()
	case fullAccessPasses:
		remedy = "a person can set its role.code.access to full_access"
	case g.Relation >= AccessRead:
		remedy = fmt.Sprintf("with role.code.access full_access it pushes under %s; or ask %s", ns, g.lead())
	default:
		remedy = fmt.Sprintf("ask %s, and a person to set its role.code.access to full_access", g.lead())
	}
	return held + " — " + remedy
}

// lead is who a seat asks for more on this repository: the relation is the
// chart's, and whoever leads the repository's owner is who moves it.
func (g Grant) lead() string {
	if g.Owner.Kind == OwnerSeat {
		return "the seat that owns the repository"
	}
	return "the lead of the repository's owning unit"
}
