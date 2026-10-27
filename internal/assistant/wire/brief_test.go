package wire_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// validBrief is a brief every rule accepts.
func validBrief() wire.BriefCard {
	return wire.BriefCard{
		Scope:   wire.BriefScope{Screen: "inbox", Title: "Inbox"},
		Summary: "1 decision waits on you.",
		Needs: []wire.BriefNeed{{Ref: wire.ResourceRef{Kind: wire.ResourceTask, ID: "ENG-1"},
			Text: "QA asks", Tone: wire.BriefWarning, Action: &wire.BriefAction{
				Kind: wire.BriefOpen, Label: "Open ENG-1", Link: &wire.Link{Label: "ENG-1",
					Screen: "item", Href: "#/worklet/ENG-1"}}}},
		Recommended: &wire.BriefStep{Text: "Answer it.", Action: &wire.BriefAction{
			Kind: wire.BriefAsk, Label: "Ask", Question: "What did QA test?"}},
		FollowUps: []string{"What shipped?"},
		AsOf:      time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC),
	}
}

// A BRIEF IS HELD TO ITS SHAPE ON THE WIRE: its bounds, its closed sets, and an
// action that carries exactly what its kind needs, so an open is never a link
// outside the dashboard and an ask never carries one. Mutation: drop any rule
// and its case passes as valid.
func TestABriefIsHeldToItsShape(t *testing.T) {
	t.Parallel()
	ok := validBrief()
	if err := (wire.Card{Type: wire.CardBrief, Brief: &ok}).Validate(); err != nil {
		t.Fatalf("a valid brief was refused: %v", err)
	}
	cases := map[string]func(b *wire.BriefCard){
		"no summary":      func(b *wire.BriefCard) { b.Summary = "" },
		"a long summary":  func(b *wire.BriefCard) { b.Summary = strings.Repeat("a", wire.BriefSummaryMaxRunes+1) },
		"two lines":       func(b *wire.BriefCard) { b.Needs[0].Text = "one\ntwo" },
		"four needs":      func(b *wire.BriefCard) { b.Needs = append(b.Needs, b.Needs[0], b.Needs[0], b.Needs[0]) },
		"an unknown tone": func(b *wire.BriefCard) { b.Needs[0].Tone = "loud" },
		"an outside link": func(b *wire.BriefCard) { b.Needs[0].Action.Link.Href = "https://example.com" },
		"an open asking":  func(b *wire.BriefCard) { b.Needs[0].Action.Question = "Why?" },
		"an ask linking":  func(b *wire.BriefCard) { b.Recommended.Action.Link = &wire.Link{Href: "#/home", Screen: "home"} },
		"an empty ask":    func(b *wire.BriefCard) { b.Recommended.Action.Question = "" },
		"a bad ref":       func(b *wire.BriefCard) { b.Needs[0].Ref.Kind = "decision" },
		"no as_of":        func(b *wire.BriefCard) { b.AsOf = time.Time{} },
		"four follow-ups": func(b *wire.BriefCard) { b.FollowUps = []string{"a", "b", "c", "d"} },
		"a bad scope":     func(b *wire.BriefCard) { b.Scope.Screen = "Not A Screen" },
		"an unknown noun": func(b *wire.BriefCard) {
			b.Scope.Record = &wire.ResourceRef{Kind: wire.ResourceTask, ID: "ENG-1", Noun: "thing"}
		},
	}
	for name, mutate := range cases {
		b := validBrief()
		b.Needs = append([]wire.BriefNeed(nil), b.Needs...)
		action := *b.Needs[0].Action
		link := *action.Link
		action.Link = &link
		b.Needs[0].Action = &action
		step := *b.Recommended
		stepAction := *step.Action
		step.Action = &stepAction
		b.Recommended = &step
		mutate(&b)
		if err := b.Validate(); !errors.Is(err, wire.ErrInvalid) {
			t.Errorf("%s: answered %v, want ErrInvalid", name, err)
		}
	}
}

// A TURN'S MODE IS A CLOSED SET, and a selection's noun is one too.
func TestTheBriefsClosedSets(t *testing.T) {
	t.Parallel()
	if !wire.TurnBrief.Valid() || !wire.TurnAsk.Valid() || wire.TurnMode("summary").Valid() {
		t.Error("the turn modes are not ask and brief")
	}
	if !wire.NounDecision.Valid() || wire.RecordNoun("thing").Valid() {
		t.Error("the record nouns are not a closed set")
	}
}
