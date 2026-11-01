package wire_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// framesGolden is the vocabulary stream: every event kind and every card kind
// the protocol carries, written by this package's encoder and replayed by the
// dashboard's decoder (`dashboard/src/protocol/assistantWire.test.ts`).
const framesGolden = "testdata/frames.sse"

// vocabularyFrames is the golden's content: two turns of one conversation, the
// first reading, proposing and parking, the second resumed from a snapshot and
// interrupted, so every kind appears where it would really appear.
func vocabularyFrames() []wire.Frame {
	id := evals.ID
	answer := id("message:answer")
	proposal := id("proposal:create-task")
	page := &wire.PageChip{Title: "Worklet › Tasks", Screen: "worklet-tasks"}
	read := assistanttest.NewTurn("conversation", "turn:read")
	at := evals.Epoch
	frames := []wire.Frame{
		read.Frame(wire.Frame{Kind: wire.EventConversation, Conversation: &wire.ConversationEvent{
			Title: "Flaky sign-in test", CreatedAt: at}}),
		read.Frame(wire.Frame{Kind: wire.EventPageContext, PageContext: &wire.PageContextEvent{
			Used: true, Page: page}}),
		read.Frame(wire.Frame{Kind: wire.EventStatus, Status: &wire.StatusEvent{
			State: wire.StatusThinking}}),
		read.Frame(wire.Frame{Kind: wire.EventStep, Step: &wire.StepEvent{
			StepID: id("step:search"), Index: 1, Tool: "search_work",
			Label:   "Search tasks for “sign-in test”",
			Request: &wire.StepRequest{Method: "GET", Route: "/worklet/search"},
			State:   wire.StepRunning, StartedAt: at}}),
		read.Frame(wire.Frame{Kind: wire.EventStep, Step: &wire.StepEvent{
			StepID: id("step:search"), Index: 1, Tool: "search_work",
			Label:   "Search tasks for “sign-in test”",
			Request: &wire.StepRequest{Method: "GET", Route: "/worklet/search"},
			State:   wire.StepDone, StartedAt: at, DurationMS: 812, Summary: "2 tasks"}}),
		read.Frame(wire.Frame{Kind: wire.EventStatus, Status: &wire.StatusEvent{
			State: wire.StatusWorking, StepsDone: 1}}),
		read.Frame(wire.Frame{Kind: wire.EventDelta, Delta: &wire.DeltaEvent{
			MessageID: answer, Text: "No open task covers it yet."}}),
		read.Frame(cardFrame(answer, "card:table", wire.Card{Type: wire.CardTable,
			Table: &wire.TableCard{Title: "Matching tasks", Columns: []wire.Column{
				{Key: "key", Label: "Task", Type: wire.ColumnText},
				{Key: "spend", Label: "Tokens", Type: wire.ColumnTokens, Align: wire.AlignEnd},
			}, Rows: [][]any{{"ENG-133", 412000}, {"ENG-90", nil}}, Total: []any{"Total", 412000},
				RowCount: 2, AsOf: at, Exports: &wire.Exports{
					CSV:  "/assistant/cards/" + id("card:table").String() + "/export.csv",
					XLSX: "/assistant/cards/" + id("card:table").String() + "/export.xlsx"}}})),
		read.Frame(cardFrame(answer, "card:entity", wire.Card{Type: wire.CardEntity,
			Entity: &wire.EntityCard{Kind: wire.ResourceUnit, ID: "core", Title: "Core",
				Facts: []wire.Fact{{Label: "Lead", Value: "Agent CTO"}},
				Items: []wire.EntityItem{{Kind: wire.ResourceSeat, ID: "swe", Label: "Agent SWE",
					Fact: "Working", Depth: 1}}}})),
		read.Frame(cardFrame(answer, "card:open", wire.Card{Type: wire.CardOpenInApp,
			OpenInApp: &wire.OpenInAppCard{Link: wire.Link{Label: "ENG-133", Screen: "item",
				Href: "#/worklet/ENG-133"}, Permitted: true}})),
		read.Frame(cardFrame(answer, "card:notice", wire.Card{Type: wire.CardNotice,
			Notice: &wire.NoticeCard{Tone: wire.NoticeInfo,
				Text: "Showing the first 50 of 210 rows."}})),
		read.Frame(cardFrame(answer, "card:oversize", wire.Card{Type: wire.CardOversize,
			Oversize: &wire.OversizeCard{Title: "Matching tasks, \"brand identity\"",
				Open: &wire.Link{Label: "Search", Screen: "worklet-search",
					Href: "#/worklet/search"},
				Narrow: []string{"the search words"}}})),
		read.Frame(cardFrame(answer, "card:error", wire.Card{Type: wire.CardError,
			Error: &wire.ErrorCard{Refusal: wire.Refusal{Code: "unauthorized",
				Message: "You do not have access to this.", Reason: "no_grant",
				Grants: []string{"admin"}}, Phrase: "read the fleet"}})),
		// --- deep work (im-deep) ---
		read.Frame(wire.Frame{Kind: wire.EventStep, Step: &wire.StepEvent{
			StepID: id("step:deep"), Index: 2, Tool: "start_deep_work",
			Label: "Deep work: redesign the brand assets", State: wire.StepRunning, StartedAt: at,
			Deep: &wire.DeepProgress{State: wire.DeepRunning, Goal: "Redesign the brand assets",
				StartedAt: at, DeadlineAt: at.Add(20 * time.Minute), Steps: 1, MaxSteps: 150,
				BudgetPercent: 12, Activity: []string{"Reading the brand draft",
					"Ran a command"}}}}),
		read.Frame(wire.Frame{Kind: wire.EventStep, Step: &wire.StepEvent{
			StepID: id("step:deep-read"), Index: 3, Tool: "api_request",
			Label:   "Read GET /brand",
			Request: &wire.StepRequest{Method: "GET", Route: "/brand"},
			State:   wire.StepDone, StartedAt: at, DurationMS: 140, Summary: "1 record", Parent: 2}}),
		read.Frame(wire.Frame{Kind: wire.EventStep, Step: &wire.StepEvent{
			StepID: id("step:deep"), Index: 2, Tool: "start_deep_work",
			Label: "Deep work: redesign the brand assets", State: wire.StepDone, StartedAt: at,
			DurationMS: 412000, Summary: "2 files",
			Deep: &wire.DeepProgress{State: wire.DeepFinished, Goal: "Redesign the brand assets",
				StartedAt: at, DeadlineAt: at.Add(20 * time.Minute), Steps: 1, MaxSteps: 150,
				BudgetPercent: 40}}}),
		read.Frame(cardFrame(answer, "card:file", wire.Card{Type: wire.CardFile,
			File: &wire.FileCard{Files: []wire.FileRef{
				{FileID: id("file:logo"), Name: "logo.svg", MediaType: "image/svg+xml", Size: 4096,
					Download: wire.FilePath(id("conversation"), id("file:logo"))},
				{FileID: id("file:palette"), Name: "palette.csv", MediaType: "text/csv", Size: 210,
					Download: wire.FilePath(id("conversation"), id("file:palette"))},
			}}})),
		read.Frame(wire.Frame{Kind: wire.EventStep, Step: &wire.StepEvent{
			StepID: id("step:create"), Index: 4, Tool: "create_task",
			Label: "Prepare a new task", State: wire.StepWaiting, StartedAt: at}}),
		read.Frame(cardFrame(answer, "card:proposal", wire.Card{Type: wire.CardProposal,
			Proposal: &wire.ProposalCard{ProposalID: proposal, Scope: wire.ScopeServer,
				Tool: "create_task", Title: "Create task", Target: "Core",
				Changes: []wire.Change{
					{Kind: wire.ChangeFact, Field: "scope", Label: "Scope", To: "Saved for the company"},
					{Kind: wire.ChangeAdded, Field: "title", Label: "Title", To: "Fix the flaky sign-in test"},
					{Kind: wire.ChangeChanged, Field: "priority", Label: "Priority", From: "Normal", To: "High"},
					{Kind: wire.ChangeRemoved, Field: "label", Label: "Label", From: "triage"},
					{Kind: wire.ChangeText, Field: "body", Label: "Description",
						From: "Fails sometimes.", To: "Fails on every third run.",
						Words: []wire.Word{{Op: wire.WordSame, Text: "Fails "},
							{Op: wire.WordRemoved, Text: "sometimes."},
							{Op: wire.WordAdded, Text: "on every third run."}}},
				},
				Audience: []string{"Core"}, Taint: "Read ENG-90, written by Agent PM",
				Digest:    wire.DigestPrefix + "9f2c0d6e5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0",
				ExpiresAt: at.Add(15 * time.Minute), State: wire.ProposalProposed,
				SourceRevision: "url:3f9c1a0e", ApproveLabel: "Approve and save"}})),
		read.Frame(cardFrame(answer, "card:page", wire.Card{Type: wire.CardProposal,
			Proposal: &wire.ProposalCard{ProposalID: id("proposal:page"), Scope: wire.ScopePage,
				Tool: "propose_page_change", Title: "Filter the list", Target: "Worklet › Tasks",
				Changes: []wire.Change{
					{Kind: wire.ChangeChanged, Field: "blocked", Label: "Blocked", From: "Any", To: "Blocked only"},
				},
				Digest:    wire.DigestPrefix + "0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9",
				ExpiresAt: at.Add(15 * time.Minute), State: wire.ProposalProposed,
				SourceRevision: "url:3f9c1a0e",
				PageAction: &wire.PageActionRef{Name: "work.filter.apply",
					Payload: json.RawMessage(`{"blocked":"true"}`)},
				ApproveLabel: "Apply to this list"}})),
		read.Frame(wire.Frame{Kind: wire.EventAction, Action: &wire.ActionEvent{
			ActionID: id("action:filter"), Name: "work.filter.apply", ProposalID: &proposal,
			SourceRevision: "url:3f9c1a0e", Payload: json.RawMessage(`{"blocked":"true"}`),
			Confirm: wire.ConfirmApprove}}),
		read.Frame(wire.Frame{Kind: wire.EventAction, Action: &wire.ActionEvent{
			ActionID: id("action:invalidate"), Name: wire.ActionContextInvalidate,
			Invalidate: &wire.Invalidate{Domain: "tracker", Position: "CREWLET_TRACKER_LOG:1:4812"}}}),
		read.Frame(wire.Frame{Kind: wire.EventStatus, Status: &wire.StatusEvent{
			State: wire.StatusWaitingForApproval, StepsDone: 4}}),
		read.Frame(wire.Frame{Kind: wire.EventDone, Done: &wire.DoneEvent{
			State: wire.TurnWaitingForApproval, DurationMS: 5120, Steps: 4, MessageID: &answer,
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736"}}),
	}
	resumed := assistanttest.NewTurn("conversation", "turn:resumed")
	frames = append(frames,
		resumed.Frame(wire.Frame{Kind: wire.EventConversation, Conversation: &wire.ConversationEvent{
			Title: "Flaky sign-in test", CreatedAt: at, Resumed: true}}),
		resumed.Frame(wire.Frame{Kind: wire.EventSnapshot, Snapshot: &wire.SnapshotEvent{
			Messages: []wire.Message{{ID: id("message:question"), TurnID: resumed.ID,
				Role: wire.RoleUser, Text: "What is running right now?", CreatedAt: at, Page: page}},
			Steps: []wire.StepEvent{{StepID: id("step:running"), TurnID: resumed.ID, Index: 1,
				Tool: "list_agents", Label: "Read what the agents are doing",
				Request: &wire.StepRequest{Method: "GET", Route: "/agents"},
				State:   wire.StepDone, StartedAt: at, DurationMS: 240, Summary: "6 agents"}},
			Cards: []wire.CardEvent{}}}),
		resumed.Frame(wire.Frame{Kind: wire.EventError, Error: &wire.ErrorEvent{
			Code: wire.ErrorTurnInterrupted, Message: "The node running this answer stopped.",
			Hint: "Ask again.", Retryable: true}}),
		resumed.Frame(wire.Frame{Kind: wire.EventDone, Done: &wire.DoneEvent{
			State: wire.TurnInterrupted, DurationMS: 0, Steps: 0}}),
	)
	// A BRIEF TURN of the decision open in the Inbox: its one read, its answer
	// step and the one brief card it answers with, its prose never drawn.
	brief := assistanttest.NewTurn("conversation:brief", "turn:brief")
	briefAnswer := id("message:brief")
	inbox := &wire.PageChip{Title: "Inbox", Screen: "inbox"}
	return append(frames,
		brief.Frame(wire.Frame{Kind: wire.EventConversation, Conversation: &wire.ConversationEvent{
			Title: "Brief: Inbox", CreatedAt: at}}),
		brief.Frame(wire.Frame{Kind: wire.EventPageContext, PageContext: &wire.PageContextEvent{
			Used: true, Page: inbox}}),
		brief.Frame(wire.Frame{Kind: wire.EventStep, Step: &wire.StepEvent{
			StepID: id("step:brief"), Index: 1, Tool: "answer_brief", Label: "Writing the brief",
			State: wire.StepDone, StartedAt: at, DurationMS: 40, Summary: "Written"}}),
		brief.Frame(wire.Frame{Kind: wire.EventDelta, Delta: &wire.DeltaEvent{
			MessageID: briefAnswer}}),
		brief.Frame(cardFrame(briefAnswer, "card:brief", wire.Card{Type: wire.CardBrief,
			Brief: &wire.BriefCard{
				Scope: wire.BriefScope{Screen: "inbox", Title: "Inbox",
					Record: &wire.ResourceRef{Kind: wire.ResourceTask, ID: "ENG-133",
						Noun: wire.NounDecision}},
				About: "QA asks whether to test the store build or the nightly on ENG-133.",
				Needs: []wire.BriefNeed{{Ref: wire.ResourceRef{Kind: wire.ResourceTask,
					ID: "ENG-133"}, Text: "Answer ENG-133: test the store build",
					Detail: "Asked 2 hours ago", Tone: wire.BriefWarning,
					Action: &wire.BriefAction{Kind: wire.BriefOpen, Label: "Open ENG-133",
						Link: &wire.Link{Label: "ENG-133", Screen: "item",
							Href: "#/worklet/ENG-133"}}}},
				Context:   "The release is Thursday.",
				FollowUps: []string{"What did QA test last?"},
				AsOf:      at}})),
		brief.Frame(wire.Frame{Kind: wire.EventDone, Done: &wire.DoneEvent{
			State: wire.TurnCompleted, DurationMS: 2100, Steps: 1, MessageID: &briefAnswer}}),
	)
}

func cardFrame(message uuid.UUID, name string, card wire.Card) wire.Frame {
	return wire.Frame{Kind: wire.EventCard, Card: &wire.CardEvent{
		CardID: evals.ID(name), MessageID: message, Card: card}}
}

// THE VOCABULARY STREAM IS THE ENCODER'S OWN, AND IT CARRIES EVERY KIND.
//
// The dashboard's decoder replays this file, so it has to be what Encode
// writes, byte for byte, and it has to exercise every event kind and every
// card kind: a kind missing from it is a kind the client's decoder was never
// handed. Mutation: drop the snapshot frame and the coverage check fails;
// change any payload's tag and the golden comparison fails.
func TestTheVocabularyFixtureIsTheEncodersOwn(t *testing.T) {
	t.Parallel()
	frames := vocabularyFrames()
	events := map[wire.EventKind]bool{}
	cards := map[wire.CardKind]bool{}
	for _, f := range frames {
		events[f.Kind] = true
		if f.Card != nil {
			cards[f.Card.Card.Type] = true
		}
	}
	for _, k := range wire.EventKinds {
		if !events[k] {
			t.Errorf("the vocabulary stream carries no %s frame", k)
		}
	}
	for _, k := range wire.CardKinds {
		if !cards[k] {
			t.Errorf("the vocabulary stream carries no %s card", k)
		}
	}
	assistanttest.Golden(t, framesGolden, assistanttest.Stream(t, frames...))
}

// A STREAM DECODES TO THE FRAMES THAT WERE ENCODED, comments and all skipped.
//
// The orchestrator's tests read what a route streamed with this decoder, so it
// must hand back exactly what Encode wrote. Mutation: forget to skip a comment
// line and the keepalive between frames is refused.
func TestAStreamDecodesToTheFramesEncoded(t *testing.T) {
	t.Parallel()
	frames := vocabularyFrames()
	var b bytes.Buffer
	for i, f := range frames {
		if err := wire.Encode(&b, f); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			b.WriteString(wire.KeepaliveComment)
		}
	}
	dec := wire.NewDecoder(&b)
	for i, want := range frames {
		got, err := dec.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Errorf("frame %d decoded as %s, want %s", i, gotJSON, wantJSON)
		}
	}
	if _, err := dec.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after the last frame the decoder answered %v, want io.EOF", err)
	}
}
