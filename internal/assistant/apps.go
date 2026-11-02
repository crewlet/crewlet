package assistant

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// WHAT A MESSAGE KNOWS IS WHAT ITS TURN READ. A turn's prose is written by a
// model that has read the answers of the turn's read steps (and the history
// it was given), so every message records the apps the turn had read through
// when it was written, as a card records the app it was built from. A reader
// who no longer holds one of them is shown a notice in the message's place,
// on a reload and in the history the next turn reads; and a parked loop, which
// keeps those answers verbatim, is never handed back to the model for a person
// who lost one ([Service.Carries]).

// withheldMessageText stands in for a stored message whose apps the reader no
// longer holds.
const withheldMessageText = "This answer is hidden because it used access you no longer have."

// holds reports whether p holds every one of apps, by the rule a card is shown
// by ([cards.Visible]).
func holds(p iam.Principal, apps []string) bool {
	_, ok := cards.Visible(apps, wire.Card{}, p)
	return ok
}

// shownMessage is a stored message as reader is shown it: its text, or a
// notice in its place where they no longer hold its apps (an empty message,
// which a card hangs on, says nothing and stays empty).
func shownMessage(m assistantdomain.Message, reader iam.Principal) assistantdomain.Message {
	if m.Text != "" && !holds(reader, m.Apps) {
		m.Text, m.Apps = withheldMessageText, nil
	}
	return m
}

// used notes apps the turn has read through.
func (t *turn) used(apps []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.apps == nil {
		t.apps = map[string]struct{}{}
	}
	for _, app := range apps {
		t.apps[app] = struct{}{}
	}
}

// appsNow is every app the turn has read through so far, sorted.
func (t *turn) appsNow() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Sorted(maps.Keys(t.apps))
}
