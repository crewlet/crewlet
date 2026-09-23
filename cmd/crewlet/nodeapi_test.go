package main

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/logging"
)

// `GET /iam/check` NAMES A DANGLING BINDING THROUGH THE SEAM THIS NODE WIRES,
// not through one a case builds.
//
// The directory report asks [danglingBindings] whether a person's seat binding
// dangles, and that seam is the one place the engine's rule — the request
// path's own seat table, which the `iam_binding_dangling` alarm asks too — is
// connected to the report. internal/api/iamapi's suite hands the report a
// function of its own, so a node whose wiring passed nil, or a seam asking a
// narrower question, left every suite green while `crewlet iam check` said
// nothing about a person refused on every request. So this case boots a node,
// serves its API the way `crewlet run` does, binds a machine to an AGENT's
// seat — the residue the narrower question used to miss — and reads the
// report over HTTP.
func TestTheCheckNamesADanglingBindingThroughTheNodesOwnWiring(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := testEngine(t)
	boot := bootstrapFor(t, 0)
	boot.API.Port = freePort(t)
	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serve the node's API: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	base := "http://127.0.0.1:" + strconv.Itoa(boot.API.Port)

	// THE CONTROL FIRST: nobody is bound to anything, so the report has no
	// binding to name — which is what makes the finding below the binding's
	// rather than something the fixture always says.
	if got := danglingFindings(t, base); len(got) != 0 {
		t.Fatalf("a directory with no binding in it reports %v", got)
	}

	bot := uuid.Must(uuid.NewV7()).String()
	writer := e.IAMWriter()
	if writer == nil {
		t.Fatal("the node runs no identity domain, so this case proves nothing")
	}
	if _, err := writer.Enrol(ctx, iamdomain.Enrolment{
		PersonID: bot, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "ci:bot", OpID: "op-enrol-bot", Reason: "a pipeline",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	// THE CEO'S SEAT IS AN AGENT'S, which the request path refuses a
	// person on every request for.
	if _, err := writer.Claim(ctx, iamdomain.KindSeat, "ceo", bot, "op-bind-bot"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, f := range danglingFindings(t, base) {
			if f["person"] == bot && f["seat"] == "ceo" && f["detail"] != "" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /iam/check never named %s's binding to the agent seat "+
				"ceo; it reports %v", bot, danglingFindings(t, base))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// danglingFindings is every `binding_dangling` row the node's report carries.
func danglingFindings(t *testing.T, base string) []map[string]any {
	t.Helper()
	body := getJSON(t, base+"/iam/check")
	// A REFUSAL HAS NO FINDINGS EITHER, so the report's own position is
	// what says this was a report at all: without it every "names nothing"
	// above would be a 401 or a 503 read as a clean directory.
	if _, reported := body["position"]; !reported {
		t.Fatalf("GET /iam/check answered %v, which is not a report", body)
	}
	rows, _ := body["findings"].([]any)
	var out []map[string]any
	for _, row := range rows {
		fields, _ := row.(map[string]any)
		if fields["kind"] == "binding_dangling" {
			out = append(out, fields)
		}
	}
	return out
}
