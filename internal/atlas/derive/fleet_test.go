package derive_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/config"
)

var (
	engineService = atlas.MustRef(atlas.KindService, atlas.EngineService)
	engineBroker  = atlas.MustRef(atlas.KindQueue, "crewlet/broker")
	nodeA         = atlas.MustRef(atlas.KindHost, "crewlet/node-a")
	nodeB         = atlas.MustRef(atlas.KindHost, "crewlet/node-b")
)

// fleetView is the atlas of a company with nothing in it but nodes.
func fleetView(t *testing.T, nodes ...derive.FleetNode) (atlas.View, derive.Derived) {
	t.Helper()
	s := derive.FromSettings(&config.Company{Name: "acme"}, "r1", derive.Deployment{})
	d, _ := s.Records(derive.Records{Fleet: nodes}, now)
	return atlas.Fold(d.Claims, now), d
}

func edges(t *testing.T, v atlas.View, from atlas.Ref, rel atlas.Relation) []string {
	t.Helper()
	e, ok := v.Find(from.ID())
	if !ok {
		t.Fatalf("%v is not in the atlas", from)
	}
	var out []string
	for _, ed := range e.Relations {
		if ed.Relation == rel {
			to, _ := v.Find(ed.To)
			out = append(out, to.Ref.Key)
		}
	}
	slices.Sort(out)
	return out
}

// THE FLEET DRAWS ITSELF: each live node a server, the embedded broker a
// queue on the members that hold it and never on a leaf, the object store a
// bucket, and the engine the service that runs on every node and depends on
// both. Every claim is the fleet's own record and confirmed.
func TestTheFleetDrawsItsNodesItsBrokerAndItsObjectStore(t *testing.T) {
	t.Parallel()
	v, d := fleetView(t,
		derive.FleetNode{ID: "node-b", ObjectsKnown: true},
		derive.FleetNode{ID: "node-a", BrokerMember: true, Address: "10.0.0.4", ObjectsKnown: true})
	if len(d.Withheld) != 0 {
		t.Fatalf("withheld %+v", d.Withheld)
	}
	if got := edges(t, v, engineService, atlas.RelationRunsOn); !slices.Equal(got, []string{"crewlet/node-a", "crewlet/node-b"}) {
		t.Errorf("the engine runs on %v, want both nodes", got)
	}
	if got := edges(t, v, engineService, atlas.RelationDependsOn); !slices.Equal(got, []string{"crewlet/broker", "crewlet/objects"}) {
		t.Errorf("the engine depends on %v, want its broker and its object store", got)
	}
	if got := edges(t, v, engineBroker, atlas.RelationRunsOn); !slices.Equal(got, []string{"crewlet/node-a"}) {
		t.Errorf("the broker runs on %v, want the member alone", got)
	}
	for _, e := range v.Entries {
		if e.State != atlas.StateConfirmed || len(e.Sources) != 1 || e.Sources[0] != atlas.RecordSource(atlas.RecordFleet) {
			t.Errorf("%v is %s from %+v, want confirmed by the fleet's records alone", e.Ref, e.State, e.Sources)
		}
		if src, named := e.Ref.EngineNamed(); e.Ref.Kind != atlas.KindService && (!named || src != atlas.RecordSource(atlas.RecordFleet)) {
			t.Errorf("%v is not named by the fleet, so a person could add it by hand", e.Ref)
		}
	}
}

// A NODE'S ADDRESS IS MARKED INTERNAL WHATEVER NETWORK IT IS ON, and merges
// no two entries: a public address is private here, by rule.
func TestANodesAddressIsAlwaysMarkedInternal(t *testing.T) {
	t.Parallel()
	v, _ := fleetView(t, derive.FleetNode{ID: "node-a", Address: "203.0.113.9"},
		derive.FleetNode{ID: "node-b", Address: "203.0.113.9"})
	for _, ref := range []atlas.Ref{nodeA, nodeB} {
		e, ok := v.Find(ref.ID())
		if !ok || len(e.Members) != 1 {
			t.Fatalf("%v = %+v, want an entry of its own: a shared address merges no nodes", ref, e.Members)
		}
		found := false
		for _, f := range e.Facts {
			if f.Fact == atlas.FactAddress {
				found = true
				if !f.Value.Private || f.Value.Text != "203.0.113.9" {
					t.Errorf("%v's address = %+v, want it marked private", ref, f.Value)
				}
			}
		}
		if !found {
			t.Errorf("%v carries no address", ref)
		}
	}
}

// AN EXTERNAL BROKER IS DRAWN ON ITS OWN SERVERS, each once however many
// nodes name it, and an S3 bucket by its name and region.
func TestAnExternalBrokerAndAnS3BucketAreDrawnAsTheNodesNameThem(t *testing.T) {
	t.Parallel()
	node := func(id string) derive.FleetNode {
		return derive.FleetNode{ID: id, ExternalBroker: true, BrokerServers: []string{"nats-1.internal", "nats-2.internal"},
			ObjectsKnown: true, ObjectsS3: true, Bucket: "acme-files", Region: "eu-west-1"}
	}
	v, d := fleetView(t, node("node-a"), node("node-b"))
	if len(d.Withheld) != 0 {
		t.Fatalf("withheld %+v", d.Withheld)
	}
	if got := edges(t, v, engineBroker, atlas.RelationRunsOn); !slices.Equal(got, []string{"nats/nats-1.internal", "nats/nats-2.internal"}) {
		t.Errorf("the broker runs on %v, want the external cluster's two servers", got)
	}
	if got := edges(t, v, engineService, atlas.RelationDependsOn); !slices.Equal(got, []string{"crewlet/broker", "s3/acme-files"}) {
		t.Errorf("the engine depends on %v, want its broker and the bucket", got)
	}
	bucket, _ := v.Find(atlas.MustRef(atlas.KindBucket, "s3/acme-files").ID())
	if !slices.ContainsFunc(bucket.Facts, func(f atlas.FactView) bool {
		return f.Fact == atlas.FactRegion && f.Value.Text == "eu-west-1"
	}) {
		t.Errorf("the bucket's facts = %+v, want its region", bucket.Facts)
	}
}

// NO NODE, NO CLAIM: a fleet nobody could list draws nothing of itself, and a
// node on a build that says nothing of what it runs on is still a server.
func TestAFleetNobodyListedDrawsNothingAndASilentNodeIsStillDrawn(t *testing.T) {
	t.Parallel()
	if v, _ := fleetView(t); len(v.Entries) != 0 {
		t.Errorf("no node drew %d entries", len(v.Entries))
	}
	v, _ := fleetView(t, derive.FleetNode{ID: "node-a"})
	if _, ok := v.Find(nodeA.ID()); !ok {
		t.Error("a node that says nothing of what it runs on is not drawn")
	}
	if got := edges(t, v, engineService, atlas.RelationDependsOn); !slices.Equal(got, []string{"crewlet/broker"}) {
		t.Errorf("the engine depends on %v, want the broker alone: no node said where files are kept", got)
	}
}
