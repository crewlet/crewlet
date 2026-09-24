package topics

// The usage domain's log: what each node's seats and schedules did each company
// day, and why its grammar sits beside the other three domains'.
//
// The usage domain is the state-log framework's FOURTH, and its second
// COMPACTED one: one message per subject, so the stream is a keyed table of
// node-days rather than a history. The subject is not an arbitration unit here
// — exactly one writer, the node the day belongs to, ever publishes on it — but
// it IS the compaction key, which makes it load-bearing in a different way: a
// subject that named two different node-days would have the newer one's record
// silently delete the older one's, and a node-day spread across two subjects
// would keep a stale copy beside the current one for the stream's whole age.
//
// It lives here for the reason the others do: the publisher builds it and the
// applier and the stream's own wildcard read it, and none of the three can see
// the others.

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
