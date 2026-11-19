package chatlet

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE THREAD A CHATLET WAKE CAME FROM, read back for the turn it woke.
//
// The turn-start prefetch renders the thread a turn was woken in
// (`## The thread so far`) through [notify.ThreadReaders], one reader per chat
// surface, matched on the trigger's `transport`. Chatlet stamps `transport=chat`,
// the room and the thread root on every wake.
//
// This is the ONLY thread context a Chatlet wake gets: the record carries none
// of its own, and a second copy in one frozen system prompt would double its
// cost every round and let the two disagree. It is read at the seat's own
// level, which waits for everything committed before it, the waking message
// included, on whichever node answers.

// ThreadSource is the one read a [ThreadReader] makes: a thread from its root,
// as a viewer, with whom each of its messages was written for ([TurnThread]).
// Declared by its consumer; the engine's own turn-start facade over the estate
// router satisfies it on every node, a data node and one that holds no data
// alike, and nothing a reader of a room holds does.
//
// ONE READ, which falls back to the plain thread where no data node serves
// the turn's own read, during a rolling upgrade: that answer says it carried
// nothing ([TurnThread.Carried]), and the turn then marks no waiting message
// as worked through.
type ThreadSource interface {
	TurnThread(ctx context.Context, viewer string, q ThreadQuery,
		fresh statelog.Freshness) (TurnThread, error)
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
// [MaxLimit], the most one read answers, beside the root read on its own; the
// read's byte bound still applies, so long messages answer fewer.
//
// THE RENDERER'S BOUND IS NOT THIS READER'S TO APPLY. The turn-start block is
// bounded by BYTES alone and the messages between the root and the newest are
// CONDENSED by the seat's auxiliary model, not dropped (prefetch's
// renderThread). A smaller window would cut the middle the condensation is
// for, so the read reaches as far back as it can and the block's condensation
// refuses an input past its ceiling and counts it.
// What the window leaves out is counted EXACTLY ([notify.Transcript.Older]).
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
	turn, err := r.threads.TurnThread(ctx, handle, ThreadQuery{
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
	thread := turn.Thread
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
		// THE ENGINE'S OWN SURFACE: every sender authenticated as a seat,
		// a person or the engine, so a line whose sender the roster no
		// longer names reads as a colleague's.
		Hosted: true,
		// AND WHETHER IT SAYS WHOM EACH MESSAGE WAS WRITTEN FOR: a read
		// that could not, the plain thread a fallback asked for, marks no
		// waiting message as worked through.
		WorkingForUnread: !turn.Carried,
	}
	out.Messages = append(out.Messages, r.message(thread.Root, handle, parties, turn.WorkingFor))
	for _, reply := range thread.Replies {
		out.Messages = append(out.Messages, r.message(reply, handle, parties, turn.WorkingFor))
	}
	return out, true
}

// message is one line of the transcript.
// THE LABEL TRAVELS AS THE SENDER'S NAME for an author the renderer's lookup
// does not resolve (one the chart no longer holds, or the engine's narration):
// [AuthorLabel] describes it by kind, as for the woken message's sender.
//
// AN EDITED MESSAGE NAMES THE EDIT IT WAS READ AT ([notify.Message.Version]):
// [MessageView.Position], the same [statelog.Position.String] token an edit's
// wake carries as its trigger, so a turn's mark drops that wake and no later
// one. An unedited message names none: its post's wake is dropped by its id.
//
// WorkingFor ([notify.Message.WorkingFor]) is a copy of what the turn-start
// read's carriers hand over, never the answer's own.
func (ThreadReader) message(view MessageView, viewer string, parties notify.Parties,
	carriers map[string]*types.WorkingFor) notify.Message {

	m := view.Message
	out := notify.Message{
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
	if m.EditedAt != nil {
		out.Version = view.Position.String()
	}
	if carrier := carriers[m.ID]; carrier != nil {
		out.WorkingFor = carrier.Clone()
	}
	return out
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
