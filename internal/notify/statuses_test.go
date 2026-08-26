package notify_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/notify"
)

// driverFor builds one backend's driver over the shared poster fixture.
//
// The refresh is deliberately long: these cases are about which driver
// CLAIMS a trigger, and a heartbeat firing mid-assertion would add entries
// nobody is testing for.
func driverFor(backend string) (*notify.StatusDriver, *poster) {
	p := newPoster()
	p.backend = backend
	p.refresh = time.Hour
	return notify.NewStatusDriver(notify.StatusOptions{
		Poster: p, Mode: notify.StatusAlways,
	}), p
}

// A TURN IS TRIGGERED BY EXACTLY ONE BACKEND, and the indicator has to go up
// on that one.
//
// A caller holding a single driver would raise it on the wrong backend or,
// far more likely, on none: a driver refuses a trigger whose transport is
// not its own, so the second chat surface a company runs would go silently
// unindicated for ever.
func TestTheIndicatorGoesUpOnTheBackendThatTriggeredTheTurn(t *testing.T) {
	t.Parallel()
	first, firstPoster := driverFor("mattermost")
	second, secondPoster := driverFor("slack")
	set := notify.NewStatuses(first, second)

	session := set.Begin(context.Background(), "swe", "turn-1", "plan", map[string]string{
		"transport": "slack", "channel": "C0ENG", "ts": "1700000001.000100",
	})
	if session == nil {
		t.Fatal("a Slack trigger raised no indicator")
	}
	defer session.End(context.Background(), true)

	if len(secondPoster.shown()) == 0 {
		t.Error("the Slack indicator was not raised")
	}
	if len(firstPoster.shown()) != 0 {
		t.Errorf("a Slack trigger raised the Mattermost indicator: %v", firstPoster.shown())
	}
}

// A TRIGGER FROM SOMEWHERE ELSE RAISES NOTHING, and says so by answering a
// nil session whose methods are no-ops.
func TestATriggerFromNoChatBackendRaisesNothing(t *testing.T) {
	t.Parallel()
	set := notify.NewStatuses(func() *notify.StatusDriver { d, _ := driverFor("slack"); return d }())

	session := set.Begin(context.Background(), "swe", "turn-1", "plan", map[string]string{
		"transport": "jira", "issue_key": "ENG-42",
	})
	if session != nil {
		t.Fatal("a tracker event raised a chat indicator")
	}
	// Every method on that nil session is a no-op.
	session.Phase(context.Background(), "execute")
	session.End(context.Background(), false)
}

// AN EMPTY SET IS A COMPANY WITH NO CHAT BACKEND, and it answers the same
// way — so the turn engine never has to ask whether indicators exist.
func TestAnEmptySetIsSafeToCallThrough(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]*notify.Statuses{
		"nil":     nil,
		"empty":   notify.NewStatuses(),
		"nil arg": notify.NewStatuses(nil),
	} {
		if got := set.Begin(context.Background(), "swe", "t", "plan", map[string]string{
			"transport": "slack", "channel": "C", "ts": "1",
		}); got != nil {
			t.Errorf("%s set raised an indicator", name)
		}
		if got := set.Backends(); len(got) != 0 {
			t.Errorf("%s set names backends %v", name, got)
		}
		set.Stop(context.Background())
	}
}

// STOPPING THE SET STOPS EVERY BACKEND'S LIVE INDICATOR — a seat that
// vanished mid-turn would otherwise look like it was still thinking until
// the backend expired the status on its own.
func TestStoppingTheSetClearsEveryBackend(t *testing.T) {
	t.Parallel()
	first, firstPoster := driverFor("mattermost")
	second, secondPoster := driverFor("slack")
	set := notify.NewStatuses(first, second)

	for _, backend := range []string{"mattermost", "slack"} {
		if s := set.Begin(context.Background(), "swe", "turn-"+backend, "plan",
			map[string]string{"transport": backend, "channel": "C", "ts": "1"}); s == nil {
			t.Fatalf("%s raised no indicator", backend)
		}
	}
	set.Stop(context.Background())
	if firstPoster.clears() == 0 || secondPoster.clears() == 0 {
		t.Errorf("stopping the set left an indicator up: %d / %d",
			firstPoster.clears(), secondPoster.clears())
	}
}

// THE SET NAMES ITS BACKENDS, sorted, for the operator surface that shows
// what a company is wired to.
func TestTheSetNamesItsBackends(t *testing.T) {
	t.Parallel()
	slackDriver, _ := driverFor("slack")
	mmDriver, _ := driverFor("mattermost")
	got := notify.NewStatuses(slackDriver, mmDriver).Backends()
	if len(got) != 2 || got[0] != "mattermost" || got[1] != "slack" {
		t.Fatalf("backends = %v", got)
	}
}
