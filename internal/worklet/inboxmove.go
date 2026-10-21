package worklet

import (
	"cmp"
	"slices"
	"time"
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
// derived HERE rather than by the change feed: the feed relays a record to ONE
// node, while every DATA node applies every record, and only a data node
// serves sockets (ingress requires `data`). So each node with sockets open
// tells its own, from its own apply, and nothing crosses between nodes. A node
// without `data` runs no applier and has no dashboards to tell.
//
// # A HINT, NEVER CONTENT
//
// A movement names a handle, how many notices arrived for it — none, when the
// batch only took notices away, which a purge does — and the subject and
// reason of the newest — identifiers, never an excerpt, a title or who wrote
// it. It rides a socket frame routed to whoever watches that seat, and
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
// EXACTLY FIVE FIELDS, and a test holds the set: four identifiers and the
// broker's instant, and a field that carried content would be a read path the
// inbox question's own authority never decided.
type InboxMovement struct {
	// Handle is whose inbox moved.
	Handle string `json:"handle"`

	// UnreadDelta is how many notices the batch added for them, and ZERO
	// for an inbox the batch only took notices OUT of — a purge deletes the
	// notices its task's records routed ([Applier.forgetRecords]), and the
	// screen listing them has to re-read as surely as for an arrival.
	//
	// Never negative: a notice arrives unread, but one a purge deletes may
	// have been read long ago, and nothing here knows which — a person
	// marking their own read is their own gesture on their own screen. So a
	// removal moves the inbox without claiming a count.
	UnreadDelta int `json:"unread_delta"`

	// Subject is the id of the newest notice's subject, which is what a
	// screen can highlight without reading anything it did not ask for —
	// the newest notice the batch ADDED, and only where it added none, the
	// purged task whose notices it took away.
	Subject string `json:"subject"`

	// Reason is the one reason that notice reached them under.
	Reason Reason `json:"reason"`

	// At is the broker's own instant for the OLDEST record of the batch
	// that moved this inbox — when the first of its notices was stored,
	// which every node reads alike — and what the socket's push is timed
	// from (internal/objectives' change visibility). The oldest because
	// one push answers every record in the batch, and the person waited
	// longest for the first of them. An instant, never content, and it
	// crosses the live relay with the rest, so a peer's movement is timed
	// from the same instant as this node's.
	At time.Time `json:"at,omitzero"`
}

// inboxNote is one notice a batch wrote or deleted, until the commit is known.
type inboxNote struct {
	record, handle, subject string
	reason                  Reason
	position                int64

	// at is the broker's instant for the record that wrote or deleted it.
	at time.Time

	// removed is a notice the batch DELETED rather than wrote — a purge's
	// ([Applier.forgetRecords]). It moves its recipient's inbox and adds
	// nothing to [InboxMovement.UnreadDelta].
	removed bool
}

// noteInbox records one notice this transaction wrote or deleted.
//
// KEYED ON (record, recipient), which is the notification row's own key: the
// store re-runs the body of an attempt that failed transiently, and a note
// kept per call would announce the re-run's notice as a second one. A written
// notice and a deleted one never share a key, because a purge deletes only the
// notices of records before its own.
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
		if !n.removed {
			m.UnreadDelta++
		}
		if !n.at.IsZero() && (m.At.IsZero() || n.at.Before(m.At)) {
			m.At = n.at
		}
		// THE NEWEST BY POSITION, a tie broken on the record id: a map's
		// iteration order is not an answer, and the same batch must
		// announce the same subject on every run. An ARRIVAL outranks a
		// removal whatever their positions, because the subject is what a
		// screen highlights and a deleted notice is nothing to point at.
		if held, seen := newest[n.handle]; !seen || newerNote(n, held) {
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

// newerNote is whether n names a batch's movement for its recipient ahead of
// held: an arrival before a removal, then the later position, then the larger
// record id.
func newerNote(n, held inboxNote) bool {
	if n.removed != held.removed {
		return !n.removed
	}
	if n.position != held.position {
		return n.position > held.position
	}
	return n.record > held.record
}
