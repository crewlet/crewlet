package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
)

// movedLog records every read state [operator.ChatReads] told a person's
// screens about.
type movedLog struct {
	mu      sync.Mutex
	handles []string
	states  []coord.ChatReadState
}

func (m *movedLog) moved(handle string, state coord.ChatReadState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handles = append(m.handles, handle)
	m.states = append(m.states, state)
}

func (m *movedLog) all() ([]string, []coord.ChatReadState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.handles), slices.Clone(m.states)
}

const readRoom = "6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f"

// A FLUSH THAT LANDED IS TOLD TO THE PERSON'S OTHER SCREENS, WITH THEIR STATE
// AND NOTHING OF THE RECONCILE'S. A cursor is on no log, so no applier will
// ever announce it — the push beside the write is the only way a badge cleared
// in one window clears in the other. The absence stamp the maintenance sweep
// keeps on the same record is its machinery, so it is neither answered nor
// pushed.
func TestAFlushThatLandedIsToldToThePersonsOtherScreens(t *testing.T) {
	t.Parallel()
	store := memory.NewFleet()
	ctx := t.Context()
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if _, err := store.AdvanceChatRead(ctx, "jane-founder", coord.ChatReadDelta{
		Cursors: map[string]int64{readRoom: 3}, At: at,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.SetChatReadAbsence(ctx, "jane-founder", at); err != nil || !ok {
		t.Fatalf("the premise: the record carries an absence stamp (%v, %v)", ok, err)
	}

	told := &movedLog{}
	reads := operator.ChatReads{Store: store, Moved: told.moved}
	state, err := reads.AdvanceChatRead(ctx, "jane-founder", coord.ChatReadDelta{
		Cursors: map[string]int64{readRoom: 9}, At: at.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("the flush: %v", err)
	}
	if state.Cursors[readRoom] != 9 || !state.AbsentSince.IsZero() {
		t.Errorf("the flush answered %+v, want the cursor moved and no absence stamp", state)
	}
	handles, states := told.all()
	if len(handles) != 1 || handles[0] != "jane-founder" {
		t.Fatalf("the flush was told to %v, want the person who made it, once", handles)
	}
	if states[0].Cursors[readRoom] != 9 || !states[0].AbsentSince.IsZero() {
		t.Errorf("their screens were sent %+v, want the cursor and no absence stamp", states[0])
	}

	read, err := reads.ReadState(ctx, "jane-founder")
	if err != nil || read.Cursors[readRoom] != 9 || !read.AbsentSince.IsZero() {
		t.Errorf("the listing counts against %+v (%v), want what the flush wrote "+
			"and no absence stamp", read, err)
	}
}

// failingReads is a read-state store answering one error to everything.
type failingReads struct{ err error }

func (f failingReads) ChatRead(context.Context, string) (coord.ChatReadState, error) {
	return coord.ChatReadState{}, f.err
}

func (f failingReads) AdvanceChatRead(context.Context, string, coord.ChatReadDelta) (
	coord.ChatReadState, error) {
	return coord.ChatReadState{}, f.err
}

// THE STORE'S ERROR IS RETURNED AS IT IS, AND A FLUSH THAT DID NOT LAND IS
// TOLD TO NOBODY. The tool tells a refused flush (the caller's to change) from
// a store that could not take it (worth asking again) by the sentinel alone,
// and says the refusal in the merge's own words — so a wrap or a re-wording
// here would either lose the class or put this package's words in front of the
// person. And a frame for a flush that changed nothing would clear a badge
// another tab still owes.
//
// The read half answers the error too, never an empty map: an empty map is
// somebody who has opened nothing, to whom every room is unread.
func TestAReadStateTheStoreCouldNotTakeIsAnsweredAsTheStoresAndToldToNobody(t *testing.T) {
	t.Parallel()
	for name, cause := range map[string]error{
		"refused by the merge":   fmt.Errorf("%w: muting that would hold 300 rooms", coord.ErrInvalidReadState),
		"an unreachable store":   errors.New("nats: no responders available"),
		"a compare-and-set lost": errors.New("coord: chat read state kept changing"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			told := &movedLog{}
			reads := operator.ChatReads{Store: failingReads{err: cause}, Moved: told.moved}
			_, err := reads.AdvanceChatRead(t.Context(), "jane-founder", coord.ChatReadDelta{
				Cursors: map[string]int64{readRoom: 1}, At: time.Now().UTC(),
			})
			if err != cause { //nolint:errorlint // the identity IS the assertion
				t.Errorf("the flush answered %v, want the store's own error unchanged", err)
			}
			if handles, _ := told.all(); len(handles) != 0 {
				t.Errorf("a flush that did not land was told to %v", handles)
			}
			read, err := reads.ReadState(t.Context(), "jane-founder")
			if err != cause || read.Cursors != nil { //nolint:errorlint // as above
				t.Errorf("the read answered %+v, %v; want no state and the store's error",
					read, err)
			}
		})
	}
}

// lostAnswer is a read-state store whose flushes LAND and then lose their
// answer: the merge is written to the twin beneath, and the caller is told
// [coord.ErrWriteUnknown] — the one failure after which the record may have
// moved. Reads fail with readErr when it is set.
type lostAnswer struct {
	*memory.Fleet
	readErr error
}

func (l lostAnswer) AdvanceChatRead(ctx context.Context, handle string,
	delta coord.ChatReadDelta) (coord.ChatReadState, error) {

	if _, err := l.Fleet.AdvanceChatRead(ctx, handle, delta); err != nil {
		return coord.ChatReadState{}, err
	}
	return coord.ChatReadState{}, fmt.Errorf("%w: %w: advance the chat read state "+
		"for %s: nats: timeout", coord.ErrUnavailable, coord.ErrWriteUnknown, handle)
}

func (l lostAnswer) ChatRead(ctx context.Context, handle string) (coord.ChatReadState, error) {
	if l.readErr != nil {
		return coord.ChatReadState{}, l.readErr
	}
	return l.Fleet.ChatRead(ctx, handle)
}

// A FLUSH WHOSE ANSWER WAS LOST IS ANSWERED AS THAT, AND THE RECORD AS IT
// STANDS IS TOLD TO THE PERSON'S OTHER SCREENS.
//
// The write may have landed, so the tool answers `unknown` from the store's
// own error — unchanged here — and the other tabs must not keep a badge the
// record no longer owes: the record is read back and pushed, carrying the
// cursor that landed and not the reconcile's stamp. A read-back that fails
// pushes nothing, because a frame built from no record would be news of
// nothing.
func TestAFlushWhoseAnswerWasLostTellsTheOtherScreensTheRecordAsItStands(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	store := lostAnswer{Fleet: memory.NewFleet()}
	if _, err := store.Fleet.AdvanceChatRead(ctx, "jane-founder", coord.ChatReadDelta{
		Cursors: map[string]int64{readRoom: 3}, At: at,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.SetChatReadAbsence(ctx, "jane-founder", at); err != nil || !ok {
		t.Fatalf("the premise: the record carries an absence stamp (%v, %v)", ok, err)
	}

	told := &movedLog{}
	reads := operator.ChatReads{Store: store, Moved: told.moved}
	_, err := reads.AdvanceChatRead(ctx, "jane-founder", coord.ChatReadDelta{
		Cursors: map[string]int64{readRoom: 9}, At: at.Add(time.Minute),
	})
	if !errors.Is(err, coord.ErrWriteUnknown) {
		t.Fatalf("the flush answered %v, want the store's lost answer", err)
	}
	handles, states := told.all()
	if len(handles) != 1 || handles[0] != "jane-founder" {
		t.Fatalf("a flush that may have landed was told to %v, want the person "+
			"who made it, once", handles)
	}
	if states[0].Cursors[readRoom] != 9 || !states[0].AbsentSince.IsZero() {
		t.Errorf("their screens were sent %+v, want the record as it stands — the "+
			"cursor that landed — and no absence stamp", states[0])
	}

	// AND A READ-BACK THAT FAILS TELLS NOBODY.
	unread := &movedLog{}
	blind := operator.ChatReads{
		Store: lostAnswer{Fleet: memory.NewFleet(), readErr: errors.New("nats: timeout")},
		Moved: unread.moved,
	}
	if _, err := blind.AdvanceChatRead(ctx, "jane-founder", coord.ChatReadDelta{
		Cursors: map[string]int64{readRoom: 9}, At: at,
	}); !errors.Is(err, coord.ErrWriteUnknown) {
		t.Fatalf("the flush answered %v, want the store's lost answer", err)
	}
	if handles, _ := unread.all(); len(handles) != 0 {
		t.Errorf("a record nobody could read back was told to %v", handles)
	}

	// AND THE PERSON IS TOLD IT MAY HAVE LANDED — through the act
	// transport, as the dashboard flushes: the 503 whose `outcome: unknown`
	// is what separates it from a refusal a client retries as one, never
	// "nothing was recorded".
	s := newSurface(t, operator.Options{
		Chat: builtin.ChatDeps{
			Reader: stubChatReader{}, Writer: stubChatWriter{},
			Cursors: operator.ChatReads{Store: lostAnswer{Fleet: memory.NewFleet()}},
			Actor:   operator.ChatActor(boundChart, nil),
		},
		Org: boundChart,
	})
	raw, err := json.Marshal(map[string]any{
		"request_id": requestA, "args": map[string]any{"cursors": map[string]any{readRoom: 9}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, answer := act(t, guarded(s, false), "founder-secret", chat.MarkChatReadTool,
		"application/json", string(raw))
	detail, _ := answer["detail"].(string)
	if status != http.StatusServiceUnavailable || answer["outcome"] != "unknown" ||
		strings.Contains(detail, "nothing was recorded") {

		t.Errorf("a flush whose answer was lost answered %d %v, want 503 with "+
			"outcome unknown and no claim that nothing was recorded", status, answer)
	}
}

// A FLUSH THE MERGE REFUSES REACHES THE PERSON AS AN ARGUMENT TO CHANGE, IN THE
// MERGE'S OWN WORDS — through the act transport, over the real merge.
//
// A mute past the cap is refused the same way every time it is sent, so it is
// answered 422 `invalid` rather than the 503 a person is told to retry, and the
// sentence names what was wrong without the Go package's prefix in front of it.
// Nothing is pushed: the record did not move.
func TestAMutePastTheCapIsTheCallersToChangeOverTheActTransport(t *testing.T) {
	t.Parallel()
	told := &movedLog{}
	s := newSurface(t, operator.Options{
		Chat: builtin.ChatDeps{
			Reader: stubChatReader{}, Writer: stubChatWriter{},
			Cursors: operator.ChatReads{Store: memory.NewFleet(), Moved: told.moved},
			Actor:   operator.ChatActor(boundChart, nil),
		},
		Org: boundChart,
	})
	muted := make([]string, coord.MaxReadCursors+1)
	for i := range muted {
		muted[i] = fmt.Sprintf("room-%03d", i)
	}
	raw, err := json.Marshal(map[string]any{
		"request_id": requestA, "args": map[string]any{"mute": muted},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, answer := act(t, guarded(s, false), "founder-secret", chat.MarkChatReadTool,
		"application/json", string(raw))
	if status != http.StatusUnprocessableEntity ||
		answer["error"] != string(crewletmcp.RefusalInvalid) {

		t.Fatalf("a mute past the cap answered %d %v, want 422 invalid", status, answer)
	}
	detail, _ := answer["detail"].(string)
	if strings.Contains(detail, "coord") || strings.Count(detail, "refused") != 1 {
		t.Errorf("the person is told %q — the merge's own sentence, once, without "+
			"the package it came from", detail)
	}
	if handles, _ := told.all(); len(handles) != 0 {
		t.Errorf("a refused flush was told to %v", handles)
	}
}

// A SCREEN THAT HAS SEEN NO READ STATE MUTES ITS OWN ROOM, AND ONLY IT, over the
// act transport.
//
// The dashboard's toggle is a fresh tab's first gesture as often as not, and it
// knows one room: the one it is on. Sending that room is the whole gesture —
// nothing is read first, the rooms the person muted from another tab stay
// muted, and the state pushed to their other screens is the whole set the
// record now holds.
func TestAFreshScreenMutesItsOwnRoomAndOnlyItOverTheActTransport(t *testing.T) {
	t.Parallel()
	store := memory.NewFleet()
	if _, err := store.AdvanceChatRead(t.Context(), "jane-founder", coord.ChatReadDelta{
		Mute: []string{"room-from-another-tab"}, At: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	told := &movedLog{}
	s := newSurface(t, operator.Options{
		Chat: builtin.ChatDeps{
			Reader: stubChatReader{}, Writer: stubChatWriter{},
			Cursors: operator.ChatReads{Store: store, Moved: told.moved},
			Actor:   operator.ChatActor(boundChart, nil),
		},
		Org: boundChart,
	})
	status, answer := act(t, guarded(s, false), "founder-secret", chat.MarkChatReadTool,
		"application/json", fmt.Sprintf(`{"request_id":%q,"args":{"mute":[%q]}}`,
			requestA, readRoom))
	if status != http.StatusOK {
		t.Fatalf("a fresh screen's mute answered %d %v", status, answer)
	}
	want := []string{readRoom, "room-from-another-tab"}
	held, err := store.ChatRead(t.Context(), "jane-founder")
	if err != nil || !slices.Equal(held.Muted, want) {
		t.Fatalf("the record holds %v (%v), want this room beside the one muted "+
			"elsewhere", held.Muted, err)
	}
	if _, states := told.all(); len(states) != 1 || !slices.Equal(states[0].Muted, want) {
		t.Errorf("the person's other screens were told %+v, want the whole set", states)
	}

	// AND UNMUTING IT TAKES IT OUT ALONE.
	status, answer = act(t, guarded(s, false), "founder-secret", chat.MarkChatReadTool,
		"application/json", fmt.Sprintf(`{"request_id":%q,"args":{"unmute":[%q]}}`,
			requestB, readRoom))
	if status != http.StatusOK {
		t.Fatalf("the unmute answered %d %v", status, answer)
	}
	if held, err := store.ChatRead(t.Context(), "jane-founder"); err != nil ||
		!slices.Equal(held.Muted, []string{"room-from-another-tab"}) {

		t.Errorf("after the unmute the record holds %v (%v), want the other tab's "+
			"room alone", held.Muted, err)
	}
}
