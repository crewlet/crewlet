package stream_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// sheet is the artifact every room case is about.
var sheet = artifacts.IDFor("stream-rooms-suite-sheet")

// access decides who may be in which room: each reader's role on each
// artifact, by their record, settable mid-case, and an error that makes every
// decision unknown.
type access struct {
	mu      sync.Mutex
	roles   map[string]artifacts.Role
	trashed map[artifacts.ID]bool
	err     error
	asked   int
}

func newAccess() *access { return &access{roles: map[string]artifacts.Role{}} }

func (a *access) set(record string, id artifacts.ID, role artifacts.Role) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.roles[record+"|"+id.String()] = role
}

// trash puts id in the trash: every reader it is disclosed to is answered
// [artifactsdomain.ErrTrashed], and every other reader as before.
func (a *access) trash(id artifacts.ID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.trashed == nil {
		a.trashed = map[artifacts.ID]bool{}
	}
	a.trashed[id] = true
}

func (a *access) fail(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

func (a *access) Standing(_ context.Context, p iam.Principal, id artifacts.ID) (artifacts.Role, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	if a.err != nil {
		return "", a.err
	}
	role := a.roles[iam.RecordOwner(p)+"|"+id.String()]
	if role != "" && a.trashed[id] {
		return "", fmt.Errorf("%w: %s", artifactsdomain.ErrTrashed, id)
	}
	return role, nil
}

// stores answers every datastore at version 40 with its delete floor at 10.
type stores struct{}

func (stores) Through(context.Context, artifacts.ID) (int64, int64, error) { return 40, 10, nil }

// roomNode is one node serving the socket: its service, its presence hub on a
// broker the case's nodes share, and the address its sockets dial.
type roomNode struct {
	svc    *stream.Service
	hub    *presence.Hub
	access *access
	url    string
}

// roomRing is the keyring every room node's presence frames are sealed under.
var roomRing = statelog.OneKey("k1", "the stream rooms suite's fleet key material")

// newRoomNode serves a socket whose Tier A tokens are bound to seats, as the
// watch suite's are (`<name>-token-long-enough-to-pass`), with artifact rooms
// decided by acc and presence crossing broker.
func newRoomNode(t *testing.T, broker *memory.Broker, name string, acc *access,
	bound map[string]string) *roomNode {

	t.Helper()
	q := broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	wire, err := signedask.NewOneWay[presence.Frame](presence.Channel, roomRing)
	if err != nil {
		t.Fatal(err)
	}
	// THE ENGINE'S ASK BUDGET, not a shorter one: a room fills from its
	// peers on that ask alone until the next rebroadcast, which is later
	// than a case reads for, and an answer applies the moment it arrives,
	// so a longer budget slows nothing that passes.
	hub, err := presence.New(presence.Options{Queue: q, Wire: wire, Node: name,
		AskBudget: eventfan.FleetReadBudget, Coalesce: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Stop(context.Background()) })

	b := config.DefaultBootstrap()
	b.API.Auth.Admin = iam.AdminServe
	for login := range bound {
		id := strings.TrimPrefix(login, "token:")
		b.API.Auth.Tokens = append(b.API.Auth.Tokens, config.APIToken{
			ID: id, Token: id + "-token-long-enough-to-pass", Grants: []iam.Grant{iam.GrantWorklet},
		})
	}
	guard := auth.New(&b).BindSeats(auth.SeatBindings{
		Directory: watchBindings(bound), Chart: watchSeats(bound)})
	svc := buildService(t, stream.Options{Rooms: stream.Rooms{Access: acc, Stores: stores{},
		Presence: hub}})
	srv := httptest.NewServer(stream.Handler(guard, auth.NewCSRF(&b), svc, nil, nil))
	t.Cleanup(srv.Close)
	return &roomNode{svc: svc, hub: hub, access: acc,
		url: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream"}
}

// people is the two readers every room case seats: dana and erin, each a
// token bound to their own seat.
var people = map[string]string{"token:dana": "dana", "token:erin": "erin"}

// open dials as one person and reads past the snapshot.
func (n *roomNode) open(t *testing.T, who string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), n.url,
		&websocket.DialOptions{HTTPHeader: bearer(who + "-token-long-enough-to-pass")})
	if err != nil {
		t.Fatalf("dial as %s: %v", who, err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	if first := next(t, conn); kindOf(first) != stream.KindSnapshot {
		t.Fatalf("first frame = %v, want the snapshot", first["kind"])
	}
	return conn
}

// join asks for a room and answers the frame it was answered with.
func join(t *testing.T, conn *websocket.Conn, id artifacts.ID, since int64) map[string]any {
	t.Helper()
	write(t, conn, map[string]any{"kind": "artifact_join", "id": 7, "artifact": id.String(),
		"since": since})
	return nextOf(t, conn, stream.KindResult, stream.KindError)
}

// nextOf reads frames until one of the given kinds, skipping the rest: a
// room's pushes interleave with the answer a case waits for.
func nextOf(t *testing.T, conn *websocket.Conn, kinds ...stream.Kind) map[string]any {
	t.Helper()
	for {
		frame := next(t, conn)
		if slices.Contains(kinds, kindOf(frame)) {
			return frame
		}
	}
}

// presentIn reads presence pushes until the room shows exactly want (seats),
// as a dashboard applying them in order would.
func presentIn(t *testing.T, conn *websocket.Conn, roster map[string]string, want ...string) {
	t.Helper()
	slices.Sort(want)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got []string
		for _, seat := range roster {
			got = append(got, seat)
		}
		slices.Sort(got)
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the room shows %v, want %v", got, want)
		}
		frame := nextOf(t, conn, stream.KindArtifactPresence)
		data, _ := frame["data"].(map[string]any)
		for _, raw := range asList(data["upsert"]) {
			m, _ := raw.(map[string]any)
			session, _ := m["session"].(string)
			seat, _ := m["seat"].(string)
			roster[session] = seat
		}
		for _, raw := range asList(data["leave"]) {
			session, _ := raw.(string)
			delete(roster, session)
		}
	}
}

func asList(v any) []any {
	list, _ := v.([]any)
	return list
}

// rosterOf is a join answer's roster, by session.
func rosterOf(t *testing.T, answer map[string]any) map[string]string {
	t.Helper()
	data, _ := answer["data"].(map[string]any)
	roster, _ := data["roster"].(map[string]any)
	out := map[string]string{}
	for _, raw := range asList(roster["members"]) {
		m, _ := raw.(map[string]any)
		session, _ := m["session"].(string)
		seat, _ := m["seat"].(string)
		out[session] = seat
	}
	return out
}

// A JOIN IS DECIDED LIKE READING THE ARTIFACT, three-valued, and none of its
// answers closes the socket: allowed answers the session, the role, the room
// with the joiner in it and where the datastore stands; a reader with no
// relation is told the artifact does not exist; a node that cannot decide
// says when to ask again.
func TestAJoinIsDecidedLikeReadingTheArtifact(t *testing.T) {
	t.Parallel()
	acc := newAccess()
	acc.set("dana", sheet, artifacts.RoleEditor)
	n := newRoomNode(t, memory.NewBroker(), "node-a", acc, people)

	dana := n.open(t, "dana")
	answer := join(t, dana, sheet, 25)
	data, _ := answer["data"].(map[string]any)
	if kindOf(answer) != stream.KindResult || answer["what"] != "artifact_join" ||
		data["role"] != "editor" || data["through"] != float64(40) || data["resync"] != false ||
		data["session"] == "" {
		t.Fatalf("an allowed join answered %v", answer)
	}
	if roster := rosterOf(t, answer); len(roster) != 1 || roster[data["session"].(string)] != "dana" {
		t.Fatalf("the joiner was handed the room %v, want one holding itself", roster)
	}
	// A CATCH-UP BELOW THE DELETE FLOOR, and none at all, are whole reads.
	for _, since := range []int64{5, 0} {
		again := join(t, dana, sheet, since)
		if d, _ := again["data"].(map[string]any); d["resync"] != true {
			t.Fatalf("a join since %d answered resync %v, want a whole read", since, d["resync"])
		}
	}

	erin := n.open(t, "erin")
	refused := join(t, erin, sheet, 0)
	if kindOf(refused) != stream.KindError || refused["error"] != stream.CodeNotFound ||
		refused["artifact"] != sheet.String() {
		t.Fatalf("a reader with no relation was answered %v, want not_found naming the artifact",
			refused)
	}
	acc.fail(errors.New("the chart is behind"))
	unknown := join(t, erin, sheet, 0)
	if kindOf(unknown) != stream.KindError || unknown["error"] != stream.CodeUnavailable {
		t.Fatalf("an undecidable join was answered %v, want unavailable", unknown)
	}
	if _, ok := unknown["retry_after"]; !ok {
		t.Fatal("an undecidable join does not say when to ask again")
	}
	for _, conn := range []*websocket.Conn{dana, erin} {
		write(t, conn, map[string]any{"kind": "ping"})
		if got := nextOf(t, conn, stream.KindPong); kindOf(got) != stream.KindPong {
			t.Fatalf("a join's answer closed the socket: %v", got)
		}
	}
}

// A ROOM CROSSES THE FLEET ON THE SOCKET: a person on each of two nodes joins
// one artifact's room, and each socket comes to show both, what each is doing,
// and the one who left when their socket closed. Nothing is forwarded between
// the nodes: each node's hub hears the other's sealed broadcast.
func TestARoomCrossesTheFleetOnTheSocket(t *testing.T) {
	t.Parallel()
	broker, acc := memory.NewBroker(), newAccess()
	acc.set("dana", sheet, artifacts.RoleEditor)
	acc.set("erin", sheet, artifacts.RoleViewer)
	a := newRoomNode(t, broker, "node-a", acc, people)
	b := newRoomNode(t, broker, "node-b", acc, people)

	dana := a.open(t, "dana")
	danaRoom := rosterOf(t, join(t, dana, sheet, 0))
	erin := b.open(t, "erin")
	erinRoom := rosterOf(t, join(t, erin, sheet, 0))
	presentIn(t, erin, erinRoom, "dana", "erin")
	presentIn(t, dana, danaRoom, "dana", "erin")

	write(t, dana, map[string]any{"kind": "presence", "room": "artifact", "artifact": sheet.String(),
		"activity": "editing", "focus": map[string]any{"collection": "review", "doc": "row-12"}})
	deadline := time.Now().Add(10 * time.Second)
	for {
		frame := nextOf(t, erin, stream.KindArtifactPresence)
		data, _ := frame["data"].(map[string]any)
		found := false
		for _, raw := range asList(data["upsert"]) {
			m, _ := raw.(map[string]any)
			focus, _ := m["focus"].(map[string]any)
			if m["seat"] == "dana" && m["activity"] == "editing" && focus["doc"] == "row-12" {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node-b never showed dana editing row-12")
		}
	}

	_ = dana.Close(websocket.StatusNormalClosure, "")
	presentIn(t, erin, erinRoom, "erin")
	waitFor(t, func() bool { return len(a.svc.Hub().Members(sheet)) == 0 },
		"a closed socket stayed in the room on its node")
}

// A REVOKED SHARE DROPS FROM THE ROOM: the movement that withdrew it is
// pushed, the room is withdrawn with an error frame naming the artifact on a
// socket that stays open, the room's presence loses the reader, and nothing
// the datastore commits afterwards reaches them. A reader whose access did not
// narrow stays.
func TestARevokedShareDropsFromTheRoom(t *testing.T) {
	t.Parallel()
	acc := newAccess()
	acc.set("dana", sheet, artifacts.RoleEditor)
	acc.set("erin", sheet, artifacts.RoleViewer)
	n := newRoomNode(t, memory.NewBroker(), "node-a", acc, people)
	dana, erin := n.open(t, "dana"), n.open(t, "erin")
	danaRoom := rosterOf(t, join(t, dana, sheet, 0))
	join(t, erin, sheet, 0)
	presentIn(t, dana, danaRoom, "dana", "erin")

	acc.set("erin", sheet, "")
	n.svc.ArtifactsMoved([]artifactsdomain.Moved{{Artifact: sheet,
		What: artifactsdomain.MoveShare, AccessEpoch: 2, Head: 3}})

	changed := nextOf(t, erin, stream.KindArtifactChanged)
	if data, _ := changed["data"].(map[string]any); data["what"] != "share" || data["head"] != float64(3) {
		t.Fatalf("the movement was pushed as %v", changed)
	}
	withdrawn := nextOf(t, erin, stream.KindError)
	if withdrawn["error"] != stream.CodeNotFound || withdrawn["artifact"] != sheet.String() ||
		withdrawn["what"] != "artifact_join" {
		t.Fatalf("the revoked reader was told %v, want the room withdrawn as not found", withdrawn)
	}
	presentIn(t, dana, danaRoom, "dana")

	n.svc.ArtifactDataMoved([]artifactdata.Moved{dataBatch(sheet,
		delta("review", "row-1", artifacts.ReadAll, "dana", `{"status":"done"}`))})
	if got := nextOf(t, dana, stream.KindArtifactData); got["data"] == nil {
		t.Fatalf("the reader who kept access received %v", got)
	}
	write(t, erin, map[string]any{"kind": "ping"})
	if got := nextOf(t, erin, stream.KindPong, stream.KindArtifactData); kindOf(got) != stream.KindPong {
		t.Fatalf("a reader whose share was withdrawn received %v after it", got)
	}
}

// THE TRASH IS SAID AS THE TRASH: a member of a room whose artifact is binned
// is withdrawn with `artifact_trashed`, and a join while it is binned is
// refused with it, where a reader the artifact is not disclosed to is still
// told it is not there. The room carries no content until its owner restores
// it, and `not_found` told a reader it would never come back.
//
// Mutation: answer the trash as an empty role in the join or the withdrawal,
// or as undecidable, and this goes red.
func TestATrashedArtifactsRoomSaysItIsInTheTrash(t *testing.T) {
	t.Parallel()
	acc := newAccess()
	acc.set("dana", sheet, artifacts.RoleEditor)
	n := newRoomNode(t, memory.NewBroker(), "node-a", acc, people)
	dana := n.open(t, "dana")
	join(t, dana, sheet, 0)

	acc.trash(sheet)
	n.svc.ArtifactsMoved([]artifactsdomain.Moved{{Artifact: sheet,
		What: artifactsdomain.MoveTrash, AccessEpoch: 2, Head: 3}})
	withdrawn := nextOf(t, dana, stream.KindError)
	if withdrawn["error"] != stream.CodeArtifactTrashed || withdrawn["artifact"] != sheet.String() ||
		withdrawn["what"] != "artifact_join" {
		t.Fatalf("a member of a binned artifact's room was told %v, want the room withdrawn "+
			"as artifact_trashed", withdrawn)
	}
	if again := join(t, dana, sheet, 0); kindOf(again) != stream.KindError ||
		again["error"] != stream.CodeArtifactTrashed {
		t.Fatalf("a join of a binned artifact was answered %v, want artifact_trashed", again)
	}
	erin := n.open(t, "erin")
	if stranger := join(t, erin, sheet, 0); stranger["error"] != stream.CodeNotFound {
		t.Fatalf("a reader with no relation to a binned artifact was answered %v, want not_found",
			stranger)
	}
	write(t, dana, map[string]any{"kind": "ping"})
	if got := nextOf(t, dana, stream.KindPong); kindOf(got) != stream.KindPong {
		t.Fatalf("the trash closed the socket: %v", got)
	}
}

// A NARROWING HOLDS THE DATASTORE'S PUSHES until its decision lands: a member
// suspended receives no document, and the decision that keeps it tells it to
// read again what it missed.
func TestANarrowingHoldsTheDatastoreUntilItIsDecided(t *testing.T) {
	t.Parallel()
	svc := buildService(t, stream.Options{Rooms: stream.Rooms{Access: newAccess(),
		Stores: stores{}, Presence: nopPresence{}}})
	c := stream.NewClient(roomReader("erin"))
	svc.Hub().Register(c)
	join, _, ok := svc.Hub().JoinArtifact(c, sheet, artifacts.RoleViewer, "s-erin")
	if !ok {
		t.Fatal("the client did not join")
	}
	svc.Hub().Suspend(sheet)
	svc.Hub().Broadcast(stream.PushArtifact(stream.KindArtifactData, sheet,
		stream.ArtifactData{Artifact: sheet, Through: 41}, clock))
	svc.Hub().Broadcast(stream.PushArtifact(stream.KindArtifactChanged, sheet,
		stream.ArtifactChange{Artifact: sheet, What: artifactsdomain.MoveShare}, clock))
	if got := kindsIn(drain(c)); !slices.Equal(got, []stream.Kind{stream.KindArtifactChanged}) {
		t.Fatalf("a suspended member received %v, want only the movement", got)
	}
	missed, ok := svc.Hub().Resume(c, sheet, join, artifacts.RoleViewer)
	if !ok || !missed {
		t.Fatalf("the decision that kept the member found missed=%v ok=%v", missed, ok)
	}
	svc.Hub().Broadcast(stream.PushArtifact(stream.KindArtifactData, sheet,
		stream.ArtifactData{Artifact: sheet, Through: 42}, clock))
	if got := drain(c); len(got) != 1 || got[0].Kind() != stream.KindArtifactData {
		t.Fatalf("a resumed member received %v", kindsIn(got))
	}
}

// AN UNDECIDABLE RE-DECISION HOLDS THE ROOM: a narrowing whose decision this
// node cannot reach (the estate router failing, an identity copy that cannot
// vouch) keeps the member's datastore pushes withheld rather than resuming it
// at the role it held before the share was withdrawn, and asks again until a
// decision lands. The refusal that finally lands withdraws the room; a member
// that was undecidable only for a while and is admitted is told to re-read.
func TestAnUndecidableRedecisionHoldsTheRoom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		then artifacts.Role
		want stream.Kind
	}{
		{"refused once decidable", "", stream.KindError},
		{"admitted once decidable", artifacts.RoleViewer, stream.KindArtifactData},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			acc := newAccess()
			acc.set("erin", sheet, artifacts.RoleViewer)
			svc := buildService(t, stream.Options{Rooms: stream.Rooms{Access: acc,
				Stores: stores{}, Presence: nopPresence{}}})
			c := stream.NewClient(roomReader("erin"))
			svc.Hub().Register(c)
			if _, _, ok := svc.Hub().JoinArtifact(c, sheet, artifacts.RoleViewer, "s-erin"); !ok {
				t.Fatal("the client did not join")
			}

			acc.fail(errors.New("the estate router could not reach a data node"))
			svc.ArtifactsMoved([]artifactsdomain.Moved{{Artifact: sheet,
				What: artifactsdomain.MoveShare, AccessEpoch: 2, Head: 3}})
			// TWO decisions asked means the first finished and the room
			// was queued again: an undecidable answer that resumed the
			// member would never be asked about a second time.
			waitAsked(t, acc, 2)

			svc.ArtifactDataMoved([]artifactdata.Moved{dataBatch(sheet,
				delta("review", "row-1", artifacts.ReadAll, "dana", `{"status":"done"}`))})
			if got := kindsIn(drain(c)); slices.Contains(got, stream.KindArtifactData) {
				t.Fatalf("a member whose access could not be decided received %v", got)
			}

			acc.mu.Lock()
			acc.err = nil
			acc.roles["erin|"+sheet.String()] = tc.then
			acc.mu.Unlock()
			deadline := time.Now().Add(5 * time.Second)
			for {
				got := kindsIn(drain(c))
				if slices.Contains(got, tc.want) {
					break
				}
				if len(got) > 0 {
					t.Fatalf("once decidable the member received %v, want %s", got, tc.want)
				}
				if time.Now().After(deadline) {
					t.Fatalf("the room was never decided again once decidable")
				}
				time.Sleep(20 * time.Millisecond)
			}
			_, in := svc.Hub().Session(c, sheet)
			if in != (tc.then != "") {
				t.Fatalf("the member is in the room: %v, want %v", in, tc.then != "")
			}
		})
	}
}

// waitAsked waits until acc has been asked at least n decisions.
func waitAsked(t *testing.T, acc *access, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		acc.mu.Lock()
		asked := acc.asked
		acc.mu.Unlock()
		if asked >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the room was asked %d decisions, want at least %d", asked, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A DATASTORE PUSH IS SHAPED PER READER: an editor sees every row of an
// `own_rows` collection and nobody's `private` rows but their own; a viewer
// sees their own rows of both and every row of an open one; a reader in no
// such room sees nothing. One encode serves readers who see the same rows.
func TestADatastorePushIsShapedPerReader(t *testing.T) {
	t.Parallel()
	svc := buildService(t, stream.Options{Rooms: stream.Rooms{Access: newAccess(),
		Stores: stores{}, Presence: nopPresence{}}})
	members := map[string]*stream.Client{}
	for record, role := range map[string]artifacts.Role{"dana": artifacts.RoleEditor,
		"erin": artifacts.RoleViewer, "fred": artifacts.RoleViewer} {
		c := stream.NewClient(roomReader(record))
		svc.Hub().Register(c)
		if _, _, ok := svc.Hub().JoinArtifact(c, sheet, role, "s-"+record); !ok {
			t.Fatal("the client did not join")
		}
		members[record] = c
	}
	outside := stream.NewClient(roomReader("gail"))
	svc.Hub().Register(outside)

	svc.ArtifactDataMoved([]artifactdata.Moved{dataBatch(sheet,
		delta("notes", "open", artifacts.ReadAll, "dana", `{"t":"open"}`),
		delta("votes", "dana-vote", artifacts.ReadOwnRows, "dana", `{"v":1}`),
		delta("votes", "erin-vote", artifacts.ReadOwnRows, "erin", `{"v":2}`),
		delta("drafts", "erin-draft", artifacts.ReadPrivate, "erin", `{"d":"mine"}`),
	)})

	for record, want := range map[string][]string{
		"dana": {"notes/open", "votes/dana-vote", "votes/erin-vote"},
		"erin": {"notes/open", "votes/erin-vote", "drafts/erin-draft"},
		"fred": {"notes/open"},
	} {
		got := drain(members[record])
		if len(got) != 1 {
			t.Fatalf("%s received %d frames, want one", record, len(got))
		}
		var data stream.ArtifactData
		decodeData(t, got[0], &data)
		var docs []string
		for _, d := range data.Deltas {
			docs = append(docs, d.Collection+"/"+d.Doc)
		}
		if !slices.Equal(docs, want) || data.Through != 42 {
			t.Errorf("%s was pushed %v through %d, want %v through 42", record, docs, data.Through, want)
		}
	}
	if got := drain(outside); len(got) != 0 {
		t.Fatalf("a socket in no room received %v", kindsIn(got))
	}

	// AND A BATCH A READER MAY SEE NONE OF IS NOT PUSHED TO THEM AT ALL.
	svc.ArtifactDataMoved([]artifactdata.Moved{dataBatch(sheet,
		delta("drafts", "dana-draft", artifacts.ReadPrivate, "dana", `{"d":"hers"}`))})
	if got := drain(members["fred"]); len(got) != 0 {
		t.Fatalf("a batch fred may see none of reached him as %v", kindsIn(got))
	}
	if got := drain(members["dana"]); len(got) != 1 {
		t.Fatalf("the writer of a private row received %d frames for it", len(got))
	}
}

// A PUSH TOO LARGE TO CARRY NAMES ITS DOCUMENTS INSTEAD, and asks for a
// re-read: the frame stays inside [artifacts.PushFrameMaxBytes].
func TestALargeDatastorePushNamesItsDocuments(t *testing.T) {
	t.Parallel()
	svc := buildService(t, stream.Options{Rooms: stream.Rooms{Access: newAccess(),
		Stores: stores{}, Presence: nopPresence{}}})
	c := stream.NewClient(roomReader("dana"))
	svc.Hub().Register(c)
	if _, _, ok := svc.Hub().JoinArtifact(c, sheet, artifacts.RoleEditor, "s"); !ok {
		t.Fatal("the client did not join")
	}
	big := `{"text":"` + strings.Repeat("x", int(artifacts.PushFrameMaxBytes/2)) + `"}`
	svc.ArtifactDataMoved([]artifactdata.Moved{dataBatch(sheet,
		delta("notes", "a", artifacts.ReadAll, "dana", big),
		delta("notes", "b", artifacts.ReadAll, "dana", big),
		delta("notes", "c", artifacts.ReadAll, "dana", big))})
	got := drain(c)
	if len(got) != 1 {
		t.Fatalf("received %d frames", len(got))
	}
	if n := int64(len(got[0].Raw())); n > artifacts.PushFrameMaxBytes {
		t.Fatalf("the frame is %d bytes, over the %d a push may carry", n, artifacts.PushFrameMaxBytes)
	}
	var data stream.ArtifactData
	decodeData(t, got[0], &data)
	if !data.Reread || len(data.Deltas) != 0 || len(data.Keys) != 3 || data.Keys[1].Doc != "b" {
		t.Fatalf("a large push was sent as %+v", data)
	}
}

// A RESET OF BOTH ARTIFACT DOMAINS TELLS EVERY ROOM TO READ AGAIN, and is all a
// node's rooms are told when what they hear has a gap: the engine's live relay
// hands every switch of source to these two listeners as their own domains'
// reset (a movement naming nothing), so a room is pushed the reset, decided
// again, and told its datastore may have gaps — and keeps hearing what its
// datastore commits afterwards once the decision keeps it.
//
// Mutation: drop the reset arm from ArtifactsMoved and the room is pushed no
// reset; drop it from ArtifactDataMoved and the datastore is never re-read.
func TestAResetTellsEveryRoomToReadAgain(t *testing.T) {
	t.Parallel()
	acc := newAccess()
	acc.set("dana", sheet, artifacts.RoleEditor)
	n := newRoomNode(t, memory.NewBroker(), "node-a", acc, people)
	dana := n.open(t, "dana")
	join(t, dana, sheet, 0)

	n.svc.ArtifactsMoved([]artifactsdomain.Moved{artifactsdomain.Reset()})
	n.svc.ArtifactDataMoved([]artifactdata.Moved{artifactdata.ResetMove()})
	changed := nextOf(t, dana, stream.KindArtifactChanged)
	if data, _ := changed["data"].(map[string]any); data["what"] != "reset" ||
		data["artifact"] != sheet.String() {
		t.Fatalf("the room was pushed %v, want its artifact reset", changed)
	}
	reread := nextOf(t, dana, stream.KindArtifactData)
	if data, _ := reread["data"].(map[string]any); data["resync"] != true {
		t.Fatalf("the datastore was pushed %v, want a resync", reread)
	}

	n.svc.ArtifactDataMoved([]artifactdata.Moved{dataBatch(sheet,
		delta("notes", "a", artifacts.ReadAll, "dana", `{"done":true}`))})
	if got := nextOf(t, dana, stream.KindArtifactData, stream.KindError); kindOf(got) !=
		stream.KindArtifactData {
		t.Fatalf("the room decided again after the reset received %v", got)
	}
}

// A SOCKET IS IN A BOUNDED NUMBER OF ROOMS: past [stream.MaxRoomsPerSocket] a
// join is refused, naming the bound, and the socket stays open.
func TestASocketIsInABoundedNumberOfRooms(t *testing.T) {
	t.Parallel()
	acc := newAccess()
	var ids []artifacts.ID
	for i := range stream.MaxRoomsPerSocket + 1 {
		id := artifacts.IDFor(fmt.Sprintf("room-%d", i))
		acc.set("dana", id, artifacts.RoleViewer)
		ids = append(ids, id)
	}
	n := newRoomNode(t, memory.NewBroker(), "node-a", acc, people)
	dana := n.open(t, "dana")
	for _, id := range ids[:stream.MaxRoomsPerSocket] {
		if got := join(t, dana, id, 0); kindOf(got) != stream.KindResult {
			t.Fatalf("join %s answered %v", id, got)
		}
	}
	over := join(t, dana, ids[stream.MaxRoomsPerSocket], 0)
	if over["error"] != stream.CodeBadParams || !strings.Contains(fmt.Sprint(over["detail"]), "at most") {
		t.Fatalf("a join past the bound answered %v", over)
	}
	// REJOINING A ROOM IT IS IN is not another room.
	if got := join(t, dana, ids[0], 0); kindOf(got) != stream.KindResult {
		t.Fatalf("a rejoin answered %v", got)
	}
}

// A DATASTORE CHANGE REACHES THE SOCKET WELL WITHIN A SECOND of its applier's
// commit: the push is built on the applier's goroutine and handed to the
// socket's queue, with nothing between.
func TestADatastoreChangeReachesTheSocketWithinASecond(t *testing.T) {
	t.Parallel()
	acc := newAccess()
	acc.set("dana", sheet, artifacts.RoleEditor)
	n := newRoomNode(t, memory.NewBroker(), "node-a", acc, people)
	dana := n.open(t, "dana")
	join(t, dana, sheet, 0)
	committed := time.Now()
	n.svc.ArtifactDataMoved([]artifactdata.Moved{dataBatch(sheet,
		delta("notes", "a", artifacts.ReadAll, "dana", `{"x":1}`))})
	nextOf(t, dana, stream.KindArtifactData)
	if took := time.Since(committed); took > time.Second {
		t.Fatalf("a committed change reached the socket after %s, over a second", took)
	}
}

// A SOCKET'S PRESENCE FRAMES ARE HELD TO ITS ALLOWANCE: a burst past it is
// dropped before it reaches the hub.
func TestPresenceFramesAreHeldToTheirAllowance(t *testing.T) {
	t.Parallel()
	counting := &countingPresence{}
	acc := newAccess()
	acc.set("dana", sheet, artifacts.RoleViewer)
	svc := buildService(t, stream.Options{Rooms: stream.Rooms{Access: acc, Stores: stores{},
		Presence: counting}})
	b := config.DefaultBootstrap()
	b.API.Auth.Admin = iam.AdminServe
	b.API.Auth.Tokens = []config.APIToken{{ID: "dana", Token: "dana-token-long-enough-to-pass",
		Grants: []iam.Grant{iam.GrantWorklet}}}
	guard := auth.New(&b).BindSeats(auth.SeatBindings{
		Directory: watchBindings(people), Chart: watchSeats(people)})
	srv := httptest.NewServer(stream.Handler(guard, auth.NewCSRF(&b), svc, nil, nil))
	t.Cleanup(srv.Close)
	n := &roomNode{svc: svc, url: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream"}
	dana := n.open(t, "dana")
	join(t, dana, sheet, 0)
	for range 5 * presence.ClientRate {
		write(t, dana, map[string]any{"kind": "presence", "room": "artifact", "artifact": sheet.String(),
			"activity": "editing"})
	}
	write(t, dana, map[string]any{"kind": "ping"})
	nextOf(t, dana, stream.KindPong)
	time.Sleep(50 * time.Millisecond)
	if got := counting.updates(); got > presence.ClientRate+1 {
		t.Fatalf("a burst of %d presence frames reached the hub %d times, over the "+
			"allowance of %d", 5*presence.ClientRate, got, presence.ClientRate)
	}
	if counting.updates() == 0 {
		t.Fatal("no presence frame reached the hub, so the allowance was never asked")
	}
}

// roomReader is an audience for one person's record.
func roomReader(record string) stream.Audience {
	return stream.NewAudience(iam.Principal{Kind: iam.KindPerson, Login: record + "@example.com",
		Seat: record, Stage: iam.StageActive}, nil)
}

// delta is one changed document as an applier reports it.
func delta(collection, doc string, read artifacts.ReadRule, createdBy, body string) artifactdata.DocDelta {
	return artifactdata.DocDelta{Collection: collection, Doc: doc, Version: 42,
		Body: json.RawMessage(body), Read: read, CreatedBy: createdBy,
		UpdatedBy: iam.Actor{Name: createdBy, Kind: iam.ActorHuman}, UpdatedAt: clock}
}

// dataBatch is one store's committed batch through version 42.
func dataBatch(id artifacts.ID, docs ...artifactdata.DocDelta) artifactdata.Moved {
	return artifactdata.Moved{Artifact: id, Docs: docs,
		Writes:  []artifactdata.WriteSummary{{Seq: 42, At: time.Now()}},
		Through: statelog.Position{Stream: "CREWLET_ARTIFACT_DATA_LOG", Seq: 42}}
}

// nopPresence is a hub that holds nobody, for a case about the datastore.
type nopPresence struct{}

func (nopPresence) Join(_ presence.Room, _ presence.Local, hand presence.Hand) (string, error) {
	if !hand("s", presence.Roster{}) {
		return "", presence.ErrDeclined
	}
	return "s", nil
}
func (nopPresence) Update(presence.Room, string, presence.Activity, presence.Focus) error {
	return nil
}
func (nopPresence) Leave(presence.Room, string)                  {}
func (nopPresence) OnChange(func(presence.Change)) (stop func()) { return func() {} }

// countingPresence counts the presence frames that reached it.
type countingPresence struct {
	nopPresence
	mu sync.Mutex
	n  int
}

func (c *countingPresence) Update(presence.Room, string, presence.Activity, presence.Focus) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return nil
}

func (c *countingPresence) updates() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
