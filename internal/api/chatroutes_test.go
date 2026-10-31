package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
)

// chatReadRoutes are the chat questions as the published API spells them, one
// path per question.
var chatReadRoutes = map[string]string{
	"/chatlet/channels":                     "chat_channels",
	"/chatlet/channels/room-1":              "chat_channel",
	"/chatlet/channels/room-1/messages":     "chat_messages",
	"/chatlet/channels/room-1/threads/m-1":  "chat_thread",
	"/chatlet/search":                       "chat_search",
	"/chatlet/room-names":                   "chat_room_names",
	"/chatlet/activity":                     "chat_activity",
	"/chatlet/threads":                      "chat_threads",
	"/chatlet/saved":                        "chat_saved",
	"/chatlet/sent":                         "chat_sent",
	"/chatlet/channels/room-1/links":        "chat_links",
	"/chatlet/channels/room-1/messages/m-2": "chat_message",
	"/chatlet/rooms":                        "chat_rooms",
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
		"/chatlet/search":                                "chat_search",
		"/chatlet/room-names":                            "chat_room_names",
		"/chatlet/activity?cursor=c":                     "chat_activity",
		"/chatlet/threads":                               "chat_threads",
		"/chatlet/saved":                                 "chat_saved",
		"/chatlet/sent":                                  "chat_sent",
		"/chatlet/channels/room-1/links":                 "chat_links",
		"/chatlet/channels/room-1/messages/m-2":          "chat_message",
		"/chatlet/rooms":                                 "chat_rooms",
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
		case "chat_messages", "chat_channel", "chat_links", "chat_message":
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

// quietChat is a chat reader that answers every read empty, for a case about
// which questions exist rather than what they answer.
type quietChat struct{ noReaderViews }

func (quietChat) Channels(context.Context, string, chatlet.ChannelsQuery,
	statelog.Freshness) (chatlet.ChannelListing, error) {
	return chatlet.ChannelListing{}, nil
}

func (quietChat) Channel(context.Context, string, string,
	statelog.Freshness) (chatlet.ChannelDetail, error) {
	return chatlet.ChannelDetail{}, nil
}

func (quietChat) ChannelNamed(context.Context, string, string,
	statelog.Freshness) (chatlet.ChannelDetail, error) {
	return chatlet.ChannelDetail{}, nil
}

func (quietChat) Messages(context.Context, string, chatlet.TranscriptQuery,
	statelog.Freshness) (chatlet.Transcript, error) {
	return chatlet.Transcript{}, nil
}

func (quietChat) Thread(context.Context, string, chatlet.ThreadQuery,
	statelog.Freshness) (chatlet.Thread, error) {
	return chatlet.Thread{}, nil
}

func (quietChat) RoomNames(context.Context, string,
	statelog.Freshness) (chatlet.RoomNames, error) {
	return chatlet.RoomNames{}, nil
}

func (quietChat) Directory(context.Context, string,
	statelog.Freshness) (chatlet.Directory, error) {
	return chatlet.Directory{}, nil
}

func (quietChat) Search(context.Context, estate.ChatQuery) (estate.ChatSearchAnswer, error) {
	return estate.ChatSearchAnswer{}, nil
}

// NO CHAT PUSH KIND IS ALSO A QUESTION.
//
// A frame carries a `kind` and nothing else says which direction it was
// travelling, so a name that is both a push and a question makes a frame
// unreadable without knowing who sent it — the dashboard's dispatch would
// treat an answer as a push or a push as the answer to something it never
// asked. The push kinds are past tense and the questions are not, and this
// holds the two registries apart by their own names, the chat questions
// registered.
func TestNoChatPushKindIsAlsoAQuestion(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Sources: queries.Sources{
		Chat: quietChat{}, ChatSearch: quietChat{},
	}})
	names := a.Queries().Names()
	if !slices.Contains(names, "chat_channels") || !slices.Contains(names, "chat_search") {
		t.Fatalf("the chat questions are not registered, so this case asserts "+
			"nothing: %v", names)
	}
	for _, kind := range stream.ChatPushKinds() {
		if slices.Contains(names, string(kind)) {
			t.Errorf("%q is both a push kind and a question", kind)
		}
	}
}

// THE APP TELLS THE HUB IT SERVES ITS SOCKETS, AND ONLY ONCE IT IS BUILT.
//
// What the hub reports is what the engine's live relay follows a peer's chat
// batches for, so a hub told it serves sockets by an app that then failed to
// build would follow them for nobody — and one never told would leave a node
// whose copy is out of service silent to every screen it serves.
func TestTheAppTellsTheChatHubItServesSocketsOnceBuilt(t *testing.T) {
	t.Parallel()
	refused := stream.NewChatHub(stream.ChatHubOptions{})
	if _, err := api.New(api.Options{ChatLive: refused}); err == nil {
		t.Fatal("an app wired to nothing but a hub was built")
	}
	if refused.ServesSockets() {
		t.Error("an app that failed to build told its hub it serves sockets")
	}
	built := stream.NewChatHub(stream.ChatHubOptions{})
	newApp(t, api.Options{ChatLive: built})
	if !built.ServesSockets() {
		t.Error("the app mounted its socket over the hub and never said so")
	}
}

// noReaderViews is a chat reader double's reader views when its suite reads
// none of them: each is refused, so a case that reaches one by mistake fails
// rather than reading an empty answer as a real one.
type noReaderViews struct{}

var errNoReaderViews = errors.New("this double answers no reader view")

func (noReaderViews) Activity(context.Context, string, chatlet.ActivityQuery,
	statelog.Freshness) (chatlet.Activity, error) {
	return chatlet.Activity{}, errNoReaderViews
}

func (noReaderViews) Threads(context.Context, string, chatlet.ThreadsQuery,
	statelog.Freshness) (chatlet.FollowedThreads, error) {
	return chatlet.FollowedThreads{}, errNoReaderViews
}

func (noReaderViews) Saved(context.Context, string, chatlet.SavedQuery,
	statelog.Freshness) (chatlet.SavedMessages, error) {
	return chatlet.SavedMessages{}, errNoReaderViews
}

func (noReaderViews) Sent(context.Context, string, chatlet.SentQuery,
	statelog.Freshness) (chatlet.SentMessages, error) {
	return chatlet.SentMessages{}, errNoReaderViews
}

func (noReaderViews) Links(context.Context, string, chatlet.LinksQuery,
	statelog.Freshness) (chatlet.SharedLinks, error) {
	return chatlet.SharedLinks{}, errNoReaderViews
}

func (noReaderViews) Message(context.Context, string, chatlet.MessageQuery,
	statelog.Freshness) (chatlet.OneMessage, error) {
	return chatlet.OneMessage{}, errNoReaderViews
}
