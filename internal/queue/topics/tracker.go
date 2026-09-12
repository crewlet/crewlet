package topics

import "strings"

// The native tracker's own log, and why its grammar lives here.
//
// The tracker's mutations are a state log: one ordered stream per domain,
// every record published to the subject of the object it mutates, and the
// broker arbitrating writes per subject. That makes the subject string
// load-bearing in a way an inbox subject is not — it is the ARBITRATION UNIT.
// Three separate things compare against it and none of them can see the
// others: the publisher builds it, the wake feed's consumer filters on it, and
// the applier's kind switch dispatches on the kind inside it. A fourth is the
// table of kinds itself, which decides what `Tables()` must classify.
//
// Written by hand in each of those places, a kind added in one and missed in
// another is not an error anywhere: the record publishes into a subject the
// filter does not cover, nobody is woken, and the company simply looks quiet.

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

// The FIFTEEN object kinds are NOT constants here, and that is deliberate.
//
// A kind is one bare word — "task", "project", "turn" — and the guard that
// makes this package worth having derives its markers from these constants:
// a dotless one is matched by shape, so exporting "task" from here would fail
// the build on every string literal in the engine that happens to be the word
// task. The kinds are therefore a typed enum in internal/tracker, which is the
// package that switches on them; what lives here is the GRAMMAR they are
// composed into, which is the half two processes have to agree about.

// TrackerLogSubject builds the subject for one object.
//
// An EMPTY id is legal and means a kind with exactly one object, which the
// barrier is. An empty KIND is not: it would publish to the prefix itself, a
// real subject inside the wildcard that the applier's switch has no case for,
// so it answers the empty string and callers must treat that as "not
// publishable" rather than as a subject.
func TrackerLogSubject(kind, id string) string {
	if kind == "" {
		return ""
	}
	if id == "" {
		return TrackerLogPrefix + "." + kind
	}
	return TrackerLogPrefix + "." + kind + "." + id
}

// TrackerLogPath recovers the kind and the id from a subject on the mutation
// log, reporting whether the subject was one.
//
// The exact inverse of [TrackerLogSubject]: true only for a subject that
// function could have produced. The ID keeps its dots — a sprint's id is
// "<PROJECT>.<n>" and an alias claim's is "<OLDKEY>.<n>" — so only the FIRST
// segment is the kind, and splitting on every dot would recover a kind of
// "sprint" and an id of "ENG" from a subject naming ENG's seventh sprint.
func TrackerLogPath(subject string) (kind, id string, ok bool) {
	rest, found := strings.CutPrefix(subject, TrackerLogPrefix+".")
	if !found || rest == "" {
		return "", "", false
	}
	kind, id, found = strings.Cut(rest, ".")
	if kind == "" || (found && id == "") {
		return "", "", false
	}
	return kind, id, true
}
