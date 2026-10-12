package engine

import (
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/workkey"
)

// The worked-through record: a chat message a later turn already answered is
// not run again as a turn of its own.
//
// # The failure it closes
//
// A turn that fails before anything leaves the engine is NAKed so it runs
// again (see [Dispatcher.dispatch]), and on the only broker this engine ships a
// NAKed message comes back after its redelivery backoff, BEHIND the
// conversation's newer mail (queue.OutcomeNak). So M1 fails, M2 — the next
// message in the same thread — runs first, and M2's turn reads the thread at
// its start and sees M1 there, unanswered: it answers the thread as it stands,
// M1 included. Then M1 comes round and runs as a turn of its own, answering a
// question the thread has already moved past — a duplicate, and out of order.
//
// # Where the mark lives, and why there
//
// In the COMPLETION LEDGER, beside the record of the triggers a turn was woken
// for, and keyed on the message's own identity on its chat backend
// ([chatMessageKey]). The question "who has to agree on it?" has the same
// answer as for the ledger itself: whichever node the redelivery reaches —
// a failed delivery goes back to the broker and the seat can move before it
// returns — so it is fleet state in the coordination store, not this node's
// memory and not the seat's local store. And it IS the ledger's fact: "this
// trigger has been worked", by a turn that saw it rather than one it woke.
//
// # What counts as worked through
//
// Exactly the messages the turn's thread block SHOWED it that were waiting for
// an answer — somebody else's, after the seat's own last reply in the thread,
// before the message that woke the turn ([prefetch.Blocks.ThreadContextAnswered]).
// Not a message a bound dropped (the turn never saw it), not anything up to the
// seat's last reply (answered already), and nothing at all when the read
// stopped short or did not reach the trigger (nothing then says which messages
// came before it). Recorded only when the turn COMPLETES, with the turn's own
// triggers: a turn that failed answered nothing.
//
// It is keyed by the message rather than by the delivery's event id because
// the turn knows only what the thread showed it, and the thread names messages
// — the delivery that carries M1 is a wake whose id the reading turn never
// saw. A trigger that is not a chat message in a thread has no such key and
// is untouched: a top-level direct message has no thread block, so a later
// turn never saw it and its retry is still owed an answer.

// chatMessageKey is the completion-ledger key for one chat message, by its
// backend, its channel and the backend's own id for it.
func chatMessageKey(backend, channel, id string) string {
	return workkey.Derive([]string{"chat-message", backend, channel, id})
}

// chatKeyOf is the completion-ledger key for the chat message a trigger
// carries, and whether it carries one: the message's `ts` in a thread on a
// chat backend, read off the notification's own metadata — the same address
// the thread block is read at.
func chatKeyOf(ev *events.Event) (string, bool) {
	if ev == nil {
		return "", false
	}
	n, ok := events.DataAs[*types.ExternalNotification](ev)
	if !ok || n == nil {
		return "", false
	}
	thread, ok := notify.ThreadOf(n.Metadata)
	id := n.Metadata["ts"]
	if !ok || id == "" {
		return "", false
	}
	return chatMessageKey(thread.Backend, thread.Channel, id), true
}

// triggerMessageOf is the backend's own id for the newest chat message a
// turn's trigger carries in a thread — the message the thread block measures
// "before" from — and empty where it carries none.
//
// The SAME notification [threadOf] reads the thread off, so the message and
// the thread it is looked for in can never be two different conversations. A
// coalesced trigger's flat metadata mirrors its LATEST constituent, which is
// the newest message: everything the block showed before it came before every
// message this turn was woken for.
func triggerMessageOf(evs []*events.Event) string {
	for _, n := range notificationsIn(evs) {
		if _, ok := notify.ThreadOf(n.Metadata); ok {
			return n.Metadata["ts"]
		}
	}
	return ""
}

// workedThroughKeys are the completion-ledger keys for the messages a turn's
// thread block showed it that were waiting for an answer.
func workedThroughKeys(thread notify.Thread, answered []string) []string {
	if thread.Backend == "" || thread.Channel == "" || len(answered) == 0 {
		return nil
	}
	keys := make([]string, 0, len(answered))
	for _, id := range answered {
		keys = append(keys, chatMessageKey(thread.Backend, thread.Channel, id))
	}
	return keys
}
