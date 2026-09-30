package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE BLIND-KEY MINT AND THE SESSION COLLECTION EACH DECIDE FROM AN ABSENT ROW
// — and on a real node the applier's checkpoint moves past a record it
// RETAINS, so "caught up" off the checkpoint read a retained record's rows as
// rows nobody wrote. These cases put a real node into that state the two ways
// it happens and ask each decision on it.

// retentionCause is why a node keeps a record at its position instead of
// applying it.
type retentionCause string

const (
	// retainedUnderUnknownKey is a record signed under a keyring key this
	// node does not hold: a peer that activated a new key before this node
	// was restarted with it — the ordinary middle of a key rotation.
	retainedUnderUnknownKey retentionCause = "signed under a keyring key this node lacks"

	// retainedFromNewerBuild is a record at a version this build cannot
	// read: a peer already upgraded — the ordinary middle of a rollout.
	retainedFromNewerBuild retentionCause = "written by a newer build"
)

// retentionNode boots a real node running the identity domain, and hands back
// the bootstrap whose keyring is the one it verifies records under.
func retentionNode(t *testing.T) (*Engine, config.Bootstrap) {
	t.Helper()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: directoryConfig(t),
		Metrics: rec})
	if err != nil {
		t.Fatalf("boot a node: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if n := e.native.Load(); n == nil || n.iamReader == nil || n.log == nil {
		t.Fatal("the node runs no identity domain")
	}
	return e, b
}

// retain publishes one identity record this node cannot apply, the way a peer
// would, and waits until its applier has consumed it — checkpoint at the log's
// end, the record's rows nowhere in its estate.
func retain(t *testing.T, e *Engine, b config.Bootstrap, why retentionCause,
	rec iamdomain.MutationRecord) uint64 {

	t.Helper()
	domain := iamdomain.Domain{}
	ring := recordKeyring(&b)
	switch why {
	case retainedUnderUnknownKey:
		ring = statelog.OneKey("rotated-in", "keyring-material-this-node-was-not-restarted-with")
	case retainedFromNewerBuild:
		rec.V = iamdomain.RecordVersion + 1
	}
	signer, err := statelog.NewSigner(domain.Name(), ring)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	payload, err := iamdomain.Encode(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	running := e.native.Load().log.Domain(domain.Name())
	seq, _, err := running.log.Append(t.Context(), rec.Subject.Wire(), "", nil,
		signer.Seal(payload))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if held, ok := running.runner.Deferred(); ok && running.runner.Committed().Seq >= seq {
			if held.Position.Seq > seq {
				t.Fatalf("the node retained %s, not the record at %d", held.Position, seq)
			}
			return seq
		}
		if err := running.runner.Stopped(); err != nil {
			t.Fatalf("the applier stopped rather than retaining: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the node did not consume and retain the record at %d (checkpoint %s)",
		seq, running.runner.Committed())
	return 0
}

// claimRecord is the first record of an enrolment on a peer: a login claim for
// a person this node has never seen.
func claimRecord(t *testing.T, person, login string) iamdomain.MutationRecord {
	t.Helper()
	mutation, err := iamdomain.EncodeClaim(iamdomain.Claim{V: iamdomain.DocumentVersion,
		Person: person})
	if err != nil {
		t.Fatalf("encode the claim: %v", err)
	}
	return iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.BaseRecordVersion, OpID: uuid.Must(uuid.NewV7()).String(),
			Subject: iamdomain.LoginSubject(login), Op: iamdomain.OpClaim,
			CreatedAt: time.Now().UTC(), Writer: "node-peer",
			Scope: iamdomain.PeopleScope(person),
		},
		Mutation: mutation, Person: person,
		Actor: "node-peer", ActorKind: "machine",
	}
}

// THE BLIND KEY IS NOT MINTED OVER ROWS A NODE RETAINED.
//
// A missing blind-index key on an estate that holds a blind was DELETED, and
// minting another orphans every address. Here the only blinded rows are a
// peer's address claim this node retained, so its rows hold none — and judged
// "caught up" off its checkpoint the node minted a fresh key over the deleted
// one on the first sign-in by address.
func TestTheBlindKeyIsNotMintedOverRowsANodeRetained(t *testing.T) {
	t.Parallel()
	for _, why := range []retentionCause{retainedUnderUnknownKey, retainedFromNewerBuild} {
		t.Run(string(why), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			e, b := retentionNode(t)
			peers, err := iamdomain.NewBlinder([]byte("the-deleted-key-the-peer-derived-under"))
			if err != nil {
				t.Fatalf("blinder: %v", err)
			}
			blind, err := peers.Email("dana@example.com")
			if err != nil {
				t.Fatalf("blind: %v", err)
			}
			person := uuid.Must(uuid.NewV7()).String()
			rec := claimRecord(t, person, "unused")
			rec.Subject = iamdomain.EmailSubject(blind)
			retain(t, e, b, why, rec)

			_, err = e.PersonBlinder().Blinder(ctx)
			if !errors.Is(err, iamdomain.ErrNotCurrent) {
				t.Fatalf("resolving the blinder answered %v, want ErrNotCurrent — "+
					"the node's rows cannot say whether a blind was ever derived", err)
			}
			store := fleetsecrets.New(e.backends.Fleet, e.cipher).Estate()
			if _, err := store.Get(ctx, iamdomain.BlindKeyName); !errors.Is(err,
				secrets.ErrNotFound) {
				t.Fatalf("a blind-index key was minted over rows the node retained (%v)", err)
			}
		})
	}
}
