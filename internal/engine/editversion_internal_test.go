package engine

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// editedThread is a running node's private room where ceo asked ada something
// (the root), ada answered (the reply) in a turn for omar, then edited that
// answer to name ceo, and then said something more (the trigger). Every record is a raw peer
// record this node's own applier committed, as another node's writer
// publishes it, and the node's own chat feed relays their wakes, which the
// rig keeps.
type editedThread struct {
	e                    *Engine
	room                 string
	root, reply, trigger string
	mu                   sync.Mutex
	relayed              []types.RawWebhook
}

func newEditedThread(t *testing.T) *editedThread {
	t.Helper()
	e, back, _ := trimmedTracker(t)
	running := e.core.Load().log.Domain(chatlet.Domain{}.Name())
	if running == nil {
		t.Fatal("the chat domain is not running")
	}
	th := &editedThread{e: e, room: "9e1f0c3a-0000-4000-8000-0000000000e1",
		root: "01a2b3c4-0000-7000-8000-0000000000e1", reply: "01a2b3c4-0000-7000-8000-0000000000e2",
		trigger: "01a2b3c4-0000-7000-8000-0000000000e3"}
	// A GROUP OF ITS OWN on the inbound topic, so it is handed a copy of
	// every wake the feed relays, beside the notification service's.
	if err := back.Queue.Subscribe(t.Context(), topics.NotificationsInbound, "test.edit-relay",
		func(_ context.Context, ev *events.Event) queue.Result {
			if w, ok := events.DataAs[*types.RawWebhook](ev); ok && w != nil {
				th.mu.Lock()
				th.relayed = append(th.relayed, *w)
				th.mu.Unlock()
			}
			return queue.Ack()
		}); err != nil {
		t.Fatalf("subscribe to the relayed wakes: %v", err)
	}

	raw := rawChat{t: t, running: running}
	raw.applied(raw.peer(chatlet.ChannelSubject(th.room), chatlet.OpCreate, "op-create-ops",
		chatlet.ChannelCreate{V: chatlet.DocumentVersion, ChannelID: th.room, Kind: chatlet.KindPrivate,
			Members: []chatlet.Member{{Handle: "ceo"}, {Handle: "ada"}}, CreatedBy: "ceo",
			CreatedByKind: chatlet.AuthorAgent}))
	post := func(author, id, root, body string, whom *types.WorkingFor, mentions ...string) {
		rec := raw.peer(chatlet.MessageSubject(th.room), chatlet.OpPost, "op-"+id, chatlet.MessagePost{
			V: chatlet.DocumentVersion, MessageID: id, Body: body, ThreadRoot: root,
			Mentions: mentions, Author: author, AuthorKind: chatlet.AuthorAgent, MembersOnly: true})
		rec.Actor, rec.WorkingFor = author, whom
		rec.Notify = &chatlet.Notify{MessageID: id, ChannelID: th.room, ChannelKind: chatlet.KindPrivate,
			ThreadRoot: root, Author: author, AuthorKind: chatlet.AuthorAgent, Excerpt: body,
			Mentions: mentions}
		raw.applied(rec)
	}
	post("ceo", th.root, "", "@ada what ships on friday?", nil, "ada")
	post("ada", th.reply, th.root, "the importer", forOmar)
	edit := raw.peer(chatlet.MessageSubject(th.room), chatlet.OpEdit, "op-edit-"+th.reply,
		chatlet.MessageEdit{V: chatlet.DocumentVersion, MessageID: th.reply,
			Body: "@ceo the importer, and the export", Mentions: []string{"ceo"},
			EditedBy: "ada", EditedByKind: chatlet.AuthorAgent})
	edit.Actor = "ada"
	edit.Notify = &chatlet.Notify{MessageID: th.reply, ChannelID: th.room,
		ChannelKind: chatlet.KindPrivate, ThreadRoot: th.root, Author: "ada",
		AuthorKind: chatlet.AuthorAgent, Excerpt: "@ceo the importer, and the export",
		Mentions: []string{"ceo"}}
	raw.applied(edit)
	post("ada", th.trigger, th.root, "and the date?", forOmar)
	return th
}

// forOmar is whom ada's reply, and the turns that work it, were written for.
var forOmar = &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "omar"}

// trigger is ada's newest message in the thread, as its wake hands it to ceo:
// written for whom.
func (th *editedThread) triggerFor(whom *types.WorkingFor) *events.Event {
	ev := events.New(types.ExternalNotification{
		NotificationSource: chatlet.Source, SourceEventType: string(chatlet.OpPost),
		Sender: "ada", Body: "and the date?",
		Writer: notify.FirstPartyWriter(iam.ActorAgent, "ada", nil, whom),
		Metadata: map[string]string{
			notify.TransportField: chatlet.Source, notify.ChannelField: th.room,
			notify.ThreadField: th.root, notify.MessageIDField: th.trigger,
		},
	}, events.TraceContext{})
	ev.Source = "notify." + chatlet.Source
	return ev
}

// workedThrough is what a turn of ceo's woken by trigger reports as worked
// through, run to its depth guard: after the prefetch that read the thread,
// before anything is launched.
func (th *editedThread) workedThrough(t *testing.T, trigger *events.Event) []string {
	t.Helper()
	res, err := th.e.runTurn(t.Context(), Request{
		Handle: "ceo", WorkKey: "wk-" + trigger.ID.String(), Depth: 3, WorkSince: time.Now().UTC(),
		Events: []*events.Event{trigger},
	})
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	return res.WorkedThrough
}

// relayedEdit is the wake the node's chat feed relayed for the reply's edit.
func (th *editedThread) relayedEdit(t *testing.T) types.RawWebhook {
	t.Helper()
	var found *types.RawWebhook
	waitUntil(t, 20*time.Second, "the chat feed to relay the edit's wake", func() bool {
		th.mu.Lock()
		defer th.mu.Unlock()
		for i := range th.relayed {
			data, err := json.Marshal(th.relayed[i].Body)
			if err != nil {
				continue
			}
			rec, err := chatlet.Decode(data)
			if err == nil && rec.Op == chatlet.OpEdit && rec.Notify != nil &&
				rec.Notify.MessageID == th.reply {
				w := th.relayed[i]
				found = &w
				return true
			}
		}
		return false
	})
	return *found
}

// read is the thread as ceo reads it at a turn's start: the engine's own
// Chatlet reader, through the router.
func (th *editedThread) read(t *testing.T) notify.Transcript {
	t.Helper()
	got, ok := th.e.ChatThreads().ReadThread(t.Context(), "ceo",
		notify.Thread{Backend: chatlet.Source, Channel: th.room, Root: th.root})
	if !ok {
		t.Fatal("ceo could not read the thread")
	}
	return got
}

// AN EDIT'S WAKE NAMES THE VERSION THE THREAD READ SHOWS. The mark a turn makes
// of an edited message names the version its thread read handed over, and the
// drop keys an edit's wake on the trigger position the change feed relayed it
// with. The applier stamps the row from its checkpoint, and the feed stamps
// the trigger from the delivery and the generation its writer stamped — two
// derivations, so this is the one case that holds them to each other, through
// the node's own applier, feed and reader.
//
// Mutation: hand the reply's version over as anything but the position of the
// record that last wrote it, and the wake's trigger and the read disagree.
func TestAnEditsWakeNamesTheVersionTheThreadReadShows(t *testing.T) {
	t.Parallel()
	th := newEditedThread(t)
	wake := th.relayedEdit(t)
	if wake.Trigger == "" {
		t.Fatal("the edit's wake carries no trigger position")
	}
	var version string
	for _, m := range th.read(t).Messages {
		if m.ID == th.reply {
			version = m.Version
		}
	}
	if version != wake.Trigger {
		t.Fatalf("the thread read shows the edited reply at version %q, but its edit's wake carries "+
			"the trigger %q: a turn's mark of the reply would never drop that edit's wake", version,
			wake.Trigger)
	}
}

// A TURN RECORDS THE EDIT IT WAS SHOWN OF A WAITING REPLY, through the real
// [Engine.runTurn] on a Chatlet thread. Ada's trigger, written for omar, wakes
// ceo's turn; the turn is shown ada's earlier reply waiting at its edit, and
// reports the reply's own key and the key of that version for omar — the
// turn's own carrier, the one its ceiling is composed from. Nothing but
// runTurn fills WorkedThrough, so every dispatcher case that sets it by hand
// would keep passing with the line that fills it gone.
//
// Mutation: mark with no carrier, or drop the version from the read, and the
// reported keys differ.
func TestATurnRecordsTheEditItWasShownOfAWaitingReply(t *testing.T) {
	t.Parallel()
	th := newEditedThread(t)
	var version string
	for _, m := range th.read(t).Messages {
		if m.ID == th.reply {
			version = m.Version
		}
	}
	if version == "" {
		t.Fatal("the edited reply reads at no version, so this case proves nothing")
	}
	got := th.workedThrough(t, th.triggerFor(forOmar))
	want := []string{chatMessageKey(chatlet.Source, th.room, th.reply),
		chatEditKey(chatlet.Source, th.room, th.reply, version, forOmar)}
	if !slices.Equal(got, want) {
		t.Fatalf("the turn reported %v as worked through, want the reply's own key and its "+
			"version's for omar (%v)", got, want)
	}
}

// A TURN'S THREAD BLOCK IS TOLD WHOM THE TURN WORKS FOR, through the real
// [Engine.runTurn]: its own carrier, from which its ceiling is composed, never
// derived again. Ada's reply was written for omar. A turn ada's trigger woke
// for omar works it through; a turn her trigger woke for nobody else works for
// ada herself, and leaves omar's reply to a turn of its own, held to omar's
// apps, rather than answering it inside ada's.
//
// Mutation: hand the prefetch no carrier, or compare the sender alone, and a
// row goes red.
func TestATurnsThreadBlockIsToldWhomTheTurnWorksFor(t *testing.T) {
	t.Parallel()
	th := newEditedThread(t)
	if got := th.workedThrough(t, th.triggerFor(forOmar)); !slices.Contains(got,
		chatMessageKey(chatlet.Source, th.room, th.reply)) {
		t.Errorf("a turn for omar reported %v, want omar's waiting reply worked through", got)
	}
	if got := th.workedThrough(t, th.triggerFor(nil)); len(got) != 0 {
		t.Errorf("a turn ada's own trigger woke reported %v as worked through, want nothing: the "+
			"waiting reply was written for omar", got)
	}
}
