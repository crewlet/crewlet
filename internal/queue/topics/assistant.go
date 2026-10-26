package topics

import "github.com/google/uuid"

// THE DASHBOARD ASSISTANT'S LOG: a person's conversations, turns and
// proposals, ordered on one stream and applied into every data node's
// replicated estate (internal/assistantdomain, ADR-0053).
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

	// AssistantTurnPrefix prefixes the EPHEMERAL channel a running turn's
	// live frames cross the fleet on (internal/assistant/live): the queue's
	// Broadcast and Listen, never a stream, for the presence channel's
	// reason. A frame is worth nothing a minute after its turn ends, and
	// what a person must see after a reload is the transcript the log
	// above records. Outside AssistantLogWildcard, so the log's stream
	// never captures a frame.
	AssistantTurnPrefix = "crewlet.assistant.live"

	// AssistantFleetPrefix prefixes the two signed instruction channels a
	// node asks the node running a turn on (internal/assistant/live): the
	// ephemeral request and reply of [queue.EventQueue.Ask], for
	// SteerPrefix's reason. Outside AssistantLogWildcard too.
	AssistantFleetPrefix = "crewlet.assistant.fleet."
	// AssistantReplay is where a request for the frames a client missed is
	// scattered. Every node serves it and only the node the request names
	// answers.
	AssistantReplay = AssistantFleetPrefix + "replay"
	// AssistantStop is where a Stop pressed on another node is scattered.
	// Every node serves it and only the node the request names answers.
	AssistantStop = AssistantFleetPrefix + "stop"
)

// AssistantTurnSubject is the live channel of one assistant turn. A
// [uuid.UUID] rather than a string, for [ArtifactPresenceSubject]'s reason.
func AssistantTurnSubject(turn uuid.UUID) string {
	return AssistantTurnPrefix + "." + turn.String()
}

// AssistantLogSubject is the subject one assistant record is published on.
func AssistantLogSubject(kind, id string) string {
	return LogSubject(AssistantLogPrefix, kind, id)
}

// AssistantLogPath is [AssistantLogSubject]'s inverse.
func AssistantLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(AssistantLogPrefix, subject)
}
