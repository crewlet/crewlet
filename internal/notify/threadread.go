package notify

import "context"

// Reading a conversation back: the mirror of the write side in this package.
//
// [StatusPoster] and [Statuses] let the engine say something ON a chat
// surface. This is the other direction — asking a surface what has already
// been said in a thread — and it exists because the engine was telling agents
// to go and do that themselves. On a company whose chat tools come from a
// per-role MCP server that instruction costs three rounds (list the server's
// tools, activate one, then call it, with the schema arriving only on the
// next message), and an agent that skips it answers eleven words of trigger
// text with no idea what the thread is about. Every value needed to just hand
// it over is already on this node: the channel and the thread root are on the
// trigger's metadata, and each seat's own authenticated client is held by its
// transport.
//
// # Vendor-neutral by construction
//
// A [Message] carries no vendor shape at all — no timestamps in a backend's
// own format, no subtype, no props. What differs between backends is which
// endpoint answers and how a seat's own post is recognised, and both of those
// are the transport's business. What the renderer needs is who said what, in
// order, and which of them was this seat.

// Message is one thing somebody said in a thread.
//
// Every field is read by the renderer: a value type here exists to be
// rendered, and a field nothing reads is a field that drifts out of step with
// the vendor that fills it.
type Message struct {
	// SenderID is the sender's id ON THAT BACKEND — a Slack `U…`, a
	// Mattermost user id — which is what the party registry resolves a
	// colleague from. Never a display name.
	SenderID string

	// SenderName is whatever the backend volunteered, and is usually
	// empty: Mattermost's post carries a user id and nothing else, and
	// Slack sends a username only for a legacy bot message. It is the
	// fallback for a sender the registry does not know, so a stranger
	// renders as a person rather than as an opaque id where the backend
	// said enough to avoid it.
	SenderName string

	// Text is what they said, already reduced to the user-visible body —
	// so a file shared with no comment reads as a file rather than as a
	// blank line.
	Text string

	// Own marks this seat's own earlier reply.
	//
	// Resolved by the TRANSPORT, which is the only thing that knows the
	// seat's identity on that backend, and it is what stops an agent
	// reading its own replies as a colleague's and answering itself.
	Own bool
}

// Transcript is what a backend read back from a thread.
//
// A TYPE RATHER THAN A SLICE, because "this is the thread" and "this is as
// much of the thread as I could reach" are different answers, and a caller
// that cannot tell them apart states the first when the second is true. The
// prompt block this feeds tells a seat that the newest message in front of it
// is the one that woke the turn — on a read that stopped short, that sentence
// is guaranteed false.
type Transcript struct {
	// Messages are the thread's messages OLDEST FIRST, with the thread's
	// own root first.
	Messages []Message

	// Older is how many earlier messages the backend read and dropped to
	// hold a window of its own. Zero on a backend that answers the whole
	// thread in one response. Counted rather than lost, so a renderer can
	// say how many earlier messages are not in front of the seat instead
	// of implying that none are.
	Older int

	// StoppedShort says the backend hit a bound of its own before it
	// reached the end of the thread, so Messages stops short of the
	// NEWEST — the message that woke the turn included.
	//
	// THE ONE STATE A RENDERER MUST NOT SMOOTH OVER. Everything else this
	// seam drops is older context a seat can do without; this is the thing
	// it is answering.
	StoppedShort bool
}

// Thread is a chat thread that ALREADY EXISTS.
//
// The distinction from [Conversation] is the whole reason both types are
// here. A Conversation answers "where does a reply land", so a top-level
// message anchors on its own id — a thread that does not exist yet, which is
// exactly right for raising an indicator where the reply will appear. A
// Thread answers "what has already been said", and a message with no
// `thread_ts` has no such answer: reading a thread rooted at the triggering
// post returns that post and nothing else, which the turn was already handed.
type Thread struct {
	// Backend is the transport key, matched against a reader's own.
	Backend string

	// Channel is the room or private conversation the thread sits in.
	Channel string

	// Root is the post everything in the thread hangs off.
	Root string
}

// ThreadOf resolves the existing thread a trigger points at.
//
// It reports false for a trigger that is not a chat message, for a top-level
// chat message that has no thread yet, and for metadata missing either half
// of the address — all three of which mean the same thing to a caller: there
// is no earlier conversation to hand over.
//
// The discriminator is the `transport` key every chat transport stamps on
// what it parses, read rather than passed in, because the caller here is
// asking WHICH backend this trigger came from rather than whether it came
// from one particular one. That key survives inbox coalescing (the merged
// event mirrors the latest constituent's metadata, and a chat partition is
// thread-grained wherever a thread exists, so a coalesced burst can never
// straddle two threads) and the detached-sandbox round trip.
func ThreadOf(metadata map[string]string) (Thread, bool) {
	backend := metadata["transport"]
	channel := metadata["channel"]
	root := metadata["thread_ts"]
	if backend == "" || channel == "" || root == "" {
		return Thread{}, false
	}
	return Thread{Backend: backend, Channel: channel, Root: root}, true
}

// ThreadReader is one chat backend's read half. Implemented by each chat
// transport, which holds the per-seat credentials.
type ThreadReader interface {
	// ThreadBackend is the transport name, matched against
	// [Thread.Backend]. The same discriminator the write side matches on,
	// so a company running two chat surfaces never reads one backend's
	// thread into the other's turn.
	ThreadBackend() string

	// ReadThread returns the thread's messages OLDEST FIRST, with the
	// thread's own root first: the root is what the thread is about, and
	// a renderer that has to bound the block keeps it whatever else it
	// drops. A backend that could not reach the end of the thread says so
	// on the [Transcript] rather than answering as though it had.
	//
	// It never returns an error. A false second result means nothing
	// could be read — no such seat on this node, a refused credential, a
	// backend that answered an error, a deadline — and the caller renders
	// that as a different sentence from an empty thread, because the two
	// send an agent to opposite places. A bool rather than an error for
	// the reason [StatusPoster.SetStatus] returns one: no caller here
	// acts on which failure it was.
	ReadThread(ctx context.Context, handle, channel, root string) (Transcript, bool)
}

// ThreadReaders is every chat backend's reader, addressed as one.
//
// The same argument [Statuses] makes for the write side, and it has to be
// made again rather than shared: a company can run more than one chat surface
// (an org migrating between them runs both for a while), and a turn is
// triggered by exactly one of them. A caller holding a single reader would
// read the wrong backend's thread or, far more likely, none at all.
type ThreadReaders struct{ readers []ThreadReader }

// NewThreadReaders collects the readers a node is running.
func NewThreadReaders(readers ...ThreadReader) *ThreadReaders {
	kept := make([]ThreadReader, 0, len(readers))
	for _, r := range readers {
		if r != nil {
			kept = append(kept, r)
		}
	}
	return &ThreadReaders{readers: kept}
}

// ReadThread reads the thread a trigger points at, from whichever backend
// owns it.
//
// NEVER NIL-CHECKED BY THE CALLER: a nil set, an empty one, and a thread on a
// backend this node has no reader for all report false, which the caller
// already has to handle — a maintenance-mode node runs no chat transport at
// all, and a node whose chat instance was unreachable at boot runs none
// either, so "no reader" is an ordinary state rather than a wiring bug.
func (r *ThreadReaders) ReadThread(ctx context.Context, handle string, t Thread) (Transcript, bool) {
	if r == nil || handle == "" || t.Backend == "" || t.Channel == "" || t.Root == "" {
		return Transcript{}, false
	}
	for _, reader := range r.readers {
		if reader.ThreadBackend() != t.Backend {
			continue
		}
		return reader.ReadThread(ctx, handle, t.Channel, t.Root)
	}
	return Transcript{}, false
}
