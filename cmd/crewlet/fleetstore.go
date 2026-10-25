package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// The fleet's secret store, reached by this command itself while the node's
// engine is stopped.
//
// # One store, whether or not the engine runs
//
// The company's values live on the coordination KV, and nowhere else. A
// running engine is reached through its API, as before; a stopped one no
// longer sends a command to a table of the node's own that the fleet never
// read — which is how a provisioning run once read a working signing secret
// as unset and minted over it. Instead the command reaches the store the
// engine keeps: on a solo node it starts the node's own broker on its store
// directory, and on an external broker it dials it ([engine.OpenFleetStore]).
//
// # The engine is kept out for as long as the command holds the store
//
// Two locks, taken in that order: the node's database, which says whether the
// engine is running ([store.ErrLocked] routes the command to its API instead)
// and keeps it from starting, and the broker's store directory, which the
// broker this starts holds until it stops. An engine started part way through
// is refused at whichever it reaches first, by name, and the command finishes
// on a store nothing else is writing.
//
// # Read directly, so read only by whoever could anyway
//
// No node logs these reads, as it logs each reveal through its API. A read here
// is made by a process holding the node's Tier A — its keyring, and the broker's
// own files or credentials — which open every value without any API; an audit
// line this process wrote about itself would record nothing that holder could
// not have done unrecorded.

// fleetSession is the fleet's store this process reached, and how many of its
// commands' readers and writers share it.
//
// SHARED, because one invocation reaches it twice — the company's values read
// before a provisioning run and the sink its minted credentials are written
// to, or `crewlet llm` reading the company and then storing a token — and a
// second broker on the store directory is refused, in this process as in any
// other ([jetstream.StoreDirLock]). Counted like the node database's own claim,
// and let go when the last of them closes.
type fleetSession struct {
	store   *engine.FleetStore
	release func()
	holds   int
}

// fleetSessions is this process's open session per node database.
var fleetSessions = struct {
	mu sync.Mutex
	by map[string]*fleetSession
}{by: map[string]*fleetSession{}}

// openFleetStore reaches the fleet's store for the node boot configures, with
// that node's engine stopped, and hands back its release.
//
// [store.ErrLocked] means the engine is running: returned as it came, for the
// caller to route through the node's API. Every other refusal names its way
// round.
func openFleetStore(ctx context.Context, boot *config.Bootstrap, bootstrapPath string) (
	*engine.FleetStore, func(), error) {

	key := filepath.Clean(boot.Store.Path)
	fleetSessions.mu.Lock()
	defer fleetSessions.mu.Unlock()
	if held := fleetSessions.by[key]; held != nil {
		held.holds++
		return held.store, sync.OnceFunc(func() { leaveFleetSession(key) }), nil
	}

	// THE NODE'S DATABASE FIRST: it is what says the engine is not running,
	// and holding it is what keeps the engine from starting while this
	// command works. Only then the broker, whose own lock covers the moment
	// an engine started before this one reached its database.
	release, err := store.Hold(boot.Store.Path)
	if err != nil {
		return nil, nil, err
	}
	fs, err := engine.OpenFleetStore(ctx, boot)
	if err != nil {
		release()
		return nil, nil, fleetStoreRemedy(err, bootstrapPath)
	}
	fleetSessions.by[key] = &fleetSession{store: fs, release: release, holds: 1}
	return fs, sync.OnceFunc(func() { leaveFleetSession(key) }), nil
}

// leaveFleetSession drops one share of key's session, stopping its broker and
// letting the node's database go with the last.
//
// THE BROKER BEFORE THE DATABASE: the database is what keeps the engine out,
// so it is the last thing this process lets go of.
func leaveFleetSession(key string) {
	fleetSessions.mu.Lock()
	defer fleetSessions.mu.Unlock()
	held := fleetSessions.by[key]
	if held == nil {
		return
	}
	if held.holds--; held.holds > 0 {
		return
	}
	delete(fleetSessions.by, key)
	held.store.Close()
	held.release()
}

// fleetStoreRemedy answers a fleet store this node cannot reach on its own in
// terms of what to do instead.
func fleetStoreRemedy(err error, bootstrapPath string) error {
	switch {
	case errors.Is(err, engine.ErrNoFleetStoreHere):
		return fmt.Errorf("%w\n\nWith %s's engine stopped, reach the store through "+
			"a node that is running: name it with -api, with one of its Tier A "+
			"tokens in %s — or start this node's engine and re-run", err,
			bootstrapPath, apiTokenEnv)
	case errors.Is(err, jetstream.ErrStoreDirHeld):
		return fmt.Errorf("%w\n\n%s's engine was not running when this began, so "+
			"another process has started a broker on its store since: an engine "+
			"starting, or another crewlet command reaching the store. Re-run once "+
			"it is up, or once that command has finished", err, bootstrapPath)
	}
	return err
}

// fleetBackend is the fleet's store, reached directly, as the secrets
// commands' backend.
type fleetBackend struct{ *fleetsecrets.Store }

// Set writes the row under the author and source offered, which — written
// here rather than by a node's API — are what the row records.
func (f fleetBackend) Set(ctx context.Context, name, value string, by secrets.Author,
	source secrets.Source, now time.Time) (secrets.Author, error) {

	return by, f.Store.Set(ctx, name, value, by, source, now)
}

// Rekey re-seals every row of the store, the engine's own keys among them.
// The identity estate is not here to move — its values are re-sealed by the
// engine that holds it, through its own log — so the rotation says nothing
// about it ([noIdentityHere]).
func (f fleetBackend) Rekey(ctx context.Context, activeKeyID string) (rotation, error) {
	rekeyed, err := f.Store.Rekey(ctx, activeKeyID)
	return rotation{Rekeyed: rekeyed}, err
}

// EngineKeys counts the engine's own keys in the store, and nothing of the
// identity estate, which is not here.
func (f fleetBackend) EngineKeys(ctx context.Context) (engineHeld, error) {
	keys, err := f.Store.EngineKeys(ctx)
	return engineHeld{Keys: keys}, err
}

// fleetSecrets is the fleet's store, reached directly, under boot's keyring.
func fleetSecrets(fs *engine.FleetStore, boot *config.Bootstrap) (*fleetsecrets.Store, error) {
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		return nil, fmt.Errorf("secrets keyring: %w", err)
	}
	return fleetsecrets.New(fs.Fleet, cipher), nil
}

// reachedDirectly is what a command says of the store it reached while the
// engine was stopped: which broker, and that no node stood in between.
func reachedDirectly(fs *engine.FleetStore) string {
	return "the fleet's store, reached through " + fs.Where +
		" — this node's engine is stopped"
}
