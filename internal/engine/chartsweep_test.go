package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
)

// A SEALED VALUE NOTHING NAMES ANY MORE IS COLLECTED — AND ONLY THAT ONE.
//
// Replacing a sealed token with the operator's own reference leaves the value
// in the store, which has no retention of its own: before the sweep it
// outlived the company. It goes once the grace has passed, and the values the
// rows still name — the same seat's address, a colleague's token — stay.
func TestTheSweepCollectsASealedValueNothingNames(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})
	readChart(t, e)
	writer := e.ChartWriter()
	if _, err := writer.WriteBatch(t.Context(), "test:hire:cfo", chart.Batch{
		Operations: []chart.Operation{{Kind: chart.OpCreateSeat,
			Object:   chart.ObjectRef{Kind: chart.KindSeat, ID: "cfo"},
			SeatKind: chart.SeatAgent}},
	}); err != nil {
		t.Fatalf("hire: %v", err)
	}
	give := func(opID, token string) {
		t.Helper()
		if _, err := writer.WriteSeat(t.Context(), opID, chart.SeatContent{
			Handle: "cfo", Name: "CFO", Email: "cfo@example.com",
			Runtime: json.RawMessage(`{"llm":["zulu"],"mcp_env":{"tracker":` +
				`{"SEAT_TOKEN":"` + token + `"}}}`),
		}); err != nil {
			t.Fatalf("write the seat: %v", err)
		}
	}
	give("test:content:cfo:1", "cfo-token")
	cfo := chart.ObjectRef{Kind: chart.KindSeat, ID: "cfo"}
	token := chart.SecretName(cfo, "mcp_env", "tracker", "SEAT_TOKEN")
	email := chart.SecretName(cfo, "email")
	dev := chart.SecretName(chart.ObjectRef{Kind: chart.KindSeat, ID: "dev"},
		"mcp_env", "tracker", "SEAT_TOKEN")
	store := fleetsecrets.New(e.Backends().Fleet, nil)
	stored := func(name string) bool {
		t.Helper()
		_, found, err := store.Describe(t.Context(), name)
		if err != nil {
			t.Fatalf("describe %s: %v", name, err)
		}
		return found
	}
	for _, name := range []string{token, email, dev} {
		if !stored(name) {
			t.Fatalf("%s is not in the store before anything was cleared", name)
		}
	}

	give("test:content:cfo:2", "${CFO_TOKEN}")
	collect := func(at time.Time) int64 {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			// THE ROWS MUST HOLD THE WHOLE LOG before the sweep may judge
			// anything, so a case that asked before this node applied
			// the second write would be asking a sweep that rightly
			// declines.
			if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			n, err := engine.CollectChartSealsForTest(t.Context(), e, at)
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			if n > 0 || time.Now().After(deadline) {
				return n
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// INSIDE THE GRACE nothing goes: a value sealed a moment ago may be one
	// whose record has not landed yet.
	if n, err := engine.CollectChartSealsForTest(t.Context(), e, time.Now()); err != nil || n != 0 {
		t.Fatalf("the sweep inside the grace = (%d, %v), want nothing collected", n, err)
	}
	if n := collect(time.Now().Add(2 * chart.SealGrace)); n != 1 {
		t.Errorf("the sweep past the grace collected %d values, want the one "+
			"token nothing names", n)
	}
	if stored(token) {
		t.Errorf("%s is still in the store with nothing naming it", token)
	}
	for _, name := range []string{email, dev} {
		if !stored(name) {
			t.Errorf("the sweep deleted %s, which a row still names", name)
		}
	}
}
