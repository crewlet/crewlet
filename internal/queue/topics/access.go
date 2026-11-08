package topics

// THE ACCESS LOG: what each seat was told about its own access, and every SSH
// certificate the engine issued and command it ran, ordered on one stream and
// applied into every data node's replicated estate (internal/accessdomain).
//
// A LOG OF ITS OWN rather than records on another domain's: it is an audit
// trail kept on a horizon of its own (`access_history.retention_days`), and a record
// one build cannot decode here must never freeze another domain's apply.
const (
	// AccessLogStream is the access domain's ordered stream.
	AccessLogStream = "CREWLET_ACCESS_LOG"

	// AccessLogPrefix prefixes every subject on it. A record's own path, its
	// kind and its id, is appended to this with a dot.
	AccessLogPrefix = "crewlet.access.log"

	// AccessLogWildcard is the subject space the stream is created with.
	AccessLogWildcard = AccessLogPrefix + ".>"
)

// AccessLogSubject is the subject one access record is published on.
func AccessLogSubject(kind, id string) string {
	return LogSubject(AccessLogPrefix, kind, id)
}

// AccessLogPath is [AccessLogSubject]'s inverse.
func AccessLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(AccessLogPrefix, subject)
}
