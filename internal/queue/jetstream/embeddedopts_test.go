package jetstream

import (
	"errors"
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
		opts, _, err := embeddedOptions(Config{StoreDir: t.TempDir()}, systemUser{})
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
			opts, _, err := embeddedOptions(cfg, systemUser{})
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

// ONLY A CLUSTERED MEMBER DECLARES A SYSTEM USER, and it declares a public key
// and nothing that authenticates as it.
//
// A solo member has no metadata group to change and keeps the posture it always
// had; a leaf runs no JetStream. A clustered member declares the system account
// under its default name — the metadata group's own state is stored under that
// name — with one nkey user and no password user of its own: the server warns
// on every start that declares a plaintext password, and the no-auth user that
// binds every anonymous client to the global account is the server's to add.
func TestOnlyAClusteredMemberDeclaresASystemUser(t *testing.T) {
	t.Parallel()
	system, err := newSystemUser()
	if err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]Config{
		"a solo member": {StoreDir: t.TempDir()},
		"a leaf":        {ServerName: "leaf", LeafURLs: []string{"nats-leaf://127.0.0.1:7422"}},
	} {
		opts, scratch, err := embeddedOptions(cfg, system)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		removeScratch(scratch)
		if len(opts.Nkeys) != 0 || len(opts.Users) != 0 || len(opts.Accounts) != 0 ||
			opts.SystemAccount != "" {
			t.Errorf("%s declares nkeys %v, users %v, accounts %v, system account %q", name,
				opts.Nkeys, opts.Users, opts.Accounts, opts.SystemAccount)
		}
	}
	member := Config{ServerName: "a", ClusterName: "c", StoreDir: t.TempDir()}
	opts, _, err := embeddedOptions(member, system)
	if err != nil {
		t.Fatal(err)
	}
	if opts.SystemAccount != "$SYS" || len(opts.Nkeys) != 1 || opts.Nkeys[0].Account == nil ||
		opts.Nkeys[0].Account.Name != "$SYS" || opts.Nkeys[0].Nkey != system.public {
		t.Fatalf("a clustered member declares system account %q and nkeys %+v, want its "+
			"one public key on $SYS", opts.SystemAccount, opts.Nkeys)
	}
	if len(opts.Users) != 0 || opts.NoAuthUser != "" {
		t.Errorf("a clustered member declares password users %+v and no-auth user %q: a "+
			"plaintext password is warned about on every start, and the no-auth user "+
			"is the server's to add", opts.Users, opts.NoAuthUser)
	}
	other, err := newSystemUser()
	if err != nil {
		t.Fatal(err)
	}
	if other.public == system.public {
		t.Error("two starts minted one key: each start mints its own")
	}
}

// A BROKER WITH NO METADATA GROUP SAYS SO — a solo member and a leaf alike —
// rather than answering a group of one or removing nothing successfully.
func TestABrokerWithNoMetadataGroupSaysSo(t *testing.T) {
	t.Parallel()
	srv, err := StartServer(t.Context(), Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	if _, err := srv.MetaGroup(); !errors.Is(err, ErrNoMetaGroup) {
		t.Errorf("a solo member's metadata group read answered %v", err)
	}
	if err := srv.RemovePeer(t.Context(), "node-b"); !errors.Is(err, ErrNoMetaGroup) {
		t.Errorf("a solo member's removal answered %v", err)
	}
}
