package topics

// The usage domain's log: what each node's seats and schedules did each company
// day, and why its grammar sits beside the other domains'.
//
// The usage stream is COMPACTED: one message per subject, a keyed table of
// node-days rather than a history. The subject is no arbitration unit (only
// the node the day belongs to publishes on it) but it IS the compaction key: a
// subject naming two node-days lets the newer record silently delete the
// older, and a node-day spread across two subjects keeps a stale copy beside
// the current one for the stream's whole age.
//
// It lives here for the others' reason: the publisher, the applier and the
// stream's wildcard each use it and none sees the others.

const (
	// UsageLogStream is the usage domain's compacted stream.
	UsageLogStream = "CREWLET_USAGE_LOG"

	// UsageLogPrefix prefixes every subject on it. A record's own path — its
	// kind, then the node, the day and the object, each an escaped
	// coordination key segment — is appended to this with a dot.
	UsageLogPrefix = "crewlet.usage.log"

	// UsageLogWildcard is the subject space the stream is created with.
	UsageLogWildcard = UsageLogPrefix + ".>"
)
