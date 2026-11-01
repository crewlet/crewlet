package inbox

import (
	"cmp"
	"slices"
)

// HOW A PERSON LEARNS THEY HAVE WORK.
//
// A domain's applier writes the inbox — one notice row per (record,
// recipient) — and a WAKE reaches an agent through that domain's change feed.
// A person has no turn to wake: an agent's mention reaches them on Slack or
// Jira, if they have those, and a Crewlet-only person has neither. The
// dashboard is that person's delivery surface, and it polled.
//
// So the applier also says, after a committed batch, whose inbox moved. It is
// derived by the APPLIER rather than by the change feed: the feed relays a
// record to ONE node, while every DATA node applies every record, and only a
// data node serves sockets (ingress requires `data`). So each node with
// sockets open tells its own, from its own apply, and nothing crosses between
// nodes — but for a node whose own copy of the domain is out of service,
// whose movements the engine relays from a sound peer (internal/engine's
// inboxrelay.go). A node without `data` runs no applier and has no dashboards
// to tell.
//
// # A HINT, NEVER CONTENT
//
// A movement names its domain, a handle, how many notices arrived for it —
// none, when the batch only took notices away, which a purge does — and the
// subject and reason of the newest: identifiers, never an excerpt, a title or
// who wrote it. It rides a socket frame routed to whoever watches that seat,
// and what the frame is for is telling a screen to re-read the inbox it
// already polls, through the same question and the same authority as the
// poll. A payload carrying content would be a second read path around that
// authority.
//
// The count is a hint too, and says so: a transaction body the store re-runs,
// or a batch that failed and is applied again, can announce one notice twice.
// The rows are the truth and the screen reads them; nothing adds these counts
// up.

// Movement is one person's inbox having moved in one committed batch of one
// domain's applier on this node.
//
// EXACTLY FIVE FIELDS, and a test holds the set: every one of them is an
// identifier, and a field that carried content would be a read path the inbox
// question's own authority never decided.
type Movement struct {
	// Source is the domain whose applier wrote the notices — its state log
	// name, `tracker` or `code` — so it names the one copy whose service the
	// announcement depends on: a node relays a domain's movements from a
	// peer exactly while its own copy of THAT domain is out of service.
	// And it tells a screen whose half of its inbox moved.
	Source string `json:"source"`

	// Handle is whose inbox moved.
	Handle string `json:"handle"`

	// UnreadDelta is how many notices the batch added for them, and ZERO
	// for an inbox the batch only took notices OUT of — a purge deletes the
	// notices its object's records routed, and the screen listing them has
	// to re-read as surely as for an arrival.
	//
	// Never negative: a notice arrives unread, but one a purge deletes may
	// have been read long ago, and nothing here knows which — a person
	// marking their own read is their own gesture on their own screen. So a
	// removal moves the inbox without claiming a count.
	UnreadDelta int `json:"unread_delta"`

	// Subject is the id of the newest notice's subject, which is what a
	// screen can highlight without reading anything it did not ask for —
	// the newest notice the batch ADDED, and only where it added none, the
	// purged object whose notices it took away.
	Subject string `json:"subject"`

	// Reason is the one reason that notice reached them under, in its
	// domain's own vocabulary.
	Reason string `json:"reason"`
}

// Note is one notice a batch wrote or deleted, until the commit is known.
type Note struct {
	// Record is the notice's own record id, and Handle its recipient: the
	// notice row's key.
	Record, Handle string

	// Subject and Reason are what a movement names of its newest notice.
	Subject, Reason string

	// Position is the record's PACKED position on its log, which orders a
	// batch's notices newest first.
	Position int64

	// Removed is a notice the batch DELETED rather than wrote — a purge's.
	// It moves its recipient's inbox and adds nothing to
	// [Movement.UnreadDelta].
	Removed bool
}

// Batch is the notices one applier's transactions wrote or deleted, until the
// commit says whose inbox they moved: filled inside Apply and drained by the
// post-commit half.
//
// NOT GUARDED BY A MUTEX, and the state log's applier contract is why: an
// applier is ONE writer, and its Apply and Committed are called from the same
// goroutine with the commit in between. Its zero value is an empty batch.
type Batch struct {
	notes map[[2]string]Note
}

// Note records one notice this transaction wrote or deleted.
//
// KEYED ON (record, recipient), which is the notice row's own key: the store
// re-runs the body of an attempt that failed transiently, and a note kept per
// call would announce the re-run's notice as a second one. A written notice
// and a deleted one never share a key, because a purge deletes only the
// notices of records before its own.
func (b *Batch) Note(n Note) {
	if b.notes == nil {
		b.notes = map[[2]string]Note{}
	}
	b.notes[[2]string{n.Record, n.Handle}] = n
}

// Drain is the batch's notices as one movement per handle, each naming
// source, sorted by handle so a batch announces identically on every run — and
// it resets the batch. AFTER THE COMMIT and never inside the transaction: the
// store re-runs the body of an attempt that failed transiently, and a
// movement announced from inside it would announce a notice no row holds.
func (b *Batch) Drain(source string) []Movement {
	if len(b.notes) == 0 {
		return nil
	}
	byHandle := map[string]*Movement{}
	newest := map[string]Note{}
	for _, n := range b.notes {
		m, ok := byHandle[n.Handle]
		if !ok {
			m = &Movement{Source: source, Handle: n.Handle}
			byHandle[n.Handle] = m
		}
		if !n.Removed {
			m.UnreadDelta++
		}
		// THE NEWEST BY POSITION, a tie broken on the record id: a map's
		// iteration order is not an answer, and the same batch must
		// announce the same subject on every run. An ARRIVAL outranks a
		// removal whatever their positions, because the subject is what a
		// screen highlights and a deleted notice is nothing to point at.
		if held, seen := newest[n.Handle]; !seen || newerNote(n, held) {
			newest[n.Handle] = n
			m.Subject, m.Reason = n.Subject, n.Reason
		}
	}
	b.notes = nil
	out := make([]Movement, 0, len(byHandle))
	for _, m := range byHandle {
		out = append(out, *m)
	}
	slices.SortFunc(out, func(x, y Movement) int { return cmp.Compare(x.Handle, y.Handle) })
	return out
}

// newerNote is whether n names a batch's movement for its recipient ahead of
// held: an arrival before a removal, then the later position, then the larger
// record id.
func newerNote(n, held Note) bool {
	if n.Removed != held.Removed {
		return !n.Removed
	}
	if n.Position != held.Position {
		return n.Position > held.Position
	}
	return n.Record > held.Record
}
