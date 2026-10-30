package topics

// The company's own chat.
//
// The chat domain is the state-log framework's NINTH, and its subject space
// is the first where a hot path and an arbitration path share a log. Channel
// state — a topic, a membership set, an archive, a prune — arbitrates on the
// channel's own subject, so two writers renaming one room contend at the
// broker and exactly one wins. New messages ride a SEPARATE kind whose id is
// that same channel's, sequenced with no expectation: two people talking in
// one room never contend, and the broker's per-subject index is bounded by the
// number of channels rather than by the number of things anybody ever said.
//
// It lives here for the reason the other domains' grammar does — the
// publisher builds the subject, the wake feed's consumer filters on it, and
// the applier's kind switch dispatches on the kind inside it, and none of the
// three can see the others.
//
// PRESENCE HAS NO SUBJECT HERE. Typing, who is looking and the working
// indicator are what is true of a room right now rather than events, so they
// are the presence hub's (internal/presence, ADR-0038): a chat room is one
// kind of room there, told on [PresenceSubject] under `crewlet.rooms.` —
// outside every log's wildcard, because a durable stream created over it
// would retain every keystroke for the length of its own age bound — in
// frames sealed under the fleet keyring.
//
// A person's READ STATE has no subject here at all, and the obvious one — the
// node that took a cursor write telling its peers to push it — is what it is
// not. A cursor is a coordination record (internal/coord) that every node
// already watches, so the push to that person's sockets on any node is derived
// from the durable record itself; a scatter beside it would be an unsigned
// instruction to show somebody a state no record holds.

const (
	// ChatLogStream is the chat domain's mutation stream.
	ChatLogStream = "CREWLET_CHAT_LOG"

	// ChatLogPrefix prefixes every subject on it. A record's own path —
	// its kind and its id — is appended to this with a dot.
	ChatLogPrefix = "crewlet.chat.log"

	// ChatLogWildcard is the subject space the stream is created with.
	ChatLogWildcard = ChatLogPrefix + ".>"
)

// The object kinds are NOT constants here, for the reason the other domains'
// are not: a kind is one bare word, and this package's own marker guard
// matches a dotless constant by shape. The kinds are a typed enum in
// internal/chatlet, which is the package that switches on them; what lives here
// is the GRAMMAR they are composed into, which is the half two processes have
// to agree about.

// ChatLogSubject builds the subject for one object on the chat log:
// [LogSubject] under [ChatLogPrefix], and refused wherever that is.
//
// The id of a message is its CHANNEL's, under the message kind — the
// sequenced kind arbitrates nothing, so a room has ONE message subject rather than one
// per message — which is the shape a reader is most likely to mistake for the
// message's own. It is not the room's own state subject either: that is the
// channel kind's, under the same id, and keeping the two apart is what keeps
// posting free of the arbitration a rename or a membership change takes.
func ChatLogSubject(kind, id string) string {
	return LogSubject(ChatLogPrefix, kind, id)
}

// ChatLogPath recovers the kind and the id from a subject on the chat log,
// reporting whether the subject was one: [LogPath] under [ChatLogPrefix], the
// exact inverse of [ChatLogSubject].
func ChatLogPath(subject string) (kind, id string, ok bool) {
	return LogPath(ChatLogPrefix, subject)
}
