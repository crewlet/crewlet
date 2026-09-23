// The agent-to-agent channel record, read by state and by seat.

package queries_test

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
)

// channelFleet seeds the coordination twin the engine itself uses — the same
// implementation, held to the same contract suite, rather than a stand-in.
func channelFleet(t *testing.T) *coordmemory.Fleet {
	t.Helper()
	f := coordmemory.NewFleet()
	at := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	open := func(id, requester, target string, minute int) {
		t.Helper()
		when := at.Add(time.Duration(minute) * time.Minute)
		if err := f.OpenChannel(t.Context(), coord.Channel{
			ID: id, Requester: requester, Target: target,
			OpenedAt: when, LastAt: when,
		}); err != nil {
			t.Fatalf("OpenChannel %s: %v", id, err)
		}
	}
	open("c-1", "agent-pm", "agent-swe", 1)
	open("c-2", "agent-swe", "agent-cto", 2)
	open("c-3", "agent-pm", "agent-cto", 3)
	if _, _, err := f.CloseChannel(t.Context(), "c-3", at.Add(10*time.Minute)); err != nil {
		t.Fatalf("CloseChannel: %v", err)
	}
	return f
}

func channelIDs(t *testing.T, params map[string]any, f *coordmemory.Fleet) []string {
	t.Helper()
	got := answeredMap(t, queries.Sources{Channels: f}, "a2a_channels", params)
	out := []string{}
	for _, row := range channelRows(t, got) {
		out = append(out, fmt.Sprint(row["id"]))
	}
	slices.Sort(out)
	return out
}

// channelRows reads the answer's own slice. NOT the shared `rows` helper: that
// one reads a wire round trip, and this surface is exercised in process, where
// the value is still the map the answer built.
func channelRows(t *testing.T, answer map[string]any) []map[string]any {
	t.Helper()
	list, ok := answer["channels"].([]map[string]any)
	if !ok {
		t.Fatalf("channels = %T, want the answer's own slice", answer["channels"])
	}
	return list
}

// A CLOSED CHANNEL IS THE RECORD, and it was unreachable.
//
// The answer read `OpenChannels`, which is the idle SWEEP's listing and
// deliberately narrower: a closed channel re-reported is a second close for
// one channel. So the authorization record the fleet keeps until its purge
// horizon — who asked whom, how many messages crossed, when it closed — could
// be reached only through the event log, one event at a time.
func TestTheChannelRecordIsReadableClosedOnesIncluded(t *testing.T) {
	t.Parallel()
	f := channelFleet(t)

	// OPEN BY DEFAULT, for a listing.
	if got := channelIDs(t, nil, f); !slices.Equal(got, []string{"c-1", "c-2"}) {
		t.Errorf("the default gave %v, want the open channels", got)
	}
	if got := channelIDs(t, map[string]any{"state": "closed"}, f); !slices.Equal(got,
		[]string{"c-3"}) {

		t.Errorf("state=closed gave %v", got)
	}
	if got := channelIDs(t, map[string]any{"state": "all"}, f); !slices.Equal(got,
		[]string{"c-1", "c-2", "c-3"}) {

		t.Errorf("state=all gave %v, want every channel the fleet holds", got)
	}
}

// A SEAT MATCHES AT EITHER END, because a seat's page asks one question — what
// did this seat ask, and what was it asked — and splitting that into two
// parameters would make the common case two reads.
func TestASeatsChannelsAreTheOnesAtEitherEnd(t *testing.T) {
	t.Parallel()
	f := channelFleet(t)
	got := channelIDs(t, map[string]any{"seat": "agent-cto", "state": "all"}, f)
	if !slices.Equal(got, []string{"c-2", "c-3"}) {
		t.Errorf("agent-cto's channels = %v, want the one it was asked on and "+
			"the one it was asked on and closed", got)
	}
	if got := channelIDs(t, map[string]any{"seat": "nobody", "state": "all"}, f); len(got) != 0 {
		t.Errorf("a seat in no channel gave %v", got)
	}
}

// A STATE NOBODY DEFINES IS REFUSED NAMING THE THREE, rather than silently
// answering the default under somebody else's heading.
func TestAnUnknownChannelStateIsRefusedNamingTheSets(t *testing.T) {
	t.Parallel()
	_, err := askNative(t, queries.Sources{Channels: channelFleet(t)}, "a2a_channels",
		map[string]any{"state": "pending"})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("state=pending answered %v, want bad params", err)
	}
	if !strings.Contains(err.Error(), "open") || !strings.Contains(err.Error(), "closed") {
		t.Errorf("the refusal is %q and does not name what would have worked", err)
	}
}

// THE PAGE IS BOUNDED AND SAYS WHEN IT CUT. The closed set is what makes a
// bound necessary — it is kept until the purge horizon, so a busy company's
// history is thousands of rows — and a page that filled is otherwise
// indistinguishable from a company with exactly that many channels.
func TestTheChannelPageIsBoundedAndSaysSo(t *testing.T) {
	t.Parallel()
	f := coordmemory.NewFleet()
	at := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	for i := range queries.MaxA2AChannels + 5 {
		when := at.Add(time.Duration(i) * time.Second)
		if err := f.OpenChannel(t.Context(), coord.Channel{
			ID: fmt.Sprintf("c-%04d", i), Requester: "agent-pm", Target: "agent-swe",
			OpenedAt: when, LastAt: when,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got := answeredMap(t, queries.Sources{Channels: f}, "a2a_channels", nil)
	list := channelRows(t, got)
	if len(list) != queries.MaxA2AChannels {
		t.Errorf("the page carries %d channels, want the cap %d",
			len(list), queries.MaxA2AChannels)
	}
	if got["truncated"] != true {
		t.Errorf("truncated = %#v on a page that filled", got["truncated"])
	}
	// AND A PAGE THAT DID NOT FILL SAYS SO TOO, which is what stops the
	// flag being a check that the field exists.
	short := answeredMap(t, queries.Sources{Channels: channelFleet(t)}, "a2a_channels", nil)
	if short["truncated"] != false {
		t.Errorf("truncated = %#v on a company with two open channels",
			short["truncated"])
	}
}

// openAt records one channel with every field a listing reads.
func openAt(t *testing.T, f *coordmemory.Fleet, c coord.Channel) {
	t.Helper()
	if err := f.OpenChannel(t.Context(), c); err != nil {
		t.Fatalf("OpenChannel %s: %v", c.ID, err)
	}
}

// THE ORDER IS THE INSTANT, NOT ITS RENDERING.
//
// RFC 3339 with fractional seconds does not sort as text: "09:00:00.5Z" is
// lexically before "09:00:00Z", because '.' is before 'Z'. Sorted on the wire
// strings, the channel active half a second LATER was listed behind the one
// that was not.
func TestTheChannelOrderIsTheInstantNotItsText(t *testing.T) {
	t.Parallel()
	f := coordmemory.NewFleet()
	whole := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	openAt(t, f, coord.Channel{ID: "whole", Requester: "a", Target: "b", OpenedAt: whole, LastAt: whole})
	half := whole.Add(500 * time.Millisecond)
	openAt(t, f, coord.Channel{ID: "half", Requester: "a", Target: "b", OpenedAt: half, LastAt: half})

	list := channelRows(t, answeredMap(t, queries.Sources{Channels: f}, "a2a_channels", nil))
	if len(list) != 2 || list[0]["id"] != "half" || list[1]["id"] != "whole" {
		t.Errorf("order = %v, want the half-second-later channel first", list)
	}
}

// THE REST OF THE RECORD IS THE NEXT PAGE, and paging reaches every channel
// exactly once.
//
// A cut page used to be the end of what anybody could read: `truncated` said
// there was more, and nothing said where. Several channels share each instant
// here, which is the case a cursor without a tiebreak gets wrong — two rows at
// one instant can each land on either side of a page boundary, and one of
// them on neither.
func TestTheChannelRecordPagesToItsEndReachingEveryChannelOnce(t *testing.T) {
	t.Parallel()
	f := coordmemory.NewFleet()
	at := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	const held = 2*queries.MaxA2AChannels + 5
	for i := range held {
		// THREE CHANNELS PER INSTANT, so every page boundary is inside a tie.
		when := at.Add(time.Duration(i/3) * time.Second)
		openAt(t, f, coord.Channel{ID: fmt.Sprintf("c-%04d", i), Requester: "a", Target: "b",
			OpenedAt: when, LastAt: when})
	}

	seen := map[string]int{}
	params := map[string]any{}
	for pages := 0; ; pages++ {
		if pages > held {
			t.Fatal("the cursor never reached the end of the record")
		}
		got := answeredMap(t, queries.Sources{Channels: f}, "a2a_channels", params)
		list := channelRows(t, got)
		if len(list) > queries.MaxA2AChannels {
			t.Fatalf("a page of %d, past the cap %d", len(list), queries.MaxA2AChannels)
		}
		for _, row := range list {
			seen[fmt.Sprint(row["id"])]++
		}
		next, _ := got["next"].(map[string]string)
		if got["truncated"] != (len(next) > 0) {
			t.Fatalf("truncated = %v beside next = %v; a cut page must say where "+
				"the rest is, and a whole one must not offer more", got["truncated"], next)
		}
		if len(next) == 0 {
			break
		}
		params = map[string]any{"before_time": next["before_time"], "before_id": next["before_id"]}
	}
	if len(seen) != held {
		t.Errorf("paging reached %d of %d channels", len(seen), held)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("channel %s was listed %d times", id, n)
		}
	}
}

// THE TOTALS DESCRIBE THE RECORD, NOT THE PAGE.
//
// The whole record is read to answer a page anyway, so a summary a screen
// draws — open channels, messages, pairs — is counted over every channel the
// filters matched. Counted from the rows instead, it describes the newest two
// hundred and changes as a reader pages.
func TestChannelTotalsDescribeTheMatchedRecordNotThePage(t *testing.T) {
	t.Parallel()
	f := coordmemory.NewFleet()
	at := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	const held = queries.MaxA2AChannels + 50
	wantMessages, wantOpen := 0, 0
	for i := range held {
		when := at.Add(time.Duration(i) * time.Second)
		c := coord.Channel{
			ID:        fmt.Sprintf("c-%04d", i),
			Requester: fmt.Sprintf("asker-%d", i%4), Target: "b",
			Messages: 1 + i%3, OpenedAt: when, LastAt: when,
		}
		// The OLDEST are open: the ones the page cuts off.
		if i >= 10 {
			c.ClosedAt = when.Add(time.Minute)
		} else {
			wantOpen++
		}
		wantMessages += c.Messages
		openAt(t, f, c)
	}
	src := queries.Sources{Channels: f}
	first := answeredMap(t, src, "a2a_channels", map[string]any{"state": "all"})
	want := map[string]int{"channels": held, "open": wantOpen, "messages": wantMessages, "pairs": 4}
	if got, _ := first["totals"].(map[string]int); !maps.Equal(got, want) {
		t.Errorf("totals = %v, want %v over every matched channel", got, want)
	}
	next, _ := first["next"].(map[string]string)
	second := answeredMap(t, src, "a2a_channels", map[string]any{
		"state": "all", "before_time": next["before_time"], "before_id": next["before_id"],
	})
	if !maps.Equal(second["totals"].(map[string]int), want) {
		t.Errorf("the second page's totals are %v; totals must not move as a "+
			"reader pages", second["totals"])
	}
}

// ONE CHANNEL IS READABLE WHEREVER IT FALLS in the order, so a link to a
// channel does not stop resolving once two hundred newer ones exist — and
// whether or not it is still open. A lookup that took the listing's default
// state filtered a closed channel out, and an empty answer to "this channel"
// reads as there being no such channel.
func TestOneChannelIsReadableByIDPastThePage(t *testing.T) {
	t.Parallel()
	f := coordmemory.NewFleet()
	at := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	for i := range queries.MaxA2AChannels + 5 {
		when := at.Add(time.Duration(i) * time.Second)
		c := coord.Channel{ID: fmt.Sprintf("c-%04d", i), Requester: "a", Target: "b",
			OpenedAt: when, LastAt: when}
		// The one asked for is CLOSED, which the listing's default hides.
		if i == 0 {
			c.ClosedAt = when.Add(time.Minute)
		}
		openAt(t, f, c)
	}
	// c-0000 is the oldest, so it is on no first page.
	got := answeredMap(t, queries.Sources{Channels: f}, "a2a_channels",
		map[string]any{"id": "c-0000"})
	list := channelRows(t, got)
	if len(list) != 1 || list[0]["id"] != "c-0000" {
		t.Errorf("id=c-0000 answered %v, want that one closed channel", list)
	}
	if got["truncated"] != false {
		t.Errorf("truncated = %v on a one-channel answer", got["truncated"])
	}
	if got["state"] != "all" {
		t.Errorf("a lookup by id answered under state %v, want the whole record", got["state"])
	}

	// A STATE THE CALLER NAMES IS HONOURED: "is this channel open" is a
	// question too, and its answer here is no.
	got = answeredMap(t, queries.Sources{Channels: f}, "a2a_channels",
		map[string]any{"id": "c-0000", "state": "open"})
	if list := channelRows(t, got); len(list) != 0 {
		t.Errorf("id=c-0000 under state=open answered %v, want nothing — it is closed", list)
	}
}

// HALF A CURSOR IS REFUSED, in either direction, rather than read as "from the
// top": a client that sent one key without the other would be handed the first
// page again and page it for ever.
func TestAHalfChannelCursorIsRefused(t *testing.T) {
	t.Parallel()
	for _, params := range []map[string]any{
		{"before_id": "c-1"},
		{"before_time": "2026-07-01T09:03:00Z"},
	} {
		_, err := askNative(t, queries.Sources{Channels: channelFleet(t)}, "a2a_channels", params)
		if !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%v answered %v, want bad params", params, err)
		}
	}
}
