# d-502 — The dashboard wire protocol, frozen

**Status:** DECIDED (REWRITE_PLAN §10 already says frozen; this writes down
*what* is frozen, extracted from both ends).

The dashboard is the compatibility reference, not the Python engine. Its
JavaScript ships unchanged and its own suite runs under bare `node` against
vendored DOM — so that suite proves nothing about the server. **The wire
protocol is the whole contract**, and it is stated here once so a Go
implementation is checked against a written spec rather than against a reading
of `stream.py`.

Sources: `src/crewlet/static/dashboard/js/socket.js` (`_onMessage`, `query`,
`_dial`) and `src/crewlet/api/routes/stream.py` (the docstring and the receive
loop). Where they disagree, **the client wins** — it is the half that ships
unchanged.

## Transport

One WebSocket at `/ws/stream`. **A running dashboard makes no HTTP request at
all**: state comes down this socket and requests go up it. The REST snapshot
endpoint exists only for degraded mode.

Every frame is one JSON object, one per text frame.

## Handshake

- Credential is `?token=<urlencoded>` on the URL. Browsers cannot set headers on
  a `WebSocket` constructor; non-browser clients may use `Authorization`
  instead. Query strings appear in proxy logs, so the header is the better
  credential where a client can send one.
- **Rejection is `close(1008)` BEFORE `accept()`.** Accepting and then closing
  makes the browser see a connection that opened and died, which a page cannot
  tell from an engine that fell over. Closing before accept surfaces as a failed
  handshake, which is what lets the dashboard show its token gate.
- On accept, the server sends a `snapshot` immediately, built entirely from the
  in-memory projection — no database round trip on connect.

## Server → client

| kind        | shape                             | notes |
| ----------- | --------------------------------- | ----- |
| `snapshot`  | `{kind, data, ts}`                | `data` = `{health, agents, events, sandboxes, org, tools, tokens, schedules}` |
| `event`     | `{kind, data, ts}`                | one event row plus its payload |
| `agents`    | `{kind, data, ts}`                | changed agent overlays — the RESULT of applying an event |
| `seats`     | `{kind, data, ts}`                | REPLACES the roster; `agents` MERGES into it |
| `sandboxes` | `{kind, data, ts}`                | in-flight sandbox runs |
| `tokens`    | `{kind, data, ts}`                | spend rollup |
| `budget`    | `{kind, data, ts}`                | |
| `schedules` | `{kind, data, ts}`                | |
| `org`       | `{kind, data, ts}`                | |
| `tools`     | `{kind, data, ts}`                | |
| `health`    | `{kind, data, ts}`                | `{status, in_flight?, shutting_down?}` |
| `result`    | `{kind, id, what, data}`          | answer to one query |
| `error`     | `{kind, id, what, error}`         | `error` is a CODE, not prose |
| `pong`      | `{kind, data: null, ts}`          | |

`seats` replaces and `agents` merges. That asymmetry is load-bearing: a roster
change has to be able to REMOVE a seat, and a merge cannot.

The derived pushes (`agents`, `sandboxes`, `tokens`, …) carry the result of
applying an event to the server's projection. A client renders them; it does not
re-derive them from the raw `event` stream. Every tab used to do that — three
private copies of the projection, each drifting its own way.

Any kind the client does not recognise is **ignored silently** (the `switch` has
no default). New kinds are therefore additive and safe.

## Client → server

```
{"kind": "ping"}
{"kind": "query", "id": N, "what": "...", "params": {…}, "token": "…"}
```

`token` rides the frame only for the operator-only (`config*`) queries. `id` is
client-minted and correlates the `result` / `error`.

## Queries (`what`)

Public: `agent`, `agent_memory`, `event`, `events`, `trace`, `tokens`,
`schedules`, `fleet`, `sandbox_runs`, `conversations`, `budgets`,
`integrations`, `stream`.
Operator-only: `config`, `config_audit`, `config_entities`, `config_diff`.

`stream` is deliberately not called `health`: **a query must never share a name
with a push kind**, or a reader of the protocol has to know which direction a
frame was travelling to know what it means.

Error codes: `unknown_query`, `unauthorized`, `query_failed`.

## Backpressure

Per-client bounded queue, **512** envelopes, **drop-oldest**. A slow tab loses
its oldest queued envelope rather than stalling the publish path or any other
tab; beyond that the reconnect flow refetches the snapshot. The client dedupes
streamed envelopes against the snapshot by event id, so registering a client
before sending its snapshot is harmless — and is the ordering that avoids a gap.

## Health tick

ONE shared timer for the whole service, not one per connection, broadcasting a
`health` envelope on a fixed interval so the in-flight pill, drain state and live
dot stay honest with no events flowing.

## Concurrency

At most **4** queries in flight per socket. Queries run concurrently so a store
scan cannot stall the live feed, but each can take a connection from a pool the
engine shares, and an unbounded fan-out from one tab would starve the engine's
own writes. Four covers the most one screen issues at once (the agent page opens
with three) and makes a burst queue rather than pile up.
