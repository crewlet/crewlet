package kv_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/objstore"
)

// secretsStream is the secrets bucket's stream under kvtest's prefix.
const secretsStream = "KV_slowleaf_secrets"

// secretsLinkRate is a link's pace for a record at the class's bound: above
// the floor, and slow enough that the record's reply (about 132 KB) outlasts
// a term read's re-ask at two seconds, so a read given the term alone asks
// again or fails.
const secretsLinkRate = 48 << 10

// EVERY READ OF A CREDENTIAL'S RECORD AT ITS BOUND CROSSES A LEAF'S LINK AT ITS
// FIRST ASK. A record at coord.MaxSecretRecordBytes, on a link at or above the
// slowest pace allowed, is read by its name, decided on by a create over it
// (refused, the round's read finding it live) and by a delete's existence
// read, each with one leader read, and the pass the boot's snapshot makes
// delivers it, asking none. Requests are counted as they reach the member.
// The pass cannot go red on a class read at the term: the raw record crosses
// this link inside the pass's idle of 5 s, so its row holds the delivery
// alone.
//
// Mutation: read the class at the term (replyTerm), and the key's read, the
// create and the delete ask again or fail.
func TestEverySecretAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// read reads the record through the leaf, answering it as stored,
		// or nil where the read answers none.
		read func(ctx context.Context, t *testing.T, across kvtest.Across, written coord.SecretRecord) *coord.SecretRecord
		asks int
	}{
		{"a credential read by its name", func(ctx context.Context, t *testing.T, across kvtest.Across,
			_ coord.SecretRecord) *coord.SecretRecord {
			t.Helper()
			rec, found, err := across.Leaf.Secret(ctx, "GITLAB_TOKEN")
			if err != nil || !found {
				t.Fatalf("read the record across the link = %v, %v", found, err)
			}
			return &rec
		}, 1},
		{"a create deciding on the record", func(ctx context.Context, t *testing.T, across kvtest.Across,
			written coord.SecretRecord) *coord.SecretRecord {
			t.Helper()
			created, err := across.Leaf.CreateSecret(ctx, written)
			if err != nil || created {
				t.Fatalf("create over the live record across the link = %v, %v; want it refused as held",
					created, err)
			}
			return nil
		}, 1},
		{"a delete deciding on the record", func(ctx context.Context, t *testing.T, across kvtest.Across,
			_ coord.SecretRecord) *coord.SecretRecord {
			t.Helper()
			removed, err := across.Leaf.DeleteSecret(ctx, "GITLAB_TOKEN")
			if err != nil || !removed {
				t.Fatalf("delete the record across the link = %v, %v", removed, err)
			}
			return nil
		}, 1},
		{"the snapshot's pass", func(ctx context.Context, t *testing.T, across kvtest.Across,
			_ coord.SecretRecord) *coord.SecretRecord {
			t.Helper()
			rows, err := across.Leaf.SecretValues(ctx)
			if err != nil || len(rows) != 1 {
				t.Fatalf("list the records across the link = %d rows, %v", len(rows), err)
			}
			return &rows[0]
		}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if floor := int64(1<<20) * int64(time.Second) / int64(objstore.MiBPace); secretsLinkRate < floor {
				t.Fatalf("premise: the link carries %d bytes a second, below the floor's %d",
					secretsLinkRate, floor)
			}
			across := kvtest.SlowLeaf(t, secretsLinkRate)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			written := secretAtBound(t)
			if err := across.Member.PutSecret(ctx, written); err != nil {
				t.Fatalf("write the record at its bound through the member: %v", err)
			}
			asked := leaderReads(t, across, secretsStream)
			if got := c.read(ctx, t, across, written); got != nil {
				got.Version = 0
				stored, err := coord.EncodeSecret(*got)
				if err != nil || len(stored) != coord.MaxSecretRecordBytes || got.Value != written.Value {
					t.Fatalf("%s across the link answered %d bytes (%v), want the %d written", c.name,
						len(stored), err, coord.MaxSecretRecordBytes)
				}
			}
			if n := asked(); n != c.asks {
				t.Errorf("%s at its bound across the link asked the leader %d times, want %d", c.name, n,
					c.asks)
			}
		})
	}
}

// secretAtBound is a credential's record exactly at coord.MaxSecretRecordBytes
// as stored, its sealed value random text no link compresses.
func secretAtBound(t *testing.T) coord.SecretRecord {
	t.Helper()
	value := randomText(t, coord.MaxSecretRecordBytes)
	build := func(n int) coord.SecretRecord {
		return coord.SecretRecord{Name: "GITLAB_TOKEN", Value: value[:n], KeyID: "k1",
			UpdatedAt: time.Date(2026, 11, 23, 10, 0, 0, 123456789, time.UTC), UpdatedBy: "founder",
			UpdatedByKind: "operator", OperatorID: "token:ops", Source: "api"}
	}
	size := func(n int) int {
		raw, err := coord.EncodeSecret(build(n))
		if err != nil {
			return coord.MaxSecretRecordBytes + 1
		}
		return len(raw)
	}
	n := sort.Search(len(value)+1, func(n int) bool { return size(n) > coord.MaxSecretRecordBytes }) - 1
	if size(n) != coord.MaxSecretRecordBytes {
		t.Fatalf("premise: no record is exactly its bound, %d bytes", coord.MaxSecretRecordBytes)
	}
	return build(n)
}
