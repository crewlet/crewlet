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
// reaches for a store handle to "just look the principal up here".
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
// # Nothing here decides anything
//
// A gate asks [Principal.Can]; a route or a query declares an [Access]; a
// store writes what [ActorFor] returns. What a request actually presented, and
// whether the answer could be reached at all, is [From]'s three-valued job.
package iam

import (
	"regexp"
	"slices"
	"strings"
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

// ValidLogin reports whether s is a well-formed person login.
func ValidLogin(s string) bool { return loginPattern.MatchString(s) }

// ValidMachineHandle reports whether s is a well-formed machine handle.
func ValidMachineHandle(s string) bool { return handlePattern.MatchString(s) }

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
