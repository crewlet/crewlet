package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A node that holds no data, end to end.
//
// What these assert is the whole premise of the `data` role: a node with a
// scratch store and a leaf broker — no JetStream, no replicated estate, no
// index — runs a seat whose tools read and write the company's tracker and
// knowledge base, and every one of those reads and writes lands in, and is
// answered from, a node that DOES hold them. The unit suites cover the wire
// against fakes; this is the real engine on both sides of a real leaf link.

// statelessPair is one data member with a leaf listener and one stateless
// node joined to it, with the seat `ceo` pinned to the stateless one.
type statelessPair struct {
	data, agent *node
	agentStore  string
	agentBoot   *config.Bootstrap
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
	logs.attribute(t)
	model := newScriptedModel(t)
	doc := fmt.Sprintf(companyDoc, model.url)
	if amend != nil {
		doc = amend(doc)
	}
	// PINNED, so the one agent seat runs where the data is not: a turn that
	// happened to land on the data node would test nothing here.
	doc = strings.Replace(doc, "    handle: ceo\n    llm: scripted\n",
		"    handle: ceo\n    llm: scripted\n    placement:\n      node: agent-1\n", 1)
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	data, dataBoot := bootDataMember(t, cfg, model, env)
	data.app, data.server = serveAPI(t, data.engine, dataBoot, nil)
	port := dataBoot.Stream.Leaf.Port

	agentDir := t.TempDir()
	agentBoot := config.DefaultBootstrap()
	agentBoot.Node.ID = "agent-1"
	agentBoot.Node.Roles = []string{"seats"}
	agentBoot.Store.Path = filepath.Join(agentDir, "crewlet.db")
	agentBoot.Store.Scratch = true
	agentBoot.Stream.Leaf.URLs = []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", port)}
	agentBoot.Coordination.Type = config.CoordinationEmbeddedKV
	agent := bootNode(t, &agentBoot, cfg, model, env)
	return statelessPair{data: data, agent: agent, agentStore: agentDir, agentBoot: &agentBoot}
}

// dataMemberBootstrap is the pair's data member: a node holding the data, with
// a leaf listener on port for the stateless node to join through.
func dataMemberBootstrap(t *testing.T, port int) *config.Bootstrap {
	t.Helper()
	boot := config.DefaultBootstrap()
	boot.Node.ID = "data-a"
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	boot.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
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
func bootDataMember(t *testing.T, cfg *config.Company, model *scriptedModel,
	env map[string]string) (*node, *config.Bootstrap) {
	t.Helper()
	for attempt := 1; ; attempt++ {
		boot := dataMemberBootstrap(t, leafPort(t))
		n, err := newNode(t, nodeOptions(boot, cfg, env), model)
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
	env map[string]string) *node {
	t.Helper()
	return bootNodeWith(t, nodeOptions(boot, cfg, env), model)
}

// nodeOptions is the engine options every harness node is built with: its
// bootstrap and company, the harness's activation instant, and its own
// environment ([nodeEnvironment]) rather than the process's.
func nodeOptions(boot *config.Bootstrap, cfg *config.Company, env map[string]string) engine.Options {
	return engine.Options{
		Bootstrap: boot, Company: cfg, ActivatedAt: harnessActivation,
		Environment: nodeEnvironment(env),
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
func newNode(t *testing.T, opts engine.Options, model *scriptedModel) (*node, error) {
	t.Helper()
	logs.attribute(t)
	seedStore(t, opts.Bootstrap)
	id := opts.Bootstrap.Node.ID
	e, err := engine.New(t.Context(), opts)
	if err != nil {
		return nil, fmt.Errorf("engine.New(%s): %w", id, err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if err := e.Start(t.Context()); err != nil {
		return nil, fmt.Errorf("engine.Start(%s): %w", id, err)
	}
	return &node{engine: e, model: model, id: id}, nil
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
	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(companyDoc, model.url)))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	boot := dataMemberBootstrap(t, held.Addr().(*net.TCPAddr).Port)
	_, err = newNode(t, nodeOptions(boot, cfg, nil), model)
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

	p.agent.model.callOnExecute(tracker.CreateWorkItemTool, map[string]any{
		"project": "ENG", "title": "filed from a node that holds nothing",
	})
	wakeWithMessage(t, p.agent, "ceo")

	var detail tracker.TaskDetail
	waitFor(t, "the task on the data node", func() bool {
		got, err := p.data.engine.Tracker().Task(t.Context(), "ENG-1", tracker.DetailWants{},
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

// A SEARCH FROM A NODE THAT HOLDS NO INDEX IS ANSWERED BY ONE THAT DOES.
func TestAStatelessNodeSearchesTheFleetsKnowledge(t *testing.T) {
	t.Parallel()
	p := startStatelessPair(t)
	if _, err := p.data.engine.PagesStore().Create(t.Context(), pageOperator(), pages.NewPage{
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
		{http.MethodPost, "/webhooks/github"}, {http.MethodPost, "/work"},
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
	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(companyDoc, model.url)))
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
			boot.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
			boot.Coordination.Type = config.CoordinationEmbeddedKV
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
	backends := n.engine.Backends()
	reconciler, err := n.engine.NewReconciler(engine.ReconcilerOptions{
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
	srv := httptest.NewServer(handler)
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
