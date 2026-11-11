package topics

// THE ATLAS LOG: what people tell the engine about what the company runs on
// (an entry they add, a fact they correct, two entries they join or keep apart,
// a value an administrator purged), ordered on one stream and applied into every
// data node's replicated estate (internal/atlasdomain).
//
// A LOG OF ITS OWN rather than records on another domain's: the atlas keeps a
// history on a horizon of its own (`atlas.retention_days`), and a record one
// build cannot decode here must never freeze another domain's apply.
const (
	// AtlasLogStream is the atlas domain's ordered stream.
	AtlasLogStream = "CREWLET_ATLAS_LOG"

	// AtlasLogPrefix prefixes every subject on it. A record's own path, its
	// kind and its id, is appended to this with a dot.
	AtlasLogPrefix = "crewlet.atlas.log"

	// AtlasLogWildcard is the subject space the stream is created with.
	AtlasLogWildcard = AtlasLogPrefix + ".>"
)

// AtlasLogSubject is the subject one atlas record is published on.
func AtlasLogSubject(kind, id string) string {
	return LogSubject(AtlasLogPrefix, kind, id)
}

// AtlasLogPath is [AtlasLogSubject]'s inverse.
func AtlasLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(AtlasLogPrefix, subject)
}
