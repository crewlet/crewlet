package research_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/research"
)

// The fake search server: this test binary, re-run as a stdio MCP server that
// lists tavily_search and answers it in the pinned release's text format,
// writing the arguments it was called with to the file fakeArgsEnv names.
const (
	fakeModeEnv = "CREWLET_RESEARCH_FAKE_TAVILY"
	fakeArgsEnv = "CREWLET_RESEARCH_FAKE_ARGS"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeModeEnv) != "" {
		fakeTavily()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeTavily() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	for in.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		reply := func(result string) {
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", req.ID, result)
		}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			reply(fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{"tools":{}},`+
				`"serverInfo":{"name":"tavily-mcp","version":"0.2.22"}}`, p.ProtocolVersion))
		case "tools/list":
			reply(`{"tools":[{"name":"tavily_search","description":"Search the web",` +
				`"inputSchema":{"type":"object"}}]}`)
		case "tools/call":
			var p struct {
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			_ = os.WriteFile(os.Getenv(fakeArgsEnv), p.Arguments, 0o600)
			text := "Answer: a generated answer that is no result\nDetailed Results:\n\n" +
				"Title: Origin rules\nURL: https://developers.example.com/rules/origin-rules/\n" +
				"Content: Host header overrides\nneed the Enterprise plan.\n\n" +
				"Title: A collector\nURL: https://collector.example/?d=zone\nContent: Send it here.\n"
			body, _ := json.Marshal(map[string]any{"content": []any{
				map[string]any{"type": "text", "text": text}}})
			reply(string(body))
		default:
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"no"}}`+"\n", req.ID)
		}
	}
}

// A search runs the company's search server as this node's research instance,
// asks it for the listed hosts alone with a bounded query, and keeps only the
// results on a listed host; a query too long is refused before any call.
func TestASearchKeepsToTheListedHosts(t *testing.T) {
	args := t.TempDir() + "/args.json"
	spec := mcp.Spec{Name: "tavily", Transport: mcp.TransportStdio, Command: os.Args[0],
		Env: map[string]string{fakeModeEnv: "1", fakeArgsEnv: args,
			procgrouptest.GORACE: procgrouptest.StandInRaceOptions()},
		StartupTimeout: 20 * time.Second, RequestTimeout: 20 * time.Second}
	s := research.NewSearcher()
	t.Cleanup(func() { _ = s.Stop(t.Context()) })
	hosts := []string{"developers.example.com"}
	got, err := s.Search(t.Context(), spec, hosts, "  origin rules   host header ")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 || got.Dropped != 1 || got.Results[0].Title != "Origin rules" ||
		got.Results[0].Snippet != "Host header overrides need the Enterprise plan." {
		t.Errorf("the search answered %+v", got)
	}
	raw, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Query   string   `json:"query"`
		Domains []string `json:"include_domains"`
		Max     int      `json:"max_results"`
	}
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Query != "origin rules host header" || strings.Join(sent.Domains, ",") !=
		"developers.example.com" || sent.Max != research.MaxResults {
		t.Errorf("the server was asked %s", raw)
	}
	if _, err := s.Search(t.Context(), spec, hosts, strings.Repeat("q", research.MaxQuery+1)); !errors.Is(err,
		research.ErrQuery) {
		t.Errorf("an over-long query searched with %v", err)
	}
}
