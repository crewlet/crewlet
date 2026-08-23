package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MCPTransport is how the engine reaches a tool server.
type MCPTransport string

const (
	// TransportStdio launches a local subprocess and speaks over its pipes.
	TransportStdio MCPTransport = "stdio"
	// TransportHTTP connects to a remote Streamable HTTP server.
	TransportHTTP MCPTransport = "http"
)

// MCPTransports is the closed set.
var MCPTransports = []MCPTransport{TransportStdio, TransportHTTP}

// MCPServer is one tool server.
//
// ALL tool servers are declared here — including the ones an integration
// block also mentions. The integration sections carry non-tool config
// (admin credentials, webhook secrets) and the engine never derives an MCP
// server from one.
//
// # Shared or per-seat
//
// Shared (the default) launches ONE instance whose tools every agent can
// call. Setting it false makes this entry a TEMPLATE: no global instance is
// launched, and instead every seat that declares credentials for this
// server under role.mcp_env gets its own instance with those credentials
// applied — environment variables for stdio, HTTP headers for http.
//
// That is what makes each agent act as a distinct identity: its own tracker
// token, its own chat bot token, its own code-host authorization header. An
// http server with shared: false is therefore how an identity-bound remote
// server gets a per-agent token.
type MCPServer struct {
	// Name identifies the server everywhere: the client name, the tool
	// prefix in prompts, and the key seats declare credentials under.
	//
	// It must be non-empty. An empty name silently mis-categorised a
	// seat's MCP tools into the prompt's builtin block, so an agent was
	// told it had tools that behave nothing like the ones it got.
	Name string `yaml:"name" json:"name" js:"required" desc:"Server name; the key seats declare credentials under."`

	Transport MCPTransport `yaml:"transport,omitempty" json:"transport,omitempty" js:"enum=stdio|http" desc:"stdio (default) or http."`

	// Shared launches one instance for the company. False makes this a
	// per-seat template — see the type's own comment.
	Shared Toggle `yaml:"shared,omitempty" json:"shared,omitzero" desc:"One shared instance (default), or false for one per seat."`

	// The stdio fields.
	Command string            `yaml:"command,omitempty" json:"command,omitempty" desc:"Executable to launch (stdio)."`
	Args    []string          `yaml:"args,omitempty" json:"args,omitempty" desc:"Arguments for the executable (stdio)."`
	Env     map[string]string `secret:"true" yaml:"env,omitempty" json:"env,omitempty" desc:"Environment for the subprocess (stdio); ${VAR} supported."`

	// ToolPrefix disambiguates tools whose bare names would collide with
	// another server's.
	ToolPrefix string `yaml:"tool_prefix,omitempty" json:"tool_prefix,omitempty" desc:"Prefix applied to this server's tool names."`

	// The http fields.
	URL     string            `yaml:"url,omitempty" json:"url,omitempty" desc:"Endpoint of the remote server (http)."`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty" desc:"Headers sent with each request (http); ${VAR} supported."`

	// ToolAnnotations corrects behavioural hints per TOOL, keyed by bare
	// tool name.
	//
	// The engine derives what a tool may be used for — whether a sub-agent
	// may call it, whether it writes to a shared surface — from the hints
	// the server advertises. A server that under-annotates its tools can
	// be corrected here without touching engine code, and an override wins
	// over what the server said.
	ToolAnnotations map[string]ToolAnnotations `yaml:"tool_annotations,omitempty" json:"tool_annotations,omitempty" desc:"Per-tool behavioural-hint overrides, keyed by bare tool name."`

	// StartupTimeoutSeconds bounds launching the process (or opening the
	// session), the handshake, and the first tool listing.
	//
	// A server that never speaks is the failure this bounds: nothing
	// raises, so without a deadline the per-server error handling never
	// runs and one silent server holds up every seat behind it. The
	// default suits a package manager fetching a server that is not yet
	// cached, which is the slow case for a HEALTHY server.
	StartupTimeoutSeconds float64 `yaml:"startup_timeout_seconds,omitempty" json:"startup_timeout_seconds,omitempty" js:"min=0" desc:"Connect, handshake and first tool listing budget."`

	// RequestTimeoutSeconds bounds any one tool call.
	//
	// Raise it for a server whose tools genuinely run long; lower it for
	// one that should always answer quickly, so a wedged call surfaces as
	// a failed tool result the agent can react to rather than a turn that
	// never ends.
	RequestTimeoutSeconds float64 `yaml:"request_timeout_seconds,omitempty" json:"request_timeout_seconds,omitempty" js:"min=0" desc:"Cap on one tool call."`
}

// MCP timeout defaults. Startup is generous because a cold package fetch is
// the slow case for a healthy server; the request default matches the
// protocol SDK's own read timeout, so a stdio server and an http one have
// one ceiling rather than one per transport.
const (
	defaultMCPStartupSeconds = 60.0
	defaultMCPRequestSeconds = 300.0
)

// IsShared reports whether one instance serves the company, applying the
// true default.
func (m *MCPServer) IsShared() bool { return m.Shared.Or(true) }

// Kind is the transport, applying the stdio default.
func (m *MCPServer) Kind() MCPTransport {
	if m.Transport == "" {
		return TransportStdio
	}
	return m.Transport
}

// StartupTimeout is the connect budget, applying the default.
func (m *MCPServer) StartupTimeout() time.Duration {
	if m.StartupTimeoutSeconds <= 0 {
		return time.Duration(defaultMCPStartupSeconds * float64(time.Second))
	}
	return time.Duration(m.StartupTimeoutSeconds * float64(time.Second))
}

// RequestTimeout is the per-call budget, applying the default.
func (m *MCPServer) RequestTimeout() time.Duration {
	if m.RequestTimeoutSeconds <= 0 {
		return time.Duration(defaultMCPRequestSeconds * float64(time.Second))
	}
	return time.Duration(m.RequestTimeoutSeconds * float64(time.Second))
}

func (m *MCPServer) validate(path string) error {
	var p problems
	if strings.TrimSpace(m.Name) == "" {
		p.add(at(path, "name"), ErrMissing,
			"a server needs a name — it is the key seats declare credentials "+
				"under and how its tools are labelled in every prompt")
	}
	if m.Transport != "" && !oneOf(m.Transport, MCPTransports) {
		p.add(at(path, "transport"), ErrUnknownValue, "%q (want %s)",
			m.Transport, names(MCPTransports))
		return p.err()
	}

	switch m.Kind() {
	case TransportStdio:
		if strings.TrimSpace(m.Command) == "" {
			p.add(at(path, "command"), ErrMissing,
				"a stdio server needs a command to launch")
		}
		// Fields belonging to the other transport are refused rather than
		// ignored: an http field on a stdio server is read by nobody, and
		// an authorization header nobody sends looks exactly like one that
		// was rejected.
		if m.URL != "" {
			p.add(at(path, "url"), ErrConflict,
				"url only applies to transport http; a stdio server is launched, not dialled")
		}
		if len(m.Headers) > 0 {
			p.add(at(path, "headers"), ErrConflict,
				"headers only apply to transport http; a stdio server takes env instead")
		}
	case TransportHTTP:
		// Python never checked this, so an http entry with no url parsed
		// cleanly and produced a server that could never connect — with
		// the seat's tools simply absent from its prompt.
		if strings.TrimSpace(m.URL) == "" {
			p.add(at(path, "url"), ErrMissing, "an http server needs a URL to connect to")
		}
		if m.Command != "" || len(m.Args) > 0 {
			p.add(at(path, "command"), ErrConflict,
				"command and args only apply to transport stdio; an http server is dialled, not launched")
		}
		if len(m.Env) > 0 {
			p.add(at(path, "env"), ErrConflict,
				"env only applies to transport stdio; an http server takes headers instead")
		}
	}

	if m.StartupTimeoutSeconds < 0 {
		p.add(at(path, "startup_timeout_seconds"), ErrOutOfRange,
			"must be 0 (the %v s default) or positive, got %v",
			defaultMCPStartupSeconds, m.StartupTimeoutSeconds)
	}
	if m.RequestTimeoutSeconds < 0 {
		p.add(at(path, "request_timeout_seconds"), ErrOutOfRange,
			"must be 0 (the %v s default) or positive, got %v",
			defaultMCPRequestSeconds, m.RequestTimeoutSeconds)
	}
	return p.err()
}

// ToolAnnotations are the behavioural hints the engine reasons about a tool
// with — whether it only reads, whether it can destroy something, whether
// it touches the world outside this company.
//
// Each is a [Toggle] because UNSET is a real third answer: it means "take
// whatever the server advertised". A plain bool would make every unset hint
// an assertion that the tool is not read-only, which is the strictest
// possible reading of silence and would quietly withdraw tools from
// sub-agents.
type ToolAnnotations struct {
	// ReadOnly marks a tool that changes nothing, which is what makes it
	// safe to hand to a sub-agent.
	ReadOnly Toggle `yaml:"read_only,omitempty" json:"read_only,omitzero" desc:"The tool changes nothing."`
	// Destructive marks a tool whose effect cannot be undone.
	Destructive Toggle `yaml:"destructive,omitempty" json:"destructive,omitzero" desc:"The tool's effect cannot be undone."`
	// Idempotent marks a tool that can be repeated safely.
	Idempotent Toggle `yaml:"idempotent,omitempty" json:"idempotent,omitzero" desc:"Repeating the call is safe."`
	// OpenWorld marks a tool that reaches outside this company.
	OpenWorld Toggle `yaml:"open_world,omitempty" json:"open_world,omitzero" desc:"The tool reaches outside this company."`
}

// IsZero lets an unset annotation drop out of a round trip.
func (t ToolAnnotations) IsZero() bool {
	return !t.ReadOnly.IsSet() && !t.Destructive.IsSet() &&
		!t.Idempotent.IsSet() && !t.OpenWorld.IsSet()
}

// annotationAliases accepts the protocol's own camelCase hint names beside
// this config's snake_case ones.
//
// An operator correcting a server's hints is reading that server's tool
// definitions, where the fields are called readOnlyHint and friends.
// Rejecting the spelling they are looking at — while the config's key space
// otherwise fails closed on a typo — would make the strictness feel
// arbitrary rather than protective.
var annotationAliases = map[string]string{
	"readOnlyHint":    "read_only",
	"destructiveHint": "destructive",
	"idempotentHint":  "idempotent",
	"openWorldHint":   "open_world",
}

// annotationFields decodes the snake_case form without re-entering
// ToolAnnotations' own unmarshaler.
type annotationFields ToolAnnotations

var _ yaml.Unmarshaler = (*ToolAnnotations)(nil)

// UnmarshalYAML accepts either spelling, one key at a time, so a block may
// mix them without either being silently dropped.
func (t *ToolAnnotations) UnmarshalYAML(node *yaml.Node) error {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	if node.Tag == "!!null" {
		*t = ToolAnnotations{}
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: %w: tool annotations must be a mapping of hints", node.Line, ErrShape)
	}
	// Rewriting the aliased keys in a COPY keeps the strict decode: an
	// unknown key still reaches decodeKnown and is still rejected.
	renamed := *node
	renamed.Content = make([]*yaml.Node, len(node.Content))
	copy(renamed.Content, node.Content)
	for i := 0; i+1 < len(renamed.Content); i += 2 {
		key := renamed.Content[i]
		canonical, ok := annotationAliases[key.Value]
		if !ok {
			continue
		}
		replacement := *key
		replacement.Value = canonical
		renamed.Content[i] = &replacement
	}
	var fields annotationFields
	if err := decodeKnown(&renamed, &fields); err != nil {
		return err
	}
	*t = ToolAnnotations(fields)
	return nil
}
