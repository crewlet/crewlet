package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/flock"
)

// clusterMemberConfig is a Tier A for a member of a three-node cluster, with
// its keyring, written into its own directory; it returns the path.
func clusterMemberConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	doc := fmt.Sprintf(`node:
  id: node-a
store:
  path: %s
coordination:
  type: embedded-kv
stream:
  store_dir: %s
  replicas: 3
  cluster:
    name: crewlet
    port: 6222
    peers: ["nats://node-b.internal:6222", "nats://node-c.internal:6222"]
secrets:
  active_key_id: k1
  keys:
    - id: k1
      material: %q
`, filepath.Join(dir, "index.db"), filepath.Join(dir, "stream"),
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A CLUSTER MEMBER WITH ITS ENGINE STOPPED IS SENT TO A RUNNING ONE, by name,
// and starts no broker: alone it cannot reach the quorum the store needs, and
// beside running peers it would rejoin the cluster as this node for the sake of
// one value. Its peers serve the store, so the way in is one of them — or this
// node's own engine, once started.
//
// Mutation: drop the refusal and the member waits out its whole readiness
// budget for a quorum of one, then fails naming no way forward.
func TestAClusterMemberWithItsEngineStoppedIsSentToARunningOne(t *testing.T) {
	t.Parallel()
	cfg := clusterMemberConfig(t)
	var out, errs bytes.Buffer
	err := run([]string{"secrets", "list", "-config", cfg}, &out, &errs)
	if err == nil {
		t.Fatalf("a cluster member reached the fleet's store on its own: %s", out.String())
	}
	if !errors.Is(err, engine.ErrNoFleetStoreHere) {
		t.Fatalf("the refusal is not ErrNoFleetStoreHere: %v", err)
	}
	for _, want := range []string{"-api", apiTokenEnv, "start this node's engine", cfg} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal omits %q: %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(cfg), "stream")); statErr == nil {
		t.Error("a broker was started on the member's store directory before it was refused")
	}
}

// THE NODE'S DATABASE IS HELD FOR AS LONG AS THE STORE IS: a command reaching
// the fleet's store keeps the engine from starting under it, and lets it go
// when it is done.
//
// Asked of the database's own lock file, with a lock of the kind every taker
// uses ([flock.Try]): it excludes a second taker in this process as firmly as
// in another, so it answers what an engine starting beside this command would
// meet.
//
// Mutation: release the database when the store opens rather than when it
// closes, and the probe takes the lock while the store is held; forget to
// release it, and the probe after the close is refused.
func TestTheNodesDatabaseIsHeldForAsLongAsTheFleetsStore(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	boot, err := loadBootstrapForStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	probe := func() error {
		lock, err := flock.Try(boot.Store.Path + ".lock")
		lock.Release()
		return err
	}
	_, closeFleet, err := openFleetStore(t.Context(), boot, cfg)
	if err != nil {
		t.Fatalf("open the fleet's store: %v", err)
	}
	if err := probe(); !errors.Is(err, flock.ErrHeld) {
		t.Errorf("the node's database was free while the store was held (%v): "+
			"an engine could have started under the command", err)
	}
	closeFleet()
	closeFleet() // twice is a no-op, so every caller can defer it
	if err := probe(); err != nil {
		t.Errorf("the node's database stayed held after the store closed: %v", err)
	}
}
