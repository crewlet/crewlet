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
		Scope: wire.BriefScope{Screen: "inbox", Title: "Inbox"},
		About: "QA asks which build to test on ENG-1.",
		Needs: []wire.BriefNeed{{Ref: wire.ResourceRef{Kind: wire.ResourceTask, ID: "ENG-1"},
			Text: "Answer ENG-1: which build QA tests", Tone: wire.BriefWarning,
			Action: &wire.BriefAction{Kind: wire.BriefOpen, Label: "Open ENG-1",
				Link: &wire.Link{Label: "ENG-1", Screen: "item", Href: "#/worklet/ENG-1"}}},
			{Ref: wire.ResourceRef{Kind: wire.ResourceSeat, ID: "qa"},
				Text: "Ask QA what it tested last", Tone: wire.BriefWarning,
				Action: &wire.BriefAction{Kind: wire.BriefAsk, Label: "Ask",
					Question: "What did QA test?"}}},
		Context:   "The beta ships Thursday.",
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
		"no about":         func(b *wire.BriefCard) { b.About = "" },
		"a long about":     func(b *wire.BriefCard) { b.About = strings.Repeat("a", wire.BriefAboutMaxRunes+1) },
		"a long do":        func(b *wire.BriefCard) { b.Needs[0].Text = strings.Repeat("a", wire.BriefTextMaxRunes+1) },
		"a long why":       func(b *wire.BriefCard) { b.Needs[0].Detail = strings.Repeat("a", wire.BriefDetailMaxRunes+1) },
		"a long context":   func(b *wire.BriefCard) { b.Context = strings.Repeat("a", wire.BriefContextMaxRunes+1) },
		"a long follow-up": func(b *wire.BriefCard) { b.FollowUps[0] = strings.Repeat("a", wire.BriefQuestionMaxRunes+1) },
		"two lines":        func(b *wire.BriefCard) { b.Needs[0].Text = "one\ntwo" },
		"four needs":       func(b *wire.BriefCard) { b.Needs = append(b.Needs, b.Needs[0], b.Needs[0]) },
		"a neutral need":   func(b *wire.BriefCard) { b.Needs[0].Tone = "neutral" },
		"no action":        func(b *wire.BriefCard) { b.Needs[0].Action = nil },
		"an outside link":  func(b *wire.BriefCard) { b.Needs[0].Action.Link.Href = "https://example.com" },
		"an open asking":   func(b *wire.BriefCard) { b.Needs[0].Action.Question = "Why?" },
		"an ask linking":   func(b *wire.BriefCard) { b.Needs[1].Action.Link = &wire.Link{Href: "#/home", Screen: "home"} },
		"an empty ask":     func(b *wire.BriefCard) { b.Needs[1].Action.Question = "" },
		"a bad ref":        func(b *wire.BriefCard) { b.Needs[0].Ref.Kind = "decision" },
		"no as_of":         func(b *wire.BriefCard) { b.AsOf = time.Time{} },
		"three follow-ups": func(b *wire.BriefCard) { b.FollowUps = []string{"a", "b", "c"} },
		"a bad scope":      func(b *wire.BriefCard) { b.Scope.Screen = "Not A Screen" },
		"an unknown noun": func(b *wire.BriefCard) {
			b.Scope.Record = &wire.ResourceRef{Kind: wire.ResourceTask, ID: "ENG-1", Noun: "thing"}
		},
	}
	for name, mutate := range cases {
		b := validBrief()
		b.Needs = append([]wire.BriefNeed(nil), b.Needs...)
		b.FollowUps = append([]string(nil), b.FollowUps...)
		for i := range b.Needs {
			action := *b.Needs[i].Action
			if action.Link != nil {
				link := *action.Link
				action.Link = &link
			}
			b.Needs[i].Action = &action
		}
		mutate(&b)
		if err := b.Validate(); !errors.Is(err, wire.ErrInvalid) {
			t.Errorf("%s: answered %v, want ErrInvalid", name, err)
		}
	}
}

// THE VERDICT IS THE ROWS, COUNTED: there is no field a writer could set to
// say "nothing needs you" beside a row that needs them, and the count is the
// rows the card carries at every length it may. Mutation: return a fixed
// string, or count anything but the rows, and a length fails.
func TestTheVerdictIsTheNumberOfRows(t *testing.T) {
	t.Parallel()
	want := []string{"Nothing needs you", "1 thing needs you", "2 things need you",
		"3 things need you"}
	b := validBrief()
	need := b.Needs[0]
	for n := 0; n <= wire.BriefMaxNeeds; n++ {
		b.Needs = make([]wire.BriefNeed, n)
		for i := range b.Needs {
			b.Needs[i] = need
		}
		if got := b.Verdict(); got != want[n] {
			t.Errorf("%d rows: the verdict is %q, want %q", n, got, want[n])
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
