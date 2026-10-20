package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// THE ENGINE'S OWN CHAT'S WORKING INDICATOR, from the company's setting to the
// hub a person's screen reads it from.
//
// The hub implemented [notify.StatusPoster] completely and nothing ever built
// a driver over it: [Engine.Status] fanned a native turn's raise across the
// vendor drivers alone, each refused a transport that was not its own, and
// `chat.native.typing_status` had no reader. So these cases start at the
// company and the engine's own entry points and end at what the hub was asked
// to show.

// recordingHub is the process's live hub as the engine sees it: an applier
// observer it does not exercise here, and a poster that records what it was
// asked to show.
type recordingHub struct {
	mu      sync.Mutex
	raised  []string
	cleared int
	// posted is closed and replaced on every raise, so a waiter that took it
	// under the same lock as its snapshot cannot miss the next one.
	posted chan struct{}
}

func newRecordingHub() *recordingHub { return &recordingHub{posted: make(chan struct{})} }

var _ ChatLive = (*recordingHub)(nil)

func (*recordingHub) Applied([]chat.Applied)          {}
func (*recordingHub) Reread()                         {}
func (*recordingHub) ServesSockets() bool             { return true }
func (*recordingHub) StatusBackend() string           { return chat.Source }
func (*recordingHub) SupportsStatusText() bool        { return true }
func (*recordingHub) StatusRefresh() time.Duration    { return time.Minute }
func (*recordingHub) AddressRule() notify.AddressRule { return chat.AddressRule() }
func (h *recordingHub) ClearStatus(context.Context, string, string, string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleared++
	return true
}

func (h *recordingHub) SetStatus(_ context.Context, post notify.StatusPost) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.raised = append(h.raised, post.Handle+" in "+post.Channel+": "+post.Text)
	close(h.posted)
	h.posted = make(chan struct{})
	return true
}

// awaitRaise waits for the hub to have been asked to show something.
func (h *recordingHub) awaitRaise(t *testing.T) []string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		h.mu.Lock()
		raised, posted := append([]string(nil), h.raised...), h.posted
		h.mu.Unlock()
		if len(raised) > 0 {
			return raised
		}
		select {
		case <-posted:
		case <-deadline:
			t.Fatal("the hub was never asked to show the seat working")
		}
	}
}

func (h *recordingHub) clears() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cleared
}

// nativeChatCompany is a company on the engine's own chat with this
// `typing_status`, empty taking the default.
func nativeChatCompany(typing config.WorkingStatus) *Company {
	c := &config.Company{Name: "Acme", Chat: config.Chat{Backend: config.ChatNative}}
	if typing != "" {
		c.Chat.Native = &config.ChatNativeConfig{TypingStatus: typing}
	}
	return &Company{Config: c}
}

// nativeTrigger is one message in a native room, stamped the way the chat
// parser stamps what it routes.
func nativeTrigger(addressed bool) *events.Event {
	meta := map[string]string{
		notify.TransportField: chat.Source,
		notify.ChannelField:   "room-1",
		notify.MessageIDField: "msg-1",
		notify.ThreadField:    "",
	}
	meta[notify.ThreadAnchorField] = notify.Anchor(meta)
	// WHY IT REACHED THE SEAT is what [chat.AddressRule] reads: a mention
	// obliges an answer, a room-wide address does not.
	meta[notify.FollowReasonField] = string(chat.ReasonCollective)
	if addressed {
		meta[notify.FollowReasonField] = string(chat.ReasonMention)
	}
	ev := events.New(types.ExternalNotification{
		NotificationSource: chat.Source, SourceEventType: "post",
		Sender: "ana", Subject: "#launch", Body: "can you look at this",
		Metadata: meta,
	}, events.TraceContext{})
	ev.Source = "notify." + chat.Source
	return ev
}

// A NATIVE CHAT TURN RAISES ITS INDICATOR ON THE HUB AND CLEARS IT — the whole
// of the defect.
func TestANativeChatTurnShowsItsSeatWorkingOnTheHub(t *testing.T) {
	t.Parallel()
	hub := newRecordingHub()
	e := &Engine{}
	e.SetChatLive(t.Context(), hub)
	e.applyChatStatus(t.Context(), nativeChatCompany(""))
	t.Cleanup(func() { e.stopNotifications(context.Background()) })

	s := e.beginWorkingStatus(t.Context(), "swe", "turn-1",
		[]*events.Event{nativeTrigger(false)})
	if s == nil {
		t.Fatal("a native chat turn raised no indicator — nothing drives the hub")
	}
	if raised := hub.awaitRaise(t); raised[0] == "" {
		t.Errorf("the hub was asked to show %v", raised)
	}
	endWorkingStatus(t.Context(), s, false)
	if hub.clears() == 0 {
		t.Error("the ended turn left its indicator up on the hub")
	}
}

// THE MODE IS `chat.native.typing_status`, read rather than assumed: under
// `addressed` a message nobody asked this seat raises nothing, and one that
// asked it does.
func TestTheNativeIndicatorFollowsTheCompanysTypingStatus(t *testing.T) {
	t.Parallel()
	hub := newRecordingHub()
	e := &Engine{}
	e.SetChatLive(t.Context(), hub)
	e.applyChatStatus(t.Context(), nativeChatCompany(config.StatusAddressed))
	t.Cleanup(func() { e.stopNotifications(context.Background()) })

	if s := e.beginWorkingStatus(t.Context(), "swe", "turn-1",
		[]*events.Event{nativeTrigger(false)}); s != nil {
		t.Error("under `addressed`, a message nobody asked this seat raised the indicator")
	}
	s := e.beginWorkingStatus(t.Context(), "swe", "turn-2",
		[]*events.Event{nativeTrigger(true)})
	if s == nil {
		t.Fatal("under `addressed`, a message that asked this seat raised nothing")
	}
	endWorkingStatus(t.Context(), s, false)
}

// AN APPLY THAT CHANGES THE MODE TAKES EFFECT, one that changes nothing keeps
// the driver — and its live indicators — and one that leaves native chat takes
// the indicator away.
func TestTheNativeIndicatorFollowsTheAppliedRevision(t *testing.T) {
	t.Parallel()
	hub := newRecordingHub()
	e := &Engine{}
	e.SetChatLive(t.Context(), hub)
	e.applyChatStatus(t.Context(), nativeChatCompany(config.StatusAddressed))
	t.Cleanup(func() { e.stopNotifications(context.Background()) })
	first := e.notify.nativeStatus

	e.applyChatStatus(t.Context(), nativeChatCompany(config.StatusAddressed))
	if e.notify.nativeStatus != first {
		t.Error("an apply that changed nothing the indicator reads rebuilt its " +
			"driver, which takes down every indicator a running turn holds")
	}

	e.applyChatStatus(t.Context(), nativeChatCompany(config.StatusAlways))
	if got := e.notify.nativeStatus.Mode(); got != notify.StatusAlways {
		t.Errorf("after a revision set `typing_status: always` the driver is %q", got)
	}

	vendor := nativeChatCompany("")
	vendor.Config.Chat.Backend = config.ChatVendor
	e.applyChatStatus(t.Context(), vendor)
	if e.notify.nativeStatus != nil {
		t.Error("a company that left native chat kept a native indicator driver")
	}
	if s := e.beginWorkingStatus(t.Context(), "swe", "turn-1",
		[]*events.Event{nativeTrigger(true)}); s != nil {
		t.Error("a company off native chat raised a native indicator")
	}
}

// NO HUB, NO DRIVER: a process that handed the engine no live half has nowhere
// to show an indicator, and a driver over a nil poster would be a feature that
// reports itself on and does nothing.
func TestANodeWithNoHubBuildsNoNativeIndicator(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	e.applyChatStatus(t.Context(), nativeChatCompany(""))
	if e.notify.nativeStatus != nil {
		t.Error("an engine with no hub built a native indicator driver")
	}
}

// THE HUB ARRIVES AFTER THE BOOT COMPANY, AND ATTACHING IT BUILDS THE DRIVER.
//
// [New] runs [Engine.startNotifications] — the call that builds the indicator
// for the company a node boots with — before the process can attach its hub,
// so that call finds no hub and builds nothing. Unless the attachment builds
// it, the boot company shows no seat working until its first apply, which on a
// company nobody edits is never. And a hub REPLACED takes its driver with it:
// a driver left over the old one raises indicators no socket is attached to.
//
// Mutation: drop the applyChatStatus call from SetChatLive, and the boot
// company has no driver; drop the attachment from the comparison that keeps a
// driver, and the replaced hub keeps the old one's.
func TestAttachingAHubBuildsTheBootCompanysIndicator(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	e.epoch.current.Store(nativeChatCompany(""))
	// THE BOOT: startNotifications' own call, with no hub yet.
	e.applyChatStatus(t.Context(), e.Company())
	if e.notify.nativeStatus != nil {
		t.Fatal("an engine with no hub yet built a native indicator driver")
	}
	t.Cleanup(func() { e.stopNotifications(context.Background()) })

	first := newRecordingHub()
	e.SetChatLive(t.Context(), first)
	if e.notify.nativeStatus == nil {
		t.Fatal("attaching the hub after the boot built no indicator for the boot " +
			"company, so its seats show nothing until its first apply")
	}
	s := e.beginWorkingStatus(t.Context(), "swe", "turn-1",
		[]*events.Event{nativeTrigger(false)})
	if s == nil {
		t.Fatal("a native chat turn raised no indicator on the attached hub")
	}
	first.awaitRaise(t)
	endWorkingStatus(t.Context(), s, false)

	second := newRecordingHub()
	was := e.notify.nativeStatus
	e.SetChatLive(t.Context(), second)
	if e.notify.nativeStatus == was {
		t.Fatal("a replaced hub kept the driver built over the old one")
	}
	s = e.beginWorkingStatus(t.Context(), "swe", "turn-2",
		[]*events.Event{nativeTrigger(false)})
	if s == nil {
		t.Fatal("a native chat turn raised no indicator on the replacing hub")
	}
	second.awaitRaise(t)
	endWorkingStatus(t.Context(), s, false)

	e.SetChatLive(t.Context(), nil)
	if e.notify.nativeStatus != nil {
		t.Error("a detached hub left its indicator driver behind")
	}
}
