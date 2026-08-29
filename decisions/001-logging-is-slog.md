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
   those are not measurable against them.

   Measured, not asserted — `go test ./internal/logging -bench .`, and the
   benchmarks are committed beside the package so the next reader can re-run them
   rather than trust this table (Xeon @ 2.10 GHz, Go 1.27):

   | | ns/op | B/op | allocs/op |
   |---|---|---|---|
   | An emitted line, through `lazy` | 976 | 296 | 5 |
   | The same line, straight at the handler | 714 | 8 | 1 |
   | A **suppressed** debug line | 9.6 | 0 | 0 |

   A suppressed debug line already costs nothing, because `lazy.Enabled` answers
   from the root without replaying the recorded ops — so a `Debug` call in a loop
   needs no hand-written guard. That is the case that would have mattered.

### The 262 ns this design does cost, and why it stays

`lazy.resolve` rebuilds the handler chain for **every emitted record** — the gap in
the table above, ~262 ns and 4 allocations a line. It buys the guarantee this package
exists for: a `var log = logging.Get("store")` evaluated at package init still follows
a `Configure` that happens after flag parsing.

It could be cached behind a generation counter bumped by `install`, which would
recover most of it. That is deliberately **not** done, for the same reason zap is not
adopted: there is no hot path, the volume is low, and the object in question is the
process-wide logging root — where a stale cache entry after a reconfiguration is
exactly the bug `lazy` exists to prevent. Buying 262 ns with that risk would
contradict the argument three paragraphs up. Revisit it if a profile ever shows
logging in it; the benchmarks above are how you would know.

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

`install`'s switch falls through to `console` for anything it does not recognise,
which is right for a typo and wrong for a format the package advertises.
`TestEveryDeclaredFormatInstallsItsOwnHandler` asserts the closed set against the
constructor, so a fourth entry in `Formats` with no case beside it fails rather than
silently rendering as `console`.

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
The quickstart told an operator to write it and the deployment guide said it "raises
the log level to DEBUG"; it changed nothing, and a boolean nobody consults looks
exactly like a boolean that is working. Tier A now carries a real `logging:` block
(`level`, `format`).

### The `debug:` boolean was retired, not wired up

Making it work was the first move, and keeping it would have mirrored the command
line, where `-debug` is the shorthand for `-log-level debug`. It is gone instead,
and the difference between the two cases is what settles it: a **flag** is typed
for one invocation by someone watching the process, so a shorthand saves them
characters and cannot drift. A **file** is written once and read by everyone
afterwards, so two keys setting one value is a state where they disagree — and
then something has to arbitrate, and that arbitration is a rule every reader of
the file has to know before they can predict what it does. `logging.level` says
everything `debug:` said and three things it could not.

The key is not simply dropped: it is in `config.retiredFields`, so a file still
carrying it is refused with the line that replaces it rather than with "check the
spelling". This project's own quickstart and example told people to write it, and
reporting that as a typo sends them hunting for one that is not there. Entries
there are permanent — a file written against any past release stays diagnosable,
and the cost is one map entry.

The two layers disagree on purpose about a value the build does not recognise. A
**flag** resolves a typo to the default, because a bad log level must never be why a
company will not boot. The **file** refuses it, with the field path: a flag is typed by
someone watching the process start, and a file is written once and deployed for months.

Neither layer is *silent* about it. The flag and environment paths log
`log_level_unrecognised` / `log_format_unrecognised`, naming what was written, what was
used instead, and the closed set — because a soft failure nobody is told about is the
entire mechanism by which `debug: true` stayed broken. The fallback was always the
right behaviour; the silence never was.
