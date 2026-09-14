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

## Creating the company

On an engine with no active configuration the lens opens on a form: the
company's name and mission, a shape to start from, and optionally a seat for
yourself.

| Start from | What it writes |
|---|---|
| New company | A chief executive and three units (Engineering, Product, Marketing), each with one agent seat reporting to the chief executive |
| Established company | A chief executive, Engineering with a Reliability team, Product with a Design team, and Go to Market, with each unit's lead reporting up the chain |
| Start empty | The charter alone |

The Established company template asks whether unit leads are **agents** (seats
the engine runs) or **people** (human seats). A template never invents a
contact identity, so human leads are created without one and the review lists
each seat that still needs one; the only identity written is the one you type
for your own seat.

Everything the template writes is one change: undo takes you back to the form.
From there the organization is edited like any other, and **Review and save**
creates the company with `PUT /config` and `If-None-Match: *`, which is
refused if a company exists anywhere in the fleet. If one was created while
you were writing yours, the builder says so and offers to discard your draft
and open the company: a draft that starts a company is never applied to one
that exists, and never replayed onto it.

After a successful create, two steps remain that the dashboard cannot take:
connecting chat and trackers on the Integrations screen, and adding a model
provider, which no dashboard screen writes. The panel gives the exact
`crewlet config import` and `PATCH /config` commands for it.

## Views and the check

The toolbar switches between the **Canvas** (a chart with a **Structure** and
a **Reporting** view) and the **Outline**. Below 860 pixels wide the lens
opens on the outline. The view, the chart and the selected unit or seat are
in the URL, so a link opens the builder where it was.

Selecting a unit or a seat names it in the URL (`unit=` and `seat=`), and the
toolbar carries that node's own actions, the same ones in the same order as
its card, so every one of them is reachable from the keyboard. **Open seat**
is offered only for a seat the saved company has: a seat added in the draft
has no screen until it is saved. A rename rewrites the name in the URL rather than leaving a
link pointing at something that no longer exists. Selecting the company itself
carries the charter's **Edit** and the same **Add** menu; it names no filter,
because the lens is already about that company. Where the builder cannot
write (a guarded or read-only posture, or a draft waiting to be updated) the
actions stay in the menu and are marked unavailable, so what the builder does
is still legible.

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
| Needs an operator token | the engine asks for a token and none is set; set one to continue |

A change is checked about a third of a second after it is made, so a burst
of changes (a held key, several undos) is checked once.

When the company has no model provider configured, a caution says so: no
agent seat can run without one, and the dashboard does not write providers.
Add one with `crewlet config import` or `PATCH /config`
([Configure via the API](configure-via-api.md)).

## Reviewing and saving

**Review and save** opens once the draft has changes, and says what the save
would do before it does it:

- **What changes:** the units and seats added, removed, renamed, moved and
  edited, and the charter fields.
- **What follows:** the consequences the engine attaches to those changes,
  computed from the hierarchy it derives for the base and for the draft:
  seats that onboard again (and why), handle changes, reporting lines and
  leads that move (including a reorder that only changes which manager comes
  first), channels, where unrouted Jira and Confluence work goes, the tool
  credentials a seat gains or loses (names only), fields a kind change
  removes, references a removal clears, the Datadog fallback seat and GitLab
  access levels, and a new seat that takes a removed seat's handle and with it
  its memory.
- **Warnings** the engine gave for exactly this save.

A company rename, a handle change, a kind change, a change of tool
credentials and removing more than half of the seats each need an
acknowledgement before **Save** enables. While the engine reports problems
the review opens so you can read it, with Save disabled until they are fixed.
While a check is out, Save waits for it. If the engine could not be reached to
check, saving is still allowed: the write itself is validated.

The **audit summary** is prefilled from the changes and recorded with the
revision, followed by a write id such as
`(write 4f1c9a0b2e7d4c81a3b5f6e7d8c9b0a1)`. A save is the
same request as its checks: a `PATCH /config` merge patch of what changed,
with `If-Match` naming the revision the draft was started from, so a newer
revision is refused (and offered as an update) rather than overwritten.

### When the answer does not arrive

A save whose answer is lost (a dropped connection, a gateway timeout) may
still have been stored. The builder never assumes either way: it reads the
revision history and treats the save as done when it finds a revision whose
parent is the draft's base and whose summary carries the write id. If the
save did not land, it says so and offers to save again. If the engine cannot
be asked, editing pauses and the review offers **Check again** and **Save
again**; a second save carries the same write id, so a first save that did
land is recognized as yours rather than replayed on top of itself.

### After saving

A save stores and activates a revision. It does not apply it: every node
applies on its own reconcile tick (about every fifteen seconds, spread a
little per node so a fleet does not apply at once), and a node can refuse a
revision and go on serving the previous one. Until this node has
applied it, the Chart, Directory and Charter lenses still draw the previous
organization, and say so.

The strip under the toolbar follows the revision:

| It reads | Meaning |
|---|---|
| Saved revision `<id>`. The engine is applying it. | stored and activated; no node has reported this epoch yet |
| Applied on N of M nodes. | the fleet is converging |
| Applied. | every node reported this epoch |
| Node `<id>` refused this revision: `<reason>`. | that node kept the previous epoch. **Open the fleet** for the rest |

Beside it: **View changes**, which opens what the save changed on the
Configuration screen (the saved revision against the one the draft was
started from; after creating the company it is **View the configuration**,
since a first revision has nothing to differ from), and **Copy as YAML**,
which reads the active company as YAML (credentials redacted, as every
configuration read is) so a `company.yaml` kept in a repository can be
brought back in step. `crewlet config import company.yaml` writes it back.

### Keeping a company file in sync

A company kept as `company.yaml` in a repository and a company edited in the
builder are two writers of one document, and the flag a node starts with
decides which one wins a restart
([Configuration](../concepts/configuration.md)):

- `crewlet run -company company.yaml` only fills an empty store. Once a
  company exists the file is ignored, so a restart never undoes a builder
  save.
- `crewlet run -import-company company.yaml` makes the file the company again
  at every start. A restart with it replaces whatever the builder saved since
  the file last changed.

So after a save, **Copy as YAML** and commit it to the file, and the next
import writes back what the builder wrote rather than undoing it. Credentials
in the copy read as `__redacted__` (a whole `${NAME}` reference is shown as it
is). Importing the file over the running company restores each masked value
from the active revision by the identity of what holds it (a seat by its
handle, a unit by its name); a store with no active revision has nothing to
restore from, which is one more reason a file kept in a repository should
hold `${NAME}` references rather than values.

## A draft survives a reload

The builder keeps the draft's list of changes (never the document itself,
which holds contact identities and policies) in the tab's session storage, so
a reload or a trip to another screen does not lose the work. When the builder
opens and finds a kept draft:

- **Made against the revision that is still active:** a banner offers **Keep
  the draft** or **Discard it**, and nothing can be edited until you choose.
  Coming back to the lens from another lens of the same page restores the
  draft without asking.
- **Made against an older revision:** the draft is restored through the same
  update described below, and **Discard the kept draft** removes it instead.
- **Made for creating a company, where a company now exists** (or the other
  way around): it is discarded, and the lens says so.

The kept draft is removed when you save, when you discard, when the operator
token changes, and when the engine refuses the token, because each of those
may mean the tab has changed hands. A browser that refuses session storage (a
private window, blocked site data) shows a caution: editing works, but the
draft will not survive a reload. A draft of more than 500 changes is not kept
either; save it in steps.

## When somebody else saves first

Every check is conditional on the revision the draft was started from, so a
revision saved by somebody else (another operator, `crewlet config import`, a
setup flow on the Integrations screen) is found at the next check, not at the
save. The status reads "The configuration changed", editing pauses, and a
banner offers **Show what changed** (the newer revision against the one the
draft was started from) and **Update my draft**. A lens with no changes on it
has nothing to update: it simply reads the newer revision and goes on.

Updating replays each change of the draft onto the revision the engine holds
now, and sorts every change into one of three outcomes:

- **Still applies:** replayed as it was made.
- **Dropped:** what it changed no longer exists (the seat was removed, the unit
  it moved into is gone), with the reason.
- **Changed by somebody else as well:** a value it recorded was changed in the
  newer revision. The dialog shows the value when you started, the value saved
  now and yours, and you choose **Keep mine** or **Keep theirs** for each. A
  change is never replayed over somebody else's value without that choice.
  Where a change is about a whole seat or unit (a removal) the dialog names
  the fields somebody changed, a position reads as where the node sits, and a
  kind change lists the fields it would remove by name; a credential is never
  shown, only that a literal value is set.

"The same seat" means what it means to the engine: a seat is its handle and a
unit is its name, because those are what its memory, mailbox, schedules and
credentials attach to. A seat removed and created again with a different
handle is a different seat, and a change made to the original is dropped
rather than applied to it. One created again under the same handle, or a unit
under the same name, is the same one to the engine, so a change made to it is
held to the values it recorded like any other: a field somebody set
differently is a conflict for you to decide.

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
keys undo typing. Each change is announced to screen readers (an undo or a
redo says it undid or redid the change), and focus moves to the unit or seat
it touched. **Discard changes** throws the whole draft
away after a confirmation; the saved configuration is not touched.

At narrow widths Undo, Redo, Discard changes, Expand all and Collapse all move
into the **More** menu; the check status stays visible.

**Fullscreen**, where the browser offers it, takes the whole builder
(toolbar, chart, editor and dialogs) rather than the chart alone.
