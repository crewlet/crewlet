package topics

// Gitlet's own log, and why its grammar sits beside the tracker's.
//
// The code domain is a state log whose subject is an arbitration unit, not a
// routing label: two pushes to one repository contend at the broker and one
// record lands per round; pushes to two repositories never contend.
//
// It lives here for the tracker's and the knowledge base's reason: the
// publisher builds the subject, the applier's kind switch dispatches on the
// kind inside it, and the snapshot and the trim name the stream, and none of
// the three sees the others. Written by hand in each, a kind added in one and
// missed in another is no error anywhere: the record publishes into a subject
// nothing reads.

const (
	// CodeLogStream is Gitlet's mutation stream.
	CodeLogStream = "CREWLET_CODE_LOG"

	// CodeLogPrefix prefixes every subject on it. A record's own path —
	// its kind and its id — is appended to this with a dot.
	CodeLogPrefix = "crewlet.code.log"

	// CodeLogWildcard is the subject space the stream is created with.
	CodeLogWildcard = CodeLogPrefix + ".>"

	// CodeStatus is where a question about the fleet's Gitlet nodes is
	// scattered: every Gitlet node answers with its own status (cache against
	// budget, repositories held and most served; internal/gitlet/host,
	// Server.Status) and the asker merges the answers.
	//
	// NOT EVENTS, for SearchPrefix's reason: it is the ephemeral request and
	// reply of [queue.EventQueue.Ask]; a durable copy would record every look.
	// OUTSIDE the code log's subject space ([CodeLogWildcard]): a stream
	// captures whatever is published under its subjects, a request included,
	// and would answer an asker with its own acknowledgement. NOT SIGNED
	// (internal/signedask): a node answers about itself and carries nothing out
	// for the asker, so a forged question moves nothing.
	CodeStatus = "crewlet.code.status"

	// CodeSearch is where a code search of several repositories is scattered
	// (internal/codesearch): every Gitlet node answers for the repositories
	// the request's assignment table names it for, as the request's asker.
	//
	// NOT EVENTS and OUTSIDE THE CODE LOG, for CodeStatus's reasons. SIGNED,
	// unlike CodeStatus (internal/signedask): a node CARRIES IT OUT as the
	// asker it names, judging that asker's grant to each repository.
	//
	// A LEAF UNDER crewlet.code.search., never that name itself: the search's
	// metrics are crewlet.code.search.queries, .duration and .coverage, and a
	// subject that is a prefix of them makes each a hand-built subject to this
	// package's guard.
	CodeSearch = "crewlet.code.search.ask"

	// CodeHolds is where the code-retention pass asks which of a page of keys
	// (a seat's scratch repositories by id) a RUNNING turn holds: every process
	// holding a seat lease answers with its lease identity and the keys its
	// running turns hold (internal/agent/turnctx's Holds, internal/engine's
	// codeholds.go), since a turn runs only under its seat's lease and its
	// holds live in that process's memory until it has a run row. An expiry
	// that does not hear from every seat holder expires nothing that page.
	//
	// EPHEMERAL, OUTSIDE THE LOG AND UNSIGNED, for CodeStatus's reasons: an
	// in-memory answer, asked again a pass later, about the answerer itself.
	CodeHolds = "crewlet.code.holds"
)

// The object kinds are NOT constants here, for the reason the tracker's are
// not: a kind is one bare word, and this package's own marker guard matches a
// dotless constant by shape. They are a typed enum in internal/gitlet, which is
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
