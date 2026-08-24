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

func TestAStdioServerRendersItsCommandAndArguments(t *testing.T) {
	got := RenderMCP(servers(), []string{"files"}, nil)["files"]
	if got["command"] != "mcp-files" {
		t.Fatalf("command = %v", got["command"])
	}
	if args, _ := got["args"].([]string); !reflect.DeepEqual(args, []string{"--root", "/src"}) {
		t.Fatalf("args = %v", got["args"])
	}
	if got["type"] != nil {
		t.Fatalf("a stdio server was given a transport tag: %v", got)
	}
}

func TestAnHttpServerRendersItsUrlAndHeaders(t *testing.T) {
	got := RenderMCP(servers(), []string{"linear"}, nil)["linear"]
	if got["type"] != "http" || got["url"] != "https://example.com/mcp" {
		t.Fatalf("spec = %v", got)
	}
	headers, _ := got["headers"].(map[string]string)
	if headers["X-Client"] != "crewlet" {
		t.Fatalf("headers = %v", headers)
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

	env, _ := rendered["files"]["env"].(map[string]string)
	if env["MODE"] != "read-only" {
		t.Fatalf("the seat's value did not win: %v", env)
	}
	if env["TOKEN"] != "seat-token" {
		t.Fatalf("the seat's own credential did not reach the box: %v", env)
	}
	headers, _ := rendered["linear"]["headers"].(map[string]string)
	if headers["Authorization"] != "Bearer seat-token" || headers["X-Client"] != "crewlet" {
		t.Fatalf("headers = %v", headers)
	}
}

// One seat's credentials must never reach another server's spec.
func TestOneServersCredentialsDoNotReachAnother(t *testing.T) {
	creds := map[string]map[string]string{"files": {"TOKEN": "for-files-only"}}
	rendered := RenderMCP(servers(), []string{"files", "linear"}, creds)
	headers, _ := rendered["linear"]["headers"].(map[string]string)
	if _, leaked := headers["TOKEN"]; leaked {
		t.Fatalf("the files credential reached the linear server: %v", headers)
	}
}

// The rendered args must not alias the configuration a later apply replaces.
func TestTheRenderedSpecDoesNotAliasTheConfiguration(t *testing.T) {
	source := servers()
	rendered := RenderMCP(source, []string{"files"}, nil)
	args, _ := rendered["files"]["args"].([]string)
	args[0] = "--mutated"
	if source[0].Args[0] != "--root" {
		t.Fatal("the rendered spec aliased the server configuration")
	}
}
