package assistantdomain_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// A TURN'S TRANSCRIPT IS THAT TURN'S AND READS NOTHING ELSE OF ITS
// CONVERSATION: a stream's snapshot is of one turn, and a read of the whole
// conversation grows with every turn before it. Two turns of one conversation
// write messages, steps and cards; the first's sealed values are then
// garbled, which a read of the whole conversation trips on, and the second's
// transcript reads back exactly its own records, in order. It is keyed on the
// owner like every read.
//
// Mutations, each red: drop the turn clause from recordQueries and the
// garbled rows of the earlier turn fail the read (or come back with it); drop
// the owner clause and another person reads it.
func TestATurnsTranscriptReadsOnlyThatTurn(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ada := uuid.New()
	c := person(ada, "ada")
	conversation := n.conversation(c, "Payroll")
	write := func(turn uuid.UUID, words string) {
		t.Helper()
		message := n.say(c, turn, 1, wire.RoleUser, words+"?")
		n.say(c, turn, 2, wire.RoleAssistant, words+".")
		if _, err := n.writer.RecordStep(t.Context(), fresh(c), turn, wire.StepEvent{
			StepID: assistantdomain.StepID(turn, 1), Index: 1, Tool: "get_spend",
			Label: words, State: wire.StepDone, StartedAt: monday}); err != nil {
			t.Fatalf("record a step: %v", err)
		}
		if _, err := n.writer.RecordCard(t.Context(), fresh(c), turn, wire.CardEvent{
			CardID: assistantdomain.CardID(turn, 1), MessageID: message,
			Card: wire.Card{Type: wire.CardNotice, Notice: &wire.NoticeCard{
				Tone: wire.NoticeInfo, Text: words}}}, []string{"worklet"}); err != nil {
			t.Fatalf("record a card: %v", err)
		}
	}
	earlier := n.openTurn(c, conversation)
	write(earlier, "Earlier")
	if _, err := n.writer.CloseTurn(t.Context(), fresh(c), earlier,
		assistantdomain.TurnEnd{State: wire.TurnCompleted, Steps: 1}); err != nil {
		t.Fatalf("close the earlier turn: %v", err)
	}
	later := n.openTurn(c, conversation)
	write(later, "Later")
	if err := n.db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, garble := range []string{
			`UPDATE assistant_messages SET text_sealed = 'garbled' WHERE turn_id = ?`,
			`UPDATE assistant_steps SET body_sealed = 'garbled' WHERE turn_id = ?`,
			`UPDATE assistant_cards SET body_sealed = 'garbled' WHERE turn_id = ?`,
		} {
			if _, err := tx.ExecContext(t.Context(), garble, earlier.String()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("garble the earlier turn: %v", err)
	}
	if _, err := n.reader.Transcript(t.Context(), ada, conversation); err == nil {
		t.Fatal("the whole conversation reads past the garbled turn; nothing here tells " +
			"a read of one turn from a read of all")
	}

	got, err := n.reader.TurnTranscript(t.Context(), ada, later)
	if err != nil {
		t.Fatalf("read the later turn: %v", err)
	}
	switch {
	case len(got.Messages) != 2 || got.Messages[0].Text != "Later?" ||
		got.Messages[1].Text != "Later." || got.Messages[0].Turn != later:
		t.Errorf("the messages read %+v", got.Messages)
	case len(got.Steps) != 1 || got.Steps[0].Label != "Later" ||
		got.Steps[0].StepID != assistantdomain.StepID(later, 1):
		t.Errorf("the steps read %+v", got.Steps)
	case len(got.Cards) != 1 || got.Cards[0].Turn != later ||
		got.Cards[0].Card.Notice == nil || got.Cards[0].Card.Notice.Text != "Later":
		t.Errorf("the cards read %+v", got.Cards)
	}
	if _, err := n.reader.TurnTranscript(t.Context(), uuid.New(), later); !errors.Is(err,
		assistantdomain.ErrNotFound) {
		t.Errorf("another person's read of the turn answers %v, want not found", err)
	}
}
