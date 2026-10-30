package api_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE REFERENCE'S GET /fleet EXAMPLE CARRIES EVERY FIELD THE ANSWER DOES, and
// none it does not — a node row, a seat row, a duty row, the activation and
// the answer's own top level, each compared both ways.
//
// The example is the only place a script author sees the answer whole, and it
// had fallen behind it: a node's `admin`, its `appliers_ready` and
// `appliers_total`, the revision and report time beside its epoch, and the
// answer's `target_epoch` and `activation` were on the wire and nowhere on the
// page, while the screen that drew them was the only reader that knew. A field
// the example lacks is a field nobody builds an alert on; one it names that
// the answer does not send is one somebody builds an alert on that never
// fires. So the answer is rendered with every optional field present — a node
// publishing a whole status, an apply record naming the revision it runs and
// would start on and the epoch it applied, an activation, an object-store
// record — and its keys are held to the example's, read by the same
// [exampleUnder] that holds the example's `objects` block to the renderer
// ([TestTheReferenceExampleIsTheRenderersOwnAnswer]).
func TestTheFleetReferenceExampleCarriesEveryFieldTheAnswerDoes(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 23, 8, 14, 2, 0, time.UTC)
	backend := coordmemory.New()
	claim := func(resource, owner string, meta map[string]any) {
		t.Helper()
		if _, _, err := backend.TryAcquire(t.Context(), resource,
			coord.AcquireOptions{Owner: owner, TTL: time.Minute, Meta: meta},
		); err != nil {
			t.Fatal(err)
		}
	}
	// EVERY OPTIONAL HALF OF A STATUS PRESENT, so a field the renderer
	// omits when it is unset is still one this answer carries.
	logs := len(engine.Domains())
	claim(coord.NodeResource("core-1"), "core-1:8f2a", map[string]any{
		coord.StatusKey: coord.NodeStatus{
			InFlight: 1, Posture: "serve", StartedAt: at.Add(-12 * time.Minute),
			AppliersReady: logs, AppliersTotal: logs, AdminPosture: iam.AdminServe,
		}.Meta(),
	})
	claim(coord.SeatResource(uuid.New()), "core-1:8f2a", nil)
	claim(coord.WorkerResource("maintenance"), "core-1:8f2a", nil)

	plane := coordmemory.NewFleet()
	published, err := plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-7", Payload: []byte("{}"), At: at, Summary: "Add the support unit",
		Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: "operator", Source: "api",
			CreatedAt: at},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := plane.RecordApply(t.Context(), coord.NodeApply{
		NodeID: "core-1", Epoch: published.Epoch, RevisionID: "rev-7",
		Applied: published.Epoch, Serving: "rev-7", Boots: "rev-7",
		Status: string(configplane.StatusOK), UpdatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}

	registry := queries.NewRegistry()
	queries.Register(registry, queries.Sources{
		Coord: backend, Plane: plane, Objects: plane, NodeID: "core-1",
		Now: func() time.Time { return at },
	})
	// ASKED AS A CREDENTIAL HOLDING ADMIN, which the fleet view takes: this
	// case is about the answer's shape rather than about who may read it.
	asker := iam.WithPrincipal(t.Context(), iam.Principal{
		ID: uuid.New(), Login: iam.TokenLogin("ops"), Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
	})
	data, err := registry.Answer(asker, "fleet", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var rendered map[string]any
	if err := json.Unmarshal(raw, &rendered); err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "reference",
		"api-endpoints.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := exampleUnder(t, string(page), "### `GET /fleet`")

	// A DATA NODE APPLIES EVERY REGISTERED LOG, so the example's node, one
	// serving with every applier up, shows as many as the engine registers.
	// Typed by hand the figure falls behind the register, which every log a
	// release adds grows.
	node := firstEntry(t, documented, "nodes")
	for _, field := range []string{"appliers_ready", "appliers_total"} {
		if got := node[field]; got != float64(logs) {
			t.Errorf("the example's node shows %s %v, want %d: a data node applies "+
				"every log engine.Domains() registers", field, got, logs)
		}
	}

	holdFields(t, "the answer", documented, rendered)
	for _, list := range []string{"nodes", "seats", "duties"} {
		holdFields(t, "a row of "+list, firstEntry(t, documented, list),
			firstEntry(t, rendered, list))
	}
	activation, _ := rendered["activation"].(map[string]any)
	example, _ := documented["activation"].(map[string]any)
	holdFields(t, "the activation", example, activation)
}

// firstEntry is the first object in one of an answer's lists.
func firstEntry(t *testing.T, answer map[string]any, list string) map[string]any {
	t.Helper()
	entries, _ := answer[list].([]any)
	if len(entries) == 0 {
		t.Fatalf("%s carries no row to compare", list)
	}
	first, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("the first of %s is %#v, not an object", list, entries[0])
	}
	return first
}

// holdFields fails for every field one side carries and the other does not.
func holdFields(t *testing.T, what string, documented, rendered map[string]any) {
	t.Helper()
	for _, key := range slices.Sorted(maps.Keys(rendered)) {
		if _, ok := documented[key]; !ok {
			t.Errorf("%s carries %q and the reference's GET /fleet example does not",
				what, key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(documented)) {
		if _, ok := rendered[key]; !ok {
			t.Errorf("the reference's GET /fleet example names %q on %s, which "+
				"does not carry it", key, what)
		}
	}
}
