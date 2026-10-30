package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A node that holds no data, end to end.
//
// What these assert is the whole premise of the `data` role: a node with a
// scratch store and a leaf broker — no JetStream, no replicated estate, no
// index — runs a seat whose tools read and write the company's tracker,
// knowledge base and chat, and every one of those reads and writes lands in,
// and is answered from, a node that DOES hold them. The unit suites cover the
// wire against fakes; this is the real engine on both sides of a real leaf
// link.

// statelessPair is one data member with a leaf listener and one stateless
// node joined to it, with the seat `ceo` pinned to the stateless one.
type statelessPair struct {
	data, agent *node
	agentStore  string
	agentBoot   *config.Bootstrap
	// agentProbes is the stateless node's own listener, serving its probes
	// and its boxes' routes; nil where it binds none.
	agentProbes *httptest.Server
}

func startStatelessPair(t *testing.T) statelessPair {
	t.Helper()
	return startStatelessPairWith(t, nil, nil)
}

// startStatelessPairWith is [startStatelessPair] over a company document the
// caller may amend first, with env added to both nodes' environment
// ([nodeEnvironment]).
func startStatelessPairWith(t *testing.T, amend func(doc string) string,
	env map[string]string) statelessPair {
	t.Helper()
	return startStatelessPairSpec(t, pairSpec{company: amend, env: env})
}

// startStatelessOnItsOwnListener is [startStatelessPair] in the shape a seat
// pool with no address of its own runs: the stateless node binds api.port, and
// is handed that listener as where its own direct boxes dial
// ([engine.Options.BoxHostURL]) — and NO fleet address, so the one way its
// executor's box reaches the seat's tools is the node's own probe listener,
// on loopback, serving the bridge there (internal/api's Probes), as
// [statelessPair.agentProbes] until the test ends.
func startStatelessOnItsOwnListener(t *testing.T) statelessPair {
	t.Helper()
	return startStatelessPairSpec(t, pairSpec{ownListener: true})
}

// pairSpec is what a case may vary about the pair it stands up. The zero value
// is [startStatelessPair]'s.
type pairSpec struct {
	// company amends the golden company document; env is added to both
	// nodes' environment.
	company func(doc string) string
	env     map[string]string

	// dataBoot amends the DATA member's Tier A — after the harness's own
	// settings and before anything is seeded or opened from it — which is
	// the member that serves the API, so the one a person signs in on
	// ([withSignIn]). The stateless member serves none: a node without
	// `data` may not take `ingress`.
	dataBoot func(*config.Bootstrap)

	// ownListener serves the stateless node's boxes their routes on its own
	// listener, with no fleet address at all
	// ([startStatelessOnItsOwnListener]). Unset, its executor's box is handed
	// the fleet's address and dials the data member, which relays each call
	// to the stateless node holding the seat.
	ownListener bool
}

// startStatelessPairSpec is [startStatelessPair] as spec describes.
func startStatelessPairSpec(t *testing.T, spec pairSpec) statelessPair {
	t.Helper()
	logs.attribute(t)
	model := newScriptedModel(t)
	doc := goldenCompany(model)
	if spec.company != nil {
		doc = spec.company(doc)
	}
	env := spec.env
	// PINNED, so the one agent seat runs where the data is not: a turn that
	// happened to land on the data node would test nothing here.
	doc = strings.Replace(doc, ceoSeat, ceoSeat+"    placement:\n      node: agent-1\n", 1)
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	// ONE KEYRING FOR THE PAIR, as for any fleet: the stateless node signs
	// what it asks of the estate and verifies what it is answered under the
	// keyring the data member holds, and a node keyed differently is a node
	// whose every request is refused.
	ring := fleetRing(t)
	dataLn := bridgeListener(t, "")
	data, dataBoot := bootDataMember(t, cfg, model, env, dataLn, ring, spec.dataBoot)
	data.app, data.server = serveAPI(t, data.engine, dataBoot, data.chat.Hub(), dataLn, nil)
	port := dataBoot.Stream.Leaf.Port

	agentDir := t.TempDir()
	agentBoot := config.DefaultBootstrap()
	agentBoot.Node.ID = "agent-1"
	agentBoot.Node.Roles = []string{"seats"}
	agentBoot.Store.Path = filepath.Join(agentDir, "crewlet.db")
	agentBoot.Store.Scratch = true
	agentBoot.Stream.Leaf.URLs = []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", port)}
	agentBoot.Coordination.Type = config.CoordinationEmbeddedKV
	ring.hold(&agentBoot)
	agentEnv := maps.Clone(env)
	if agentEnv == nil {
		agentEnv = map[string]string{}
	}
	if spec.ownListener {
		// ITS OWN api.port, the address `crewlet run` hands its direct
		// boxes, and no fleet address in its environment at all.
		ln := bridgeListener(t, "")
		agentBoot.API.Host = "127.0.0.1"
		agentBoot.API.Port = ln.Addr().(*net.TCPAddr).Port
		agent := bootNode(t, &agentBoot, cfg, model, agentEnv, listenerURL(ln))
		probes, _ := serveProbesOn(t, agent, &agentBoot, ln)
		return statelessPair{data: data, agent: agent, agentStore: agentDir,
			agentBoot: &agentBoot, agentProbes: probes}
	}
	// THE FLEET'S BRIDGE ADDRESS FOR THE SEATS NODE, which binds no listener
	// here: its executor's box dials the data node — the one serving the API
	// — and that node relays each call to this one, which holds the seat
	// (internal/api/mcpbridge). The shape a seats node on api.port 0, or a
	// box off its host, reaches its tools in.
	agentEnv[mcpbridge.BaseURLVar] = listenerURL(dataLn)
	agent := bootNode(t, &agentBoot, cfg, model, agentEnv, "")
	return statelessPair{data: data, agent: agent, agentStore: agentDir, agentBoot: &agentBoot}
}

// dataMemberBootstrap is the pair's data member: a node holding the data, with
// a leaf listener on port for the stateless node to join through — on the
// pair's keyring, serving the fixture's credential ([withCredential]).
func dataMemberBootstrap(t *testing.T, port int, ring keyring) *config.Bootstrap {
	t.Helper()
	boot := config.DefaultBootstrap()
	ring.hold(&boot)
	withCredential(t, &boot)
	boot.Node.ID = "data-a"
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&boot.Stream, filepath.Join(t.TempDir(), "stream"))
	boot.Stream.Leaf.Host, boot.Stream.Leaf.Port = "127.0.0.1", port
	boot.Coordination.Type = config.CoordinationEmbeddedKV
	return &boot
}

// bootDataMember boots the pair's data member on a leaf port nothing held a
// moment ago, and on ANOTHER one when that port was taken in between.
//
// THE PORT IS RELEASED BEFORE THE MEMBER BINDS IT ([leafPort]) — nothing can
// hand a listener's port to a broker the engine builds — and with the cases
// here running in parallel, another case's listener can take it in that gap.
// The member's own pre-bind probe refuses a taken port by name
// ([jetstream.ErrLeafPortTaken], which
// [TestADataMemberRefusesATakenLeafPortByName] holds), so that one failure is
// worth another set of numbers, for the reason
// [jetstreamtest.ClusterStartAttempts] gives and on its count: the collision
// is with work this case does not coordinate with, so a wider window does not
// help and a different number does. Any other failure ends the case — the
// route listener's [jetstream.ErrRoutePortTaken] among them, since this member
// is no cluster's and opens no route listener to lose.
//
// THE API IS SERVED ON ln, and the member's api.port names it: the member is
// the pair's one node with the ingress role, so it is the address the seats
// node's executor box dials where the pair hands it the fleet's address, and
// an ingress node whose api.port is 0 serves nothing and joins no relay
// (internal/engine's joinBridgeFleet) — the box's every call would reach a
// node that cannot hand it to the seat's holder.
//
// amend, when set, changes the member's bootstrap after the harness's own
// settings and before the member is seeded or opened from it. The listener's
// address is one of those settings, and a sign-in's api.external_url
// ([withSignIn]) replaces it as the address the member serves people at:
// nothing a box dials is read from it — the box is handed the listener, and
// the relay is joined on api.port.
func bootDataMember(t *testing.T, cfg *config.Company, model *scriptedModel,
	env map[string]string, ln net.Listener, ring keyring, amend func(*config.Bootstrap)) (
	*node, *config.Bootstrap) {

	t.Helper()
	for attempt := 1; ; attempt++ {
		boot := dataMemberBootstrap(t, leafPort(t), ring)
		boot.API.Port = ln.Addr().(*net.TCPAddr).Port
		boot.API.ExternalURL = listenerURL(ln)
		if amend != nil {
			amend(boot)
		}
		n, err := newNode(t, nodeOptions(boot, cfg, env, listenerURL(ln)), model)
		switch {
		case err == nil:
			return n, boot
		case errors.Is(err, jetstream.ErrLeafPortTaken) &&
			attempt < jetstreamtest.ClusterStartAttempts:
			t.Logf("data member attempt %d/%d lost its leaf port, retrying on "+
				"another: %v", attempt, jetstreamtest.ClusterStartAttempts, err)
		default:
			t.Fatalf("data member (attempt %d/%d): %v", attempt,
				jetstreamtest.ClusterStartAttempts, err)
		}
	}
}

// bootNode builds and starts one engine.
//
// Neither bootstrap is validated here: engine.New holds every bootstrap it is
// given to Tier A, and a refusal names the node it was building.
func bootNode(t *testing.T, boot *config.Bootstrap, cfg *config.Company, model *scriptedModel,
	env map[string]string, bridge string) *node {
	t.Helper()
	return bootNodeWith(t, nodeOptions(boot, cfg, env, bridge), model)
}

// nodeOptions is the engine options every harness node is built with: its
// bootstrap and company, the harness's activation instant, its own
// environment ([nodeEnvironment]) rather than the process's, and where a box on
// this host dials its bridge ([listenerURL]; "" for a node that serves none).
func nodeOptions(boot *config.Bootstrap, cfg *config.Company, env map[string]string,
	bridge string) engine.Options {
	return engine.Options{
		Bootstrap: boot, Company: cfg, ActivatedAt: harnessActivation,
		Environment: nodeEnvironment(env), SandboxPollInterval: runPollInterval,
		BoxHostURL: bridge,
	}
}

// bootNodeWith is [bootNode] over the engine options the caller chose — a
// maintenance mode, say.
func bootNodeWith(t *testing.T, opts engine.Options, model *scriptedModel) *node {
	t.Helper()
	n, err := newNode(t, opts, model)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// newNode is [bootNodeWith] handing its failure back, for a caller that can do
// something about one.
//
// ITS CHAT HUB WITH IT ([newEngine]), whether or not the node will serve an
// API: the pair's stateless member serves none, and it is exactly the node
// whose seat raises the working indicator a room on the data member shows.
func newNode(t *testing.T, opts engine.Options, model *scriptedModel) (*node, error) {
	t.Helper()
	logs.attribute(t)
	seedStore(t, opts.Bootstrap)
	id := opts.Bootstrap.Node.ID
	e, chatLive, stops, err := newEngine(t.Context(), opts)
	t.Cleanup(func() { stopInReverse(stops) })
	if err != nil {
		return nil, fmt.Errorf("engine.New(%s): %w", id, err)
	}
	if err := e.Start(t.Context()); err != nil {
		return nil, fmt.Errorf("engine.Start(%s): %w", id, err)
	}
	return &node{engine: e, model: model, id: id, chat: chatLive}, nil
}

// A DATA MEMBER REFUSES A LEAF PORT SOMEBODY HOLDS, BY NAME — the premise
// [bootDataMember] retries on. A refusal that stopped carrying
// [jetstream.ErrLeafPortTaken] would turn every lost port back into a failed
// case, and a member that came up on a port it does not hold would leave the
// stateless node joining somebody else's listener. And it is the LEAF
// listener's name: refused as the route listener's, a member with no cluster
// block was told its cluster route port was taken.
func TestADataMemberRefusesATakenLeafPortByName(t *testing.T) {
	t.Parallel()
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer held.Close()
	model := newScriptedModel(t)
	cfg, err := config.ParseCompany([]byte(goldenCompany(model)))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	boot := dataMemberBootstrap(t, held.Addr().(*net.TCPAddr).Port, fleetRing(t))
	_, err = newNode(t, nodeOptions(boot, cfg, nil, ""), model)
	if !errors.Is(err, jetstream.ErrLeafPortTaken) {
		t.Fatalf("a data member on a held leaf port answered %v, want %v", err,
			jetstream.ErrLeafPortTaken)
	}
	if errors.Is(err, jetstream.ErrRoutePortTaken) {
		t.Errorf("a data member's held leaf port is reported as a route port: %v", err)
	}
}

// leafPort is a loopback port nothing held a moment ago.
func leafPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// A SEAT ON A NODE THAT HOLDS NOTHING FILES WORK THE FLEET HOLDS. Its create
// runs on the data node, attributed to the seat, and the node that ran the
// turn keeps no copy of it — nor any replicated estate at all.
func TestASeatOnAStatelessNodeWritesThroughADataNode(t *testing.T) {
	t.Parallel()
	p := startStatelessPair(t)
	waitFor(t, "the stateless node to be admitted by a data node", hydrated(t, p.agent.engine))
	waitForSeat(t, p.agent, "ceo")

	p.agent.model.callOnExecute(worklet.CreateWorkItemTool, map[string]any{
		"project": "ENG", "title": "filed from a node that holds nothing",
	})
	wakeWithMessage(t, p.agent, "ceo")

	var detail worklet.TaskDetail
	waitFor(t, "the task on the data node", func() bool {
		got, err := p.data.engine.Tracker().Task(t.Context(), "ENG-1", worklet.DetailWants{},
			statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			return false
		}
		detail = got
		return true
	})
	if detail.Task.Title != "filed from a node that holds nothing" || detail.Task.Reporter != "ceo" {
		t.Errorf("the data node holds %q reported by %q", detail.Task.Title, detail.Task.Reporter)
	}
	// THE TOOL SAW ITS OWN WRITE: the create's answer is what the model is
	// shown, and a stateless node that lost it would file the task again.
	waitFor(t, "the create's answer to reach the model", func() bool {
		return slices.ContainsFunc(p.agent.model.toolResults(), func(r string) bool {
			return strings.Contains(r, "ENG-1")
		})
	})

	// AND THE STATELESS NODE KEPT NO COPY. Its store directory holds the
	// node estate it is allowed and nothing else — no replicated estate
	// file for anything to have been written into.
	replicated := store.ReplicatedPath(filepath.Join(p.agentStore, "crewlet.db"), "")
	if _, err := os.Stat(replicated); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stateless node has a replicated estate at %s (stat: %v)", replicated, err)
	}
	if p.agent.engine.Tracker() != nil || p.agent.engine.TrackerWriter() != nil {
		t.Error("the stateless node runs a local tracker")
	}
}

// A SEAT ON A NODE THAT HOLDS NOTHING ANSWERS IN A ROOM THE FLEET HOLDS, AND
// THE ROOM SHOWS IT AT WORK WHILE IT DOES.
//
// The chat half of [TestASeatOnAStatelessNodeWritesThroughADataNode], and not
// the same claim twice: a chat turn is WOKEN by the room as well as writing to
// it. A person says something on the data member, from her own tab; the wake
// is derived from that committed record by the change feed and reaches the
// seat's mailbox on the stateless member, whose turn reads the trigger with
// the chat parser a node without data still registers; and its answer goes
// back through a data node's router into the room it was asked in — which is
// where the assertion reads it, on the node that holds the room.
//
// AND THE INDICATOR IS THE TURN'S OWN. The node running a seat's turn is rarely
// the node a browser is attached to, and here it is a node with no API: what
// raises the indicator is that node's engine, from the turn
// ([notify.StatusDriver] over its own chat hub, as the seat's member of the
// room's presence), and what shows it is the presence hub of the data member
// Alice's tab is attached to, told it over the broker in a sealed frame. So
// the seat is HELD on the round that answers until her socket has shown it
// working — [TestAnIndicatorRaisedOnANodeWithNoAPIShowsInARoomOnAnother]
// raises one by hand and certifies the presence hubs alone.
func TestASeatOnAStatelessNodeAnswersInARoomTheFleetHolds(t *testing.T) {
	t.Parallel()
	p := startStatelessPairSpec(t, pairSpec{company: peopleDoc, dataBoot: withSignIn})
	waitFor(t, "the stateless node to be admitted by a data node", hydrated(t, p.agent.engine))
	waitForSeat(t, p.agent, "ceo")
	alices := signedIn(t, p.data, alice)

	made := openRoomAs(t, p.data, alices, map[string]any{
		"channel": "deploys", "kind": string(chatlet.KindPublic), "members": []any{"ceo"},
	})
	// ALICE IS LOOKING AT THE ROOM, from a tab on the data member, before
	// anything is said in it.
	conn := dialAs(t, p.data, alices)
	focusOn(t, conn, made.Channel)

	hold := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	const answer = "It finished at 14:20; the node drained cleanly."
	p.agent.model.script(&chatScript{channel: made.Channel, answer: answer, hold: hold})
	t.Cleanup(func() { p.agent.model.script(nil) })
	const asked = "@ceo did the afternoon deploy finish?"
	sayAs(t, p.data, alices, made.Channel, asked)

	// THE SEAT AT WORK, as her tab is shown it, while its turn is held.
	awaitFrame(t, conn, stream.KindChatPresence, "ceo working in #"+made.Name,
		func(data json.RawMessage) bool {
			return showsWorking(data, made.Channel, "ceo", "")
		})
	release()

	var page chatlet.Transcript
	waitFor(t, "the seat's answer in the room, as the data node holds it", func() bool {
		page = transcript(t, p.data, alice.seat, made.Channel)
		return slices.ContainsFunc(page.Messages, func(v chatlet.MessageView) bool {
			return v.Message.Author == "ceo"
		})
	}, func() string {
		return fmt.Sprintf("room holds %v; the model saw %v", bodies(page), p.agent.model.seen())
	})
	said := page.Messages[0]
	if said.Message.Body != answer || said.Message.AuthorKind != chatlet.AuthorAgent {
		t.Errorf("the room's newest message is %q by a %s, want the seat's answer",
			said.Message.Body, said.Message.AuthorKind)
	}
	// WOKEN BY WHAT WAS ASKED: the turn that answered was shown the message
	// — the trigger crossed the leaf link whole — rather than answering a
	// wake that carried nothing.
	if !p.agent.model.sawText(asked) {
		t.Errorf("the seat answered without ever being shown %q", asked)
	}
	// AND DELIVERED, on its own submission: its record is on the data
	// member, which keeps a node without data's audit trail in custody.
	waitFor(t, "the seat to submit citing its answer", p.agent.model.chatSubmitted)
	deliveredThrough(t, p.data, chatlet.PostMessageTool)
	// AND THE STATELESS NODE KEPT NO COPY of the room it answered in.
	if p.agent.engine.Chat() != nil || p.agent.engine.ChatStore() != nil {
		t.Error("the stateless node runs a local chat")
	}
}

// A SEAT WOKEN BY A REPLY IN A THREAD IS HANDED THE THREAD, ON A NODE THAT
// HOLDS NONE OF IT, AND ANSWERS IN IT.
//
// A thread reply is usually thin — "can you take this one?" — and the thread
// is the context, so the turn-start prefetch hands the thread over rather than
// leaving the seat to go and read it ([notify.ThreadReader]). For the engine's
// own chat that read is [chatlet.ThreadReader] over the estate router, as the
// woken seat: on a node that holds no data it is a data node's copy that
// answers, and a router that could not reach one renders the unreadable hint
// instead — which tells the seat it has only the triggering message. So the
// prompt is the assertion: the thread block, with the message that opened the
// thread in it, and not the hint.
//
// The reply goes where a thread reply belongs: in the thread, through
// [chatlet.ReplyInThreadTool], read back on the node that holds the room.
func TestASeatWokenInAThreadOnAStatelessNodeIsHandedTheThread(t *testing.T) {
	t.Parallel()
	p := startStatelessPairSpec(t, pairSpec{company: peopleDoc, dataBoot: withSignIn})
	waitFor(t, "the stateless node to be admitted by a data node", hydrated(t, p.agent.engine))
	waitForSeat(t, p.agent, "ceo")
	alices := signedIn(t, p.data, alice)

	made := openRoomAs(t, p.data, alices, map[string]any{
		"channel": "releases", "kind": string(chatlet.KindPublic), "members": []any{"ceo"},
	})
	// THE THREAD'S OPENING, addressed to nobody: nothing wakes for it, and
	// the only way the seat can be shown it is the thread block.
	const opened = "The 2.4 release notes are drafted; the migration section still " +
		"needs a reviewer."
	root := sayAs(t, p.data, alices, made.Channel, opened).MessageID

	const answer = "Taking the migration section; review by end of day."
	p.agent.model.script(&chatScript{channel: made.Channel, thread: root, answer: answer})
	t.Cleanup(func() { p.agent.model.script(nil) })
	const asked = "@ceo can you take the migration section?"
	reply := receiptOf[chatPostReceipt](t, alices.acted(p.data, chatlet.ReplyInThreadTool,
		map[string]any{"channel": made.Channel, "reply_to": root, "body": asked}))
	if reply.ThreadRoot != root {
		t.Fatalf("Alice's reply was filed under %q, not the thread of %s", reply.ThreadRoot, root)
	}

	var page chatlet.Transcript
	var answered chatlet.MessageView
	waitFor(t, "the seat's answer, as the data node holds it", func() bool {
		page = transcript(t, p.data, alice.seat, made.Channel)
		i := slices.IndexFunc(page.Messages, func(v chatlet.MessageView) bool {
			return v.Message.Author == "ceo"
		})
		if i < 0 {
			return false
		}
		answered = page.Messages[i]
		return true
	}, func() string {
		return fmt.Sprintf("room holds %v; the model saw %v", bodies(page), p.agent.model.seen())
	})
	if answered.Message.Body != answer || answered.Message.ThreadRoot != root {
		t.Errorf("the seat said %q under %q, want its answer in the thread of %s",
			answered.Message.Body, answered.Message.ThreadRoot, root)
	}
	waitFor(t, "the seat to submit citing its answer", p.agent.model.chatSubmitted)
	deliveredThrough(t, p.data, chatlet.ReplyInThreadTool)

	// AND THE EXECUTOR WAS HANDED THE THREAD, opening and all — in its own
	// block of the system prompt, where the trigger's text is not.
	system := executorPrompt(t, p.agent)
	const heading = "## The thread so far"
	at := strings.Index(system, heading)
	switch {
	case at < 0:
		t.Fatalf("the executor of a turn woken in a thread was shown no %q block:\n%s",
			heading, tail(system))
	case strings.Contains(system, prefetch.UnreadableThreadHint):
		t.Errorf("the executor was told its thread could not be read from this node — "+
			"the Chatlet reader through the router answered nothing:\n%s", tail(system[at:]))
	case !strings.Contains(system[at:], opened):
		t.Errorf("the thread block does not hold the message that opened the thread "+
			"(%q):\n%s", opened, tail(system[at:]))
	}
}

// A SEAT POOL WITH NO ADDRESS OF ITS OWN RUNS ITS SEATS. A node without the
// ingress role used to hand its direct boxes no address but the fleet's, so
// one with no CREWLET_MCP_BRIDGE_URL — a satellite on a private host, a
// Kubernetes seat pool with no route in — refused every executor launch. Now
// it serves its own boxes the bridge on its own listener, to this host's
// loopback alone, and its seat files work as any other: the box's call
// reaches the stateless node's listener, the node holds the seat, and the
// write runs on the data node through the estate router.
//
// Mutation: serve the probe surface without the bridge and the box's every
// call meets a 404, so the task is never filed; derive no address and the
// launch is refused.
func TestAStatelessNodesBoxReachesItsToolsOnItsOwnListener(t *testing.T) {
	t.Parallel()
	p := startStatelessOnItsOwnListener(t)
	if got := p.agent.engine.Bridge().Reaches(false); got != "" {
		t.Fatalf("the premise: the stateless node has a fleet address, %q", got)
	}
	if got := p.agent.engine.Bridge().Reaches(true); got != p.agentProbes.URL {
		t.Fatalf("a direct box on the stateless node dials %q, want its own listener %q",
			got, p.agentProbes.URL)
	}
	waitFor(t, "the stateless node to be admitted by a data node", hydrated(t, p.agent.engine))
	waitForSeat(t, p.agent, "ceo")

	p.agent.model.callOnExecute(worklet.CreateWorkItemTool, map[string]any{
		"project": "ENG", "title": "filed through its own node's listener",
	})
	wakeWithMessage(t, p.agent, "ceo")
	waitFor(t, "the task on the data node", func() bool {
		got, err := p.data.engine.Tracker().Task(t.Context(), "ENG-1", worklet.DetailWants{},
			statelog.Freshness{Level: statelog.ReadStale})
		return err == nil && got.Task.Title == "filed through its own node's listener"
	})
	waitFor(t, "the create's answer to reach the model", func() bool {
		return slices.ContainsFunc(p.agent.model.toolResults(), func(r string) bool {
			return strings.Contains(r, "ENG-1")
		})
	})
}

// A SEARCH FROM A NODE THAT HOLDS NO INDEX IS ANSWERED BY ONE THAT DOES.
func TestAStatelessNodeSearchesTheFleetsKnowledge(t *testing.T) {
	t.Parallel()
	p := startStatelessPair(t)
	if _, err := p.data.engine.PagesStore().Create(t.Context(), pageOperator(), doclet.NewPage{
		Container: "ENG", Title: "Rollback runbook",
		Body: "When a deploy hangs on rollback, drain the node before retrying.",
	}); err != nil {
		t.Fatalf("create page: %v", err)
	}
	searcher := p.agent.engine.Knowledge()
	if searcher == nil {
		t.Fatal("the stateless node has no knowledge searcher")
	}
	waitFor(t, "the stateless node's search to find the page", func() bool {
		answer := searcher.Search(t.Context(), knowledge.Query{
			Text: "rollback drain node", Org: p.agent.engine.Company().Org, Limit: 5,
		})
		// AND THE SEARCH RAN: a data node answered it, serving a mode.
		return answer.ServedMode != "" && slices.ContainsFunc(answer.Hits,
			func(h knowledge.Hit) bool { return h.Title == "Rollback runbook" })
	})
}

// THE RECORD OF WHAT A STATELESS NODE DID OUTLIVES IT. Its turns' audit rows
// cannot live in a store deleted at every boot, so they are in a data node's
// event log — and the data node ran no turn itself, so every phase row there
// came across.
func TestAStatelessNodesAuditTrailLandsOnADataNode(t *testing.T) {
	t.Parallel()
	p := startStatelessPair(t)
	waitFor(t, "the stateless node to be admitted by a data node", hydrated(t, p.agent.engine))
	waitForSeat(t, p.agent, "ceo")
	wakeWithMessage(t, p.agent, "ceo")
	waitForTurn(t, p.agent)

	completed := types.AgentPhaseCompleted{}.EventType()
	waitFor(t, "the stateless node's phase rows on the data node", func() bool {
		rows, err := p.data.engine.Backends().Store.Events().List(t.Context(),
			store.ListQuery{Limit: 200})
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		return slices.ContainsFunc(rows, func(r store.EventRecord) bool { return r.Type == completed })
	})
}

// A STATELESS NODE ROUTES BY THE DIRECTORY A DATA NODE HOLDS, so a suspended
// person's word is never attributed to their seat on a node that wins
// deliveries and holds no rows of its own.
//
// Decided by the wiring [engine.New] gives a node without `data`, on the real
// engine at both ends of a real leaf link, because that wiring is the whole of
// the guarantee: such a node runs no applier, so nothing would ever signal its
// party registry that a seat's standing moved, and the one way it learns is
// the directory [engine.New] hands it — a data node's, read through the
// estate's router on the registry's own timer ([engine.DirectoryRefresh]).
// Handed none, it builds the chart-only registry, which routes every suspended,
// retired and removed holder's accounts for as long as the node runs.
//
// The suspension is written on the DATA node and observed on the stateless
// one, so the reading below can only have crossed the leaf.
//
// Mutation: drop New's hand-over of the routed directory to a node without
// `data` and the first assertion fails at once — the registry consulted no
// directory — and the suspension never reaches it.
func TestAStatelessNodesPartyRegistryReadsTheDirectory(t *testing.T) {
	t.Parallel()
	p := startStatelessPair(t)
	agent := p.agent.engine
	standing := func() string {
		if reg := agent.Registry(); reg != nil {
			s := reg.Standing()
			return fmt.Sprintf("consulted=%v unread=%v withheld=%v",
				s.Consulted(), s.Unread(), s.Withheld())
		}
		return "no registry"
	}

	// BUILT FROM A READING FROM THE FIRST REGISTRY ON: a node with no
	// directory to read is chart-only, which is the defect. Unread is a
	// reading — the fail-closed one — so it passes here; the suspension
	// below is what proves a read through a data node lands.
	if reg := agent.Registry(); reg == nil || !reg.Standing().Consulted() {
		t.Fatalf("the stateless node's party registry was built from no "+
			"directory (%s): a node without `data` would route a suspended "+
			"person's word to their seat", standing())
	}

	// THE FOUNDER, created on their seat and then suspended, on the data
	// node — which is the only node holding the rows either record lands in.
	writer := p.data.engine.IAMWriter().As(iam.Principal{
		Login: iam.TokenLogin(e2eTokenID), Kind: iam.KindMachine,
		Grants: []iam.Grant{iam.GrantAdmin},
	})
	key, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	founder, err := iamdomain.CreatedPersonID(key.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Create(t.Context(), iamdomain.Creation{
		PersonID: founder, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Jane Doe", Email: janeAddress, Login: janeLogin, Seat: janeSeat,
		LinkExpiresAt: time.Now().Add(credential.EnrolmentLinkLifetime),
		OpID:          key.String(), Reason: "the stateless directory case",
	}); err != nil {
		t.Fatalf("create the founder on the data node: %v", err)
	}
	if _, err := writer.SetStage(t.Context(), founder, iam.StageSuspended,
		statelog.NewOpID(time.Now(), "suspend"), "the stateless directory case"); err != nil {
		t.Fatalf("suspend the founder on the data node: %v", err)
	}

	// AND THE STATELESS NODE WITHHOLDS THEIR SEAT, within one net of the
	// suspension landing — a reading nothing on this node could have made.
	waitFor(t, "the stateless node to withhold the suspended founder's seat", func() bool {
		why, withheld := agent.Registry().Withholding(janeSeat)
		return withheld && why == notify.WithheldSuspended
	}, standing)
	if _, routes := agent.Registry().ByExternalID("slack", "U0FOUNDER"); routes {
		t.Error("the stateless node still attributes the suspended founder's " +
			"Slack account to their seat")
	}
}

// A NODE THAT SERVES NO API CAN STILL BE PROBED, and its readiness is whether
// it is doing its work. A stateless node binds no API, no dashboard and no
// webhook — so an orchestrator running it had no way to restart one that
// wedged or to wait for one to join before replacing the next. Its probe
// surface answers both, from the real engine on both sides of a real leaf
// link: ready once a data node has admitted it, unready while no data node
// admits it, unready from the first moment of its own drain while /health
// stays 200, and every route that is not a probe refused throughout.
func TestAStatelessNodeAnswersItsProbes(t *testing.T) {
	t.Parallel()
	p := startStatelessPair(t)
	probes, _ := serveProbes(t, p.agent, p.agentBoot)

	waitFor(t, "the stateless node to be ready", func() bool {
		status, _ := probeAt(t, probes, http.MethodGet, "/ready")
		return status == http.StatusOK
	}, func() string {
		_, body := probeAt(t, probes, http.MethodGet, "/ready")
		return fmt.Sprint(body)
	})
	waitForSeat(t, p.agent, "ceo")
	status, health := probeAt(t, probes, http.MethodGet, "/health")
	if status != http.StatusOK || health["node"] != "agent-1" ||
		health["status"] != api.StatusOK || health["configured"] != true {
		t.Errorf("/health = %d %v, want 200 naming agent-1, ok and configured", status, health)
	}
	if roles := fmt.Sprint(health["roles"]); roles != "[seats]" {
		t.Errorf("/health roles = %s, want [seats]", roles)
	}
	waitFor(t, "/health to list the seat the node holds", func() bool {
		_, health := probeAt(t, probes, http.MethodGet, "/health")
		return fmt.Sprint(health["seats"]) == "[ceo]"
	})

	// NOTHING BUT THE PROBES. Every route the full surface serves is refused
	// here, by the router or by the guard in front of it.
	for _, route := range [][2]string{
		{http.MethodGet, "/"}, {http.MethodGet, "/dashboard"}, {http.MethodGet, "/agents"},
		{http.MethodGet, "/events"}, {http.MethodGet, "/query/viewer"},
		{http.MethodGet, "/ws/stream"}, {http.MethodGet, "/config"},
		{http.MethodPost, "/webhooks/github"}, {http.MethodPost, "/worklet"},
		{http.MethodPost, "/operator/act/create_work_item"},
	} {
		if status, _ := probeAt(t, probes, route[0], route[1]); status != http.StatusNotFound &&
			status != http.StatusUnauthorized {
			t.Errorf("%s %s = %d on a stateless node, want it refused", route[0], route[1], status)
		}
	}

	// NO DATA NODE ADMITS IT. The data node drains — it drops its presence,
	// so no copy of the estate is any longer one the stateless node can be
	// admitted by — while its broker, and so the leaf link and the
	// coordination store, stay up. The stateless node is linked and present,
	// and is not doing its work.
	p.data.engine.Drain(t.Context())
	waitFor(t, "the stateless node to report its admission withheld", func() bool {
		status, body := probeAt(t, probes, http.MethodGet, "/ready")
		return status == http.StatusServiceUnavailable &&
			body["reason"] == api.ReasonAdmissionWithheld
	}, func() string {
		_, body := probeAt(t, probes, http.MethodGet, "/ready")
		return fmt.Sprint(body)
	})

	// ITS OWN DRAIN outranks everything, from the first moment, and liveness
	// stays 200 for as long as the process is finishing its turns.
	p.agent.engine.Drain(t.Context())
	status, ready := probeAt(t, probes, http.MethodGet, "/ready")
	if status != http.StatusServiceUnavailable || ready["reason"] != api.ReasonDraining {
		t.Errorf("/ready during the drain = %d %v, want 503 naming the drain", status, ready)
	}
	status, health = probeAt(t, probes, http.MethodGet, "/health")
	if status != http.StatusOK || health["shutting_down"] != true ||
		health["status"] != api.StatusShuttingDown {
		t.Errorf("/health during the drain = %d %v, want 200 and shutting_down", status, health)
	}
}

// A NODE SEAT ADMISSION DOES NOT APPLY TO IS READY ON ITS PRESENCE. Admission
// is a seats node's in a mode that publishes, and nobody else's: a node whose
// roles leave out seats claims none whatever the gate says, and a node started
// in maintenance mode withholds every claim BY DESIGN — that is the mode doing
// its job, and a probe calling it unready would stall the rollout the mode
// exists for. Each of the two rules is what keeps such a node out of
// admission_withheld, and each case here is the one its rule decides.
func TestANodeAdmissionDoesNotApplyToIsReadyOnItsPresence(t *testing.T) {
	t.Parallel()
	model := newScriptedModel(t)
	cfg, err := config.ParseCompany([]byte(goldenCompany(model)))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	for _, tc := range []struct {
		name  string
		roles []string
		mode  statelog.MaintenanceMode
	}{
		{name: "without the seats role", roles: []string{"data", "workers"},
			mode: statelog.ModeNormal},
		{name: "in maintenance mode, running seats", roles: []string{"data", "seats", "workers"},
			mode: statelog.ModeMaintenance},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boot := config.DefaultBootstrap()
			boot.Node.ID = "probed"
			boot.Node.Roles = tc.roles
			boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
			brokertest.StoreIn(&boot.Stream, filepath.Join(t.TempDir(), "stream"))
			boot.Coordination.Type = config.CoordinationEmbeddedKV
			withKeyring(t, &boot)
			n := bootNodeWith(t, engine.Options{
				Bootstrap: &boot, Company: cfg, ActivatedAt: harnessActivation, Mode: tc.mode,
				Environment: nodeEnvironment(nil),
			}, model)
			probes, runtime := serveProbes(t, n, &boot)

			if work := runtime.Work(); work.Admission {
				t.Errorf("seat admission applies to a node %s: %+v", tc.name, work)
			}
			waitFor(t, "the node to be ready on its presence", func() bool {
				status, _ := probeAt(t, probes, http.MethodGet, "/ready")
				return status == http.StatusOK
			}, func() string {
				_, body := probeAt(t, probes, http.MethodGet, "/ready")
				return fmt.Sprint(body, " ", runtime.Work())
			})
			if tc.mode != statelog.ModeMaintenance {
				return
			}
			// THE PREMISE: the mode's gate really did withhold this node's
			// claims, so the rule is what kept it out of admission_withheld.
			waitFor(t, "a placement pass to withhold its claims", func() bool {
				last, swept := n.engine.Node().Host().LastSweep()
				return swept && last.Withheld
			})
			if status, body := probeAt(t, probes, http.MethodGet, "/ready"); status != http.StatusOK {
				t.Errorf("/ready after a withheld pass = %d %v, want 200", status, body)
			}
		})
	}
}

// serveProbes is a node's probe surface, wired to its engine by the same
// function `crewlet run` wires a node without the ingress role through, on a
// test listener torn down when the test ends. The runtime it answers from is
// returned beside it, so a case can read the facts /ready judged.
func serveProbes(t *testing.T, n *node, boot *config.Bootstrap) (*httptest.Server, api.ProbeRuntime) {
	t.Helper()
	return serveProbesOn(t, n, boot, nil)
}

// serveProbesOn is [serveProbes] on ln, a listener the case bound before the
// node — the one whose address the node hands its own boxes — or on a
// listener of its own where ln is nil.
func serveProbesOn(t *testing.T, n *node, boot *config.Bootstrap,
	ln net.Listener) (*httptest.Server, api.ProbeRuntime) {
	t.Helper()
	backends := n.engine.Backends()
	reconciler, err := n.engine.NewReconciler(t.Context(), engine.ReconcilerOptions{
		Store: backends.Store, Fleet: backends.Fleet, Queue: backends.Queue,
		NodeID: n.engine.Node().ID(),
	})
	if err != nil {
		t.Fatalf("reconciler: %v", err)
	}
	opts, err := api.EngineProbeOptions(boot, n.engine, reconciler)
	if err != nil {
		t.Fatalf("probe options: %v", err)
	}
	handler, err := api.Probes(opts)
	if err != nil {
		t.Fatalf("probe surface: %v", err)
	}
	srv := httptest.NewUnstartedServer(handler)
	if ln != nil {
		_ = srv.Listener.Close()
		srv.Listener = ln
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, opts.Runtime
}

// probeAt is one request to a probe surface: its status and its JSON body.
func probeAt(t *testing.T, srv *httptest.Server, method, path string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("%s %s answered %d with a body that is not JSON: %v", method, path,
			res.StatusCode, err)
	}
	return res.StatusCode, body
}
