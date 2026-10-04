package objstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
)

// ErrNotFound is a chunk the backend does not hold.
var ErrNotFound = errors.New("objstore: no such chunk")

// ErrCorrupt is a chunk whose bytes no longer hash to the name it is stored
// under — never handed to a reader.
var ErrCorrupt = errors.New("objstore: a chunk's bytes do not match its name")

// Held is one chunk a backend lists: its name and when it was last written.
type Held struct {
	Hash Hash
	// Written is when the chunk was last PUT — a put of a chunk already
	// held moves it, which is what the collector's grace is measured from.
	Written time.Time
}

// Backend is where the bytes live: one store the whole fleet shares.
//
// Implemented by the fleet's own JetStream object store (natsobj), by an
// S3-compatible bucket (s3obj) and by the in-memory twin (memobj), and all of
// them certified by ONE suite (objstoretest).
//
// THE CONTRACT IS FOUR VERBS AND A LISTING, and every one is idempotent.
// Content addressing is what makes that enough: a name is its bytes, so a put
// that lands twice stores one thing, and nothing ever has to compare versions.
type Backend interface {
	// Put stores data under h, REPLACING any copy already there and
	// moving its written instant to now — the one property the collector's
	// grace rests on: a file re-using a chunk the collector has judged old
	// re-puts it, and the re-put is what makes it young again.
	Put(ctx context.Context, h Hash, data []byte) error

	// Get answers the bytes stored under h, or [ErrNotFound]. A backend
	// returns what it holds; checking it against h is [Store]'s.
	Get(ctx context.Context, h Hash) ([]byte, error)

	// Stat answers when h was last written, or [ErrNotFound].
	Stat(ctx context.Context, h Hash) (time.Time, error)

	// Delete removes h. Deleting what is not there is not an error.
	Delete(ctx context.Context, h Hash) error

	// List hands every chunk the backend holds to visit, in no particular
	// order, stopping at the first error visit returns.
	List(ctx context.Context, visit func(Held) error) error
}

// Locks serialises the two writers that can disagree about one chunk: a file
// re-putting a chunk the store already holds, and the collector deleting it.
//
// # Why a lock, and why only there
//
// The collector deletes a chunk no row names once it is older than a grace,
// and a file that re-uses an existing chunk re-puts it to make it young again
// ([Backend.Put]). Without exclusion, the collector can read the chunk's age,
// the writer's re-put can land, and the collector's delete can then remove
// the bytes the writer is about to name — a file whose record lands pointing
// at nothing. With it, the collector's read of the age and its delete are one
// step against every re-put, so either the re-put came first (the chunk is
// young and stays) or the delete did (and the re-put stores it again).
//
// A chunk the store does NOT hold needs no lock: nothing can be deleting it,
// and a put of it is the first copy.
type Locks interface {
	// LockChunk takes h for owner, answering false while somebody else
	// holds it. A holder that dies is let go by the lock's own age.
	LockChunk(ctx context.Context, h, owner string) (bool, error)

	// UnlockChunk lets go of h if owner holds it.
	UnlockChunk(ctx context.Context, h, owner string) error
}

// LockTTL is how long a chunk lock outlives a holder that never let it go.
//
// A MINUTE, against [LockedBudget]: what is done under a lock — one stat and
// one delete, or one put of at most a mebibyte — is bounded well inside it,
// so a lock can only lapse under a holder that has already given up its
// request. The cost of the length is how long a writer waits behind a
// collector that died holding one, which is the rarest case there is.
const LockTTL = time.Minute

// LockedBudget bounds what a holder does under one chunk lock.
//
// TWENTY SECONDS: one put of a mebibyte or one stat and delete, against any
// backend this package speaks, is a second at its slowest healthy. A third of
// [LockTTL], so a request abandoned at its deadline has two thirds of the
// lock still to run before anybody else can take the chunk.
const LockedBudget = 20 * time.Second

// lockWait is how long a writer waits for a chunk the collector holds before
// it gives up: two of [LockTTL], so a collector that died holding the lock is
// outlived by its expiry before the upload fails.
const lockWait = 2 * LockTTL

// Store is the object store as every caller uses it: a [Backend] whose every
// read is checked against its name, and whose re-puts take the lock the
// collector deletes under.
type Store struct {
	backend Backend
	locks   Locks
	owner   string
}

// NewStore builds the store over backend. owner names this process in the
// chunk locks it takes.
func NewStore(backend Backend, locks Locks, owner string) (*Store, error) {
	if backend == nil || locks == nil || owner == "" {
		return nil, errors.New("objstore: a store needs a backend, the chunk locks and an owner")
	}
	return &Store{backend: backend, locks: locks, owner: owner}, nil
}

// Backend is the store's backend, for the collector and the backup.
func (s *Store) Backend() Backend { return s.backend }

// Locks is the chunk locks the store takes, for the collector.
func (s *Store) Locks() Locks { return s.locks }

// Owner is the name the store's locks are taken under.
func (s *Store) Owner() string { return s.owner }

// Put stores one chunk, refusing bytes that are not what h names.
//
// A chunk the backend already holds is re-put UNDER ITS LOCK — see [Locks] —
// so a file re-using a chunk the collector is about to delete either makes it
// young first or stores it again after.
func (s *Store) Put(ctx context.Context, h Hash, data []byte) error {
	if !h.Valid() {
		return fmt.Errorf("%w: %q", ErrBadHash, h)
	}
	if got := HashOf(data); got != h {
		return fmt.Errorf("objstore: put %s with bytes that hash to %s", h, got)
	}
	_, err := s.backend.Stat(ctx, h)
	switch {
	case errors.Is(err, ErrNotFound):
		return s.put(ctx, h, data)
	case err != nil:
		return fmt.Errorf("objstore: is %s already stored: %w", h, err)
	}
	return s.Locked(ctx, h, func(ctx context.Context) error {
		return s.put(ctx, h, data)
	})
}

func (s *Store) put(ctx context.Context, h Hash, data []byte) error {
	if err := s.backend.Put(ctx, h, data); err != nil {
		return fmt.Errorf("objstore: store chunk %s: %w", h, err)
	}
	return nil
}

// Locked runs fn holding h's lock, waiting for it while another holder has it
// and giving up after [lockWait]. fn runs under [LockedBudget].
func (s *Store) Locked(ctx context.Context, h Hash, fn func(context.Context) error) error {
	wait, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	for attempt := 0; ; attempt++ {
		got, err := s.locks.LockChunk(wait, string(h), s.owner)
		if err != nil {
			return fmt.Errorf("objstore: lock chunk %s: %w", h, err)
		}
		if got {
			break
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("objstore: chunk %s stayed locked for %v (the collector "+
				"is judging it, or a holder died and its lock has not aged out): %w",
				h, lockWait, wait.Err())
		case <-time.After(backoff.Doubling(attempt, 50*time.Millisecond, 2*time.Second)):
		}
	}
	// LET GO EVEN WHEN THE CALLER'S CONTEXT IS GONE — a release is the
	// teardown of the lock, and one inheriting a dead context leaves the
	// chunk held until it ages out.
	defer func() { _ = s.locks.UnlockChunk(context.WithoutCancel(ctx), string(h), s.owner) }()
	bounded, stop := context.WithTimeout(ctx, LockedBudget)
	defer stop()
	return fn(bounded)
}

// Get answers one chunk's bytes, checked against its name: a chunk whose
// bytes no longer hash to h is [ErrCorrupt], never handed out.
func (s *Store) Get(ctx context.Context, h Hash) ([]byte, error) {
	if !h.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrBadHash, h)
	}
	data, err := s.backend.Get(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("objstore: read chunk %s: %w", h, err)
	}
	if got := HashOf(data); got != h {
		return nil, fmt.Errorf("%w: %s holds bytes that hash to %s", ErrCorrupt, h, got)
	}
	return data, nil
}
