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
// implements over the identity directory; the rule stays here because the
// tools, the questions and the routes all have to answer it the same way, and
// this is the one package all three already import.
//
// # Three namespaces that can never collide
//
// Every actor this engine durably records is written under a NAME, and the
// name alone has to be enough: internal/api/opsmcp used to prefix an
// operator's with "operator:" and the audit feed showed one person as two
// people three rows apart, because a work commit carried the bare id and a
// page commit carried the prefixed one. The prefix is gone, the author KIND is
// a column, and what keeps the names apart now is the SHAPE OF THE VALUE —
// tracker/rank.go's move, applied to names instead of ranks:
//
//   - a SEAT HANDLE is one segment, [a-z0-9][a-z0-9-]* (internal/org/role.go
//     owns that grammar and this package deliberately does not restate it);
//   - a PERSON's login is segments joined by DOTS — jane.doe;
//   - a MACHINE's handle is segments joined by a COLON — ci:release.
//
// Both of the grammars here therefore REQUIRE a separator, which is the one
// thing a seat handle can never carry, and they require different ones, which
// is what makes them disjoint from each other. No collision is possible, in
// either direction, by construction rather than by a uniqueness check some
// later writer has to remember to run.
//
// AND BOTH ARE BOUNDED AT [MaxLogin], a seat handle's own width: a login is a
// subject token the broker indexes for the life of the deployment and the
// name in an author column beside a seat handle. The bound is IN the grammar,
// so every surface that enrols, renames or composes a login is held to it by
// asking the one question it already asks.
//
// AND EACH GRAMMAR BELONGS TO ONE KIND — [ValidLoginFor] — because a name is
// read back as a claim about the kind that holds it. `token:<id>` is the login
// a Tier A token acts under, and the identity directory's row under it is what
// binds that token to a seat, so a person free to choose a coloned login could
// make the deployment's own credential act as their seat.
//
// # Nothing here decides anything
//
// A gate asks [Principal.Can]; a route or a query declares an [Access]; a
// store writes what [ActorFor] returns. What a request actually presented, and
// whether the answer could be reached at all, is [From]'s three-valued job.
//
// # Whose record a name is, and why it is one function
//
// A person's inbox, pins, priorities and personal views are kept under ONE
// name — [RecordOwner]'s, the name their writes are attributed to — and every
// surface that reads or writes somebody's record resolves the name it was
// given through [OwnerOf]: the caller's own names are their own record,
// somebody else's LOGIN is their holder's record (a bound person's seat), and
// anything else is the chart's. A login is never a seat — [NamesLogin] — so it
// is never matched against the chart's roster, where `jane.doe` resembles the
// seat `jane` closely enough to land on it.
package iam

import (
	"regexp"
	"slices"
	"strings"

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
// internal hyphens. Deliberately the SAME shape as a seat handle's, so the
// only thing telling the three namespaces apart is the separator — one rule to
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
// SIXTY-FOUR, which is the bound a seat handle already has (internal/chart's
// MaxKey), for the reasons that bound it — restated here because this package
// is a leaf and cannot import the chart:
//
//   - a login is a SUBJECT TOKEN: `iam.login.<login>` is the claim it
//     arbitrates on, so the broker keeps it in a per-subject index for the
//     life of the deployment, and a Tier A token's `token:<id>` is one too;
//   - it lands in the same author column a seat handle does — the tracker's,
//     the knowledge base's, the chart's and the identity trail's — so the
//     three names share one width wherever a screen renders who did
//     something;
//   - and the directory prints it on every row.
//
// Nothing bounded it before, so a login was whatever length somebody typed,
// and a proposal derived from a sixty-four-byte address local part plus a
// domain label could run past a hundred. A Tier A token id therefore stops at
// fifty-eight: it acts under `token:<id>`.
const MaxLogin = 64

// ValidLogin reports whether s is a well-formed person login.
func ValidLogin(s string) bool { return len(s) <= MaxLogin && loginPattern.MatchString(s) }

// ValidMachineHandle reports whether s is a well-formed machine handle.
//
// THE `pat` CLASS IS NOBODY'S: it is how a machine token is named in the
// operator column ([MachineTokenPrefix]), so a handle in it would be a
// principal indistinguishable from a credential.
func ValidMachineHandle(s string) bool {
	return len(s) <= MaxLogin && handlePattern.MatchString(s) &&
		!strings.HasPrefix(s, MachineTokenPrefix)
}

// TokenLoginPrefix is the class segment a Tier A token's login carries.
//
// The machine grammar joins segments with a COLON, which is the one separator
// a seat handle can never contain and a person's dotted login never uses — so
// `token:ops` cannot collide with either namespace by construction. It is what
// [ActorFor] writes into an audit row, and it is deliberately the WHOLE name
// rather than a prefix the store strips: the author kind is already a column,
// and a name half the rows carry a prefix on is a name a reader filtering on
// it matches half of.
//
// HERE, WITH THE GRAMMAR, because two layers have to agree on it: the API
// guard composes the login a token acts under, and internal/config refuses a
// token id that would compose one outside the machine grammar — see
// [ValidTokenID].
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
// principal whose NAME reads as a credential in every single-column trail —
// `created_by`, `set_by` — and a reader resolving it would look for a token
// that never existed. [ValidMachineHandle] refuses the class for exactly that,
// so the name is a machine token's and nothing else's.
const MachineTokenPrefix = "pat:"

// MachineTokenName is how a machine token is recorded as the credential a
// write was made through ([Principal.Via]).
func MachineTokenName(id string) string { return MachineTokenPrefix + id }

// ValidMachineTokenName reports whether s names a machine token: the class and
// a credential id in its canonical form, which is the only form one is minted
// in.
func ValidMachineTokenName(s string) bool {
	id, ok := strings.CutPrefix(s, MachineTokenPrefix)
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
// # The grammar is PER KIND, and "either one" is the bug this closes
//
// The two grammars being disjoint from each other is only half of what keeps
// the namespaces apart. The other half is that each belongs to ONE kind —
// because a name is read back as meaning something about the kind that holds
// it. `token:<id>` is the login a Tier A token acts under, and the identity
// directory's row under that login is what binds that token to a seat; a
// PERSON who could choose `token:ops` as their own login would therefore have
// made the deployment's `ops` credential act as their seat. Accepting either
// grammar for either kind is how that name became choosable by the one party
// it must never belong to.
//
// A SEAT and the ENGINE answer false for every string. A seat's name is its
// handle, which internal/org owns and which carries no separator by design; the
// engine's is the node's own id, minted rather than typed. Neither enrols, so
// neither has a login for this grammar to admit — and a kind this build cannot
// name answers false too, which is the direction that fails closed.
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
// LOWER-CASED AND PLUS-TAG STRIPPED. Inbound Jira and GitHub payloads identify
// people by address, and a company routinely subscribes its seats with a
// plus-addressed form (`notif+sarah-chen@example.com`) so vendor mail is
// filterable — so the address on a record and the address in a payload are
// routinely different strings naming one person.
//
// IT IS COMPUTED ONCE, AT THE WRITE, and stored or hashed. The alternative is
// a LOWER(...) predicate over every row on every inbound webhook, which cannot
// use an index and has to re-derive the plus rule in SQL, in a dialect where
// this Go answer beside it would then be a second opinion.
//
// # Why it lives in the vocabulary leaf
//
// TWO DOMAINS MATCH ON IT and they must never disagree: the org chart derives
// `chart_seats.email_index` from it so a vendor payload resolves to a seat,
// and the identity estate derives a person's keyed BLIND from it so a sign-in
// resolves to a person. Written twice, one address would reach a seat and a
// different person — the two halves of "who is this" answering differently
// about one string, which is the class of failure this package's namespace
// rules exist to make unrepeatable. A leaf is where the shared answer can sit
// without either domain importing the other.
//
// IT DOES NOT VALIDATE. An address that is not one is returned folded and
// unchanged, because the caller that has to refuse one says so where the field
// is named — a normaliser that refused would make every caller handle an error
// for the same reason, in the same words, separately.
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
// A PROPOSAL AND NEVER A DECISION. Every person enrols with a login — it is
// the name an unbound person's changes are recorded under — and somebody
// redeeming an invitation has typed nothing yet, so the form they are shown
// arrives pre-filled with this and they keep it or change it. Nothing derives
// a login SILENTLY from here: a name recorded beside everything a person does
// is one they saw before it was theirs.
//
// THE LOCAL PART, FOLDED INTO THE GRAMMAR: the address is normalised by
// [NormalizeEmail] (so a plus tag is not part of anybody's name), every run of
// characters outside a segment becomes one separator, and a hyphen survives
// only between two alphanumerics, which is the segment's own shape. A local
// part that yields one segment — `jane@example.com` — borrows the domain's
// first label as its second (`jane.example`), because the dot is what keeps a
// login out of the seat-handle namespace and a proposal that could never be
// accepted would be a form that refuses its own default. For the same reason
// an address whose proposal would run past [MaxLogin] proposes NOTHING rather
// than a cut name: a login shortened at an arbitrary byte is one the person
// did not choose and would not recognise, and an empty field asks them to.
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

// loginSegments splits text into the segments a login is made of: lowercase
// alphanumeric runs, joined by single hyphens where the text had one between
// two of them, and nothing else.
func loginSegments(text string) []string {
	var segments []string
	for _, piece := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-')
	}) {
		words := strings.FieldsFunc(piece, func(r rune) bool { return r == '-' })
		if len(words) > 0 {
			segments = append(segments, strings.Join(words, "-"))
		}
	}
	return segments
}
