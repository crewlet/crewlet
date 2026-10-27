package assistant_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// itemPage is the screen of ENG-7, with ENG-7 open.
func itemPage() *wire.PageContext {
	return &wire.PageContext{Screen: "item", Title: "Tasks › ENG-7", Params: map[string]string{
		"id": "ENG-7"}, Query: map[string]string{}, Resources: []wire.Resource{},
		Selection:      &wire.ResourceRef{Kind: wire.ResourceTask, ID: "ENG-7", Noun: wire.NounTask},
		SourceRevision: "item@1", Accepts: []string{}}
}

// briefOf is an answer_brief call citing ENG-7 in a row of tone.
func briefOf(id string, tone wire.BriefTone) evals.Round {
	return evals.Call(id, catalog.AnswerBriefTool, map[string]any{
		"summary": "ENG-7, Rotate keys, waits on you.",
		"needs": []any{map[string]any{"ref_kind": "task", "ref_id": "ENG-7",
			"text": "ENG-7 waits on your review", "tone": string(tone), "action": "open"}},
		"follow_ups": []any{"Who is on ENG-7?"},
	})
}

// A BRIEF IS ASKED OF A PAGE: one with no page, or a mode the engine does not
// know, is refused before anything is recorded or spent.
//
// Mutation: drop the mode check and a brief with no page runs an ordinary turn.
func TestABriefNeedsThePageItBriefs(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)}, Handler: task})
	if _, err := f.Brief("no-page", "Brief me on this page", nil); !errors.Is(err,
		assistant.ErrInvalid) {
		t.Errorf("a brief with no page answered %v, want ErrInvalid", err)
	}
	_, err := f.Svc.Start(t.Context(), assistant.TurnStart{Person: f.Person,
		Conversation: f.Conversation, Key: evals.Key("bad-mode"), Input: "Brief me",
		Page: itemPage(), Read: f.Grant("bad-mode"), Mode: "summary"})
	if !errors.Is(err, assistant.ErrInvalid) {
		t.Errorf("an unknown mode answered %v, want ErrInvalid", err)
	}
	if sent := f.Dispatcher.Sent(); len(sent) != 0 {
		t.Errorf("a refused brief sent %+v", sent)
	}
}

// A BRIEF'S ANSWER IS ONE BRIEF CARD: its reads draw no card, its prose is
// never drawn, its row opens the engine's own address for the record it read,
// and a danger the record's read does not carry is drawn warning.
//
// Mutation: emit read cards in brief mode, or trust the model's danger, and
// this fails.
func TestABriefAnswersWithOneBriefCard(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)},
		Handler: task, Rounds: []evals.Round{readTask(), evals.Say("Let me write it."),
			briefOf("c2", wire.BriefDanger)}})
	accepted, err := f.Brief("brief", "Brief me on this task", itemPage())
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	var shown []wire.Card
	for _, fr := range frames {
		switch fr.Kind {
		case wire.EventCard:
			shown = append(shown, fr.Card.Card)
		case wire.EventDelta:
			if fr.Delta.Text != "" {
				t.Errorf("a brief drew prose %q", fr.Delta.Text)
			}
		}
	}
	if len(shown) != 1 || shown[0].Type != wire.CardBrief {
		t.Fatalf("cards = %+v, want one brief card", shown)
	}
	b := shown[0].Brief
	if b.Scope.Record == nil || b.Scope.Record.ID != "ENG-7" {
		t.Errorf("scope = %+v, want the open ENG-7", b.Scope)
	}
	if len(b.Needs) != 1 {
		t.Fatalf("needs = %+v, want the one row", b.Needs)
	}
	need := b.Needs[0]
	if need.Tone != wire.BriefWarning {
		t.Errorf("tone = %s, want warning: the task's read carries no urgency", need.Tone)
	}
	if need.Action == nil || need.Action.Link == nil || need.Action.Link.Href != "#/worklet/ENG-7" {
		t.Errorf("action = %+v, want the link table's address of ENG-7", need.Action)
	}
	if done := evals.Last(frames, wire.EventDone); done == nil || done.Done.State != wire.TurnCompleted {
		t.Errorf("done = %+v, want completed", done)
	}
}

// A BRIEF THE ENGINE CANNOT ACCEPT IS WRITTEN AGAIN ONCE, and a second refusal
// ends the turn brief_invalid, with nothing drawn.
//
// Mutation: allow more answers, or end on the first, and this fails.
func TestABriefIsWrittenAgainOnceThenFails(t *testing.T) {
	t.Parallel()
	bad := func(id string) evals.Round {
		return evals.Call(id, catalog.AnswerBriefTool, map[string]any{"summary": "",
			"needs": []any{}, "follow_ups": []any{}})
	}
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)},
		Handler: task, Rounds: []evals.Round{bad("c1"), bad("c2"), evals.Say("Sorry.")}})
	accepted, err := f.Brief("bad", "Brief me on this task", itemPage())
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	if card := evals.Last(frames, wire.EventCard); card != nil {
		t.Errorf("a refused brief drew %+v", card.Card.Card)
	}
	failure := evals.Last(frames, wire.EventError)
	if failure == nil || failure.Error.Code != wire.ErrorBriefInvalid {
		t.Errorf("error = %+v, want brief_invalid", failure)
	}
}

// A ROW ABOUT A RECORD THE TURN NEVER READ IS DROPPED, never drawn, and the
// brief still stands without it.
//
// Mutation: skip the citation check and ENG-9 is drawn.
func TestABriefRowAboutAnUnreadRecordIsDropped(t *testing.T) {
	t.Parallel()
	unread := evals.Call("c1", catalog.AnswerBriefTool, map[string]any{
		"summary": "Nothing waits on you.",
		"needs": []any{map[string]any{"ref_kind": "task", "ref_id": "ENG-9",
			"text": "ENG-9 is overdue", "tone": "warning", "action": "open"}},
		"follow_ups": []any{},
	})
	handler := assistanttest.Routes(map[string]exec.Response{
		"GET /worklet/ENG-9": assistanttest.Task("ENG-9", "Ghost", "Never read."),
	})
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)},
		Handler: handler, Rounds: []evals.Round{unread}})
	accepted, err := f.Brief("unread", "Brief me on this page", itemPage())
	if err != nil {
		t.Fatal(err)
	}
	card := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventCard)
	if card == nil || card.Card.Card.Brief == nil {
		t.Fatal("no brief card was drawn")
	}
	if needs := card.Card.Card.Brief.Needs; len(needs) != 0 {
		t.Errorf("needs = %+v, want the unread ENG-9 dropped", needs)
	}
	if sent := f.Dispatcher.Sent(); len(sent) != 0 {
		t.Errorf("an uncited record was read: %+v", sent)
	}
}
