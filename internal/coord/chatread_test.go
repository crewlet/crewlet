package coord_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// A PERSON'S READ STATE CARRIES ONLY WHAT IS TRUE OF THEM.
//
// The record goes onto the wire as it is stored — the answer to a flush and
// the cursor push are both this struct — and the dashboard reads a present
// `dnd_until` as an instant somebody's do-not-disturb ends. `omitempty` never
// omits a struct, so a zero instant under it went out as
// "0001-01-01T00:00:00Z": a person never in do-not-disturb was sent a
// do-not-disturb two thousand years over. So the zero state is pinned as the
// literal bytes with no instant in it at all, and a set one round-trips.
func TestAReadStateCarriesNoInstantItDoesNotHave(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(coord.ChatReadState{Cursors: map[string]int64{"room-a": 7}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(raw), `{"cursors":{"room-a":7}}`; got != want {
		t.Fatalf("a state with no do-not-disturb, no stamp and no absence encodes "+
			"as %s, want %s", got, want)
	}

	until := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	set := coord.ChatReadState{DNDUntil: until, UpdatedAt: at, AbsentSince: at}
	raw, err = json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(raw), `{"dnd_until":"2026-10-08T18:00:00Z",`+
		`"updated_at":"2026-10-08T09:00:00Z","absent_since":"2026-10-08T09:00:00Z"}`; got != want {
		t.Fatalf("a state with every instant set encodes as %s, want %s", got, want)
	}
	var back coord.ChatReadState
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.DNDUntil.Equal(until) || !back.UpdatedAt.Equal(at) || !back.AbsentSince.Equal(at) {
		t.Fatalf("the state read back as %+v, want %+v", back, set)
	}
}

// A REFUSED FLUSH READS AS ITS OWN SENTENCE, because the caller that sent it
// is told it: the operator catalogue's tool answers a refused flush in it to
// the model that made the call, and "coord: a chat read flush was refused:
// muting that …" is a Go package path and a doubled "refused" where an
// instruction belongs. Only the merge's own refusal is shortened — a wrap a
// caller added is context, and anything else is not this sentinel's to tidy.
func TestARefusedFlushReadsAsItsOwnSentence(t *testing.T) {
	t.Parallel()
	muted := make([]string, coord.MaxReadCursors+1)
	for i := range muted {
		muted[i] = fmt.Sprintf("room-%d", i)
	}
	_, refused := coord.MergeChatRead(coord.ChatReadState{}, coord.ChatReadDelta{Mute: muted})
	if !errors.Is(refused, coord.ErrInvalidReadState) {
		t.Fatalf("a mute past the cap was refused with %v, want the sentinel", refused)
	}
	got := coord.ReadStateSentence(refused)
	if strings.Contains(got, "coord") || strings.Contains(got, "refused") {
		t.Errorf("the merge's refusal reads %q — the package and the sentinel's "+
			"own words are still in it", got)
	}
	if !strings.HasPrefix(got, fmt.Sprintf("muting that would hold %d channels", len(muted))) {
		t.Errorf("the merge's refusal reads %q, want what was wrong with the flush", got)
	}

	for name, err := range map[string]error{
		"a refusal a caller wrapped": fmt.Errorf("flush for ada: %w", refused),
		"an unreachable store":       fmt.Errorf("%w: lost every compare-and-swap", coord.ErrUnavailable),
	} {
		if got := coord.ReadStateSentence(err); got != err.Error() {
			t.Errorf("%s reads %q, want its whole message %q", name, got, err.Error())
		}
	}
}

// A RECORD OVER THE MUTE CAP STILL TAKES EVERY FLUSH THAT DOES NOT GROW IT.
//
// The merge refuses a mute that would take the set past the cap, and checks
// the set the flush would LEAVE — so a set already over it, which no flush
// through this merge can write but a record kept from a build with a larger
// cap would hold, must still take a cursor and an unmute. Checked against the
// size alone, every flush to that record would be refused for good, its
// cursors frozen and the unmute that would bring it back under refused with
// them.
func TestARecordOverTheMuteCapTakesEveryFlushThatDoesNotGrowIt(t *testing.T) {
	t.Parallel()
	over := make([]string, coord.MaxReadCursors+8)
	for i := range over {
		over[i] = fmt.Sprintf("room-%03d", i)
	}
	stored := coord.ChatReadState{Muted: over}

	for name, delta := range map[string]coord.ChatReadDelta{
		"a cursor":             {Cursors: map[string]int64{"eng": 4}},
		"an unmute":            {Unmute: []string{"room-000"}},
		"a room already muted": {Mute: []string{"room-001"}},
	} {
		if _, err := coord.MergeChatRead(stored, delta); err != nil {
			t.Errorf("%s to a record over the cap was refused: %v", name, err)
		}
	}
	_, err := coord.MergeChatRead(stored, coord.ChatReadDelta{Mute: []string{"one-more"}})
	if !errors.Is(err, coord.ErrInvalidReadState) {
		t.Errorf("a mute that grows a record already over the cap answered %v, "+
			"want the sentinel", err)
	}
}

// A READ STATE HANDED TO A CLIENT SAYS WHOSE IT IS, beside the fields it
// always carried — at the top level, where a client reads them, so naming the
// person moved nothing a reader already finds.
func TestAPersonsReadStateNamesThePersonBesideItsFields(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(coord.PersonChatRead{
		Handle: "ana",
		ChatReadState: coord.ChatReadState{
			Cursors: map[string]int64{"room-a": 7}, Muted: []string{"room-b"}, Revision: 41,
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"handle":"ana","cursors":{"room-a":7},"muted":["room-b"],"revision":41}`
	if got := string(raw); got != want {
		t.Fatalf("a person's read state encodes as %s, want %s", got, want)
	}
}
