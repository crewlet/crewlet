package queries_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/integration"
)

// THE ADVISOR'S SIGNATURE MOVES WITH AN INTEGRATION'S FINDINGS, READ BY THEIR
// DIGEST: the duty reads it every tick, so it reads each surface's status
// alone, which names its findings by SHA-256, and never the findings
// themselves. Findings that change change the signature; the same findings
// leave it as it was.
//
// Mutation: read the states with their findings (Reconciles) and hash them,
// or leave the digest out of the signature, and a check below fails.
func TestTheAdvisorSignatureMovesWithTheFindingsDigest(t *testing.T) {
	t.Parallel()
	surface, _ := configSurface(t, companyDoc)
	var withFindings atomic.Int32
	signed := func(digest string) string {
		t.Helper()
		s := queries.Sources{Config: surface,
			Reconciles: func(context.Context) []integration.State {
				withFindings.Add(1)
				return []integration.State{{Kind: integration.KindGitLab}}
			},
			ReconcileStatuses: func(context.Context) []integration.State {
				return []integration.State{{Kind: integration.KindGitLab,
					FindingsRecord: integration.FindingsRecord{Bytes: 40, SHA256: digest, Count: 1}}}
			}}
		sig, err := s.SetupSignature(t.Context())
		if err != nil {
			t.Fatalf("signature: %v", err)
		}
		return sig
	}
	first, again, moved := signed("aa"), signed("aa"), signed("bb")
	if first != again {
		t.Errorf("the same findings signed %q, then %q: want one signature", first, again)
	}
	if first == moved {
		t.Errorf("findings of another digest left the signature at %q", first)
	}
	if n := withFindings.Load(); n != 0 {
		t.Errorf("the signature read the integrations with their findings %d times, want never", n)
	}
}
