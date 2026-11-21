package eventfan

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/store"
)

// A TURN PAST THE TRANSPORT IS CUT FROM ITS HEAD, THEN ITS ENDING'S OLDEST
// ROWS, NEVER REFUSED: an ending that fits is kept whole beside no head, and
// one that alone passes the transport keeps its newest rows, rather than the
// reply being ErrTooLarge.
//
// Mutations, each red: never cut the ending (one that passes the transport is
// refused); count only the head as cuttable (an ending that fits is cut).
func TestATurnPastTheTransportIsCutNotRefused(t *testing.T) {
	t.Parallel()
	row := func(id string, mib int) store.EventRecord {
		return store.EventRecord{ID: id, Type: "agent_turn_progress",
			Payload: json.RawMessage(`{"text":"` + strings.Repeat("x", mib<<20) + `"}`)}
	}
	rows := func(prefix string, n, mib int) []store.EventRecord {
		var out []store.EventRecord
		for i := range n {
			out = append(out, row(fmt.Sprintf("%s-%d", prefix, i), mib))
		}
		return out
	}
	for _, tc := range []struct {
		name          string
		head, closing []store.EventRecord
		want          []string
	}{
		{"an ending past the transport", rows("head", 2, 3), rows("end", 3, 3), []string{"end-1", "end-2"}},
		{"an ending that fits", rows("head", 1, 3), rows("end", 3, 2), []string{"end-0", "end-1", "end-2"}},
	} {
		part := turnPart{Head: tc.head, Closing: tc.closing, Total: len(tc.head) + len(tc.closing) + 4,
			Traces: []store.TurnTrace{}, Nodes: []string{}}
		out, err := fit("node-b", part, queue.MaxPayloadBytes, Protocol)
		if err != nil {
			t.Fatalf("%s: fit: %v", tc.name, err)
		}
		var r reply
		var got turnPart
		if err := json.Unmarshal(out, &r); err != nil || r.Error != "" || json.Unmarshal(r.Answer, &got) != nil {
			t.Fatalf("%s: the reply is %q (error %q, %v), want a cut turn", tc.name, out[:min(len(out), 200)],
				r.Error, err)
		}
		if len(got.Head) != 0 || !slices.Equal(rowIDs(got.Closing), tc.want) {
			t.Errorf("%s: the cut turn kept head %d rows and ending %v, want no head and %v", tc.name,
				len(got.Head), rowIDs(got.Closing), tc.want)
		}
	}
}

func rowIDs(rows []store.EventRecord) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}
