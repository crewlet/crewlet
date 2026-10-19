package topics

import "github.com/google/uuid"

// THE ARTIFACTS SUBJECTS: two state logs and the presence channel.
//
// TWO LOGS, NOT ONE, because a publish is a human-scale event and a datastore
// write is a click: on one log the two would share one pinned writer, one
// ceiling, one trim and one gate reserve sized by the larger record, and a
// record one build cannot decode would freeze both. Each log is sized by its
// own records instead.
//
// THE DATASTORE'S PREFIX IS `crewlet.artifactdata`, NOT `crewlet.artifacts.data`,
// so no log's prefix is a dotted extension of another's: a parser matching
// on a looser head would read one domain's record as the other's, which is
// the hazard the tracker's mutation log and its vectors already share a head
// for and are tested against.
const (
	// ArtifactsLogStream is the artifacts domain's ordered stream: every
	// create, publish, share, trash and purge of an artifact.
	ArtifactsLogStream = "CREWLET_ARTIFACTS_LOG"

	// ArtifactsLogPrefix prefixes every subject on it. A record's own path,
	// its kind and its id, is appended to this with a dot.
	ArtifactsLogPrefix = "crewlet.artifacts.log"

	// ArtifactsLogWildcard is the subject space the stream is created with.
	ArtifactsLogWildcard = ArtifactsLogPrefix + ".>"

	// ArtifactDataLogStream is the datastore's ordered stream: one subject
	// per artifact store, every write sequenced on it.
	ArtifactDataLogStream = "CREWLET_ARTIFACT_DATA_LOG"

	// ArtifactDataLogPrefix prefixes every subject on it.
	ArtifactDataLogPrefix = "crewlet.artifactdata.log"

	// ArtifactDataLogWildcard is the subject space the stream is created with.
	ArtifactDataLogWildcard = ArtifactDataLogPrefix + ".>"

	// ArtifactPresencePrefix prefixes the EPHEMERAL presence channel: core
	// publish and subscribe, never a stream, so a heartbeat is never stored.
	// It is not under either log's prefix, so neither stream can capture it.
	ArtifactPresencePrefix = "crewlet.artifacts.presence"

	// ArtifactPresenceAsk is where a node that gains its first watcher of
	// an artifact asks its peers for the roster they hold, once, rather
	// than waiting a rebroadcast interval to learn who is already there.
	ArtifactPresenceAsk = ArtifactPresencePrefix + ".ask"
)

// ArtifactsLogSubject is the subject one artifacts record is published on.
func ArtifactsLogSubject(kind, id string) string {
	return LogSubject(ArtifactsLogPrefix, kind, id)
}

// ArtifactsLogPath is [ArtifactsLogSubject]'s inverse.
func ArtifactsLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(ArtifactsLogPrefix, subject)
}

// ArtifactDataLogSubject is the subject one datastore record is published on.
func ArtifactDataLogSubject(kind, id string) string {
	return LogSubject(ArtifactDataLogPrefix, kind, id)
}

// ArtifactDataLogPath is [ArtifactDataLogSubject]'s inverse.
func ArtifactDataLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(ArtifactDataLogPrefix, subject)
}

// ArtifactPresenceSubject is the presence channel of one artifact.
//
// A [uuid.UUID] rather than a string, for the reason every builder here takes
// one: an artifact's id is the only token a subject may carry for it, and a
// label or a title passed in its place must be a compile error rather than a
// channel nobody listens on.
func ArtifactPresenceSubject(artifact uuid.UUID) string {
	return ArtifactPresencePrefix + "." + artifact.String()
}
