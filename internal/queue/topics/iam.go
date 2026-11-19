package topics

// The identity estate's own log, whose grammar carries its uniqueness rule:
// every write that sets or frees a login, an address or a seat is a record on
// ONE subject, the directory, whose decide reads the whole directory. No
// uniqueness constraint exists in the estate: the replicated schema forbids a
// UNIQUE index outside a primary key (a violation inside an apply transaction
// stalls every node's log). A person's content and a session's life each
// arbitrate on a subject of their own.
//
// It lives here for the other domains' reason: the publisher, the wake feed's
// filter and the applier's kind switch each use the subject and none sees the
// others; a kind added in one and missed in another publishes unheard.

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
