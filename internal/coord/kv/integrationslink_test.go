package kv_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/objstore"
)

// integrationsStream is the integrations bucket's stream under kvtest's
// prefix.
const integrationsStream = "KV_slowleaf_integrations"

// integrationsLinkRate is a link's pace for the bucket's classes at their
// bounds: above the floor, and slow enough that the findings' reply at its
// bound (about 88 KB) held a second behind another outlasts a term read's
// re-ask, while a status's (about 23 KB) crosses inside the term.
const integrationsLinkRate = 36 << 10

// EVERY INTEGRATIONS CLASS AT ITS BOUND IS READ ACROSS A LEAF'S LINK AT ITS
// FIRST ASK. A surface's status at coord.MaxIntegrationStatusBytes and its
// findings at coord.MaxIntegrationFindingsBytes, on a link at or above the
// slowest pace allowed: the status pass and the findings pass each deliver
// their record asking the leader nothing, the status by its key asks once at
// the term, and the findings by their key, held a second behind a reply sent
// ahead of them, ask once. Requests are counted as they reach the member.
//
// Mutation: read the findings at the term (replyTerm), and their read by key
// asks again or fails.
func TestEveryIntegrationsClassAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// behind is how long the link holds the member's bytes as the read
		// begins: none, or a second past a term read's re-ask.
		behind time.Duration
		asks   int
		bound  int
		read   func(ctx context.Context, across kvtest.Across) ([]byte, error)
	}{
		{"the status pass", 0, 0, coord.MaxIntegrationStatusBytes,
			func(ctx context.Context, across kvtest.Across) ([]byte, error) {
				all, err := across.Leaf.IntegrationStatuses(ctx)
				return all["gitlab"], err
			}},
		{"a status by its key", 0, 1, coord.MaxIntegrationStatusBytes,
			func(ctx context.Context, across kvtest.Across) ([]byte, error) {
				value, _, err := across.Leaf.IntegrationStatus(ctx, "gitlab")
				return value, err
			}},
		{"the findings by their key", time.Second, 1, coord.MaxIntegrationFindingsBytes,
			func(ctx context.Context, across kvtest.Across) ([]byte, error) {
				value, _, err := across.Leaf.IntegrationFindingsOf(ctx, "gitlab")
				return value, err
			}},
		{"the findings pass", 0, 0, coord.MaxIntegrationFindingsBytes,
			func(ctx context.Context, across kvtest.Across) ([]byte, error) {
				all, err := across.Leaf.IntegrationFindings(ctx)
				return all["gitlab"], err
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if floor := int64(1<<20) * int64(time.Second) / int64(objstore.MiBPace); integrationsLinkRate < floor {
				t.Fatalf("premise: the link carries %d bytes a second, below the floor's %d",
					integrationsLinkRate, floor)
			}
			across := kvtest.SlowLeaf(t, integrationsLinkRate)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			status := []byte(`"` + randomText(t, coord.MaxIntegrationStatusBytes-2) + `"`)
			findings := []byte(`"` + randomText(t, coord.MaxIntegrationFindingsBytes-2) + `"`)
			if err := across.Member.RecordIntegrationPass(ctx, "gitlab", status, findings); err != nil {
				t.Fatalf("record a pass at the classes' bounds through the member: %v", err)
			}
			asked := leaderReads(t, across, integrationsStream)
			if c.behind > 0 {
				across.Link.Delay(c.behind)
			}
			got, err := c.read(ctx, across)
			if err != nil || len(got) != c.bound {
				t.Fatalf("%s across the link = %d bytes, %v; want its %d", c.name, len(got), err, c.bound)
			}
			if n := asked(); n != c.asks {
				t.Errorf("%s at its bound across the link asked the leader %d times, want %d", c.name, n, c.asks)
			}
		})
	}
}
