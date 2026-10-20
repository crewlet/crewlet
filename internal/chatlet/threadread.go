package chatlet

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE THREAD A CHATLET WAKE CAME FROM, read back for the turn it woke.
//
// The turn-start prefetch renders the chat thread a turn was woken in
// (`## The thread so far`) through [notify.ThreadReaders], one reader per
// chat surface, matched on the trigger's own `transport`. Chatlet stamps
// `transport=chat`, the room and the thread root on every wake, and had no
// reader — so every Chatlet thread reply was handed the "could not be read
// from this node" hint and sent to read the thread with its tools, rounds
// spent fetching what the node could have handed over.
//
// This is that reader, and it is the ONLY thread context a Chatlet wake gets:
// the record carries none of its own. A second copy of one thread in one
// frozen system prompt would double its cost on every round of the turn and
// let the two disagree. Nor is it a stale copy a feed consumer happened to
// hold: it is read at the seat's own level, which waits for everything
// committed before it — the message that woke the turn included — on
// whichever node answers.

// ThreadSource is the one read a [ThreadReader] makes: a thread from its root,
// as a viewer. Declared by its consumer; the estate router's chat facade
// satisfies it on every node, a data node and one that holds no data alike.
type ThreadSource interface {
	Thread(ctx context.Context, viewer string, q ThreadQuery,
		fresh statelog.Freshness) (Thread, error)
}

// ThreadReader is Chatlet's [notify.ThreadReader].
type ThreadReader struct {
	threads ThreadSource
	parties func() notify.Parties
}

var _ notify.ThreadReader = ThreadReader{}

// NewThreadReader builds the reader over a thread source and the company's
// live party registry, read per call because a reader outlives every epoch.
func NewThreadReader(threads ThreadSource, parties func() notify.Parties) ThreadReader {
	return ThreadReader{threads: threads, parties: parties}
}

// threadWindow is how many of a thread's newest replies one read hands back:
// [MaxLimit], the most one read answers, beside the root, which is read on its
// own — and the read's own byte bound still applies, so a thread of long
// messages answers fewer.
//
// THE RENDERER'S BOUND IS NOT THIS READER'S TO APPLY. The turn-start block is
// bounded by BYTES alone: the root and the newest whole messages fill most of
// it and the messages between them are CONDENSED by the seat's auxiliary model
// rather than dropped (prefetch's renderThread). A window sized to what the
// block renders verbatim — it was the thirty messages a count bound once
// rendered — therefore cut the very middle the condensation is for: a Chatlet
// thread past thirty lost it as "N earlier messages not shown", where a
// Mattermost thread of the same length, read whole, was condensed. So the read
// reaches as far back as one read can carry and leaves the bound to the block,
// whose condensation refuses an input past its own ceiling and counts it.
//
// What the window leaves out is counted ([notify.Transcript.Older]) — and on
// this surface it is EXACT, because the read counts the rest in the same
// snapshot rather than walking from the root and stopping where a budget ran
// out, so a Chatlet transcript never stops short.
const threadWindow = MaxLimit

// threadRead is the level the read is made at: a seat's own
// ([statelog.SurfaceSeat]), because the block it feeds decides how the seat
// answers. ONE read per woken turn — the turn-start thread read is one of the
// four linearizable chat reads a turn is declared to make
// ([LinearizableReadsPerSeatDay]).
var threadRead = statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)}

// ThreadBackend implements [notify.ThreadReader]: the transport the chat parser
// stamps on every wake.
func (ThreadReader) ThreadBackend() string { return Source }

// ReadThread implements [notify.ThreadReader], reading AS THE SEAT: a private
// room or a direct conversation answers only a member, which the woken seat is
// — and a room it is not in answers nothing, which is the honest answer.
//
// The root comes first whatever it holds: a deleted root is a tombstone whose
// body is blank, and it keeps its slot so the renderer can say the opening
// could not be shown rather than promote the oldest reply into its place.
func (r ThreadReader) ReadThread(ctx context.Context, handle, channel, root string) (
	notify.Transcript, bool) {

	if r.threads == nil || handle == "" || channel == "" || root == "" {
		return notify.Transcript{}, false
	}
	thread, err := r.threads.Thread(ctx, handle, ThreadQuery{
		ChannelID: channel, RootID: root, Newest: threadWindow,
	}, threadRead)
	if err != nil {
		// DEBUG, like the hosted surface's: a refused read costs the block
		// and nothing else, the turn runs on the unreadable hint, and a
		// room this seat has been taken out of would otherwise log a
		// warning on every turn that thread wakes.
		log.DebugContext(ctx, "chat_thread_unreadable", "handle", handle,
			"channel", channel, "thread", root, "error", err.Error())
		return notify.Transcript{}, false
	}
	var parties notify.Parties
	if r.parties != nil {
		parties = r.parties()
	}
	out := notify.Transcript{
		Messages: make([]notify.Message, 0, 1+len(thread.Replies)),
		// WHAT THE WINDOW LEFT OUT, and what this node could not decode —
		// a reply a newer build wrote. Neither is in front of the seat,
		// and the renderer says so rather than implying the thread is
		// whole.
		Older: thread.Earlier + thread.Unreadable,
	}
	out.Messages = append(out.Messages, r.message(thread.Root.Message, handle, parties))
	for _, reply := range thread.Replies {
		out.Messages = append(out.Messages, r.message(reply.Message, handle, parties))
	}
	return out, true
}

// message is one line of the transcript.
//
// THE LABEL TRAVELS AS THE SENDER'S NAME, for the author the renderer's own
// lookup does not resolve. That lookup answers a Chatlet author by the handle
// the sender id is, and names a colleague as the roster does; what it leaves
// is an author the chart no longer holds, or the engine's own narration,
// which [AuthorLabel] describes by kind — the rendering the woken message's
// own sender is given, so one person reads the same in both.
func (ThreadReader) message(m Message, viewer string, parties notify.Parties) notify.Message {
	return notify.Message{
		// THE MESSAGE'S OWN ID, which is what a Chatlet wake stamps as the
		// trigger's `ts` ([notify.MessageIDField]): it is how the block says
		// which waiting messages a turn was shown, and without it nothing
		// read here ever matched the message that woke the turn.
		ID:         m.ID,
		SenderID:   m.Author,
		SenderName: AuthorLabel(m.Author, m.AuthorKind, parties),
		Text:       threadText(m),
		Own:        m.Author == viewer,
	}
}

// threadText is what a message said, as a transcript line.
//
// THE LINKS COME WITH IT, because a message may carry nothing else and a line
// rendered from the body alone would then read as a blank one, which the
// renderer drops. A tombstone has no body and no links, so it is empty — and
// dropped, or for a root, stood in for.
func threadText(m Message) string {
	text := strings.TrimSpace(m.Body)
	if len(m.Links) > 0 {
		text = strings.TrimSpace(text + " " + strings.Join(m.Links, " "))
	}
	return text
}
