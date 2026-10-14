package engine_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/notify"
)

// THE NATIVE WORKING INDICATOR FOLLOWS THE COMPANY, at boot and on every apply.
//
// The setting `chat.native.typing_status` had no reader: nothing built a
// driver over the hub, at boot or later. These cases go through the engine's
// own two entry points — [engine.New] with a company and [engine.Engine.Apply]
// with the next revision — and read the answer back the way a turn does,
// through [engine.Engine.Status].

// indicatorDoc is a company on the engine's own chat (it declares no vendor
// chat surface, so its chat derives native), with its typing_status a hole.
const indicatorDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
chat:
  native:
    typing_status: MODE
units:
  - name: Engineering
    roles:
      - name: SWE
        handle: swe
        llm: zulu
`

func indicatorCompany(mode string) string {
	return strings.Replace(indicatorDoc, "MODE", mode, 1)
}

// quietHub is the process's live hub with nothing behind it: it accepts every
// raise and every clear.
type quietHub struct{}

var _ engine.ChatLive = (*quietHub)(nil)

func (*quietHub) Applied([]chat.Applied)          {}
func (*quietHub) StatusBackend() string           { return chat.Source }
func (*quietHub) SupportsStatusText() bool        { return true }
func (*quietHub) StatusRefresh() time.Duration    { return time.Minute }
func (*quietHub) AddressRule() notify.AddressRule { return chat.AddressRule() }
func (*quietHub) ClearStatus(context.Context, string, string, string) bool {
	return true
}

func (*quietHub) SetStatus(context.Context, string, string, string, string) bool {
	return true
}

// broadcast is a native message that reached the seat through a room-wide
// address: a wake, and not one that obliges an answer.
func broadcast() map[string]string {
	meta := map[string]string{
		notify.TransportField:    chat.Source,
		notify.ChannelField:      "room-1",
		notify.MessageIDField:    "msg-1",
		notify.FollowReasonField: string(chat.ReasonCollective),
	}
	meta[notify.ThreadAnchorField] = notify.Anchor(meta)
	return meta
}

func TestTheNativeIndicatorFollowsTheCompanyAtBootAndOnApply(t *testing.T) {
	t.Parallel()
	activated := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	e := newEngine(t, engine.Options{
		Company: parsedCompany(t, indicatorCompany("addressed")), ActivatedAt: activated,
		ChatLive: &quietHub{},
	})
	if s := e.Status().Begin(t.Context(), "swe", "turn-1", "execute",
		broadcast()); s != nil {
		s.End(t.Context(), false)
		t.Error("under `typing_status: addressed` a broadcast raised the indicator")
	}
	if !slices.Contains(e.Status().Backends(), chat.Source) {
		t.Fatalf("the boot built no native indicator: the drivers are %v",
			e.Status().Backends())
	}

	if _, _, err := e.Apply(t.Context(), parsedCompany(t, indicatorCompany("always")),
		activated.Add(time.Minute)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	s := e.Status().Begin(t.Context(), "swe", "turn-2", "execute", broadcast())
	if s == nil {
		t.Fatal("after a revision set `typing_status: always` a broadcast raised " +
			"nothing — the apply did not reach the native indicator")
	}
	s.End(t.Context(), false)
}
