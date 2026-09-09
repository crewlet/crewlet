package topics

import "strings"

// The knowledge base's own log, and why its grammar sits beside the tracker's.
//
// The pages domain is the state-log framework's THIRD, and it is the second
// whose subject is an arbitration unit rather than a routing label: two
// writers saving one page contend at the broker and exactly one wins, where
// two writers on different pages never contend at all.
//
// It lives here for the reason the tracker's does — the publisher builds the
// subject, the wake feed's consumer filters on it, and the applier's kind
// switch dispatches on the kind inside it, and none of the three can see the
// others. Written by hand in each place, a kind added in one and missed in
// another is not an error anywhere: the record publishes into a subject the
// filter does not cover, nobody is woken, and the company simply looks quiet.

const (
	// PagesLogStream is the knowledge base's mutation stream.
	PagesLogStream = "CREWLET_PAGES_LOG"

	// PagesLogPrefix prefixes every subject on it. A record's own path —
	// its kind and its id — is appended to this with a dot.
	PagesLogPrefix = "crewlet.pages.log"

	// PagesLogWildcard is the subject space the stream is created with.
	PagesLogWildcard = PagesLogPrefix + ".>"
)

// The object kinds are NOT constants here, for the reason the tracker's are
// not: a kind is one bare word, and this package's own marker guard matches a
// dotless constant by shape. The kinds are a typed enum in internal/pages,
// which is the package that switches on them; what lives here is the GRAMMAR
// they are composed into, which is the half two processes have to agree about.

// PagesLogSubject builds the subject for one object.
//
// An EMPTY id is legal and means a kind with exactly one object, which the
// barrier is. An empty KIND is not: it would publish to the prefix itself, a
// real subject inside the wildcard that the applier's switch has no case for,
// so it answers the empty string and callers must treat that as "not
// publishable" rather than as a subject.
func PagesLogSubject(kind, id string) string {
	if kind == "" {
		return ""
	}
	if id == "" {
		return PagesLogPrefix + "." + kind
	}
	return PagesLogPrefix + "." + kind + "." + id
}

// PagesLogPath recovers the kind and the id from a subject on the pages log,
// reporting whether the subject was one.
//
// The exact inverse of [PagesLogSubject]. The ID keeps its dots — a revision's
// is "<page>.<n>" and a comment's is "<page>.<comment>" — so only the FIRST
// segment is the kind, and splitting on every dot would recover a kind of
// "revision" and an id of one uuid segment from a subject naming a page's
// seventh body.
func PagesLogPath(subject string) (kind, id string, ok bool) {
	rest, found := strings.CutPrefix(subject, PagesLogPrefix+".")
	if !found || rest == "" {
		return "", "", false
	}
	kind, id, found = strings.Cut(rest, ".")
	if kind == "" || (found && id == "") {
		return "", "", false
	}
	return kind, id, true
}
