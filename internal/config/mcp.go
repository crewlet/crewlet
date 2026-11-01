package config

import (
	"net/textproto"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
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

// ToolServerName is the name a seat's coding CLI sees the seat's own tools
// under, wherever the CLI runs: the MCP server a phase on this host dials
// through `crewlet mcp-relay`, and the tool bridge the seat's executor — its
// CLI's own run in a box — dials over HTTP. One name, so a seat's tools are
// spelled one way in every phase — the prefix its model sees
// (`mcp__crewlet__reply`) is the same for the reviewer on this host and the
// executor in its box — and the built-in CLI profiles, which write it into
// each CLI's own configuration grammar, are held to it by a test in cliagent.
//
// RESERVED: an `mcp_servers` entry may not take it. The engine writes the
// bridge into every executor box's server list under this key, so a company
// server of the same name would be overwritten there — its tools gone from
// the run with no validation error and no log line saying why. Named here
// rather than beside either server because this is where the reservation is
// enforced, and every reader takes the one value.
//
// "crewlet" rather than the seat's handle because it reaches the CLI's own
// prompt: a tool called `mcp__swe__reply` reads as somebody else's tool, where
// `mcp__crewlet__reply` reads as the agent's own.
const ToolServerName = "crewlet"

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
	// told it had tools that behave nothing like the ones it got. And it is
	// held to one spelling every place it lands can take ([mcpServerName],
	// [MaxMCPServerName]), which the schema states too.
	Name string `yaml:"name" json:"name" js:"required;pattern=^[a-z0-9]([a-z0-9-]*[a-z0-9])?$;maxlen=32" desc:"Server name: lowercase letters, digits and hyphens, at most 32 characters; the key seats declare credentials under."`

	// Enabled turns the server off without removing it: off, nothing starts
	// it, no seat is granted it and no executor box is given it, while the
	// entry, its seats' mcp_env blocks and its box listings stay in the
	// document, validated as ever, for the day it is turned back on. See
	// [MCPServer.IsEnabled].
	Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Run this server (default on). Off keeps the entry and everything that names it, and starts nothing."`

	// Description says what the server is for, in the operator's words. The
	// executor's prompt lists it beside the server's name, which is all a
	// model is shown of a server before it asks for its tools, so it is
	// what tells the model which server to look in. See
	// [MaxMCPDescription] for its bound.
	Description string `yaml:"description,omitempty" json:"description,omitempty" js:"maxlen=200" desc:"What this server is for, one line of at most 200 characters; listed beside its name in each agent's prompt."`

	Transport MCPTransport `yaml:"transport,omitempty" json:"transport,omitempty" js:"enum=stdio|http" desc:"stdio (default) or http."`

	// Shared launches one instance for the company. False makes this a
	// per-seat template — see the type's own comment.
	Shared Toggle `yaml:"shared,omitempty" json:"shared,omitzero" desc:"One shared instance (default), or false for one per seat."`

	// The stdio fields.
	Command string            `yaml:"command,omitempty" json:"command,omitempty" desc:"Executable to launch (stdio)."`
	Args    []string          `yaml:"args,omitempty" json:"args,omitempty" desc:"Arguments for the executable (stdio)."`
	Env     map[string]string `secret:"true" yaml:"env,omitempty" json:"env,omitempty" desc:"Environment for the subprocess (stdio); ${VAR} supported."`

	// ToolPrefix disambiguates tools whose bare names would collide with
	// another server's. Held to a model's tool-name characters and to
	// [MaxMCPToolPrefix], which the schema's bound is held to by a test.
	ToolPrefix string `yaml:"tool_prefix,omitempty" json:"tool_prefix,omitempty" js:"pattern=^[A-Za-z0-9_-]+$;maxlen=49" desc:"Prefix put in front of every tool this server serves: letters, digits, _ and -, at most 49 characters."`

	// The http fields.
	//
	// Headers is tagged as a credential for the reason Env is: it is where
	// an http server's authorization header lives, which is the same token
	// a stdio server receives through its environment. An untagged map
	// published that token verbatim on every config read.
	URL     string            `yaml:"url,omitempty" json:"url,omitempty" desc:"Endpoint of the remote server (http)."`
	Headers map[string]string `secret:"true" yaml:"headers,omitempty" json:"headers,omitempty" desc:"Headers sent with each request (http); ${VAR} supported."`

	// SeatCredentials declares which integration's per-seat credential a
	// per-seat server reads, and under which key, so the engine binds each
	// seat's own sealed credential to it (D15). Absent, a server this build
	// recognises by its command or address reads what [SeatToolBindings]
	// says; see toolbinding.go.
	SeatCredentials []SeatCredential `yaml:"seat_credentials,omitempty" json:"seat_credentials,omitempty" desc:"Per-seat credentials this server reads: integration, field, and the variable or header it reads each under. Absent for a server the engine recognises."`

	// OptionalKeys names the keys whose value may resolve to nothing: of
	// this server's env or headers, and of a seat's or unit's mcp_env block
	// for it, which lands in the same environment or the same headers.
	//
	// EVERY OTHER REFERENCE MUST RESOLVE before the server is started — on
	// the engine host or in a coding agent's box — because a credential that
	// arrives empty either fails the server or lets it act as nobody
	// (internal/engine, toolcredentials.go). What that rule cannot tell on
	// its own is a host setting passed through by name, like a private
	// registry's NPM_CONFIG_REGISTRY, which most hosts never set. Such a key
	// is listed here, and when any reference in its value resolves to
	// nothing the key is LEFT OUT, whole: the child is started without the
	// variable or the request without the header, as Kubernetes starts a
	// container without an `optional` secret reference. Never half a value
	// — "Bearer ${TOKEN}" with TOKEN unset would be "Bearer ", a credential
	// that looks set — and never an empty one.
	//
	// BY KEY AND NOT BY VARIABLE, because a key is what the server reads:
	// seats point one key at variables of their own (${ALICE_X},
	// ${BOB_X}), and one variable can sit under a key that may go without it
	// and another that may not. And not as shell syntax (`${VAR:-}`): the
	// reference grammar deliberately leaves that alone, so a setup step's
	// script survives resolution untouched (internal/envref).
	//
	// A VARIABLE A PROVISIONING PASS MINTS for the seat is waited for
	// whatever this list says: the declaration is for a value nothing will
	// supply, and that one is coming. Started without it, the seat's child
	// would act as nobody until the seal — the failure the rule exists to
	// stop. The url has no key and is never optional: a server is dialled
	// at it.
	OptionalKeys []string `yaml:"optional_keys,omitempty" json:"optional_keys,omitempty" desc:"Keys of env, headers or a seat's mcp_env block for this server whose value may resolve to nothing; such a key is left out rather than holding the server back."`

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
	// session) and the handshake, and separately the first tool listing, so
	// a start can take up to twice it before the server is given up on.
	//
	// A server that never speaks is the failure this bounds: nothing
	// raises, so without a deadline the per-server error handling never
	// runs and one silent server holds up every seat behind it. The
	// default suits a package manager fetching a server that is not yet
	// cached, which is the slow case for a HEALTHY server.
	StartupTimeoutSeconds float64 `yaml:"startup_timeout_seconds,omitempty" json:"startup_timeout_seconds,omitempty" js:"min=0" desc:"Bounds connect and handshake, and separately the first tool listing. 0 is the engine's default."`

	// RequestTimeoutSeconds bounds any one tool call.
	//
	// Raise it for a server whose tools genuinely run long; lower it for
	// one that should always answer quickly, so a wedged call surfaces as
	// a failed tool result the agent can react to rather than a turn that
	// never ends.
	RequestTimeoutSeconds float64 `yaml:"request_timeout_seconds,omitempty" json:"request_timeout_seconds,omitempty" js:"min=0" desc:"Cap on one tool call."`
}

// IdentityKey is the server's name — the key seats declare credentials under,
// so its own `env` belongs to it rather than to its slot.
func (m MCPServer) IdentityKey() string { return m.Name }

// IsShared reports whether one instance serves the company, applying the
// true default.
func (m *MCPServer) IsShared() bool { return m.Shared.Or(true) }

// IsEnabled reports whether the server runs at all, applying the true
// default.
//
// OFF IS NOT REMOVED. A removal has to clear every block that names the
// server first (a seat's mcp_env, a box's listing), which loses the
// credentials and the scoping somebody wrote for it; off keeps all of that and
// costs the server alone. So everything that DECLARES the server reads it as
// ever (its mcp_env blocks name a declared server and are held to every rule
// a running server's are), and everything that RUNS it reads this: nothing
// starts it, [MCPServer.Grants] grants it to nobody, and no executor box is
// given it.
func (m *MCPServer) IsEnabled() bool { return m.Enabled.Or(true) }

// MaxMCPDescription is the most characters a server's description may hold.
//
// ONE LINE OF THE EXECUTOR'S PROMPT: the description is listed beside the
// server's name on every turn of every agent the server reaches, where it is
// paid for on each request and read beside every other server's. 200
// characters is two sentences, enough to say what a server reaches and when
// to look in it, and short enough that a company with a dozen servers keeps
// that list to a screen. Counted in characters (Unicode code points), the
// unit the schema's maxLength counts in, so an editor and this validator
// agree on a description in any script.
const MaxMCPDescription = 200

// Grants reports whether seat's turns are offered this server's tools.
//
// A server turned off ([MCPServer.IsEnabled]) is granted to nobody. Otherwise
// a shared server serves every AGENT seat; a per-seat template serves only a
// seat that declares credentials for it under mcp_env — its own or its unit's,
// which is why this takes the seat AS THE ENGINE RUNS IT ([org.Organization]
// as the engine built it, where a unit's credentials are already layered under
// its agent members), rather than the authored role — because a template with nobody's identity in it is a server
// nobody can act through. A human seat runs no tools at all.
//
// THE ONE RULE, read by the engine when it starts a seat's children and by the
// org projection when it tells a reader which servers a seat can reach, so the
// two cannot answer differently. What it describes is the GRANT: whether the
// server actually started is the heartbeat's report, not this.
func (m *MCPServer) Grants(seat *org.Role) bool {
	if seat == nil || !seat.IsAgent() || !m.IsEnabled() {
		return false
	}
	if m.IsShared() {
		return true
	}
	_, declared := seat.MCPEnv[m.Name]
	return declared
}

// OptionalKey reports whether key's value may resolve to nothing, the key then
// being left out rather than holding the server back — see [MCPServer.OptionalKeys].
// Matched as [MCPServer.ValueKey] tells keys apart, so `authorization` declares
// an http server's `Authorization` header optional.
func (m *MCPServer) OptionalKey(key string) bool {
	return slices.ContainsFunc(m.OptionalKeys, func(listed string) bool {
		return m.ValueKey(listed) == m.ValueKey(key)
	})
}

// ValueKey is the identity of key among the values this server is started
// with: for an http server, the header's canonical spelling, and for a stdio
// server the variable's own name.
//
// HTTP NAMES A HEADER IN ANY CASE, so `authorization` and `Authorization` are
// one header, sent once. Compared as written, a seat's `authorization` did not
// replace its template's `Authorization`: both were kept, the connection set
// whichever came last out of a map on each request, and the seat's child sent
// the template's identity on some of them — or, with the seat's key optional
// and unset, on all of them. An environment does tell `token` from `TOKEN`,
// so a stdio server's keys stay as written.
func (m *MCPServer) ValueKey(key string) string {
	if m.Kind() == TransportHTTP {
		return textproto.CanonicalMIMEHeaderKey(key)
	}
	return key
}

// Kind is the transport, applying the stdio default.
func (m *MCPServer) Kind() MCPTransport {
	if m.Transport == "" {
		return TransportStdio
	}
	return m.Transport
}

func (m *MCPServer) validate(path Path) error {
	var p problems
	if strings.TrimSpace(m.Name) == "" {
		p.add(at(path, "name"), ErrMissing,
			"a server needs a name: it is the key seats declare credentials "+
				"under and how its tools are labelled in every prompt")
	}
	m.validateDescription(&p, at(path, "description"))
	if m.Transport != "" && !slices.Contains(MCPTransports, m.Transport) {
		p.add(at(path, "transport"), ErrUnknownValue, "%q (want %s)",
			m.Transport, words.Or(MCPTransports))
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
		// Unchecked, an http entry with no url parses cleanly and
		// produces a server that can never connect — with the seat's
		// tools simply absent from its prompt.
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

	m.validateSeatCredentials(&p, at(path, "seat_credentials"))

	// NO NUMBER IN EITHER MESSAGE. The defaults are the MCP client's
	// (mcp.DefaultStartupTimeout, mcp.DefaultRequestTimeout), applied where
	// a zero reaches it, and this package sits below that one; a copy kept
	// here to quote had already drifted to half the real startup default.
	if m.StartupTimeoutSeconds < 0 {
		p.add(at(path, "startup_timeout_seconds"), ErrOutOfRange,
			"must be 0 (the default) or positive, got %v", m.StartupTimeoutSeconds)
	}
	if m.RequestTimeoutSeconds < 0 {
		p.add(at(path, "request_timeout_seconds"), ErrOutOfRange,
			"must be 0 (the default) or positive, got %v", m.RequestTimeoutSeconds)
	}
	return p.err()
}

// validateDescription holds the description to one line of the executor's
// prompt ([MaxMCPDescription]).
//
// A LINE BREAK IS REFUSED, not folded: the prompt lists one server per line,
// so a description carrying a newline would start a line of its own that reads
// as something else in the list, and a tab or another control character is
// nothing a reader of the list can see. Unicode's own line and paragraph
// separators (U+2028, U+2029) are line breaks too, to every reader that
// follows the standard, though no control character. Refused rather than
// rewritten, because a description the engine silently reflowed is not the
// one the operator reads back from the configuration.
func (m *MCPServer) validateDescription(p *problems, path Path) {
	if i := strings.IndexFunc(m.Description, breaksALine); i >= 0 {
		control, _ := utf8.DecodeRuneInString(m.Description[i:])
		p.add(path, ErrUnknownValue,
			"holds %q at character %d: a description is one line beside the server's "+
				"name in each agent's prompt, so write it without line breaks or tabs",
			control, utf8.RuneCountInString(m.Description[:i])+1)
	}
	if n := utf8.RuneCountInString(m.Description); n > MaxMCPDescription {
		p.add(path, ErrOutOfRange,
			"is %d characters, and a description is at most %d: it is one line "+
				"beside the server's name in each agent's prompt", n, MaxMCPDescription)
	}
}

// breaksALine reports whether r is a character a one-line description may not
// hold ([MCPServer.validateDescription]): a control character, or a line or
// paragraph separator.
func breaksALine(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp)
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
		return nodeFault(node, "tool annotations must be a mapping of hints")
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
