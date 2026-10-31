package code

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
)

// WHO MAY DO WHAT TO A REPOSITORY — a grant DERIVED from the chart, never
// stored (ADR-0058). A list of who may push to a repository would be a second
// chart, and it would drift from the first the day a seat moved units; so a
// principal's standing on a repository is computed, every time it is asked,
// from the repository's row and the epoch's chart, through ONE function
// ([GrantOf]) that every judge of it calls: the code host's admission, the
// push decide (its rule 2b, decide.go, the authority, from the DECIDING node's
// chart) and the read tools — and a coding box's admission, which holds the
// seat's grant to its run's ([Grant.Within]).
//
// A PERSON — an operator credential bound to a human seat — maintains every
// repository, and a token bound to nobody reads every one: ADR-0055's rules,
// which need no chart, so [GrantOf] never consults the roster for an operator.
// A SEAT holds the minimum of two levels: what its tier lets it do anywhere
// (role.code.access, [Tier.Cap]) and what the chart relates it to this
// repository by (its RELATION: the owning unit's effective lead maintains, the
// owning unit's subtree writes, the lead chain above reads, and every other
// seat reads a `visibility: company` repository and nothing of a `visibility:
// unit` one). VISIBILITY NARROWS SEATS ONLY: it is read in the last row of a
// seat's relation and nowhere else, so a person's standing never depends on it.
// A full_access seat also owns a namespace of its own on every
// repository it may read, [SeatNamespace], which is where it pushes when its
// relation is below write — the seat's own branches replace forks. M2's
// protection rules are judged AFTER the grant, by their own code, and a grant
// is never a reason to skip one.

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
	// AccessReview is read, and (M4) reviewing changes; it moves no ref.
	AccessReview
	// AccessWrite creates and fast-forwards branches, and creates tags.
	AccessWrite
	// AccessMaintain also forces and deletes branches, deletes tags, and
	// (M3 P5) changes a tool-managed repository's settings.
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

// Tier is a seat's role.code.access, in configuration's own spelling — the
// constants ARE configuration's, so the two cannot drift, and
// config.TestRoleCodeAccessIsTheOneVocabulary holds the lists equal anyway,
// because internal/github spells its own.
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

// Cap is the most the tier lets a seat do on any repository: read_only reads,
// review reviews, full_access maintains. UNSET reads, the least this engine
// hands out — as configuration's own default is (config.RoleCode.AccessOrDefault)
// — and a value no build of this tree writes caps at nothing, the narrow
// direction, rather than at the widest level a reader might guess it meant.
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

// spelled is the tier as a refusal sentence names it: [Tier.String] for a
// tier this build knows or an unset one, and the same words for every other
// value — which caps at nothing ([Tier.Cap]) — so no value a document carries
// can lengthen a sentence that travels in a proposal.
func (t Tier) spelled() string {
	if t != "" && !t.Valid() {
		return "unknown"
	}
	return t.String()
}

// Roster is the chart as the grant rules read it: the SERVING or DECIDING
// node's current epoch, adapted by the engine over its organization. This
// package declares it and never reads the organization itself, so a grant is a
// pure function of a repository row and these four answers.
//
// EVERY UNIT ARGUMENT IS A REFERENCE, never a key compared as a string: an
// [Owner]'s Ref — durable, written when the repository was created or declared
// — or a [RosterSeat]'s Unit. A roster resolves each the way the chart
// resolves a durable unit reference (internal/org's Index.UnitByRef: folded,
// an id before a name), because a unit that gained an id after a repository
// named it by its name is still that unit, and a roster that looked the
// reference up as the unit's current key would take its lead's maintain and
// its members' write away on their own repository. A reference the chart does
// not resolve — "" included, a root seat's unit — is a unit with no lead and no
// members, within nothing.
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
	// Human is a person's seat: a person acts through their own bound token
	// (ADR-0055), never as a seat, so a grant asked for a human seat as an
	// agent is none.
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
	// Owner is the repository's owner, which a refusal's remedy names the
	// kind of: who to ask for more is whoever leads it.
	Owner Owner
	// Visibility is the repository's — read for an agent only, the one kind
	// it narrows, and kept so a read refusal can say why a seat reads nothing.
	Visibility Visibility
	// Tier is the seat's tier — an agent's only.
	Tier Tier
	// Relation is what the principal is related to the repository by: a
	// seat's from the chart (the owning unit's effective lead or the owning
	// seat maintains, the owning unit's subtree writes, the lead chain above
	// it reads, any other seat reads); a bound person's is maintain, and a
	// token bound to nobody's read.
	Relation Access
	// Access is what the principal may do OUTSIDE its own namespace: the
	// minimum of Tier.Cap() and Relation for a seat, Relation otherwise.
	Access Access

	// none is why the principal stands nowhere at all — no seat of the chart,
	// a human seat asked as an agent, a node with no chart, a kind this build
	// does not grant — and "" for every principal that holds a level. It
	// names no identifier, because [Grant.RefRule] answers it as a command's
	// refusal ([Grant.refused] says why that bounds it).
	none string

	// limit is the grant of the CODING RUN whose box asks ([Grant.Within]),
	// which the principal's own is held to, and limited whether one does: a
	// grant no run holds is unlimited, the zero value.
	limit   Access
	limited bool
}

// Within is g held to a coding run's grant — what the run's run_sandbox call
// was given of the repository, read or write: its box does no more than BOTH
// the run's grant and the seat's own allow. Access no higher than run outside
// the seat's own namespace; and, where run is below write, no command at all,
// the seat's own namespace included, since that row needs only the seat's tier
// and its relation, which a box given the repository to read must not borrow.
// Within(AccessMaintain) is g.
//
// The decide never reads it: a run's grant is coordination state, judged at
// the code host's box admission alone, while the seat's grant is judged at the
// host and again by the decide (rule 2b).
func (g Grant) Within(run Access) Grant {
	if run >= AccessMaintain {
		return g
	}
	g.limit, g.limited = run, true
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

// RunReadDetail is the sentence every command of a coding box whose run was
// given the repository to read is refused with — which the code host's box
// admission refuses at the push's advertisement first, in words naming the
// repository.
const RunReadDetail = "this coding box's run was given this repository to read: a push needs access: " +
	"write in its run_sandbox call"

// GrantOf is p's standing on repo, read from r.
//
// It NEVER CONSULTS r FOR AN OPERATOR — a person or a token, whose rules need
// no chart (ADR-0055) — so a nil roster answers every operator, and gives
// every agent none. An agent naming no seat the roster holds, or a HUMAN seat,
// stands nowhere: a person acts through their own token, never as a seat.
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

// AccessOf is GrantOf(p, repo, r).Access.
func AccessOf(p Principal, repo RepoRef, r Roster) Access { return GrantOf(p, repo, r).Access }

// JudgeRead is whether asker may read repo — and, where the read is made for
// the seat its turn is acting for ([ReadQuery.CeilingSeat]; "" for none),
// whether that seat could read it itself — answering the asker's grant, which
// names the asker's access, and the refusal: [ErrNotPermitted] wrapping the
// asker's [Grant.ReadRefusal] where its own grant reads nothing of the
// repository, else [ErrAboveCeiling] wrapping the ceiling seat's where that
// seat's reads nothing of it, else nil.
//
// THE ONE JUDGMENT OF A READ, every reader's — the data node's listing,
// detail, history, refs and resolve, and the git node's read by object id —
// so the two principals are judged by one chart and one row in one place, and
// no reader can judge the asker and forget whom it reads for. The asker
// first: an asker that reads nothing is told so about ITSELF, which is the
// remedy it can act on, and a read it may make is bounded after.
//
// THE CEILING IS A SEAT'S GRANT, judged as the seat itself would be judged
// asking ([GrantOf] for an agent principal): its tier, its relation to the
// repository on the chart, the repository's visibility. A HUMAN seat as the
// ceiling stands nowhere — a person is never asked as a seat — so the caller
// never sends one: a person maintains every repository and bounds nothing.
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

// relation is what the chart relates seat s to repo by — the FIRST row that
// holds, top to bottom:
//
//	owner unit U:  s is U's effective lead              maintain
//	               s sits in U or a unit below it        write
//	               s leads a unit above U                read
//	owner seat H:  s is H                                maintain
//	               s leads H's unit or a unit above it   read
//	any other seat, visibility company                   read
//	any other seat, visibility unit                      none
//
// Every unit comparison is the roster's — [Roster.Within] and the lead chain —
// never a string compared here, because the owner's Ref is a durable reference
// that may spell the unit another way than the seat's Unit does.
//
// THE LAST ROW IS VISIBILITY'S, AND ONLY IT: a `unit` repository is read by the
// rows above it and by no other seat. A visibility this build does not know
// reads as `unit` — the narrow direction, as an unknown tier caps at nothing —
// since the domain refuses to record one ([RepoSettings.Validate]), so a row
// carries one only from a newer build, and widening it to every seat would be
// guessing that build meant `company`.
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

// MayRead reports whether the principal reads the repository at all.
func (g Grant) MayRead() bool { return g.effective() >= AccessRead }

// MayPush reports whether any command at all could pass [Grant.RefRule]: a
// principal at write or above, or a full_access seat that reads the repository
// (its own namespace) — and in either case not a coding box whose run was
// given the repository to read ([Grant.Within]). The code host refuses a push
// that may not at its advertisement, before the client sends a byte of pack.
func (g Grant) MayPush() bool {
	if g.limited && g.limit < AccessWrite {
		return false
	}
	return g.effective() >= AccessWrite || g.ownsNamespace()
}

// PushRefusal is the sentence a principal that may push nothing here
// ([Grant.MayPush] false) is refused with before any command is read — at
// its advertisement — and "" for one that may push something. A token bound
// to nobody is told [UnboundDetail], the sentence every one of its commands
// would be refused with, and a box whose run reads [RunReadDetail].
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

// outside reports a seat OUTSIDE the repository's owners: an agent whose
// relation to it is below write — every seat but the owning unit's subtree
// and its effective lead, or but the owning seat — which may push nothing but
// its own namespace there, and which the push decide holds to
// [OutsideSeatShare] of the repository's refs and bytes. A person and a token
// are no seat, and a seat of the owners is held to the repository's whole
// bounds as a person is.
func (g Grant) outside() bool { return g.Principal.Kind == PrincipalAgent && g.Relation < AccessWrite }

// ReadRefusal is the sentence a read below read is refused with
// ([ErrNotPermitted]): who asked, which repository, and why the principal
// stands nowhere on it.
//
// A seat the visibility shuts out is told so, and told who among the seats
// does read the repository — the people a person would move the seat beside,
// or ask on its behalf. It names the owner as the repository's row records
// it: a read refusal answers whoever asked and is never carried into a
// proposal, so nothing bounds its length the way RefRule's sentences are.
func (g Grant) ReadRefusal() string {
	why := g.none
	switch {
	case why != "":
	case g.limited && g.limit < AccessRead && g.Access >= AccessRead:
		why = "the coding run its box acts for was given no access to it"
	case g.Principal.Kind == PrincipalAgent && g.Relation == AccessNone:
		// THE ROW'S OWN VISIBILITY, never the word unit spelled here: one
		// this build does not know is read as unit, and the sentence says
		// so rather than claim the repository says what it does not.
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

// SeatNamespaceRoot is where every seat's own branches live: a seat's
// namespace is refs/heads/seats/<handle>/, never refs/heads/<handle>/ —
// gitref's [gitref.SeatsRoot], whose doc says why, read here by the domain's
// name for it.
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

// UnboundDetail is the sentence a push by a token bound to nobody is refused
// with — the ONE spelling, which the code host prints at the advertisement and
// on every ref of a push that skipped it.
//
// ITS REMEDIES ARE THE TWO A DIRECTORY ADMITS: a person's own machine token,
// which acts as the seat they hold, and a binding on a human seat NOBODY
// holds — never a Tier A token bound to the person's own seat, which the
// directory refuses, since a bind is a claim on the seat and a person's seat
// is theirs alone.
const UnboundDetail = "this token is not bound to a person: a person pushes with their own machine " +
	"token, and a pipeline with a Tier A token or service account the identity directory binds to a " +
	"human seat nobody holds; a credential bound to no seat fetches only"

// RefRule judges one command against the grant: "" and "" where the grant
// permits it, else [RefusalNotPermitted] and one sentence naming what the
// principal holds, what the command needs, and a remedy the rule itself would
// admit.
//
//	a ref StatelessRefusal reserves             nobody (the stateless rule refuses it first)
//	under the principal's own seat namespace    a read relation and the full_access tier
//	under another seat's namespace              maintain
//	create or fast-forward a branch             write
//	force or delete a branch                    maintain
//	create a tag                                write
//	move or delete a tag                        maintain
//
// A bound person maintains and passes every row but the first; a token bound
// to nobody reads and is refused every one with [UnboundDetail]. The first row
// never reaches RefRule from a caller that asks [StatelessRefusal] first, as
// every caller does — it answers it all the same, so a caller that skipped the
// stateless check still lands nothing the engine reserves. M2's protection
// rules are judged after it, by their own code. A coding box's grant
// ([Grant.Within]) is held to its run's: every row refused where the run was
// given the repository to read, and every row outside the seat's own
// namespace held to what the run was given.
//
// THE ENGINE'S OWN ACCOUNT ([PrincipalSystem]) is granted nothing ([GrantOf])
// but the one thing its expiry does: deleting a branch in a seat's namespace,
// which [Proposal.Validate] already holds its every command to — so this row
// keeps the decide's word on it should a command of any other shape ever
// reach it.
//
// THE SENTENCE NAMES NO REF: git prints the ref beside the reason, and a
// receiver carries the sentence into a proposal, whose every Detail is bounded.
func (g Grant) RefRule(c Command) (Refusal, string) {
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
// command needs, and a remedy that would admit it — its own namespace where
// the seat owns one; the full_access tier where the tier alone stands in the
// way; the namespace that tier would give it where its relation reads the
// repository; else whoever leads the repository's owner, whose chart the
// relation is, and the tier too where the seat lacks it.
//
// IT NAMES NOTHING THE CHART OR THE CLIENT SPELLS — no handle, no ref, no
// repository — only this package's own words for levels and tiers, so its
// length is the same for every seat on every repository: a receiver carries
// the sentence into a proposal, whose every Detail is bounded
// (maxDetailBytes), and a handle is bounded by nothing but a ref's room. The
// seat's namespace is named by its pattern, refs/heads/seats/<its handle>/,
// which a seat reads with its own handle; git prints the ref beside the reason.
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
