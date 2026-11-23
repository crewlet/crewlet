package kv_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/objstore"
)

// uploadsStream is the uploads bucket's stream under kvtest's prefix.
const uploadsStream = "KV_slowleaf_uploads"

// EVERY UPLOADS CLASS AT ITS BOUND IS READ ACROSS A LEAF'S LINK AT ITS FIRST
// ASK. The reservation ledger, an upload's record and a seat's save ledger,
// each at its class's bound on a link at or above the slowest pace allowed,
// cross past a term read's re-ask: a read given its class's crossing asks the
// leader once. Each is held a second behind a reply sent ahead of it, so a
// read at the term could not have its reply by its re-ask. Requests are
// counted as they reach the member.
//
// Mutation: read a class at the term (replyTerm), and its row asks again or
// fails.
func TestEveryUploadsClassAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		rate  int64
		bound int
		write func(ctx context.Context, member *kv.FleetStore, value []byte) (uint64, error)
		read  func(ctx context.Context, leaf *kv.FleetStore) ([]byte, uint64, error)
	}{
		{"the reservation ledger", 36 << 10, coord.MaxUploadLedgerBytes,
			func(ctx context.Context, member *kv.FleetStore, value []byte) (uint64, error) {
				return member.SwapUploadLedger(ctx, value, 0)
			}, func(ctx context.Context, leaf *kv.FleetStore) ([]byte, uint64, error) {
				return leaf.UploadLedger(ctx)
			}},
		{"an upload's record", 1 << 20, coord.MaxUploadRecordBytes,
			func(ctx context.Context, member *kv.FleetStore, value []byte) (uint64, error) {
				return member.SwapUploadRecord(ctx, "u-1", value, 0)
			}, func(ctx context.Context, leaf *kv.FleetStore) ([]byte, uint64, error) {
				return leaf.UploadRecord(ctx, "u-1")
			}},
		{"a seat's save ledger", 36 << 10, coord.MaxSaveLedgerBytes,
			func(ctx context.Context, member *kv.FleetStore, value []byte) (uint64, error) {
				return member.SwapSaveLedger(ctx, "sre", value, 0)
			}, func(ctx context.Context, leaf *kv.FleetStore) ([]byte, uint64, error) {
				return leaf.SaveLedger(ctx, "sre")
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if floor := int64(1<<20) * int64(time.Second) / int64(objstore.MiBPace); c.rate < floor {
				t.Fatalf("premise: the link carries %d bytes a second, below the floor's %d", c.rate, floor)
			}
			across := kvtest.SlowLeaf(t, c.rate)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			value := []byte(`"` + randomText(t, c.bound-2) + `"`)
			if wrote, err := c.write(ctx, across.Member, value); err != nil || wrote == 0 {
				t.Fatalf("write %s at its bound = %d, %v", c.name, wrote, err)
			}
			asked := leaderReads(t, across, uploadsStream)
			across.Link.Delay(time.Second)
			got, rev, err := c.read(ctx, across.Leaf)
			if err != nil || rev == 0 || len(got) != c.bound {
				t.Fatalf("read %s at its bound across the link = %d bytes at %d, %v; want its %d", c.name,
					len(got), rev, err, c.bound)
			}
			if n := asked(); n != 1 {
				t.Errorf("reading %s at its bound across the link asked the leader %d times, want once", c.name, n)
			}
		})
	}
}
