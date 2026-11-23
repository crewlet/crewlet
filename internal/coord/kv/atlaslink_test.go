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

// The atlas buckets' streams under kvtest's prefix.
const (
	atlasSourcesStream = "KV_slowleaf_atlas_sources"
	atlasCodeStream    = "KV_slowleaf_atlas_code_scans"
	atlasRefreshStream = "KV_slowleaf_atlas_source_refreshes"
)

// EVERY ATLAS CLASS AT ITS BOUND IS READ ACROSS A LEAF'S LINK AT ITS FIRST
// ASK. On a link at or above the slowest pace allowed, a source's misses at
// their bound are read by their key with one leader read, though their reply
// takes seconds to cross; a listing of the standings, of the refresh asks or
// of a host's code scans, each record at its class's bound, delivers it in
// its pass, asking none; and a code scan standing at coord.SmallValueBytes is
// read by its key at the term, once. Requests are counted as they reach the
// member.
//
// Mutation: read the misses at the term (replyTerm), and their read asks
// again or fails.
func TestEveryAtlasClassAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		stream string
		rate   int64
		// asks is how many leader reads the read makes: one for a key's
		// read, none for a pass that delivers its value.
		asks  int
		write func(ctx context.Context, member *kv.FleetStore, value []byte) error
		bound int
		read  func(ctx context.Context, leaf *kv.FleetStore) (int, error)
	}{
		{"a source's misses, by its key", atlasSourcesStream, 256 << 10, 1,
			func(ctx context.Context, member *kv.FleetStore, value []byte) error {
				return member.PutAtlasSourceMisses(ctx, "cloudflare", value)
			}, coord.MaxAtlasSourceMissesBytes, func(ctx context.Context, leaf *kv.FleetStore) (int, error) {
				value, found, err := leaf.AtlasSourceMisses(ctx, "cloudflare")
				return len(value), found1(found, err)
			}},
		{"the sources' standings", atlasSourcesStream, 36 << 10, 0,
			func(ctx context.Context, member *kv.FleetStore, value []byte) error {
				return member.PutAtlasSourceStatus(ctx, "cloudflare", value)
			}, coord.MaxAtlasSourceStandingBytes, func(ctx context.Context, leaf *kv.FleetStore) (int, error) {
				all, err := leaf.AtlasSourceStatuses(ctx)
				return len(all["cloudflare"]), found1(len(all) == 1, err)
			}},
		{"the refresh asks", atlasRefreshStream, 36 << 10, 0,
			func(ctx context.Context, member *kv.FleetStore, value []byte) error {
				return member.PutAtlasSourceRefresh(ctx, "cloudflare", value)
			}, coord.SmallValueBytes, func(ctx context.Context, leaf *kv.FleetStore) (int, error) {
				all, err := leaf.AtlasSourceRefreshes(ctx)
				return len(all["cloudflare"]), found1(len(all) == 1, err)
			}},
		{"a host's code scans", atlasCodeStream, 36 << 10, 0,
			func(ctx context.Context, member *kv.FleetStore, value []byte) error {
				return member.PutAtlasCodeScan(ctx, "github", "github.com:acme:shop", value)
			}, coord.SmallValueBytes, func(ctx context.Context, leaf *kv.FleetStore) (int, error) {
				all, err := leaf.AtlasCodeScans(ctx, "github")
				return len(all["github.com:acme:shop"]), found1(len(all) == 1, err)
			}},
		{"a code scan standing, by its key", atlasCodeStream, 36 << 10, 1,
			func(ctx context.Context, member *kv.FleetStore, value []byte) error {
				return member.PutAtlasCodeScan(ctx, "github", "", value)
			}, coord.SmallValueBytes, func(ctx context.Context, leaf *kv.FleetStore) (int, error) {
				value, found, err := leaf.AtlasCodeScan(ctx, "github", "")
				return len(value), found1(found, err)
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
			if err := c.write(ctx, across.Member, []byte(`"`+randomText(t, c.bound-2)+`"`)); err != nil {
				t.Fatalf("write %s at its bound: %v", c.name, err)
			}
			asked := leaderReads(t, across, c.stream)
			got, err := c.read(ctx, across.Leaf)
			if err != nil || got != c.bound {
				t.Fatalf("read %s at its bound across the link = %d bytes, %v; want its %d", c.name, got, err,
					c.bound)
			}
			if n := asked(); n != c.asks {
				t.Errorf("reading %s at its bound across the link asked the leader %d times, want %d", c.name, n,
					c.asks)
			}
		})
	}
}
