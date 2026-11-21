package topics

// The company's own chat: the first log where a hot path and an arbitration
// path coexist. Channel state arbitrates on the channel's own subject, so two writers
// renaming one room contend and one wins. Messages ride a SEPARATE kind under
// that channel's id with no expectation: talking never contends, and the
// per-subject index is bounded by channels. The grammar lives here as the
// publisher, wake feed and applier cannot see each other.
//
// PRESENCE has no subject here: it is the presence hub's (ADR-0038), outside
// every log's wildcard, since a durable stream would retain every keystroke.
// READ STATE has none: a cursor is a coordination record (internal/coord), so
// a scatter would be an unsigned instruction to show a state no record holds.

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
