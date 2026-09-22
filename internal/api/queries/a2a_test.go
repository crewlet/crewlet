// The agent-to-agent channel record, read by state and by seat.

package queries_test

import (
	"errors"
	"fmt"
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
	got := answeredTranscripts(t, queries.Sources{Channels: f}, "a2a_channels", params)
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

	// OPEN BY DEFAULT, which is what already shipped.
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
	_, err := askTranscripts(t, queries.Sources{Channels: channelFleet(t)}, "a2a_channels",
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
	raw, err := askTranscripts(t, queries.Sources{Channels: f}, "a2a_channels", nil)
	got := answerMap(t, raw, err)
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
	short := answeredTranscripts(t, queries.Sources{Channels: channelFleet(t)}, "a2a_channels", nil)
	if short["truncated"] != false {
		t.Errorf("truncated = %#v on a company with two open channels",
			short["truncated"])
	}
}
