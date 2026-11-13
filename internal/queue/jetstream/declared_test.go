package jetstream

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// A TEST'S BROKER DECLARES ITS SIZE, whichever way it is started: in a test
// binary an embedded broker with a store directory and no StoreMaxBytes
// refuses to start, naming the declarations that answer it and the one way
// out, while one with a declared limit starts and so does one whose streams
// are in memory. One started under SizedByTheDisk starts and holds the limit
// nats-server derived from the volume, which is what such a case is about; an
// empty reason opts nothing out.
//
// Mutations: check the size in Open alone (StartServer and OpenCoordination
// start undeclared brokers); read an empty reason as an opt-out (the
// reasonless row starts).
func TestATestsBrokerDeclaresItsSize(t *testing.T) {
	t.Parallel()
	for way, start := range brokerStarts {
		t.Run(way, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name   string
				ctx    func(context.Context) context.Context
				cfg    func(dir string) Config
				refuse bool
			}{
				{"a store directory and no limit", nil,
					func(dir string) Config { return Config{StoreDir: dir} }, true},
				{"a store directory and an empty reason",
					func(ctx context.Context) context.Context { return SizedByTheDisk(ctx, "") },
					func(dir string) Config { return Config{StoreDir: dir} }, true},
				{"a declared limit", nil,
					func(dir string) Config {
						return Config{StoreDir: dir, StoreMaxBytes: brokertest.StoreMaxBytes}
					}, false},
				{"streams in memory", nil, func(string) Config { return Config{} }, false},
				{"a case about the derivation",
					func(ctx context.Context) context.Context {
						return SizedByTheDisk(ctx, "this case is about the limit nats-server derives")
					},
					func(dir string) Config { return Config{StoreDir: dir} }, false},
			} {
				ctx := t.Context()
				if tc.ctx != nil {
					ctx = tc.ctx(ctx)
				}
				dir := brokertest.TempDir(t)
				stop, err := start(ctx, tc.cfg(dir))
				if !tc.refuse {
					if err != nil {
						t.Errorf("%s: %s refused: %v", tc.name, way, err)
					} else {
						stop()
					}
					continue
				}
				if err == nil {
					stop()
					t.Errorf("%s: %s started a broker that sizes itself from the machine's disk", tc.name, way)
					continue
				}
				if !errors.Is(err, ErrUndeclaredSize) {
					t.Errorf("%s: %s answered %v, want ErrUndeclaredSize", tc.name, way, err)
				}
				for _, want := range []string{dir, "brokertest.StoreIn", "brokertest.StoreMaxBytes",
					"jetstream.SizedByTheDisk"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s: the refusal does not name %s: %v", tc.name, want, err)
					}
				}
			}
		})
	}
}

// A TEST'S BROKER KEEPS ITS STORE IN A brokertest.TempDir, whichever way it is
// started: in a test binary an embedded broker whose store directory is
// neither one brokertest.TempDir made nor under one refuses to start, naming
// the directory and brokertest.TempDir, and before it writes anything there,
// since a t.TempDir's removal tries once and a stopped broker's last write can
// land after it stopped (crewlet/crewlet#1079). One in such a directory, or
// under it, starts, and so does one whose streams are in memory.
//
// Mutations: start the broker without the check (the t.TempDir rows start);
// read only the store directory's own name (the row under a brokertest.TempDir
// is refused).
func TestATestsBrokerKeepsItsStoreInATempDir(t *testing.T) {
	t.Parallel()
	for way, start := range brokerStarts {
		t.Run(way, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name   string
				dir    func(t *testing.T) string
				refuse bool
			}{
				{"a t.TempDir", func(t *testing.T) string { return t.TempDir() }, true},
				{"a directory under a t.TempDir",
					func(t *testing.T) string { return filepath.Join(t.TempDir(), "stream") }, true},
				{"a brokertest.TempDir", func(t *testing.T) string { return brokertest.TempDir(t) }, false},
				{"a directory under a brokertest.TempDir",
					func(t *testing.T) string { return filepath.Join(brokertest.TempDir(t), "stream") }, false},
				{"streams in memory", func(*testing.T) string { return "" }, false},
			} {
				dir := tc.dir(t)
				cfg := Config{StoreDir: dir}
				if dir != "" {
					cfg.StoreMaxBytes = brokertest.StoreMaxBytes
				}
				stop, err := start(t.Context(), cfg)
				if !tc.refuse {
					if err != nil {
						t.Errorf("%s: %s refused: %v", tc.name, way, err)
					} else {
						stop()
					}
					continue
				}
				if err == nil {
					stop()
					t.Errorf("%s: %s started a broker whose store a late write can keep from "+
						"being removed", tc.name, way)
					continue
				}
				if !errors.Is(err, ErrStoreOutsideTempDir) {
					t.Errorf("%s: %s answered %v, want ErrStoreOutsideTempDir", tc.name, way, err)
				}
				for _, want := range []string{dir, "brokertest.TempDir"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s: the refusal does not name %s: %v", tc.name, want, err)
					}
				}
				// REFUSED BEFORE ANYTHING IS WRITTEN THERE, the store
				// directory's lock file included, so the case's own
				// removal of it has nothing a broker wrote to meet.
				if entries, readErr := os.ReadDir(dir); len(entries) != 0 ||
					(readErr != nil && !errors.Is(readErr, fs.ErrNotExist)) {
					t.Errorf("%s: %s wrote %d entries in the store it refused (read: %v)",
						tc.name, way, len(entries), readErr)
				}
			}
		})
	}
}

// brokerStarts is every way a case starts an embedded broker from a Config —
// a queue opened, a server started and a coordination store opened offline,
// each reaching startEmbedded — with how to stop what it started.
var brokerStarts = map[string]func(context.Context, Config) (func(), error){
	"a queue opened": func(ctx context.Context, cfg Config) (func(), error) {
		q, err := Open(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return func() { _ = q.Stop(context.WithoutCancel(ctx)) }, nil
	},
	"a server started": func(ctx context.Context, cfg Config) (func(), error) {
		s, err := StartServer(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return s.Shutdown, nil
	},
	"a coordination store opened offline": func(ctx context.Context, cfg Config) (func(), error) {
		c, err := OpenCoordination(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return c.Close, nil
	},
}

// A CASE ABOUT THE DERIVATION GETS THE DERIVED LIMIT: the broker it starts
// under SizedByTheDisk holds to what nats-server took from the volume, never to
// a declared figure — so opting out changes nothing about the broker but
// whether a test may start it.
func TestABrokerSizedByTheDiskHoldsTheDerivedLimit(t *testing.T) {
	t.Parallel()
	q, err := Open(SizedByTheDisk(t.Context(), "the case reads back the limit the volume derives"),
		Config{StoreDir: brokertest.TempDir(t)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	claim, found, err := q.VolumeClaim()
	if err != nil || !found {
		t.Fatalf("VolumeClaim = (%+v, %t, %v), want the broker's claim on its volume", claim, found, err)
	}
	if claim.Limit <= 0 || claim.Limit == brokertest.StoreMaxBytes {
		t.Errorf("a broker sized by the disk holds to %d, want the limit nats-server derived from the "+
			"volume's free space", claim.Limit)
	}
}
