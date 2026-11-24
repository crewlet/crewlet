// Package iam is the engine's identity vocabulary: WHO is acting, what sort of
// thing they are, how far through enrolment they got, and which capabilities
// they carry.
//
// # Why it is a leaf, and why that is the point
//
// The engine's API today knows an OPERATOR TOKEN rather than a person:
// [internal/api/auth] resolves a request to an id and a bool. Real identity —
// people, sessions, credentials, grants — needs a name that config, the tool
// layer and the query registry can all say without any of them pulling in a
// password hasher, a store client or a coordination backend. So this package
// holds VALUES ONLY: named string types, one struct, pure functions over them
// and one context key. Nothing here opens a file, dials anything or hashes
// anything, and leaf_test.go walks the imports and fails the build on the day somebody
// reaches for a store handle to "just look the principal up here". The one
// function that needs a read it cannot make — [OwnerOf], whose record
// somebody else's login names — is HANDED it, as a seam ([Holders]) the engine
// implements over the identity directory, and is handed the authority decision
// that comes BEFORE that read ([MayLook]) the same way; the rule stays here
// because the tools, the questions and the routes all have to answer it the
// same way, and this is the one package all three already import.
//
// # Three namespaces that can never collide
//
// Every actor this engine durably records is written under a NAME, and the
// name alone has to be enough: internal/api/opsmcp used to prefix an
// operator's with "operator:" and the audit feed showed one person as two
// people three rows apart, because a work commit carried the bare id and a
// page commit carried the prefixed one. The prefix is gone, the author KIND is
// a column, and what keeps the names apart now is the SHAPE OF THE VALUE —
// worklet/rank.go's move, applied to names instead of ranks:
//
//   - a SEAT HANDLE is one segment, [a-z0-9][a-z0-9-]* — [ValidSeatHandle];
//   - a PERSON's login is segments joined by DOTS — jane.doe;
//   - a MACHINE's handle is segments joined by a COLON — ci:release.
//
// Both of the login grammars therefore REQUIRE a separator, which is the one
// thing a seat handle can never carry, and they require different ones, which
// is what makes them disjoint from each other. No collision is possible, in
// either direction, by construction rather than by a uniqueness check some
// later writer has to remember to run.
//
// ALL THREE GRAMMARS LIVE HERE, the seat handle's included, so "by
// construction" holds wherever a name is asked about: a second copy of the
// seat rule that disagreed by one character would accept `jane.doe`,
// `ci:release` or `token:ops` as a seat, after which [OwnerOf] sent every name
// of that shape to the identity directory rather than to the seat. One
// definition, here, is what every path that creates a seat asks.
//
// THE ENGINE'S OWN NAMES ([EngineNames]) have a seat handle's shape and are
// no seat's: the engine writes its lines in a room under them, beside the
// seats' words, and the org chart refuses a seat one (internal/org), so a line
// read back by its name alone is never handed to a seat, nor a seat's to the
// engine.
//
// AND BOTH ARE BOUNDED AT [MaxLogin], a seat handle's own width: a login is
// the name in an author column beside a seat handle, and a login is a SUBJECT
// TOKEN the broker indexes for the life of the deployment, so a width nothing
// bounded could not be taken back. The bound is IN the grammar, so every
// surface that enrols, renames or composes a login is held to it by asking the
// one question it already asks.
//
// AND EACH GRAMMAR BELONGS TO ONE KIND — [ValidLoginFor] — because a name is
// read back as a claim about the kind that holds it. `token:<id>` is the login
// a Tier A token acts under, and the identity directory's row under it is what
// binds that token to a seat, so a person free to choose a coloned login could
// make the deployment's own credential act as their seat.
//
// # Nothing here decides anything
//
// A gate asks [Principal.Can]; a store writes what [ActorFor] returns. What a request actually presented, and
// whether the answer could be reached at all, is [From]'s three-valued job:
// the engine knowing you are nobody and the engine being unable to tell are
// different answers.
//
// [ActorFor] also answers the CREDENTIAL ([Actor.OperatorID]): a machine token
// acting as its owner is recorded as `pat:<id>` and a browser session as
// `session:<lineage>`, because without it every write a token made read
// exactly as its owner's own. No login may take either class, so each name
// only ever means its credential. A third class, `assist:<grant>` (ADR-0050),
// is a request the Dashboard Assistant made for a person: the principal is the
// person, and [Principal.AssistScope] is set exactly when [Principal.Via]
// names a grant, on a person and nobody else.
//
// # Permissions, and what is everybody's
//
// A permission is the only authority a principal carries beside its relations
// (internal/authz asks those); everything else is everybody's. A company
// decides about somebody who joins whether they RUN it and which apps they USE,
// so the set is seven (ADR-0033) and a colleague level stored beside it was
// removed rather than kept as a setting that changes nothing. [Expand] is the
// one comparison, [Apps] the one declaration of an app and [OptInApps] the one
// marking of an app given only on purpose. Nobody confers what they do not
// hold, on anybody, themselves included. A name this build does not know is
// kept and ignored in a replicated row (it may be removed, never added), while
// Tier A refuses one: a file an operator wrote is a typo, not an upgrade.
// [GrantAdmin] is host access by decision, one permission with no runtime split.
//
// [AdminPosture] is `api.auth.admin` as a value, here for [SecondFactor]'s
// reason. [AdminPosture.Effective] runs [Expand] FIRST and clamps admin SECOND,
// so a refusing node keeps every app, and an invalid posture, the zero
// included, confers nothing: it fails closed.
//
// A machine token may carry admin (a deploy job applying configuration is what
// a token is for). What keeps a token off a person-present gesture is
// internal/authz's `presence` rows and internal/iamdomain's refusal of an
// administrative record written through one, never a permission it cannot hold.
//
// # Proof
//
// proof.go holds the two values config reads about proof ([SecondFactor],
// [MinPasswordChars]) here and not in iam/credential, so config can say "a
// second factor is required" without linking a password hasher. [Recency] has
// ONE window, `step_up`, over every sensitive gesture, as GitHub's sudo mode
// keeps one: a second, shorter window sent people to re-prove inside the hour
// they had proved in, while what keeps a stolen machine credential from those
// gestures was never a proof's age.
//
// # Whose record a name is, and why it is one function
//
// A person's inbox, pins, priorities and personal views are kept under ONE
// name — [RecordOwner]'s, the name their writes are attributed to — and every
// surface that reads or writes somebody's record resolves the name it was
// given through [OwnerOf]: the caller's own names are their own record,
// somebody else's LOGIN is their holder's record (a bound person's seat), and
// anything else is the chart's. A bound principal's login and seat name the
// same record ([NamesSelf]); an unbound token or service account's record is
// under its login, because the tools writing under the login and the query
// surface reading under the seat once left such a record where nothing read it.
// A `token:<id>` login is held by the Tier A entry of that id on the answering
// node, and by nobody when none declares it. A login is never a seat — [NamesLogin] — so it
// is never matched against the chart's roster, where `jane.doe` resembles the
// seat `jane` closely enough to land on it. And the directory is asked only
// once the caller's authority is decided as far as it can be without the
// record ([MayLook]), because what the directory says differs by login and a
// caller it could never admit must not learn any of it.
package iam

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// Kind is what sort of thing a principal is.
//
// A NAMED STRING WHOSE ZERO IS INVALID, for the reason every closed set in
// this tree is: an unknown value off the wire is a value rather than a panic,
// and a zero that validated would let a principal nobody classified through
// the one switch — [ActorFor] — whose whole job is to classify it.
type Kind string

const (
	// KindPerson is a human being: the founder at the dashboard, a
	// teammate with a login. Bound to a seat or not — both are ordinary,
	// and which one it is decides whether they act as themselves or as
	// the credential they hold (see [ActorFor]).
	KindPerson Kind = "person"

	// KindSeat is an agent seat acting inside its own turn. Its name is
	// the seat handle, because that is what the org chart, the mailbox
	// and every routing decision already key on.
	KindSeat Kind = "seat"

	// KindMachine is a token with nobody behind it — CI, a pipeline, an
	// automation. internal/org/role.go calls this out as an ORDINARY
	// state rather than a misconfiguration, which is why it is a kind of
	// its own and not a person with a field left blank.
	KindMachine Kind = "machine"

	// KindEngine is the engine itself: a duty's repair, a chart apply, a
	// trim publishing what it concluded. It is a principal because those
	// writes land in the same audit trail as everybody else's, and
	// attributing a machine's housekeeping to a person would make it
	// indistinguishable from somebody's decision.
	KindEngine Kind = "engine"
)

// Kinds are the four.
var Kinds = []Kind{KindPerson, KindSeat, KindMachine, KindEngine}

// Valid reports whether a kind off the wire is one this build knows.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// segment is one part of a compound name: lowercase alphanumerics, with
// internal hyphens. Deliberately a seat handle's own alphabet — only a seat
// handle may also end in a hyphen or double one ([ValidSeatHandle]) — so the
// only thing telling the three namespaces apart is the separator: one rule to
// read rather than three grammars to compare.
const segment = `[a-z0-9]+(?:-[a-z0-9]+)*`

// loginPattern is a person's login: two or more segments joined by DOTS.
//
// THE DOT IS REQUIRED, and that is the whole reason this is a pattern and not
// a length check. A login is recorded as an author name beside seat handles
// and machine handles with no prefix to tell them apart, so `jane` would be a
// login that an audit row cannot distinguish from a seat called `jane`. A dot
// is not in the seat-handle grammar, so `jane.doe` can never be one.
var loginPattern = regexp.MustCompile(`^` + segment + `(?:\.` + segment + `)+$`)

// handlePattern is a machine's handle: two or more segments joined by a COLON.
//
// A COLON for the same reason the dot is required above, and a DIFFERENT
// character from the login's so the two are disjoint from each other as well
// as from a seat handle — `ci:release` names the class and the machine, the
// segmented shape the coordination layer already addresses resources with. An
// empty segment is refused rather than tolerated: a name with one is a key
// nothing can decode, and the write lands while every listing misses it.
var handlePattern = regexp.MustCompile(`^` + segment + `(?::` + segment + `)+$`)

// MaxLogin bounds a person's login and a machine's handle, in bytes — which
// is characters too, since both grammars admit ASCII alone.
//
// SIXTY-FOUR, the one width a seat handle ([ValidSeatHandle]), a person's
// login and a machine's handle share: all three land in the same author column
// (the tracker's, the knowledge base's, the identity trail's) and the
// directory prints it on every row. A Tier A token id therefore stops at
// fifty-eight: it acts under `token:<id>`.
const MaxLogin = 64

// ValidLogin reports whether s is a well-formed person login.
func ValidLogin(s string) bool { return len(s) <= MaxLogin && loginPattern.MatchString(s) }

// seatHandlePattern is a seat handle: ONE run of lowercase alphanumerics and
// hyphens, starting with an alphanumeric.
//
// NO SEPARATOR OF EITHER LOGIN GRAMMAR, which is the whole of what keeps the
// three namespaces apart: a dot is a person's and a colon a machine's, so a
// name carrying either is never a seat and a seat's name never carries one.
// Looser than [segment] in one direction only — it admits a trailing or doubled
// hyphen, which is what a slug of a name like "R & D -" produces and what
// companies already run on — and that looseness cannot reach a separator.
var seatHandlePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidSeatHandle reports whether s is a well-formed seat handle: one segment,
// at most [MaxLogin] bytes.
//
// THE ONE DEFINITION of the shape. internal/org's ValidHandle is this, so a
// document and the party registry refuse exactly the same names — a second
// copy that disagreed by one character would accept a seat at one of them and
// refuse it at the next.
//
// BOUNDED AT [MaxLogin], a handle's own width: a handle sits in the same
// author column a login does, so the three share one width.
func ValidSeatHandle(s string) bool {
	return len(s) <= MaxLogin && seatHandlePattern.MatchString(s)
}

// The names the engine writes its own lines in a room under (internal/chatlet's
// system kind): a turn's conversation told its run's lost session, an auto-join
// room's newcomers and a unit room's members. Each says what speaks, so a
// reader of the room's record can tell the narrations apart. No seat takes one
// (the package doc's "THE ENGINE'S OWN NAMES").
const (
	EngineSessions = "sessions"
	EngineAutoJoin = "auto-join"
	EngineOrgChart = "org-chart"
)

// EngineNames is every name the engine writes its own lines under.
func EngineNames() []string { return []string{EngineAutoJoin, EngineOrgChart, EngineSessions} }

// IsEngineName reports whether s is one of [EngineNames], which no seat takes.
func IsEngineName(s string) bool { return slices.Contains(EngineNames(), s) }

// ValidMachineHandle reports whether s is a well-formed machine handle.
//
// THE `pat` AND `session` CLASSES ARE NOBODY'S: they are how a machine token
// and a browser session are named in the operator column ([MachineTokenPrefix],
// [SessionPrefix]), so a handle in either would be a principal
// indistinguishable from a credential.
func ValidMachineHandle(s string) bool {
	if len(s) > MaxLogin || !handlePattern.MatchString(s) {
		return false
	}
	for _, class := range credentialClasses {
		if strings.HasPrefix(s, class) {
			return false
		}
	}
	return true
}

// credentialClasses are the classes a CREDENTIAL's name carries in the
// operator column, which no login may take.
var credentialClasses = []string{MachineTokenPrefix, SessionPrefix, AssistPrefix}

// TokenLoginPrefix is the class segment a Tier A token's login carries.
//
// The machine grammar joins segments with a COLON, which a seat handle never
// contains and a person's dotted login never uses, so `token:ops` cannot
// collide with either namespace. [ActorFor] writes the WHOLE name into an audit
// row, never a prefix the store strips, or a filter on the name would match
// half the rows.
//
// HERE, WITH THE GRAMMAR, because the API guard composes the login a token acts
// under and internal/config refuses an id composing one outside the machine
// grammar ([ValidTokenID]).
const TokenLoginPrefix = "token:"

// TokenLogin is the login a Tier A token acts under.
func TokenLogin(id string) string { return TokenLoginPrefix + id }

// MachineTokenPrefix is the class a machine token's NAME carries where the
// audit trail records which credential a write was made through: every row
// its owner's work lands in says `pat:<credential id>` in the operator column,
// beside the owner as the author.
//
// A CLASS NO LOGIN MAY TAKE. It is coloned like a machine handle, so without a
// reservation a service account enrolled as `pat:<something>` would be a
// principal whose NAME reads as a credential in the operator column, and a
// reader resolving it would look for a token that never existed.
// [ValidMachineHandle] refuses the class for exactly that, so the name is a
// machine token's and nothing else's.
const MachineTokenPrefix = "pat:"

// MachineTokenName is how a machine token is recorded as the credential a
// write was made through ([Principal.Via]).
func MachineTokenName(id string) string { return MachineTokenPrefix + id }

// ValidMachineTokenName reports whether s names a machine token: the class and
// a credential id in its canonical form, which is the only form one is minted
// in.
func ValidMachineTokenName(s string) bool {
	return validCredential(s, MachineTokenPrefix)
}

// SessionPrefix is the class a browser SESSION's name carries where the audit
// trail records which credential a write was made through:
// `session:<lineage>` in the operator column, beside the person as the author.
//
// THE LINEAGE AND NOT THE LOGIN: the login is already the author, and two
// browsers signed in as one person would be one name in the trail. An
// investigation follows the lineage from a row to the sign-in behind it: the
// session's row, its `iam_session_started` event and its `iam_history` entry.
//
// A CLASS NO LOGIN MAY TAKE, for [MachineTokenPrefix]'s reason.
const SessionPrefix = "session:"

// SessionName is how a browser session is recorded as the credential a write
// was made through ([Principal.Via]).
func SessionName(lineage string) string { return SessionPrefix + lineage }

// ValidSessionName reports whether s names a browser session: the class and a
// lineage in its canonical form, which is the only form one is minted in.
func ValidSessionName(s string) bool { return validCredential(s, SessionPrefix) }

// ValidCredentialName reports whether s names a credential a principal may act
// THROUGH ([Principal.Via]): a machine token, a browser session or an
// assistant grant.
func ValidCredentialName(s string) bool {
	return ValidMachineTokenName(s) || ValidSessionName(s) || ValidAssistName(s)
}

// validCredential reports whether s is class followed by a canonical uuid.
func validCredential(s, class string) bool {
	id, ok := strings.CutPrefix(s, class)
	if !ok {
		return false
	}
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

// ValidTokenID reports whether a Tier A token id composes a login in the
// machine grammar.
//
// A TOKEN IS A MACHINE, so its login is held to the machine's grammar like
// every other. An id outside it — `Founder`, `ci_bot`, `ci.bot` — composes a
// name no directory row can hold (so the token can never be bound to a seat,
// and the refusal arrives only when somebody tries) and an author name
// outside every grammar the three namespaces are kept apart by.
func ValidTokenID(id string) bool { return ValidMachineHandle(TokenLogin(id)) }

// ValidLoginFor reports whether s is a well-formed login for a principal of
// kind k: a person's must be dotted and a machine's coloned, and no other kind
// holds a login at all.
//
// The grammar is PER KIND, never "either one": each belongs to ONE kind, or a
// PERSON able to choose `token:ops` would make the deployment's `ops` token
// credential act as their seat, since the directory's row under a login binds
// it to a seat.
//
// A SEAT and the ENGINE answer false for every string (a seat's name is its
// [ValidSeatHandle], the engine's a minted node id; neither enrols), and so
// does a kind this build cannot name, failing closed.
func ValidLoginFor(k Kind, s string) bool {
	switch k {
	case KindPerson:
		return ValidLogin(s)
	case KindMachine:
		return ValidMachineHandle(s)
	}
	return false
}

// NormalizeEmail is the form an email address is MATCHED on.
//
// LOWER-CASED AND PLUS-TAG STRIPPED, because people sign a vendor up with a
// sub-addressed form (`sarah.chen+jira@example.com`) naming the same mailbox.
//
// IN THE LEAF because TWO PLACES MATCH ON IT and must never fold differently:
// the party registry (internal/notify) resolves a payload to a SEAT, and the
// identity estate derives a person's keyed BLIND at the write. Two rules would
// send one address to a seat and a different person.
//
// IT DOES NOT VALIDATE: a non-address comes back folded and unchanged; the
// caller that must refuse one says so where the field is named.
func NormalizeEmail(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	local, domain, ok := strings.Cut(email, "@")
	if !ok {
		return email
	}
	if tagged, _, cut := strings.Cut(local, "+"); cut {
		local = tagged
	}
	return local + "@" + domain
}

// LoginFromAddress PROPOSES a person's login from their address, or answers ""
// when nothing in the address fits the person grammar.
//
// A PROPOSAL, NEVER SILENT: an invitation's form arrives pre-filled with it,
// and a create with a blank login names the login the engine took.
//
// THE LOCAL PART, FOLDED INTO THE GRAMMAR: normalised by [NormalizeEmail],
// every run outside a segment becomes one separator, and a hyphen survives only
// between two alphanumerics. A one-segment local part (`jane@example.com`)
// borrows the domain's first label (`jane.example`), since the dot keeps a
// login out of the seat-handle namespace. A proposal past [MaxLogin] is
// NOTHING rather than a cut name the person did not choose.
func LoginFromAddress(address string) string {
	local, domain, ok := strings.Cut(NormalizeEmail(address), "@")
	if !ok {
		return ""
	}
	segments := loginSegments(local)
	if len(segments) == 1 {
		label, _, _ := strings.Cut(domain, ".")
		segments = append(segments, loginSegments(label)...)
	}
	login := strings.Join(segments, ".")
	if !ValidLogin(login) {
		return ""
	}
	return login
}

// LoginFor proposes a person's login from their name, and from their address
// when the name can't make one. A name of two or more plain words ("Maya
// Chen") proposes them joined by dots ("maya.chen"), which is how a person
// writes their own; a one-word name, no name, or a name with letters outside
// a to z leaves the proposal to [LoginFromAddress], since folding "José" to
// "jos" would be a login nobody recognises.
func LoginFor(name, address string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, r := range lower {
		if unicode.IsLetter(r) && (r < 'a' || r > 'z') {
			return LoginFromAddress(address)
		}
	}
	if segments := loginSegments(lower); len(segments) >= 2 {
		if login := strings.Join(segments, "."); ValidLogin(login) {
			return login
		}
	}
	return LoginFromAddress(address)
}

// loginSegments splits text into the segments a login is made of: lowercase
// alphanumeric runs, joined by single hyphens where the text had one between
// two of them, and nothing else.
func loginSegments(text string) []string {
	var segments []string
	for _, piece := range strings.FieldsFunc(text, func(r rune) bool {
		return !segmentRune(r)
	}) {
		words := strings.FieldsFunc(piece, func(r rune) bool { return r == '-' })
		if len(words) > 0 {
			segments = append(segments, strings.Join(words, "-"))
		}
	}
	return segments
}

// segmentRune reports whether r may appear inside a login segment: a
// lowercase letter, a digit, or the hyphen that joins two runs of them.
func segmentRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-'
}
