package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/store"
)

// fakeMattermost is a Mattermost instance as far as one bot's transport can
// tell: it names the bot, lists one direct-message channel, answers a
// backfill read, and pushes a post down every websocket open on it — one per
// node, since every node reads every seat's socket.
type fakeMattermost struct {
	*httptest.Server

	mu      sync.Mutex
	sockets []*websocket.Conn
	posts   []map[string]any
}

func startFakeMattermost(t *testing.T) *fakeMattermost {
	t.Helper()
	m := &fakeMattermost{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"bot-ceo","username":"ceo"}`))
	})
	mux.HandleFunc("/api/v4/users/bot-ceo/teams", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"t1","name":"acme"}]`))
	})
	mux.HandleFunc("/api/v4/users/bot-ceo/teams/t1/channels", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"D1","name":"bot-ceo__u-ana","type":"D"}]`))
	})
	mux.HandleFunc("/api/v4/channels/D1/posts", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		m.mu.Lock()
		defer m.mu.Unlock()
		order, byID := []string{}, map[string]any{}
		for _, p := range m.posts {
			if at, _ := p["create_at"].(float64); int64(at) > since {
				order = append([]string{p["id"].(string)}, order...)
				byID[p["id"].(string)] = p
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"order": order, "posts": byID})
	})
	mux.HandleFunc("/api/v4/websocket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		m.mu.Lock()
		m.sockets = append(m.sockets, conn)
		m.mu.Unlock()
		// Held open until the client goes: reading is what answers its
		// pings, and its frames are nothing this stand-in acts on.
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Close)
	return m
}

// open is how many sockets are attached.
func (m *fakeMattermost) open() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sockets)
}

// post writes a direct message from a person to the bot, and pushes it down
// every socket as Mattermost does.
func (m *fakeMattermost) post(t *testing.T, id, message string) {
	t.Helper()
	p := map[string]any{
		"id": id, "channel_id": "D1", "user_id": "u-ana", "message": message,
		"create_at": float64(time.Now().UnixMilli()),
	}
	serialised, _ := json.Marshal(p)
	frame, _ := json.Marshal(map[string]any{
		"event": "posted",
		"data": map[string]any{
			"post": string(serialised), "channel_type": "D",
			"channel_name": "bot-ceo__u-ana", "sender_name": "@ana",
		},
		"broadcast": map[string]any{"channel_id": "D1"},
	})
	m.mu.Lock()
	m.posts = append(m.posts, p)
	sockets := slices.Clone(m.sockets)
	m.mu.Unlock()
	for _, conn := range sockets {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err := conn.Write(ctx, websocket.MessageText, frame)
		cancel()
		if err != nil {
			t.Logf("push to one socket: %v", err)
		}
	}
}

// ONE POST WAKES ITS SEAT ONCE ON A FLEET, and is counted as one delivery.
//
// Every node opens every seat's socket — that is the redundancy a restart
// relies on — so a post arrives once per node. Deduplicated only within each
// process, a two-node fleet published it twice, woke the seat twice under two
// random wake ids, and the seat answered twice. Here two real engines — a data
// node and a stateless one, which reaches the claim store and the data node's
// event log over its leaf link — both read the bot's socket, and the seat is
// woken once under the post's derived wake id, with ONE delivery row on the
// data node whichever of the two delivered it.
func TestAMattermostPostWakesItsSeatOnceOnAFleet(t *testing.T) {
	mm := startFakeMattermost(t)
	p := startStatelessPairWith(t, func(doc string) string {
		doc = strings.Replace(doc, "roles:\n", "integrations:\n  mattermost:\n    enabled: true\n"+
			"    url: "+mm.URL+"\n    team: acme\nroles:\n", 1)
		return strings.Replace(doc, "    handle: ceo\n    llm: scripted\n",
			"    handle: ceo\n    llm: scripted\n    integrations:\n      mattermost:\n"+
				"        bot_token: \"${CREWLET_TEST_MM_TOKEN}\"\n", 1)
	}, map[string]string{"CREWLET_TEST_MM_TOKEN": "tok-ceo"})
	waitFor(t, "the stateless node to be admitted by a data node", hydrated(t, p.agent.engine))
	waitFor(t, "both nodes to open the bot's socket", func() bool { return mm.open() >= 2 })
	box := watchInbox(t, p.data, "ceo")

	mm.post(t, "post-1", "can you look at the rollback?")

	got := box.settled(t, 1)
	time.Sleep(time.Second)
	box.mu.Lock()
	wakes := slices.Clone(box.envelopes)
	box.mu.Unlock()
	if len(got) == 0 || len(wakes) != 1 {
		t.Fatalf("the seat was woken %d times by one post read on two nodes, want once", len(wakes))
	}
	if want := mattermost.WakeID("ceo", "post-1"); wakes[0].ID != want {
		t.Errorf("wake id = %s, want the post's derived %s", wakes[0].ID, want)
	}

	deliveries := func() []store.EventRecord {
		rows, err := p.data.engine.Backends().Store.Events().List(t.Context(),
			store.ListQuery{Category: events.WebhookCategory, Limit: 50})
		if err != nil {
			t.Fatalf("list deliveries: %v", err)
		}
		return rows
	}
	waitFor(t, "the post's delivery row on the data node", func() bool { return len(deliveries()) >= 1 })
	time.Sleep(time.Second)
	rows := deliveries()
	if len(rows) != 1 {
		t.Fatalf("%d delivery rows for one post to one seat, want 1", len(rows))
	}
	row := rows[0]
	if row.Type != "socket:posted" || row.Source != mattermost.Backend ||
		row.Tags["recipient"] != "ceo" || row.Tags["delivery_key"] != "post-1" {
		t.Errorf("delivery row = (%q, %q, %v), want socket:posted from mattermost for ceo, post-1",
			row.Type, row.Source, row.Tags)
	}
}
