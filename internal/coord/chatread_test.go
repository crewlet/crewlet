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

// A PERSON'S READ STATE CARRIES ONLY WHAT IS TRUE OF THEM. The record goes on
// the wire as stored, and the dashboard reads a present `dnd_until` as when a
// do-not-disturb ends; `omitempty` never omits a struct, so a zero instant
// would send everybody one long over. The zero state is pinned as bytes with
// no instant in them, and a set one round-trips.
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

// A REFUSED FLUSH READS AS ITS OWN SENTENCE: the operator catalogue's tool
// hands it to the model that made the call, so the package path and a doubled
// "refused" go from the merge's own refusal only — a caller's wrap is context,
// and any other error is not this sentinel's to tidy.
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
// The merge checks the set a flush would LEAVE, so a record kept from a build
// with a larger cap still takes a cursor and an unmute; checked by size alone,
// it would refuse every flush for good, the unmute that cures it included.
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
