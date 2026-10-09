package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/seat/placement"
)

// brokerStreams is one stream block per way a node's broker can take part in
// the fleet's, each otherwise valid — the member a fleet member, persisted, so
// the member-in-a-fleet rule is not what any case below is about.
var brokerStreams = map[placement.BrokerKind]string{
	placement.BrokerMember: "stream:\n  store_dir: /var/lib/crewlet/stream\n  replicas: 3\n" +
		"  cluster:\n    name: acme\n    peers: [nats://b.example.com:6222, nats://c.example.com:6222]\n" +
		"coordination:\n  type: embedded-kv\n",
	placement.BrokerLeaf: "stream:\n  leaf:\n    urls: [nats-leaf://data-a.example.com:7422]\n" +
		"coordination:\n  type: embedded-kv\n",
	placement.BrokerClient: "stream:\n  type: nats\n  url: nats://nats.example.com:4222\n" +
		"coordination:\n  type: embedded-kv\n",
}

// TestBrokerMembershipComesFromTheStreamBlock: a node's broker kind is DERIVED
// from its stream block, whatever its roles, and it is what the node advertises
// on its presence.
//
// Derived rather than declared, because a setting could say `member` over a
// stream block that starts a leaf — and the capacity seal, which counts every
// member, would then wait on a node whose broker holds nothing, or pass
// without one that does.
func TestBrokerMembershipComesFromTheStreamBlock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		stream func(*Stream)
		want   placement.BrokerKind
	}{
		{"a solo embedded node", func(*Stream) {}, placement.BrokerMember},
		{"an embedded member of a cluster", func(s *Stream) {
			s.Cluster = StreamCluster{Name: "acme", Peers: []string{"nats://b.example.com:6222"}}
		}, placement.BrokerMember},
		{"a member opening a leaf listener", func(s *Stream) { s.Leaf.Port = 7422 },
			placement.BrokerMember},
		{"an unset stream type", func(s *Stream) { s.Type = "" }, placement.BrokerMember},
		{"an embedded node joining through leaf urls", func(s *Stream) {
			s.Leaf.URLs = []string{"nats-leaf://data-a.example.com:7422"}
		}, placement.BrokerLeaf},
		{"an external cluster", func(s *Stream) {
			s.Type, s.URL = StreamNATS, "nats://nats.example.com:4222"
		}, placement.BrokerClient},
	} {
		// EVERY ROLE SET, the refused pairings included: the kind of a
		// stream block is no less definite for Tier A refusing what the
		// roles pair it with.
		for _, roles := range [][]string{nil, {"data"}, {"seats"}, {"ingress", "workers"}} {
			t.Run(fmt.Sprintf("%s/%v", tc.name, roles), func(t *testing.T) {
				t.Parallel()
				b := DefaultBootstrap()
				b.Node.Roles = roles
				tc.stream(&b.Stream)
				if got := b.BrokerKind(); got != tc.want {
					t.Fatalf("broker kind %q, want %q: the roles must not decide it", got, tc.want)
				}
				if got := b.Profile("n1").Broker; got != tc.want {
					t.Fatalf("the presence profile advertises %q over a stream block that "+
						"makes a %q", got, tc.want)
				}
			})
		}
	}
}

// EVERY BROKER KIND × EVERY ROLE SET, with exactly the refusals each earns.
//
// The table is the rule set [Bootstrap.checkRolesAndBroker] states, written as
// outcomes. Three pairings are refused — a data node on a leaf, a member
// without data, and ingress or workers without data — because every data node
// holds the whole estate as a member of the broker and a node without data
// holds none; and each refusal has to NAME the data role it turns on, or an
// operator is told what is wrong and not which of the two ways out is theirs.
func TestEveryBrokerKindAndRoleCombination(t *testing.T) {
	t.Parallel()
	type refusal struct{ path, kind string }
	var (
		ok               []refusal
		leafDataNode     = refusal{"stream.leaf.urls", "conflict"}
		datalessMember   = refusal{"stream.leaf.urls", "missing"}
		statelessIngress = refusal{"node.roles", "conflict"}
	)
	for _, kind := range placement.BrokerKinds() {
		for _, tc := range []struct {
			roles string
			// want is the refusals this pairing earns, every one of them
			// naming the data role.
			want map[placement.BrokerKind][]refusal
		}{
			{"", map[placement.BrokerKind][]refusal{
				placement.BrokerLeaf: {leafDataNode},
			}},
			{"[data]", map[placement.BrokerKind][]refusal{
				placement.BrokerLeaf: {leafDataNode},
			}},
			{"[data, ingress, workers]", map[placement.BrokerKind][]refusal{
				placement.BrokerLeaf: {leafDataNode},
			}},
			{"[seats]", map[placement.BrokerKind][]refusal{
				placement.BrokerMember: {datalessMember},
			}},
			{"[ingress]", map[placement.BrokerKind][]refusal{
				placement.BrokerMember: {datalessMember, statelessIngress},
				placement.BrokerLeaf:   {statelessIngress},
				placement.BrokerClient: {statelessIngress},
			}},
			{"[workers, seats]", map[placement.BrokerKind][]refusal{
				placement.BrokerMember: {datalessMember, statelessIngress},
				placement.BrokerLeaf:   {statelessIngress},
				placement.BrokerClient: {statelessIngress},
			}},
		} {
			doc := brokerStreams[kind]
			if tc.roles != "" {
				doc = "node:\n  roles: " + tc.roles + "\n" + doc
			}
			if !strings.Contains(tc.roles, "data") && tc.roles != "" {
				doc += "store:\n  scratch: true\n"
			}
			want := tc.want[kind]
			if want == nil {
				want = ok
			}
			t.Run(fmt.Sprintf("%s/roles %s", kind, orEvery(tc.roles)), func(t *testing.T) {
				t.Parallel()
				_, err := ParseBootstrap([]byte(doc), EnvOnly())
				var got []refusal
				for _, p := range Problems(err) {
					got = append(got, refusal{p.Path, p.Kind})
					if !strings.Contains(p.Message, "data") {
						t.Errorf("%s is refused without naming the data role it turns "+
							"on: %s", p.Path, p.Message)
					}
				}
				if !slices.Equal(got, want) {
					t.Fatalf("refusals %v, want %v\n%v", got, want, err)
				}
			})
		}
	}
}

func orEvery(roles string) string {
	if roles == "" {
		return "undeclared"
	}
	return roles
}

// THE RULES ABOUT THE DISK TURN ON `data`, WHATEVER THE BROKER: a data node
// never has a scratch store, and a node without data always has one and keeps
// no estate — on a leaf and on an external cluster alike.
func TestTheDiskRulesFollowTheDataRoleOnEveryBroker(t *testing.T) {
	t.Parallel()
	for _, kind := range []placement.BrokerKind{placement.BrokerLeaf, placement.BrokerClient} {
		for name, tc := range map[string]struct{ store, path, kind string }{
			"a node without data that keeps its store": {
				"store:\n  path: /var/n.db\n", "store.scratch", "missing"},
			"a node without data keeping an estate": {
				"store:\n  scratch: true\n  replicated_path: /var/r.db\n", "store", "conflict"},
		} {
			t.Run(string(kind)+"/"+name, func(t *testing.T) {
				t.Parallel()
				doc := "node:\n  roles: [seats]\n" + tc.store + brokerStreams[kind]
				_, err := ParseBootstrap([]byte(doc), EnvOnly())
				problems := Problems(err)
				if len(problems) != 1 || problems[0].Path != tc.path || problems[0].Kind != tc.kind {
					t.Fatalf("want one %s at %s, got %+v", tc.kind, tc.path, problems)
				}
			})
		}
	}
	// And a data node with a scratch store, on the two brokers a data node
	// may have today.
	for _, kind := range []placement.BrokerKind{placement.BrokerMember, placement.BrokerClient} {
		doc := "store:\n  scratch: true\n" + brokerStreams[kind]
		_, err := ParseBootstrap([]byte(doc), EnvOnly())
		problems := Problems(err)
		if len(problems) != 1 || problems[0].Path != "store.scratch" || problems[0].Kind != "conflict" {
			t.Errorf("%s: a data node with a scratch store: want one conflict at "+
				"store.scratch, got %+v", kind, problems)
		}
	}
}

// A MEMBER OF A FLEET PERSISTS, WHATEVER ITS ROLES. Its broker holds every
// stream the fleet writes for every node that reaches it, so an in-memory one
// loses its copy of all of them at its next restart. A named cluster, a peer
// list and a leaf listener each make it one; a solo member does not, and a leaf
// or a client holds no stream of its own to lose.
func TestAMemberOfAFleetMustPersistItsStreams(t *testing.T) {
	t.Parallel()
	const kv = "coordination:\n  type: embedded-kv\n"
	for name, doc := range map[string]string{
		"a named cluster": kv + "stream:\n  cluster:\n    name: acme\n",
		"a peer list": kv + "stream:\n  replicas: 3\n  cluster:\n    name: acme\n" +
			"    peers: [nats://b.example.com:6222, nats://c.example.com:6222]\n",
		"a leaf listener": kv + "stream:\n  leaf:\n    port: 7422\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseBootstrap([]byte(doc), EnvOnly())
			problems := Problems(err)
			if len(problems) != 1 || problems[0].Path != "stream.store_dir" ||
				problems[0].Kind != "missing" {
				t.Fatalf("an in-memory member of a fleet: want one missing "+
					"stream.store_dir, got %+v", problems)
			}
		})
	}
	for name, doc := range map[string]string{
		"a solo member":   "",
		"a leaf":          "node:\n  roles: [seats]\nstore:\n  scratch: true\n" + brokerStreams[placement.BrokerLeaf],
		"a client":        brokerStreams[placement.BrokerClient],
		"a persisted one": brokerStreams[placement.BrokerMember],
	} {
		if _, err := ParseBootstrap([]byte(doc), EnvOnly()); err != nil {
			t.Errorf("%s must load: %v", name, err)
		}
	}
}

// TWO WARNINGS ABOUT THE BROKER, each a valid fleet with a consequence.
func TestTheBrokerWarnings(t *testing.T) {
	t.Parallel()
	peers := func(n int) string {
		list := make([]string, n)
		for i := range list {
			list[i] = fmt.Sprintf("nats://m%d.example.com:6222", i)
		}
		return "[" + strings.Join(list, ", ") + "]"
	}
	member := func(others int, roles string) string {
		return roles + "stream:\n  store_dir: /var/js\n  cluster:\n    name: acme\n" +
			"    peers: " + peers(others) + "\ncoordination:\n  type: embedded-kv\n"
	}
	const declared = "node:\n  roles: [data, seats]\n"
	for _, tc := range []struct {
		name string
		doc  string
		want []string // warning paths among stream.cluster.peers and node.roles
	}{
		// W1: MaxStreamReplicas copies at most, so the sixth member is a
		// voter holding nothing. Five is the recommended fleet and fine.
		{"five members", member(4, declared), nil},
		{"six members", member(5, declared), []string{"stream.cluster.peers"}},
		// W2: the every-role default is the single process's, and a fleet
		// node that took it keeps data and runs agents whether meant to or
		// not.
		{"a fleet member with undeclared roles", member(2, ""), []string{"node.roles"}},
		{"a solo node with undeclared roles", "", nil},
		{"a fleet member that declared its roles", member(2, declared), nil},
		{"six undeclared members", member(5, ""), []string{"stream.cluster.peers", "node.roles"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := ParseBootstrap([]byte(tc.doc), EnvOnly())
			if err != nil {
				t.Fatalf("the fixture must be valid, since a warning is: %v", err)
			}
			var got []string
			for _, w := range b.Warnings() {
				if w.Path == "stream.cluster.peers" || w.Path == "node.roles" {
					got = append(got, w.Path)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("warnings at %v, want %v", got, tc.want)
			}
		})
	}
}

// THE IN-MEMORY STREAM IS A MEMBER'S WARNING, because only a member holds a
// stream: it is the one broker running JetStream in this process. A stateless
// leaf runs none and is REFUSED a store directory, so warning it that its
// stream is in memory told a correct node to set the one field it may not; a
// client's streams are its external cluster's. Every shape below is valid, and
// each case pins which of the two in-memory warnings it raises — none, or
// exactly the ones a member with no store directory earns.
func TestOnlyABrokerThatHoldsStreamsIsWarnedTheyAreInMemory(t *testing.T) {
	t.Parallel()
	const (
		kv       = "coordination:\n  type: embedded-kv\n"
		data     = "node:\n  roles: [data, seats]\n"
		seats    = "node:\n  roles: [seats]\nstore:\n  scratch: true\n"
		inMemory = "stream.store_dir"
		limit    = "stream.store_max_bytes"
	)
	for _, tc := range []struct {
		name string
		doc  string
		want []string
	}{
		{"a solo data member with a store directory",
			data + "stream:\n  store_dir: /var/js\n", nil},
		{"a solo data member with no store directory", data, []string{inMemory}},
		{"the every-role default with no store directory", "", []string{inMemory}},
		{"a data member with a store limit and no store directory",
			data + "stream:\n  store_max_bytes: 5368709120\n", []string{inMemory, limit}},
		{"a data member with a store limit inside its store directory",
			data + "stream:\n  store_dir: /var/js\n  store_max_bytes: 5368709120\n", nil},
		{"a clustered member", data + brokerStreams[placement.BrokerMember], nil},
		{"a member opening a leaf listener",
			data + kv + "stream:\n  store_dir: /var/js\n  leaf:\n    port: 7422\n", nil},
		{"a stateless leaf", seats + brokerStreams[placement.BrokerLeaf], nil},
		{"a data node on an external cluster", data + brokerStreams[placement.BrokerClient], nil},
		{"a stateless node on an external cluster",
			seats + brokerStreams[placement.BrokerClient], nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := ParseBootstrap([]byte(tc.doc), EnvOnly())
			if err != nil {
				t.Fatalf("the fixture must be valid, since a warning is: %v", err)
			}
			var got []string
			for _, w := range b.Warnings() {
				if w.Path == inMemory || w.Path == limit {
					got = append(got, w.Path)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("%s broker: warnings at %v, want %v", b.BrokerKind(), got, tc.want)
			}
		})
	}
}

// A LEAF HAS NO FILE STORE, SO IT DECLINES NO FSYNC. `stream.sync` configures
// the embedded server's file store and a leaf's server runs no JetStream: a
// window written there reached nothing, and was then warned about as this
// node's own exposure. The members it joins decide what an acknowledged write
// has reached, so a declined fsync on a leaf is refused exactly as it is
// against an external cluster — and the same window on a member that can
// honour it still loads, so the refusal is about the leaf and not the value.
func TestALeafDeclinesNoFsync(t *testing.T) {
	t.Parallel()
	const seats = "node:\n  roles: [seats]\nstore:\n  scratch: true\n" +
		"coordination:\n  type: embedded-kv\n"
	const leaf = "  leaf:\n    urls: [nats-leaf://data-a.example.com:7422]\n"

	// THREE REPLICAS, the count that let it through: below three the
	// no-quorum rule refused it anyway, naming a member's disk the leaf
	// does not have.
	err := rejectsBootstrap(t, seats+"stream:\n  replicas: 3\n  sync: 30s\n"+leaf, "stream.sync")
	problems := Problems(err)
	if len(problems) != 1 || problems[0].Kind != "conflict" ||
		!strings.Contains(problems[0].Message, "leaf runs no JetStream") {
		t.Fatalf("want one conflict at stream.sync saying why, got %+v", problems)
	}
	// One reason, not two: the no-quorum rule is a member's and must not
	// pile on.
	err = rejectsBootstrap(t, seats+"stream:\n  sync: 30s\n"+leaf, "stream.sync")
	if problems := Problems(err); len(problems) != 1 ||
		!strings.Contains(problems[0].Message, "leaf runs no JetStream") {
		t.Fatalf("want only the leaf's refusal, got %+v", problems)
	}

	for name, doc := range map[string]string{
		"a leaf saying the default out loud": seats + "stream:\n  sync: always\n" + leaf,
		"the same window on a three-member fleet": "node:\n  roles: [data, seats]\n" +
			"coordination:\n  type: embedded-kv\nstream:\n  store_dir: /var/js\n" +
			"  replicas: 3\n  sync: 30s\n  cluster:\n    name: acme\n" +
			"    peers: [nats://b.example.com:6222, nats://c.example.com:6222]\n",
	} {
		if _, err := ParseBootstrap([]byte(doc), EnvOnly()); err != nil {
			t.Errorf("%s must load: %v", name, err)
		}
	}
}

// THE OPERATOR FLOOR IS A WARNING ABOUT THE TRIM, and only a node holding the
// estate runs the trim: it reads the estate's own eviction rows, so a node
// without `data` never arms it and reads `stream.tracker_retention` for
// nothing else. Warning a stateless node that its floor stops the trim
// described the members' trim from a value they never see. The backup owner
// is a different question — who owns the deployment's backups — and is asked
// of every node, so it stays.
func TestOnlyANodeThatRunsTheTrimIsWarnedAboutItsFloor(t *testing.T) {
	t.Parallel()
	const (
		floor = "stream.tracker_retention.backup_floor"
		owner = "retention.backup_owner"
		data  = "node:\n  roles: [data, seats]\n"
		seats = "node:\n  roles: [seats]\nstore:\n  scratch: true\n"
	)
	operator := func(stream string) string {
		return strings.Replace(stream, "stream:\n",
			"stream:\n  tracker_retention:\n    backup_floor: operator\n", 1)
	}
	for _, tc := range []struct {
		name string
		doc  string
		want []string
	}{
		{"the every-role default", operator("stream:\n"), []string{floor, owner}},
		{"a solo data member", data + operator("stream:\n  store_dir: /var/js\n"),
			[]string{floor, owner}},
		{"a clustered data member", data + operator(brokerStreams[placement.BrokerMember]),
			[]string{floor, owner}},
		{"a data node on an external cluster", data + operator(brokerStreams[placement.BrokerClient]),
			[]string{floor, owner}},
		{"a stateless leaf", seats + operator(brokerStreams[placement.BrokerLeaf]),
			[]string{owner}},
		{"a stateless node on an external cluster", seats + operator(brokerStreams[placement.BrokerClient]),
			[]string{owner}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := ParseBootstrap([]byte(tc.doc), EnvOnly())
			if err != nil {
				t.Fatalf("the fixture must be valid, since a warning is: %v", err)
			}
			if b.Stream.TrackerRetention.Floor() != BackupFloorOperator {
				t.Fatalf("the fixture must declare the operator floor: %q", tc.doc)
			}
			var got []string
			for _, w := range b.Warnings() {
				if w.Path == floor || w.Path == owner {
					got = append(got, w.Path)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("warnings at %v, want %v", got, tc.want)
			}
		})
	}
}
