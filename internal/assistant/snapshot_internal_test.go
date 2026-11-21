package assistant

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// snapshotFrame is a snapshot frame of one turn carrying snap.
func snapshotFrame(turn uuid.UUID, snap *wire.SnapshotEvent) wire.Frame {
	return wire.Frame{V: wire.ProtocolVersion, ConversationID: uuid.New(), TurnID: turn, Seq: 4,
		At: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC), Kind: wire.EventSnapshot, Snapshot: snap}
}

// noticeOf is a notice card of n bytes of text hanging on message.
func noticeOf(message uuid.UUID, n int) wire.CardEvent {
	return wire.CardEvent{CardID: uuid.New(), MessageID: message, Card: wire.Card{
		Type: wire.CardNotice, Notice: &wire.NoticeCard{Tone: wire.NoticeInfo,
			Text: strings.Repeat("n", n)}}}
}

// proposalOf is a proposal card carrying n bytes in its title.
func proposalOf(message uuid.UUID, n int) wire.CardEvent {
	return wire.CardEvent{CardID: uuid.New(), MessageID: message, Card: wire.Card{
		Type: wire.CardProposal, Proposal: &wire.ProposalCard{ProposalID: uuid.New(),
			Title: strings.Repeat("p", n), State: wire.ProposalProposed}}}
}

// A SNAPSHOT PAST ONE FRAME IS CUT IN THE ORDER ITS FIELD PROMISES, until it
// fits: every step, then the cards that propose no change, then the oldest
// answers, never a proposal while an answer is left to cut and never the
// question; and one that fits is handed on whole and unmarked. The size is the
// frame's own encoding.
//
// Mutations, each red: cut answers before cards, or proposals before answers,
// and the kept parts differ; cut the question and it is missing; keep cutting
// past the bound and the newest answer is gone; never set Cut and the frame
// does not say so; mark a frame that fits and the whole case does.
func TestASnapshotPastOneFrameIsCutInItsPromisedOrder(t *testing.T) {
	t.Parallel()
	turn := uuid.New()
	question := wire.Message{ID: uuid.New(), TurnID: turn, Role: wire.RoleUser, Text: "Why?"}
	answers := make([]wire.Message, 3)
	for i := range answers {
		answers[i] = wire.Message{ID: uuid.New(), TurnID: turn, Role: wire.RoleAssistant,
			Text: strings.Repeat("a", 1200<<10)}
	}
	steps := make([]wire.StepEvent, 3)
	for i := range steps {
		steps[i] = wire.StepEvent{StepID: uuid.New(), TurnID: turn, Index: i + 1, Tool: "read",
			Label: strings.Repeat("s", 10<<10), State: wire.StepDone}
	}
	snap := &wire.SnapshotEvent{
		Messages: append([]wire.Message{question}, answers...),
		Steps:    steps,
		Cards: []wire.CardEvent{noticeOf(answers[0].ID, 512<<10),
			proposalOf(answers[0].ID, 1<<20), noticeOf(answers[1].ID, 512<<10),
			proposalOf(answers[2].ID, 1<<20)},
	}
	got, err := fitSnapshot(snapshotFrame(turn, snap))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	cut := got.Snapshot
	ids := func(ms []wire.Message) []uuid.UUID {
		out := []uuid.UUID{}
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	switch kept := ids(cut.Messages); {
	case len(encoded) > wire.FrameMaxBytes:
		t.Errorf("the cut snapshot encodes to %d bytes, over %d", len(encoded), wire.FrameMaxBytes)
	case !cut.Cut:
		t.Error("a cut snapshot does not say it was cut")
	case len(cut.Steps) != 0:
		t.Errorf("%d steps were kept while cards and answers were cut", len(cut.Steps))
	case len(cut.Cards) != 2 || cut.Cards[0].Card.Type != wire.CardProposal ||
		cut.Cards[1].Card.Type != wire.CardProposal:
		t.Errorf("the cards kept are %+v, want both proposals and no notice", cut.Cards)
	case len(kept) != 2 || kept[0] != question.ID || kept[1] != answers[2].ID:
		t.Errorf("the messages kept are %v, want the question and the newest answer %v",
			kept, []uuid.UUID{question.ID, answers[2].ID})
	}

	small := &wire.SnapshotEvent{Messages: []wire.Message{question}, Steps: steps[:1],
		Cards: []wire.CardEvent{noticeOf(question.ID, 64)}}
	whole, err := fitSnapshot(snapshotFrame(turn, small))
	if err != nil {
		t.Fatal(err)
	}
	if whole.Snapshot.Cut || len(whole.Snapshot.Steps) != 1 || len(whole.Snapshot.Cards) != 1 ||
		len(whole.Snapshot.Messages) != 1 {
		t.Errorf("a snapshot within one frame was changed: %+v", whole.Snapshot)
	}
}
