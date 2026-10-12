package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// A RUNNING NODE PUBLISHES ITS OWN DAY, AND NAMES THE SEAT FROM THE CHART.
//
// The usage domain can be registered, provisioned, applied and certified with
// nothing ever writing to it — which reports as a healthy log over an empty
// stream, while every spend answer stays exactly as partial as it was. This is
// the wiring: a phase this node's own event log holds reaches the replicated
// rows through the node's own publisher, on the company's clock, with the
// seat's handle taken from the chart rather than from a record that may carry
// none.
//
// THE PHASE IS RECORDED AFTER BOOT, which is what makes this a case about a
// LIVE loop: the boot flush runs before it exists, so an engine that flushed
// once and let the publisher die — a loop bound to the start's context, a
// single Flush where Run belongs — publishes nothing here. The cadence is
// shortened so a later tick comes in milliseconds rather than fifteen
// seconds; what it is in production is the usage package's to pin.
func TestARunningNodePublishesItsOwnDay(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.WithUsageFlushEvery(engine.Options{}, 50*time.Millisecond))
	db := e.Backends().Store

	var seatID, handle string
	c := e.Company()
	for role := range c.Org.AllRoles() {
		if id, ok := c.Org.AgentIDFor(role); ok {
			seatID, handle = id.String(), role.Handle()
			break
		}
	}
	if seatID == "" {
		t.Fatal("the test company has no agent seat")
	}

	now := time.Now().UTC()
	payload, err := json.Marshal(map[string]any{
		"agent_id": seatID, "turn_id": "turn-usage", "phase": "execute",
		"model": "m-1", "provider_key": "primary",
		"input_tokens": 321, "output_tokens": 12, "total_tokens": 333,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Events().Append(t.Context(), store.EventRecord{
		ID: "usage-phase-1", Type: "agent_phase_completed", Category: "agent",
		Time: now, Tags: map[string]string{"agent_id": seatID, "turn_id": "turn-usage"},
		Payload: payload,
	}); err != nil {
		t.Fatalf("append a phase: %v", err)
	}

	day := period.At(period.Day, now, e.Zone()).Label
	// TEN SECONDS, two hundred of the shortened ticks: a bound only a loop
	// that never ticks again reaches.
	const patience = 10 * time.Second
	deadline := time.Now().Add(patience)
	for {
		rows, err := usage.Spend(t.Context(), db.Replicated(), usage.SpendQuery{From: day, To: day})
		if err != nil {
			t.Fatalf("read the spend: %v", err)
		}
		if len(rows) == 1 {
			r := rows[0]
			if r.AgentID != seatID || r.Input != 321 || r.Handle != handle || r.Node == "" {
				t.Fatalf("the node published %+v — want seat %s named %q at input 321, "+
					"under this node's id", r, seatID, handle)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s the replicated rows hold %+v for %s — nothing "+
				"publishes this node's day", patience, rows, day)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
