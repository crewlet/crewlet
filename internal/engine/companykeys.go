package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/secrets"
)

// COMPANY KEYS: the secrets the engine mints for itself, once per company, and
// every node then reads.
//
// Two exist: the chart's blind-index key and the identity estate's. Each is the
// key a BLIND is derived under, and a blind is compared across nodes — a claim
// arbitrates on it, a vendor payload is routed by it — so every node has to
// hold the SAME key. A node computing under one it minted and did not keep is
// computing values nobody else can match, and nothing reports it, because a
// blind is opaque by construction.
//
// # Why the mint is held, and why a read-back alone was not enough
//
// The company's secret store is last-write-wins. Two nodes that each found the
// key absent each minted one, and each read its own back when the other's
// write had not landed yet, so both went on computing under different keys
// while the store held one of them. A fresh fleet booting every node at once
// is exactly when that happens. So the mint is taken under a fleet hold AND an
// in-process gate: the hold keeps two nodes apart, and the gate keeps two
// goroutines of this process apart, since a hold claimed again by the owner
// that holds it answers yes.

// companyKeyHoldTTL bounds how long a node that died while minting keeps every
// other node from minting.
//
// ONE SEAT HEARTBEAT (15 s at the shipped 45 s seat lease), the budget the node
// already gives a handful of coordination writes: a mint is a read, a write and
// a read back, so the TTL is a backstop for a dead holder rather than a
// deadline for the work, and the hold is given back the moment the work ends.
const companyKeyHoldTTL = seat.SeatLeaseTTL / seat.HeartbeatRatio

// companyKeyPoll is how often a node that found another minting looks again.
//
// A QUARTER OF A SECOND, because the holder's work is three coordination round
// trips and a waiter is a request somebody is waiting on: polling slower would
// add up to the interval to the first sign-in by address on a fresh fleet,
// and faster would buy nothing but reads against a store that is mid-write.
const companyKeyPoll = 250 * time.Millisecond

// errKeyMintBehind is a node that cannot yet tell whether a missing key may be
// minted, because it has not applied everything its estate might say about it.
var errKeyMintBehind = errors.New("engine: this node has not applied the " +
	"whole identity log yet, so it cannot tell whether a missing key was " +
	"ever in use; retry once it has caught up")

// mintGate is the in-process half of the mint's exclusion. Its zero value is
// ready, so an engine a test built by hand holds one too.
type mintGate struct {
	once sync.Once
	ch   chan struct{}
}

// enter waits for the gate, or for the caller to give up.
func (g *mintGate) enter(ctx context.Context) (func(), error) {
	g.once.Do(func() { g.ch = make(chan struct{}, 1) })
	select {
	case g.ch <- struct{}{}:
		return func() { <-g.ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// companyKey reads a company key, minting it first if no node ever has.
//
// may is asked, under the hold, before a key is minted, and its error refuses
// the mint: a missing key is not always a key nobody minted — somebody may have
// deleted one that every stored blind was derived under — and only the caller
// knows what its estate would say about that. Nil mints unconditionally.
func (e *Engine) companyKey(ctx context.Context, name, source string,
	may func(context.Context) error) (string, error) {

	if e.backends == nil || e.backends.Fleet == nil || e.cipher == nil {
		return "", fmt.Errorf("engine: this node has no company secret store, "+
			"so it cannot read %s", name)
	}
	store := fleetsecrets.New(e.backends.Fleet, e.cipher).Estate()
	if value, err := presentKey(ctx, store, name); value != "" || err != nil {
		return value, err
	}
	leave, err := e.keyMint.enter(ctx)
	if err != nil {
		return "", err
	}
	defer leave()
	hold := e.workerHold("company-key-"+name, companyKeyHoldTTL)
	for {
		if value, err := presentKey(ctx, store, name); value != "" || err != nil {
			return value, err
		}
		release, held := func() {}, true
		if hold != nil {
			// NIL IS THE SINGLE-NODE ANSWER: no coordination store, so
			// no peer to be kept apart from.
			release, held, err = hold(ctx)
			if err != nil {
				return "", fmt.Errorf("engine: claim the right to mint %s: %w",
					name, err)
			}
		}
		if held {
			value, err := e.mintKey(ctx, store, name, source, may)
			release()
			return value, err
		}
		// A PEER IS MINTING IT. Its write is what this node will read
		// next, so wait for that rather than minting a second one.
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("engine: wait for a peer to mint %s: %w",
				name, ctx.Err())
		case <-time.After(companyKeyPoll):
		}
	}
}

// mintKey mints a company key under the hold, unless it has appeared since.
func (e *Engine) mintKey(ctx context.Context, store *fleetsecrets.Estate, name,
	source string, may func(context.Context) error) (string, error) {

	// AGAIN UNDER THE HOLD: a peer that held it a moment ago may have
	// written the key between this node's last read and its claim.
	if value, err := presentKey(ctx, store, name); value != "" || err != nil {
		return value, err
	}
	if may != nil {
		if err := may(ctx); err != nil {
			return "", err
		}
	}
	minted, err := secrets.GenerateKey()
	if err != nil {
		return "", fmt.Errorf("engine: mint %s: %w", name, err)
	}
	if err := store.Set(ctx, name, secrets.EncodeKey(minted), e.id, source,
		time.Now().UTC()); err != nil {
		return "", fmt.Errorf("engine: store %s: %w", name, err)
	}
	// READ BACK rather than returning what was minted: what every other
	// node will read is the stored value, and a node that used anything
	// else would be deriving blinds nobody can match.
	value, err := presentKey(ctx, store, name)
	if err == nil && value == "" {
		err = fmt.Errorf("engine: %s was written and reads back empty", name)
	}
	if err != nil {
		return "", fmt.Errorf("engine: read back %s: %w", name, err)
	}
	log.Info("company_key_minted", "name", name, "source", source)
	return value, nil
}

// presentKey reads a company key, answering "" and no error for one nobody has
// stored — the one absence that is not a failure.
func presentKey(ctx context.Context, store *fleetsecrets.Estate, name string) (
	string, error) {

	value, err := store.Get(ctx, name)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("engine: read %s: %w", name, err)
	}
	return value, nil
}

// personBlinds is the identity estate's blinder, resolved on first use and
// shared by every writer and sign-in on this node.
//
// CACHED ONCE RESOLVED, and only then: the key never changes while a company
// runs (rotating it is a migration, see [iamdomain.BlindKeyName]), so a blinder
// that was built is right for the life of the process, and a failure — a store
// that could not be read, a peer still minting — is retried by the next caller
// rather than remembered.
type personBlinds struct {
	mu   sync.Mutex
	held *iamdomain.Blinder
}

// personBlindSource is [Engine.PersonBlinder]'s answer: this engine's one cache,
// behind the seam every writer and sign-in resolves through.
type personBlindSource struct{ e *Engine }

// Blinder resolves the blinder, minting the company's key if no node ever has.
func (s personBlindSource) Blinder(ctx context.Context) (*iamdomain.Blinder, error) {
	p := &s.e.personBlinds
	p.mu.Lock()
	held := p.held
	p.mu.Unlock()
	if held != nil {
		return held, nil
	}
	value, err := s.e.companyKey(ctx, iamdomain.BlindKeyName, "iam",
		s.e.mayMintPersonBlindKey)
	if err != nil {
		return nil, err
	}
	blinder, err := iamdomain.NewBlinder([]byte(value))
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held == nil {
		p.held = blinder
	}
	return p.held, nil
}

// mayMintPersonBlindKey refuses to mint the identity estate's key over one
// somebody deleted.
//
// A MISSING KEY ON AN ESTATE THAT HOLDS BLINDS IS A DELETION, never a fresh
// company: no blind can be derived without a key, so one stored anywhere means
// a key existed. Minting a new one then would orphan every address in the
// directory — each sign-in by address matches nobody — and, worse, let a second
// person claim each of them, since a claim arbitrates on the blind and a new
// key is a new subject. So the remedy is the operator's: restore the key.
//
// THE ANSWER IS ONLY AS GOOD AS THIS NODE'S ROWS, so it is asked only once
// this node has applied everything the log held when it asked. Behind that, an
// estate with no blinded row may simply be one whose blinded rows have not
// arrived yet.
func (e *Engine) mayMintPersonBlindKey(ctx context.Context) error {
	if e.native == nil || e.native.iamReader == nil || e.native.log == nil {
		return fmt.Errorf("%w: this node runs no identity domain, so it "+
			"cannot tell whether %s was ever in use", iamdomain.ErrNoBlindKey,
			iamdomain.BlindKeyName)
	}
	running := e.native.log.Domain(iamdomain.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("%w: the identity log is not running on this node",
			iamdomain.ErrNoBlindKey)
	}
	end, err := running.log.End(ctx)
	if err != nil {
		return fmt.Errorf("engine: read how far the identity log goes: %w", err)
	}
	if applied := running.runner.Committed(); applied.Seq < end {
		return fmt.Errorf("%w (applied %d of %d)", errKeyMintBehind,
			applied.Seq, end)
	}
	holds, err := e.native.iamReader.HoldsBlinds(ctx)
	if err != nil {
		return err
	}
	if holds {
		return fmt.Errorf("%w: %s is missing from the company's secret store "+
			"while the identity estate holds values derived under it, so it "+
			"was deleted rather than never minted — a new key would orphan "+
			"every address in the directory and let each be claimed again. "+
			"It is the engine's own key, which no operator command writes: "+
			"restore the coordination store's secrets from the backup that "+
			"holds it (docs/guides/backup.md, Restoring); the engine will not "+
			"mint over it", iamdomain.ErrNoBlindKey, iamdomain.BlindKeyName)
	}
	return nil
}
