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

// COMPANY KEYS: the secrets the engine mints for itself, once per company, and
// every node then reads.
//
// One exists: the identity estate's blind-index key. It is the key a BLIND is
// derived under, and a blind is compared across nodes — a claim arbitrates on
// it — so every node has to hold the SAME key. A node computing under one it
// minted and did not keep is computing values nobody else can match, and
// nothing reports it, because a blind is opaque by construction.
//
// There were two. The org chart's was minted on demand for a keyed address
// index no chart row holds — the chart matches on the address's normalised
// form — and it was the one mint with no guard against a deleted key. It went
// with the unwired index it was for; see [Engine.chartSealer].
//
// # Every key is minted behind a guard
//
// A missing key is not always a key nobody minted: somebody may have deleted
// one that every stored value was derived under, and minting another then
// silently orphans all of them. Only the estate that derived values under a
// key can say whether it ever did, so [Engine.companyKey] REQUIRES the caller's
// guard rather than taking a nil for "mint whatever happens" — which is exactly
// how the chart's key came to have none.
//
// # Why the mint is a create, and what the hold is still for
//
// The company's secret store is last-write-wins for a PUT. Two nodes that each
// found the key absent each minted one, and each read its own back when the
// other's write had not landed yet, so both went on computing under different
// keys while the store held one of them. A fresh fleet booting every node at
// once is exactly when that happens.
//
// The key is therefore written with a CREATE, which the store refuses over a
// key that exists: whichever mint lands first is THE key, and every other
// minter reads that one back. That is the whole of the correctness, and it is
// the store's own — a lease cannot give it, because a holder paused past its
// lease (a GC stop, a coordination write that hangs and then lands) writes
// after a second holder has minted and read its key back, and a put then
// splits the company exactly as it did with no hold at all.
//
// The fleet hold and the in-process gate stay, as the COURTESY they are: they
// keep two nodes — and two goroutines of this process, since a hold claimed
// again by the owner that holds it answers yes — from each running the mint's
// guard and generating a key only one of them can keep.

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
// knows what its estate would say about that. It is REQUIRED; see the file's
// header.
func (e *Engine) companyKey(ctx context.Context, name, source string,
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
	if err := may(ctx); err != nil {
		return "", err
	}
	minted, err := secrets.GenerateKey()
	if err != nil {
		return "", fmt.Errorf("engine: mint %s: %w", name, err)
	}
	// A CREATE, which a key that landed first refuses — see the file's
	// header — and whoever's landed is the one read back. THE NODE, AS THE
	// ENGINE: nobody asked for this key — the first write that needed one
	// did — so no credential made it either.
	if _, err := store.Create(ctx, name, secrets.EncodeKey(minted), secrets.Author{
		Name: e.id, Kind: string(iam.ActorSystem),
	}, source, time.Now().UTC()); err != nil {
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
// "NO BLIND HERE" IS AN ABSENCE, and the rows asked prove it only when their
// own snapshot has APPLIED everything the log held when the question was asked
// — see [iamdomain.CoversLog]. A node behind the log, or holding a record it
// retained, may simply not have the blinded rows yet.
func (e *Engine) mayMintPersonBlindKey(ctx context.Context) error {
	if e.native == nil || e.native.iamReader == nil || e.native.log == nil {
		return fmt.Errorf("%w: this node runs no identity domain, so it "+
			"cannot tell whether %s was ever in use", iamdomain.ErrNoBlindKey,
			iamdomain.BlindKeyName)
	}
	return judgeBlindKeyMint(ctx, e.IdentityLogEnd, e.native.iamReader.HoldsBlinds)
}

// IdentityLogEnd is the identity log's last sequence, as the broker holds it
// now — what an answer read from this node's rows afterwards is proved against
// ([iamdomain.CoversLog]).
//
// THE END ONLY, and never a verdict of "caught up" beside it: whether the rows
// hold the log is a fact about the SNAPSHOT that reads them, which says how
// much it applied in the same transaction as the rows. A verdict formed here,
// from the applier's checkpoint, is the one that moved past a record the node
// retained and called the node current while that record's rows were missing.
func (e *Engine) IdentityLogEnd(ctx context.Context) (uint64, error) {
	if e.native == nil || e.native.log == nil {
		return 0, errors.New("engine: this node runs no identity domain, so " +
			"there is no identity log to read the end of")
	}
	running := e.native.log.Domain(iamdomain.Domain{}.Name())
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
// SEPARATE FROM THE ENGINE so every arm is a case of its own. THE END IS READ
// FIRST: rows asked afterwards can only have more of the log than the end they
// are proved against, so a record landing between the two makes the answer
// "cannot say" rather than letting a snapshot that missed it say "none".
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
