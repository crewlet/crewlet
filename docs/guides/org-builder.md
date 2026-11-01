# The Org Builder

The dashboard's Org chart, under Company, has an **Edit org** section
(`#/company/org/edit`, the button on the org chart) for editing the
organization of a running company, and for creating the company on an engine
that has none. It edits the same company document `GET /config` serves and
`PATCH /config` writes, so every change it makes is an ordinary configuration
revision: stored, activated, audited, and applied by every node on its own
tick.

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
client of it needs: `admin`, to read it — credentials masked — and to save it,
a removal included, since it is one more change to the same document. A save
may ask you to confirm your password first, as every configuration write does.
Without `admin`, the lead of a unit edits the units they lead instead
([Editing as a unit lead](#editing-as-a-unit-lead)).

The credential is a person signed in holding `admin`, or an API token that
carries it, exchanged for a session on the dashboard's sign-in screen.
There is no posture in which it is not needed — `api.auth.disabled` is
retired, and `crewlet run -dev-principal` is a flag on a node reached over
loopback rather than a configuration a browser can meet. What the builder
shows is decided from what the engine answers, never from whether somebody is
signed in:

| The engine answers `GET /config` with | The builder shows |
|---|---|
| the active revision — the fleet's, which every node answers whatever it runs | the organization, ready to edit |
| `404 no_active_revision` — the fleet has no activation and this node knows no company — and the organization the node pushes names no company | creating the company |
| `404 no_active_revision`, while the organization names a company | "This node is still catching up". Try again shortly, or use another node |
| `503 unavailable`: the node could not read the fleet's activation from the coordination store | "The engine could not serve the configuration", saying so, with **Retry**. A coordination store electing a leader is out for a few seconds |
| `401` or `403`, to somebody signed in | "Reading the configuration needs Admin access.", with **Sign in as somebody else** |
| `401` or `403`, to nobody signed in | "Editing the organization needs you to sign in.", with **Sign in**, which comes back to the builder |
| a plain `404`, or an answer that is not JSON | "This process does not serve the configuration." The process has no configuration store; open the dashboard on a node running the engine |
| nothing | the engine could not be reached, with **Retry** |

A node that has not applied the fleet's revision yet — one that has just
joined, or one that refused it and runs the revision before — still answers
`GET /config` with the fleet's revision, so the builder edits the company
there as on any other node, and is never offered create mode while the fleet
runs one. A node whose engine runs a company while it answers that there is
none is not offered create mode either: a company exists, and a create would
replace it.

A node that is draining refuses every check and save with `503 draining`, and
one that is still starting with `503 starting`, and the builder treats either
as a node it could not reach: the draft is kept, and the check retries until a
peer or the node itself answers. Nothing is written by a refused write, so
there is nothing to settle afterwards.

Signing in as somebody else — in another tab, since the session cookie is the
browser's — while a draft is open keeps the draft on screen. The builder reads
the company again and checks the draft as the new reader, and forgets the
copy it kept for the last one; if the engine refuses it, editing pauses until
somebody who may edit it — an administrator, or its lead — signs in. Signing out empties the
tab's kept draft too.

When another system [manages the company
document](../concepts/configuration.md#managed-configuration) — Tier A names
its writers in `api.auth.company_writers` — the builder opens read-only for
every other token: the organization is drawn, every editing control is
disabled, and a callout says who manages it. Change the company at its source
and let that system write it. The org chart's **Add seat** is disabled with the
same sentence. A writer's token edits as usual.

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
| `unit=<key>` or `seat=<handle>`, and `project=<KEY>` | that node's editor on its **Work** tab, its tracker project set to the key and not yet applied: Worklet's **New project** continues here, and saving it is what creates the project. Setting a project takes `admin`, so a reader without it finds the field held with that reason |

It happens once, on arrival, and only when the builder can edit: once the
draft is loaded, and not while a kept draft waits for **Keep** or
**Discard**, a save's outcome is unknown or the company changed under the
draft. The request waits through each of those
and opens once they are settled. `add=` and `project=` are then taken out of
the address, so a reload or a copied link does not ask a second time; `seat=` and `unit=`
stay, because they are also the selection (see
[Selecting a node](#selecting-a-node)). Selecting another node inside the
builder, or a Back to an earlier selection while you stay in it, opens
nothing. But because the selection is part of the address, ANY arrival that
carries one is read the same way as a link: a reload with a node selected, or
a Back into Edit org from another screen (a seat's profile reached from the
node's **Open seat**, say), opens that node's editor, since the builder cannot
tell that address from a link somebody sent you. A link naming an address the
draft does not hold selects nothing and opens nothing.

The company's mission, vision and standing policies are not opened here: they
are written on **Company › Principles**, and every way into them links there
(see [The company](#the-company)).

## Creating the company

On an engine with no active configuration the builder opens on a form: the
company's name, a shape to start from, and a seat for yourself:
**Add a seat for yourself**, ticked from the start and named "Founder" (rename
it for the role; the seat outlives whoever holds it), with an optional contact
identity for it.

**Leave that seat ticked unless agents alone will run the company.** Every
person holds exactly one human seat for as long as they exist, and is put on
it by an invitation or an administrator's create that names it — so the seat
you add here is the one you put yourself on once the company exists: sign in
with the deployment's API token, open **Company › Org chart** and press
**Invite** or **Add person** on it (see [How the first person
exists](../concepts/identity-and-access.md#how-the-first-person-exists)). A
company that declares no human seat can admit nobody, the operator included,
until one is added, and its check warns about that at `roles`. Unticking the
box is still a legitimate answer: a company its agents run alone, administered
through the deployment's Tier A tokens, which act as themselves and need no
seat.

| Start from | What it writes |
|---|---|
| New company | A chief executive and three units (Engineering, Product, Marketing), each with one agent seat reporting to the chief executive |
| Established company | A chief executive, Engineering with a Reliability team, Product with a Design team, and Go to Market, with each unit's lead reporting up the chain |
| Start empty | The company alone |

The Established company template asks whether unit leads are **agents** (seats
the engine runs) or **people** (human seats). A template never invents a
contact identity, so human leads are created without one, and the only identity
written is the one you type for your own seat — which is optional too: leave it
empty if you will work only through the dashboard. A human seat with no contact
identity is legitimate. On the engine's own chat an agent mentions a person by
their seat's handle — a lead's roster gives each human report as `@handle`, and
the note on human colleagues says to mention them there — so nothing is missing;
on a company that talks on Mattermost or Slack, nothing addressed to the seat
there reaches anybody, so
the configuration warns about it at the seat's `contact` — and the review lists
that warning with the others — until an id on that workspace is added.

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

After a successful create, three steps remain. **Open Principles** opens
Company › Principles in edit, to write the mission, the vision and the standing
policies every agent reads. The builder cannot take the other two: connecting
chat and trackers in Company › Integrations, and adding a model provider. The
engine applies the new company without one, but until the provider is added no
agent seat takes a turn: whatever is sent to a seat waits on its inbox and runs
once the provider exists. **Add a model** opens
[Settings › Models](../concepts/llm-backends.md#managing-models-from-the-dashboard)
on its wizard, which adds the model, signs it in and tests it; the panel also
gives the terminal steps for it, folded, which
[Giving the company a model](#giving-the-company-a-model) explains. On a company
another system [manages](../concepts/configuration.md#managed-configuration),
it says the model is added there instead.

### Giving the company a model

Every model is a [coding CLI](../concepts/llm-backends.md) the engine drives.
[Settings › Models](../concepts/llm-backends.md#managing-models-from-the-dashboard)
adds one from the dashboard, signed in and tested. To add it at the node's host
instead, in a terminal beside the node's `crewlet.yaml`: the company the builder
created is in the store and in no file, so the steps start by writing it to
one. The builder's panel after a create gives them, and so does its caution
while the company has no model:

```bash
# Stop crewlet run first (Ctrl+C), and run these beside its
# crewlet.yaml: the export reads the company out of the store,
# which a running node holds.

# 1. Write the running company to a file
crewlet config export > company.yaml

# 2. Add the model to it, and the sandbox every seat's executor
#    runs in: here this machine, as your user
providers:
  llm:
    default:
      model: sonnet
  sandbox:
    local: {}
    default_run_in: direct

# 3. Sign its CLI in to your subscription, and check it end to end
#    (on a member of a cluster, add -api with a running member's URL
#    and a Tier A token in CREWLET_API_TOKEN: a member alone cannot
#    reach the fleet's store)
crewlet llm login default -capture-token
crewlet llm doctor default

# 4. Import the file, and start the node again
crewlet config import company.yaml
crewlet run
```

- **The node is stopped first** because `crewlet config export` reads the
  company out of the node's own store, and a running node holds it: one
  process at a time. Run the steps on the host and in the directory of the
  node that holds the store. `crewlet llm` itself would run beside the node,
  reading the stored values through it, but the export it follows would not.
  With the node stopped, `crewlet llm` reaches the fleet's secret store
  itself: on a solo node it starts the node's own broker on its
  `stream.store_dir`, listening on nothing, and stops it when the command
  ends, and on an external broker it dials it. A member of a cluster
  cannot — alone it never reaches the quorum the store needs — so there
  step 3 runs with `-api` naming a member that is running, with one of its
  Tier A tokens in `CREWLET_API_TOKEN`.
- **The file comes from `crewlet config export`**, the company the node's
  store knows — the fleet's revision as the node last saw it, or an offline
  change made on it since — as it is stored, rather than from
  `GET /config?format=yaml` or **Copy as YAML**: those are redacted, and
  `crewlet llm` refuses a company file that still carries a `__redacted__`
  mask — the import would restore one, but `crewlet llm` writes no revision
  and restores nothing. A `${NAME}` reference comes out as the reference, and
  a credential written inline comes out in the clear (a company the builder
  created holds none).
- **The entry needs only its model.** `default` drives Claude Code, the
  default CLI, on your subscription; `-capture-token` mints a headless token
  and stores it in the fleet's [secret store](../concepts/secret-store.md)
  under the CLI's own token variable, which an entry that names no
  `auth.token` reads — written there directly with the node stopped, so the
  row's source is `llm-login` and its author your shell's user, and the node
  reads it when it starts.
  `crewlet llm doctor` then makes a real call with a tool to prove the CLI is
  installed, signed in and still matches the profile Crewlet drives it with.
  For another CLI or an API key, write the entry
  [LLM Backends](../concepts/llm-backends.md) describes instead.
- **The sandbox is this machine.** A `direct` box runs the CLI as your user
  on the node's own host, and calls the seat's tools back through the node's
  own listener on loopback, so there is nothing else to set — the node only
  needs its `api.port`, whatever its roles. A `container` or `e2b` cell, or a
  seats node started with `api.port: 0`, instead needs
  `CREWLET_MCP_BRIDGE_URL`, the fleet's address as that box reaches it (see
  [Code Sandbox](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box)).
  The CLI runs fully permissioned there, so `direct` is for a first run on
  your own machine while its seats take work from you alone, a dedicated host
  you are willing to hand to an autonomous agent, or the entry that needs the
  host itself; before a seat takes work from anyone else, place the runs in
  `container` with [an image of your
  own](../concepts/code-sandbox.md#bring-your-own-image) (see [Choosing a
  cell](../concepts/code-sandbox.md#choosing-a-cell)).
- **The import is offline**, with the node stopped: it makes the file what
  the node boots on, built on the fleet's revision the export came from, and
  the node publishes it to the fleet as it starts — as long as the fleet
  still runs that revision. The work that waited on the seats' inboxes runs
  on the new model. If another node changed the company while this one was
  stopped, the import is withheld rather than published over that change:
  the node starts on it, logs `offline_revision_not_published` at warn, and
  moves to the fleet's revision once it applies it. Add the model to the
  fleet's company through a running node then: a `PATCH /config` carrying
  only step 2's `providers` block — its `llm` entry, and its `sandbox` block
  unless the fleet's company already has one, since a company with an agent
  seat and a model is refused without it — which is built on the fleet's
  revision and keeps the other node's change. Importing `company.yaml` with
  `-api` would replace the whole document with the one exported before that
  change. See [Offline commands make an
  offer](../concepts/configuration.md#offline-commands-make-an-offer).

None of this applies to a company another system
[manages](../concepts/configuration.md#managed-configuration): an offline
import of a managed document is refused, so the model is added at the
company's source like any other change, and the builder says so instead of
giving these steps.

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

The builder draws the organization the way the console's org chart does. An
agent seat is its own Crewlet character on a card toned in its own colour,
with the branch arriving at it and its table row in the same hue; a person's
seat is a person's glyph in a dashed ring; a unit and the company are their
own glyphs; and every node but an agent seat is drawn on the same neutral
surface. An agent's character and colour are the ones its author chose (see
[A seat](#a-seat)), the original Crewlet in purple until somebody does: they
are never a hash of a name, so a renamed seat keeps them. The
selected node's card takes the accent on its own edge. The live org chart
(Company › Org chart) draws the same nodes, marks and hue, with what each seat
is doing under its name, and both are drawn on the same dotted field, which
says it pans before you drag it.

### The structure chart

The company is the root. The seats declared at the top level and the units
hang off it, and each unit's seats and child units hang off that unit: every
seat is a node of its own, joined to the unit it sits in by the same
orthogonal branch the live org chart draws, at the weight the design system
gives a chart of nodes. A node shows its mark (an
agent's figure, a person's ringed glyph, or a unit's or the company's glyph),
its name and, under it, what kind of thing it
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
**Delete** down its end (the company has neither, see
[The company](#the-company)), and, under a node that can take a child, the **Add**
on the branch its children hang from, which splits into **Add unit**, **Add
agent seat** and **Add human seat** where it stands.

Every node is one stop in a tree. The arrow keys walk the visible order: Right
opens a closed unit or steps into an open one, Left closes it or climbs to the
unit above, Home and End jump, and typing a name moves to the next node it
matches. On a unit or a seat, Enter or a double-click opens its editor (on the
company they do nothing), Delete or Backspace deletes it (never the company), Alt+Up and Alt+Down move it among its siblings, as they
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
**Edit** and **Delete** on every row but the company's, which has no Edit and
cannot be deleted; and a menu of the rest (**Open seat**, **Edit reports**,
**Move to** and the two moves among the siblings, and **Rename company** on the
company's row). Where the builder
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
inside the builder never does. Selecting the company itself carries its
**Edit principles**, **Rename company** and the same **Add** menu (see
[The company](#the-company)); it names no filter, because the
builder is already about that company. Where the builder cannot write (a
guarded or read-only posture, or a draft waiting to be updated) the actions
stay in the menu and are marked unavailable, so what the builder does is still
legible.

A click on empty space puts the selection down: on the chart's field, in the
space round the table, or on the page under either. The node's card or row
lets go of the accent, the URL drops it and the toolbar's actions with it. A
click on a node, a row or a control is that thing's own, nothing inside a
menu or an editor counts, and dragging the chart to pan it keeps the
selection.

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
| Needs Admin access | the session was accepted and does not carry `admin`; sign in as somebody who holds it |
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
The builder does not write providers: the caution sends the reader to
[Settings › Models](../concepts/llm-backends.md#managing-models-from-the-dashboard),
where a model is added, signed in and tested, and folds away the steps that add
one from a terminal at the node's host, starting from the company the store
holds ([Giving the company a model](#giving-the-company-a-model)).
On a company another system [manages](../concepts/configuration.md#managed-configuration)
it gives no steps and says the model is added there, by whoever reads it, a
writer the document names included: the steps end in an offline
`crewlet config import`, which a managed document refuses, and the managing
system replaces any revision written around it at its next reconcile. The
model is added at the company's source, like every other change to it: the
repository a pipeline applies, or the resources an operator renders it from.
The caution waits while the panel a create leaves is on screen, which says the
same and gives the same steps. Any write of the document adds the entry, `PATCH /config`
included ([Configure via the API](configure-via-api.md)); what only a terminal
does is sign its CLI in, and `crewlet llm` reads the entry it signs in from a
company file.

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
one the person is reached through the dashboard only. An agent seat is added
with a profile icon: the form suggests the first Crewlet character no agent in
the draft wears yet and the next colour round the six, so a company growing
one agent at a time is told apart before anybody has chosen, and you can pick
another character and colour before adding it.

**A new human seat is vacant until somebody is put on it**, which no save
does: who holds a seat is the identity directory's, not the company
document's. Once the save is applied, the seat's card in **Company › Org
chart** reads **Open seat** — to everybody, since who holds a seat is the
overview's — and an administrator invites somebody onto it or creates a person on
it from there, its peek, its page or the **Person** section of its editor's
**Profile** tab — see [Putting a person in the
seat](../concepts/humans-in-the-org.md#putting-a-person-in-the-seat). That
section shows who holds the seat, by name, to everybody, with **Invite**,
**Add person** and **Cancel invitation** for an administrator alone. Those gestures write the identity directory at
once, about the seat the running company holds, so the draft's **Apply** and
**Undo** reach none of them — and a seat added in the draft is filled only
once it is saved, since the directory knows only the seats a saved revision
declares.

## Editing a node

Choose **Edit** on a unit or a seat to open its editor at the side
of the chart. The editor's head shows the node's mark (an agent's profile
icon, which opens its picker), its name as you type it, and under the name the
seat's handle and kind (or the unit's type); where the node sits in the
organization is on its **Work** or **Leadership** tab rather than in the
head. **Cancel** and **Apply** stay in the head while the form scrolls.

A unit's and a seat's fields are split into tabs, so a long form reads as
parts:

| Node | Tabs |
|---|---|
| Agent seat | **Profile** (name, email, handle, kind, profile icon, goal, backstory, responsibilities, behavioral guidelines), **Work** (manages, owns, schedules), **Model** (providers and token ceilings), **Integrations** (one card per connected tool), **Advanced** (what the configuration document sets, and Delete) |
| Human seat | **Profile** (name, email, handle and kind; the **Person** who holds the seat; and the role: goal, backstory, responsibilities), **Contact** (contact identities and availability), **Work** (manages, and Delete) |
| Unit | **Profile** (name, key, type, purpose, goals), **Leadership** (lead and channel), **Work** (owns, references, schedules), **Advanced** (tool credentials, and Delete) |

The company's name form is one field, with no tabs. A tab carries one marker, the most
urgent: the number of problems in it, else a warning, else a dot for changes
you have made there and not applied. A field that stops **Apply** marks its
tab the same way, and pressing Enter in any field opens the tab that holds the
reason.

A field's help line shows while the field has focus, so the form reads as its
labels and values. A value the builder shows and does not edit is a locked
row: hover it, or read it with a screen reader, for the reason and where it is
changed instead.

**Edit reports** opens a seat's editor on **Work** at **Manages**, and a lead
chip's **Choose another seat** opens a unit's on **Leadership** at **Lead**, so
an action about one field starts on that field. The editor holds your changes in its
own form until you press **Apply**, which adds them to the draft as **one
step**: one Undo takes the whole edit back. Closing an editor that holds
changes (Cancel, Escape or a click outside it) asks before discarding
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

### The company

The company's **mission**, **vision** and **standing policies** are written on
**Company › Principles**, in place, and nowhere else. So the company's node
carries no **Edit**: no pencil on its card or its row, and Enter or a
double-click on it does nothing. Select the company and the toolbar's menu
offers **Edit principles**, which leaves the builder for that page, opened in
edit for a reader who may write the principles. A reader who may not is offered
**Open principles**, which opens the same page to read. Your draft is kept for when you come back (see
[A draft survives a reload](#a-draft-survives-a-reload)); if the principles
were saved meanwhile, your next save meets the newer revision and is
[updated onto it](#when-somebody-else-saves-first) like any other.
The builder never records or sends a change to the principles, so a save from
here cannot put an older copy of them back.

What the builder writes about the company itself is its name, through
**Rename company** in the company's menu (on its card and its row, and in the
toolbar's menu while it is selected):

| You can change | Notes |
|---|---|
| Name | Renaming the company asks you to confirm what it does. An agent seat's id is derived from the company name and its handle, so every agent seat gets a new id: each seat's diary and onboarding progress stay under the old id and are no longer read, and every agent seat onboards again. Handles, mailboxes and episodes are unchanged. |

While you are [creating the company](#creating-the-company) it has no
Principles page yet, so its menu offers **Rename company** alone, which opens
the name form in the builder; write the principles once it is created, from the
next steps the builder shows.

A problem the engine reports with the mission, the vision or a policy is listed
with the problems about the whole configuration, with **Open Principles**
beside it.

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
| Type | A row of chips, one per type the engine knows (division, department, group, team, squad, pod, guild, chapter, unit), and **Custom**, which opens a box in the row for any other name. It is informational; a unit that names no type is a `team`, so Team shows as chosen. |
| Purpose, goals | |
| Lead | Any seat. The empty choice shows the lead the unit inherits from the unit above it, as the last check reported it. |
| Channel | An empty channel inherits the one above it. |
| References | The unit's `knowledge` list: free-text references, not a read scope. |
| Owns: tracker project, knowledge space | The unit's `project` and `space`: where unrouted work for the unit goes and where it files its own. Not a permission. Each names a container on whichever tracker and knowledge base the company runs, the engine's own included, so each is shown unless the company runs none (`tracker.backend: none`, `knowledge.backend: none`). Changing either takes `admin`, because a lead's authority is derived from them — and a `space` is where pages may be created at all. |
| Schedules: enabled | Each schedule can be switched on or off. |

Shown and not changed: each schedule's cron, timezone, runner and task, and the
unit's tool credentials as server and variable names. Schedules and tool
credentials are written in the configuration document.

### A seat

| You can change | Notes |
|---|---|
| Name | Prose; two seats may share a name. An existing seat keeps its handle through a rename, and with it its memory and mailbox, but an agent seat that is renamed onboards again: its onboarding progress is stamped with its own name and the names of the units above it. |
| Handle | The address every lead, `manages` entry and mention names the seat by, and the seat's identity: its mailbox, schedules and everything it has learned (its diary, episodes, skills, profiles of colleagues and thread history) are keyed on it. Editable on a seat this draft added, and shown and not edited on a saved one, where **Replace this seat…** gives the role another handle (see [Replacing a seat](#replacing-a-seat)). |
| Kind | Never. A seat keeps the kind it was added with; see [A seat keeps its kind](#a-seat-keeps-its-kind). |
| Profile icon: character, color | Agent seats. One of the thirty Crewlet characters and one of six colours, written to the seat's `avatar`. The chosen character and colour are the seat's badge on every screen and its node on both charts. A seat that never chose is drawn as the original Crewlet in purple, and changing only one of the two writes only that one. |
| Email | The address inbound Jira and GitHub payloads identify the seat by, written as typed. |
| Goal, backstory, responsibilities | |
| Behavioral guidelines | Agent seats. |
| Manages | Seats and units. Seats this seat manages automatically as a unit's lead are listed apart, because the engine adds them whatever the list says. |
| Contact identities, availability | Human seats. Contact identities are optional: a seat with none is reached through the dashboard only, and the configuration warns about it at the seat's `contact`. |
| Providers, in order | Agent seats. An ordered chain of the company's `providers.llm` keys, tried in the order shown; each chip moves earlier or later in place. A seat with no providers runs on the provider keyed `default`, else the first provider in the company's order. A per-phase mapping is shown and edited in the configuration document. |
| Token ceilings: daily, weekly, monthly | Agent seats. One box per calendar window on the company's clock, each optional: an empty box is no ceiling on that window, and a 0 is refused rather than read as unlimited. A turn runs only while every capped window has room, and the company's own `token_budget` applies on top. The check warns about a ceiling that can never refuse anything — a week at or above seven days of the daily one, a seat at or above the company. |
| Schedules: enabled | Agent seats. |
| Integrations | Agent seats; see below. |
| Owns: tracker project, knowledge space | Agent seats. The seat's own `project` and `space`: where unrouted work for the seat goes and where it files its own. Not a permission. Shown unless the company runs no tracker or no knowledge base, as for a unit. Changing either takes `admin`. |

Shown with the reason they are not changed here: per-phase models
(`llm_review` and the other `llm_*` fields), the executor box (the seat's
setup steps, variables, tool servers and turn cap; where it runs is its model
entry's `run_in`), workers, placement, learning, tool credentials (names only)
and whether the seat is the Datadog fallback. The executor box, placement,
workers and tool credentials each depend on a company-level block the builder
does not edit.

**Delete seat** is on the editor's last tab (**Advanced** for an agent seat,
**Work** for a human seat) and opens the same dialog as the chart's Delete.

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

Each tool the company has connected is a card with the tool's own mark, its
name and its state in the **Integrations** screen's words and colour:
**Connected**, **Action needed** (or **Credential expiring**), **Connecting**,
**Paused** or **Status unavailable**. That state is the company's, one word
per tool everywhere. What is this seat's own (its GitHub app, and whether that
app is installed yet) is a row inside the card, never a second status. The
tools the company does not use are one line under the cards, with a link to
**Integrations**. No credential is ever shown.

- **GitHub:** access tier and repositories (an empty list means every
  repository the installation covers). Setting a tier on a seat with no GitHub
  block enrols the seat in GitHub; create its app from Integrations. A seat's
  app permissions are fixed when the app is created, so after changing the tier
  of a seat whose app exists, raise the app's permissions at GitHub as well.
- **Slack:** whether the seat speaks as its own Slack app. A seat's app has no channel of its own: it speaks in its unit's `channel`.
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

Two more things the builder deliberately does not have. A colour nobody
chose: an agent seat is drawn in the colour its profile icon names and a
person's seat and a unit in none, so no hue is ever made from a name. And the
zoom is the chart's own:
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

A human seat that something holds cannot be removed while it does — a
person on it at any stage short of removal (a suspended or retired person
keeps their seat), a service account bound to it, or an open invitation that
names it. The engine refuses the save with `409 seat_held`, naming the one
thing holding each such seat, and the builder places that refusal on the seat
with the remedy for that holder, then keeps the draft:

| What holds the seat | The builder says |
|---|---|
| A person | "@pat is held by pat.doe: move them to another human seat (`crewlet iam bind <person id> SEAT`) or remove them (`crewlet iam remove <person id>`) first, then save again" |
| A service account | "@pat is held by the service account ci:release: unbind it first (`crewlet iam unbind <person id>`), then save again" |
| An open invitation | "@pat is held by the open invitation <id>: cancel it first (`crewlet iam cancel-invite <id>`), then save again" — never the invitee's address, since the editor may be a unit's lead with no grant over the directory |

A person is never unbound: they hold a human seat for as long as they exist,
so the engine refuses an unbind of a person, and moving them or removing them
is what frees it. Each remedy is an administrator's to make, and there
is nothing to retry until it has landed; the seat's page in the org chart says
what holds it. The same refusal meets a held seat replaced
([Replacing a seat](#replacing-a-seat)), and a document written outside the
builder that makes a held human seat an agent's.

A node that cannot read who holds the seats a change removes refuses it
`503 seat_holders_unavailable`, and nothing is written. Where waiting clears
it (the engine names a wait), the check asks again by itself and reads *Could
not reach the engine to check* meanwhile; where waiting does not (the node has
been evicted, or its identity log is full), each seat the change removes shows
the engine's own sentence for what clears it, and stays so until the draft
changes or the node can read them again.

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
  **Company › Integrations** and **Settings › Secrets**. A seat added in this draft was never saved,
  so nothing exists for it outside the chart.
- **The mailbox, executor and memory.** A removed agent seat's mailbox, and
  the mail still addressed to it, is kept for 24 hours after the engine applies
  the change and then retired, together with its executor on any turn it still has out. Its
  memory (diary, episodes, counterparty profiles, onboarding markers) is kept,
  and a seat added later under the same handle reattaches to it. See
  [Seat Ownership](../concepts/seat-ownership.md#the-removed-seat).

## A seat keeps its kind

The builder does not change a seat from an agent to a person or back. The two
kinds share almost nothing: the engine refuses a human seat every runtime field
(models, sandbox, token budget, workers, learning, schedules, placement, chat
app blocks, the project and space it owns, tool credentials, behavioral
guidelines, its own GitHub App and its profile icon) and refuses an agent seat
`contact` and `availability`, and an agent seat's memory, mailbox and vendor
accounts belong to the agent it was. The editor shows the kind as a locked row.
To make a seat the other kind, delete it and add a new seat of the kind you
want; the delete dialog lists what the removal leaves behind. A draft never
gives a new seat an address it removes, so the new seat takes another handle,
or the same one in a later draft, where it reattaches to the memory the handle
kept. A human seat somebody holds is deleted only once nothing holds it
([Deleting a node](#deleting-a-node)).

## Reviewing and saving

**Review and save** opens once the draft has changes, and says what the save
would do before it does it:

- **What changes:** the units and seats added, removed, renamed, moved and
  edited, and the company's new name when the save renames it.
- **What follows:** the consequences the engine attaches to those changes,
  computed from the hierarchy it derives for the base and for the draft:
  a replaced seat (listed as the old seat removed and the new one added,
  since the new seat carries none of the old one's memory or mail), seats
  that onboard again (and why), reporting lines and leads that move
  (including a reorder that only changes which manager comes first),
  channels, where unrouted work goes, the tool credentials a seat gains or
  loses (names only), credentials a replaced seat leaves behind, references a removal clears, the Datadog fallback seat
  and GitLab access levels.
- **Warnings** the engine gave for exactly this save.

A company rename, a change of tool credentials (a credential a replacement
leaves behind included) and removing more than half of the seats each need an
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
parent is the draft's base and whose summary carries the write id. It walks
back from the active revision to find it, so it finds a save somebody has
built on since as well: the builder then says "Saved. Revision `<id>`, built
on it, is active now.", naming that later revision, which every node applies
with your changes in it. If the save did not land, it says so and offers to
save again. If the engine cannot be asked, editing pauses and the review offers **Check again** and **Save
again**; a second save carries the same write id, so a first save that did
land is recognized as yours rather than replayed on top of itself.

The same holds when you leave the builder, or the tab reloads, before the
answer arrives. The kept draft is marked with the save's write id before the
save is sent, so the next time the builder opens in this tab it settles that
save first: if it landed, the builder says "Your last save was stored and is
being applied" — or, when a revision built on it is active by then, "Your
last save was stored. Revision `<id>`, built on it, is active now." — and
does not offer the draft again (replaying it onto its own revision would
apply every change twice); if it did not, the draft comes back
as any kept draft does. While the engine still cannot say, editing stays
paused, with **Check again**. Coming back to the builder while that save is
still on its way, the builder waits for the save's own answer before it
settles anything.

### After saving

A save stores and activates a revision. It does not apply it: every node
applies on its own reconcile tick (about every fifteen seconds, spread a
little per node so a fleet does not apply at once), and a node can refuse a
revision and go on serving the previous one — and start on that one again if
it restarts. Until this node has applied it, the org chart and Teams still
draw the previous organization, and say so: *Still applying* while it may yet,
and *This node refused* the revision once its attempt failed, since it runs
the one before until a later revision is activated. The configuration itself answers
the saved revision at once, on every node: `GET /config`, the next check and
**Copy as YAML** read it whether or not the node serving them has applied it.

The strip under the toolbar follows the revision:

| It reads | Meaning |
|---|---|
| Saved revision `<id>`. The engine is applying it. | stored and activated; no node has reported this epoch yet |
| Applied on N of M nodes. | the fleet is converging: N nodes have applied it |
| Applied. | every node has applied it: each reported this revision applied at this epoch, runs it, or has applied a later epoch |
| Node `<id>` refused this revision: `<reason>`. | that node goes on running the revision it ran before. **Open the fleet** for the rest |
| This node refused this revision and goes on running the one before. | a unit lead's view, which reads no fleet: the node serving the dashboard attempted the revision and failed. Why is on **Settings › Nodes**, which is `admin`'s |
| Revision `<id>` has been activated since. | another revision was activated before every node had applied yours — often one built on it, carrying your changes. Every node has either applied yours or gone straight to the later one without running yours (two activations inside one reconcile tick are never attempted one by one) and failed to apply it there. Said too of a save whose answer was lost, found afterwards, by when another revision had been activated. The strip stops following it; **Settings › Nodes** shows how the nodes take the newer one |
| Whether it has been applied can't be followed from here. The org chart shows its changes once this node applies them. | a unit lead's save whose answer was lost. Without `admin` the strip reads no fleet, and the save carries no epoch to compare this node's against until this node attempts the saved revision, when the strip follows it from there |

Beside it: **View changes**, which opens what the save changed on the
Configuration screen (the saved revision against the one the draft was
started from; after creating the company it is **View the configuration**,
since a first revision has nothing to differ from), and **Copy as YAML**,
which reads the active company as YAML (credentials redacted, as every
configuration read is) so a `company.yaml` kept in a repository can be
brought back in step. `crewlet config import company.yaml` writes it back.
Both read the company document, so a lead without `admin` is offered
neither.

Without `admin`, the strip follows a save by the node serving the dashboard
alone: its health push says the epoch it applied and what its last attempt
made of which revision, so a refusal there is said as one rather than as an
apply that never ends. With `admin`, each node's row in the fleet view is read
by the same rule, since the row carries the epoch the node last applied beside
the one it last attempted, as the push does: a node that moved past the save's
epoch has applied the save once it has applied an epoch at or past it — even
if it has refused a still later one since — or when it runs the saved
revision, which only the row says. While the dashboard has lost its connection
to the engine, the strip and the org chart's note keep what the node last said
about the save rather than going back to saying it is being applied, and so
does a screen opened during the outage: Teams after the chart, or the Builder
after another page.

A node that restarts forgets the epoch it applied, on its push and on its row
alike: a node with the `data` role boots on the revision it applied before,
and reports no applied epoch until it applies one again. So such a node that
restarted and then refused a later revision reads as having gone past your
save rather than as having applied it. (A node without `data` boots on
nothing and runs no company until it applies one, so for it that reading is
simply true.) The one exception is the strip of somebody with `admin`, for a
node that booted on your revision itself: its row says it runs it. A verdict
the strip gave before the restart can then change from **Applied.** to
*Revision `<id>` has been activated since.*, and the org chart's note can
come back as *This node did not apply* your revision although it did.
**Settings › Nodes** shows the revision each node runs. Neither the push nor
the row says which activation a restarted node booted on, so the dashboard
cannot tell such a node from one that never ran your revision; carrying that
across a restart belongs with
[#322](https://github.com/crewlet/crewlet/issues/322), an activation's
identity across pointer lifetimes.

A save whose answer was lost ([When the answer does not
arrive](#when-the-answer-does-not-arrive)) carries no epoch of its own. The
strip takes the one in the fleet's activation, and only while that activation
names the saved revision (for a lead, the one in this node's last attempt,
only while it names the saved revision); while the node cannot read the activation from the
coordination store, it goes on saying the engine is applying it and keeps
asking, rather than counting every node as past an epoch it does not know.
Once either names it, the tab keeps that epoch for the save, so a revision
activated afterwards does not turn an applied or refused save back into one
that cannot be followed; the org chart's note follows the save from then on
too, and learns the epoch from this node's last attempt itself when the
Builder is not open.

### Keeping a company file in sync

A company kept as `company.yaml` in a repository and a company edited in the
builder are two writers of one document, and the flag a node starts with
decides which one wins a restart
([Configuration](../concepts/configuration.md)):

- `crewlet run -company company.yaml` only creates a company where none
  exists. Once one does — the node boots on it or has seen the fleet run it,
  or the fleet runs one — the file is ignored, so a restart never undoes a
  builder save.
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
from the company by the identity of what holds it (a seat by its handle, a
unit by its key): `crewlet config import` through a running node restores
from the fleet's revision; offline, and a node starting on the file with
either flag, from the company the node's own store knows — the fleet's
revision as it last saw it, or an offline change made on it since. A store
that knows no company has nothing to restore from, and a member that is new
or renamed in the file has nothing of its own there, so either is refused
naming the field; that is one more reason a file kept in a repository should
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
  Coming back to Edit org from another section of the Org chart restores the draft
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
setup flow in Company › Integrations) is found at the next check, not at the
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
  the fields somebody changed and a position reads as where the node sits; a
  credential is never shown, only that a literal value is set.

"The same seat" means what it means to the engine: a seat is its handle and a
unit is its key, because those are what its memory, mailbox, schedules and
credentials attach to. A seat removed and created again with a different
handle is a different seat, and a change made to the original is dropped
rather than applied to it. One created again under the same handle, or a unit
under the same key, is the same one to the engine, so a change made to it is
held to the values it recorded like any other: a field somebody set
differently is a conflict for you to decide.

Every node answers `GET /config` with the fleet's revision, so the update
reads the newer revision wherever the request lands, on a node that has not
applied it yet too. If the revision it reads is still the draft's own, or one
the builder cannot trace back to the newer revision through its parents, the
update waits: "Still catching up. Try again shortly." If the configuration the
draft edits is no longer active at all, the banner offers **Discard and
reload**.

## Editing as a unit lead

The lead of a unit edits that unit and everything inside it without
`admin`: its own fields, its seats, the units inside it and theirs,
seats added, moved between its teams, reordered or removed. A lead is the seat a unit
names as its lead, or inherits from the unit above it when it names none, so
leading a unit is leading every unit inside it, a sub-unit with a lead of its
own included. The person leads through the human seat they hold — every
person holds one — and needs no permission for it; an administrator edits the
whole company as above, units they lead included.

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

- the company's name, its settings and its top level, and the unit's own
  lead and place (a move or a deletion), which decide who leads it. The
  builder draws these controls disabled with the reason, and the Lead of a
  unit inside it stays the lead's to change;
- a lead or a `manages` entry naming a seat outside the units the caller leads;
- a value another system finds a seat or a unit by — a project, a knowledge
  space, a channel, an email address, a contact identity — on any seat or
  unit, one the draft adds included, and every credential, a `${VAR}`
  anywhere and a tool server's `mcp_env` block included. These take
  `admin`. The builder draws the first kind disabled with the reason,
  asks for no contact identity when a lead adds a human seat, and refuses to
  replace a seat that holds one.

A lead reads no revision history, which takes `admin`. A save whose
answer is lost is settled by reading the unit back: still on the base
revision, it did not land; at a newer one holding exactly the unit as sent,
it did; anything else is a newer company, which the draft is updated onto as
in [When somebody else saves first](#when-somebody-else-saves-first), finding
the changes the save landed already there. **Show what changed**, **View
changes** and **Copy as YAML** read the company document and are not offered.

The same rule reaches a lead outside the builder, for the seats in the units
they lead: a seat's profile shows its settings, read through
`GET /config/roles/{handle}` and its unit's own fields, and its token ceilings
are changed in Company › Budgets and with **Raise budget** where a decision about
a stopped seat offers it, through `PUT /config/roles/{handle}`. The company's
own ceilings stay `admin`'s.

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
