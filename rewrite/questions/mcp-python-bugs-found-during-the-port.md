# q — three defects in the Python MCP layer, found while porting it

Status: **OPEN — fixed in the Go port, NOT fixed in `src/crewlet/`** · Raised by
the `mcp` port · Spec: `src/crewlet/mcp/bridge.py`, `src/crewlet/engine.py` ·
Port: `go/internal/mcp/`

The port brief scopes me to `go/internal/mcp/` and `rewrite/`, so these are
raised rather than fixed in the Python tree. All three are live in
`src/crewlet/` today.

---

## 1. `MCPToolBridge.add_server` orphans a subprocess on a duplicate name

`_register` ends with:

```python
self._clients[client.name] = client
```

There is no check for an existing entry. A second `add_server` with a name the
bridge already runs indexes the new client OVER the old one — the first
subprocess is still running, no longer reachable through the bridge, and
`stop_all` cannot see it. It survives until the engine process exits.

Today the engine always reaches this through `restart_server`, which stops
first, so the leak is latent rather than active. It is one careless call site
away from being real, and nothing says so.

**Go port:** `Bridge.Add` returns `ErrServerExists` rather than replacing.
Replacing is `Restart`, which says so in its name.
`TestAddRefusesADuplicateRatherThanOrphaningTheChild` plus a concurrent variant
pin it.

---

## 2. A live edit to `tool_annotations` silently does nothing

`Engine._apply_mcp_servers_live` decides whether to restart a server with:

```python
needs_restart = (
    old_cfg is None
    or old_cfg.transport != cfg.transport
    or old_cfg.command != cfg.command
    or old_cfg.args != cfg.args
    or old_cfg.env != cfg.env
    or old_cfg.url != cfg.url
    or old_cfg.headers != cfg.headers
    or old_cfg.tool_prefix != cfg.tool_prefix
)
```

`tool_annotations` is not in the list, and `exclude_tools` is not either. Both
change what reaches the agents without changing the child:

- **annotations** decide whether a sub-agent may call a tool. This is the
  operator's documented escape hatch for a server that under-annotates
  (`docs/concepts/tool-capabilities.md` § *Operator overrides*), and editing it
  on a running engine produces no effect at all — the revision activates, the
  epoch advances, and the guard goes on reading what the server advertised.
- **exclusions** decide whether a tool is in the catalogue at all.

There is no error and no log line; the operator's only signal is that nothing
changed.

**Go port:** `Spec.SameProcess` covers the child's identity and
`Spec.SameCatalogue` adds prefix, exclusions and annotations. A live diff
branches on the second. `TestSameProcessAndSameCatalogue` pins the split.

---

## 3. Two servers exposing the same tool name lose one permanently

`MCPToolBridge._tools` is a flat `dict[str, MCPToolWrapper]` and is the source
of truth. Two servers with no `tool_prefix` that both expose, say, `search`:
the second registration overwrites the first, so the index holds only the
second. Then `_remove_server_tools` filters by client name:

```python
self._tools = {k: v for k, v in self._tools.items() if v._client.name != name}
```

Stopping the second server deletes `search` outright. The first server is still
running and still serving it, and it never comes back — for the life of the
process.

**Go port:** the per-server lists are the source of truth and the flat
catalogue is DERIVED, rebuilt on every mutation, with the winner picked by
sorted server name so it does not depend on add order. A collision is logged
(`tool_name_collision`), and stopping the winner puts the loser back — reported
in the same `Change` the caller registers from.
`TestCollidingToolNamesResolveDeterministically` pins both halves.

---

## What is being asked

Whether these should be fixed in `src/crewlet/` now, or left to be retired
with it. (2) is the one an operator can hit today without doing anything
unusual, and it fails silently, which is the combination that makes it worth
fixing ahead of the rewrite.
