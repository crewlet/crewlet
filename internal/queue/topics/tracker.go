package topics

// Worklet's own log, and why its grammar lives here.
//
// The tracker's mutations are a state log: every record is published to the
// subject of the object it mutates, and the broker arbitrates writes per
// subject, so the subject is the ARBITRATION UNIT. Three things use it and
// none sees the others: the publisher builds it, the wake feed's consumer
// filters on it, and the applier's kind switch dispatches on the kind inside
// it. A fourth is the table of kinds, which decides what `Tables()` must
// classify. A kind added in one place and missed in another is no error: the
// record publishes into a subject the filter does not cover and nobody wakes.

const (
	// TrackerLogStream is the mutation domain's stream.
	TrackerLogStream = "CREWLET_TRACKER_LOG"

	// TrackerLogPrefix prefixes every subject on it. A record's own path —
	// its kind and its id — is appended to this with a dot.
	TrackerLogPrefix = "crewlet.tracker.log"

	// TrackerLogWildcard is the subject space the stream is created with.
	TrackerLogWildcard = TrackerLogPrefix + ".>"

	// TrackerVectorsStream is the compacted vector domain's stream, and
	// TrackerVectorsPrefix its subject space. A SECOND DOMAIN on the same
	// framework rather than a second design: one message per subject, an
	// age bound, and the same envelope.
	TrackerVectorsStream = "CREWLET_TRACKER_VECTORS"
	// TrackerVectorsPrefix prefixes every subject on that stream.
	TrackerVectorsPrefix = "crewlet.tracker.vectors"
	// TrackerVectorsWildcard is the subject space it is created with.
	TrackerVectorsWildcard = TrackerVectorsPrefix + ".>"
)

// The object kinds are NOT constants here, and that is deliberate.
//
// A kind is one bare word — "task", "project", "turn" — and the guard that
// makes this package worth having derives its markers from these constants:
// a dotless one is matched by shape, so exporting "task" from here would fail
// the build on every string literal in the engine that happens to be the word
// task. The kinds are therefore a typed enum in internal/worklet, which is the
// package that switches on them; what lives here is the GRAMMAR they are
// composed into, which is the half two processes have to agree about.

// TrackerLogSubject builds the subject for one object on the mutation log:
// [LogSubject] under [TrackerLogPrefix], and refused wherever that is.
func TrackerLogSubject(kind, id string) string {
	return LogSubject(TrackerLogPrefix, kind, id)
}

// TrackerLogPath recovers the kind and the id from a subject on the mutation
// log, reporting whether the subject was one: [LogPath] under
// [TrackerLogPrefix], the exact inverse of [TrackerLogSubject].
func TrackerLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(TrackerLogPrefix, subject)
}
