package presence_test

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/presence"
)

// THE DASHBOARD RE-ASSERTS TYPING WELL INSIDE THE ENGINE'S LAPSE.
//
// The hub puts a chat room's typing member back to viewing once nothing has
// re-asserted it within [presence.TypingLapse] — that is what clears the line
// under a tab whose lid closed mid-word — and the chat screen re-asserts at
// most every `TYPING_THROTTLE_MS` (`contract/chat.ts`) while somebody types.
// The lapse is sized as TWO of those intervals, so a re-assert that came late
// is invisible and a second one is not. Neither side can see the other's
// number, and the drift is silent in the one direction that matters: a
// throttle past half the lapse blinks the line off and on under somebody who
// never stopped typing, and nothing anywhere fails. So the relation is held
// here, by the package that owns the lapse.
func TestTheDashboardReassertsTypingInsideTheEnginesExpiry(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "TYPING_THROTTLE_MS")
	if err != nil {
		// FAILS rather than skips: the dashboard source is committed, so a
		// missing declaration is a renamed one.
		t.Fatal(err)
	}
	ms, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("TYPING_THROTTLE_MS is %q, which is not an integer: %v", raw, err)
	}
	throttle := time.Duration(ms) * time.Millisecond
	if throttle <= 0 {
		t.Fatalf("TYPING_THROTTLE_MS is %v; typing re-asserted on every keystroke "+
			"is a presence frame per character, past what a socket may send", throttle)
	}
	if 2*throttle > presence.TypingLapse {
		t.Errorf("the dashboard re-asserts typing every %v and the engine lapses a "+
			"typing line after %v, so one late re-assert blinks the line off under "+
			"somebody still typing — TYPING_THROTTLE_MS in contract/chat.ts must be "+
			"at most half the engine's lapse", throttle, presence.TypingLapse)
	}
}

// THE DASHBOARD SPEAKS THE PRESENCE VOCABULARY, IN THE ENGINE'S ORDER.
//
// A tab's `presence` frame names the KIND of room it is about, and every room
// rides this hub, so `contract/presence.ts` declares the room kinds once: a
// kind the engine grew that the dashboard lacks is a room no screen can be
// present in, and one the dashboard keeps that the engine dropped is a frame
// the socket drops without a word. The activities are what every room's
// chrome draws a member doing, and the frame rate is what a screen coalesces
// what it says to: the engine drops a socket's frames past [ClientRate], so a
// faster screen spends its own CPU telling the room nothing.
func TestTheDashboardSpeaksThePresenceVocabulary(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, engine := range map[string][]string{
		"PRESENCE_ROOMS":      names(presence.RoomKinds),
		"PRESENCE_ACTIVITIES": names(presence.Activities),
	} {
		body, err := clientsource.Literal(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		client := clientsource.Strings(body)
		if len(client) == 0 {
			t.Fatalf("%s names nothing, so this gate certifies nothing", name)
		}
		if !slices.Equal(client, engine) {
			t.Errorf("the dashboard's %s is %q and the engine's is %q, in that order: "+
				"change contract/presence.ts to the engine's", name, client, engine)
		}
	}
	raw, err := clientsource.Scalar(tree, "PRESENCE_FRAMES_PER_SECOND")
	if err != nil {
		t.Fatal(err)
	}
	rate, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("PRESENCE_FRAMES_PER_SECOND is %q, which is not an integer: %v", raw, err)
	}
	if rate != presence.ClientRate {
		t.Errorf("the dashboard sends %d presence frames a second and a socket takes %d: "+
			"change contract/presence.ts to the engine's figure", rate, presence.ClientRate)
	}
}

// names is a closed set's values as the strings the dashboard declares.
func names[S ~string](set []S) []string {
	out := make([]string, len(set))
	for i, v := range set {
		out[i] = string(v)
	}
	return out
}
