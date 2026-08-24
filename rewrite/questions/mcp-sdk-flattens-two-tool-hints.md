# q — the official Go MCP SDK cannot represent an unset `readOnlyHint`

Status: **OPEN — worked around in `internal/mcp/probe.go`; upstream not yet
filed** · Raised by the `mcp` port · Spec: `src/crewlet/tools/capabilities.py`,
`docs/concepts/tool-capabilities.md` · Port: `internal/mcp/`

## What

`github.com/modelcontextprotocol/go-sdk@v1.7.0` declares:

```go
type ToolAnnotations struct {
    DestructiveHint *bool   `json:"destructiveHint,omitempty"`
    IdempotentHint  bool    `json:"idempotentHint"`
    OpenWorldHint   *bool   `json:"openWorldHint,omitempty"`
    ReadOnlyHint    bool    `json:"readOnlyHint"`
    Title           string  `json:"title,omitempty"`
}
```

Two hints carry absence in a pointer; two do not. For `readOnlyHint` and
`idempotentHint`, a server that said nothing and a server that said `false`
decode identically, and re-marshalling cannot restore the difference either
(`MarshalJSON` emits both unconditionally; the `MCPGODEBUG=hintomitempty=1`
escape only affects marshalling).

## Why it matters here

Crewlet's tool-capability model is tri-state on purpose, and
`WritesToSharedSurface` — the sub-agent guard — is built on the distinction.
Read naively, an UNANNOTATED tool decodes as `ReadOnly=false` with `OpenWorld`
unset, which is exactly the shape the classifier calls a write to a shared
surface. So the guard would deny every under-annotated tool in the company,
having been told nothing at all — the opposite of the documented behaviour,
and a large behavioural regression against the Python engine, which
deliberately admits what it cannot classify.

Proven, not asserted: `TestSDKAnnotationsCannotHoldTheTriState` decodes `{}`
and `{"readOnlyHint":false,"idempotentHint":false}` and asserts they are
indistinguishable, then asserts that the naive reading of the first inverts the
guard.

## What was done

A `Transport` decorator reads the RAW `annotations` object out of each
`tools/list` result before the SDK's typed decode discards the distinction. It
is confined to that one question; everything else on the connection passes
through. The tradeoffs it carries — a lost unexported `sessionUpdated` hook,
paid for explicitly on the HTTP path — are recorded in `d-602`.

## What is being asked

1. **Should this be filed upstream?** The fix is small and source-compatible in
   one direction only (`ReadOnlyHint bool` → `*bool` is a breaking API change
   for every server author who sets it). A non-breaking alternative is an
   accessor pair plus a raw-annotations escape hatch on `Tool`. Someone with
   standing in that project should decide the shape before an issue is opened
   in Crewlet's name.
2. **If upstream fixes it, the probe should go.** It is ~120 lines whose whole
   justification is this defect, and the reason probe fails loudly when the
   defect disappears — but only if someone runs the suite against a newer SDK.
   Worth a note wherever the Go dependency bumps are reviewed.

## What is NOT being asked

Whether to soften `WritesToSharedSurface` so the flattening stops mattering.
That was considered and rejected: the classifier's polarity is one of the
invariants `d-000` says must not be modernised away, and the guard's caller
(the parent's explicit allowlist) is what makes "admit the unclassifiable"
safe in the first place.
