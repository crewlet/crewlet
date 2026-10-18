package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
)

// chatReadRoutes are the chat questions as the published API spells them, one
// path per question.
var chatReadRoutes = map[string]string{
	"/chatlet/channels":                    "chat_channels",
	"/chatlet/channels/room-1":             "chat_channel",
	"/chatlet/channels/room-1/messages":    "chat_messages",
	"/chatlet/channels/room-1/threads/m-1": "chat_thread",
	"/chatlet/mentions":                    "chat_mentions",
	"/chatlet/search":                      "chat_search",
}

// EVERY CHAT READ ROUTE REACHES THE REGISTRY.
//
// A path nothing mounted answers the mux's own 404, while a route whose
// QUESTION has no source answers the query layer's `unknown_query`. The
// difference is the whole of what this asserts: these paths exist, and what a
// node without Chatlet is missing is the source rather than the route.
func TestEveryChatReadRouteReachesTheQueryLayer(t *testing.T) {
	t.Parallel()
	a := restApp(t)
	for path := range chatReadRoutes {
		status, body := get(t, a, path)
		if status != http.StatusNotFound || body["error"] != "unknown_query" {
			t.Errorf("GET %s answered %d %v; a route that reached the registry "+
				"with no source answers 404 unknown_query", path, status, body)
		}
	}
}

// AND EACH ONE IS ITS OWN QUESTION, WITH ITS PATH VALUES UNDER THE NAMES THE
// QUESTION READS.
//
// The literal segments sit under the room's wildcard, so a route registered
// in the wrong place is answered as ANOTHER question — the room called
// "messages", or a thread asked as a room — and a path value captured under a
// shorter name reaches the question as no room at all, refused naming a key
// the caller did send. Each question here answers its own name and the values
// it was handed, so either mistake is a different body; and the same request
// over the generic form must answer the same bytes, because the two
// transports are one implementation or they drift.
func TestEachChatReadRouteAsksItsOwnQuestionWithItsPath(t *testing.T) {
	t.Parallel()
	a := restApp(t)
	echo := func(what string) queries.Answer {
		return func(_ context.Context, p queries.Params) (any, error) {
			return map[string]any{
				"what":       what,
				"channel_id": p.String("channel_id"),
				"root_id":    p.String("root_id"),
				"cursor":     p.String("cursor"),
			}, nil
		}
	}
	for _, what := range chatReadRoutes {
		a.Queries().Register(what, queries.Everybody(), echo(what))
	}

	for path, what := range map[string]string{
		"/chatlet/channels/room-1/messages?cursor=41":    "chat_messages",
		"/chatlet/channels/room-1/threads/m-1?cursor=17": "chat_thread",
		"/chatlet/channels/room-1":                       "chat_channel",
		"/chatlet/channels":                              "chat_channels",
		"/chatlet/mentions?cursor=c":                     "chat_mentions",
		"/chatlet/search":                                "chat_search",
	} {
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, path, nil)))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s answered %d: %s", path, rec.Code, rec.Body.String())
			continue
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if got["what"] != what {
			t.Errorf("GET %s was answered by %v, want %s", path, got["what"], what)
		}
		wantRoom, wantRoot := "", ""
		switch what {
		case "chat_messages", "chat_channel":
			wantRoom = "room-1"
		case "chat_thread":
			wantRoom, wantRoot = "room-1", "m-1"
		}
		if got["channel_id"] != wantRoom || got["root_id"] != wantRoot {
			t.Errorf("GET %s handed the question channel_id=%v root_id=%v, want "+
				"%q and %q", path, got["channel_id"], got["root_id"], wantRoom, wantRoot)
		}
	}

	// THE GENERIC FORM, the same request: one implementation, one answer.
	_, named := get(t, a, "/chatlet/channels/room-1/threads/m-1?cursor=17")
	_, generic := get(t, a, "/query/chat_thread?channel_id=room-1&root_id=m-1&cursor=17")
	namedRaw, _ := json.Marshal(named)
	genericRaw, _ := json.Marshal(generic)
	if string(namedRaw) != string(genericRaw) {
		t.Errorf("the named route and the generic form answered differently:\n"+
			"  named:   %s\n  generic: %s", namedRaw, genericRaw)
	}
}
