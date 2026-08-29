package mcp

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"time"
)

// TransportKind selects how the engine reaches a server.
type TransportKind string

const (
	// TransportStdio runs the server as a child process and speaks
	// newline-delimited JSON over its stdin/stdout.
	TransportStdio TransportKind = "stdio"
	// TransportHTTP connects to a remote server over Streamable HTTP.
	TransportHTTP TransportKind = "http"
)

// ErrInvalidSpec is returned for a server description the engine cannot act
// on. Callers branch on it to tell "this config is wrong" from "this server
// would not start", which are reported to an operator differently.
var ErrInvalidSpec = errors.New("mcp: invalid server spec")

// Spec describes ONE MCP server instance: a shared server, or one seat's own
// child of a `shared: false` template.
//
// It carries the transport rather than leaving it to which method the caller
// picked, and that is deliberate. The Python bridge had add_server /
// add_http_server / restart_server / restart_http_server, and the restart pair
// is where it cost: a live edit to a shared HTTP server's url went through the
// stdio restart, which relaunched a remote connection as a subprocess with an
// empty command. With the kind ON the spec there is one Add and one Restart,
// and picking the wrong one is not a thing a caller can do.
type Spec struct {
	// Name is the instance name: the bare server name for a shared server,
	// or InstanceName(server, role) for a per-role child.
	//
	// It is the source of truth for three separate things — the client index
	// key, the per-role instance prefix, and (via ServerName) the server name
	// the model is shown and types — so an empty one would propagate silently
	// through all three. Rejected here rather than at the config layer,
	// because this is the layer that cannot recover from it.
	Name string

	// Transport defaults to TransportStdio when empty.
	Transport TransportKind

	// Command and Args launch a stdio server.
	Command string
	Args    []string

	// Env is layered OVER the engine's own environment for a stdio child.
	// Values must already have their ${VAR} references resolved: this package
	// never reads the secret store.
	Env map[string]string

	// URL and Headers reach an HTTP server. Headers carry the per-seat
	// identity for a per-role HTTP instance — an Authorization bearer for a
	// remote server, where a stdio child would get an env var.
	URL     string
	Headers map[string]string

	// ToolPrefix is prepended to every tool name this server contributes,
	// keeping two servers' identically-named tools apart in one catalogue.
	ToolPrefix string

	// ExcludeTools names tools to skip. Matched against the SERVER's own tool
	// name, before ToolPrefix is applied — an operator excluding a tool has
	// the server's listing in front of them, not the engine's catalogue.
	ExcludeTools []string

	// AnnotationOverrides are operator-supplied behavioural hints layered over
	// what the server advertises, for servers that under-annotate.
	//
	// Keyed by EITHER the server's raw tool name or the prefixed catalogue
	// name. An operator keying it by the name they see in the dashboard must
	// not have it silently do nothing.
	AnnotationOverrides map[string]Annotations

	// StartupTimeout bounds connect + handshake, and separately the first
	// tools/list. Zero means DefaultStartupTimeout.
	StartupTimeout time.Duration

	// RequestTimeout bounds one tools/call. Zero means DefaultRequestTimeout.
	RequestTimeout time.Duration
}

func (s Spec) kind() TransportKind {
	if s.Transport == "" {
		return TransportStdio
	}
	return s.Transport
}

func (s Spec) startupTimeout() time.Duration {
	if s.StartupTimeout <= 0 {
		return DefaultStartupTimeout
	}
	return s.StartupTimeout
}

func (s Spec) requestTimeout() time.Duration {
	if s.RequestTimeout <= 0 {
		return DefaultRequestTimeout
	}
	return s.RequestTimeout
}

// equal reports whether two specs describe the same running server.
//
// DEEP, and every field: an apply that changed only Env — a rotated token —
// must restart the child, because the environment is handed to it once at
// exec and there is no other way in. Compared with reflect.DeepEqual rather
// than field by field precisely so a field ADDED to Spec is covered the day
// it is added; a hand-written comparison would silently keep answering "same"
// for the one thing nobody remembered to list.
func (s Spec) equal(other Spec) bool { return reflect.DeepEqual(s, other) }

func (s Spec) validate() error {
	if s.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidSpec)
	}
	switch s.kind() {
	case TransportStdio:
		if s.Command == "" {
			return fmt.Errorf("%w: server %q: stdio needs a command", ErrInvalidSpec, s.Name)
		}
	case TransportHTTP:
		if s.URL == "" {
			return fmt.Errorf("%w: server %q: http needs a url", ErrInvalidSpec, s.Name)
		}
	default:
		return fmt.Errorf("%w: server %q: unknown transport %q", ErrInvalidSpec, s.Name, s.Transport)
	}
	return nil
}

// Server is the bare server name this instance serves, with any per-role
// suffix stripped.
func (s Spec) Server() string { return ServerName(s.Name) }

// SameProcess reports whether two specs would bring up the same connection —
// the same child with the same environment, or the same endpoint with the same
// headers.
//
// It covers BOTH transports' identity in one place. The Python check compared
// only the stdio fields, so an edit to a shared HTTP server's url or headers
// (rotating a remote token, say) matched as "unchanged" and the stale
// connection went on serving with the credential the operator had just
// revoked.
//
// It is NOT the whole question a live config diff asks — see SameCatalogue —
// and it is not the question CREDENTIAL ROTATION asks at all. On that path the
// config payload is byte-identical by construction and only the resolved
// ${VAR} values moved, so this returns true for two specs whose children hold
// different credentials. Rotation restarts unconditionally; that is why
// Restart does not consult this.
func (s Spec) SameProcess(other Spec) bool {
	return s.kind() == other.kind() &&
		s.Command == other.Command &&
		slices.Equal(s.Args, other.Args) &&
		maps.Equal(s.Env, other.Env) &&
		s.URL == other.URL &&
		maps.Equal(s.Headers, other.Headers) &&
		s.StartupTimeout == other.StartupTimeout &&
		s.RequestTimeout == other.RequestTimeout
}

// SameCatalogue reports whether two specs would put the same tools, under the
// same names and the same behavioural hints, in front of the agents.
//
// This is what a live config diff should branch on, not SameProcess. Three of
// these fields change nothing about the child and everything about the
// catalogue — the prefix renames every tool, the exclusions remove some, and
// the annotation overrides decide whether a sub-agent may call one. The Python
// diff compared only the process fields, so an operator who added
// tool_annotations to a running server got no effect at all and no way to tell
// why: the edit activated, the revision advanced, and the guard went on
// reading what the server had advertised.
func (s Spec) SameCatalogue(other Spec) bool {
	return s.SameProcess(other) &&
		s.ToolPrefix == other.ToolPrefix &&
		slices.Equal(s.ExcludeTools, other.ExcludeTools) &&
		maps.Equal(s.AnnotationOverrides, other.AnnotationOverrides)
}
