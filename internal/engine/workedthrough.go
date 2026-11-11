package engine

import (
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/chatlet"
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
// an answer — its own writer's, after the seat's own last reply in the thread,
// before the message that woke the turn ([prefetch.Blocks.ThreadContextAnswered]).
// Not a message a bound dropped (the turn never saw it), not anything up to the
// seat's last reply (answered already), not ANOTHER WRITER'S (one turn works
// for one writer, ADR-0109, so their message is owed a turn of its own, held
// to their apps), and nothing at all when the read stopped short or did not
// reach the trigger (nothing then says which messages came before it).
// Recorded only when the turn COMPLETES, with the turn's own triggers: a turn
// that failed answered nothing.
//
// It is keyed by the message rather than by the delivery's event id because
// the turn knows only what the thread showed it, and the thread names messages
// — the delivery that carries M1 is a wake whose id the reading turn never
// saw. A trigger that is not a chat message in a thread has no such key and
// is untouched: a top-level direct message has no thread block, so a later
// turn never saw it and its retry is still owed an answer.
//
// # An edit is a version of the message, not the message
//
// A message's id names every version of it, and an edit's wake carries that
// same id as its `ts`. Keyed on the id alone, a mark dropped every later wake
// of the message — the wake of an edit made after the marking turn read the
// thread among them, whose words no turn ever saw, an edit that newly names
// the seat included. So an edited message is recorded at the version the block
// showed it ([notify.Message.Version]: on Chatlet, the log position of the
// record that last wrote it) and for whom the marking turn worked
// ([chatEditKey]), and an edit's wake is dropped by that key alone, built from
// its own trigger position and the carrier its writer stated: never by the
// message's key, which a mark of any earlier version holds. An edit made after
// the read, one made for another requester, and an earlier edit the turn saw
// only through a later one each run as a turn of their own, the last a
// duplicate, which is the safe direction. No writer is in the key: only a
// message's author edits it, and a mark covers only the trigger's writer's
// messages.
//
// The two halves are spelled alike ([statelog.Position.String]): the read's
// position is the row's, which the applier stamped from its checkpoint, and
// the wake's is the change feed's trigger, from the delivery and the
// generation its writer stamped. They differ only across a reanchor of the
// chat log, when the edit's wake runs again. NOT THE INSTANT THE LOG STORED
// THE EDIT, which an applier holds in two shapes by the path it applied on,
// where the position survives both.

// chatMessageKey is the completion-ledger key for one chat message, by its
// backend, its channel and the backend's own id for it.
func chatMessageKey(backend, channel, id string) string {
	return workkey.Derive([]string{"chat-message", backend, channel, id})
}

// chatEditKey is the completion-ledger key for one version of an edited chat
// message, worked for one whom: its backend, its channel, the backend's own id
// for it, the version ([notify.Message.Version], which is an edit's wake's
// trigger position) and whom the turn worked for, as the partition keys it
// ([notify.WorkingForKey]).
func chatEditKey(backend, channel, id, version string, workingFor *types.WorkingFor) string {
	return workkey.Derive([]string{"chat-edit", backend, channel, id, version,
		notify.WorkingForKey(workingFor)})
}

// chatKeyOf is the completion-ledger key for the chat message a trigger
// carries, whether that key is an edit's, and whether the trigger carries one
// at all: the message's `ts` in a thread on a chat backend, read off the
// notification's own metadata — the same address the thread block is read at.
//
// AN EDIT'S WAKE IS KEYED BY ITS VERSION ALONE ([chatEditKey]): its own
// trigger position and the carrier its writer stated (writtenForIn), never
// the message's key. One that carries no trigger position has no key, and
// runs.
func chatKeyOf(ev *events.Event) (key string, edit, ok bool) {
	if ev == nil {
		return "", false, false
	}
	n, isNotice := events.DataAs[*types.ExternalNotification](ev)
	if !isNotice || n == nil {
		return "", false, false
	}
	thread, inThread := notify.ThreadOf(n.Metadata)
	id := n.Metadata[notify.MessageIDField]
	if !inThread || id == "" {
		return "", false, false
	}
	if chatletEdit(n) {
		version := notify.TriggerOf(ev)
		if version == "" {
			return "", true, false
		}
		return chatEditKey(thread.Backend, thread.Channel, id, version, writtenForIn(n)), true, true
	}
	return chatMessageKey(thread.Backend, thread.Channel, id), false, true
}

// chatletEdit says a notification is the wake of an edit to a Chatlet
// message, as Chatlet's parser states it: its source, and its record's own op
// as the event type.
func chatletEdit(n *types.ExternalNotification) bool {
	return n.NotificationSource == chatlet.Source && n.SourceEventType == string(chatlet.OpEdit)
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
// thread block showed it that were waiting for an answer: each message's own
// key, and for an edited one the key of the version it was shown at, for whom
// the turn worked ([chatEditKey]).
func workedThroughKeys(thread notify.Thread, answered []prefetch.Answered,
	workingFor *types.WorkingFor) []string {

	if thread.Backend == "" || thread.Channel == "" || len(answered) == 0 {
		return nil
	}
	keys := make([]string, 0, len(answered))
	for _, a := range answered {
		keys = append(keys, chatMessageKey(thread.Backend, thread.Channel, a.ID))
		if a.Version != "" {
			keys = append(keys, chatEditKey(thread.Backend, thread.Channel, a.ID, a.Version,
				workingFor))
		}
	}
	return keys
}
