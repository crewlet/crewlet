package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// fakeBrokerNode answers the broker routes with the ENGINE'S OWN renderings:
// the listing is the committed answer internal/api's test writes from an
// engine.BrokerView, and a refusal is api.RenderBrokerRefusal's. A fixture
// spelling this command's idea of the shape would agree with the command
// whatever a node sends.
type fakeBrokerNode struct {
	server *httptest.Server
	answer []byte

	mu      sync.Mutex
	refuse  error
	removed []string // the path each removal was posted to
	forced  []bool
}

func newFakeBrokerNode(t *testing.T) *fakeBrokerNode {
	t.Helper()
	answer, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "internal", "api",
		"testdata", "fleet_broker_answer.json"))
	if err != nil {
		t.Fatalf("read the engine's broker answer: %v", err)
	}
	n := &fakeBrokerNode{answer: answer}
	n.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/fleet/broker":
			_, _ = w.Write(n.answer)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/fleet/broker/remove"):
			done := engine.BrokerRemoved{By: "node-a",
				Group: &jetstream.MetaGroup{Cluster: "acme", Leader: "node-a",
					Peers: []jetstream.MetaPeer{{Name: "node-a"}, {Name: "node-b"}}}}
			switch {
			case strings.HasPrefix(r.URL.Path, "/fleet/broker/remove-peer/"):
				done.Peer = strings.TrimPrefix(r.URL.Path, "/fleet/broker/remove-peer/")
			case strings.HasPrefix(r.URL.Path, "/fleet/broker/remove/"):
				done.Node = strings.TrimPrefix(r.URL.Path, "/fleet/broker/remove/")
				done.Peer = jetstream.PeerIDOf(done.Node)
			default:
				http.NotFound(w, r)
				return
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			if refusal, ok := api.RenderBrokerRefusal(n.refuse); ok {
				w.WriteHeader(refusal.Status)
				_ = json.NewEncoder(w).Encode(refusal.Body)
				return
			}
			n.removed = append(n.removed, r.URL.Path)
			n.forced = append(n.forced, r.URL.Query().Get("force") == "true")
			_ = json.NewEncoder(w).Encode(done)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(n.server.Close)
	return n
}

// THE LISTING PUTS WHAT EACH NODE ADVERTISES BESIDE WHAT THE GROUP COUNTS, and
// says in words what disagrees and what to do: a dead member gets the exact
// command that stops the group counting it, and a voter with no live node is
// still a row — the member an operator is looking for.
func TestFleetBrokerListNamesTheDeadMemberAndTheRemedy(t *testing.T) {
	node := newFakeBrokerNode(t)
	out, stderr, err := cli(t, "fleet", "broker", "list", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("fleet broker list: %v\n%s", err, stderr)
	}
	for _, want := range []string{
		"Metadata group of acme, as node-a reports it: 3 voters, led by node-a.",
		"NODE", "ADVERTISES", "VOTER", "PEER",
		"old-1", "unknown",
		"sat-eu-1", "leaf",
		"node-c", "offline, last heard 1h30m ago", jetstream.PeerIDOf("node-c"),
		"DEAD MEMBER node-c",
		"crewlet fleet broker remove node-c -confirm node-c",
		"UNKNOWN old-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing does not say %q:\n%s", want, out)
		}
	}
}

// A VOTER NOBODY CAN NAME IS LISTED BY ITS PEER ID, and the command printed for
// it is the one that works: by -peer, since it has no node id to give. The
// answer is the engine's own, as a member answers once it has restarted since
// node-c died and never heard its name — and a live node the group counts
// under a name the answer does not carry is still that node's row, matched
// by the peer id its node id hashes to.
func TestFleetBrokerListNamesAVoterNobodyCanNameByItsPeerID(t *testing.T) {
	node := newFakeBrokerNode(t)
	var answer map[string]any
	if err := json.Unmarshal(node.answer, &answer); err != nil {
		t.Fatal(err)
	}
	group := answer["group"].(map[string]any)
	for _, p := range group["peers"].([]any) {
		peer := p.(map[string]any)
		if peer["name"] == "node-c" || peer["name"] == "node-b" {
			peer["name"] = ""
		}
	}
	for _, f := range answer["findings"].([]any) {
		finding := f.(map[string]any)
		if finding["node"] == "node-c" {
			finding["node"] = ""
		}
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	node.answer = raw
	out, _, err := cli(t, "fleet", "broker", "list", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	gone := jetstream.PeerIDOf("node-c")
	for _, want := range []string{
		"(name unknown)",
		"DEAD MEMBER (peer " + gone + ")",
		"crewlet fleet broker remove -peer " + gone + " -confirm " + gone,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing does not say %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "node-b ") && !strings.Contains(line, "current") {
			t.Errorf("node-b is live and counted under a name the answer lost, and its "+
				"row reads %q", line)
		}
	}
}

// AN EXTERNAL CLUSTER'S FLEET IS TOLD WHOSE MEMBERSHIP IT IS, and shown no
// table of a broker this fleet does not run.
func TestFleetBrokerListOnAnExternalCluster(t *testing.T) {
	node := newFakeBrokerNode(t)
	node.answer = []byte(`{"node":"node-a","kind":"client","external":true,"nodes":[],"findings":[]}`)
	out, _, err := cli(t, "fleet", "broker", "list", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "external NATS cluster") || strings.Contains(out, "ADVERTISES") {
		t.Fatalf("an external cluster's fleet was answered:\n%s", out)
	}
}

// A REMOVAL REPEATS THE NODE, forces only when asked, and a refusal reaches the
// operator with the node's own detail and hint. A voter named by -peer goes to
// the peer route, and naming it both ways — or neither — is refused before
// anything is sent.
func TestFleetBrokerRemoveConfirmsForcesOnlyOnAskAndRelaysRefusals(t *testing.T) {
	node := newFakeBrokerNode(t)
	conf := bootstrapForURL(t, node.server.URL)
	if _, _, err := cli(t, "fleet", "broker", "remove", "node-c", conf); err == nil ||
		!strings.Contains(err.Error(), "-confirm") {
		t.Fatalf("an unconfirmed removal answered %v", err)
	}
	out, stderr, err := cli(t, "fleet", "broker", "remove", "node-c", conf, "-confirm", "node-c")
	if err != nil {
		t.Fatalf("remove: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, "node-c is no longer a voter") || !strings.Contains(out, "node-a's system account") {
		t.Errorf("the removal's answer:\n%s", out)
	}
	if _, _, err := cli(t, "fleet", "broker", "remove", "node-d", conf, "-confirm", "node-d", "-force"); err != nil {
		t.Fatal(err)
	}
	if len(node.forced) != 2 || node.forced[0] || !node.forced[1] {
		t.Fatalf("forced %v, want only the second removal forced", node.forced)
	}
	peer := jetstream.PeerIDOf("node-e")
	out, stderr, err = cli(t, "fleet", "broker", "remove", "-peer", peer, "-confirm", peer, conf)
	if err != nil {
		t.Fatalf("remove by peer id: %v\n%s", err, stderr)
	}
	if got := node.removed[len(node.removed)-1]; got != "/fleet/broker/remove-peer/"+peer {
		t.Fatalf("a removal by -peer was posted to %s", got)
	}
	if !strings.Contains(out, "(peer "+peer+") is no longer a voter") {
		t.Errorf("the removal's answer:\n%s", out)
	}
	for _, args := range [][]string{
		{"remove", "node-e", "-peer", peer, "-confirm", peer},
		{"remove", "-peer", peer, "-confirm", "node-e"},
		{"remove", "-confirm", peer},
	} {
		sent := len(node.removed)
		if _, _, err := cli(t, append(append([]string{"fleet", "broker"}, args...), conf)...); err == nil {
			t.Errorf("%v was accepted", args)
		}
		if len(node.removed) != sent {
			t.Errorf("%v reached the node", args)
		}
	}
	node.refuse = &engine.BrokerMemberLive{Node: "node-b"}
	_, _, err = cli(t, "fleet", "broker", "remove", "node-b", conf, "-confirm", "node-b")
	if err == nil || !strings.Contains(err.Error(), "member_live") ||
		!strings.Contains(err.Error(), "stop the node first") {
		t.Fatalf("a live member's refusal reached the operator as %v", err)
	}
}

// THE COMMAND WAITS PAST THE NODE'S READ OF THE GROUP. A listing's node may
// spend [engine.BrokerReadWait] asking members before it answers; a call that
// gave up first would report an unreachable node for a fleet whose broker had
// a member wedged inside its lease — the fleet the command exists for.
func TestTheListingWaitsPastTheNodesReadOfTheGroup(t *testing.T) {
	if nodeRequestTimeout <= engine.BrokerReadWait() {
		t.Errorf("the command waits %s for a listing the node may spend %s reading the "+
			"group for", nodeRequestTimeout, engine.BrokerReadWait())
	}
}
