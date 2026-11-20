package queries

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// A PARK STRETCH IS READ ONCE, HOWEVER OFTEN IT WAS RENEWED: a seat's parks
// are published again at every probe while the limit stands, so a stretch of
// hundreds of records reads one record whole, the first, for the entry its
// chain headed; a stretch still standing runs to the window's end. Mutation:
// read every park's payload, and a day on a limit reads hundreds.
func TestAParkStretchIsReadOnceHoweverOftenItWasRenewed(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 14, 9, 0, 0, 0, time.UTC)
	since := now.AddDate(0, 0, -7)
	seat := uuid.New()
	parked, released := types.SeatProviderParked{}.EventType(), types.SeatProviderReleased{}.EventType()
	rows := map[string][]store.EventRecord{}
	payloads := map[string]json.RawMessage{}
	park := func(id string, at time.Time, entry string) {
		rows[parked] = append(rows[parked], store.EventRecord{ID: id, AgentID: seat.String(), Time: at})
		raw, err := json.Marshal(types.SeatProviderParked{ProviderKeys: []string{entry, "spare"}})
		if err != nil {
			t.Fatal(err)
		}
		payloads[id] = raw
	}
	first := since.Add(24 * time.Hour)
	for i := range 300 {
		park(fmt.Sprintf("a-%03d", i), first.Add(time.Duration(i)*time.Minute), "main")
	}
	rows[released] = append(rows[released], store.EventRecord{ID: "r-1", AgentID: seat.String(),
		Time: first.Add(10 * time.Hour)})
	second := first.Add(48 * time.Hour)
	for i := range 200 {
		park(fmt.Sprintf("b-%03d", i), second.Add(time.Duration(i)*time.Minute), "backup")
	}
	log := newPagedLog(rows)
	log.payloads = payloads
	reads := 0
	s := Sources{Events: countingLog{pagedLog: log, reads: &reads}}
	spans, err := s.parkSpans(t.Context(), nil, since, now)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 2 {
		t.Errorf("read %d parks whole, want one for each of the two stretches", reads)
	}
	if len(spans) != 2 {
		t.Fatalf("spans = %+v, want two stretches", spans)
	}
	got := map[string][2]time.Time{}
	for _, sp := range spans {
		got[sp.Entry] = [2]time.Time{sp.From, sp.To}
	}
	if got["main"] != [2]time.Time{first, first.Add(10 * time.Hour)} || got["backup"] != [2]time.Time{second, now} {
		t.Errorf("stretches = %+v, want main for ten hours and backup to the window's end", got)
	}
}
