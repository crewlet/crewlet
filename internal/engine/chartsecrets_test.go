package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/envref"
)

// A VALUE A CHART WRITE SEALS RESOLVES ON THE NODE THAT APPLIES IT — AND SO
// DOES THE NEXT ONE SEALED OVER IT.
//
// A node resolves `${VAR}` from a snapshot of the secret store taken at boot,
// at an apply and after a provisioning pass, and a chart write is none of
// those. Before the view rebuild re-read what its rows name, a seat hired with
// an address and a token resolved both to nothing until somebody re-activated
// a config, and a token rotated through the chart kept resolving to the old
// one — the re-seal writes the same name, so the reference on the row never
// changed and nothing compared could see it.
func TestAValueAChartWriteSealsResolvesOnTheNodeThatAppliesIt(t *testing.T) {
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
	give("test:content:cfo:1", "first-token")
	resolved := func() (email, token string) {
		t.Helper()
		if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		role := e.Company().Org.Role("cfo")
		if role == nil {
			return "", ""
		}
		ref := role.MCPEnv["tracker"]["SEAT_TOKEN"]
		if name, whole := envref.Whole(ref); !whole || !chart.OwnsSecret(name) {
			t.Fatalf("the seat's token reached the view as %q, want a sealed reference", ref)
		}
		return e.Resolve(role.Email), e.Resolve(ref)
	}
	waitFor := func(wantToken string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			email, token := resolved()
			if email == "cfo@example.com" && token == wantToken {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the seat resolves (%q, %q) on the node that applied "+
					"its write, want (cfo@example.com, %q) — a value sealed after "+
					"this node's snapshot must be read before a seat is built "+
					"from the rows that name it", email, token, wantToken)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitFor("first-token")

	// THE ROTATION: the same field, a new literal, the same sealed name.
	give("test:content:cfo:2", "second-token")
	waitFor("second-token")
}
