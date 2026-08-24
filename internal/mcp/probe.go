package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// methodListTools is the only JSON-RPC method that carries tool annotations.
// The SEP-2575 server/discover handshake returns capabilities and server info
// and no tools, so this is the whole surface the probe watches.
const methodListTools = "tools/list"

// hintTable holds the annotations recovered from the wire, keyed by the
// server's own tool name (never the prefixed catalogue name — the prefix is
// applied on this side and the server has never heard of it).
type hintTable struct {
	mu     sync.Mutex
	byTool map[string]Annotations
}

func newHintTable() *hintTable { return &hintTable{byTool: map[string]Annotations{}} }

func (h *hintTable) record(name string, a Annotations) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.byTool[name] = a
}

func (h *hintTable) lookup(name string) (Annotations, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.byTool[name]
	return a, ok
}

// annotationsFromSDK is the DEGRADED read, used only when the probe has no
// record for a tool the server returned.
//
// The SDK's ToolAnnotations declares DestructiveHint and OpenWorldHint as
// *bool — absence survives — but ReadOnlyHint and IdempotentHint as plain
// bool. For those two, "the server did not say" and "the server said false"
// arrive as the same value, and the round trip cannot restore it either: the
// type's MarshalJSON emits both unconditionally.
//
// That flattening is not cosmetic. Read naively, an UNANNOTATED tool decodes
// as ReadOnly=false with OpenWorld unset, which is exactly the shape
// WritesToSharedSurface classifies as a write to a shared surface — so the
// sub-agent guard would deny every under-annotated tool in the company, having
// been told nothing at all. Hence the wire probe; hence this fallback trusts
// only an explicit true and reports everything else Unknown, which fails the
// same way the Python engine did rather than a new way.
func annotationsFromSDK(a *sdk.ToolAnnotations) Annotations {
	if a == nil {
		return Annotations{}
	}
	out := Annotations{Title: a.Title}
	if a.ReadOnlyHint {
		out.ReadOnly = Yes
	}
	if a.IdempotentHint {
		out.Idempotent = Yes
	}
	if a.DestructiveHint != nil {
		out.Destructive = hintOf(*a.DestructiveHint)
	}
	if a.OpenWorldHint != nil {
		out.OpenWorld = hintOf(*a.OpenWorldHint)
	}
	return out
}

// probeTransport wraps a Transport so every tools/list RESULT can be read as
// the server serialized it, before the SDK decodes it into a lossy struct.
//
// This is a deliberate reach past the SDK's typed surface, and it is confined
// to exactly one question: which annotation keys were present. Everything else
// on the connection passes through untouched.
//
// What wrapping COSTS, stated because it is invisible otherwise: the SDK
// probes its own connections for an unexported clientConnection interface, and
// a wrapper cannot implement an unexported method. The one hook lost is
// sessionUpdated, which does two things for the streamable HTTP transport —
// opens the standalone SSE stream, and remembers the negotiated protocol
// version for the Mcp-Protocol-Version header. Both are handled explicitly
// where the HTTP transport is built (see newHTTPTransport); the stdio
// transport does not implement the interface at all, so it loses nothing.
type probeTransport struct {
	inner sdk.Transport
	hints *hintTable
	log   *slog.Logger
}

func (p *probeTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	conn, err := p.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &probeConn{
		Connection: conn,
		hints:      p.hints,
		log:        p.log,
		pending:    map[any]struct{}{},
	}, nil
}

// probeConn watches request ids out and results back.
//
// Matching on the id rather than sniffing every result for something
// tools/list-shaped is the difference between reading one method's replies and
// guessing at the whole protocol.
type probeConn struct {
	sdk.Connection

	hints *hintTable
	log   *slog.Logger

	mu      sync.Mutex
	pending map[any]struct{}
}

func (c *probeConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if req, ok := msg.(*jsonrpc.Request); ok && req.Method == methodListTools && req.ID.IsValid() {
		c.mu.Lock()
		c.pending[req.ID.Raw()] = struct{}{}
		c.mu.Unlock()
	}
	return c.Connection.Write(ctx, msg)
}

func (c *probeConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if err != nil {
		return msg, err
	}
	resp, ok := msg.(*jsonrpc.Response)
	if !ok || !resp.ID.IsValid() {
		return msg, nil
	}
	c.mu.Lock()
	_, wanted := c.pending[resp.ID.Raw()]
	delete(c.pending, resp.ID.Raw())
	c.mu.Unlock()
	if wanted && resp.Error == nil {
		c.recordListing(resp.Result)
	}
	return msg, nil
}

// recordListing pulls the raw annotations object out of a tools/list result.
//
// A malformed result is not this probe's problem to report: the SDK is about
// to decode the same bytes and will fail the call properly. Logging it at
// debug keeps the probe from inventing a second, differently-worded failure
// for one wire error.
func (c *probeConn) recordListing(result json.RawMessage) {
	var payload struct {
		Tools []struct {
			Name        string          `json:"name"`
			Annotations json.RawMessage `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		c.log.Debug("annotation_probe_undecodable", "error", err.Error())
		return
	}
	for _, t := range payload.Tools {
		if t.Name == "" {
			continue
		}
		ann, err := annotationsFromJSON(t.Annotations)
		if err != nil {
			c.log.Debug("annotation_probe_undecodable", "tool", t.Name, "error", err.Error())
			continue
		}
		c.hints.record(t.Name, ann)
	}
}
