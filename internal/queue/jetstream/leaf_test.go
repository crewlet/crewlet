package jetstream

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

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
	queuetest.RunWith(t, func(t *testing.T) queue.EventQueue {
		return openLeafForTest(t, Config{})
	}, capabilitiesFor(openLeafForTest))
}

// openLeafForTest starts a member with a leaf listener and a leaf joined to
// it, and returns a client of the LEAF — inspected through the member, which
// is where everything it writes actually lives.
func openLeafForTest(t *testing.T, cfg Config) *Queue {
	t.Helper()
	cfg = testTimings(cfg)

	memberCfg := cfg
	memberCfg.ServerName = "member"
	memberCfg.LeafHost = "127.0.0.1"
	memberCfg.LeafPort = unusedPort(t)
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
	leafCfg.LeafURLs = []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", memberCfg.LeafPort)}
	leaf, err := StartServer(t.Context(), leafCfg)
	if err != nil {
		t.Fatalf("start the leaf: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	return clientUnderTest(t, leaf, member)
}

// unusedPort is a loopback port nothing held a moment ago. The race to bind
// it is the test's to lose, and the member's own probe names it if it does.
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
		if _, _, err := embeddedOptions(cfg); err == nil {
			t.Errorf("a leaf with %s was accepted", name)
		}
	}
	// THE CONTROL: the leaf itself builds, with JetStream off and no
	// listener, or the refusals above pass on a function that refuses
	// every leaf.
	opts, scratch, err := embeddedOptions(leaf)
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
		opts, scratch, err := embeddedOptions(cfg)
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

// A LEAF THAT CANNOT REACH A MEMBER SAYS SO, rather than booting and then
// timing out on its first stream with a message about the stream.
func TestALeafWithNoMemberToReachNamesTheLink(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), clusterReadyTimeout/10)
	defer cancel()
	_, err := Open(ctx, testTimings(Config{
		ServerName: "leaf",
		LeafURLs:   []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", unusedPort(t))},
	}))
	if err == nil {
		t.Fatal("a leaf with no member listening opened a queue")
	}
	if !strings.Contains(err.Error(), "leaf") {
		t.Errorf("the refusal %q does not name the leaf link", err)
	}
}
