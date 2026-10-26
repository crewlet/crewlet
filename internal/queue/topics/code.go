package topics

// The code host's own log, and why its grammar sits beside the tracker's.
//
// The code domain is a state log whose subject is an arbitration unit rather
// than a routing label: two pushes to one repository contend at the broker
// and exactly one record lands per round, where pushes to two repositories
// never contend at all.
//
// It lives here for the reason the tracker's and the knowledge base's do —
// the publisher builds the subject, the applier's kind switch dispatches on
// the kind inside it, and the snapshot and the trim name the stream — and none
// of the three can see the others. Written by hand in each place, a kind added
// in one and missed in another is not an error anywhere: the record publishes
// into a subject nothing reads.

const (
	// CodeLogStream is the code host's mutation stream.
	CodeLogStream = "CREWLET_CODE_LOG"

	// CodeLogPrefix prefixes every subject on it. A record's own path —
	// its kind and its id — is appended to this with a dot.
	CodeLogPrefix = "crewlet.code.log"

	// CodeLogWildcard is the subject space the stream is created with.
	CodeLogWildcard = CodeLogPrefix + ".>"

	// CodeStatus is where a question about the fleet's git nodes is
	// scattered: every node serving the code host answers it with its own
	// status — its cache against its budget, the repositories it holds and
	// the ones it served most (internal/code/host, Server.Status) — and the
	// asker merges what came back.
	//
	// NOT EVENTS, for SearchPrefix's reason: a status read is the ephemeral
	// request and reply of [queue.EventQueue.Ask], and a durable copy would be
	// a record of every time somebody looked. And OUTSIDE the code log's
	// subject space, which its stream is created over ([CodeLogWildcard]): a
	// stream captures whatever is published under its subjects, a request
	// included, and would answer an asker with its own acknowledgement. NOT
	// SIGNED (internal/signedask): a node answers it about itself and carries
	// nothing out for the asker, so a forged question gets an answer nobody
	// needed and moves nothing.
	CodeStatus = "crewlet.code.status"
)

// The object kinds are NOT constants here, for the reason the tracker's are
// not: a kind is one bare word, and this package's own marker guard matches a
// dotless constant by shape. They are a typed enum in internal/code, which is
// the package that switches on them.

// CodeLogSubject builds the subject for one object on the code log:
// [LogSubject] under [CodeLogPrefix], and refused wherever that is.
func CodeLogSubject(kind, id string) string {
	return LogSubject(CodeLogPrefix, kind, id)
}

// CodeLogPath recovers the kind and the id from a subject on the code log,
// reporting whether the subject was one: [LogPath] under [CodeLogPrefix], the
// exact inverse of [CodeLogSubject].
func CodeLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(CodeLogPrefix, subject)
}
