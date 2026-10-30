package topics

// THE ARTIFACTS SUBJECTS: two state logs. An artifact's room crosses the fleet
// on the presence channel every kind of room shares (presence.go).
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
