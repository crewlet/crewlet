package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/secrets"
)

// COMPANY KEYS: secrets the engine mints once per company and every node reads;
// one exists, the identity estate's blind-index key. Blinds are compared across
// nodes, so all must hold the SAME key, and a mismatch fails silently.
//
// Every mint is behind a guard ([Engine.companyKey] requires one): a missing
// key may be a DELETED one, and only the caller's estate can tell.
//
// The mint is a CREATE, refused over an existing key, so the first to land is
// THE key and every minter reads it back. That alone is the correctness: a
// lease cannot fence a write. The fleet hold, and the in-process gate (a hold
// answers yes to its own holder again), only spare keys nobody keeps.

// companyKeyHoldTTL bounds how long a node that died while minting keeps every
// other node from minting.
//
// ONE SEAT HEARTBEAT (15 s at the shipped 45 s lease): a mint is three
// coordination round trips, so this backstops a dead holder rather than timing
// the work, and the hold is released as soon as the work ends.
const companyKeyHoldTTL = seat.SeatLeaseTTL / seat.HeartbeatRatio

// companyKeyPoll is how often a node that found another minting looks again.
//
// A QUARTER OF A SECOND: the holder's work is three round trips and a waiter
// is a request somebody waits on; faster buys only reads of a mid-write store.
const companyKeyPoll = 250 * time.Millisecond

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
// may is asked under the hold before a mint, and its error refuses it. It is
// REQUIRED (the file's header).
func (e *Engine) companyKey(ctx context.Context, name string, source secrets.Source,
	may func(context.Context) error) (string, error) {

	if may == nil {
		return "", fmt.Errorf("engine: %s has no mint guard, and a company key "+
			"minted with none is minted over a deleted one the day somebody "+
			"deletes it", name)
	}
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
		if value, readErr := presentKey(ctx, store, name); value != "" || readErr != nil {
			return value, readErr
		}
		release, held := func() {}, true
		if hold != nil {
			// Nil hold: single node, no peer to keep apart from.
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
		// A PEER IS MINTING: wait to read its write.
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("engine: wait for a peer to mint %s: %w",
				name, ctx.Err())
		case <-time.After(companyKeyPoll):
		}
	}
}

// mintKey mints a company key under the hold, unless it has appeared since.
func (e *Engine) mintKey(ctx context.Context, store *fleetsecrets.Estate, name string,
	source secrets.Source, may func(context.Context) error) (string, error) {

	// AGAIN UNDER THE HOLD: a peer that held it a moment ago may have
	// written the key between this node's last read and its claim.
	if value, err := presentKey(ctx, store, name); value != "" || err != nil {
		return value, err
	}
	if err := may(ctx); err != nil {
		return "", err
	}
	minted, err := secrets.GenerateKey()
	if err != nil {
		return "", fmt.Errorf("engine: mint %s: %w", name, err)
	}
	// A CREATE (the file's header). Authored by the node as the engine: no
	// credential asked for this key.
	if _, err = store.Create(ctx, name, secrets.EncodeKey(minted), secrets.Author{
		Name: e.id, Kind: string(iam.ActorSystem),
	}, source, time.Now().UTC()); err != nil {
		return "", fmt.Errorf("engine: store %s: %w", name, err)
	}
	// READ BACK, never the minted value: another node's may have landed.
	value, err := presentKey(ctx, store, name)
	if err == nil && value == "" {
		err = fmt.Errorf("engine: %s was written and reads back empty", name)
	}
	if err != nil {
		return "", fmt.Errorf("engine: read back %s: %w", name, err)
	}
	if value == secrets.EncodeKey(minted) {
		log.Info("company_key_minted", "name", name, "source", source)
	}
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
// runs (rotating it is a migration, [iamdomain.BlindKeyName]), and a failure
// is retried by the next caller
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
	value, err := s.e.companyKey(ctx, iamdomain.BlindKeyName, secrets.SourceIAM,
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
// A MISSING KEY ON AN ESTATE THAT HOLDS BLINDS IS A DELETION (no blind exists
// without a key): a new key would
// orphan every address in the directory and let a second person claim each one
// (a new key derives a different blind), so the remedy is restoring the key.
// "No blind here" holds only when the rows' snapshot covers the log
// ([iamdomain.CoversLog]); a node behind it may not have the rows yet.
func (e *Engine) mayMintPersonBlindKey(ctx context.Context) error {
	c := e.core.Load()
	if c == nil || c.log == nil {
		return fmt.Errorf("%w: this engine runs no core runtime, so it has no "+
			"identity rows to tell whether %s was ever in use",
			iamdomain.ErrNoBlindKey, iamdomain.BlindKeyName)
	}
	return judgeBlindKeyMint(ctx, e.IdentityLogEnd, c.iamReader.HoldsBlinds)
}

// IdentityLogEnd is the identity log's last sequence, as the broker holds it
// now — what an answer read from this node's rows afterwards is proved against
// ([iamdomain.CoversLog]).
//
// THE END ONLY, never a "caught up" verdict: whether rows hold the log is a
// fact about the snapshot reading them, not the applier's checkpoint, which
// can pass a retained record whose rows are missing.
func (e *Engine) IdentityLogEnd(ctx context.Context) (uint64, error) {
	s, err := e.stateLogOf()
	if err != nil {
		return 0, fmt.Errorf("engine: there is no identity log to read the end "+
			"of: %w", err)
	}
	running := s.Domain(iamdomain.Domain{}.Name())
	if running == nil {
		return 0, errors.New("engine: the identity log is not running on this node")
	}
	end, err := running.log.End(ctx)
	if err != nil {
		return 0, fmt.Errorf("engine: read how far the identity log goes: %w", err)
	}
	return end, nil
}

// judgeBlindKeyMint is the mint decision over its two seams: the identity log's
// end, and whether this node's rows — proved against it — hold a blind.
//
// THE END IS READ FIRST: rows read afterwards can only hold more of the log,
// so a record landing between makes the answer "cannot say", never "none".
func judgeBlindKeyMint(ctx context.Context, logEnd func(context.Context) (uint64, error),
	holdsBlinds func(context.Context, uint64) (bool, error)) error {

	end, err := logEnd(ctx)
	if err != nil {
		return err
	}
	holds, err := holdsBlinds(ctx, end)
	if err != nil {
		return err
	}
	if holds {
		return fmt.Errorf("%w: %s was deleted from the secret store, and "+
			"people's email addresses depend on it. Restore the secrets from "+
			"a backup (docs/guides/backup.md, Restoring)",
			iamdomain.ErrNoBlindKey, iamdomain.BlindKeyName)
	}
	return nil
}
