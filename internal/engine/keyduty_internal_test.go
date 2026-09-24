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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE KEY DUTY, THE BLIND-KEY MINT AND THE SESSION COLLECTION EACH DECIDE FROM
// AN ABSENT ROW — and on a real node the applier's checkpoint moves past a
// record it RETAINS, so "caught up" off the checkpoint read a retained
// record's rows as rows nobody wrote. These cases put a real node into that
// state the two ways it happens and ask each decision on it.

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
	if e.native == nil || e.native.iamReader == nil || e.native.log == nil {
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
	running := e.native.log.Domain(domain.Name())
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

// A LIVE PERSON'S KEY SURVIVES ON A NODE THAT RETAINED THEIR ENROLMENT.
//
// A peer enrols somebody and mints their key; this node retains the enrolment's
// records — signed under a key it was not restarted with, or at a version it
// cannot read — and moves its checkpoint past them. An hour later the key duty
// runs HERE: the store holds the person's key, no row here owns it, and the
// checkpoint is at the log's end. Judged "caught up" off that checkpoint the
// key was destroyed — every copy of a live person's name and address,
// irreversibly, for somebody nobody removed.
//
// The CONTROL runs first on the same node before anything is retained: a
// refused enrolment's key, past the grace and owned by nobody, IS collected —
// so what spares the live person's key is the retention and nothing else.
func TestALivePersonsKeySurvivesOnANodeThatRetainedTheirEnrolment(t *testing.T) {
	t.Parallel()
	for _, why := range []retentionCause{retainedUnderUnknownKey, retainedFromNewerBuild} {
		t.Run(string(why), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			e, b := retentionNode(t)
			store := fleetsecrets.New(e.backends.Fleet, e.cipher).Estate()
			sealer, err := iamdomain.NewSealer(store)
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			pass := func() iamdomain.ShredReport {
				t.Helper()
				report, err := iamdomain.ShredKeys(ctx, e.native.iamReader,
					e.PersonKeyIndex(), sealer, time.Now(), e.IdentityLogEnd)
				if err != nil {
					t.Fatalf("the key duty's pass: %v", err)
				}
				return report
			}
			past := time.Now().Add(-2 * iamdomain.OrphanKeyGrace)

			// THE CONTROL.
			refused := uuid.Must(uuid.NewV7()).String()
			if err := sealer.Mint(ctx, refused, secrets.Author{Name: "node-a", Kind: string(iam.ActorSystem)}, past); err != nil {
				t.Fatalf("mint the refused enrolment's key: %v", err)
			}
			if report := pass(); len(report.Collected) != 1 || report.Collected[0] != refused {
				t.Fatalf("before anything was retained the pass reported %+v, want "+
					"the refused enrolment's key collected — without that this "+
					"case proves nothing about what spares the other", report)
			}

			// THE PEER'S ENROLMENT, RETAINED HERE.
			person := uuid.Must(uuid.NewV7()).String()
			if err := sealer.Mint(ctx, person, secrets.Author{Name: "node-peer", Kind: string(iam.ActorSystem)}, past); err != nil {
				t.Fatalf("mint the person's key: %v", err)
			}
			retain(t, e, b, why, claimRecord(t, person, "dana.sre"))
			end, err := e.IdentityLogEnd(ctx)
			if err != nil {
				t.Fatalf("read the log's end: %v", err)
			}
			if got := e.native.log.Domain(iamdomain.Domain{}.Name()).runner.Committed().Seq; got < end {
				t.Fatalf("the checkpoint is at %d of %d — the node is behind, not "+
					"holding a retained record, so this case is not the one it names",
					got, end)
			}

			report := pass()
			if _, err := store.Get(ctx, iamdomain.PersonDEKName(person)); err != nil {
				t.Fatalf("the key duty destroyed a live person's key on a node that "+
					"retained their enrolment (%v) — report %+v", err, report)
			}
			if len(report.Collected) != 0 || report.Unproven != 1 ||
				!errors.Is(report.Unjudged, iamdomain.ErrNotCurrent) {
				t.Errorf("the pass reported %+v, want the key counted as unproven "+
					"because this node's rows cannot vouch for an absence", report)
			}
		})
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
