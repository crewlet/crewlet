package jetstream

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
)

// TestTheEmbeddedServerCarriesWhatTheContractPromises pins the two options
// whose absence is invisible until the day they are needed.
//
// Neither can be caught by an ordinary suite. An unset MaxPayload leaves the
// broker at nats-server's 1 MiB default, which every test payload fits inside
// — the failure is one oversized delivery in production, refused forever. An
// unset SyncAlways leaves the file store on a 2-minute background flush, and
// nothing short of cutting power to the host tells the difference.
//
// So they are asserted where they are decided, on the options a Config
// produces, rather than through behaviour that cannot be provoked.
func TestTheEmbeddedServerCarriesWhatTheContractPromises(t *testing.T) {
	t.Parallel()

	t.Run("the payload ceiling is the contract's, not the broker's default", func(t *testing.T) {
		t.Parallel()
		opts, _, err := embeddedOptions(Config{StoreDir: t.TempDir()})
		if err != nil {
			t.Fatalf("embeddedOptions: %v", err)
		}
		if int(opts.MaxPayload) != queue.MaxPayloadBytes {
			t.Errorf("MaxPayload = %d, want queue.MaxPayloadBytes (%d): a producer "+
				"that sized itself against the contract would have its publish "+
				"refused by the broker",
				opts.MaxPayload, queue.MaxPayloadBytes)
		}
	})

	// THE FSYNC IS THE OPERATOR'S SETTING, not an inference from the
	// replica count. The inference this replaces — a replicated member has
	// a quorum, so it needs no fsync — is true of one failure class and was
	// applied to five: a rack losing power takes a majority together, and a
	// three-node fleet on one rack is what a first production deployment
	// looks like.
	//
	// The replica count is asserted to have NO effect here, which is the
	// half a "does it pass through" test would miss.
	for _, tc := range []struct {
		name     string
		cfg      Config
		want     bool
		wantSync time.Duration
	}{
		{"unset declines the fsync", Config{}, false, 0},
		{"set at R1", Config{SyncAlways: true, Replicas: 1}, true, 0},
		{"set at R3, where the inference used to refuse it",
			Config{SyncAlways: true, Replicas: 3}, true, 0},
		{"unset at R1, where the inference used to force it",
			Config{Replicas: 1}, false, 0},
		{"an interval is passed through when the fsync is declined",
			Config{SyncInterval: 30 * time.Second}, false, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			cfg.StoreDir = t.TempDir()
			opts, _, err := embeddedOptions(cfg)
			if err != nil {
				t.Fatalf("embeddedOptions: %v", err)
			}
			if opts.SyncAlways != tc.want {
				t.Errorf("SyncAlways = %v, want %v: the fsync per write is "+
					"stream.sync's answer and nothing else's",
					opts.SyncAlways, tc.want)
			}
			if opts.SyncInterval != tc.wantSync {
				t.Errorf("SyncInterval = %v, want %v: an operator who declines "+
					"the fsync is choosing a window, and leaving it unset "+
					"takes the server's two minutes instead",
					opts.SyncInterval, tc.wantSync)
			}
		})
	}
}

// TestTheClientIsToldTheCeiling is the other half: setting MaxPayload on the
// server is only useful because the client learns it from the server's INFO
// and refuses an oversized publish itself. That local refusal is what makes an
// over-limit delivery a reportable error rather than a dropped connection.
func TestTheClientIsToldTheCeiling(t *testing.T) {
	t.Parallel()
	e, err := startEmbedded(t.Context(), Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("startEmbedded: %v", err)
	}
	t.Cleanup(e.shutdown)

	nc, err := e.connect()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	if got := nc.MaxPayload(); got != int64(queue.MaxPayloadBytes) {
		t.Errorf("the client sees a ceiling of %d, want %d", got, queue.MaxPayloadBytes)
	}
}
