package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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
// engine is stopped. The company's values live on the coordination KV alone; a
// running engine is reached through its API, for a stopped one the command
// reaches the store the engine keeps: on a solo node it starts the node's own
// broker on its store directory, on an external broker it dials it
// ([engine.OpenFleetStore]).
//
// The node's database is ASKED, never held ([store.Probe]): its lock says whether
// the engine runs ([store.ErrLocked] routes to its API, `503 starting` while it
// boots), and holding it would make a second command read the first as
// an engine. What is held is the broker's directory ([jetstream.StoreDirLock]):
// an engine started part way through, or a second command, is refused there.

// openFleetStore reaches the fleet's store for the node boot configures, with
// that node's engine stopped, and hands back its release, which may be called
// more than once.
//
// ONCE PER RUN. A run decides its route to the store once and shares it with
// everything it reads and writes ([routeSecretStore], [companyStore]), so one
// invocation starts one broker; a second one on the store directory would be
// refused, in this process as in any other ([jetstream.StoreDirLock]).
//
// [store.ErrLocked] means the engine is running, or starting: returned as it
// came, for the caller to route through the node's API. Every other refusal names its way
// round.
func openFleetStore(ctx context.Context, boot *config.Bootstrap, bootstrapPath string) (
	*engine.FleetStore, func(), error) {

	// THE NODE'S DATABASE IS ASKED FIRST, and only asked: it is what says
	// the engine is not running. The broker's own lock covers an engine that
	// starts after the answer.
	if err := store.Probe(boot.Store.Path); err != nil {
		return nil, nil, err
	}
	fs, err := engine.OpenFleetStore(ctx, boot)
	if err != nil {
		return nil, nil, fleetStoreRemedy(err, bootstrapPath)
	}
	return fs, sync.OnceFunc(fs.Close), nil
}

// fleetStoreRemedy answers a fleet store this node cannot reach on its own in
// terms of what to do instead.
func fleetStoreRemedy(err error, bootstrapPath string) error {
	switch {
	case errors.Is(err, engine.ErrNoFleetStoreHere):
		return fmt.Errorf("%w\n\nWith %s's engine stopped, reach the store through "+
			"a node that is running: name it with -api, with one of its Tier A "+
			"tokens in %s, or start this node's engine and re-run", err,
			bootstrapPath, apiTokenEnv)
	case errors.Is(err, jetstream.ErrStoreDirHeld):
		// AN ENGINE STARTING WITH -import-company is the one engine named
		// here: any other start holds the node's database from the read it
		// decides on, before its broker starts ([bootEngine]), so the probe
		// above has already sent the run to its API.
		return fmt.Errorf("%w\n\n%s's engine is not running, and the process named "+
			"above holds its store directory: another crewlet command reaching the "+
			"fleet's store, or an engine part way through a start with "+
			"-import-company. Re-run once that command has finished, or once the "+
			"engine is up", err, bootstrapPath)
	}
	return err
}

// fleetBackend is the fleet's store, reached directly, as the secrets
// commands' backend.
//
// Its reads write no audit line, as a node's reveal does: the process holds the
// node's Tier A, which opens every value without any API, so a line it wrote
// about itself would record nothing it could not have done unrecorded.
type fleetBackend struct{ *fleetsecrets.Store }

// Set writes the row under the author and source offered, which — written
// here rather than by a node's API — are what the row records.
//
// NOTHING IS RE-ACTIVATED: this node's engine is stopped, so it reads the
// store when it starts, and a peer running on the same store is told to reload
// by the note every write prints ([secretsStoppedNote]).
func (f fleetBackend) Set(ctx context.Context, name, value string, by secrets.Author,
	source secrets.Source, now time.Time) (storedSecret, error) {

	return storedSecret{Author: by}, f.Store.Set(ctx, name, value, by, source, now)
}

// Unset removes the row. Nothing is re-activated, as for [fleetBackend.Set]:
// the note an unset prints says when each engine stops resolving it
// ([secretsStoppedRemovalNote]).
func (f fleetBackend) Unset(ctx context.Context, name string) (removedSecret, error) {
	gone, err := f.Store.Unset(ctx, name)
	return removedSecret{Removed: gone}, err
}

// SetMany writes each row in turn, every one checked before the first is
// written so a value no store keeps refuses the whole set.
func (f fleetBackend) SetMany(ctx context.Context, values map[string]string, by secrets.Author,
	source secrets.Source, now time.Time, _ bool) (storedSecret, error) {

	names := slices.Sorted(maps.Keys(values))
	for _, name := range names {
		if err := errors.Join(secrets.CheckName(name), secrets.CheckValue(values[name])); err != nil {
			return storedSecret{}, fmt.Errorf("%s: %w", name, err)
		}
	}
	for _, name := range names {
		if err := f.Store.Set(ctx, name, values[name], by, source, now); err != nil {
			return storedSecret{}, fmt.Errorf("writing %s: %w", name, err)
		}
	}
	return storedSecret{Author: by}, nil
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
		" (this node's engine is stopped)"
}
