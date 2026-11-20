package stream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/statelog"
)

// heardIn is what [panics] heard of the panics match picks: their scopes, as
// heard.
func heardIn(t *testing.T, match func(*panics.Panic) bool) func() []panics.Scope {
	t.Helper()
	var mu sync.Mutex
	var heard []panics.Scope
	t.Cleanup(panics.Observe(func(p *panics.Panic) {
		if match(p) {
			mu.Lock()
			heard = append(heard, p.Scope)
			mu.Unlock()
		}
	}))
	return func() []panics.Scope {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(heard)
	}
}

// valueIs picks a panic whose value is defect.
func valueIs(defect error) func(*panics.Panic) bool {
	return func(p *panics.Panic) bool {
		err, ok := p.Value.(error)
		return ok && errors.Is(err, defect)
	}
}

// within picks a panic under the module function fn, wherever in its frames.
func within(fn string) func(*panics.Panic) bool {
	return func(p *panics.Panic) bool {
		return slices.ContainsFunc(p.Frames, func(f panics.Frame) bool { return strings.HasSuffix(f.Function, fn) })
	}
}

// heardOnce fails the case unless heard holds n panics, each in [panics.ScopeAPI].
func heardOnce(t *testing.T, heard func() []panics.Scope, n int) {
	t.Helper()
	got := heard()
	if len(got) != n || slices.ContainsFunc(got, func(s panics.Scope) bool { return s != panics.ScopeAPI }) {
		t.Errorf("panics heard %v, want %d in %q", got, n, panics.ScopeAPI)
	}
}

// armedChart answers as [authz.NoChart] until armed, and then panics with
// defect on every question.
type armedChart struct {
	authz.NoChart
	defect error
	armed  atomic.Bool
}

func (c *armedChart) ask() {
	if c.armed.Load() {
		panic(c.defect)
	}
}

func (c *armedChart) Leads(ctx context.Context, actor, subject string) (bool, error) {
	c.ask()
	return c.NoChart.Leads(ctx, actor, subject)
}

func (c *armedChart) LeadsAnyone(ctx context.Context, actor string) (bool, error) {
	c.ask()
	return c.NoChart.LeadsAnyone(ctx, actor)
}

// armedAccess decides every join at role, or panics with defect once while
// armed.
type armedAccess struct {
	defect error
	armed  atomic.Bool
	mu     sync.Mutex
	role   artifacts.Role
}

func (a *armedAccess) Standing(context.Context, iam.Principal, artifacts.ID) (artifacts.Role, error) {
	if a.armed.CompareAndSwap(true, false) {
		panic(a.defect)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.role, nil
}

func (a *armedAccess) set(role artifacts.Role) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.role = role
}

// armedPresence is [quietPresence] whose updates and leaves panic with defect
// while armed.
type armedPresence struct {
	quietPresence
	defect error
	armed  atomic.Bool
}

func (p *armedPresence) Update(presence.Room, string, presence.Activity, presence.Focus) error {
	if p.armed.Load() {
		panic(p.defect)
	}
	return nil
}

func (p *armedPresence) Leave(presence.Room, string) {
	if p.armed.Load() {
		panic(p.defect)
	}
}

// answerTo reads the socket's frames until the answer to a frame naming id,
// a result or an error, and is it as the browser reads it.
func (s *served) answerTo(t *testing.T, id int64) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		_, raw, err := s.conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if kind := Kind(got["kind"].(string)); (kind == KindResult || kind == KindError) && got["id"] == float64(id) {
			return got
		}
	}
}

// A PANIC ON A GOROUTINE A SOCKET SERVES ITS CLIENT ON IS CONTAINED, as
// net/http contains a request handler's: [panics] hears it once, in
// [panics.ScopeAPI], the process and the node's other sockets go on, and each
// goroutine answers as it can. A frame the read loop handles (a watch whose
// chart panics) ends the socket as an internal error; a decision of its
// credential closes it [CloseUndecided], since a socket nobody decides again
// may not stay open; a frame the writer writes ends it, as a failed write
// does; a room frame answers a join as failed, naming the artifact, and the
// socket goes on.
//
// Mutation: drop any of these containments, and the panic ends the binary.
func TestAPanicOnASocketsOwnGoroutineIsContained(t *testing.T) {
	t.Parallel()
	ana := person("ana")
	t.Run("a frame it reads", func(t *testing.T) {
		t.Parallel()
		defect := errors.New("a read frame's own defect")
		heard := heardIn(t, valueIs(defect))
		chart := &armedChart{defect: defect}
		svc := newDecidingService(t, chart)
		s := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana), decide: resolvedAs(ana)})
		s.settled(t, 1)
		s.open(t)
		chart.armed.Store(true)
		s.write(t, map[string]any{"kind": "watch", "seat": "ben"})
		if got := s.closedWith(t); got != websocket.StatusInternalError {
			t.Fatalf("a socket whose frame panicked closed %d, want %d", got, websocket.StatusInternalError)
		}
		heardOnce(t, heard, 1)
		chart.armed.Store(false)
		serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana), decide: resolvedAs(ana)}).open(t)
	})
	t.Run("a decision of its credential", func(t *testing.T) {
		t.Parallel()
		defect := errors.New("a credential decision's own defect")
		heard := heardIn(t, valueIs(defect))
		decide := &switched{before: resolvedAs(ana), after: func(*http.Request) (*http.Request, *auth.Refusal) {
			panic(defect)
		}}
		svc := newDecidingService(t, authz.NoChart{})
		s := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana), decide: decide.answer})
		s.settled(t, 1)
		s.open(t)
		decide.flip()
		svc.CredentialsMoved(Moved{People: []string{ana.ID.String()}})
		if got := s.closedWith(t); got != CloseUndecided {
			t.Fatalf("a socket whose decision panicked closed %d, want %d", got, CloseUndecided)
		}
		heardOnce(t, heard, 1)
	})
	t.Run("a frame it writes", func(t *testing.T) {
		t.Parallel()
		heard := heardIn(t, within("/internal/api/stream.writeLoop"))
		svc := newDecidingService(t, authz.NoChart{})
		s := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana), decide: resolvedAs(ana)})
		s.settled(t, 1)
		s.open(t)
		svc.hub.mu.RLock()
		clients := make([]*Client, 0, len(svc.hub.clients))
		for c := range svc.hub.clients {
			clients = append(clients, c)
		}
		svc.hub.mu.RUnlock()
		if len(clients) != 1 {
			t.Fatalf("the hub holds %d clients, want the socket's one", len(clients))
		}
		clients[0].out <- nil // a frame that is none: its writer's defect
		s.closedWith(t)
		heardOnce(t, heard, 1)
		serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana), decide: resolvedAs(ana)}).open(t)
	})
	t.Run("a room frame", func(t *testing.T) {
		t.Parallel()
		defect := errors.New("a room frame's own defect")
		heard := heardIn(t, valueIs(defect))
		acc := &armedAccess{defect: defect, role: artifacts.RoleViewer}
		pres := &armedPresence{defect: defect}
		opts := quietOptions()
		opts.Rooms = Rooms{Access: acc, Stores: quietStores{}, Presence: pres}
		svc, err := NewService(livestate.New(), opts)
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		t.Cleanup(svc.Stop)
		s := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana), decide: resolvedAs(ana)})
		s.settled(t, 1)
		failed, sheet := artifacts.IDFor("stream-contained-failed"), artifacts.IDFor("stream-contained-sheet")

		acc.armed.Store(true)
		s.write(t, map[string]any{"kind": "artifact_join", "id": 1, "artifact": failed.String()})
		if got := s.answerTo(t, 1); got["error"] != string(CodeQueryFailed) || got["artifact"] != failed.String() {
			t.Fatalf("a join whose decision panicked was answered %v, want query_failed naming it", got)
		}
		s.write(t, map[string]any{"kind": "artifact_join", "id": 2, "artifact": sheet.String()})
		if got := s.answerTo(t, 2); Kind(got["kind"].(string)) != KindResult {
			t.Fatalf("the join after one that panicked was answered %v, want its result", got)
		}
		pres.armed.Store(true)
		s.write(t, map[string]any{"kind": "presence", "room": presence.RoomArtifact, "artifact": sheet.String(),
			"activity": "viewing"})
		s.write(t, map[string]any{"kind": "artifact_leave", "artifact": sheet.String()})
		// ANSWERED BY THE ROOM GOROUTINE, AFTER THE TWO FRAMES BEFORE IT: a ping is
		// the read loop's, which answers it before the room frames are carried out.
		s.write(t, map[string]any{"kind": "artifact_join", "id": 3, "artifact": failed.String()})
		if got := s.answerTo(t, 3); Kind(got["kind"].(string)) != KindResult {
			t.Fatalf("the join after a presence frame and a leave that panicked was answered %v, want its result",
				got)
		}
		pres.armed.Store(false)
		heardOnce(t, heard, 3)
	})
}

// armedRooms is [permissiveRooms] whose next read panics with defect once
// armed.
type armedRooms struct {
	permissiveRooms
	defect error
	armed  atomic.Bool
}

func (r *armedRooms) Channel(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {
	if r.armed.CompareAndSwap(true, false) {
		panic(r.defect)
	}
	return r.permissiveRooms.Channel(ctx, viewer, channelID, fresh)
}

func (r *armedRooms) ChannelHere(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {
	return r.Channel(ctx, viewer, channelID, fresh)
}

// armedReads panics with defect at its first watch, and holds every later one
// open, saying it was asked.
type armedReads struct {
	defect  error
	watches atomic.Int64
	asked   chan struct{}
}

func (r *armedReads) WatchChatReads(context.Context) (<-chan coord.ChatReadUpdate, error) {
	if r.watches.Add(1) == 1 {
		panic(r.defect)
	}
	select {
	case r.asked <- struct{}{}:
	default:
	}
	return make(chan coord.ChatReadUpdate), nil
}

// A PANIC IN A PASS OF A LOOP SERVING EVERY SOCKET IS CONTAINED, AND THE LOOP
// GOES ON to its next pass: the shared tick, the chat hub's fan-out of a batch
// and its read-state watch, and the decider of every artifact room. [panics]
// hears it once, in [panics.ScopeAPI]; the next tick is read, the next batch
// reaches its viewer, the watch is begun again, and a room whose decision
// panicked is held, asked again and decided.
//
// Mutation: drop a loop's containment, and the panic ends the test binary; end
// the loop on it, and its next pass never comes.
func TestAPanicInALoopServingEverySocketIsContainedAndTheLoopGoesOn(t *testing.T) {
	t.Parallel()
	t.Run("the shared tick", func(t *testing.T) {
		t.Parallel()
		defect := errors.New("a tick's own defect")
		heard := heardIn(t, valueIs(defect))
		var armed atomic.Bool
		next := make(chan struct{})
		var once sync.Once
		opts := quietOptions()
		opts.HealthInterval = time.Millisecond
		opts.Health = func() Health {
			if armed.CompareAndSwap(true, false) {
				panic(defect)
			}
			if len(heard()) > 0 {
				once.Do(func() { close(next) })
			}
			return nodeHealth{Status: "ok"}
		}
		svc, err := NewService(livestate.New(), opts)
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		t.Cleanup(svc.Stop)
		armed.Store(true)
		svc.StartHealthTicks(t.Context())
		waitFor(t, next, "the tick after the one that panicked")
		heardOnce(t, heard, 1)
	})
	t.Run("the chat hub's fan-out", func(t *testing.T) {
		t.Parallel()
		defect := errors.New("a chat batch's own defect")
		heard := heardIn(t, valueIs(defect))
		rooms := &armedRooms{permissiveRooms: twoRooms(), defect: defect}
		h := liveHubOver(t, rooms)
		viewer := watcherFor(t, h, chatPerson("alice"))
		rooms.armed.Store(true)
		h.Applied([]chatlet.Applied{post(publicRoom, 1)})
		h.Applied([]chatlet.Applied{post(publicRoom, 2)})
		var seqs []int64
		waitUntil(t, func() bool {
			for _, f := range drainClient(viewer) {
				seqs = append(seqs, dataAs[ChatChange](t, f).ChannelSeq)
			}
			return len(seqs) > 0
		}, "the batch after the one that panicked never reached its viewer")
		if !slices.Equal(seqs, []int64{2}) {
			t.Errorf("the viewer was sent the posts %v, want the one after the batch that panicked", seqs)
		}
		heardOnce(t, heard, 1)
	})
	t.Run("the chat hub's read-state watch", func(t *testing.T) {
		t.Parallel()
		defect := errors.New("a read-state watch's own defect")
		heard := heardIn(t, valueIs(defect))
		reads := &armedReads{defect: defect, asked: make(chan struct{}, 1)}
		h := NewChatHub(ChatHubOptions{Reads: reads})
		h.Start(context.WithoutCancel(t.Context()))
		t.Cleanup(h.Stop)
		h.AttachSockets()
		waitFor(t, reads.asked, "the watch begun again after the one that panicked")
		heardOnce(t, heard, 1)
	})
	t.Run("the decider of every room", func(t *testing.T) {
		t.Parallel()
		defect := errors.New("a room decision's own defect")
		heard := heardIn(t, valueIs(defect))
		id := artifacts.IDFor("stream-contained-decider")
		acc := &armedAccess{defect: defect, role: artifacts.RoleViewer}
		opts := quietOptions()
		opts.Rooms = Rooms{Access: acc, Stores: quietStores{}, Presence: quietPresence{}}
		svc, err := NewService(livestate.New(), opts)
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		t.Cleanup(svc.Stop)
		c := NewClient(NewAudience(person("erin"), nil))
		svc.hub.Register(c)
		if _, _, ok := svc.hub.JoinArtifact(c, id, artifacts.RoleViewer, "s-erin"); !ok {
			t.Fatal("the client did not join")
		}
		acc.set("")
		acc.armed.Store(true)
		svc.ArtifactsMoved([]artifactsdomain.Moved{{Artifact: id, What: artifactsdomain.MoveShare,
			AccessEpoch: 2, Head: 3}})
		waitUntil(t, func() bool {
			_, in := svc.hub.Session(c, id)
			return !in
		}, "the room whose decision panicked was never decided again")
		heardOnce(t, heard, 1)
	})
}
