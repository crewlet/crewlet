package topics

// The identity estate's own log, and why its grammar is the strictest of the
// state logs'.
//
// The iam domain is the state-log framework's FOURTH, and its third strictly
// ordered one. What is particular to it is that the subject grammar carries
// its uniqueness rule: every write that sets or frees a login, an address or a
// seat is a record on ONE subject, the directory, whose decide reads the whole
// directory and contends with every other — there is no uniqueness constraint
// anywhere in the estate, because the replicated schema forbids a UNIQUE index
// outside a primary key (a constraint violation inside an apply transaction
// stalls every node's log at once). A person's own content and a session's
// life each arbitrate on a subject of their own.
//
// It lives here for the reason the other domains' grammar does — the
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

// The object kinds are NOT constants here, for the reason the other domains'
// are not: a kind is one bare word — "person", "directory", "session" —
// and this package's own marker guard matches a dotless constant by shape, so
// exporting "person" from here would fail the build on every string literal in
// the engine that happens to be that word. The kinds are a typed enum in
// internal/iamdomain, which is the package that switches on them.

// IamLogSubject builds the subject for one object on the identity log:
// [LogSubject] under [IamLogPrefix], and refused wherever that is.
//
// An EMPTY id is legal and means a kind with exactly one object, which the
// directory, the invalidation and the barrier all are.
func IamLogSubject(kind, id string) string {
	return LogSubject(IamLogPrefix, kind, id)
}

// IamLogPath recovers the kind and the id from a subject on the identity log,
// reporting whether the subject was one: [LogPath] under [IamLogPrefix], the
// exact inverse of [IamLogSubject]. The id keeps its dots, which matters more
// here than on any other log: a person's login REQUIRES one, so `jane.doe` is
// what every login's subject looks like rather than an occasional awkward id.
func IamLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(IamLogPrefix, subject)
}
