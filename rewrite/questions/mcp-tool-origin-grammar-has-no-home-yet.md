# q — the tool-origin grammar lives in `internal/mcp` because there is no
# registry package yet

Status: **OPEN — parked in `internal/mcp/origin.go`** · Raised by the `mcp`
port · Spec: `src/crewlet/tools/registry.py` · Port: `internal/mcp/origin.go`

## The grammar

`builtin` | `custom` | `extension:<name>` | `mcp:<server>` — recorded at
REGISTRATION because it cannot be recovered afterwards. A tool an extension
registers is structurally identical to one the engine ships, so with nothing
recorded the operator surface called both "builtin", and a tool missing because
its extension failed to load read as a missing builtin.

## The problem

It covers four registrants and only one of them is MCP. It belongs beside the
tool registry — `internal/tools`, which does not exist in the Go tree yet. MCP
is the only producer that does exist, so the four constants are parked in
`internal/mcp/origin.go` with a note on the file saying where they go.

`internal/mcp` also defines two things a registry would otherwise own, for the
same reason:

- `Callable` — the four-method interface a phase surface needs to offer
  something to a model and run what it asked for. Both a bridged tool and a
  discovery meta-tool satisfy it.
- `Registration` — a `Callable` plus its origin. The bridge does not hand out
  a bare tool for filing; it hands out the pair, so a registry's register call
  cannot lose the answer.

## What is being asked

Whoever builds `internal/tools`: **move `origin.go` wholesale, and move
`Callable`/`Registration` with it if the registry wants them.** What must not
happen is a second copy of those four strings appearing next to the registry —
that is exactly how a grammar stops being one, and it is the failure the
grammar was introduced to end.

Related: the same package carries a guard test
(`TestInstanceSeparatorHasOneDefinition`) that fails the build on a hand-built
`server::Role` string. Its scope is `internal/mcp` ONLY, stated in the test.
When the seat cascade grows a caller that builds instance names, that guard
must be widened to the new package or the same drift becomes possible one
directory away.
