package catalog_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
)

// ASK CREWLET FINDS THE TURN A MESSAGE STARTED: list_turns with a message's id
// asks `GET /turns` for the turns that message started in the seat it reached,
// over the history a node keeps unless the call narrows it.
func TestListTurnsFindsTheTurnAMessageStarted(t *testing.T) {
	t.Parallel()
	turns, ok := catalog.Lookup("list_turns")
	if !ok {
		t.Fatal("the catalogue has no list_turns")
	}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"message_id": "msg-1", "handle": "qa"},
			"days=30&limit=25&origin=chat_message%3Amsg-1&seat=qa"},
		{map[string]any{"message_id": "msg-1", "handle": "qa", "days": 7},
			"days=7&limit=25&origin=chat_message%3Amsg-1&seat=qa"},
		{map[string]any{"handle": "qa"}, "limit=25&seat=qa"},
	} {
		req, err := turns.Request(c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if got := req.Query.Encode(); got != c.want {
			t.Errorf("%v asked %q, want %q", c.args, got, c.want)
		}
	}
}
