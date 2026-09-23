package oidc_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam/oidc"
)

// probeAudit keeps what a probe announced.
type probeAudit struct {
	mu   sync.Mutex
	seen []events.Payload
}

func (a *probeAudit) Emit(_ context.Context, payload events.Payload) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, payload)
}

// A SESSION THE PROBE ENDS IS ANNOUNCED AS `idp_revoked`, AND ONLY ONE IT ENDED.
//
// The provider's verdict is the one way a central off-boarding reaches this
// engine, and the live feed is where an administrator watching somebody's
// departure sees it land. An ending that failed to record is not announced —
// its session is still live. Mutation: announce before the End call returns
// and the failed-end case announces one.
func TestAnEndedSessionIsAnnouncedAsIdpRevoked(t *testing.T) {
	t.Parallel()
	idp := newIssuer(t)
	idp.refusal = "invalid_grant"
	sessions := newSessions(oidc.LiveSession{
		Lineage: "lin-1", Person: "p-1", Refresh: "refresh-1",
	})
	audit := &probeAudit{}
	prober := oidc.NewProber(
		oidc.NewProvider(idp.config(), idp.Client(), func() time.Time { return at }),
		sessions, quiet()).WithAudit(audit)
	if _, _, err := prober.Run(t.Context()); err != nil {
		t.Fatalf("the pass failed: %v", err)
	}
	if len(audit.seen) != 1 {
		t.Fatalf("announced %d events, want 1", len(audit.seen))
	}
	row, ok := audit.seen[0].(types.IAMSessionEnded)
	if !ok || row.Reason != types.EndIdPRevoked || row.Lineage != "lin-1" ||
		row.Person != "p-1" || row.By != "" {
		t.Errorf("announced %#v", audit.seen[0])
	}
	if oidc.ReasonIdPRevoked != string(types.EndIdPRevoked) {
		t.Errorf("the record says %q and the event %q: one fact, two spellings",
			oidc.ReasonIdPRevoked, types.EndIdPRevoked)
	}

	failing := newSessions(oidc.LiveSession{
		Lineage: "lin-2", Person: "p-2", Refresh: "refresh-2",
	})
	failing.endErr = errors.New("the log is unreachable")
	unlanded := &probeAudit{}
	prober = oidc.NewProber(
		oidc.NewProvider(idp.config(), idp.Client(), func() time.Time { return at }),
		failing, quiet()).WithAudit(unlanded)
	if _, _, err := prober.Run(t.Context()); err != nil {
		t.Fatalf("the pass failed: %v", err)
	}
	if len(unlanded.seen) != 0 {
		t.Errorf("an ending that did not record was announced: %v", unlanded.seen)
	}
}
