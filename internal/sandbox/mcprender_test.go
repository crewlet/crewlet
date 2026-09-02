package sandbox

import (
	"reflect"
	"testing"
)

func servers() []MCPServer {
	return []MCPServer{
		{Name: "files", Command: "mcp-files", Args: []string{"--root", "/src"},
			Env: map[string]string{"MODE": "read-write"}},
		{Name: "linear", Transport: "http", URL: "https://example.com/mcp",
			Headers: map[string]string{"X-Client": "crewlet"}},
		{Name: "unscoped", Command: "mcp-secret"},
	}
}

// Only the servers the seat named — never its whole surface. A coding agent
// inside a box reaches whatever it is given with no per-tool control left, so
// what it is given IS the decision.
func TestOnlyTheScopedServersReachTheBox(t *testing.T) {
	got := RenderMCP(servers(), []string{"files"}, nil)
	if len(got) != 1 {
		t.Fatalf("rendered %d servers, want just the scoped one: %v", len(got), got)
	}
	if _, leaked := got["unscoped"]; leaked {
		t.Fatal("a server the seat did not scope reached the box")
	}
}

func TestNothingScopedRendersNoConfigAtAll(t *testing.T) {
	if got := RenderMCP(servers(), nil, nil); got != nil {
		t.Fatalf("RenderMCP with nothing scoped = %v, want nil", got)
	}
}

// The agent must never be shown a server the engine does not know: it would
// spend a round discovering that it cannot connect.
func TestAScopedServerTheEngineDoesNotKnowIsSkipped(t *testing.T) {
	got := RenderMCP(servers(), []string{"files", "typo-in-the-name"}, nil)
	if len(got) != 1 {
		t.Fatalf("rendered %v, want only the server that exists", got)
	}
}

func TestAStdioServerKeepsItsCommandAndArguments(t *testing.T) {
	got := RenderMCP(servers(), []string{"files"}, nil)["files"]
	if got.Command != "mcp-files" {
		t.Fatalf("command = %q", got.Command)
	}
	if !reflect.DeepEqual(got.Args, []string{"--root", "/src"}) {
		t.Fatalf("args = %v", got.Args)
	}
	// THE OTHER TRANSPORT'S FIELDS ARE CLEARED. A stdio server carrying a
	// url or headers is a config nobody wrote, and leaving one there lets a
	// future runner render a credential into a file the transport never
	// reads.
	if got.URL != "" || got.Headers != nil {
		t.Fatalf("a stdio server carried http fields: %+v", got)
	}
}

func TestAnHttpServerKeepsItsUrlAndHeaders(t *testing.T) {
	got := RenderMCP(servers(), []string{"linear"}, nil)["linear"]
	if got.Transport != TransportHTTP || got.URL != "https://example.com/mcp" {
		t.Fatalf("server = %+v", got)
	}
	if got.Headers["X-Client"] != "crewlet" {
		t.Fatalf("headers = %v", got.Headers)
	}
	if got.Command != "" || got.Args != nil || got.Env != nil {
		t.Fatalf("an http server carried stdio fields: %+v", got)
	}
}

// A server declares the SHAPE and a seat declares WHO IT IS, so a seat that
// supplies a token is saying something more specific than the company-wide
// default it replaces.
func TestTheSeatsOwnCredentialsWinOverTheServersDefaults(t *testing.T) {
	creds := map[string]map[string]string{
		"files":  {"MODE": "read-only", "TOKEN": "seat-token"},
		"linear": {"Authorization": "Bearer seat-token"},
	}
	rendered := RenderMCP(servers(), []string{"files", "linear"}, creds)

	env := rendered["files"].Env
	if env["MODE"] != "read-only" {
		t.Fatalf("the seat's value did not win: %v", env)
	}
	if env["TOKEN"] != "seat-token" {
		t.Fatalf("the seat's own credential did not reach the box: %v", env)
	}
	headers := rendered["linear"].Headers
	if headers["Authorization"] != "Bearer seat-token" || headers["X-Client"] != "crewlet" {
		t.Fatalf("headers = %v", headers)
	}
}

// One seat's credentials must never reach another server's spec.
func TestOneServersCredentialsDoNotReachAnother(t *testing.T) {
	creds := map[string]map[string]string{"files": {"TOKEN": "for-files-only"}}
	rendered := RenderMCP(servers(), []string{"files", "linear"}, creds)
	if _, leaked := rendered["linear"].Headers["TOKEN"]; leaked {
		t.Fatalf("the files credential reached the linear server: %v",
			rendered["linear"].Headers)
	}
}

// The rendered args must not alias the configuration a later apply replaces.
func TestTheRenderedServerDoesNotAliasTheConfiguration(t *testing.T) {
	source := servers()
	rendered := RenderMCP(source, []string{"files"}, nil)
	rendered["files"].Args[0] = "--mutated"
	if source[0].Args[0] != "--root" {
		t.Fatal("the rendered server aliased the configuration")
	}
}

// AN UNRECOGNISED TRANSPORT IS SKIPPED, not rendered as stdio.
//
// The field used to be a bare `string` and the renderer branched on
// `== "http"`, so anything else — a typo, a transport a newer build knows and
// this one does not — fell through to the stdio branch and yielded a server
// with an empty command. The agent inside the box then spends a round failing
// to launch it, and nothing anywhere says why. Skipping is what an unknown
// NAME already did, and this is the same class of mistake.
func TestAServerWithAnUnrecognisedTransportIsSkipped(t *testing.T) {
	source := append(servers(), MCPServer{
		Name: "mistyped", Transport: "htp", URL: "https://example.com/mcp",
	})
	got := RenderMCP(source, []string{"files", "mistyped"}, nil)
	if _, rendered := got["mistyped"]; rendered {
		t.Errorf("a server with an unrecognised transport reached the box: %v", got["mistyped"])
	}
	// AND THE REST STILL RENDER. One bad entry must not cost a seat its
	// whole scoped surface.
	if _, ok := got["files"]; !ok {
		t.Errorf("the valid servers were lost too: %v", got)
	}
}

// EMPTY MEANS STDIO, which is the default an operator gets by saying nothing.
// Refusing it would drop every server that did not name a transport — which
// is most of them.
func TestTheTransportSetIsClosedAndEmptyMeansStdio(t *testing.T) {
	for _, tc := range []struct {
		transport Transport
		valid     bool
	}{
		{"", true},
		{TransportStdio, true},
		{TransportHTTP, true},
		{"htp", false},
		{"HTTP", false}, // Not case-folded: the config layer's set is not either.
		{"sse", false},  // A transport that exists in the wider MCP world and not here.
	} {
		if got := tc.transport.Valid(); got != tc.valid {
			t.Errorf("Transport(%q).Valid() = %v, want %v", tc.transport, got, tc.valid)
		}
	}
}
