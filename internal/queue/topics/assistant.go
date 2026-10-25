package topics

// THE DASHBOARD ASSISTANT'S LOG: a person's conversations, turns and
// proposals, ordered on one stream and applied into every data node's
// replicated estate (internal/assistantdomain, ADR-0049).
//
// A LOG OF ITS OWN rather than records on another domain's, because a
// conversation is sized by how much people talk to the assistant, retained
// thirty days and purged on a schedule of its own, and a record one build
// cannot decode here must never freeze the tracker's or the knowledge base's
// apply. The turn's live frames are not on it at all: they are told, never
// stored (internal/assistant/live).
const (
	// AssistantLogStream is the assistant domain's ordered stream.
	AssistantLogStream = "CREWLET_ASSISTANT_LOG"

	// AssistantLogPrefix prefixes every subject on it. A record's own path,
	// its kind and its id, is appended to this with a dot.
	AssistantLogPrefix = "crewlet.assistant.log"

	// AssistantLogWildcard is the subject space the stream is created with.
	AssistantLogWildcard = AssistantLogPrefix + ".>"
)

// AssistantLogSubject is the subject one assistant record is published on.
func AssistantLogSubject(kind, id string) string {
	return LogSubject(AssistantLogPrefix, kind, id)
}

// AssistantLogPath is [AssistantLogSubject]'s inverse.
func AssistantLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(AssistantLogPrefix, subject)
}
