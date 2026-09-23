package engine_test

import (
	"maps"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
)

// THE IDENTITY DUTIES ARE ARMED WHERE THEIR INPUTS ARE, and nowhere else.
//
// Each of the four had no loop behind it until it was armed here — a sweep
// record nothing published, a probe nobody ran, a failed key delete nobody
// retried, three indexes nobody read — and each of those looked, from every
// other vantage point, exactly like a duty quietly finding nothing to do. So
// the arming decision is held three ways:
//
//  1. A node running the identity domain with no provider arms the three that
//     need no provider, at their stated intervals.
//  2. The same node with a provider configured also arms the probe, at the
//     operator's `deactivation_probe` — the interval that was once dropped on
//     the way from Tier A to the flow.
//  3. A seats-only satellite runs no identity domain and arms nothing.
func TestTheIdentityDutiesAreArmedWhereTheirInputsAre(t *testing.T) {
	t.Parallel()
	storeDir := func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	}
	without := map[string]time.Duration{
		"iam_sweep":     engine.IdentitySweepInterval,
		"iam_claims":    engine.IdentityClaimsInterval,
		"iam_key_shred": engine.IdentityKeysInterval,
	}

	t.Run("no provider", func(t *testing.T) {
		t.Parallel()
		e := newEngine(t, engine.Options{Bootstrap: bootstrap(t, storeDir)})
		if got := e.IdentityDuties(); !maps.Equal(got, without) {
			t.Fatalf("armed %v, want %v", got, without)
		}
	})

	t.Run("a provider", func(t *testing.T) {
		t.Parallel()
		e := newEngine(t, engine.Options{Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			storeDir(b)
			b.API.Auth.Backend = config.AuthBackendOIDC
			b.API.Auth.OIDC = &config.APIOIDC{
				Issuer: "https://idp.example.com", ClientID: "crewlet",
				ClientSecret:         "not-a-real-secret",
				DeactivationProbeRaw: "20m",
			}
		})})
		want := maps.Clone(without)
		want["iam_deactivation_probe"] = 20 * time.Minute
		if got := e.IdentityDuties(); !maps.Equal(got, want) {
			t.Fatalf("armed %v, want %v — the probe at the operator's interval",
				got, want)
		}
		if got := e.IdentityProvider().Config().Probe(); got != 20*time.Minute {
			t.Errorf("the process's provider probes every %s, want the configured 20m", got)
		}
	})

	t.Run("a seats-only satellite", func(t *testing.T) {
		t.Parallel()
		e := newEngine(t, engine.Options{Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			storeDir(b)
			b.Node.Roles = []string{"seats"}
		})})
		if got := e.IdentityDuties(); len(got) != 0 {
			t.Fatalf("a node running no identity domain armed %v", got)
		}
	})
}
