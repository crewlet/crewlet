# d-001 — Logging is `log/slog`, and the console encoder is ours

Status: **decided**

Applies to: `internal/logging`

## The question that keeps coming back

The engine's logging looks like Python's `logging` module — `logging.Get("task.engine")`
reads exactly like `logging.getLogger(__name__)`, and the component names are dotted
paths. That resemblance is deliberate and it is **only** in the naming: it exists so an
operator's runbooks and log queries survive the rewrite from the Python engine, which
is stated at `Get`'s doc comment. Underneath it is `log/slog` from the standard
library, which is as native as Go logging gets.

The resemblance invites a second question, and it arrives with a link to
`uber-go/zap` attached: *shouldn't this be a real logging library?* Almost always the
thing actually being asked for is one of zap's features, and almost always it is the
console encoder — because `slog.NewTextHandler` emits no colour and a booting company
is a wall of identical grey `time=… level=… msg=…` prefixes.

## The decision

**`*slog.Logger` stays the type the engine passes around, and the human-facing
encoder is a handler in this package.**

### Why not zap

1. **`*slog.Logger` is the interface, not an implementation choice.** It is what
   `net/http`'s `Server.ErrorLog` bridge, the MCP SDK, and every library written
   after Go 1.21 accepts. Adopting zap does not remove slog from the tree; it adds a
   second logger and a `zapslog` bridge at every boundary that wants the standard
   type. The engine has 63 component loggers and passes `*slog.Logger` across almost
   every package seam.

2. **Nothing here needs the performance.** zap's case is zero-allocation structured
   logging at very high rates. This engine's hot paths are network calls — an LLM
   completion, a JetStream publish, a store write. A handful of `Debug` lines around
   those are not measurable against them, and `lazy.Enabled` already answers a
   suppressed debug line without allocating.

3. **Sampling would be wrong here anyway.** zap's other headline feature drops
   duplicate lines under load. A seat's event log is an audit trail an operator
   reconstructs an incident from; dropping the duplicates is dropping the evidence
   that something happened *repeatedly*.

4. **The dependency rule.** `CLAUDE.md`: a dependency has to earn its place against
   what `std` already does. slog does levels, structure, groups, custom handlers and
   `context` plumbing. What it does not ship is a pretty console encoder — and that
   is one file, `console.go`, with no ongoing surface to track.

The same reasoning rejects `lmittmann/tint` and `charmbracelet/log`, which are
smaller than zap and would fit: neither could hoist `component` into a column, because
neither knows that every logger in this tree carries one. That column is most of what
makes the format readable, and it is the part a general-purpose library cannot supply.

### Why a third format rather than colouring `text`

`text` is slog's own `key=value`: self-describing on every field, greppable with no
parser, and something a runbook may already depend on. Colouring it in place would
have put escape codes into a format whose whole value is being machine-legible
without one.

So there are three, and each has one reader:

| Format | Reader | Shape |
|---|---|---|
| `console` (default) | A person at a terminal | Columns — time, level, component, event — attributes dimmed, colour when the sink is a live terminal |
| `text` | `grep` | slog's `key=value` |
| `json` | A log shipper | One object per line |

`console` is the default because the default reader of `crewlet run` is a person
watching it start.

### What `console` decides from its sink, and why it is not config

One probe — *is a person watching this stream right now?* — settles two things:

- **Colour.** A file, a pipe or a container's captured stream renders `\x1b[32m`
  literally, so auto-detection is off by default there. `CREWLET_LOG_COLOR=always`
  overrides it for a CI viewer that renders ANSI without being a terminal;
  `NO_COLOR` (the [no-color.org](https://no-color.org) convention) and `TERM=dumb`
  suppress it.
- **The timestamp.** A terminal gets `19:55:12.482`, because the date is today and
  the eight columns are better spent on the event. Anything else gets the full date:
  a redirected log is read *later*, and a line that says only `19:55:12` cannot be
  correlated with anything a day afterwards.

Colour is **not** a Tier A field, and that is the same call `-roles`, `-api-host` and
`$CREWLET_LOG_LEVEL` already make. The same `crewlet.yaml` is applied to a container
with no terminal and run on a laptop with one; a field that had to be edited between
those two would be describing the reader rather than the node.

## The constraint a future handler must keep

`lazy.Enabled` asks the root handler directly **without replaying** the
`WithAttrs`/`WithGroup` ops recorded on it, because replaying would allocate a handler
for every suppressed debug line. That shortcut is correct only while every handler
`Configure` can install answers `Enabled` from its level and nothing else. slog's text
and JSON handlers do; `consoleHandler` does. A handler that consulted its attributes
there would filter the wrong lines, and only the *suppressed* ones would show it.

## What this replaced

`debug: true` in Tier A was a declared field that **nothing in the tree ever read**.
The quickstart tells an operator to write it and the deployment guide said it "raises
the log level to DEBUG"; it changed nothing, and a boolean nobody consults looks
exactly like a boolean that is working. Tier A now carries a real `logging:` block
(`level`, `format`) with `debug:` as its documented shorthand — the same pairing
`-debug` and `-log-level` already had on the command line, with the same rule that the
shorthand wins.

The two layers disagree on purpose about a value the build does not recognise. A
**flag** resolves a typo to the default, because a bad log level must never be why a
company will not boot. The **file** refuses it, with the field path: a flag is typed by
someone watching the process start, and a file is written once and deployed for months.
