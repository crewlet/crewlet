package queries

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/insights"
)

// THE INSIGHTS READ SAYS THE CONSENT, THE SCHEMA AND EVERY RECORD KEPT, with when
// each is due.
func TestTheInsightsReadSaysTheConsentAndEveryRecord(t *testing.T) {
	t.Parallel()
	store := memory.NewFleet()
	due := time.Date(2030, 1, 8, 15, 0, 0, 0, time.UTC)
	if _, _, err := store.ClaimInsightRecord(t.Context(), coord.InsightRecord{Class: "wake.noop_share",
		Week: "2030-W02", Status: coord.InsightScheduled, SendAt: due, Payload: []byte(`{"schema":1}`)},
		func(coord.InsightRecord) bool { return true }); err != nil {
		t.Fatal(err)
	}
	records, err := store.ListInsightRecords(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := insightsAnswerOf(config.InsightsSharing{}, records)
	if err != nil {
		t.Fatal(err)
	}
	if answer.Share != "" || answer.Schema != insights.SchemaVersion || answer.Relay != insights.RelayURL ||
		len(answer.Catalogue) == 0 {
		t.Errorf("the read is %+v, want no consent and this build's schema and catalogue", answer)
	}
	if len(answer.Records) != 1 || answer.Records[0].Status != coord.InsightScheduled ||
		answer.Records[0].SendAt == nil || !answer.Records[0].SendAt.Equal(due) {
		t.Errorf("the records are %+v, want the scheduled one with its time", answer.Records)
	}
}
