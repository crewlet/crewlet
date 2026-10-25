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
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/store"
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

// A COMMAND HOLDS THE STORE DIRECTORY IT USES, AND NOT THE DATABASE IT DOES
// NOT. The database's lock is how the command learns the engine is stopped, so
// it only asks; a claim kept there would make a second command read the first
// as a running engine, and send it to an API nobody serves. The broker the
// command starts holds the store directory, which is what keeps an engine — or
// a second command — off the files it is using.
//
// Asked of each lock file with a lock of the kind every taker uses
// ([flock.Try]): it excludes a second taker in this process as firmly as in
// another, so it answers what another process would meet.
//
// Mutation: keep the database's claim for the session, and the database probe
// is refused; let the store directory go before the broker stops, and the
// directory probe is not.
func TestACommandHoldsTheStoreDirectoryAndNotTheDatabase(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	boot, err := loadBootstrapForStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	probe := func(path string) error {
		lock, err := flock.Try(path)
		lock.Release()
		return err
	}
	database := boot.Store.Path + ".lock"
	directory := filepath.Join(boot.Stream.StoreDir, jetstream.StoreDirLock)

	_, closeFleet, err := openFleetStore(t.Context(), boot, cfg)
	if err != nil {
		t.Fatalf("open the fleet's store: %v", err)
	}
	if err := probe(database); err != nil {
		t.Errorf("the node's database was held while the command worked (%v): a "+
			"second command would read this one as a running engine", err)
	}
	if err := probe(directory); !errors.Is(err, flock.ErrHeld) {
		t.Errorf("the store directory was free while the command's broker ran "+
			"(%v): an engine could have started on the same files", err)
	}
	closeFleet()
	closeFleet() // twice is a no-op, so every caller can defer it
	if err := probe(directory); err != nil {
		t.Errorf("the store directory stayed held after the store closed: %v", err)
	}
}

// A SECOND COMMAND ON A STOPPED NODE IS TOLD ANOTHER COMMAND HAS ITS STORE,
// and is not sent to an engine's API: the database's lock is free, so the
// engine is not running, and the store directory names who holds it.
//
// Mutation: route a held store directory through the running node's API, and
// the refusal names an API that nothing serves.
func TestASecondCommandIsToldAnotherHoldsTheStore(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	boot, err := loadBootstrapForStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	other, err := flock.Try(filepath.Join(boot.Stream.StoreDir, jetstream.StoreDirLock))
	if err != nil {
		t.Fatalf("stand in for another command's broker: %v", err)
	}
	defer other.Release()

	var out, errs bytes.Buffer
	err = run([]string{"secrets", "list", "-config", cfg}, &out, &errs)
	if !errors.Is(err, jetstream.ErrStoreDirHeld) {
		t.Fatalf("the refusal is not ErrStoreDirHeld: %v", err)
	}
	if errors.Is(err, store.ErrLocked) {
		t.Errorf("a held store directory was read as a running engine: %v", err)
	}
	for _, want := range []string{"another crewlet command", "re-run once that command has finished"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("the refusal omits %q: %v", want, err)
		}
	}
}
