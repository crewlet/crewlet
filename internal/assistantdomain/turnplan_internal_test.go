package assistantdomain

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// ONE TURN'S RECORDS ARE SOUGHT BY THE TURN, never walked through its whole
// conversation: each read [Reader.TurnTranscript] makes seeks an index whose
// leading columns are the conversation and the turn, so a snapshot of the
// latest turn of a long conversation visits that turn's rows alone. Planned
// as a deployment plans it, with no statistics.
//
// Mutations, each red: drop replicated/0202's index on a table and its read
// seeks the conversation's index alone; drop the turn clause from
// recordQueries and no read seeks by the turn.
func TestOneTurnsRecordsAreSoughtByTheTurn(t *testing.T) {
	t.Parallel()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	q := recordQueries(true)
	args := []any{uuid.NewString(), uuid.NewString()}
	for name, read := range map[string]struct{ sql, index string }{
		"messages": {q.messages, "assistant_messages_turn_idx"},
		"steps":    {q.steps, "assistant_steps_conversation_idx"},
		"cards":    {q.cards, "assistant_cards_turn_idx"},
	} {
		t.Run(name, func(t *testing.T) {
			steps := storetest.Plan(t, db, read.sql, args)
			shown := strings.Join(steps, "\n")
			if !storetest.Seeks(steps, read.index) || !strings.Contains(shown, "turn_id=") {
				t.Errorf("this read does not seek %s by the conversation and the turn:\n%s",
					read.index, shown)
			}
		})
	}
}
