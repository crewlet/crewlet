package search_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/search"
)

// THE DASHBOARD ASKS FOR THE CHAT SEARCH PAGE THE ENGINE SERVES.
//
// The chat search screen and the room finder ask `chat_search` for a page
// (`CHAT_SEARCH_PAGE`, contract/chat.ts) and the index answers at most
// [search.MaxChatSearchLimit], clamping a larger ask without a word. They
// asked for fifty — the transcript's page — so every full answer was forty hits
// a screen took for fewer than it asked, and drew as everything that matched.
// Asking for exactly the engine's page is what lets a screen say a full page
// may have more behind it: above it, the clamp hides the truncation; below it,
// a person is shown less than one answer holds.
//
// Mutation: change CHAT_SEARCH_PAGE in contract/chat.ts, or MaxChatSearchLimit,
// and this goes red.
func TestTheDashboardAsksForTheChatSearchPageTheEngineServes(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "CHAT_SEARCH_PAGE")
	if err != nil {
		t.Fatal(err)
	}
	client, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("CHAT_SEARCH_PAGE is %q, which is not an integer: %v", raw, err)
	}
	if client != search.MaxChatSearchLimit {
		t.Errorf("the dashboard asks chat_search for %d hits and the engine answers "+
			"at most %d (search.MaxChatSearchLimit); the engine owns the value, so "+
			"contract/chat.ts is what changes", client, search.MaxChatSearchLimit)
	}
}
