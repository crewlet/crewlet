package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/websocket"
)

// CODEX'S NAMES, as rust-v0.171.0's tools.rs makes them and 0.171.0 was
// measured handing them to its model: each character outside [A-Za-z0-9_] is
// `_`, and the mcp__ prefix goes on unless the cleaned name opens with it, so
// `mcp::crewlet` and `mcp__crewlet` name the namespace `crewlet` does.
//
// Mutation: keep `:` or drop the prefix's exception, and this goes red.
func TestTheBridgeCheckNamesANamespaceAsCodexDoes(t *testing.T) {
	t.Parallel()
	for server, want := range map[string]string{
		"crewlet": "mcp__crewlet", "mcp::crewlet": "mcp__crewlet", "mcp__crewlet": "mcp__crewlet",
		"mcp//crewlet": "mcp__crewlet", "mcp.-crewlet": "mcp__crewlet", "crewle": "mcp__crewle",
		"crewlet-x": "mcp__crewlet_x", "mcp_crewlet": "mcp__mcp_crewlet", "é": "mcp___", "": "mcp___",
	} {
		if got := codexNamespace(server); got != want {
			t.Errorf("server %q: namespace %q, want %q", server, got, want)
		}
	}
}

// bridgeAt is the bridge's address in the check's cases.
const bridgeAt = "http://127.0.0.1:9100/mcp/token"

// bridgeCase is Codex as config/read and mcpServerStatus/list answer for a
// run whose bridge `crewlet`, at bridgeAt from the run's own layer, lists
// submit_work and ask_and_wait, beside others (each from /etc/codex/config.toml
// listing tools, nil for a server whose listing failed).
func bridgeCase(others map[string][]string) (codexConfigRead, []codexServerStatus) {
	url := bridgeAt
	user := json.RawMessage(`{"type":"user","file":"/h/.codex/config.toml"}`)
	system := json.RawMessage(`{"type":"system","file":"/etc/codex/config.toml"}`)
	read := codexConfigRead{
		Config: codexLayerConfig{MCPServers: map[string]codexServerEntry{"crewlet": {URL: &url}}},
		Layers: []codexLayer{
			{Name: user, Config: codexLayerConfig{MCPServers: map[string]codexServerEntry{"crewlet": {URL: &url}}}},
			{Name: system, Config: codexLayerConfig{MCPServers: map[string]codexServerEntry{}}},
		},
	}
	listing := []codexServerStatus{{Name: "crewlet", Tools: map[string]json.RawMessage{
		"submit_work": json.RawMessage(`{}`), "ask_and_wait": json.RawMessage(`{}`)}}}
	for name, tools := range others {
		other := "http://127.0.0.1:9101/mcp"
		read.Config.MCPServers[name] = codexServerEntry{URL: &other}
		read.Layers[1].Config.MCPServers[name] = codexServerEntry{URL: &other}
		s := codexServerStatus{Name: name, Tools: map[string]json.RawMessage{}}
		if tools == nil {
			failed := "connection refused"
			s.ToolsError = &failed
		}
		for _, tool := range tools {
			s.Tools[tool] = json.RawMessage(`{}`)
		}
		listing = append(listing, s)
	}
	return read, listing
}

// theBridge is the bridge as the run's own configuration declares it.
func theBridge() []bridgeServer {
	sum := sha256.Sum256([]byte(bridgeAt))
	return []bridgeServer{{Name: "crewlet", Digests: []string{"0f", hex.EncodeToString(sum[:])}}}
}

// THE CHECK REFUSES EVERY SHAPE THAT RENAMES THE BRIDGE UNSEEN, naming the
// server and the file that declares it, and leaves an ordinary server alone:
// a server whose namespace cleans to the bridge's (both get hashed, measured
// on 0.171.0); one of whose tools joins to a bridge tool's name (`crewle`'s
// `tsubmit_work` left the bridge `submit_work_<hash>`, measured), whichever
// sorts first; one whose tools could not be listed though its namespace
// opens such a name; the bridge served elsewhere or turned off; and a feature
// that renames the tools. A server turned off reaches no model.
//
// Mutation: drop any one rule of bridgeRefusals, and its case goes red.
func TestTheBridgeCheckRefusesEveryServerThatWouldRenameTheBridge(t *testing.T) {
	t.Parallel()
	command, other, off := "evil", "http://127.0.0.1:9101/mcp", false
	for _, c := range []struct {
		name   string
		others map[string][]string
		alter  func(*codexConfigRead, *[]codexServerStatus)
		want   []string // every refusal holds each; none for nil
	}{
		{name: "an ordinary server", others: map[string][]string{"docs": {"search"}, "crewlet_docs": {"search"}}},
		{name: "a namespace mcp__crewlet", others: map[string][]string{"mcp__crewlet": {"x"}},
			want: []string{`"mcp__crewlet"`, "/etc/codex/config.toml", "namespace"}},
		{name: "a namespace mcp::crewlet", others: map[string][]string{"mcp::crewlet": {"submit_work"}},
			want: []string{`"mcp::crewlet"`, "/etc/codex/config.toml", "namespace"}},
		{name: "a joined name sorting first", others: map[string][]string{"crewle": {"search", "tsubmit_work"}},
			want: []string{`"crewle"`, `"tsubmit_work"`, `"submit_work"`, "/etc/codex/config.toml"}},
		{name: "a joined name sorting last", others: map[string][]string{"mcp__crewletask": {"_and_wait"}},
			want: []string{`"mcp__crewletask"`, `"_and_wait"`, `"ask_and_wait"`}},
		{name: "an unlisted server opening a bridge name", others: map[string][]string{"crewl": nil},
			want: []string{`"crewl"`, "could not be listed", "connection refused"}},
		{name: "an unlisted ordinary server", others: map[string][]string{"docs": nil}},
		{name: "a turned-off server", others: map[string][]string{"mcp::crewlet": {"submit_work"}},
			alter: func(r *codexConfigRead, _ *[]codexServerStatus) {
				r.Config.MCPServers["mcp::crewlet"] = codexServerEntry{URL: &other, Enabled: &off}
			}},
		{name: "the bridge served elsewhere",
			alter: func(r *codexConfigRead, _ *[]codexServerStatus) {
				r.Config.MCPServers["crewlet"] = codexServerEntry{URL: &other}
				r.Layers = append([]codexLayer{{Name: json.RawMessage(`{"type":"legacyManagedConfigTomlFromFile","file":"/etc/codex/managed_config.toml"}`),
					Config: codexLayerConfig{MCPServers: map[string]codexServerEntry{"crewlet": {URL: &other}}}}}, r.Layers...)
			}, want: []string{`"crewlet"`, "elsewhere", "/etc/codex/managed_config.toml"}},
		{name: "the bridge served by a command",
			alter: func(r *codexConfigRead, _ *[]codexServerStatus) {
				url := bridgeAt
				r.Config.MCPServers["crewlet"] = codexServerEntry{URL: &url, Command: &command}
			}, want: []string{`"crewlet"`, "elsewhere"}},
		{name: "the bridge turned off",
			alter: func(r *codexConfigRead, _ *[]codexServerStatus) {
				url := bridgeAt
				r.Config.MCPServers["crewlet"] = codexServerEntry{URL: &url, Enabled: &off}
			}, want: []string{`"crewlet"`, "turns off"}},
		{name: "the bridge unlisted",
			alter: func(_ *codexConfigRead, l *[]codexServerStatus) { *l = (*l)[1:] },
			want:  []string{`"crewlet"`, "could not list"}},
		{name: "a feature dropping the prefix",
			alter: func(r *codexConfigRead, _ *[]codexServerStatus) {
				r.Config.Features = map[string]json.RawMessage{"non_prefixed_mcp_tool_names": json.RawMessage("true"),
					"mcp_2026_07_28": json.RawMessage("true")}
			}, want: []string{"features.non_prefixed_mcp_tool_names"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			read, listing := bridgeCase(c.others)
			if c.alter != nil {
				c.alter(&read, &listing)
			}
			got := bridgeRefusals(theBridge(), read, listing)
			if c.want == nil {
				if len(got) > 0 {
					t.Fatalf("refused %q", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("refused %d times, want once: %q", len(got), got)
			}
			for _, w := range c.want {
				if !strings.Contains(got[0], w) {
					t.Errorf("the refusal %q does not say %q", got[0], w)
				}
			}
		})
	}
}

// fakeCodexForCheck is an app-server answering config/read with read and
// mcpServerStatus/list with listing, a server a page, recording each
// config/read's parameters.
func fakeCodexForCheck(t *testing.T, read codexConfigRead, listing []codexServerStatus) (string, *record[json.RawMessage]) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	asked := &record[json.RawMessage]{}
	srv := &http.Server{Handler: websocket.Server{Handler: func(ws *websocket.Conn) {
		dec := json.NewDecoder(ws)
		for {
			var m wire
			if dec.Decode(&m) != nil {
				return
			}
			var result any = map[string]any{}
			switch m.Method {
			case "initialized":
				continue
			case "config/read":
				asked.add(m.Params)
				result = read
			case "mcpServerStatus/list":
				var p struct {
					Cursor string `json:"cursor"`
				}
				_ = json.Unmarshal(m.Params, &p)
				i := 0
				if p.Cursor != "" {
					i = int(p.Cursor[0] - '0')
				}
				page := map[string]any{"data": listing[i : i+1], "nextCursor": nil}
				if i+1 < len(listing) {
					page["nextCursor"] = string(rune('0' + i + 1))
				}
				result = page
			}
			answer, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
			if _, err := ws.Write(answer); err != nil {
				return
			}
		}
	}}}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path, asked
}

// THE CHECK READS CODEX'S OWN ANSWERS as the thread will start: the
// configuration from the working directory, every page of the listing, and
// it exits 1 naming the server and the tool the last page brings, 0 for an
// ordinary one.
//
// Mutation: read only the listing's first page, or exit 0 on a refusal, and
// this goes red.
func TestTheBridgeCheckReadsEveryPageOfCodexsListing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		other string
		code  int
	}{{"docs", exitDone}, {"crewle", exitError}} {
		read, listing := bridgeCase(map[string][]string{c.other: {"tsubmit_work"}})
		socket, asked := fakeCodexForCheck(t, read, listing)
		raw, _ := json.Marshal(bridgeConfig{Socket: socket, Wait: 30, Bridges: theBridge()})
		config := filepath.Join(t.TempDir(), "bridge.json")
		if err := os.WriteFile(config, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errs bytes.Buffer
		code, once := runOnce(context.Background(), []string{"-adapter", "codex-bridge", "-config", config},
			func(string) string { return "" }, strings.NewReader(""), &out, &errs)
		if !once || code != c.code {
			t.Errorf("%s: exit %d (one-shot %v), want %d; stderr %s", c.other, code, once, c.code, errs.String())
		}
		if c.code == exitError && !strings.Contains(errs.String(), `"crewle" (declared in /etc/codex/config.toml) lists the tool "tsubmit_work"`) {
			t.Errorf("%s: the refusal does not name the server and the tool its page lists: %s", c.other, errs.String())
		}
		cwd, _ := os.Getwd()
		var p struct {
			IncludeLayers bool   `json:"includeLayers"`
			Cwd           string `json:"cwd"`
		}
		if got := asked.all(); len(got) != 1 || json.Unmarshal(got[0], &p) != nil || !p.IncludeLayers || p.Cwd != cwd {
			t.Errorf("%s: config/read was asked %s, want once with its layers from %s", c.other, got, cwd)
		}
	}
}
