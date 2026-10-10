package queries_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ACTIVITY VIEWS' ROWS ARE THE FEED'S — the listing and its axis alike.
//
// The live ring holds no accounting row ([events.KeptOutOfFeed]), and the event
// log, the Live strip and a seat's latest events are each drawn over it or
// scrolled on from it. Asked without `feed_only`, an older page listed every
// auxiliary_spend record the ring left out and the axis above it counted
// them, so a bar claimed rows no list showed and a seat's last few events
// were its spend records. With it, both leave out EVERY unfed type; without
// it, every stored row is still listed — a turn's or a type's own read wants
// them. A word that is neither is refused.
//
// Mutation: drop the `feed_only` reader, and both answers count the spend.
func TestTheActivityViewsAskForTheFeedsRowsAlone(t *testing.T) {
	t.Parallel()
	unfed := events.Unfed()
	if len(unfed) == 0 {
		t.Fatal("no type is kept out of the feed — the case asserts nothing")
	}
	log := openStore(t).Events()
	at := time.Now().UTC().Add(-20 * time.Minute)
	stored := []store.EventRecord{
		{ID: "turn", Type: "agent_turn_completed", Category: "system"},
		{ID: "phase", Type: "agent_phase_completed", Category: "task"},
	}
	for _, typ := range unfed {
		stored = append(stored, store.EventRecord{ID: "unfed-" + typ, Type: typ, Category: "system"})
	}
	for i, rec := range stored {
		rec.Source, rec.Time, rec.Payload = "engine", at.Add(time.Duration(i)*time.Second), []byte(`{}`)
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatal(err)
		}
	}
	r := registryOver(t, queries.Sources{Events: fleetOf(log)})

	if got := eventIDs(t, r, map[string]any{"feed_only": "true"}); !slices.Equal(got, []string{"phase", "turn"}) {
		t.Errorf("feed-only events = %v, want every row but the unfed ones", got)
	}
	series := askRaw(t, r, "event_series", map[string]any{"feed_only": "true", "bucket": "hour"}).(queries.SeriesAnswer)
	if series.Total != 2 {
		t.Errorf("the feed-only axis counts %d, the listing shows 2", series.Total)
	}
	if got := eventIDs(t, r, map[string]any{}); len(got) != len(stored) {
		t.Errorf("events = %v, want every stored row when the feed is not asked for", got)
	}
	whole := askRaw(t, r, "event_series", map[string]any{"bucket": "hour"}).(queries.SeriesAnswer)
	if whole.Total != len(stored) {
		t.Errorf("the axis counts %d, want every stored row (%d)", whole.Total, len(stored))
	}
	if _, err := r.Answer(t.Context(), "events", map[string]any{"feed_only": "yes"}, asAdmin("ops")); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("feed_only=yes: err %v, want bad params", err)
	}
}
