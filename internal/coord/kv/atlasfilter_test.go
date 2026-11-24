package kv

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// randomValue is a JSON string of exactly size bytes of random text, which
// no link compresses.
func randomValue(t *testing.T, size int) []byte {
	t.Helper()
	random := make([]byte, size)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	return []byte(`"` + base64.StdEncoding.EncodeToString(random)[:size-2] + `"`)
}

// A LISTING OF THE ATLAS STANDINGS CARRIES NO RECORD OF ANOTHER CLASS OR
// HOST ACROSS A LINK: the broker filters the pass, so a source's misses at
// their bound beside its standing, and another host's code scans at their
// bound beside a forge's, never cross to the leaf listing the standings or
// the forge's repositories. The listings' answers alone cannot show it, since
// each reader's key decoder drops a key it did not ask for.
//
// Mutation: list the standings, or a host's code scans, over the whole bucket
// (jetstream.AllKeys), and the misses or the other host's records cross.
func TestAnAtlasListingCarriesNoRecordOutsideItsFilterAcrossALink(t *testing.T) {
	t.Parallel()
	member, leaf, link := overASlowLeaf(t, runLinkRate)
	ctx := t.Context()
	if err := member.PutAtlasSourceStatus(ctx, "cloudflare", []byte(`{"calls":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := member.PutAtlasSourceMisses(ctx, "cloudflare",
		randomValue(t, coord.MaxAtlasSourceMissesBytes)); err != nil {
		t.Fatalf("put a source's misses at their bound: %v", err)
	}
	if err := member.PutAtlasCodeScan(ctx, "github", "github.com:acme:shop", []byte(`{"tip":"1"}`)); err != nil {
		t.Fatal(err)
	}
	// SIXTEEN OF ANOTHER HOST'S AT THEIR BOUND: 256 KiB a whole-bucket pass
	// would carry.
	for i := range 16 {
		if err := member.PutAtlasCodeScan(ctx, "gitlab", fmt.Sprintf("gitlab.com:acme:r%02d", i),
			randomValue(t, coord.SmallValueBytes)); err != nil {
			t.Fatalf("put another host's code scan at its bound: %v", err)
		}
	}
	for _, c := range []struct {
		name string
		list func() (int, error)
	}{
		{"the standings", func() (int, error) {
			all, err := leaf.AtlasSourceStatuses(ctx)
			return len(all), err
		}},
		{"github's code scans", func() (int, error) {
			all, err := leaf.AtlasCodeScans(ctx, "github")
			return len(all), err
		}},
	} {
		before := link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToNode).Carried
		n, err := c.list()
		if err != nil || n != 1 {
			t.Fatalf("list %s across the link = %d records, %v; want its one", c.name, n, err)
		}
		if carried := link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToNode).Carried - before; carried >= 64<<10 {
			t.Errorf("listing %s carried %d bytes to the leaf: a record outside its filter crossed", c.name,
				carried)
		}
	}
}
