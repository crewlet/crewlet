package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
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
}

func startStatelessPair(t *testing.T) statelessPair {
	t.Helper()
	model := newScriptedModel(t)
	doc := fmt.Sprintf(companyDoc, model.url)
	// PINNED, so the one agent seat runs where the data is not: a turn that
	// happened to land on the data node would test nothing here.
	doc = strings.Replace(doc, "    handle: ceo\n    llm: scripted\n",
		"    handle: ceo\n    llm: scripted\n    placement:\n      node: agent-1\n", 1)
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	port := leafPort(t)

	dataBoot := config.DefaultBootstrap()
	dataBoot.Node.ID = "data-a"
	dataBoot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	dataBoot.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	dataBoot.Stream.Leaf.Host, dataBoot.Stream.Leaf.Port = "127.0.0.1", port
	dataBoot.Coordination.Type = config.CoordinationEmbeddedKV
	if err := dataBoot.Validate(); err != nil {
		t.Fatalf("the data node's bootstrap: %v", err)
	}
	data := bootNode(t, &dataBoot, cfg, model)
	data.app, data.server = serveAPI(t, data.engine, &dataBoot, nil)

	agentDir := t.TempDir()
	agentBoot := config.DefaultBootstrap()
	agentBoot.Node.ID = "agent-1"
	agentBoot.Node.Roles = []string{"seats"}
	agentBoot.Store.Path = filepath.Join(agentDir, "crewlet.db")
	agentBoot.Store.Scratch = true
	agentBoot.Stream.Leaf.URLs = []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", port)}
	agentBoot.Coordination.Type = config.CoordinationEmbeddedKV
	if err := agentBoot.Validate(); err != nil {
		t.Fatalf("the stateless node's bootstrap: %v", err)
	}
	agent := bootNode(t, &agentBoot, cfg, model)
	return statelessPair{data: data, agent: agent, agentStore: agentDir}
}

// bootNode builds and starts one engine.
func bootNode(t *testing.T, boot *config.Bootstrap, cfg *config.Company, model *scriptedModel) *node {
	t.Helper()
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: boot, Company: cfg, ActivatedAt: harnessActivation,
	})
	if err != nil {
		t.Fatalf("engine.New(%s): %v", boot.Node.ID, err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("engine.Start(%s): %v", boot.Node.ID, err)
	}
	return &node{engine: e, model: model, id: boot.Node.ID}
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
		hits := searcher.Search(t.Context(), knowledge.Query{
			Text: "rollback drain node", Org: p.agent.engine.Company().Org, Limit: 5,
		})
		return slices.ContainsFunc(hits, func(h knowledge.Hit) bool { return h.Title == "Rollback runbook" })
	})
}

// THE RECORD OF WHAT A STATELESS NODE DID OUTLIVES IT. Its turns' audit rows
// cannot live in a store deleted at every boot, so they are in a data node's
// event log — and the data node ran no turn itself, so every phase row there
// came across.
func TestAStatelessNodesAuditTrailLandsOnADataNode(t *testing.T) {
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
