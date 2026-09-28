package topics

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

// PagesLogSubject builds the subject for one object on layout 0's pages log:
// [LogSubject] under [PagesLogPrefix], and refused wherever that is.
func PagesLogSubject(kind, id string) string {
	return LogSubject(PagesLogPrefix, kind, id)
}

// PagesLogPath recovers the kind and the id from a subject on layout 0's pages
// log, reporting whether the subject was one: [LogPath] under
// [PagesLogPrefix], the exact inverse of [PagesLogSubject].
func PagesLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(PagesLogPrefix, subject)
}
