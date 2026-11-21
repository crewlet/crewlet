package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/provenance"
)

// wakesAgainHeader is prompts' header of the wakes a session is told again
// (prompts.WakesReadAgain).
const wakesAgainHeader = "## Messages about this work that the session above may not show"

// A CONTINUATION IS TOLD AGAIN EVERY WAKE ITS SESSION MAY LACK, AND A FRESH
// BRIEF EVERY WAKE THE TURN READ. A wake its job read is the turn's, its
// delivery worked, so no session of the turn may lose it: a continued session
// is told one read past its capture's cursor, and one its job was served and
// never shown reading, as messages it may already show, and never one its
// capture holds; a fresh session is told every one read, and never one only
// served, which runs as a turn of its own.
//
// Mutation: tell a continuation only the wakes read past its cursor
// (served-wake-untold), or tell a fresh brief a served one, and this goes red.
func TestAContinuationIsToldEveryWakeItsSessionMayLack(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	wake := func(id, text string, place int, read bool) runner.HeldWake {
		return runner.HeldWake{Wake: steer.Wake{ID: id, By: "bob", Conversation: "chat:C1",
			Label: provenance.Label{Source: provenance.SourceChatThread}, Text: text}, Place: place, Read: read}
	}
	held := wake("held", "the message its session read at its first batch", 1, true)
	past := wake("past", "the message its job read after the session was saved", 3, true)
	served := wake("served", "the message its job was served and never shown reading", 2, false)
	basis := keptBasis
	basis.NoteCursor = 1
	relauncher := &probingLauncher{basis: basis}
	resume := &runner.Resume{State: parked.State, Wakes: []runner.HeldWake{held, past, served},
		Asked: &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
			SessionID: "s-1", BoxKept: true},
		Bridged: []runner.BridgedCall{numbered("jira_create", map[string]any{"summary": "the job's issue"}, 1, 1)}}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		steer: steer.New(), resume: resume})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if again.Session == nil {
		t.Fatal("the relaunch continues no kept session")
	}
	brief := again.Session.Brief
	header := strings.Index(brief, wakesAgainHeader)
	if header < 0 || !strings.Contains(brief[header:], "the 2 messages below") {
		t.Fatalf("the wakes its session may lack are not headed and counted:\n%s", brief)
	}
	for _, w := range []runner.HeldWake{past, served} {
		if i := strings.Index(brief, w.Text); i < header || strings.Count(brief, w.Text) != 1 {
			t.Errorf("the session is not told %q once, under the header:\n%s", w.ID, brief)
		}
	}
	if strings.Contains(brief, held.Text) {
		t.Errorf("the session is told the wake its capture holds:\n%s", brief)
	}
	for _, w := range []runner.HeldWake{held, past} {
		if !strings.Contains(again.Brief, w.Text) {
			t.Errorf("the fresh brief does not tell the wake %q the turn read", w.ID)
		}
	}
	if strings.Contains(again.Brief, served.Text) || strings.Contains(again.Brief, wakesAgainHeader) {
		t.Errorf("the fresh brief tells the served wake, or tells wakes as ones it may already show:\n%s",
			again.Brief)
	}
}
