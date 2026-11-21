package store_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A BYTE-BOUNDED TURN READ STOPS BEFORE THE ROW PAST ITS BOUND, keeping the
// first row whatever it holds, and says it stopped; the turn's ending, read
// newest first, keeps its newest rows. An unbounded read (0) reads every row.
//
// Mutations, each red: count the bound past the row that crosses it; drop the
// first row when it alone passes the bound; report no cut; keep the ending's
// oldest rows.
func TestAByteBoundedTurnReadStopsBeforeTheRowPastItsBound(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC()
	for i := range 5 {
		id := fmt.Sprintf("tr-%d", i)
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: "agent_turn_progress", Source: "Lead", Category: "agent",
			Time: now.Add(time.Duration(i-5) * time.Second), Actor: "Lead",
			Tags:    map[string]string{"agent_id": "a-lead", "turn_id": "tn-bytes"},
			Payload: []byte(`{"text":"` + strings.Repeat("x", 1000) + `"}`),
		}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	all, cut, err := log.Turn(t.Context(), "tn-bytes", 0, time.Now())
	if err != nil || cut || len(all) != 5 {
		t.Fatalf("an unbounded turn read = %d rows, cut %v, %v; want all 5, uncut", len(all), cut, err)
	}
	two := store.RecordBytes(all[0]) + store.RecordBytes(all[1])
	for _, tc := range []struct {
		maxBytes int
		want     []string
	}{
		{two, []string{"tr-0", "tr-1"}},
		{two + store.RecordBytes(all[2]) - 1, []string{"tr-0", "tr-1"}},
		{1, []string{"tr-0"}},
	} {
		rows, cut, err := log.Turn(t.Context(), "tn-bytes", tc.maxBytes, time.Now())
		if err != nil || !cut || !slices.Equal(idsOf(rows), tc.want) {
			t.Errorf("a turn read within %d bytes = %v, cut %v, %v; want %v, cut", tc.maxBytes, idsOf(rows), cut,
				err, tc.want)
		}
	}
	ending, err := log.TurnClosing(t.Context(), "tn-bytes", 5, two, time.Now())
	if want := []string{"tr-3", "tr-4"}; err != nil || !slices.Equal(idsOf(ending), want) {
		t.Errorf("the turn's ending within %d bytes = %v, %v; want its newest rows %v", two, idsOf(ending), err,
			want)
	}
}

// A BYTE-BOUNDED PHASE PAGE SKIPS NO RECORD: the page reads each record type
// apart and merges them, so a type stopped at the bound leaves rows unread
// that may be newer than the other type's; the page ends at the newest row a
// stopped type read, says there is more, and paging on from it reads every
// record once, newest first.
//
// Mutations, each red: merge the stopped type's rows without ending at its
// last (the page skips a record); report no more past a page cut by bytes;
// hold the merged page to no byte bound.
func TestAByteBoundedPhasePageSkipsNoRecord(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC()
	var want []string
	for i := range 6 {
		id, kind, payload := fmt.Sprintf("ph-%d", i), "executor_collected", `{"turn_id":"t","report":"`+
			strings.Repeat("e", 1500)+`"}`
		if i%2 == 1 {
			kind, payload = "agent_phase_completed", `{"turn_id":"t","response":"`+strings.Repeat("r", 4000)+`"}`
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: kind, Source: "Lead", Category: "agent",
			Time: now.Add(time.Duration(i-6) * time.Second), Actor: "Lead",
			Tags:    map[string]string{"agent_id": "a-lead", "agent_role": "Lead", "turn_id": "t"},
			Payload: []byte(payload),
		}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
		want = append([]string{id}, want...)
	}
	whole, _, err := log.Phases(t.Context(), "", 0, 0, nil, time.Now())
	if err != nil || !slices.Equal(idsOf(whole), want) {
		t.Fatalf("the unbounded page = %v, %v; want %v", idsOf(whole), err, want)
	}
	// One and a half phase records: the phase type stops after its newest
	// row (ph-5), whose older records stay unread, and a later page holding
	// two launches beside a phase passes the bound with neither type stopped.
	bound := store.RecordBytes(whole[0]) + store.RecordBytes(whole[0])/2
	first, more, err := log.Phases(t.Context(), "", 0, bound, nil, time.Now())
	if err != nil || !more || !slices.Equal(idsOf(first), []string{"ph-5"}) {
		t.Errorf("the first page within %d bytes = %v, more %v, %v; want [ph-5], more", bound, idsOf(first), more,
			err)
	}
	var got []string
	var before *store.Cursor
	for range len(want) {
		page, more, err := log.Phases(t.Context(), "", 0, bound, before, time.Now())
		if err != nil || len(page) == 0 {
			t.Fatalf("a page after %v = %v, %v", got, idsOf(page), err)
		}
		held := 0
		for _, r := range page {
			held += store.RecordBytes(r)
		}
		if len(page) > 1 && held > bound {
			t.Errorf("a page holds %d bytes past its bound %d: %v", held, bound, idsOf(page))
		}
		got = append(got, idsOf(page)...)
		if !more {
			break
		}
		last := page[len(page)-1]
		before = &store.Cursor{Time: last.Time, ID: last.ID}
	}
	if !slices.Equal(got, want) {
		t.Errorf("paging within %d bytes read %v, want every record once, newest first: %v", bound, got, want)
	}
}
