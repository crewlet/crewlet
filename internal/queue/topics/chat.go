package topics

import "strings"

// The company's own chat, and the one grammar on this broker that carries two
// subject spaces rather than one.
//
// The chat domain is the state-log framework's FOURTH, and its subject space
// is the first where a hot path and an arbitration path share a log. Channel
// state — a topic, a membership set, an archive, a prune — arbitrates on the
// channel's own subject, so two writers renaming one room contend at the
// broker and exactly one wins. New messages ride a SEPARATE kind whose id is
// that same channel's, additively: two people talking in one room never
// contend, and the broker's per-subject index is bounded by the number of
// channels rather than by the number of things anybody ever said.
//
// It lives here for the reason the tracker's and the wiki's do — the publisher
// builds the subject, the wake feed's consumer filters on it, and the applier's
// kind switch dispatches on the kind inside it, and none of the three can see
// the others.
//
// THE SECOND SPACE IS NOT ON THE LOG. Typing, presence and the working
// indicator are questions rather than events ("who is doing what in this room
// right now"), so they ride the queue's ephemeral scatter — no stream, no
// consumer, no retention — on a subject deliberately OUTSIDE the log's
// wildcard, because a durable stream created over it would retain every
// keystroke window for the length of its own age bound.

const (
	// ChatLogStream is the chat domain's mutation stream.
	ChatLogStream = "CREWLET_CHAT_LOG"

	// ChatLogPrefix prefixes every subject on it. A record's own path —
	// its kind and its id — is appended to this with a dot.
	ChatLogPrefix = "crewlet.chat.log"

	// ChatLogWildcard is the subject space the stream is created with.
	ChatLogWildcard = ChatLogPrefix + ".>"

	// ChatPresence is where a node asks its peers what is happening in a
	// set of rooms right now, and where every node answers from its own
	// memory.
	//
	// ONE SUBJECT FOR THE FLEET, not one per channel, and the reason is
	// the search fan-out's: a server answers a request/reply on a subject
	// it is serving EXACTLY, so a per-channel subject would need every
	// node to serve a wildcard it cannot, and the memory twin every test
	// runs against matches the string. The rooms a caller is asking about
	// ride INSIDE the request, where a list belongs.
	//
	// It sits under `crewlet.chat.` beside the log rather than inside it,
	// so the log's wildcard cannot capture it: a presence probe filed as a
	// mutation would be an undecodable record on a strict, identity-
	// claiming log, which is the one failure that stops an applier.
	ChatPresence = "crewlet.chat.presence"
)

// The object kinds are NOT constants here, for the reason the tracker's and
// the wiki's are not: a kind is one bare word, and this package's own marker
// guard matches a dotless constant by shape. The kinds are a typed enum in
// internal/chat, which is the package that switches on them; what lives here
// is the GRAMMAR they are composed into, which is the half two processes have
// to agree about.

// ChatLogSubject builds the subject for one object.
//
// An EMPTY id is legal and means a kind with exactly one object, which the
// barrier is. An empty KIND is not: it would publish to the prefix itself, a
// real subject inside the wildcard that the applier's switch has no case for,
// so it answers the empty string and callers must treat that as "not
// publishable" rather than as a subject.
func ChatLogSubject(kind, id string) string {
	if kind == "" {
		return ""
	}
	if id == "" {
		return ChatLogPrefix + "." + kind
	}
	return ChatLogPrefix + "." + kind + "." + id
}

// ChatLogPath recovers the kind and the id from a subject on the chat log,
// reporting whether the subject was one.
//
// The exact inverse of [ChatLogSubject]. Only the FIRST segment is the kind,
// so an id may legally carry dots — which chat's do not today, and which the
// wiki's do, and the two grammars agree so that the next composed id here is
// not a silent reshape of this function.
func ChatLogPath(subject string) (kind, id string, ok bool) {
	rest, found := strings.CutPrefix(subject, ChatLogPrefix+".")
	if !found || rest == "" {
		return "", "", false
	}
	kind, id, found = strings.Cut(rest, ".")
	if kind == "" || (found && id == "") {
		return "", "", false
	}
	return kind, id, true
}
