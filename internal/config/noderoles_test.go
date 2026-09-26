package config

import (
	"errors"
	"testing"
)

// statelessDoc is a node without `data`, on the embedded stream, as the
// satellite guide writes it.
const statelessDoc = "node:\n  roles: [seats]\nstore:\n  scratch: true\n" +
	"stream:\n  leaf:\n    urls: [\"nats-leaf://data-a.example.com:7422\"]\n" +
	"coordination:\n  type: embedded-kv\n"

// A NODE THAT HOLDS NOTHING, AND A MEMBER THAT LETS SUCH NODES IN, BOTH LOAD —
// the two sides of the leaf link as the guide writes them.
func TestBothSidesOfTheLeafLinkLoad(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"stateless on the embedded stream": statelessDoc,
		"stateless on an external cluster": "node:\n  roles: [seats]\nstore:\n  scratch: true\n" +
			"stream:\n  type: nats\n  url: nats://nats.example.com:4222\n" +
			"coordination:\n  type: embedded-kv\n",
		"a member with a leaf listener": "stream:\n  store_dir: /var/lib/crewlet/stream\n" +
			"  leaf:\n    port: 7422\ncoordination:\n  type: embedded-kv\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseBootstrap([]byte(doc), EnvOnly()); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// EVERY WAY A NODE'S ROLES CONTRADICT ITS STORE OR ITS BROKER IS REFUSED,
// naming the field to change. Each of them is otherwise a node that boots and
// then either loses the company's history at its next restart or reads an
// estate it does not have.
func TestRolesThatContradictTheNodeAreRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, doc, path string
		kind            error
	}{
		{"a data node with a scratch store",
			"store:\n  scratch: true\n", "store.scratch", ErrConflict},
		{"a data node that joins as a leaf",
			"stream:\n  leaf:\n    urls: [\"nats-leaf://a.example.com:7422\"]\n" +
				"coordination:\n  type: embedded-kv\n",
			"stream.leaf.urls", ErrConflict},
		{"a stateless node without a scratch store",
			"node:\n  roles: [seats]\nstream:\n  leaf:\n    urls: [\"nats-leaf://a.example.com:7422\"]\n" +
				"coordination:\n  type: embedded-kv\n",
			"store.scratch", ErrMissing},
		{"a stateless node with no way into the fleet",
			"node:\n  roles: [seats]\nstore:\n  scratch: true\n",
			"stream.leaf.urls", ErrMissing},
		{"a stateless node running ingress",
			"node:\n  roles: [ingress, seats]\nstore:\n  scratch: true\n" +
				"stream:\n  leaf:\n    urls: [\"nats-leaf://a.example.com:7422\"]\n" +
				"coordination:\n  type: embedded-kv\n",
			"node.roles", ErrConflict},
		{"a stateless node running workers",
			"node:\n  roles: [workers]\nstore:\n  scratch: true\n" +
				"stream:\n  leaf:\n    urls: [\"nats-leaf://a.example.com:7422\"]\n" +
				"coordination:\n  type: embedded-kv\n",
			"node.roles", ErrConflict},
		{"a stateless node keeping a replicated estate",
			"node:\n  roles: [seats]\nstore:\n  scratch: true\n  replicated_path: /var/r.db\n" +
				"stream:\n  leaf:\n    urls: [\"nats-leaf://a.example.com:7422\"]\n" +
				"coordination:\n  type: embedded-kv\n",
			"store", ErrConflict},
		{"a stateless node opening a leaf listener",
			"node:\n  roles: [seats]\nstore:\n  scratch: true\n" +
				"stream:\n  leaf:\n    urls: [\"nats-leaf://a.example.com:7422\"]\n    port: 7422\n" +
				"coordination:\n  type: embedded-kv\n",
			"stream.leaf.port", ErrConflict},
		{"a leaf that is also a cluster member",
			"node:\n  roles: [seats]\nstore:\n  scratch: true\n" +
				"stream:\n  cluster:\n    name: c\n  leaf:\n    urls: [\"nats-leaf://a.example.com:7422\"]\n" +
				"coordination:\n  type: embedded-kv\n",
			"stream.cluster", ErrConflict},
		{"a leaf with a stream store",
			"node:\n  roles: [seats]\nstore:\n  scratch: true\n" +
				"stream:\n  store_dir: /var/js\n  leaf:\n    urls: [\"nats-leaf://a.example.com:7422\"]\n" +
				"coordination:\n  type: embedded-kv\n",
			"stream.store_dir", ErrConflict},
		{"a leaf url that is not a leaf listener",
			"node:\n  roles: [seats]\nstore:\n  scratch: true\n" +
				"stream:\n  leaf:\n    urls: [\"http://a.example.com\"]\n" +
				"coordination:\n  type: embedded-kv\n",
			"stream.leaf.urls[0]", ErrUnknownValue},
		{"a leaf block on an external cluster",
			"node:\n  roles: [seats]\nstore:\n  scratch: true\n" +
				"stream:\n  type: nats\n  url: nats://x:4222\n  leaf:\n    urls: [\"nats-leaf://a.example.com:7422\"]\n" +
				"coordination:\n  type: embedded-kv\n",
			"stream.leaf", ErrConflict},
		{"a leaf listener's host with no port",
			"stream:\n  leaf:\n    host: 10.0.0.4\ncoordination:\n  type: embedded-kv\n",
			"stream.leaf.port", ErrMissing},
		// A MEMBER THAT LETS LEAVES IN IS IN A FLEET, peers or not: the
		// nodes that join it hold presence leases, and one kept in this
		// process is one they never see.
		{"a leaf listener on local coordination",
			"stream:\n  store_dir: /var/js\n  leaf:\n    port: 7422\n", "coordination.type", ErrConflict},
		// AND IT PERSISTS: the nodes that join it keep nothing, so it keeps
		// everything they do.
		{"a leaf listener on an in-memory stream",
			"stream:\n  leaf:\n    port: 7422\ncoordination:\n  type: embedded-kv\n",
			"stream.store_dir", ErrMissing},
		{"a stateless node holding objects",
			"node:\n  roles: [seats]\nstore:\n  scratch: true\n  objects:\n    weight: 2\n" +
				"stream:\n  leaf:\n    urls: [\"nats-leaf://a.example.com:7422\"]\n" +
				"coordination:\n  type: embedded-kv\n",
			"store.objects", ErrConflict},
		{"an object weight past the ceiling",
			"store:\n  objects:\n    weight: 65\n", "store.objects.weight", ErrOutOfRange},
		{"a negative object weight",
			"store:\n  objects:\n    weight: -1\n", "store.objects.weight", ErrOutOfRange},
		{"a stateless node on local coordination",
			"node:\n  roles: [seats]\nstore:\n  scratch: true\n" +
				"stream:\n  leaf:\n    urls: [\"nats-leaf://a.example.com:7422\"]\n",
			"coordination.type", ErrConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, tc.doc, tc.path)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
		})
	}
}

// A DATA NODE OFFERS AN OBJECT SHARE ON ITS PRESENCE LEASE — the default one
// when it names none — and a node without `data` offers none, which is what
// keeps writers from sending it chunks it will not keep.
func TestOnlyADataNodeOffersAnObjectShare(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		boot Bootstrap
		want int
	}{
		{"every role, no block", Bootstrap{}, DefaultObjectWeight},
		{"a weight named", Bootstrap{Store: Store{Objects: StoreObjects{Weight: 5}}}, 5},
		{"no data role", Bootstrap{Node: Node{Roles: []string{"seats"}}}, 0},
	} {
		if got := tc.boot.Profile("n1").ObjectWeight; got != tc.want {
			t.Errorf("%s: object weight %d, want %d", tc.name, got, tc.want)
		}
	}
}

// THE CHUNK DIRECTORY RESOLVES AGAINST THE STORE, for the reason the snapshot
// directory does: the same file is run from a container and from a shell.
func TestTheObjectsDirectoryResolvesAgainstTheStore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ dir, want string }{
		{"", "/var/lib/crewlet/objects"},
		{"chunks", "/var/lib/crewlet/chunks"},
		{"/mnt/objects/", "/mnt/objects"},
	} {
		s := Store{Path: "/var/lib/crewlet/crewlet.db", Objects: StoreObjects{Dir: tc.dir}}
		if got := s.ObjectsDirFor(); got != tc.want {
			t.Errorf("dir %q resolves to %q, want %q", tc.dir, got, tc.want)
		}
	}
}
