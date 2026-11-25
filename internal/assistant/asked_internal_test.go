package assistant

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// A PLACE OPENS BY ITSELF ONLY WHEN THE PERSON'S OWN QUESTION ASKS FOR IT: a
// request to go there and a name it goes by, both in their words; a setup page
// is also asked for by a request to connect what it sets up.
//
// Mutation: accept the verb alone, or a name inside a longer word, and this
// fails.
func TestAPlaceOpensOnlyWhenTheQuestionAsksForIt(t *testing.T) {
	t.Parallel()
	google := []string{"Open Google Cloud setup", "google_cloud", "Google Cloud"}
	for _, c := range []struct {
		question string
		names    []string
		setup    bool
		want     bool
	}{
		{"please open the Google Cloud integration setup for me", google, true, true},
		{"please help me connect Google Cloud", google, true, true},
		{"Take me to my inbox.", []string{"Inbox"}, false, true},
		{"open ENG-12", []string{"ENG-12"}, false, true},
		{"please help me connect Google Cloud", []string{"Google Cloud"}, false, false},
		{"what does this page say?", []string{"Nodes"}, false, false},
		{"open the Google Cloud setup", []string{"Nodes"}, false, false},
		{"who reopened ENG-12?", []string{"ENG-12"}, false, false},
		{"how do I open an inboxing rule?", []string{"Inbox"}, false, false},
	} {
		if got := askedToOpen(c.question, c.names, c.setup); got != c.want {
			t.Errorf("%q naming %v (setup %v) asked = %v, want %v", c.question, c.names,
				c.setup, got, c.want)
		}
	}
}

// A RESUMED SEGMENT ASKS WITH THE QUESTION THAT OPENED ITS TURN, never another
// turn's or an answer's words.
func TestAResumedTurnKeepsItsOwnQuestion(t *testing.T) {
	t.Parallel()
	turn, other := uuid.New(), uuid.New()
	messages := []assistantdomain.Message{
		{Turn: other, Role: wire.RoleUser, Text: "open my inbox"},
		{Turn: turn, Role: wire.RoleAssistant, Text: "open Settings"},
		{Turn: turn, Role: wire.RoleUser, Text: "connect Google Cloud"},
	}
	if got := questionOf(messages, turn); got != "connect Google Cloud" {
		t.Errorf("the resumed turn asks with %q", got)
	}
}
