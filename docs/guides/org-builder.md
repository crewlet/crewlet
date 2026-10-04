# The Org Builder

The dashboard's Agents workspace has an **Edit org** section
(`#/agents/edit`, the button on the org chart) for editing the organization of
a running company, and for creating the company on an engine that has none.
The org chart — its units, its seats, who leads and who manages whom — is part
of the company document, beside the charter, providers and integrations, which
`GET /config` serves and `PUT` / `PATCH /config` write as a stored, activated,
audited revision ([Organization Model](../concepts/organization-model.md)).

Every change is made to a draft in the browser. Nothing reaches the engine
until you review the draft and save it, and the builder keeps every field it
does not show. What it shows and what it can change is listed under
[Editing a node](#editing-a-node), field by field.

A save is **one write** of that document: a new revision, holding the units,
the seats and the settings the draft changed together. The review shows it
with what the engine answered
([Reviewing and saving](#reviewing-and-saving)).

## Opening the builder

The builder reads and writes the company document, so it needs what any other
client of it needs: `config:read` to read it, credentials masked, and
`config:write` to save it — a removal included, since it is one more change to
the same document. A save may ask you to confirm your password first, as every
configuration write does.

The credential is a person signed in with those grants, or an API token that
carries them, exchanged for a session on the dashboard's sign-in screen.
There is no posture in which it is not needed — `api.auth.disabled` is
retired, and `crewlet run -dev-principal` is a flag on a node reached over
loopback rather than a configuration a browser can meet. What the builder
shows is decided from what the engine answers, never from whether somebody is
signed in:

| The engine answers | The builder shows |
|---|---|
| `GET /config` with the active revision | the organization, ready to edit |
| `GET /config` with `404 no_active_revision`, and the organization the node pushes names no company | creating the company |
| `GET /config` with `404 no_active_revision`, while the organization names a company | "This node has not caught up with the fleet's configuration yet." Try again once the node has applied the fleet's revision, or use another node |
| either read with `403` naming grants | "Editing the organization needs *grant*, which the credential you presented does not carry." — the grants the refusal named, any one of which would do |
| either read with `401` | "The engine refused this browser's session." when somebody is signed in, and otherwise a request for a credential — **Sign in**, which comes back to the builder |
| `GET /config` with a plain `404`, or an answer that is not JSON | "This process does not serve the configuration." The process has no configuration store; open the dashboard on a node running the engine |
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

It happens once, on arrival, and only when the builder can edit: once both
halves are loaded and the engine has described the draft, and not while a
kept draft waits for **Keep** or **Discard**, a save's outcome is unknown or
the company changed under the draft. The request waits through each of those
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
creates the company: first the settings, with `PUT /config` and
`If-None-Match: *`, which is refused if a company exists anywhere in the
fleet, and then the org chart it holds. If a company was created while you
were writing yours — its settings, or seats and units in its chart (the
builder hears of it as soon as the node reports the new organization, not only
when you save) — the builder says so and offers to discard your draft and
open the company: a draft that starts a company is never applied to one that
exists, and never replayed onto it. **Keep my draft** leaves it on screen to
read, read-only, with the same offer beside it.

After a successful create, two steps remain that the dashboard cannot take:
connecting chat and trackers in Settings › Integrations, and adding a model
provider, which no dashboard screen writes. The engine applies the new company
without one, but until the provider is added no agent seat takes a turn:
whatever is sent to a seat waits on its inbox and runs once the provider
exists. The panel gives the exact `PATCH /config` for it — a merge patch of
`providers`, which changes nothing else. A `crewlet config import` would write
a whole company file instead, its org chart included, over the chart you just
made.

## Reading the organization

The toolbar switches between two views of the draft: the **Visualization**, a
chart of the organization in a **Structure** and a **Reporting** arrangement
(chosen in the chart's own corner, under its zoom controls), and the
**Table**, the same structure as rows and columns. Below 640 pixels wide the
builder opens on the table. The view (`view=visualization|table`), the chart
(`chart=structure|reporting`) and the selected unit or seat are in the URL,
so a link opens the builder where it was.

Both views draw the draft as it stands. The chart's rows state where every
seat sits and who leads each unit, so that is drawn straight from the draft,
and so is the lead a unit inherits: a unit that names no lead takes the one
the unit above it resolved to, after every change. Who each seat **reports
to** follows from the whole organization — every `manages` list, with its unit
entries expanded, and every lead managing its unit's members — and only the
engine derives it, from the chart it holds: the builder shows it from the
engine's description of the **saved** company, and, once the draft has moved
past that, says the lines are the saved ones rather than guessing the new
ones.

Colour is state, never identity. Every node is drawn on the same neutral
surface whatever it holds; what tells a person's seat from an agent's is its
badge, a circle for a person and a squircle for an agent, and the one colour a
node takes is the accent ring of the selection. The live org chart follows the
same rule, and there colour says what a seat is doing: a draft is doing
nothing, so here there is nothing for colour to say.

### The structure chart

The company is the root. The seats at the top level and the units hang off
it, and each unit's seats and child units hang off that unit: every seat is a
node of its own, joined to the unit it sits in by the same orthogonal branch
the live org chart draws. The branch is heavier here: a node is half the
height of the live chart's card, and the design system keeps the branch at the
same weight against the node it joins. A node shows its badge (or a unit's or
the company's glyph), its name and, under it, what kind of thing it is, with
the marks that apply as small glyphs on that line. Each mark says its sentence
as its name and its tooltip:

| Mark | Meaning |
|---|---|
| a caution | a reference that names nothing in the draft: a unit's lead that names no seat ("Lead names no seat"), or a `manages` entry that names no seat and no unit. The chart keeps such a reference as written, so it is a warning rather than a problem |
| Alerts that name no seat wake this seat | the Datadog fallback seat, while Datadog is enabled |

A node carries neither the seat's live state nor a problem count. Drawn on
every node they were a column of "idle" dots down a company where nothing was
running and an empty box beside every node with nothing wrong; the table
writes both out, the toolbar's status counts the draft's problems, and each
problem is shown in the editor of the node it names. A name too long for its
node is shortened rather than growing the node, so the chart does not move
while a check is on its way. A caution stays for as long as the node still
writes the reference, so a check on its way does not take it off the node
either.

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
(never the company), and the context menu key or Shift+F10 opens its menu,
which holds everything the node's own controls do not. Each unit collapses and
expands on its own, and **Expand all** and **Collapse all** in the toolbar do
it for the whole chart.

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
the tops of the chart, and seats that manage each other in a loop are drawn
under one **Reporting cycle** group, each loop from its first seat in the
engine's order. The chart is read-only, because a reporting line is not
written anywhere as such: **Edit reports** on a seat (Enter on its node) opens
its editor at **Manages**, where the lines are changed.

The lines are the engine's description of the saved company. Until the node
has pushed one — a company not saved yet has nothing to describe — the chart
reads "Reporting lines appear once the engine describes the company", and
while the draft holds changes to the chart it says "These are the saved
company's reporting lines. The changes in this draft appear here once it is
saved."

### The table

The table is the structure as a tree of rows, in the order the chart draws
it, with the columns **Name** (the node's kind or type under its name, and a
saved agent seat's live state beside it), **Address** (a seat's handle, a
unit's key), **Lead or reports to** (a unit's lead chip, or a seat's primary
manager — "Not derived yet" once the draft has moved past the saved chart),
**Problems** and the row's actions. It has no sort and no column settings: an
organization has one order its rows mean anything in. A row takes the same
keys as a node on the chart, and Right also steps from a row into its cells,
Left back out; in a cell, Up and Down keep the column, and a cell that holds a
control (the lead chip, the actions menu, an add button) puts focus on the
control itself. At narrow widths the table scrolls sideways in its own box,
never the page.

Each row ends in its controls: on the company's row and a unit's, the **Add**
that splits into **Add unit**, **Add agent seat** and **Add human seat**;
**Edit** and **Delete** on every row but the company's, which cannot be
deleted; and a menu of the rest (**Open seat**, **Edit reports**, **Change
kind** and **Move to**). Where the builder
cannot write, the controls are disabled with the reason, never hidden.

Rows are listed in the chart's own order, by address. The chart keeps no other
order among a unit's seats or units, so there is nothing to reorder: where a
node sits is its unit, which **Move to** changes.

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

A check sends the write a save would send as a dry run (`?dry_run=true`), so
the engine answers exactly what the save would get — the org chart, its rules
and the settings together — and each problem is placed on the unit or seat it
names. A reference that names nothing is a warning. When the draft changes
nothing, the company is read instead, and a newer revision is found here
rather than at the first edit.

The check status beside the view controls says what it found:

| Status | Meaning |
|---|---|
| Checking | a check of the current draft is on its way |
| No problems | nothing the check can see stops the draft; the write itself is still decided where it lands |
| *N problems* | each problem is marked on the unit or seat it names, and problems about the whole company are listed above the chart |
| Could not reach the engine to check | the check got no answer; the builder retries with an increasing wait |
| The company changed | somebody saved a newer revision after this draft was started |
| Needs *grant* | the credential presented was accepted and does not carry the grant the refusal named; sign in as somebody who holds it |
| The engine refused the session | the session this browser holds was not accepted — it ended, or was revoked; sign in again to continue |
| Needs a credential | nobody is signed in; sign in |

A change is checked about a third of a second after it is made, so a burst of
changes (a held key, several undos) is checked once.

When the company has no model provider configured, a caution says so. The
engine applies such a company and places its seats, but no agent seat takes a
turn until a provider exists: work sent to a seat waits on its inbox and runs
once one is added
([A Company With No Model Provider](../concepts/configuration.md#a-company-with-no-model-provider)).
The builder does not write providers, and the dashboard does not add one:
Settings › Models & keys edits a model the configuration already declares. Add
one with a merge patch of `providers` to `PATCH /config`
([Configure via the API](configure-via-api.md)).

## Adding a unit or a seat

**Add unit**, **Add agent seat** and **Add human seat** on the company or a
unit (the **Add** on its branch or its row, its menu, or the toolbar) add the
new node to that unit, or at the top level of the company. On the structure
chart the form is drawn IN the chart, in the place the new node will take: the
chart makes room in that rank, draws the branch to it and eases onto it, so
what you are adding to stays in view while you name it. On the table and on
the reporting chart, which have no place to draw it, the same form opens as a
dialog. A name is prose, and two seats or two units may share one: every
reference names a seat by its **handle** and a unit by its **key**, and the
form asks for that address beside the name. It follows the name until you
type one, and it is never an address a node of the saved chart or the draft
holds — one this draft removes included. A human seat's contact identity is optional: the form
offers it (to a reader shown the runtime half), it can be added later in the
seat's editor, and without one the person is reached through the dashboard
only.

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

If the last check found something about the node, the problem is shown beside
the field it names. Problems that name no field in the editor are listed at
its top. A warning (a lead that names no seat, for example) does not stop a
save, so it is listed at the top as a caution, never shown as a field's error.

A node's **runtime half** is edited only by a reader the chart showed it to.
For anybody else the editor leaves those fields out and says why, and a save
leaves that half as it is on every node.

### The charter

| You can change | Notes |
|---|---|
| Name | Renaming the company asks you to confirm what it does. An agent seat's id is derived from the company name, so every agent seat gets a new id: what the engine keeps under the old id — each seat's mailbox and anything still waiting in it, its diary, its onboarding progress and its schedule ledger — is no longer read, and every agent seat starts over with an empty inbox and onboards again. Handles are unchanged. |
| Mission, vision, policies | Policies are an ordered list. |

Everything else in the settings (providers, integrations, workers, MCP
servers, sandbox and scheduling settings) is edited in the configuration
document, not in the builder.

A company has no lead. The reporting chart's roots are the seats no one
manages, so the seat at the top of a company is the one nothing else manages,
not a field somebody sets.

### A unit

| You can change | Notes |
|---|---|
| Name | Prose; two units may share a name. Onboarding pages are looked up under a unit's name, so the seats in it read the pages under the new one. |
| Key | The address every `manages` entry and seat placement names the unit by, and the unit's identity: a different key on a saved unit is a different unit, the old one removed and a new one added. |
| Type | One of the well-known types or a custom one. It is informational; an empty type is `team`. |
| Purpose, goals | |
| Lead | Any seat. The empty choice shows the lead the unit would inherit from the unit above it. |
| Channel | An empty channel inherits the one above it. Changing it takes `config:write`. |
| Knowledge | Free-text references, not a read scope. |
| Owns: project, knowledge space | Where unrouted work for the unit goes and where it files its own, whichever backend runs the tracker and the knowledge base. Not a permission. Changing either takes `config:write`, because a lead's authority is derived from them. |
| Schedules: enabled | Each schedule can be switched on or off. Runtime half. |

Shown and not changed: each schedule's cron, timezone, runner and task, and the
unit's tool credentials as server and variable names. They are in the runtime
half, which `crewlet config import` writes from a company file.

### A seat

| You can change | Notes |
|---|---|
| Name | Prose; two seats may share a name. An agent seat that is renamed keeps its handle, its memory and its mailbox. |
| Handle | The address every lead, `manages` entry and mention names the seat by, and the seat's identity: its mailbox, schedules and everything it has learned (its diary, episodes, skills, profiles of colleagues and thread history) are keyed on it. A different handle on a saved seat is a different seat: the old one is removed and a new one added, with an empty mailbox and no memory. |
| Email | The address inbound Jira and GitHub payloads identify the seat by. A field of the company document like the name, written as typed. |
| Goal, backstory, responsibilities | |
| Behavioral guidelines | Agent seats. |
| Manages | Seats and units. Seats this seat manages automatically as a unit's lead are listed apart, because the engine adds them whatever the list says. |
| Contact identities, availability | Human seats; runtime half. Contact identities are optional: a seat with none is reached through the dashboard only, and the configuration warns about it at the seat's `contact`. |
| Model | Agent seats; runtime half. An ordered chain of the company's `providers.llm` keys, tried in the order shown, which you reorder in place. A seat with no model runs on the provider keyed `default`, else the first provider in the company's order. |
| Token ceilings: daily, weekly, monthly | Agent seats; runtime half. One box per calendar window on the company's clock, each optional: an empty box is no ceiling on that window, and a 0 is refused rather than read as unlimited. A turn runs only while every capped window has room, and the company's own `token_budget` applies on top; a seat ceiling the company's makes unable to refuse anything is reported as a warning on the write's answer. |
| Schedules: enabled | Agent seats; runtime half. |
| Integrations | Agent seats; see below. |
| Owns: project, knowledge space | Where unrouted work for the seat goes and where it files its own. Not a permission. Changing either takes `config:write`. |

Shown with the reason they are not changed here: the models of the other
phases (`llm_review` and the other `llm_*` fields), sandbox (enabled, where it
runs), workers, placement, learning, tool credentials (names only) and whether
the seat is the Datadog fallback. They are in the runtime half, which
`crewlet config import` writes from a company file; sandbox, placement,
workers and tool credentials each depend on a settings block the builder does
not edit.

A seat's kind is changed with **Change to human seat** or **Change to agent
seat**, in its menu or in the editor, which is its own step because it
removes fields.

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

**Move to** moves a seat or a unit (with everything inside it) to another
unit, or to the top level of the company. A unit is never offered a
destination inside itself. Before you confirm, the dialog shows what the move
changes:

- who a moved seat reports to today, and whether that ends with the move: a
  unit's lead manages the unit's direct members, so a seat managed only that
  way stops reporting to the lead of the unit it leaves. This is the engine's
  description of the saved company, so it is shown only while the draft's
  chart is still the saved one;
- the lead of the destination, who manages its direct members unless another
  member manages the seat;
- the lead and the channel a moved unit, and the units inside it that declare
  none, would inherit instead;
- the agent seats that onboard again because the units above them change;
- the tool credential servers a moved agent seat gains or loses from its home
  unit's `mcp_env` (names only).

The review lists the move before you save.

A unit's lead is a seat's handle, not a position, so a seat that leads a unit
**stays its lead** wherever it moves. The dialog says so and offers **Clear
lead** to remove it as part of the move.

The dialog also names any unit schedule the move would leave with no runner (a
schedule for members needs a direct agent member, and a schedule for the lead
fails when the effective lead is a human seat), and says when a seat being
moved is working: its current turn continues on the organization it started
with until the engine applies the change.

## Deleting a node

**Delete** removes a seat, or a unit with every unit and seat inside it, from
the draft. Undo brings it back until the draft is saved. Once saved, a removal
is the one change nothing undoes: the chart **retires the address** and never
gives it to anything again. The dialog lists what the removal clears inside
the chart (a unit's lead, a `manages` entry), the unit schedules it would
leave with no runner, and the seats that are working now.

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
  because an entry left behind would be a setting about nobody.
- **Vendor identities and sealed credentials.** The seat's GitHub App, Slack
  app and Mattermost bot, the GitLab, Datadog and Atlassian accounts it is
  enrolled for (it holds a credential for that tool's `mcp_env` server, or its
  unit does), and the secret store entries its runtime half references, stay
  until you decommission them. They are listed by name, never by value, with
  links to **Settings › Integrations** and **Settings › Secrets**. A seat added in this draft was
  never saved, so nothing exists for it outside the chart.
- **The mailbox, coding runs and memory.** A removed agent seat's mailbox, and
  the mail still addressed to it, is kept for 24 hours after the engine
  applies the removal and then retired, together with any coding runs it still
  has. Its memory is kept, under an identity no later seat can have. See
  [Seat Ownership](../concepts/seat-ownership.md#the-removed-seat).

## Changing a seat's kind

**Change to human seat** and **Change to agent seat** are their own step,
because the change removes fields: the engine refuses a human seat every
field an agent runs on (models, token ceilings, workers, learning, schedules,
chat app blocks, tool credentials, behavioral guidelines and its own GitHub
App), and refuses an agent seat `contact` and `availability`. All of them are
in the runtime half, so a reader the chart did not show that half cannot
change a kind: nothing could say what would go. The dialog lists the fields by
name before anything is recorded, and calls out the ones that hold
credentials: the builder never shows a credential, so it cannot type one back
in and the value is gone for good once the change is saved. Removing a field
tears nothing down at a vendor, so the seat's apps, bots and accounts, and the
secret store entries the removed fields referenced, are listed as they are for
a deleted seat.

Becoming a human seat offers a contact identity without requiring one, and
cannot be done to the Datadog fallback (while Datadog is enabled) without
choosing the agent seat that takes over. Where the seat is the company's only
agent seat, the dialog says so and the change waits until another agent seat
exists or Datadog is disconnected. The schedules the change would strand, and
a turn the seat is running now, are named first. A seat that becomes human
stops running; its memory is kept but unused while it is a human seat.

## Reviewing and saving

**Review and save** opens once the draft has changes, and says what the save
would do before it does it:

- **What changes:** the units and seats added, removed, renamed, moved and
  edited, and the charter fields.
- **What follows:** the consequences a reader can work out from the chart's
  own rows: a new address for a seat or a unit (which keeps its identity, and
  whose old address goes on reaching it), seats that onboard again and why,
  where onboarding pages are looked up after a unit is renamed, the tool
  credentials a seat gains or loses (names only), fields a kind change
  removes, references a removal clears, the Datadog fallback seat and GitLab
  access levels. Who reports to whom, the lead and channel a unit inherits
  and where unrouted work goes are derived by the engine once the chart is
  saved, so they are not listed.
- **Warnings** the last check found.

A company rename, a kind change, a change of tool credentials and removing
more than half of the seats each need an acknowledgement before **Save**
enables. While the check reports problems the review opens so you can read
it, with Save disabled until they are fixed. While a check is out, Save waits
for it. If the engine could not be reached to check, saving is still allowed:
the write is decided where it lands.

### What a save sends

A save is one write of the company document — its units, its seats and its
settings together — made when you press Save:

- **In create mode**, `PUT /config` with `If-None-Match: *`, because that
  write is what makes the company exist.
- **In edit mode**, `PATCH /config`, a merge patch of what changed with
  `If-Match` naming the revision the draft was started from, so a save never
  writes over somebody else's change: a newer revision refuses it.

The write carries the save's own **write id** in its audit summary. The review
shows it as it goes, with what the engine answered:

| A write reads | Meaning |
|---|---|
| Written as revision `<id>` | the revision was stored and activated |
| Not confirmed (operation `<id>`) | nothing could establish whether it landed |
| Refused: *reason* | the engine will not take it as sent, naming the unit or seat the refusal is about |
| Somebody else's write got there first | a newer revision landed first |

The **audit summary** is prefilled from the changes and recorded with the
settings revision, followed by the write id, such as
`(write 4f1c9a0b2e7d4c81a3b5f6e7d8c9b0a1)`.

### When the answer does not arrive

A write whose answer is lost (a dropped connection, a gateway timeout) may
still have landed. The builder never assumes either way: the save is settled
from the revision history, and counts as done when a revision whose parent is
the draft's base carries the write id in its summary. If nothing was stored
the builder says so, and **Retry** sends it again under the same write id; if
somebody else's revision was stored instead, the draft is updated onto it, as
below; and while the engine cannot be asked, editing pauses with **Retry**.

The same holds when you leave the builder, or the tab reloads, before the
answers arrive. The kept draft is marked with the save's write id and the
nodes it creates before the first write is sent, so the next time the builder
opens in this tab it carries the draft onto whatever landed rather than
creating those nodes twice: the restore dialog says which changes are saved
already. Coming back to the builder while a save from this page is still on
its way, the builder waits for it, with editing paused, before it decides
anything about the kept draft.

### After saving

A save is not an apply. A settings revision is stored and activated, and every
node applies it on its own reconcile tick (about every fifteen seconds, spread
a little per node so a fleet does not apply at once); a node can refuse a
revision and go on serving the previous one. Until this node has applied it,
the org chart, Teams and Settings › General still draw the previous
organization, and say so.

The strip under the toolbar follows each half:

| It reads | Meaning |
|---|---|
| Saved settings revision `<id>`. The engine is applying it. | stored and activated; no node has reported this epoch yet |
| … Applied on N of M nodes. | the fleet is converging |
| … Applied. | every node reported the epoch, or applied the chart through the save's position |
| … Node `<id>` refused this revision: `<reason>`. | that node kept the previous epoch. **Open the fleet** (Settings › Nodes) for the rest |
| Saved the org chart at `<position>`. The nodes are applying it. | no node has reported applying the chart's log that far yet |
| Saved the org chart at `<position>`. Applied on this node. / This node is applying it. | what this node's own answer said, for a reader the fleet's positions are not shown to |

Beside it: **View changes**, which opens what the save changed in the settings
in Settings › Configuration (the saved revision against the one the draft was
started from; after creating the company it is **View the configuration**,
since a first revision has nothing to differ from), **Copy settings as YAML**,
which reads the active settings as YAML (credentials redacted, as every
configuration read is), and **Copy the chart**, which reads the org chart as
YAML — every unit, every seat and every `manages` list, its credentials
masked. They are what keeps a company file in a repository in step with what
the builder wrote.

### Keeping a company file in sync

A company kept as `company.yaml` in a repository and a company edited in the
builder are two writers of one company, and how the file is applied decides
which one wins
([Configuration](../concepts/configuration.md)):

- `crewlet run -company company.yaml` fills an empty store. Once a company
  exists the file is ignored, so a restart never undoes a builder save.
- `crewlet run -import-company company.yaml` makes the file the active
  revision again at every start, replacing what the builder saved.
- `crewlet config import company.yaml` through a running node writes the file
  as a new revision over the one the builder saved.

So after a save, bring the file in step with the stored company —
`crewlet config export` writes the whole document, the org chart included —
and commit it, and the next import writes back what the builder wrote rather
than undoing it. Keep every seat's `handle` and every unit's `id` in it: the
stored company holds the ones it minted, while a file that leaves them out is
minted afresh from its names on every import, so a name corrected there would
replace that seat or unit. Credentials in a redacted copy read as
`__redacted__`, or as the whole `${NAME}` reference they are stored under, and
importing the file back into the same deployment restores each masked value
from what is stored. Another deployment holds nothing to restore them from,
which is one more reason a file kept in a repository should hold `${NAME}`
references of your own rather than values.

## A draft survives a reload

The builder keeps the draft's list of changes (never the company itself) in
the tab's session storage, so a reload or a trip to another screen does not
lose the work. The list holds what you typed into it, so the storage belongs
to the tab and is emptied when you sign out — or when somebody else signs in
here after your session ended — and a kept draft is only ever offered back to
the person it was kept for. When the builder opens and finds a kept draft:

- **Made against the company as it still is** (the same settings revision and
  the same chart rows): a banner offers **Keep the draft** or **Discard it**,
  and nothing can be edited until you choose. Coming back to Edit org from
  another section of Agents restores the draft without asking.
- **Made against a company that has moved since:** the draft is restored
  through the same update described below, in a dialog titled **Restore the
  kept draft**, and **Discard the kept draft** removes it instead.
- **Made while a save from this tab was on its way:** it is restored as an
  update, never offered as it was, and the changes the save landed are marked
  "It is saved already."
- **Made for creating a company, where a company now exists** (or the other
  way around): it is discarded, and the builder says so — except a create
  draft whose own save was on its way, which is carried onto the company that
  save made.

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

Every check compares the draft's base with the company the engine holds, so a
change somebody else saved — another operator, `crewlet config import`, a
setup flow in Settings › Integrations — is found at the next check, not at the
save. The status reads
"The company changed", editing pauses, and a banner says which half moved
("Somebody changed the org chart since you started editing." or "The settings
changed since you started editing.") and offers **Update my draft**, with
**Show what changed** (the newer settings revision against the one the draft
was started from) where the settings moved. A builder with no changes on it
has nothing to update: it moves onto the newer company by itself and goes on.
An editor you have open keeps what you typed in it, and the selected unit or
seat stays selected, so an edit you had not applied yet applies to the newer
company.

Updating replays each change of the draft onto the company the engine holds
now, and sorts every change into one of four outcomes:

- **Still applies:** replayed as it was made.
- **Saved already:** the company already holds it — a save that landed part
  of the draft, or a colleague who made the same change.
- **Dropped:** what it changed no longer exists (the seat was removed, the unit
  it moved into is gone), with the reason.
- **Changed by somebody else as well:** a value it recorded was changed in the
  newer company. The dialog shows the value when you started, the value saved
  now and yours, and you choose **Keep mine** or **Keep theirs** for each. A
  change is never replayed over somebody else's value without that choice.
  Where a change is about a whole seat or unit (a removal) the dialog names
  the fields somebody changed, a position reads as where the node sits, an
  address somebody else now holds is named, and a kind change lists the
  fields it would remove by name; a credential is never shown, only that a
  literal value is set.

"The same seat" means what it means to the engine: a seat is its handle and
a unit is its key, neither of which an edit moves — that is what its memory,
mailbox and schedules are keyed on.

If this node still serves the older settings revision (it has not applied the
newer one yet, or a load balancer sent the read to a node that has not), the
update waits: "This node has not caught up with the newer settings revision
yet." Try again in a moment. If the configuration the draft edits is no
longer active at all, the banner offers **Discard and reload**.

## Undo, redo and the keyboard

Every change is an operation in the draft's log. **Undo** and **Redo** in the
toolbar, or <kbd>Ctrl</kbd>+<kbd>Z</kbd> and
<kbd>Shift</kbd>+<kbd>Ctrl</kbd>+<kbd>Z</kbd> (<kbd>Command</kbd> on a Mac),
walk the log from anywhere in the builder except a text field, where the same
keys undo typing. Each change is announced to screen readers (an undo or a
redo says it undid or redid the change), and focus moves to the unit or seat
it touched: an added node, the node before a deleted one (or its unit), a
moved node where it went. **Discard changes** throws the whole draft away
after a confirmation; the saved company is not touched. The keys of the chart
and the table are described under
[Reading the organization](#reading-the-organization).

At narrow widths Undo, Redo, Discard changes, Expand all and Collapse all move
into the **More** menu; the check status stays visible.

**Fullscreen**, where the browser offers it, takes the whole builder
(toolbar, chart, editor and dialogs) rather than the chart alone.
