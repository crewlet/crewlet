package jetstream

import (
	"context"
	"errors"
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
	for way, start := range map[string]func(context.Context, Config) (func(), error){
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
	} {
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
