package engine

import (
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/signedask"
)

// valueRing is a keyring for one key, derived from its id, so two engines
// built with the same id open each other's envelopes.
func valueRing(t *testing.T, id string) secrets.Cipher {
	t.Helper()
	sum := sha256.Sum256([]byte(id))
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: id, Keys: map[string][]byte{id: sum[:]}})
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

// echoingSearch is a company whose shared server writes the value of
// API_TOKEN into its one tool's description, stored as an old value, and is
// turned off.
func echoingSearch(t *testing.T) *Company {
	t.Helper()
	return companyFor(t, `
name: Acme
mcp_servers:
  - name: search
    command: `+os.Args[0]+`
    enabled: false
    env:
      `+ToolServerModeEnv+`: "server"
      `+ToolServerEchoEnv+`: "API_TOKEN"
      API_TOKEN: "old"
      `+procgrouptest.GORACE+`: "`+procgrouptest.StandInRaceOptions()+`"
roles:
  - name: CEO
    handle: ceo
`)
}

// A value is tested as the saved entry will run with it, turned on, and never
// recorded. Mutation: test without the value, or record it, and this is red.
func TestAValueIsTestedAsTheEntryWillRunWithItAndNeverRecorded(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}, testNode: "node-a", cipher: valueRing(t, "k1")}
	e.epoch.current.Store(echoingSearch(t))
	try := []MCPValueTry{{Key: "API_TOKEN", Variable: "MCP_SEARCH_API_TOKEN", Value: "candidate-not-real"}}

	result, err := e.CheckMCPServer(t.Context(), MCPCheckRequest{Server: "search", Values: try, By: checkingJane})
	if err != nil || !result.OK || len(result.Nodes) != 1 || !result.Nodes[0].Answered || result.Draft {
		t.Fatalf("the value's test = %+v, %v; want node-a's passing test of the saved entry", result, err)
	}
	tools := result.Nodes[0].Report.Tools
	// THE STORED VALUE IS SHORTER THAN ANY WITHHELD, so a test without the
	// candidate would print it after the "=".
	if len(tools) != 1 || !strings.HasSuffix(tools[0].Description, "=[withheld]") ||
		strings.Contains(tools[0].Description, "candidate") {
		t.Fatalf("the tool's description = %+v, want the value under test, withheld", tools)
	}
	if all, _ := fleet.ListMCPChecks(t.Context()); len(all) != 0 {
		t.Fatalf("a value's test was recorded as the server's latest: %+v", all)
	}
	if _, err := e.CheckMCPServer(t.Context(), MCPCheckRequest{Server: "search", By: checkingJane}); !errors.Is(err, ErrMCPServerOff) {
		t.Errorf("the saved entry's own test = %v, want it refused as turned off", err)
	}

	keyless := &Engine{backends: &Backends{Fleet: fleet}, testNode: "node-a"}
	keyless.epoch.current.Store(echoingSearch(t))
	if _, err := keyless.CheckMCPServer(t.Context(), MCPCheckRequest{Server: "search", Values: try,
		By: checkingJane}); !errors.Is(err, errNoValueKeyring) {
		t.Errorf("a node with no keyring = %v, want the value not sent unsealed", err)
	}
}

// A value under test crosses the broker sealed, bound to its request: another
// keyring or request opens nothing. Mutation: send it in the clear.
func TestAValueUnderTestCrossesTheBrokerSealed(t *testing.T) {
	t.Parallel()
	company := echoingSearch(t)
	asker := &Engine{testNode: "node-a", cipher: valueRing(t, "k1")}
	ask := mcpCheckAsk{Version: mcpCheckWire, ID: signedask.NewID(), Asker: "node-a", Server: "search",
		Nodes: []string{"node-a", "node-b"}}
	sealed, err := asker.cipher.Encrypt("candidate-not-real", mcpValueAAD(ask.ID, "MCP_SEARCH_API_TOKEN"))
	if err != nil {
		t.Fatal(err)
	}
	ask.Values = []mcpValueAsk{{Key: "API_TOKEN", Variable: "MCP_SEARCH_API_TOKEN", Sealed: sealed}}
	if strings.Contains(sealed, "candidate") {
		t.Fatal("the request carries the value in the clear")
	}

	peer := &Engine{testNode: "node-b", cipher: valueRing(t, "k1"), mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	peer.epoch.current.Store(company)
	report := peer.testHere(t.Context(), ask)
	if report == nil || !report.Serving() || len(report.Tools) != 1 ||
		!strings.HasSuffix(report.Tools[0].Description, "=[withheld]") {
		t.Fatalf("a peer holding the keyring = %+v, want it to test with the value it opened", report)
	}

	stranger := &Engine{testNode: "node-b", cipher: valueRing(t, "other"), mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	stranger.epoch.current.Store(company)
	lifted := ask
	lifted.ID = signedask.NewID()
	for name, tc := range map[string]struct {
		e   *Engine
		ask mcpCheckAsk
	}{
		"another keyring":        {stranger, ask},
		"an envelope moved over": {peer, lifted},
	} {
		got := tc.e.testHere(t.Context(), tc.ask)
		if got == nil || got.Serving() || got.Failure == nil ||
			!strings.Contains(got.Failure.Said, "could not open the value under test") ||
			strings.Contains(got.Failure.Said, "candidate") {
			t.Errorf("%s: %+v, want nothing tested and why", name, got)
		}
	}
}

// A draft is tested with every value typed for it, each under the variable it
// would be stored under, the one it replaces hidden, and each sealed apart:
// a key the draft spells another way gives way to the value under test.
// Mutation: point only the first value, or seal them as one, and this is red.
func TestADraftIsTestedWithEveryValueTypedForIt(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}, testNode: "node-a", cipher: valueRing(t, "k1")}
	e.epoch.current.Store(echoingSearch(t))
	draft := echoingSearch(t).Config.MCPServers[0]
	draft.Env = map[string]string{
		ToolServerModeEnv:    "server",
		ToolServerEchoEnv:    "API_TOKEN,TEAM",
		"api_token":          "old",
		"TEAM":               "old",
		procgrouptest.GORACE: procgrouptest.StandInRaceOptions(),
	}
	tried := []MCPValueTry{
		{Key: "API_TOKEN", Variable: "MCP_SEARCH_API_TOKEN", Value: "first-candidate-not-real"},
		{Key: "TEAM", Variable: "MCP_SEARCH_TEAM", Value: "second-candidate-not-real"},
	}

	result, err := e.CheckMCPServer(t.Context(), MCPCheckRequest{Server: "search", Draft: &draft, Values: tried,
		By: checkingJane})
	if err != nil || !result.OK || len(result.Nodes) != 1 || !result.Draft {
		t.Fatalf("the draft's test = %+v, %v; want node-a's passing test of the draft", result, err)
	}
	tools := result.Nodes[0].Report.Tools
	if len(tools) != 1 || tools[0].Description != "API_TOKEN=[withheld],TEAM=[withheld]" {
		t.Fatalf("the tool's description = %+v, want both values under test, each withheld", tools)
	}
	if all, _ := fleet.ListMCPChecks(t.Context()); len(all) != 0 {
		t.Fatalf("a draft's test was recorded as the server's latest: %+v", all)
	}

	ask := mcpCheckAsk{ID: signedask.NewID()}
	for _, v := range tried {
		sealed, sealErr := e.cipher.Encrypt(v.Value, mcpValueAAD(ask.ID, v.Variable))
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		ask.Values = append(ask.Values, mcpValueAsk{Key: v.Key, Variable: v.Variable, Sealed: sealed})
	}
	opened, err := e.openTried(ask)
	if err != nil || len(opened) != 2 || opened[0] != tried[0] || opened[1] != tried[1] {
		t.Fatalf("the values opened = %+v, %v; want both, each its own", opened, err)
	}
	swapped := ask
	swapped.Values = []mcpValueAsk{ask.Values[0], ask.Values[1]}
	swapped.Values[0].Sealed, swapped.Values[1].Sealed = ask.Values[1].Sealed, ask.Values[0].Sealed
	if _, err := e.openTried(swapped); err == nil {
		t.Error("a value sealed for one variable opened under another")
	}
}
