package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// WHERE AN ASKER MAY PROMISE TO REPORT A DECISION is the chart's, read from the
// epoch current when the tool runs: a chat surface counts for a seat only when
// the company runs it AND the seat holds a bot on it — each alone admits a
// promise nobody can keep — and the channels are the ones a unit declares,
// once each.
func TestTheChannelDirectoryIsTheChartsSurfacesAndUnitChannels(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	e.epoch.current.Store(companyFor(t, `
name: Acme
providers:
  llm:
    gateway:
      model: sonnet
integrations:
  mattermost:
    enabled: false
    url: https://chat.example.com
    team: acme
  slack: {}
units:
  - name: Engineering
    channel: engineering
    children:
      - name: Platform
      - name: Release
        channel: releases
    roles:
      - name: Dev
        handle: dev
        integrations:
          slack:
            bot_token: ${DEV_SLACK_BOT}
            signing_secret: ${DEV_SLACK_SIGNING}
          mattermost:
            bot_token: ${DEV_MM_BOT}
      - name: Ops
        handle: ops
`))
	directory := liveChannels{engine: e}
	if got := directory.ChatSurfaces("dev"); !slices.Equal(got, []worklet.InformSurface{worklet.InformSlack}) {
		t.Errorf("dev can post on %v, want slack alone — Mattermost is switched "+
			"off for the company, so dev's bot there is started by nothing", got)
	}
	if got := directory.ChatSurfaces("ops"); len(got) != 0 {
		t.Errorf("ops holds no bot and can post on %v", got)
	}
	if got := directory.ChatSurfaces("nobody"); len(got) != 0 {
		t.Errorf("a handle the chart does not hold can post on %v", got)
	}
	if got := directory.UnitChannels(); !slices.Equal(got, []string{"engineering", "releases"}) {
		t.Errorf("the declared channels are %v, want engineering and releases once each", got)
	}
	if got := (liveChannels{engine: &Engine{}}).ChatSurfaces("dev"); got != nil {
		t.Errorf("a node with no company can post on %v", got)
	}
}

// THE ENGINE'S OWN CHAT IS A SURFACE EVERY AGENT SEAT CAN POST ON, with no bot
// to hold: on a company whose chat is Chatlet, a seat posts as itself, and the
// inform it promises is discharged by its own chat tools. It was left out, so
// every inform a Chatlet company's seat promised was refused as a surface
// it could not post on. A person has no turn to hold open, and a company whose
// chat is a vendor's has no Chatlet at all.
func TestTheEnginesOwnChatIsEveryAgentSeatsSurface(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
providers:
  llm:
    gateway:
      agent: codex
      model: gpt-4o
chat:
  backend: CHAT
units:
  - name: Engineering
    channel: engineering
    roles:
      - name: Dev
        handle: dev
      - name: Ana
        handle: ana
        kind: human
`
	chatlet := &Engine{}
	chatlet.epoch.current.Store(companyFor(t, strings.Replace(doc, "CHAT", "chatlet", 1)))
	directory := liveChannels{engine: chatlet}
	if got := directory.ChatSurfaces("dev"); !slices.Equal(got,
		[]worklet.InformSurface{worklet.InformChat}) {
		t.Errorf("an agent seat of a Chatlet company can post on %v, want "+
			"the engine's own chat", got)
	}
	if got := directory.ChatSurfaces("ana"); len(got) != 0 {
		t.Errorf("a person can promise an inform on %v — a person has no turn "+
			"to hold open until the post is made", got)
	}

	none := &Engine{}
	none.epoch.current.Store(companyFor(t, strings.Replace(doc, "CHAT", "none", 1)))
	if got := (liveChannels{engine: none}).ChatSurfaces("dev"); len(got) != 0 {
		t.Errorf("a company with no chat surface offers %v", got)
	}
}
