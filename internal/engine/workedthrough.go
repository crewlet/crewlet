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
// not run again as a turn of its own. A failed turn is NAKed
// ([Dispatcher.dispatch]) and comes back BEHIND the conversation's newer mail
// (queue.OutcomeNak), so M2's turn can answer the thread with M1 in it before
// M1 runs again: a duplicate, out of order.
//
// The mark lives in the completion ledger, in the coordination store, since
// the redelivery can reach whichever node holds the seat by then. It is keyed
// on the message's identity on its chat backend ([chatMessageKey]), not the
// delivery's event id, which the reading turn never saw, and recorded only
// when the turn completes. A trigger that is not a message in a thread (a
// top-level direct message) has no key: no later turn saw it, so its retry runs.

// chatMessageKey is the completion-ledger key for one chat message, by its
// backend, its channel and the backend's own id for it.
func chatMessageKey(backend, channel, id string) string {
	return workkey.Derive([]string{"chat-message", backend, channel, id})
}

// chatEditKey is the completion-ledger key for one version of an edited chat
// message worked for one whom: its backend, channel and id, the version
// ([notify.Message.Version]: on Chatlet the log position of the record that
// last wrote it, which is also an edit's wake's trigger position) and
// [notify.WorkingForKey]. A key on the id alone would drop the wake of an edit
// made after the read, words no turn saw; an edit after the read, for another
// requester, or seen only through a later one runs again (a duplicate, the
// safe direction). No writer is in it: only the author edits a message, and a
// mark covers only the trigger's writer's. Both sides spell the version by
// [statelog.Position.String] (the read's from its row, the wake's from the
// change feed's trigger), differing only across a chat-log reanchor, when the
// wake runs again; never the stored instant, which has two shapes by apply path.
func chatEditKey(backend, channel, id, version string, workingFor *types.WorkingFor) string {
	return workkey.Derive([]string{"chat-edit", backend, channel, id, version,
		notify.WorkingForKey(workingFor)})
}

// chatKeyOf is the completion-ledger key for the chat message a trigger
// carries, whether that key is an edit's, and whether the trigger carries one
// at all: the message's `ts` in a thread on a chat backend, read off the
// notification's own metadata, the address the thread block is read at. An
// edit's wake is keyed by its version alone ([chatEditKey]) and the carrier
// its writer stated (writtenForIn), never by the message's key; one with no
// trigger position has no key, and runs.
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
// turn's trigger carries in a thread (where the thread block measures "before"
// from), empty where it carries none. It reads the SAME notification
// [threadOf] does, so message and thread are never two conversations; a
// coalesced trigger's metadata mirrors its LATEST constituent, so everything
// the block showed before it precedes every message the turn was woken for.
func triggerMessageOf(evs []*events.Event) string {
	for _, n := range notificationsIn(evs) {
		if _, ok := notify.ThreadOf(n.Metadata); ok {
			return n.Metadata["ts"]
		}
	}
	return ""
}

// workedThroughKeys are the completion-ledger keys for the messages a turn's
// thread block showed it waiting for an answer, as
// [prefetch.Blocks.ThreadContextAnswered] chooses them (the trigger's writer's,
// for the same whom, after the seat's last reply: another's is owed a turn of
// its own, ADR-0109): each one's own key, and for an edited one the key of the
// version it was shown at ([chatEditKey]).
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
