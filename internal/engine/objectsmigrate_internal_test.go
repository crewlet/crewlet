package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/statelog"
)

// migrationSource names a fixed set of objects.
type migrationSource struct{ refs []collect.Reference }

func (*migrationSource) Name() string { return "tracker" }

func (*migrationSource) Barrier(context.Context) (statelog.Position, error) {
	return statelog.Position{}, nil
}

func (*migrationSource) Referenced(context.Context, []objstore.Key, statelog.Position) (map[objstore.Key]struct{}, bool, error) {
	return nil, false, errors.New("unused")
}

func (s *migrationSource) Each(_ context.Context, _ statelog.Position, _ objstore.Key,
	visit func(collect.Reference) error) (bool, error) {
	for _, r := range s.refs {
		if err := visit(r); err != nil {
			return false, err
		}
	}
	return true, nil
}

// migrationNode is an engine on the nats backend holding three named files,
// with an S3 bucket to move them to.
func migrationNode(t *testing.T, mode statelog.MaintenanceMode) (*Engine, *coordmemory.Fleet,
	*migrationSource, config.StoreObjects, *s3fake.Server) {
	t.Helper()
	fleet := coordmemory.NewFleet()
	if _, err := fleet.AgreeObjectBackend(t.Context(), "nats"); err != nil {
		t.Fatal(err)
	}
	store, err := objstore.NewStore(memobj.New())
	if err != nil {
		t.Fatal(err)
	}
	src := &migrationSource{}
	for _, body := range []string{"one", "two", "three"} {
		o, err := store.Put(t.Context(), strings.NewReader(body), 1<<10, objstore.PutMeta{})
		if err != nil {
			t.Fatal(err)
		}
		src.refs = append(src.refs, collect.Reference{Object: o, NamedBy: "ENG/" + body})
	}
	srv := s3fake.Start(t, "files")
	var objects config.StoreObjects
	objects.Backend = config.ObjectBackendS3
	objects.S3 = config.ObjectsS3{Endpoint: srv.URL, Region: "us-east-1", Bucket: "files",
		Prefix: "acme/", PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret"}
	e := &Engine{id: "data-a", mode: mode,
		backends: &Backends{Fleet: fleet, objectsIdentity: "nats", Objects: memobj.New()},
		objects:  &objectStore{store: store}}
	return e, fleet, src, objects, srv
}

// A COPY PASS MOVES NOTHING, and the final pass moves the fleet's record
// once every object is in the bucket: a node still naming the broker is
// then told the bucket.
func TestTheFinalPassMovesTheFleetOnlyOnceEveryObjectIsCopied(t *testing.T) {
	t.Parallel()
	e, fleet, src, objects, srv := migrationNode(t, statelog.ModeNormal)
	report := ObjectMigrationReport{From: "nats", To: objects.Identity()}
	copied, err := e.migrateTo(t.Context(), false, objects, collect.References{src}, report)
	if err != nil || copied.Pass.Copied != 3 || copied.Moved {
		t.Fatalf("the copy pass = %+v, %v; want three copied and nothing moved", copied, err)
	}
	if got, _ := fleet.AgreeObjectBackend(t.Context(), "nats"); got != "nats" {
		t.Fatalf("a copy pass moved the fleet to %q", got)
	}
	// THE FINAL PASS IS REFUSED ON A PUBLISHING NODE.
	if _, err := e.migrateTo(t.Context(), true, objects, collect.References{src}, report); !errors.Is(err, ErrMigrationRefused) {
		t.Fatalf("a final pass in normal mode answered %v, want a refusal", err)
	}
	e.mode = statelog.ModeMaintenance
	// AND WHILE A NODE MAY STILL PUBLISH.
	if err := fleet.PutAdmission(t.Context(), coord.Admission{NodeID: "data-b", Incarnation: "x", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.migrateTo(t.Context(), true, objects, collect.References{src}, report); !errors.Is(err, ErrMigrationRefused) ||
		!strings.Contains(err.Error(), `"data-b"`) {
		t.Fatalf("a final pass beside an admitted node answered %v, want a refusal naming it", err)
	} else if strings.Contains(err.Error(), "exclude") {
		// NO EXCLUSION IS OFFERED: it is a gesture on an open capacity
		// window, which a final pass is not, so the command it named
		// answered that there was none. Mutation: name it again.
		t.Fatalf("a final pass beside an admitted node offered an exclusion: %v", err)
	}
	if err := fleet.ForgetAdmission(t.Context(), "data-b", "x"); err != nil {
		t.Fatal(err)
	}
	final, err := e.migrateTo(t.Context(), true, objects, collect.References{src}, report)
	if err != nil || !final.Moved || final.Pass.Present != 3 || final.Pass.Copied != 0 {
		t.Fatalf("the final pass = %+v, %v; want everything present and the fleet moved", final, err)
	}
	if got, _ := fleet.AgreeObjectBackend(t.Context(), "nats"); got != objects.Identity() {
		t.Fatalf("after the move a node naming nats is told %q", got)
	}
	if len(srv.Keys()) != 3 {
		t.Fatalf("the bucket holds %v, want the three files", srv.Keys())
	}
}

// A TARGET THAT IS NOT A BUCKET, OR IS THE FLEET'S OWN STORE, IS REFUSED
// before anything is read.
func TestAMigrationTargetIsRefusedByName(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := migrationNode(t, statelog.ModeNormal)
	if _, err := e.MigrateObjects(t.Context(), ObjectMigration{Target: "relative.yaml"}); !errors.Is(err, ErrMigrationRefused) {
		t.Fatalf("a relative target answered %v", err)
	}
	dir := t.TempDir()
	nats := filepath.Join(dir, "nats.yaml")
	if err := os.WriteFile(nats, []byte("store:\n  objects:\n    backend: nats\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MigrateObjects(t.Context(), ObjectMigration{Target: nats}); !errors.Is(err, ErrMigrationRefused) {
		t.Fatalf("a target naming no bucket answered %v", err)
	}
}
