package assistant_test

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// snapshotSeen collects a stream and says when its first snapshot arrived.
type snapshotSeen struct {
	evals.Collector
	once sync.Once
	seen chan struct{}
}

func (s *snapshotSeen) Frame(f wire.Frame) error {
	if err := s.Collector.Frame(f); err != nil {
		return err
	}
	if f.Kind == wire.EventSnapshot {
		s.once.Do(func() { close(s.seen) })
	}
	return nil
}

// A SNAPSHOT PAST THE BROKER'S LARGEST MESSAGE IS CUT, NOT REFUSED: a running
// turn whose records would seal past queue.MaxPayloadBytes, followed from seq
// 0 on another node after its log dropped its first frames, is answered by
// the runner's replay with one snapshot within wire.FrameMaxBytes, marked
// cut, keeping the question, every answer, the proposal and the newest cards,
// and then the turn to its end.
//
// Mutations, each red: drop fitSnapshot and the broker refuses the reply
// (node-b answers its own snapshot and no end); never set Cut; cut answers
// before cards; number the gap's snapshot after+1 in filled and the next page
// asks into the gap again, drawing a second snapshot.
func TestASnapshotPastTheBrokersLimitIsCutNotRefused(t *testing.T) {
	t.Parallel()
	started, hold := make(chan struct{}), make(chan struct{})
	// PATIENT ASKS: sealing and opening this much under the race detector on a
	// loaded runner outlasts live.DefaultAskBudget, and a replay that times
	// out is answered by node-b's own snapshot, which is not this case.
	a, b := fleetAsking(t, time.Minute, evals.RigOptions{Rounds: []evals.Round{{Text: "Done.",
		Input: 100, Output: 20, Streamed: started, Hold: hold}}}, evals.RigOptions{})
	released := false
	defer func() {
		if !released {
			close(hold)
		}
	}()
	accepted, err := a.Ask("large", "Read every report", nil)
	if err != nil {
		t.Fatal(err)
	}
	turn := accepted.TurnID
	<-started

	ctx := t.Context()
	w := assistant.Write{Owner: a.Person.ID, By: iam.ActorFor(a.Person), Key: evals.Key("large")}
	answers := make([]uuid.UUID, 3)
	for i := range answers {
		answers[i] = assistantdomain.MessageID(turn, 100+i)
		if err := a.Store.RecordMessage(ctx, w, turn, assistantdomain.NewMessage{
			ID: answers[i], Role: wire.RoleAssistant, Text: "Report " + string(rune('A'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 10; i++ {
		if err := a.Store.RecordStep(ctx, w, turn, wire.StepEvent{
			StepID: assistantdomain.StepID(turn, i), Index: i, Tool: "read_api",
			Label: strings.Repeat("l", 8<<10), State: wire.StepDone}); err != nil {
			t.Fatal(err)
		}
	}
	proposal := wire.CardEvent{CardID: assistantdomain.CardID(turn, 100), MessageID: answers[0],
		Card: wire.Card{Type: wire.CardProposal, Proposal: &wire.ProposalCard{
			ProposalID: uuid.New(), Scope: wire.ScopeServer, Tool: "create_task",
			Title:   "Create task",
			Changes: []wire.Change{{Kind: wire.ChangeFact, Field: "project", Label: "Project", To: wire.Plain("ENG")}},
			Digest:  "sha256:" + strings.Repeat("d", 64), ExpiresAt: evals.Epoch.Add(15 * time.Minute),
			State: wire.ProposalProposed, ApproveLabel: "Approve and create"}}}
	if err := a.Store.RecordCard(ctx, w, turn, proposal, nil); err != nil {
		t.Fatal(err)
	}
	var notices []uuid.UUID
	for i := range 36 {
		id := assistantdomain.CardID(turn, 101+i)
		notices = append(notices, id)
		if err := a.Store.RecordCard(ctx, w, turn, wire.CardEvent{CardID: id, MessageID: answers[1],
			Card: wire.Card{Type: wire.CardNotice, Notice: &wire.NoticeCard{Tone: wire.NoticeInfo,
				Text: strings.Repeat("r", 200<<10)}}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// The runner's log drops its first frames past its byte budget.
	l, ok := a.Svc.FrameLog(turn)
	if !ok {
		t.Fatal("node-a holds no frame log for the turn it runs")
	}
	for range 3 {
		if _, err := l.Append(wire.Frame{Kind: wire.EventDelta, Delta: &wire.DeltaEvent{
			MessageID: answers[2], Text: strings.Repeat("d", 1536<<10)}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, gap, _ := l.Since(0); !gap {
		t.Fatal("node-a's log still holds the turn's first frame; nothing here asks for a snapshot")
	}

	c := &snapshotSeen{seen: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- b.Svc.Stream(ctx, b.Person.ID, b.Conversation, turn, 0, c) }()
	select {
	case <-c.seen:
	case err := <-done:
		t.Fatalf("node-b's stream ended before any snapshot: %v, %v", err, kinds(c.Frames()))
	}
	close(hold)
	released = true
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	frames := c.Frames()
	if n := len(slices.DeleteFunc(slices.Clone(frames), func(f wire.Frame) bool {
		return f.Kind != wire.EventSnapshot
	})); n != 1 {
		t.Errorf("node-b drew %d snapshots, want one: %v", n, kinds(frames))
	}
	if d := evals.Last(frames, wire.EventDone); d == nil || d.Done.State != wire.TurnCompleted {
		t.Fatalf("node-b's stream ended %v, want the replay followed to the completed turn",
			kinds(frames))
	}
	snapFrame := evals.Last(frames, wire.EventSnapshot)
	encoded, err := json.Marshal(*snapFrame)
	if err != nil {
		t.Fatal(err)
	}
	snap := snapFrame.Snapshot
	var question bool
	var kept []uuid.UUID
	for _, m := range snap.Messages {
		question = question || (m.Role == wire.RoleUser && m.Text == "Read every report")
		if m.Role == wire.RoleAssistant {
			kept = append(kept, m.ID)
		}
	}
	var cards []uuid.UUID
	proposalKept := false
	for _, card := range snap.Cards {
		if card.CardID == proposal.CardID {
			proposalKept = true
			continue
		}
		cards = append(cards, card.CardID)
	}
	switch {
	case len(encoded) > wire.FrameMaxBytes:
		t.Errorf("the snapshot encodes to %d bytes, over %d", len(encoded), wire.FrameMaxBytes)
	case !snap.Cut:
		t.Error("the cut snapshot does not say it was cut")
	case !question:
		t.Error("the snapshot lost the person's question")
	case slices.ContainsFunc(answers, func(id uuid.UUID) bool { return !slices.Contains(kept, id) }):
		t.Errorf("the snapshot kept answers %v, want every one of %v", kept, answers)
	case !proposalKept:
		t.Error("the snapshot cut the proposal while it still held cards that propose nothing")
	case len(snap.Steps) != 0:
		t.Errorf("the snapshot kept %d steps while it cut cards", len(snap.Steps))
	case len(cards) == 0 || len(cards) == len(notices) ||
		!slices.Equal(cards, notices[len(notices)-len(cards):]):
		t.Errorf("the snapshot kept cards %v, want the newest of %d and not all", cards,
			len(notices))
	}
}
