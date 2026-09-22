package topics

import "strings"

// The identity estate's own log, and why its grammar is the strictest of the
// five.
//
// The iam domain is the state-log framework's FIFTH, and its fourth strictly
// ordered one. What is particular to it is that ONE SUBJECT PER CLAIM is the
// whole design rather than a routing convenience: an email address, a login, a
// seat binding and a session lineage each arbitrate on THEMSELVES, so two
// operators enrolling one address contend at the broker and exactly one wins.
// There is no uniqueness check anywhere in the estate — the replicated schema
// forbids a UNIQUE index outside a primary key, because a constraint violation
// inside an apply transaction stalls every node's log at once — so the subject
// grammar IS the uniqueness check, and a shared subject would silently be a
// duplicate identity nobody refused.
//
// It lives here for the reason the other four domains' grammar does — the
// publisher builds the subject, the wake feed's consumer filters on it, and
// the applier's kind switch dispatches on the kind inside it, and none of the
// three can see the others. Written by hand in each place, a kind added in one
// and missed in another is not an error anywhere: the record publishes into a
// subject the filter does not cover, nobody is woken, and the company simply
// looks quiet.

const (
	// IamLogStream is the identity estate's mutation stream.
	IamLogStream = "CREWLET_IAM_LOG"

	// IamLogPrefix prefixes every subject on it. A record's own path —
	// its kind and its id — is appended to this with a dot.
	//
	// `crewlet.iam.log` rather than `crewlet.identity.log` because the
	// stream name, the register key, the manifest key, the operator's own
	// column and the Go package all say `iam`, and a subject space spelled
	// differently from the domain it carries is one more mapping every
	// reader has to hold.
	IamLogPrefix = "crewlet.iam.log"

	// IamLogWildcard is the subject space the stream is created with.
	IamLogWildcard = IamLogPrefix + ".>"
)

// The object kinds are NOT constants here, for the reason the other four
// domains' are not: a kind is one bare word — "person", "email", "session" —
// and this package's own marker guard matches a dotless constant by shape, so
// exporting "person" from here would fail the build on every string literal in
// the engine that happens to be that word. The kinds are a typed enum in
// internal/iamdomain, which is the package that switches on them.

// IamLogSubject builds the subject for one claim.
//
// An EMPTY id is legal and means a kind with exactly one object, which the
// bootstrap and the barrier both are. An empty KIND is not: it would publish
// to the prefix itself, a real subject inside the wildcard that the applier's
// switch has no case for, so it answers the empty string and callers must
// treat that as "not publishable" rather than as a subject.
func IamLogSubject(kind, id string) string {
	if kind == "" {
		return ""
	}
	if id == "" {
		return IamLogPrefix + "." + kind
	}
	return IamLogPrefix + "." + kind + "." + id
}

// IamLogPath recovers the kind and the id from a subject on the iam log,
// reporting whether the subject was one.
//
// The exact inverse of [IamLogSubject]. The ID keeps its dots, for the chart
// log's reason turned the other way up: nothing this domain addresses is prose,
// but a LOGIN is `jane.doe` by construction — internal/iam requires the dot,
// because it is what tells a person's name apart from a seat handle in an audit
// row — so a login claim's subject carries one in every case rather than
// occasionally. Splitting on every dot would recover `jane` from a subject
// naming `jane.doe`, which is a different claim that nobody holds.
func IamLogPath(subject string) (kind, id string, ok bool) {
	rest, found := strings.CutPrefix(subject, IamLogPrefix+".")
	if !found || rest == "" {
		return "", "", false
	}
	kind, id, found = strings.Cut(rest, ".")
	if kind == "" || (found && id == "") {
		return "", "", false
	}
	return kind, id, true
}
