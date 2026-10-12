package e2e

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/queue/topics"
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
		// pings and takes in the pongs to this side's ([caughtUp]), and
		// its frames are nothing this stand-in acts on. A socket the
		// client has left is let go, so a post and a ping go only to a
		// node that is still listening.
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				m.mu.Lock()
				m.sockets = slices.DeleteFunc(m.sockets,
					func(c *websocket.Conn) bool { return c == conn })
				m.mu.Unlock()
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

// open is how many sockets are attached, one per node listening.
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

// caughtUp returns once every node listening has finished with every frame
// pushed to it so far: claimed each post and queued it, or found a peer's claim
// and dropped it.
//
// A PING BEHIND THE FRAMES, answered by the node itself. The client library
// answers a ping only from inside a read (coder/websocket runs no reader of its
// own), and a seat's pump reads its socket again only once it has delivered the
// frame before (internal/mattermost's pump) — so a node's pong is its own word
// that every frame ahead of the ping is done with. It is the only word a node
// that lost a post's claim gives: it publishes nothing and logs nothing,
// because nothing is owed, and an absence read after a fixed sleep instead
// passed whenever the duplicate came later than the sleep.
//
// [TestANodeAnswersAPingOnlyOnceItHasQueuedThePostAheadOfIt] holds that
// premise, since nothing else here would notice it stop holding.
func (m *fakeMattermost) caughtUp(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()
	if err := m.pingAll(ctx); err != nil {
		t.Fatalf("a node did not answer a ping behind the frames pushed to it: %v", err)
	}
}

// pingAll pings every open socket at once and waits for every answer, or for
// ctx. With no socket open it is an error rather than a vacuous answer, since
// "every node is done" read off no node at all would pass anything.
func (m *fakeMattermost) pingAll(ctx context.Context) error {
	m.mu.Lock()
	sockets := slices.Clone(m.sockets)
	m.mu.Unlock()
	if len(sockets) == 0 {
		return errors.New("no node has a socket open to ping")
	}
	errs := make([]error, len(sockets))
	var pings sync.WaitGroup
	for i, conn := range sockets {
		pings.Go(func() { errs[i] = conn.Ping(ctx) })
	}
	pings.Wait()
	return errors.Join(errs...)
}

// mattermostCompany enables the integration against url, with the CEO seat's
// bot token a reference to CREWLET_TEST_MM_TOKEN.
func mattermostCompany(url string) func(doc string) string {
	return func(doc string) string {
		doc = strings.Replace(doc, "roles:\n", "integrations:\n  mattermost:\n    enabled: true\n"+
			"    url: "+url+"\n    team: acme\nroles:\n", 1)
		return strings.Replace(doc, "    handle: ceo\n    llm: scripted\n",
			"    handle: ceo\n    llm: scripted\n    integrations:\n      mattermost:\n"+
				"        bot_token: \"${CREWLET_TEST_MM_TOKEN}\"\n", 1)
	}
}

// mattermostEnv is what mattermostCompany's reference resolves to.
var mattermostEnv = map[string]string{"CREWLET_TEST_MM_TOKEN": "tok-ceo"}

// A NODE ANSWERS A PING ONLY ONCE IT HAS QUEUED THE POST AHEAD OF IT.
//
// Held here because [fakeMattermost.caughtUp] rests on it rather than because
// anything promises it: the fleet case reads the absence of a duplicate once
// every node has answered a ping sent behind the post, and that is whole only
// if a node cannot answer while a frame ahead of the ping is still being
// delivered. A pump that handed posts to goroutines, or a socket given a reader
// of its own, would answer at once — and the fleet case would be back to
// reading an absence at an arbitrary moment, with nothing failing.
//
// The post's queueing is held INSIDE its publish, by a listener the publishing
// goroutine runs, and the ping is given [holdFirst] to go unanswered: a node
// that answered pings beside its deliveries would answer within milliseconds,
// and one that does not costs this case that second once.
func TestANodeAnswersAPingOnlyOnceItHasQueuedThePostAheadOfIt(t *testing.T) {
	t.Parallel()
	mm := startFakeMattermost(t)
	n := startNode(t, nodeSpec{company: mattermostCompany(mm.URL), env: mattermostEnv})
	waitFor(t, "the node to open the bot's socket", func() bool { return mm.open() >= 1 })

	held, release := make(chan struct{}), make(chan struct{})
	var holding, releasing sync.Once
	let := func() { releasing.Do(func() { close(release) }) }
	// RELEASED ON EVERY PATH, the failing ones included: a pump left
	// blocked in its publish never stops, and neither does the engine.
	t.Cleanup(let)
	n.engine.Backends().Queue.AddPublishListener(
		func(_ context.Context, topic string, ev *events.Event) {
			if topic != topics.NotificationsInbound || ev.Source != mattermost.Backend {
				return
			}
			holding.Do(func() { close(held) })
			<-release
		})

	mm.post(t, "post-1", "can you look at the rollback?")
	select {
	case <-held:
	case <-time.After(waitBudget):
		t.Fatal("timed out waiting for the node to queue the post")
	}

	ctx, cancel := context.WithTimeout(t.Context(), holdFirst)
	err := mm.pingAll(ctx)
	cancel()
	if err == nil {
		t.Fatal("the node answered a ping while the post ahead of it was still being " +
			"queued, so an answer is not its word that it is done with every frame " +
			"before the ping, which caughtUp takes it for")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the held ping ended for another reason than going unanswered: %v", err)
	}

	let()
	mm.caughtUp(t)
}

// published is what each node handed its broker for a Mattermost post: the
// raw webhook that QUEUES it for the inbound edge, and the delivery RECORD that
// counts it on the integrations screen — by post id, naming the node.
//
// HEARD AS A PUBLISH LISTENER, which runs in the publishing goroutine once the
// broker has acknowledged the event. So once a node is done with a post
// ([fakeMattermost.caughtUp]), every publish it made for it is already counted
// here; a subscription would hear it some time later, and a count read then
// would race a consumer.
type published struct {
	mu      sync.Mutex
	queued  map[string][]string
	records map[string][]string
}

// hear starts counting what each node publishes for a post.
func hear(nodes ...*node) *published {
	p := &published{queued: map[string][]string{}, records: map[string][]string{}}
	for _, n := range nodes {
		name := n.id
		n.engine.Backends().Queue.AddPublishListener(
			func(_ context.Context, topic string, ev *events.Event) {
				if ev.Source != mattermost.Backend {
					return
				}
				if w, ok := events.DataAs[*types.RawWebhook](ev); ok &&
					topic == topics.NotificationsInbound {
					post, _ := w.Body["post"].(map[string]any)
					id, _ := post["id"].(string)
					p.add(p.queued, id, name)
				}
				if d, ok := events.DataAs[*types.InboundDelivery](ev); ok {
					p.add(p.records, d.DeliveryKey, name)
				}
			})
	}
	return p
}

func (p *published) add(into map[string][]string, post, node string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	into[post] = append(into[post], node)
}

// by names the node behind each publish of post one of p's tallies heard, in
// the order it heard them.
func (p *published) by(tally map[string][]string, post string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(tally[post])
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
//
// COUNTED ONCE BOTH NODES ARE DONE WITH THE POST ([fakeMattermost.caughtUp]),
// never after a sleep: the node that loses the claim publishes nothing, so the
// absence of its copy can only be read once that node has said it is past the
// post. The two one-second sleeps this replaced passed a copy the losing node
// queued three seconds late.
func TestAMattermostPostWakesItsSeatOnceOnAFleet(t *testing.T) {
	t.Parallel()
	mm := startFakeMattermost(t)
	p := startStatelessPairWith(t, mattermostCompany(mm.URL), mattermostEnv)
	waitFor(t, "the stateless node to be admitted by a data node", hydrated(t, p.agent.engine))
	waitFor(t, "both nodes to open the bot's socket", func() bool { return mm.open() >= 2 })
	box := watchInbox(t, p.data, "ceo")
	sent := hear(p.data, p.agent)

	mm.post(t, "post-1", "can you look at the rollback?")
	mm.caughtUp(t)

	// QUEUED ONCE, by whichever node won the claim — the first layer, and
	// the one this case is about: read on two nodes, a post the claim did
	// not collapse is queued twice and wakes its seat twice. And RECORDED
	// ONCE, by that same node, since only a post that was queued is a
	// delivery.
	queued := sent.by(sent.queued, "post-1")
	if len(queued) != 1 {
		t.Fatalf("the post was queued by %v, want exactly one node: every node reads "+
			"it, and the claim lets one publish it", queued)
	}
	if recorded := sent.by(sent.records, "post-1"); len(recorded) != 1 ||
		recorded[0] != queued[0] {
		t.Fatalf("the post's delivery was recorded by %v, want once, by %s, which "+
			"queued it", recorded, queued[0])
	}

	// UNDER THE POST'S DERIVED ID, the second layer: a wake for this post
	// carries the same id whichever node's delivery it came from, and the
	// inbox and the completion ledger collapse on it. How many wakes one
	// queued delivery makes is the inbound edge's own claim
	// (TestAVerifiedDeliveryWakesTheSeat holds it exactly), and on a fleet a
	// delivery is handled by whichever member takes it, so it is the count
	// above, not a count of wakes, that this case can read whole.
	box.settled(t, 1)
	box.mu.Lock()
	wakes := slices.Clone(box.envelopes)
	box.mu.Unlock()
	want := mattermost.WakeID("ceo", "post-1")
	for _, wake := range wakes {
		if wake.ID != want {
			t.Errorf("wake id = %s, want the post's derived %s", wake.ID, want)
		}
	}

	deliveries := func() []store.EventRecord {
		rows, err := p.data.engine.Backends().Store.Events().List(t.Context(),
			store.ListQuery{Category: events.WebhookCategory, Limit: 50})
		if err != nil {
			t.Fatalf("list deliveries: %v", err)
		}
		return rows
	}
	// ON THE DATA NODE whichever node recorded it: the stateless node's
	// record reaches it through custody. ONE row is a whole answer once it
	// is there, because a row is a record kept and one record was
	// published.
	waitFor(t, "the post's delivery row on the data node", func() bool { return len(deliveries()) >= 1 })
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
