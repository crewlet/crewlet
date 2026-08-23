# d-601 — MCP: the tri-state must survive the SDK, and the child is a tree

Status: decided · Applies to: `go/internal/mcp`
Related: `000` (rewrite, not transliteration), `docs/concepts/tool-capabilities.md`
Spec: `src/crewlet/mcp/`, `src/crewlet/tools/capabilities.py`

Two decisions in this package are non-obvious enough that a future reader will
be tempted to delete them. Both are load-bearing.

---

## 1. The annotation wire probe

### What the official Go SDK does

`mcp.ToolAnnotations` declares two of the four behavioural hints as **plain
bools**:

```go
type ToolAnnotations struct {
    DestructiveHint *bool   // absence survives
    IdempotentHint  bool    // absence does NOT
    OpenWorldHint   *bool   // absence survives
    ReadOnlyHint    bool    // absence does NOT
    Title           string
}
```

For `readOnlyHint` and `idempotentHint`, "the server did not say" and "the
server said false" decode to the same value, and the round trip cannot restore
it either — `MarshalJSON` emits both unconditionally.

### Why that is not cosmetic

Crewlet's whole tool-capability model is tri-state, and
`WritesToSharedSurface` — the sub-agent guard — depends on the distinction:

| annotations on the wire | correct reading | naive SDK reading | guard says |
|---|---|---|---|
| *(none at all)* | all unknown | `ReadOnly = false` | **write** (wrong) |
| `{"title":"x"}` | title only | `ReadOnly = false` | **write** (wrong) |
| `{"readOnlyHint":false,"openWorldHint":true}` | not read-only, open world | same | write (right) |

So reading the SDK struct naively flips the guard for **every under-annotated
tool in the company** — which is most of them, and precisely the population the
Python engine deliberately admits. A guard that denies everything it was told
nothing about is not conservative, it is broken: it would cut sub-agents off
from the tools their parent legitimately holds.

### What we do

A `Transport` decorator (`probe.go`) watches `tools/list` request ids out and
matches the results back, decoding the RAW `annotations` object per tool before
the SDK's typed decode discards the distinction. Everything else on the
connection passes through untouched.

### What wrapping costs, and how each cost is paid

The SDK probes its own connections for an **unexported** `clientConnection`
interface, which a wrapper cannot implement. The one hook lost is
`sessionUpdated`, and it does two things for the streamable HTTP transport:

- **opens the standalone SSE stream.** Paid by setting
  `DisableStandaloneSSE: true` explicitly. The stream carries server-initiated
  list-changed notifications and this engine registers no handler for any of
  them, so it carried nothing anyone read. Leaving it nominally enabled while
  the hook is gone would be a lie about what the connection does.
- **remembers the negotiated version for `Mcp-Protocol-Version`.** Paid by
  restoring it from `session.InitializeResult()` in the round tripper. Without
  it, a legacy remote server that enforces the header answers 400 for a reason
  nothing in the engine could explain.

The stdio transport does not implement the interface at all, so it loses
nothing.

### The fallback, and why it fails the way it does

If the probe has no record for a tool the server returned — the wire shape
moved under it — `annotationsFromSDK` trusts only an explicit `true` and
reports everything else `Unknown`. That degrades to the PYTHON engine's
behaviour rather than to a new one.

`TestSDKAnnotationsCannotHoldTheTriState` is the reason probe: it fails loudly
if the SDK ever starts carrying absence, so the next reader is told the
justification has changed instead of discovering it by deleting the mechanism.

---

## 2. A stdio server is a process TREE

An MCP server is very often launched through a package runner — `uvx
mcp-atlassian`, `npx @some/mcp` — which forks the real server underneath
itself. The SDK's transport shutdown signals only the process it started, so
the runner's child can outlive its parent, keep the inherited stderr
descriptor open, and go on holding whatever the server held.

So the child is started with `Setpgid`, and the group is signalled at teardown.

### The group is signalled only on EVIDENCE

Signalling a group whose leader has already been reaped is a theoretical route
to a recycled pid. Two facts count as evidence, and both are owned by this
package rather than inferred:

- the stderr pipe has not reached EOF, so some process still holds the
  descriptor handed to the child; or
- the transport's own close did not finish inside the caller's budget, so
  nothing reaped anything.

`TestAnOrdinaryStopDoesNotSignalTheGroup` is the control that keeps this a
gate rather than an unconditional kill.

### What it does not catch

A descendant that closes the inherited stderr and then goes on living, after
the transport has reaped the leader. Nothing available here can see it: there
is no descriptor left to observe and no pid we are entitled to signal. Stated
in the code rather than papered over, because a check that looks like it covers
that case is worse than none.

### The stderr pipe is a real `*os.File`, deliberately

Handing `exec.Cmd.Stderr` any other `io.Writer` makes `exec` create its own
pipe **and a copying goroutine that `Wait` blocks on** — so a grandchild
holding the descriptor would wedge the child's reaping inside the SDK. With a
file, `exec` hands the descriptor straight to the child and this package owns
both ends: it closes the parent's write end immediately after connect (so EOF
is reachable at all), pumps the read end into bounded per-server tail, and
force-closes the read end as a last resort so no goroutine outlives the server.

---

## 3. Two smaller calls, recorded because they are choices

- **Stop and Restart RETURN the catalogue diff.** The doomed tool names must be
  captured before anything stops, because stopping drops the bridge's own
  index. In Python that ordering was the caller's job to remember, in two
  places, and the one that forgot left a removed server's tools in every later
  turn's catalogue. Making the bridge compute the diff means a caller cannot
  ask too late and cannot forget to ask.
- **Add REFUSES a name it already runs.** The Python bridge indexed the new
  client over the old one, leaking the first subprocess for the life of the
  engine. Replacing a server is `Restart`, which says so in its name.
