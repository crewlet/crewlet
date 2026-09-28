package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
)

// fakeFleetBroker is the broker-membership seam: a view it lists, the
// removals it was asked for, and the refusal it answers them with.
type fakeFleetBroker struct {
	view    engine.BrokerView
	listErr error
	removed []engine.BrokerRemoval
	refuse  error
}

func (f *fakeFleetBroker) List(context.Context) (engine.BrokerView, error) {
	return f.view, f.listErr
}

func (f *fakeFleetBroker) Remove(_ context.Context, req engine.BrokerRemoval) (engine.BrokerRemoved, error) {
	if f.refuse != nil {
		return engine.BrokerRemoved{}, f.refuse
	}
	f.removed = append(f.removed, req)
	return engine.BrokerRemoved{Node: req.Node, By: "node-a"}, nil
}

// getAs runs one authenticated read and returns the status and decoded body.
func getAs(t *testing.T, a *api.App, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: undecodable body %q: %v", path, rec.Body.String(), err)
	}
	return rec.Code, body
}

// removeBroker posts one removal and returns the status and the decoded body.
func removeBroker(t *testing.T, a *api.App, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("POST %s: undecodable body %q: %v", path, rec.Body.String(), err)
	}
	return rec.Code, body
}

// THE LISTING IS THE ENGINE'S VIEW, AND AN UNREADABLE ONE IS NEVER AN EMPTY
// ONE: a presence listing that failed answers 503 rather than a fleet with no
// nodes, which would read as a broker nobody is a member of.
func TestTheBrokerListingIsTheEnginesViewOrAFailure(t *testing.T) {
	t.Parallel()
	seam := &fakeFleetBroker{view: engine.BrokerView{Node: "node-a", Kind: "member",
		Nodes:    []engine.BrokerNode{{Node: "node-a", Kind: "member"}},
		Findings: []engine.BrokerFinding{{Kind: engine.BrokerDeadMember, Node: "node-c"}}}}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, FleetBroker: seam})
	if status, _ := get(t, a, "/fleet/broker"); status != http.StatusUnauthorized {
		t.Fatalf("an anonymous read of the broker's membership answered %d: it is the "+
			"deployment's shape, operator-only like the fleet view", status)
	}
	status, body := getAs(t, a, "/fleet/broker")
	if status != http.StatusOK || body["node"] != "node-a" {
		t.Fatalf("GET /fleet/broker = %d %v", status, body)
	}
	if findings, _ := body["findings"].([]any); len(findings) != 1 {
		t.Fatalf("the findings did not reach the answer: %v", body["findings"])
	}

	seam.listErr = fmt.Errorf("engine: list the live nodes: %w", coord.ErrUnavailable)
	status, body = getAs(t, a, "/fleet/broker")
	if status != http.StatusServiceUnavailable || body["error"] != "unavailable" {
		t.Fatalf("a lease table that could not be reached answered %d %v, want a "+
			"503 to ask again about", status, body)
	}

	// AND IT IS THE SOCKET'S QUESTION TOO: one implementation behind both.
	if _, err := a.Queries().AnswerWith(t.Context(), "fleet_broker", queries.Params{}, "operator"); err == nil ||
		!errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("the query channel answered %v", err)
	}
}

// A REMOVAL CONFIRMS THE NODE, AND EVERY REFUSAL SENDS THE OPERATOR SOMEWHERE
// OF ITS OWN: a member still running, a name the group does not list, a change
// in flight, a group with no leader, no member to carry it, an external
// cluster. Force reaches the engine only when asked for.
func TestABrokerRemovalConfirmsAndNamesEachRefusal(t *testing.T) {
	t.Parallel()
	seam := &fakeFleetBroker{}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, FleetBroker: seam})

	if status, body := removeBroker(t, a, "/fleet/broker/remove/node-c"); status != http.StatusBadRequest ||
		body["error"] != "confirm_required" {
		t.Fatalf("an unconfirmed removal answered %d %v", status, body)
	}
	if status, body := removeBroker(t, a, "/fleet/broker/remove/node-c?confirm=node-c&force=true"); status != http.StatusOK ||
		body["node"] != "node-c" || body["by"] != "node-a" {
		t.Fatalf("a confirmed removal answered %d %v", status, body)
	}
	if len(seam.removed) != 1 || !seam.removed[0].Force {
		t.Fatalf("the engine was asked %+v, want one forced removal", seam.removed)
	}
	if _, _ = removeBroker(t, a, "/fleet/broker/remove/node-d?confirm=node-d"); seam.removed[1].Force {
		t.Fatal("a removal nobody forced reached the engine forced")
	}

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{&engine.BrokerMemberLive{Node: "node-c"}, http.StatusConflict, "member_live"},
		{engine.ErrExternalBroker, http.StatusConflict, "external_broker"},
		{jetstream.ErrNotAMetaPeer, http.StatusNotFound, "not_a_member"},
		{jetstream.ErrMembershipChanging, http.StatusConflict, "membership_changing"},
		{jetstream.ErrNoMetaLeader, http.StatusServiceUnavailable, "no_leader"},
		{engine.ErrNoBrokerMember, http.StatusServiceUnavailable, "no_member"},
		{errors.New("something else"), http.StatusInternalServerError, "broker_remove_failed"},
	} {
		seam.refuse = tc.err
		status, body := removeBroker(t, a, "/fleet/broker/remove/node-c?confirm=node-c")
		if status != tc.status || body["error"] != tc.code || body["hint"] == "" {
			t.Errorf("%v answered %d %v, want %d %s with a hint", tc.err, status, body,
				tc.status, tc.code)
		}
	}
}

// THE FLEET VIEW'S NODE ROWS CARRY THE BROKER KIND, and the dashboard's
// fixture is the engine's own rendering of one: `unknown` for a row that did
// not say, never an empty cell.
func TestTheBrokerAnswerFixtureIsTheEnginesOwn(t *testing.T) {
	t.Parallel()
	view := engine.BrokerView{
		Node: "node-a", Kind: "member",
		Nodes: []engine.BrokerNode{
			{Node: "node-a", Kind: "member", Roles: []string{"data", "ingress", "seats", "workers"}},
			{Node: "node-b", Kind: "member", Roles: []string{"data", "seats"}},
			{Node: "old-1", Kind: "unknown", Roles: []string{"data", "ingress", "seats", "workers"}},
			{Node: "sat-eu-1", Kind: "leaf", Roles: []string{"seats"}},
		},
		Group: &jetstream.MetaGroup{Cluster: "acme", Leader: "node-a", Peers: []jetstream.MetaPeer{
			{Name: "node-a", Peer: "yrzKKRBu", Self: true, Leader: true, Current: true},
			{Name: "node-b", Peer: "cnrtt3eg", Current: true, Active: 412000000},
			{Name: "node-c", Peer: "b4qKmd1Z", Offline: true, Active: 5400000000000},
		}},
		GroupFrom: "node-a",
		Findings: []engine.BrokerFinding{
			{Kind: engine.BrokerDeadMember, Node: "node-c", Detail: "the metadata group " +
				"counts it as a voter and no live node is it"},
			{Kind: engine.BrokerUnknownKind, Node: "old-1", Detail: "its presence does " +
				"not say what its broker is"},
		},
	}
	got, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("CREWLET_REGENERATE_BROKER_ANSWER") == "1" {
		if err := os.WriteFile(brokerGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(brokerGolden)
	if err != nil {
		t.Fatalf("read %s: %v — `make broker-answer` writes it", brokerGolden, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the broker answer no longer matches %s. Run `make broker-answer`, "+
			"read the diff, and follow it in the dashboard's fleet screen — its suite "+
			"loads this file.\n--- rendered now ---\n%s", brokerGolden, got)
	}
}

// brokerGolden is GET /fleet/broker's answer for a fleet with a dead member and
// a node on an older build, committed, and read by the dashboard's own suite as
// its fixture.
const brokerGolden = "testdata/fleet_broker_answer.json"
