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

Every change is made to a draft in the browser. Nothing reaches the engine until you review the draft and save it, and the builder keeps every field of the document it does not show. What it shows and what it can change is listed under [Editing a node](#editing-a-node), field by field.

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
you were writing yours (the builder hears of it as soon as the node reports
the new organization, not only when you save), the builder says so and
offers to discard your draft and open the company: a draft that starts a
company is never applied to one that exists, and never replayed onto it.
**Keep my draft** leaves it on screen to read, read-only, with the same offer
beside it.

After a successful create, two steps remain that the dashboard cannot take:
connecting chat and trackers on the Integrations screen, and adding a model
provider, which no dashboard screen writes. Until the provider is added every
node refuses to apply the new company (the strip after the save says so), and
the panel gives the exact `crewlet config import` and `PATCH /config` commands
for it.

## Views and the check

The toolbar switches between the **Canvas** (a chart with a **Structure** and
a **Reporting** view) and the **Outline**. Below 860 pixels wide the lens
opens on the outline. The view, the chart and the selected unit or seat are
in the URL, so a link opens the builder where it was.

Selecting a unit or a seat names it in the URL (`unit=` and `seat=`), and the
toolbar carries that node's own actions, the same ones in the same order as
its card, so every one of them is reachable from the keyboard. **Open seat**
is offered only for a seat the saved company has: a seat added in the draft
has no screen until it is saved. A rename rewrites the name in the URL rather
than leaving a link pointing at something that no longer exists. Selecting
the company itself carries the charter's **Edit** and the same **Add** menu;
it names no filter, because the lens is already about that company. Where the
builder cannot write (a guarded or read-only posture, or a draft waiting to be
updated) the actions stay in the menu and are marked unavailable, so what the
builder does is still legible.

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

When the company has no model provider configured, a caution says so. The
engine builds the models for every apply and refuses a company with none, so
until a provider exists every node keeps the revision it had and nothing in
this company runs; the dashboard does not write providers. Add one with
`crewlet config import` or `PATCH /config`
([Configure via the API](configure-via-api.md)).

## Adding a unit or a seat

**Add unit**, **Add agent seat** and **Add human seat** on the company or a unit
open the Add dialog, which adds the new node at the end of that unit (or at the
top level of the company). Seat names are unique, and so are unit names,
because a lead, a unit reference and a `manages` entry each name exactly one.
The dialog starts with a name nobody holds, and when you type a name that is
taken it offers the next free one, such as "Software Engineer 2". A human seat
needs one contact identity before the company can be saved; the dialog asks
for it, and it can also be added later in the seat's editor.

## Editing a node

Choose **Edit** on the company, a unit or a seat to open its editor at the side
of the chart. The editor holds your changes in its own form until you press
**Apply**, which adds them to the draft as **one step**: one Undo takes the
whole edit back. Closing an editor that holds changes (Cancel, Close, Escape or
a click outside it) asks before discarding them.

If the engine refused something about the node at the last check, the problem
is shown beside the field it names. Problems that name no field in the editor
are listed at its top.

### The charter

| You can change | Notes |
|---|---|
| Name | Renaming the company asks you to confirm what it does. An agent seat's id is derived from the company name and its handle, so every agent seat gets a new id: each seat's diary and onboarding progress stay under the old id and are no longer read, and every agent seat onboards again. Handles, mailboxes and episodes are unchanged. |
| Mission, vision, policies | Policies are an ordered list. |

Everything else in the company document (providers, integrations, workers,
MCP servers, sandbox and scheduling settings) is edited in the configuration
document, not in the builder.

A company has no lead. The reporting chart's roots are the seats no one
manages, so the seat at the top of a company is the one nothing else manages,
not a field somebody sets.

### A unit

| You can change | Notes |
|---|---|
| Name | Unit names are unique. Renaming an existing unit re-keys what is attached to its name: agent seats in it and in its units onboard again, onboarding pages are looked up under the new name, and its schedules get a new identity, so a run due that minute may fire again. |
| Type | One of the well-known types or a custom one. It is informational; an empty type is `team`. |
| Purpose, goals | |
| Lead | Any seat. The empty choice shows the lead the unit inherits from the unit above it, as the last check reported it. |
| Channel | An empty channel inherits the one above it. |
| Knowledge | Free-text references, not a read scope. |
| Owns: Jira project, Confluence space | Where unrouted work for the unit goes. Not a permission. Shown only when the company has connected Jira or Confluence. |
| Schedules: enabled | Each schedule can be switched on or off. |

Shown and not changed: each schedule's cron, timezone, runner and task, and the
unit's tool credentials as server and variable names. Schedules and tool
credentials are written in the configuration document.

**Renaming a unit that holds literal credentials.** The engine never sends a
credential to the dashboard: a literal value arrives masked, and a save that
carries the mask back is restored from the stored unit of the same name. A
renamed unit has no stored unit of its new name, so the engine refuses the
save. Before you rename such a unit, the editor lists the paths of its masked
credentials. Move each one to the secret store (**Secrets**) and reference it
as `${NAME}` first; a reference is a name, so it survives the rename.

### A seat

| You can change | Notes |
|---|---|
| Name | Seat names are unique. An existing seat keeps its handle through a rename. |
| Handle | Only on a seat added in this draft. Leave it empty and the engine derives one from the name; the editor shows the derived handle after the next check. An existing seat's handle is its identity (its memory and mailbox attach to it), so it is not editable. |
| Email, goal, backstory, responsibilities | |
| Behavioral guidelines | Agent seats. |
| Manages | Seats and units. Seats this seat manages automatically as a unit's lead are listed apart, because the engine adds them whatever the list says. |
| Contact identities, availability | Human seats. A human seat needs at least one contact identity. |
| Model | Agent seats. An ordered chain of the company's `providers.llm` keys, tried in the order chosen; to change the order, remove a provider and choose it again. A seat with no model runs on the provider keyed `default`, else the first provider in the company's order. A per-phase mapping is shown and edited in the configuration document. |
| Token budget | Agent seats. Empty or 0 is unlimited. |
| Schedules: enabled | Agent seats. |
| Integrations | Agent seats; see below. |
| Owns: Jira project, Confluence space | Agent seats. Where unrouted work for the seat goes. Not a permission. |

Shown with the reason they are not changed here: per-phase models
(`llm_review` and the other `llm_*` fields), sandbox (enabled, where it runs),
workers, placement, learning, tool credentials (names only) and whether the
seat is the Datadog fallback. Sandbox, placement, workers and tool credentials
each depend on a company-level block the builder does not edit.

A seat's kind is changed with **Change to a human seat** or **Change to an
agent seat**, which is its own step because it removes fields.

A seat has no skills to edit. A skill is a knowledge base page the engine
admits and injects per phase, and the learning subsystem drafts new ones from
what turns did, so skills are managed in the knowledge base rather than on the
seat. See [Tool Skills](../concepts/tool-skills.md).

### Seat integrations

Each field is shown only when the company has connected the tool. Otherwise the
editor says the tool is not connected and links to **Integrations**. No
credential is ever shown.

- **GitHub:** access tier and repositories (an empty list means every
  repository the installation covers). Setting a tier on a seat with no GitHub
  block enrols the seat in GitHub; create its app from Integrations. A seat's
  app permissions are fixed when the app is created, so after changing the tier
  of a seat whose app exists, raise the app's permissions at GitHub as well.
- **Slack:** the default channel ID, for a seat that has its own Slack app.
- **Mattermost:** the default channel name, for a seat that has its own bot.
  The engine provisions a bot only where the seat's `bot_token` is a whole
  `${NAME}` reference, so for such a seat the bot username is read-only:
  changing it would make the provisioner find or create a second bot. A seat
  whose token is a literal is a bot somebody manages by hand, and its username
  stays editable. Empty means the provisioning username prefix and the seat's
  handle, lowercased.
- **GitLab:** the access level (developer or maintainer) the seat's account
  joins with, when GitLab provisioning is set up. Access levels are kept by
  handle, so a seat added in this draft can have one once the check reports
  its handle.

Not in the builder: per-seat allow or block lists for GitLab, Atlassian,
Datadog or Mattermost (the engine provisions every agent seat), a per-seat
Datadog role, GitLab tiers beyond developer and maintainer, and flags that
grant access to everything (an empty repository list already does).

## Moving a node

**Move to** moves a seat or a unit (with everything inside it) to the end of
another unit, or to the top level of the company. A unit is never offered a
destination inside itself. Before you confirm, the dialog shows what the move
changes, read from the engine's last check of your draft:

- who a moved seat reports to now, and the lead of the destination, who
  manages a unit's direct members unless another member manages the seat;
- the lead and the channel a moved unit, and the units inside it that declare
  none, would inherit instead;
- the agent seats that onboard again because the units above them change;
- the tool credential servers a moved agent seat gains or loses from its home
  unit's `mcp_env` (names only).

The next check confirms the result, and the review lists it before you save.

A unit's lead is a seat's name, so a seat that leads a unit **stays its lead**
wherever it moves. The dialog says so and offers **Clear lead** to remove it as
part of the move. A seat placed in a unit by its `unit:` reference is written
into the destination and the reference is removed.

The dialog also names any unit schedule the move would leave with no runner
(a schedule for members needs a direct agent member, and a schedule for the
lead fails when the effective lead is a human seat), because the engine refuses
the save until the schedule is disabled or has a runner, and says when a seat
being moved is working: its current turn continues on the previous
configuration until the engine applies the change.

## Deleting a node

**Delete** removes a seat, or a unit with every unit and seat inside it, from
the draft. Undo brings it back until the draft is saved. The dialog lists what
the removal clears inside the chart (a unit's lead, a `manages` entry, a root
seat's unit reference), the unit schedules it would leave with no runner, and
the seats that are working now.

Root seats declared at the top level with a `unit:` reference to the unit being
deleted are drawn inside it, so the dialog asks whether to delete them too or
keep them at the top level with the reference cleared.

Removing more than half of the saved company's seats asks for an
acknowledgement first.

### Outside the chart

Some of what a seat has is not in the chart, and the dialog says so before the
seat goes:

- **The Datadog fallback.** An alert whose tags name no seat wakes the fallback
  seat, and the engine refuses a Datadog block whose `route_to` names no agent
  seat. Deleting the fallback seat therefore asks for the agent seat that takes
  over, and writes it with the removal. When the removal would leave no agent
  seat at all, the dialog says so instead: add an agent seat first, or
  disconnect Datadog.
- **A GitLab access level.** The per-handle override is removed with the seat,
  because an entry left behind would grant its level to the next seat that
  derives the same handle.
- **Vendor identities and sealed credentials.** The GitHub App, Slack app,
  Mattermost bot, GitLab service account and Atlassian account the engine made
  for the seat, and the secret store entries its config references, stay until
  you decommission them. They are listed by name, never by value, with links to
  **Integrations** and **Secrets**.
- **The mailbox, coding runs and memory.** A removed agent seat's mailbox, and
  the mail still addressed to it, is kept for 24 hours after the engine applies
  the change and then retired, together with any coding runs it still has. Its
  memory (diary, episodes, counterparty profiles, onboarding markers) is kept,
  and a seat added later under the same handle reattaches to it. See
  [Seat Ownership](../concepts/seat-ownership.md#the-removed-seat).

## Changing a seat's kind

**Change to a human seat** and **Change to an agent seat** are their own step,
because the change removes fields: the engine refuses a human seat every
runtime field (models, token budget, workers, learning, schedules, chat app
blocks, Jira and Confluence ownership, tool credentials, behavioral guidelines
and its own GitHub App), and refuses an agent seat `contact` and
`availability`. The dialog lists the fields by name before anything is
recorded, and calls out the ones that hold credentials: the builder never shows
a credential, so it cannot type one back in and the value is gone for good once
the change is saved.

Becoming a human seat needs one contact identity, and cannot be done to the
Datadog fallback without choosing the agent seat that takes over. Where the
seat is the company's only agent seat, the dialog says so and the change waits
until another agent seat exists or Datadog is disconnected. The
schedules the change would strand, and a turn the seat is running now, are
named first. A seat that becomes human stops running; its memory is kept but
unused while it is a human seat.

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
