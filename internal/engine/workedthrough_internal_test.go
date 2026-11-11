package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RECORD AND THE DROP NAME A MESSAGE THE SAME WAY. A turn records what its
// thread block showed it under the thread it read and the ids the block
// reported; a retried message is looked up under its own metadata. If the two
// derivations disagreed by a byte, every record would be written and no drop
// would ever match it — a guard that cannot fire, passing every test that
// stubs one side.
func TestTheWorkedThroughRecordAndTheDropAgreeOnAMessagesKey(t *testing.T) {
	t.Parallel()
	message := func(ts string) *events.Event {
		return events.New(types.ExternalNotification{
			NotificationSource: "mattermost", SourceEventType: "message",
			Metadata: map[string]string{
				notify.TransportField: "mattermost", "channel": "C9",
				"thread_ts": "root-post", "ts": ts,
			},
		}, events.TraceContext{})
	}
	waiting, trigger := message("post-1"), message("post-2")

	if got := triggerMessageOf([]*events.Event{trigger}); got != "post-2" {
		t.Fatalf("the trigger's message is %q, want its own ts", got)
	}
	recorded := workedThroughKeys(threadOf([]*events.Event{trigger}),
		[]prefetch.Answered{{ID: "post-1"}}, nil)
	looked, edit, ok := chatKeyOf(waiting)
	if !ok || edit || len(recorded) != 1 || recorded[0] != looked {
		t.Fatalf("recorded %v, the retry looks up %q (edit=%v, ok=%v): the two do not agree",
			recorded, looked, edit, ok)
	}
	// AND A TOP-LEVEL MESSAGE HAS NO KEY: no thread block ever showed it to a
	// later turn, so nothing may drop it as answered there.
	toplevel := events.New(types.ExternalNotification{
		Metadata: map[string]string{notify.TransportField: "mattermost", "channel": "C9", "ts": "post-3"},
	}, events.TraceContext{})
	if _, _, ok := chatKeyOf(toplevel); ok {
		t.Fatal("a top-level message was given a worked-through key")
	}
}

// AN EDITED MESSAGE IS RECORDED AT THE VERSION THE BLOCK SHOWED, FOR WHOM THE
// TURN WORKED, and an edit's wake is looked up by that version alone. Bea's
// trigger, written for ada, showed bea's earlier message at its edit e1. The
// mark drops e1's wake made for ada and the redelivery of the message's post,
// and nothing else: not the wake of a later edit e2, whose words no turn saw,
// and not e1's wake made for omar, which is owed a turn worked for omar. An
// unedited message is recorded by its id alone, and an edit's wake that
// carries no trigger position has no key, so it runs.
//
// Mutation: key an edit's wake by the message's key, as main did; write no
// edit key; key the post's version for an unedited message; or leave the
// carrier or the version out of the edit key; and a row goes red.
func TestAMarkDropsOnlyTheEditItsTurnWasShownForTheSameWhom(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	omar := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "omar"}
	wake := func(id string, op chatlet.OpKind, trigger string, workingFor *types.WorkingFor) *events.Event {
		ev := events.New(types.ExternalNotification{
			NotificationSource: chatlet.Source, SourceEventType: string(op),
			Writer: notify.FirstPartyWriter(iam.ActorAgent, "bea", nil, workingFor),
			Metadata: map[string]string{
				notify.TransportField: chatlet.Source, notify.ChannelField: "room-1",
				notify.ThreadField: "msg-0", notify.MessageIDField: id,
			},
		}, events.TraceContext{})
		if trigger != "" {
			ev.Payload = map[string]any{notify.TriggerField: trigger}
		}
		return ev
	}
	at := func(seq uint64) string {
		return statelog.Position{Stream: "chat", Generation: 1, Seq: seq}.String()
	}
	e1, e2 := at(41), at(57)

	marked := workedThroughKeys(threadOf([]*events.Event{wake("msg-3", chatlet.OpPost, at(60), ada)}),
		[]prefetch.Answered{{ID: "msg-1", Version: e1}, {ID: "msg-2"}}, ada)
	dropped := func(ev *events.Event) bool {
		key, _, ok := chatKeyOf(ev)
		return ok && slices.Contains(marked, key)
	}

	if key, edit, ok := chatKeyOf(wake("msg-1", chatlet.OpEdit, e1, ada)); !ok || !edit ||
		!slices.Contains(marked, key) {
		t.Errorf("the wake of edit e1, made for ada, is not dropped by the mark of a turn shown e1 "+
			"working for ada (key %q, edit %v, ok %v; the mark holds %v)", key, edit, ok, marked)
	}
	if !dropped(wake("msg-1", chatlet.OpPost, at(30), ada)) {
		t.Error("the redelivery of msg-1's post is not dropped: the turn answered the message")
	}
	if dropped(wake("msg-1", chatlet.OpEdit, e2, ada)) {
		t.Error("the wake of a later edit e2 is dropped by a mark of e1: no turn was shown its words")
	}
	if dropped(wake("msg-1", chatlet.OpEdit, e1, omar)) {
		t.Error("the wake of edit e1 made for omar is dropped by a turn that worked for ada: it is " +
			"owed a turn worked for omar, held to omar's apps")
	}
	if key, edit, ok := chatKeyOf(wake("msg-1", chatlet.OpEdit, "", ada)); ok || !edit {
		t.Errorf("an edit's wake with no trigger position has the key %q (edit %v, ok %v), want "+
			"none: nothing says which version it is, so it runs", key, edit, ok)
	}
	if !dropped(wake("msg-2", chatlet.OpPost, at(35), ada)) {
		t.Error("the redelivery of the unedited msg-2's post is not dropped")
	}
	if len(marked) != 3 {
		t.Errorf("the mark holds %d keys (%v), want three: msg-1's own and its version e1's, and "+
			"msg-2's own, since an unedited message names no version", len(marked), marked)
	}
}
