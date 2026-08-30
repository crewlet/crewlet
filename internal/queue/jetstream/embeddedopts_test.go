package jetstream

import (
	"testing"

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

	// Replicas is the whole input: a member with no peers to recover from
	// must reach the disk before Publish returns, and a replicated one has
	// already reached a majority by then.
	for _, tc := range []struct {
		name     string
		replicas int
		want     bool
	}{
		{"a solo member has only its own disk", 1, true},
		{"an unset replica count is solo", 0, true},
		{"a replicated member has a quorum instead", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts, _, err := embeddedOptions(Config{StoreDir: t.TempDir(), Replicas: tc.replicas})
			if err != nil {
				t.Fatalf("embeddedOptions: %v", err)
			}
			if opts.SyncAlways != tc.want {
				t.Errorf("replicas=%d gave SyncAlways=%v, want %v",
					tc.replicas, opts.SyncAlways, tc.want)
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
