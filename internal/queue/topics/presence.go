package topics

import "github.com/google/uuid"

// THE PRESENCE CHANNEL: who is in a room, crossing between nodes on the queue's
// EPHEMERAL verbs — core publish and subscribe, never a stream, so a heartbeat
// is never stored. ONE SPACE FOR EVERY KIND OF ROOM, under no log's prefix, so
// no stream's wildcard can capture a frame, and a room's kind is a token of its
// subject so two kinds' rooms of one id are two subjects.
//
// `crewlet.rooms`, NOT `crewlet.presence`: the presence hub's metrics are named
// under `crewlet.presence.` (crewlet.presence.frames, crewlet.presence.rooms),
// and a subject space sharing a metric namespace's head is one the hand-built
// subject guard cannot tell from it.
const (
	// PresencePrefix prefixes every presence subject.
	PresencePrefix = "crewlet.rooms"

	// PresenceAsk is where a node that gains its first watcher of a room asks
	// its peers for the members they hold there, once, rather than waiting a
	// rebroadcast interval to learn who is already there. The room it asks
	// about is inside the sealed frame, so one answerer serves every room.
	PresenceAsk = PresencePrefix + ".ask"
)

// PresenceSubject is the presence channel of one room: its kind and the id of
// what it is the presence of.
//
// A [uuid.UUID] rather than a string, for the reason every builder here takes
// one: a room's id is the only token a subject may carry for it, and a label
// or a title passed in its place must be a compile error rather than a channel
// nobody listens on. The kind is the presence package's own closed set, every
// member a lower-case word, so it is a token as it is.
func PresenceSubject(kind string, id uuid.UUID) string {
	return PresencePrefix + "." + kind + "." + id.String()
}
