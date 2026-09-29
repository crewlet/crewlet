package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/sandbox"
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

// A SEAT'S SETUP FILE REACHES ITS BOX AS THE BODY SOMEBODY WROTE.
//
// A file is a credential the chart seals, and it is CONTENT: nothing on the
// way to a box expands a file, so a seat's `.npmrc` sealed into references —
// or sealed whole and never read back — reached the box as the text of the
// chart's own `${CHART_…}` names, and the registry refused every install with
// nothing anywhere saying why. What the launch hands over is the body as
// written, its own `${NPM_TOKEN}` and `${HOME}` left for the box's npm and
// shell; a provider-wide file is read by the same rule, and the manager's own
// steps are never written through.
func TestASeatsSetupFileReachesTheBoxAsWritten(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})
	readChart(t, e)
	writer := e.ChartWriter()
	if _, err := writer.WriteBatch(t.Context(), "test:hire:builder", chart.Batch{
		Operations: []chart.Operation{{Kind: chart.OpCreateSeat,
			Object:   chart.ObjectRef{Kind: chart.KindSeat, ID: "builder"},
			SeatKind: chart.SeatAgent}},
	}); err != nil {
		t.Fatalf("hire: %v", err)
	}
	const (
		npmrc  = "registry=https://r.example.com\n//r.example.com/:_authToken=${NPM_TOKEN}\n"
		helper = "#!/bin/sh\n[ \"$1\" = get ] || exit 0\necho \"password=${GIT_TOKEN}\"\n"
	)
	runtime, err := json.Marshal(map[string]any{
		"llm": []string{"zulu"},
		"sandbox": map[string]any{"enabled": true, "setup": []any{map[string]any{
			"name": "registry",
			"files": map[string]string{
				"/root/.npmrc":     npmrc,
				"/usr/local/bin/h": helper,
			},
			"env": map[string]string{"NPM_TOKEN": "npm-literal-token"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteSeat(t.Context(), "test:content:builder", chart.SeatContent{
		Handle: "builder", Name: "Builder", Runtime: runtime,
	}); err != nil {
		t.Fatalf("write the seat: %v", err)
	}

	provider := []sandbox.SetupStep{{Name: "shared", Files: map[string]string{
		"/etc/plain":  "home is ${HOME}\n",
		"/etc/sealed": "${SHARED_FILE_UNSET}",
	}}}
	var steps []sandbox.SetupStep
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		steps = engine.SeatBoxSetupForTest(e, provider, "builder")
		if len(steps) == 2 && steps[1].Files["/root/.npmrc"] == npmrc {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the seat's box is given %+v, want the provider's step and "+
				"then the seat's own, each file the body that was written — a "+
				"file sealed by the chart must be read back before a box is "+
				"provisioned with it", steps)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := steps[1].Files["/usr/local/bin/h"]; got != helper {
		t.Errorf("the helper script reaches the box as %q, want %q", got, helper)
	}
	role := e.Company().Org.Role("builder")
	for path, body := range role.Sandbox.Setup[0].Files {
		if name, whole := envref.Whole(body); !whole || !chart.OwnsSecret(name) {
			t.Errorf("%s reached the running seat as %q, want ONE sealed "+
				"reference — a file cut around its own ${…} is the chart's "+
				"references strung together", path, body)
		}
	}
	if got := steps[0].Files["/etc/plain"]; got != "home is ${HOME}\n" {
		t.Errorf("a provider-wide file was expanded to %q — a ${…} inside a "+
			"body is the box's, never the engine host's", got)
	}
	if got, held := steps[0].Files["/etc/sealed"]; !held || got != "" {
		t.Errorf("a file naming a variable nothing answers for reaches the "+
			"box as (%q, %v), want present and empty", got, held)
	}
	if provider[0].Files["/etc/sealed"] != "${SHARED_FILE_UNSET}" {
		t.Errorf("reading the provider's step wrote through to the manager's "+
			"own copy: %v", provider[0].Files)
	}
}
