package jetstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/queuetest"
)

// A LEAF'S QUEUE IS CERTIFIED ON THE SAME SUITE AS A MEMBER'S.
//
// A node with no `data` role joins the fleet as a leaf: its broker runs no
// JetStream, and every stream, consumer and bucket its clients use is a
// member's, reached across one leaf link in the fleet's domain. Nothing above
// internal/queue may branch on which kind of node it is on — so the only way
// to know the queue it gets behaves identically is to run every case the
// member's queue runs against it.
func TestConformanceThroughALeaf(t *testing.T) {
	t.Parallel()
	queuetest.RunWith(t, func(t *testing.T, opts ...queue.Option) queue.EventQueue {
		return openLeafForTest(t, Config{}, opts...)
	}, capabilitiesFor(func(t *testing.T, cfg Config) *Queue { return openLeafForTest(t, cfg) }))
}

// openLeafForTest starts a member with a leaf listener and a leaf joined to
// it, and returns a client of the LEAF — inspected through the member, which
// is where everything it writes actually lives. The leaf's client is built
// with opts.
func openLeafForTest(t *testing.T, cfg Config, opts ...queue.Option) *Queue {
	t.Helper()
	cfg = testTimings(cfg)

	memberCfg := cfg
	memberCfg.ServerName = "member"
	memberCfg.LeafHost = "127.0.0.1"
	memberCfg.LeafPort = AnyPort
	// A MEMBER THAT SERVES LEAVES PERSISTS — it is refused otherwise — and
	// the leaf creates what it is first to use in the file store to match.
	if memberCfg.StoreDir == "" {
		memberCfg.StoreDir = t.TempDir()
	}
	member, err := StartServer(t.Context(), memberCfg)
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(member.Shutdown)

	leafCfg := cfg
	leafCfg.ServerName = "leaf"
	leafCfg.LeafURLs = []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", member.LeafPort())}
	leaf, err := StartServer(t.Context(), leafCfg)
	if err != nil {
		t.Fatalf("start the leaf: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	return clientUnderTest(t, leaf, member, opts...)
}

// unusedPort is a loopback port nothing held a moment ago, for a leaf that has
// to find NO member listening. A member is never started on one: it binds
// [AnyPort] and is asked which port that was, because a port reserved here is
// released before the member binds it and anything else may take it between.
func unusedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// A LEAF HOLDS NOTHING, so every member setting is refused on one rather than
// ignored: each is a way of holding something, and a leaf quietly given a
// store directory is a node its operator believes is durable.
func TestALeafRefusesEveryMemberSetting(t *testing.T) {
	t.Parallel()
	leaf := Config{ServerName: "leaf", LeafURLs: []string{"nats-leaf://127.0.0.1:7422"}}
	cases := map[string]func(*Config){
		"a cluster":       func(c *Config) { c.ClusterName = "fleet" },
		"a route port":    func(c *Config) { c.ClusterPort = 6222 },
		"route peers":     func(c *Config) { c.ClusterURLs = []string{"nats://peer:6222"} },
		"a leaf listener": func(c *Config) { c.LeafPort = 7422 },
		"a store dir":     func(c *Config) { c.StoreDir = t.TempDir() },
		"no server name":  func(c *Config) { c.ServerName = "" },
	}
	for name, mutate := range cases {
		cfg := leaf
		mutate(&cfg)
		if _, _, err := embeddedOptions(cfg, systemUser{}); err == nil {
			t.Errorf("a leaf with %s was accepted", name)
		}
	}
	// THE CONTROL: the leaf itself builds, with JetStream off and no
	// listener, or the refusals above pass on a function that refuses
	// every leaf.
	opts, scratch, err := embeddedOptions(leaf, systemUser{})
	if err != nil {
		t.Fatalf("a plain leaf was refused: %v", err)
	}
	if opts.JetStream || !opts.DontListen || scratch != "" || len(opts.LeafNode.Remotes) != 1 {
		t.Fatalf("a leaf built as JetStream=%t DontListen=%t scratch=%q remotes=%d — "+
			"want no JetStream, no listener, nothing on disk and one link",
			opts.JetStream, opts.DontListen, scratch, len(opts.LeafNode.Remotes))
	}
}

// EVERY MEMBER SERVES THE FLEET'S DOMAIN, or a leaf has no JetStream to
// address across its link.
func TestEveryMemberServesTheFleetsDomain(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]Config{
		"solo":      {},
		"clustered": {ClusterName: "fleet", ServerName: "a", ClusterPort: 6222},
		// A LEAF LISTENER NEEDS A STORE DIRECTORY: the nodes joining it
		// keep nothing, so this member keeps everything they do.
		"with a leaf listener": {ServerName: "a", LeafPort: 7422, StoreDir: t.TempDir()},
	} {
		opts, scratch, err := embeddedOptions(cfg, systemUser{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		removeScratch(scratch)
		if opts.JetStreamDomain != jsapi.Domain {
			t.Errorf("a %s member serves domain %q, want %q", name,
				opts.JetStreamDomain, jsapi.Domain)
		}
	}
}

// A MEMBER ASKED FOR ANY LEAF PORT BINDS ONE AND SAYS WHICH, and the port it
// names is the one accepting. A member with no leaf listener names none, and a
// negative port other than [AnyPort] is refused rather than handed to
// nats-server as a port to bind.
func TestAMemberOnAnyLeafPortSaysWhichItBound(t *testing.T) {
	t.Parallel()
	member, err := StartServer(t.Context(), testTimings(Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: AnyPort, StoreDir: t.TempDir()}))
	if err != nil {
		t.Fatalf("start a member on any leaf port: %v", err)
	}
	t.Cleanup(member.Shutdown)
	port := member.LeafPort()
	if port <= 0 {
		t.Fatalf("a member asked for any leaf port names %d, so no leaf can be "+
			"told where to join it", port)
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("the leaf port the member named, %d, accepts nothing: %v", port, err)
	}
	_ = conn.Close()

	plain, err := StartServer(t.Context(), testTimings(Config{}))
	if err != nil {
		t.Fatalf("start a member with no leaf listener: %v", err)
	}
	t.Cleanup(plain.Shutdown)
	if got := plain.LeafPort(); got != 0 {
		t.Errorf("a member with no leaf listener names leaf port %d", got)
	}

	if _, _, err := embeddedOptions(Config{ServerName: "member", LeafPort: -2,
		StoreDir: t.TempDir()}, systemUser{}); err == nil {
		t.Error("a leaf port of -2 was accepted")
	}
}

// A LEAF THAT CANNOT REACH A MEMBER SAYS SO, rather than booting and then
// timing out on its first stream with a message about the stream — and what
// it says is the SETTING to fix.
//
// That sentence is reached only when the leaf's own wait runs out — sixty
// seconds in production — so the wait is shortened on the server itself and
// the caller's context is left long. Cut short by the caller instead, which is
// how this case used to run, the wait ends with the caller's deadline and
// names nothing: and the assertion then read "leaf", which every error this
// wait can return contains, so it passed whatever the message said.
func TestALeafWithNoMemberToReachNamesTheLink(t *testing.T) {
	t.Parallel()
	cfg := testTimings(Config{
		ServerName: "edge-1",
		LeafURLs:   []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", unusedPort(t))},
	})
	e, err := startEmbedded(t.Context(), cfg)
	if err != nil {
		t.Fatalf("start the leaf: %v", err)
	}
	t.Cleanup(e.shutdown)
	e.ready = readiness{timeout: 300 * time.Millisecond, poll: 10 * time.Millisecond,
		ask: 100 * time.Millisecond}

	q, err := newQueueOn(t.Context(), cfg, e, false, queue.Resolve())
	if err == nil {
		_ = q.Stop(context.WithoutCancel(t.Context()))
		t.Fatal("a leaf with no member listening opened a queue")
	}
	for _, want := range []string{"stream.leaf.urls", "made no link"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q, so an operator is not told "+
				"which setting reaches no member:\n%v", want, err)
		}
	}
}

// A LEAF WHOSE LINK IS DOWN SAYS SO, while its own connection still reads as
// healthy. The connection is to the broker in this process, which stays up when
// the member across the link goes away — so a readiness check that asked only
// the connection would call a leaf that can reach no stream ready. And a
// stopped client is not linked to anything.
func TestALeafReportsItsLinkGoingDown(t *testing.T) {
	t.Parallel()
	cfg := testTimings(Config{})
	memberCfg := cfg
	memberCfg.ServerName = "member"
	memberCfg.LeafHost = "127.0.0.1"
	memberCfg.LeafPort = unusedPort(t)
	memberCfg.StoreDir = t.TempDir()
	member, err := StartServer(t.Context(), memberCfg)
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(member.Shutdown)

	leafCfg := cfg
	leafCfg.ServerName = "leaf"
	leafCfg.LeafURLs = []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", memberCfg.LeafPort)}
	leaf, err := StartServer(t.Context(), leafCfg)
	if err != nil {
		t.Fatalf("start the leaf: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	q, err := leaf.Client(t.Context())
	if err != nil {
		t.Fatalf("leaf client: %v", err)
	}

	if err := q.Link(); err != nil {
		t.Fatalf("a leaf linked to a running member reports %v", err)
	}
	member.Shutdown()
	deadline := time.Now().Add(10 * time.Second)
	for q.Link() == nil {
		if time.Now().After(deadline) {
			t.Fatal("a leaf whose only member shut down still reports its link up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := q.Link(); !strings.Contains(err.Error(), "leaf") {
		t.Errorf("the link error %q does not name the leaf link", err)
	}

	if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := q.Link(); !errors.Is(err, ErrClosed) {
		t.Errorf("a stopped client reports its link as %v, want ErrClosed", err)
	}
}

// A MEMBER'S BROKER IS IN ITS OWN PROCESS, so its client is linked for as long
// as it runs.
func TestAMemberIsLinkedWhileItRuns(t *testing.T) {
	t.Parallel()
	q := openForTest(t, Config{})
	if err := q.Link(); err != nil {
		t.Errorf("a solo member's client reports %v", err)
	}
}
