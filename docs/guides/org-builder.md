# The Org Builder

The dashboard's Org chart screen has a **Builder** lens
(`#/org?lens=builder`) for editing the organization of a running company, and
for creating the company on an engine that has none. It edits the same
company document `GET /config` serves and `PATCH /config` writes, so every
change it makes is an ordinary configuration revision: stored, activated,
audited, and applied by every node on its own tick.

The engine is the validator. The builder checks every draft by sending the
engine a dry run of exactly the write a save would send, and draws the
problems, warnings and derived hierarchy the engine answers with. It does not
implement any configuration rule of its own.

## Opening the builder

The configuration is guarded, reads included, so the builder needs what any
other configuration client needs: an operator token the engine accepts
(unless the node runs with `api.auth.disabled`). What the lens shows is
decided from what the engine answers, never from whether the browser holds a
token:

| The engine answers `GET /config` with | The lens shows |
|---|---|
| the active revision | the organization, ready to edit |
| `404 no_active_revision`, and the organization the node pushes names no company | creating the company |
| `404 no_active_revision`, while the organization names a company | "This node has not caught up with the fleet's configuration yet." Try again once the node has applied the fleet's revision, or use another node |
| `401` or `403` | a request for a token, with **Set token** |
| a plain `404`, or an answer that is not JSON | "This process does not serve the configuration." The process has no configuration store; open the dashboard on a node running the engine |
| nothing | the engine could not be reached, with **Retry** |

A node that has not caught up is never offered create mode: its own store is
empty while the fleet runs a company, and a create from there could only be
refused.

If the engine refuses the first dry run with `503 no_control_plane`, the lens
is **read-only**: the process can read the configuration but has no
coordination store to write it through. The status reads "Read-only here".

Changing the operator token while a draft is open keeps the draft. The
builder reads the configuration again and checks the draft under the new
token; if the engine refuses it, editing pauses until a token it accepts is
set.

## Views and the check

The toolbar switches between the **Canvas** (a chart with a **Structure** and
a **Reporting** view) and the **Outline**. Below 860 pixels wide the lens
opens on the outline. The view, the chart and the selected unit or seat are
in the URL, so a link opens the builder where it was.

The check status beside the view controls says what the engine made of the
current draft:

| Status | Meaning |
|---|---|
| Checking | a dry run of the current draft is on its way |
| No problems | the engine would accept the draft as it stands |
| *N problems* | the engine would refuse it; each problem is marked on the unit or seat it names, and problems about the whole document are listed above the chart |
| Could not reach the engine to check | the dry run got no answer; the builder retries with an increasing wait |
| Read-only here | this process cannot write the configuration |
| The configuration changed | another revision was activated after this draft was started |
| The engine refused the token | set a token the engine accepts to continue |

A change is checked about a third of a second after it is made, so a burst
of changes (a held key, several undos) is checked once.

When the company has no model provider configured, a caution says so: no
agent seat can run without one, and the dashboard does not write providers.
Add one with `crewlet config import` or `PATCH /config`
([Configure via the API](configure-via-api.md)).

## When somebody else saves first

Every check is conditional on the revision the draft was started from, so a
revision saved by somebody else (another operator, `crewlet config import`, a
setup flow on the Integrations screen) is found at the next check, not at the
save. The status reads "The configuration changed", editing pauses, and a
banner offers **Show what changed** (the revision history's diff) and **Update
my draft**.

Updating replays each change of the draft onto the revision the engine holds
now, and sorts every change into one of three outcomes:

- **Still applies:** replayed as it was made.
- **Dropped:** what it changed no longer exists (the seat was removed, the unit
  it moved into is gone), with the reason.
- **Changed by somebody else as well:** a value it recorded was changed in the
  newer revision. The dialog shows the value when you started, the value saved
  now and yours, and you choose **Keep mine** or **Keep theirs** for each. A
  change is never replayed over somebody else's value without that choice.

A seat or unit that was removed and created again under the same name is a
different entity to the builder, so a change made to the original is dropped
rather than applied to the new one.

If this node still serves the older revision (it has not applied the newer one
yet, or a load balancer sent the read to a node that has not), the update
waits: "This node has not caught up with the newer revision yet." Try again in
a moment. If the configuration the draft edits is no longer active at all, the
banner offers **Discard and reload**.

## Undo, redo and the keyboard

Every change is an operation in the draft's log. **Undo** and **Redo** in the
toolbar, or <kbd>Ctrl</kbd>+<kbd>Z</kbd> and
<kbd>Shift</kbd>+<kbd>Ctrl</kbd>+<kbd>Z</kbd> (<kbd>Command</kbd> on a Mac),
walk the log from anywhere in the builder except a text field, where the same
keys undo typing. Each change is announced to screen readers, and focus moves
to the unit or seat it touched. **Discard changes** throws the whole draft
away after a confirmation; the saved configuration is not touched.

At narrow widths Undo, Redo, Discard changes, Expand all and Collapse all move
into the **More** menu; the check status stays visible.

**Fullscreen**, where the browser offers it, takes the whole builder
(toolbar, chart, editor and dialogs) rather than the chart alone.
