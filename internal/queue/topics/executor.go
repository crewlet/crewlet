package topics

// THE EXECUTOR LOG: the record of each seat's executor sessions, ordered on
// one stream and applied into every data node's replicated estate
// (internal/executordomain, ADR-0072). A row names the sealed object in the
// fleet's object store that holds one turn's captured harness session; the bytes
// are never on this stream.
//
// A LOG OF ITS OWN rather than records on another domain's, because it is
// written at the cadence executors are collected, retained on the company's
// own conversation horizon and purged by a duty of its own, and a record one
// build cannot decode here must never freeze the tracker's, the knowledge
// base's or chat's apply.
const (
	// ExecutorLogStream is the executor domain's ordered stream.
	ExecutorLogStream = "CREWLET_EXECUTOR_LOG"

	// ExecutorLogPrefix prefixes every subject on it. A record's own path,
	// its kind and its id, is appended to this with a dot.
	ExecutorLogPrefix = "crewlet.executor.log"

	// ExecutorLogWildcard is the subject space the stream is created with.
	ExecutorLogWildcard = ExecutorLogPrefix + ".>"
)

// ExecutorLogSubject is the subject one executor record is published on.
func ExecutorLogSubject(kind, id string) string {
	return LogSubject(ExecutorLogPrefix, kind, id)
}

// ExecutorLogPath is [ExecutorLogSubject]'s inverse.
func ExecutorLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(ExecutorLogPrefix, subject)
}
