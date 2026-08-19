# Extension System

Extensions are Python packages that plug into the engine via well-defined lifecycle hooks. They get access to the engine's subsystems (event queue, tool registry, storage, …) through an `ExtensionContext`, so additions like metrics exporters or custom chat bridges can be built without modifying the engine.

---

## Extension Protocol

```python
class Extension(Protocol):
    @property
    def name(self) -> str: ...
    @property
    def version(self) -> str: ...

    async def on_register(self, ctx: ExtensionContext) -> None:
        """Called when the extension is registered. Use to subscribe to
        events, register custom tools, and wire up subsystem access."""
        ...

    async def on_engine_start(self, ctx: ExtensionContext) -> None: ...
    async def on_engine_stop(self, ctx: ExtensionContext) -> None: ...
```

A failing `on_register` aborts that extension's registration; a failing `on_engine_start` / `on_engine_stop` is logged and the remaining extensions still start/stop.

---

## ExtensionContext

Every hook receives the same context object exposing the engine's subsystems:

| Field | What it gives you |
|---|---|
| `ctx.event_queue` | Publish/subscribe engine events (the full [EventQueue](../concepts/event-system.md) surface) |
| `ctx.agent_pool` | Live agent instances **on this node** — see [The agent pool is per-node](#the-agent-pool-is-per-node) |
| `ctx.execution_tracker` | Agent ↔ issue mappings and the dependency graph |
| `ctx.tool_registry` | Register custom agent tools (`ctx.tool_registry.register(tool)`) |
| `ctx.role_mcp_tools` | Per-role MCP tool maps |
| `ctx.storage` | Persist arbitrary data via the storage backend |
| `ctx.notification_service` | Send outbound notifications |
| `ctx.org` | The in-memory `Organization` model |
| `ctx.observability` | Token/turn metrics (`ObservabilityManager`) |
| `ctx.debug` | Engine debug flag |

---

## The agent pool is per-node

`ctx.agent_pool` holds the agents **this process is running**, which is not the
same as the agents the company has. An engine spawns an `AgentInstance` only for
the seats whose lease it holds (see
[Seat ownership](../concepts/seat-ownership.md)), so in a fleet each node's pool
is a slice of the org, and at `on_engine_start` it is typically **empty** — seats
are claimed after the subsystems come up.

Two rules follow, and both matter even on a single node, because a single node is
just a fleet of one that may not stay that way:

- **Never resolve a recipient through the pool.** Address a seat by handle. Its
  inbox topic and its agent id are both derivable from the org, which every node
  has in full: `crewlet.queue.topics.agent_inbox_topic(handle)` for the topic,
  and `ctx.org.agent_seat_by_handle(handle)` + `ctx.org.agent_id_for(seat)` for
  the id. A pool lookup answers "is this agent here?", and a miss means "not on
  this node" — not "does not exist".
- **Never treat pool membership as company membership.** `ctx.org` is the
  company; the pool is this process's share of it. Iterating the pool to count
  seats, build a colleague list, or decide whether a role exists gives an answer
  that changes with placement.

Use the pool for what it is for: inspecting or acting on a turn that is running
*here* — state, current task, in-flight work.

---

## Writing an Extension

See `src/crewlet/extensions/template.py` for a documented starting point — copy the class, implement the hooks you need, and either pass an instance to the engine or declare the module in config.

### YAML Configuration

Each entry under `extensions:` maps an importable module name to that extension's settings. The loader imports the module and instantiates `module.Extension(**settings)` (or calls `module.create_extension(**settings)`):

```yaml
extensions:
  - my_metrics_extension:
      export: prometheus
  - my_chat_bridge: {}
```

Extensions can also be added/removed live via [`/config/extensions`](../reference/api-endpoints.md) — a same-name re-add triggers an `unregister` + `register` cycle for that one extension; unchanged neighbours keep their live instance.

### Programmatic Registration

```python
from crewlet import Engine

engine = Engine(
    organization=org,
    extensions=[MyMetricsExtension()],
)
```

### Custom tools

Custom tools implement the `Tool` protocol (`name`, `description`, `parameters` JSON schema, and an `async execute(params, context) -> ToolResult`). Pass them at construction or register them from an extension:

```python
from crewlet import Engine
from crewlet.tools.protocol import ToolResult

class ReviewCodeTool:
    name = "review_code"
    description = "Run static analysis on code"
    parameters = {
        "type": "object",
        "properties": {"code": {"type": "string"}},
        "required": ["code"],
    }

    async def execute(self, params, context) -> ToolResult:
        return ToolResult(output=run_linter(params["code"]))

engine = Engine(organization=org, tools=[ReviewCodeTool()])
```

### Observability hooks

The engine exposes two event-backed hooks (both async — they subscribe your callback to the relevant event topics):

```python
await engine.on_task_state_change(callback)  # task created/assigned/started/completed/failed/delegated
await engine.on_agent_spawn(callback)        # agent_spawned events
```

For anything finer-grained (per-turn progress, LLM invocations), subscribe to the event stream directly via `ctx.event_queue` — every phase completion and turn event is published there (see [Event System](../concepts/event-system.md)).
