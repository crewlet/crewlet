package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
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
      type: openai
      model: gpt-4o
      api_keys: ["${OPENAI_API_KEY}"]
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
	if got := directory.ChatSurfaces("dev"); !slices.Equal(got, []tracker.InformSurface{tracker.InformSlack}) {
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
