package tracker

import (
	"cmp"
	"slices"
)

// HOW A PERSON LEARNS THEY HAVE WORK.
//
// The applier has always written the inbox — one row per (commit, candidate)
// in `tracker_notifications` — and a WAKE reaches an agent through the change
// feed. A person has no turn to wake: an agent's mention reaches them on Slack
// or Jira, if they have those, and a Crewlet-only person has neither. The
// dashboard is that person's delivery surface, and it polled.
//
// So the applier also says, after a committed batch, whose inbox moved. It is
// the second consequence of a tracker record that is not a row, and it is
// derived HERE rather than by the change feed for the reason the chart view's
// trigger is: the feed relays a record to ONE node, while every node applies
// every record and every node has its own sockets open. Each node tells its
// own sockets, from its own apply, and nothing crosses between nodes.
//
// # A HINT, NEVER CONTENT
//
// A movement names a handle, how many notices arrived for it, and the subject
// and reason of the newest — identifiers, never an excerpt, a title or who
// wrote it. It rides a socket frame routed to whoever watches that seat, and
// what the frame is for is telling a screen to re-read the inbox it already
// polls, through the same question and the same authority as the poll. A
// payload carrying content would be a second read path around that authority.
//
// The count is a hint too, and says so: a transaction body the store re-runs,
// or a batch that failed and is applied again, can announce one notice twice.
// The rows are the truth and the screen reads them; nothing adds these counts
// up.

// InboxMovement is one seat's inbox having moved in one committed batch on
// this node.
//
// EXACTLY FOUR FIELDS, and a test holds the set: every one of them is an
// identifier, and a field that carried content would be a read path the inbox
// question's own authority never decided.
type InboxMovement struct {
	// Handle is whose inbox moved.
	Handle string `json:"handle"`

	// UnreadDelta is how many notices the batch added for them. Never
	// negative: a notice arrives unread, and a person marking their own
	// read is their own gesture on their own screen.
	UnreadDelta int `json:"unread_delta"`

	// Subject is the id of the newest notice's subject, which is what a
	// screen can highlight without reading anything it did not ask for.
	Subject string `json:"subject"`

	// Reason is the one reason the newest notice reached them under.
	Reason Reason `json:"reason"`
}

// inboxNote is one notice a batch wrote, until the commit is known.
type inboxNote struct {
	record, handle, subject string
	reason                  Reason
	position                int64
}

// noteInbox records one notice this transaction wrote.
//
// KEYED ON (record, recipient), which is the notification row's own key: the
// store re-runs the body of an attempt that failed transiently, and a note
// kept per call would announce the re-run's notice as a second one.
func (a *Applier) noteInbox(n inboxNote) {
	if a.inboxed == nil {
		a.inboxed = map[[2]string]inboxNote{}
	}
	a.inboxed[[2]string{n.record, n.handle}] = n
}

// drainInbox is the batch's notices as one movement per handle, sorted by
// handle so a batch announces identically on every run, and resets the set.
func (a *Applier) drainInbox() []InboxMovement {
	if len(a.inboxed) == 0 {
		return nil
	}
	byHandle := map[string]*InboxMovement{}
	newest := map[string]inboxNote{}
	for _, n := range a.inboxed {
		m, ok := byHandle[n.handle]
		if !ok {
			m = &InboxMovement{Handle: n.handle}
			byHandle[n.handle] = m
		}
		m.UnreadDelta++
		// THE NEWEST BY POSITION, a tie broken on the record id: a map's
		// iteration order is not an answer, and the same batch must
		// announce the same subject on every run.
		if held, seen := newest[n.handle]; !seen || n.position > held.position ||
			(n.position == held.position && n.record > held.record) {
			newest[n.handle] = n
			m.Subject, m.Reason = n.subject, n.reason
		}
	}
	a.inboxed = nil
	out := make([]InboxMovement, 0, len(byHandle))
	for _, m := range byHandle {
		out = append(out, *m)
	}
	slices.SortFunc(out, func(x, y InboxMovement) int { return cmp.Compare(x.Handle, y.Handle) })
	return out
}
