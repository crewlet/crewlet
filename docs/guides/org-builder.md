# The Org Builder

The dashboard's Agents workspace has an **Edit org** section
(`#/agents/edit`, the button on the org chart) for editing the organization of a running company, and
for creating the company on an engine that has none. It edits the same
company document `GET /config` serves and `PATCH /config` writes, so every
change it makes is an ordinary configuration revision: stored, activated,
audited, and applied by every node on its own tick.

The engine is the validator. The builder checks every draft by sending the
engine a dry run of exactly the write a save would send, and draws the
problems, warnings and derived hierarchy the engine answers with. It does not
implement any configuration rule of its own.

Every change is made to a draft in the browser. Nothing reaches the engine
until you review the draft and save it, and the builder keeps every field of
the document it does not show. What it shows and what it can change is listed
under [Editing a node](#editing-a-node), field by field.

## Opening the builder

The builder reads and writes the company document, so it needs what any other
client of it needs: `config:read` to read it, credentials masked, and
`config:write` to save it — a removal included, since it is one more change to
the same document. A save may ask you to confirm your password first, as every
configuration write does. Without `config:write`, the lead of a unit edits the
units they lead instead ([Editing as a unit lead](#editing-as-a-unit-lead)).

The credential is a person signed in with those grants, or an API token that
carries them, exchanged for a session on the dashboard's sign-in screen.
There is no posture in which it is not needed — `api.auth.disabled` is
retired, and `crewlet run -dev-principal` is a flag on a node reached over
loopback rather than a configuration a browser can meet. What the builder
shows is decided from what the engine answers, never from whether somebody is
signed in:

| The engine answers `GET /config` with | The builder shows |
|---|---|
| the active revision | the organization, ready to edit |
| `404 no_active_revision`, and the organization the node pushes names no company | creating the company |
| `404 no_active_revision`, while the organization names a company | "This node has not caught up with the fleet's configuration yet." Try again once the node has applied the fleet's revision, or use another node |
| `401` or `403`, to somebody signed in | "Reading the configuration needs config:read.", with **Sign in as somebody else** |
| `401` or `403`, to nobody signed in | "Editing the organization needs you to sign in.", with **Sign in**, which comes back to the builder |
| a plain `404`, or an answer that is not JSON | "This process does not serve the configuration." The process has no configuration store; open the dashboard on a node running the engine |
| nothing | the engine could not be reached, with **Retry** |

A node that has not caught up is never offered create mode: its own store is
empty while the fleet runs a company, and a create from there could only be
refused.

A node that is draining refuses every check and save with `503 draining`,
and the builder treats it as a node it could not reach: the draft is kept,
and the check retries until a peer or the restarted node answers. Nothing is
written by a refused write, so there is nothing to settle afterwards.

Signing in as somebody else — in another tab, since the session cookie is the
browser's — while a draft is open keeps the draft on screen. The builder reads
the company again and checks the draft as the new reader, and forgets the
copy it kept for the last one; if the engine refuses it, editing pauses until
somebody whose grants reach the company signs in. Signing out empties the
tab's kept draft too.

### Arriving from a link

Every way into Edit org from another screen asks to change one thing, and the
builder opens on it. The address says what, naming a node by its address — a
seat by its handle and a unit by its key, never by a name, since two units or
two seats may share one:

| The link carries | The builder opens |
|---|---|
| `seat=<handle>` | that seat's editor (a seat's **Edit** beside its setup on its profile, **Edit in org** in its menu) |
| `unit=<key>` | that unit's editor |
| `add=agent`, `add=human` or `add=unit` | the Add on that kind at the company's top level (the org chart's **Add seat** is `add=agent`) |
| `unit=<key>` and `add=…` | the Add under that unit |

It happens once, on arrival, and only when the builder can edit: once the
draft is loaded, and not while a kept draft waits for **Keep** or
**Discard**, a save's outcome is unknown or the company changed under the
draft. The request waits through each of those
and opens once they are settled. `add=` is then taken out of the address, so a
reload or a copied link does not ask for a second node; `seat=` and `unit=`
stay, because they are also the selection (see
[Selecting a node](#selecting-a-node)). Selecting another node inside the
builder, or a Back to an earlier selection while you stay in it, opens
nothing. But because the selection is part of the address, ANY arrival that
carries one is read the same way as a link: a reload with a node selected, or
a Back into Edit org from another screen (a seat's profile reached from the
node's **Open seat**, say), opens that node's editor, since the builder cannot
tell that address from a link somebody sent you. A link naming an address the
draft does not hold selects nothing and opens nothing.

## Creating the company

On an engine with no active configuration the builder opens on a form: the
company's name and mission, a shape to start from, and optionally a seat for
yourself.

| Start from | What it writes |
|---|---|
| New company | A chief executive and three units (Engineering, Product, Marketing), each with one agent seat reporting to the chief executive |
| Established company | A chief executive, Engineering with a Reliability team, Product with a Design team, and Go to Market, with each unit's lead reporting up the chain |
| Start empty | The charter alone |

The Established company template asks whether unit leads are **agents** (seats
the engine runs) or **people** (human seats). A template never invents a
contact identity, so human leads are created without one, and the only identity
written is the one you type for your own seat — which is optional too: leave it
empty if you will work only through the dashboard. A human seat with no contact
identity is legitimate, but no agent can @-mention that person, so the review
names each one and the configuration keeps warning about it at the seat's
`contact` until an identity is added.

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
connecting chat and trackers in Settings › Integrations, and adding a model
provider, which no dashboard screen writes. The engine applies the new company
without one, but until the provider is added no agent seat takes a turn:
whatever is sent to a seat waits on its inbox and runs once the provider
exists. The panel gives the exact `crewlet config import` and `PATCH /config`
commands for it.

## Reading the organization

The toolbar switches between two views of the draft: the **Visualization**, a
chart of the organization in a **Structure** and a **Reporting** arrangement
(chosen in the chart's own corner, under its zoom controls), and the
**Table**, the same structure as rows and columns. Below 640 pixels wide the
builder opens on the table. The view (`view=visualization|table`), the chart
(`chart=structure|reporting`) and the selected unit or seat are in the URL,
so a link opens the builder where it was.

Both views draw the draft as it stands, and take everything the engine
derives from its last check of it: where a seat declared at the top level
with a unit reference is placed, the lead a unit inherits, and who each seat
reports to. A derived value is shown only while the draft still holds what
the check saw, so just after a change a node can read "Lead after the
check" or "Manager after the check" for a moment rather than show an answer
the engine has not given.

Colour is state, never identity. Every node is drawn on the same neutral
surface whatever it holds; what tells a person's seat from an agent's is its
badge, a circle for a person and a squircle for an agent, and the one colour a
node takes is the accent ring of the selection. The live org chart follows the
same rule, and there colour says what a seat is doing: a draft is doing
nothing, so here there is nothing for colour to say.

### The structure chart

The company is the root. The seats declared at the top level and the units
hang off it, and each unit's seats and child units hang off that unit: every
seat is a node of its own, joined to the unit it sits in by the same
orthogonal branch the live org chart draws. The branch is heavier here: a node
is half the height of the live chart's card, and the design system keeps the
branch at the same weight against the node it joins. A node shows its badge (or a
unit's or the company's glyph), its name and, under it, what kind of thing it
is, with the marks that apply as small glyphs on that line. Each mark says its
sentence as its name and its tooltip:

| Mark | Meaning |
|---|---|
| Declared at the root with a unit reference | a seat declared at the top level that its `unit:` reference places in this unit |
| No unit named *X* | a `unit:` reference the engine resolved to no unit; the seat stays at the top level |
| Lead names no seat | the unit's lead names no seat of the company |
| Alerts that name no seat wake this seat | the Datadog fallback seat, while Datadog is enabled |

A node carries neither the seat's live state nor a problem count. Drawn on
every node they were a column of "idle" dots down a company where nothing was
running and an empty box beside every node with nothing wrong; the table
writes both out, the toolbar's status counts the draft's problems, and each
problem is shown in the editor of the node it names. A name too long for its
node is shortened rather than growing the node, so the chart does not move
while a check is on its way.

A unit's node carries its **lead chip**: the lead it declares, the one it
inherits from the unit above (marked inherited), or "No lead". Pressing the
chip opens the lead choice in place: **No lead** (saying what the unit would
then inherit), each seat in the unit, and **Choose another seat**, which opens
the unit's editor at its lead.

A node's controls appear when it is reached, by the pointer, by focus or by
being the selected node: the expander on its leading edge, **Edit** and
**Delete** down its end, and, under a node that can take a child, the **Add**
on the branch its children hang from, which splits into **Add unit**, **Add
agent seat** and **Add human seat** where it stands.

Every node is one stop in a tree. The arrow keys walk the visible order: Right
opens a closed unit or steps into an open one, Left closes it or climbs to the
unit above, Home and End jump, and typing a name moves to the next node it
matches. On a node, Enter opens its editor, Delete or Backspace deletes it
(never the company), Alt+Up and Alt+Down move it among its siblings, as they
move a row in [the table](#the-table), and the context menu key or
Shift+F10 opens its menu, which holds everything the node's own controls do
not. Each unit collapses and expands on its own, and **Expand all** and
**Collapse all** in the toolbar do it for the whole chart.

The chart pans with a drag, or with the wheel while it has focus; Ctrl or
Command with the wheel zooms toward the pointer, and plus, minus and zero zoom
and fit while the chart itself has focus. On a touch screen one finger scrolls
the page until you tap the chart, which then pans with one finger until you
press **Done**; two fingers pinch at any time.

The chart fits itself when it is first drawn, but never below the size its
text can be read at: 85%, the floor the live org chart keeps. A company too
wide to be read whole is drawn at exactly that size, centred across on the
selected node or, with none selected, on the company itself, and placed down
the pane where **Fit** would put it (in the middle when it is shorter than the
pane, from the top when it is not, and never with the selected node off it);
the rest of it is a pan away; **Fit** (the control or zero) draws the whole
company at whatever size that takes, because that is a request for all of it.
After that the chart moves only when you move it, when a node you act on has
to be revealed, or when an editor or a dialog opens over a node, which eases
the chart onto it and back.

### The reporting chart

**Reporting** draws who each seat reports to, as the engine derives it from
`manages`, unit leads and the unit tree: a seat's primary manager is the first
seat, in the engine's order, that manages it. The seats with no manager are
the tops of the chart, marked "No manager", and seats that manage each other
in a loop are drawn under one **Reporting cycle** group, each loop from its
first seat in the engine's order. The chart is read-only, because a reporting
line is not written anywhere as such: **Edit reports** on a seat (Enter on its
card) opens its editor at **Manages**, where the lines are changed. Until the
first check answers the chart says the lines appear after it, and while the
check of later changes is on its way it says that it shows the lines of the
last one.

### The table

The table is the structure as a tree of rows, in the order the chart draws
it, with the columns **Name** (the node's kind or type under its name, and a
saved agent seat's live state beside it), **Handle or key** (a seat's handle,
a unit's key), **Lead or reports to**
(a unit's lead chip, or a seat's primary manager), **Problems** and the row's
actions. It has no sort and no column settings: an organization has one order
its rows mean anything in. A row takes the same keys as a node on the chart,
and Right also steps from a row into its cells, Left back out; in a cell, Up
and Down keep the column, and a cell that holds a control (the lead chip, the
actions menu, an add button) puts focus on the control itself. At narrow
widths the table scrolls sideways in its own box, never the page.

Each row ends in its controls: on the company's row and a unit's, the **Add**
that splits into **Add unit**, **Add agent seat** and **Add human seat**;
**Edit** and **Delete** on every row but the company's, which cannot be
deleted; and a menu of the rest (**Open seat**, **Edit reports**, **Change
kind**, **Move to** and the two moves among the siblings). Where the builder
cannot write, the controls are disabled with the reason, never hidden.

**Alt+Up** and **Alt+Down** move a row among its siblings of the same kind (a
seat among its unit's seats, a unit among its parent's units), past the row
drawn beside it. A seat placed in a unit by its unit reference is declared at
the top level, so it is not reordered inside that unit; move it into the unit
first. Order matters to the engine: a seat's primary manager is the first seat
that manages it, so when the check of a reordered draft reports that a seat
now reports to someone else, the builder says so.

### Selecting a node

Selecting a unit or a seat names it in the URL by its address — `unit=` with
the unit's key and `seat=` with the seat's handle, never a name, since two
units may share one — and the toolbar carries that node's own actions: the
same menu as its node and its row, so every action is reachable from the
keyboard. **Open seat** is offered only for a seat the saved company has: a
seat added in the draft has no screen until it is saved. A new address
rewrites the URL rather than leaving a link pointing at something that no
longer answers to it. A link that arrives naming a node opens its editor as
well (see [Arriving from a link](#arriving-from-a-link)); selecting a node
inside the builder never does. Selecting the company itself carries the
charter's **Edit** and the same **Add** menu; it names no filter, because the
builder is already about that company. Where the builder cannot write (a
guarded or read-only posture, or a draft waiting to be updated) the actions
stay in the menu and are marked unavailable, so what the builder does is still
legible.

### The check

The check status beside the view controls says what the engine made of the
current draft:

| Status | Meaning |
|---|---|
| Checking | a dry run of the current draft is on its way |
| No problems | the engine would accept the draft as it stands |
| *N problems* | the engine would refuse it; each problem is marked on the unit or seat it names, and problems about the whole document are listed above the chart |
| Could not reach the engine to check | the dry run got no answer; the builder retries with an increasing wait |
| The configuration changed | another revision was activated after this draft was started |
| Needs config:write | the session was accepted and does not carry `config:write`; sign in as somebody who holds it |
| Needs confirmation | the write asks you to confirm your password first and the confirmation was declined; **Check again** asks again |
| Refused | the engine refused the check for another reason, which the banner under the toolbar gives |
| Needs sign-in | nobody is signed in, or the session ended; sign in |

A change is checked about a third of a second after it is made, so a burst of
changes (a held key, several undos) is checked once.

When the company has no model provider configured, a caution says so. The
engine applies such a company and places its seats, but no agent seat takes a
turn until a provider exists: work sent to a seat waits on its inbox and runs
once one is added
([A Company With No Model Provider](../concepts/configuration.md#a-company-with-no-model-provider)).
The builder does not write providers, and the dashboard does not add one:
Settings › Models & keys edits a model the configuration already declares.
Add one with `crewlet config import` or `PATCH /config`
([Configure via the API](configure-via-api.md)).

## Adding a unit or a seat

**Add unit**, **Add agent seat** and **Add human seat** on the company or a
unit (the **Add** on its branch or its row, its menu, or the toolbar) add the
new node at the end of that unit, or at the top level of the company. On the structure
chart the form is drawn IN the chart, in the place the new node will take: the
chart makes room in that rank, draws the branch to it and eases onto it, so
what you are adding to stays in view while you name it. On the table and on
the reporting chart, which have no place to draw it, the same form opens as a
dialog. A name is prose, and two seats or two units may share one: every
reference names a seat by its **handle** and a unit by its **key**, and the
form asks for that address beside the name. It follows the name until you
type one (lowercase letters, digits and hyphens for a handle; a unit's key
starts with a letter and may hold underscores; at most 64 characters), and it
is never an address a seat or unit of the saved company or the draft holds —
one this draft removes included, because a new seat under a removed seat's
handle would be that seat again, its memory and mailbox included. A lead's
draft avoids the rest of the company's addresses too, though it holds only
their unit. The address
is fixed once the draft is saved. A human seat's contact identity is optional:
the form offers it, it can be added later in the seat's editor, and without
one the person is reached through the dashboard only.

## Editing a node

Choose **Edit** on the company, a unit or a seat to open its editor at the side
of the chart. **Edit reports** opens a seat's editor at **Manages**, and a
lead chip's **Choose another seat** opens a unit's at **Lead**, so an action
about one field starts on that field. The editor holds your changes in its
own form until you press **Apply**, which adds them to the draft as **one
step**: one Undo takes the whole edit back. Closing an editor that holds
changes (Cancel, Close, Escape or a click outside it) asks before discarding
them, and so does anything that would leave the builder under it: one of its
links, the browser's Back or Forward to another screen, or a reload.
**Keep editing** leaves you where you were, form and all. Moving between the
builder's own views (Back from the table to the visualization, say) keeps the
editor open with your changes, so it asks nothing.

If the engine refused something about the node at the last check, the problem
is shown beside the field it names. Problems that name no field in the editor
are listed at its top. A warning (a lead that names no seat, for example)
does not stop a save, so it is listed at the top as a caution with the path
it names, never shown as a field's error.

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
| Name | Prose; two units may share a name. Renaming a unit re-onboards the agent seats in it and in its units, because onboarding pages are looked up under the new name. |
| Key | The address every `manages` entry and seat placement names the unit by, and the unit's identity: its schedules and work are filed under it. Editable on a unit this draft added; shown and not edited on a saved one, since another key is another unit (remove it and add a new one). |
| Type | One of the well-known types or a custom one. It is informational; an empty type is `team`. |
| Purpose, goals | |
| Lead | Any seat. The empty choice shows the lead the unit inherits from the unit above it, as the last check reported it. |
| Channel | An empty channel inherits the one above it. |
| Knowledge | Free-text references, not a read scope. |
| Owns: tracker project, knowledge space | Where unrouted work for the unit goes and where it files its own, whichever backend runs the tracker and the knowledge base. Not a permission. Changing either takes `config:write`, because a lead's authority is derived from them. |
| Schedules: enabled | Each schedule can be switched on or off. |

Shown and not changed: each schedule's cron, timezone, runner and task, and the
unit's tool credentials as server and variable names. Schedules and tool
credentials are written in the configuration document.

### A seat

| You can change | Notes |
|---|---|
| Name | Prose; two seats may share a name. An existing seat keeps its handle through a rename, and with it its memory and mailbox, but an agent seat that is renamed onboards again: its onboarding progress is stamped with its own name and the names of the units above it. |
| Handle | The address every lead, `manages` entry and mention names the seat by, and the seat's identity: its mailbox, schedules and everything it has learned (its diary, episodes, skills, profiles of colleagues and thread history) are keyed on it. Editable on a seat this draft added, and shown and not edited on a saved one, where **Replace this seat…** gives the role another handle (see [Replacing a seat](#replacing-a-seat)). |
| Email | The address inbound Jira and GitHub payloads identify the seat by, written as typed. |
| Goal, backstory, responsibilities | |
| Behavioral guidelines | Agent seats. |
| Manages | Seats and units. Seats this seat manages automatically as a unit's lead are listed apart, because the engine adds them whatever the list says. |
| Contact identities, availability | Human seats. Contact identities are optional: a seat with none is reached through the dashboard only, and the configuration warns about it at the seat's `contact`. |
| Model | Agent seats. An ordered chain of the company's `providers.llm` keys, tried in the order shown, which you reorder in place. A seat with no model runs on the provider keyed `default`, else the first provider in the company's order. A per-phase mapping is shown and edited in the configuration document. |
| Token ceilings: daily, weekly, monthly | Agent seats. One box per calendar window on the company's clock, each optional: an empty box is no ceiling on that window, and a 0 is refused rather than read as unlimited. A turn runs only while every capped window has room, and the company's own `token_budget` applies on top. The check warns about a ceiling that can never refuse anything — a week at or above seven days of the daily one, a seat at or above the company. |
| Schedules: enabled | Agent seats. |
| Integrations | Agent seats; see below. |
| Owns: tracker project, knowledge space | Agent seats. Where unrouted work for the seat goes and where it files its own. Not a permission. Changing either takes `config:write`. |

Shown with the reason they are not changed here: per-phase models
(`llm_review` and the other `llm_*` fields), sandbox (enabled, where it runs),
workers, placement, learning, tool credentials (names only) and whether the
seat is the Datadog fallback. Sandbox, placement, workers and tool credentials
each depend on a company-level block the builder does not edit.

A seat's kind is changed with **Change to human seat** or **Change to agent
seat**, in its menu or in the editor, which is its own step because it
removes fields.

#### Replacing a seat

A saved seat's handle never changes, because a seat with another handle is
another seat: a new mailbox, new memory and a new agent id. **Replace this
seat…**, beside the handle in the editor, gives the role a new handle by doing
exactly that, and says so before it records anything. The new seat takes the
old one's place and fields, and every unit lead, `manages` entry, Datadog
fallback and GitLab access level that named the old handle moves to the new
one. A credential the old seat holds as a value rather than a `${NAME}`
reference does not come along: the engine fills a masked value back in by the
seat's handle, which the new seat does not share, and the builder never holds
the value to copy it, so the replacement names each one it leaves behind and
the review lists them again. A reference carries over as written. The old
seat's work, memory and history stay with its handle, and its
mailbox is retired like any removed seat's
([Deleting a node](#deleting-a-node)). The replacement is one step: one undo
puts the old seat back. It waits while the editor holds changes of its own;
apply or discard them first. A seat this draft added has nothing to replace:
its handle is edited in place until the save.


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
- **Mattermost:** the default channel name, for a seat that has its own bot.
  The engine provisions a bot only where the seat's `bot_token` is a whole
  `${NAME}` reference, so for such a seat the bot username is read-only:
  changing it would make the provisioner find or create a second bot. A seat
  whose token is a literal is a bot somebody manages by hand, and its username
  stays editable. Empty means the provisioning username prefix and the seat's
  handle, lowercased.
- **GitLab:** the access level (developer or maintainer) the seat's account
  joins with, when GitLab provisioning is set up. Access levels are kept in
  the settings by handle, so a new handle carries its level with it.

Not in the builder: a seat's own Slack channel, per-seat allow or block lists
for GitLab, Atlassian, Datadog or Mattermost (the engine provisions every
agent seat), a per-seat Datadog role, GitLab tiers beyond developer and
maintainer, and flags that grant access to everything (an empty repository
list already does).

Two more things the builder deliberately does not have. A seat has no colour
of its own: colour on the dashboard shows state, never identity, so seats are
told apart by their names and their badges. And the zoom is the chart's own:
zoom in, zoom out and fit are buttons on the chart and keys while it has focus
(plus, minus and zero), the percentage between the buttons opens a field to
type one into, and Ctrl or Command with the wheel zooms toward the pointer.

## Moving a node

**Move to** moves a seat or a unit (with everything inside it) to the end of
another unit, or to the top level of the company. A unit is never offered a
destination inside itself. Before you confirm, the dialog shows what the move
changes, read from the engine's check of the draft as it stands. While that
check is still on its way (just after another change, say), the dialog says
so rather than reading an older answer:

- who a moved seat reports to before the move, and whether that ends with
  it: a unit's lead manages the unit's direct members, so a seat managed only
  that way stops reporting to the lead of the unit it leaves;
- the lead of the destination, who manages its direct members unless another
  member manages the seat;
- the lead and the channel a moved unit, and the units inside it that declare
  none, would inherit instead;
- the agent seats that onboard again because the units above them change;
- the tool credential servers a moved agent seat gains or loses from its home
  unit's `mcp_env` (names only).

The next check confirms the result, and the review lists it before you save.

A unit's lead is a seat's handle, so a seat that leads a unit **stays its lead**
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

A human seat somebody is bound to cannot be removed while they are: the
engine refuses the save with `409 seat_held`, naming each person who holds
the seat. The builder places that refusal on the seat — "@pat is held by
pat.doe: unbind them first (`crewlet iam unbind <person id>`), then save
again" — and keeps the draft; there is nothing to retry until the person is
unbound (People & access shows who holds each seat). The same refusal meets a
held human seat made an agent's, and a held seat replaced.

Removing more than half of the saved company's seats asks for an
acknowledgement first.

### Outside the chart

Some of what a seat has is not in the chart, and the dialog says so before the
seat goes:

- **The Datadog fallback.** While Datadog is enabled, an alert whose tags name
  no seat wakes the fallback seat, and the engine refuses an enabled Datadog
  block whose `route_to` names no agent seat. Deleting the fallback seat
  therefore asks for the agent seat that takes over, and writes it with the
  removal. When the removal would leave no agent seat at all, the dialog says
  so instead: add an agent seat first, or disconnect Datadog. A Datadog block
  that is switched off wakes nobody and requires no fallback, so its
  `route_to` asks nothing of a removal.
- **A GitLab access level.** The per-handle override is removed with the seat,
  because an entry left behind would grant its level to the next seat added
  under the same handle.
- **Vendor identities and sealed credentials.** The seat's GitHub App, Slack
  app and Mattermost bot, the GitLab, Datadog and Atlassian accounts it is
  enrolled for (it holds a credential for that tool's `mcp_env` server, or its
  unit does), and the secret store entries its config references, stay until
  you decommission them. They are listed by name, never by value, with links to
  **Settings › Integrations** and **Settings › Secrets**. A seat added in this draft was never saved,
  so nothing exists for it outside the chart.
- **The mailbox, coding runs and memory.** A removed agent seat's mailbox, and
  the mail still addressed to it, is kept for 24 hours after the engine applies
  the change and then retired, together with any coding runs it still has. Its
  memory (diary, episodes, counterparty profiles, onboarding markers) is kept,
  and a seat added later under the same handle reattaches to it. See
  [Seat Ownership](../concepts/seat-ownership.md#the-removed-seat).

## Changing a seat's kind

**Change to human seat** and **Change to agent seat** are their own step,
because the change removes fields: the engine refuses a human seat every
field an agent runs on (models, token ceilings, workers, learning, schedules,
chat app blocks, its tracker project and knowledge space, tool credentials,
behavioral guidelines and its own GitHub App), and refuses an agent seat
`contact` and
`availability`. The dialog lists the fields by name before anything is
recorded, and calls out the ones that hold credentials: the builder never shows
a credential, so it cannot type one back in and the value is gone for good once
the change is saved. Removing a field tears nothing down at a vendor, so the
seat's apps, bots and accounts, and the secret store entries the removed fields
referenced, are listed as they are for a deleted seat.

Becoming a human seat offers a contact identity without requiring one, and
cannot be done to the
Datadog fallback (while Datadog is enabled) without choosing the agent seat
that takes over. Where the
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
  a replaced seat (listed as the old seat removed and the new one added,
  since the new seat carries none of the old one's memory or mail), seats
  that onboard again (and why), reporting lines and leads that move
  (including a reorder that only changes which manager comes first),
  channels, where unrouted work goes, the tool credentials a seat gains or
  loses (names only), fields a kind change removes, credentials a replaced
  seat leaves behind, references a removal clears, the Datadog fallback seat
  and GitLab access levels.
- **Warnings** the engine gave for exactly this save.

A company rename, a kind change, a change of tool credentials (a credential a
kind change or a replacement leaves behind included) and removing more than
half of the seats each need an
acknowledgement before **Save** enables. While the engine reports problems
the review opens so you can read it, with Save disabled until they are fixed.
While a check is out, Save waits for it. If the engine could not be reached to
check, saving is still allowed: the write itself is validated.

The **audit summary** is prefilled from the changes and recorded with the
revision, followed by a write id such as
`(write 4f1c9a0b2e7d4c81a3b5f6e7d8c9b0a1)`. A save is the
same request as its checks: a `PATCH /config` merge patch of what changed,
with `If-Match` naming the revision the draft was started from, so a newer
revision is refused (and offered as an update) rather than overwritten. A
unit's lead saves that unit whole instead, with `PUT /config/units/{key}` under
the same `If-Match` ([Editing as a unit lead](#editing-as-a-unit-lead)).

### When the answer does not arrive

A save whose answer is lost (a dropped connection, a gateway timeout) may
still have been stored. The builder never assumes either way: it reads the
revision history and treats the save as done when it finds a revision whose
parent is the draft's base and whose summary carries the write id. If the
save did not land, it says so and offers to save again. If the engine cannot
be asked, editing pauses and the review offers **Check again** and **Save
again**; a second save carries the same write id, so a first save that did
land is recognized as yours rather than replayed on top of itself.

The same holds when you leave the builder, or the tab reloads, before the
answer arrives. The kept draft is marked with the save's write id before the
save is sent, so the next time the builder opens in this tab it settles that
save first: if it landed, the builder says "The last save from this tab was
stored" and does not offer the draft again (replaying it onto its own revision
would apply every change twice); if it did not, the draft comes back as any
kept draft does. While the engine still cannot say, editing stays paused, with
**Check again**. Coming back to the builder while that save is still on its
way, the builder waits for the save's own answer before it settles anything.

### After saving

A save stores and activates a revision. It does not apply it: every node
applies on its own reconcile tick (about every fifteen seconds, spread a
little per node so a fleet does not apply at once), and a node can refuse a
revision and go on serving the previous one. Until this node has
applied it, the org chart, Teams and Settings › General still draw the
previous organization, and say so.

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
Both read the company document, so a lead without `config:read` is offered
neither.

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
import writes back what the builder wrote rather than undoing it. Keep every
seat's `handle` and every unit's `id` in it: the stored company holds the ones
it minted, while a file that leaves them out is minted afresh from its names
on every import, so a name corrected there would replace that seat or unit. Credentials
in the copy read as `__redacted__` (a whole `${NAME}` reference is shown as it
is). Importing the file over the running company restores each masked value
from the active revision by the identity of what holds it (a seat by its
handle, a unit by its key); a store with no active revision has nothing to
restore from, which is one more reason a file kept in a repository should
hold `${NAME}` references rather than values.

## A draft survives a reload

The builder keeps the draft's list of changes (never the document itself,
which holds contact identities and policies) in the tab's session storage, so
a reload or a trip to another screen does not lose the work. The list holds
what you typed into it, so the storage belongs to the tab and is emptied when
you sign out — or when somebody else signs in here after your session ended —
and a kept draft is only ever offered back to the person it was kept for.
When the builder opens and finds a kept draft:

- **Made against the revision that is still active:** a banner offers **Keep
  the draft** or **Discard it**, and nothing can be edited until you choose.
  Coming back to Edit org from another section of Agents restores the draft
  without asking.
- **Made against an older revision:** the draft is restored through the same
  update described below, and **Discard the kept draft** removes it instead.
- **Made for creating a company, where a company now exists** (or the other
  way around): it is discarded, and the builder says so.
- **Made of a unit you no longer lead** (a lead's draft): it is discarded,
  and the builder says so. While you still lead it, the builder opens that
  unit for it, whichever unit a link names, so a reload never loses a draft
  of your second unit.

The kept draft is removed when you save, when you discard, when the reader
changes, and when the engine refuses the credential, because each of those
may mean the tab has changed hands. Session storage does not outlive the tab,
so while the draft holds changes the browser asks before the tab closes. It
asks on a reload too, because a browser cannot tell the two apart; the draft
itself survives the reload.

A browser that refuses session storage (a private window, blocked site data)
shows a caution: editing works, but the draft will not survive a reload or
leaving the builder, and leaving the builder asks before it discards the
draft. A draft of more than 500 changes is not kept either; save it in steps.

## When somebody else saves first

Every check is conditional on the revision the draft was started from, so a
revision saved by somebody else (another operator, `crewlet config import`, a
setup flow in Settings › Integrations) is found at the next check, not at the
save. The status reads "The configuration changed", editing pauses, and a
banner offers **Show what changed** (the newer revision against the one the
draft was started from — to a reader who may read the company document) and
**Update my draft**. A builder with no changes on
it has nothing to update: it moves onto the newer revision by itself and goes
on. An editor you have open keeps what you typed in it, and the selected unit
or seat stays selected, so an edit you had not applied yet applies to the
newer revision.

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
unit is its key, because those are what its memory, mailbox, schedules and
credentials attach to. A seat removed and created again with a different
handle is a different seat, and a change made to the original is dropped
rather than applied to it. One created again under the same handle, or a unit
under the same key, is the same one to the engine, so a change made to it is
held to the values it recorded like any other: a field somebody set
differently is a conflict for you to decide.

If this node still serves the older revision (it has not applied the newer one
yet, or a load balancer sent the read to a node that has not), the update
waits: "This node has not caught up with the newer revision yet." Try again in
a moment. If the configuration the draft edits is no longer active at all, the
banner offers **Discard and reload**.

## Editing as a unit lead

The lead of a unit edits that unit and everything inside it without
`config:write`: its own fields, its seats, the units inside it and theirs,
seats added, moved between its teams or removed. A lead is the seat a unit
names as its lead, or inherits from the unit above it when it names none, so
leading a unit is leading every unit inside it, a sub-unit with a lead of its
own included. The person needs a seat binding in the identity directory and
no configuration grant; whoever holds `config:write` edits the whole company
as above, units they lead included.

Such a person opening Edit org is shown the unit they lead rather than the
company, and the toolbar says so: **Editing *unit* as its lead**. Somebody who
leads several units that are not inside one another edits one of them per
draft, and that label is a menu of the others; another unit opens only while
the draft has no changes. A link that names a seat or a unit opens the unit
holding it.

The draft is that unit, whole, read with `GET /config/units/{key}` and
checked and saved with `PUT /config/units/{key}` under `If-Match`, so the rest
of the company is never sent and a newer revision refuses the save exactly as
it refuses a whole-company one. An unchanged unit is checked too, so its
managers and inherited leads are drawn before anything is edited: the engine
refuses a lead only the save of a unit as it was, which would re-publish the
company. The engine judges every write
part by part and refuses what reaches outside the units the caller leads,
naming each part, and the builder marks each on the seat or unit it is about:

- the company's charter, its settings and its top level, and the unit's own
  lead and place (a move or a deletion), which decide who leads it. The
  builder draws these controls disabled with the reason, and the Lead of a
  unit inside it stays the lead's to change;
- a lead or a `manages` entry naming a seat outside the units the caller leads;
- a value another system finds a seat or a unit by — a project, a knowledge
  space, a channel, a contact identity — and every credential, a `${VAR}`
  anywhere and a tool server's `mcp_env` block included. These take
  `config:write`.

A lead reads no revision history, which takes `config:read`. A save whose
answer is lost is settled by reading the unit back: still on the base
revision, it did not land; at a newer one holding exactly the unit as sent,
it did; anything else is a newer company, which the draft is updated onto as
in [When somebody else saves first](#when-somebody-else-saves-first), finding
the changes the save landed already there. **Show what changed**, **View
changes** and **Copy as YAML** read the company document and are not offered.

The same rule reaches a lead outside the builder, for the seats in the units
they lead: a seat's profile shows its settings, read through
`GET /config/roles/{handle}` and its unit's own fields, and its token ceilings
are changed in Spend › Budgets and with **Raise budget** where a decision about
a stopped seat offers it, through `PUT /config/roles/{handle}`. The company's
own ceilings stay `config:write`'s.

## Undo, redo and the keyboard

Every change is an operation in the draft's log. **Undo** and **Redo** in the
toolbar, or <kbd>Ctrl</kbd>+<kbd>Z</kbd> and
<kbd>Shift</kbd>+<kbd>Ctrl</kbd>+<kbd>Z</kbd> (<kbd>Command</kbd> on a Mac),
walk the log from anywhere in the builder except a text field, where the same
keys undo typing. Each change is announced to screen readers (an undo or a
redo says it undid or redid the change), and focus moves to the unit or seat
it touched: an added node, the node before a deleted one (or its unit), a
moved node where it went. **Discard changes** throws the whole draft away
after a confirmation; the saved configuration is not touched. The keys of the
chart and the table are described under
[Reading the organization](#reading-the-organization).

At narrow widths Undo, Redo, Discard changes, Expand all and Collapse all move
into the **More** menu; the check status stays visible.

**Fullscreen**, where the browser offers it, takes the whole builder
(toolbar, chart, editor and dialogs) rather than the chart alone.
