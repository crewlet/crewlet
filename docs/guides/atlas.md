# Reading the Atlas

How to open the company's atlas in the dashboard, read its map and its
catalogue, follow what depends on what, and read where each fact came from.
What the atlas
*is* (entries, facts, relations, where each claim comes from and how one thing
seen by several sources becomes one entry) is the [atlas](../concepts/atlas.md)
concept page; the questions a script reads are in the
[API reference](../reference/api-endpoints.md#the-atlas). This page is the
screens.

---

## Who can open it

**Company › Atlas** sits after Integrations in the sidebar (`g` then `o`).
Reading it takes the **Atlas** permission (`atlas`): an administrator holds it,
and anybody else is given it on purpose, from Settings › People, because a map
of the company's servers, addresses and vendors is what an intruder looks for
first ([who reads it](../concepts/atlas.md#who-reads-it)). A person without it
sees the row locked, with the permission it needs, never hidden.

The atlas is never a way round another permission: a person who does not use
Gitlet is shown no repository from it, and an integration's standing or a
server's connection test is shown only to an administrator, as on the screens
those come from.

## What it shows today

This version of the atlas draws what the engine already knows, and what people
holding Atlas add and correct, by asking Ask Crewlet or through the operator
tools ([changing the atlas](../concepts/atlas.md#changing-the-atlas)):

| Lane | What fills it in this version |
|---|---|
| Entry | the zones the Cloudflare integration declares, the host names the engine answers at (`api.external_url`, `api.public.external_url`), and what the engine reads from the Cloudflare account: every zone, every name an A, AAAA or CNAME record gives and what it points at, and every certificate pack ([what integrations read](../concepts/atlas.md#what-integrations-read)) |
| Services | the services of the Datadog service catalogue and the ones its monitors' `service:` tags name, the containers running on each server you [let the atlas read](#reading-inside-a-server), and what people add |
| Runtime | the servers the SSH integration declares, with the open ports of each one you let the atlas read, and the hosts that report to Datadog |
| Data | what people add: databases, queues and buckets |
| Platforms | each connected integration's account |
| Code and delivery | Gitlet's repositories and their checks, the repositories the GitHub and GitLab integrations name, and what the engine reads from the GitHub organisation: every repository the agents' apps cover, its workflows and the environments it deploys to ([what integrations read](../concepts/atlas.md#what-integrations-read)) |
| Observability | what the engine reads from the Datadog organisation: every monitor with its state, and what each one watches ([what integrations read](../concepts/atlas.md#what-integrations-read)), and what people add |
| Tools and models | the tool servers of Settings › MCP & tools and the model entries of Settings › Models |

An empty cluster says this on the map itself, so a new company sees what it would
take to fill each one. These screens only read: every change goes through Ask
Crewlet's proposals, which you approve, or the operator tools. What agents
record arrives with later versions.

## Reading inside a server

A server the SSH integration declares is on the map from the start, with its address. What runs on it is not, because finding out means running commands on the server, and that is yours to decide for each server.

To let the atlas read a server, an administrator opens **Company › Integrations › SSH › Settings** and, under **Servers**, switches **Read for Atlas** on for it. The dialog shows the exact commands before anything is saved: one that lists the listening ports and, where the server's container reads are on, one that lists the running containers. Nothing else can be run this way, the engine runs them itself rather than through an agent, and every run is recorded in the SSH audit. Then run the server's setup on it again, so the server accepts the engine's inventory.

From the next refresh the server shows its open ports, and each container appears as a workload in the Services lane, by its name and image, running on that server. A container's command line is never kept. If the server cannot be read (it is down, or its setup was not run again), the SSH source says which server and what to do, and what the atlas already showed for it stays.

Switching it off stops the commands. The integration page has every detail: [let the atlas read a server](../integrations/ssh.md#let-the-atlas-read-a-server).

## The page

Company › Atlas is one page, read top to bottom: a band of four tiles, the
search with one Filter control, the map, and the catalogue under it. The map and the
catalogue are the same entries, so they share one search, one set of filters
and one selection.

## The band

- **The company runs on**: how many entries the filters leave, as a ring split
  into Quiet, Needs a look and Alerting, each with its count.
- **Needs attention now**: the two most urgent entries, each with the reason in
  one sentence ("A standing alarm is raised", "Two sources disagree, and nobody
  has settled it"), and how many need attention in all. Pressing a card
  selects that entry on the map.
- **Recorded this week**: how many lines the atlas's own log took in the last
  seven days, over its lines a day for the last fourteen. It is a count of what
  people, integrations and agents recorded. What the engine reads from the
  settings and its own records at every read is not recorded, so it leaves no
  line.
- **Where it comes from**: the vendors the entries come from, and how many
  entries are Confirmed, Likely and Unconfirmed.

Nothing in the band, or anywhere on the page, is a request rate, a latency or
an error budget: the engine holds none. Health is the engine's word for an
entry's open signals, and activity is a count of recorded lines.

## The map

The map reads the way a request travels. The first row is **Entry** (domains,
host names, certificates), **Services** and **Data**; the second is what those
stand on, **Runtime** and **Platforms**; the third is what stands around them,
**Code and delivery**, **Observability**, and **Tools and models**. Inside
Services, a service stands one column right of the service that calls it.
Each entry is a card: its kind's glyph, its vendor's mark on the corner, and
its name.

- **A notch means something needs attention**: red with a badge for Alerting
  (a tool server that is down, a model whose last call failed, an integration
  whose last pass failed, a Datadog monitor in Alert and everything it
  watches), amber for Needs a look (a monitor in Warn or No Data, and a
  pipeline whose last run or deployment failed with everything it deploys,
  among them). A quiet entry draws none. Each cluster's title counts its
  entries and how many of them need attention.
- **A dashed edge means unconfirmed**: an entry only an agent reported, with
  nothing the engine could check. These are hidden until somebody confirms
  them; **Show unconfirmed** under Filter shows them, and says how many it
  would add.
- **There are three families of line**, each switched from the legend. *Calls*
  are drawn at rest, with dashes that move the way the call points; the dashes
  show direction, never volume. *Runs on* is drawn at rest as one trunk between
  two clusters with its count ("7 run on"). *Around it* (the account an entry
  belongs to, the monitor on it, the repository it is built from) is drawn for
  the entry you stand on.
- **Selecting an entry lights its whole path**, everything it depends on and
  everything that depends on it, and lets the rest recede. A bar at the foot of
  the map names the selection and counts both sides ("7 entries depend on it,
  and it depends on 1"), with **Details** to open the entry beside the page.
  Press the entry again, the ground or Escape to clear.
- **Resting the pointer on an entry** lights its neighbours and shows a card
  with its kind, vendor, health and the reason it is not quiet. The same entry
  is lit in the catalogue, and a row under the pointer is lit on the map.
- **A large company opens with its entries grouped by vendor, closed**, each
  one card saying what it holds; press a group to open it. A company bigger
  than the engine draws at once arrives with some groups closed by the engine,
  named above the map with a link to read each in the catalogue.

Drag to pan, scroll or use the zoom bar to zoom, press `0` to fit the whole
map, and use the fullscreen button in the zoom bar to give the map the screen.
With the keyboard, Tab onto the map, move with the arrow keys, press Enter to
select an entry or open a group and Escape to clear; typing a name finds it.
The moving dashes stand still for a reader who asked the system for less
motion.

## The search and the filters

The search takes a name or a key. The catalogue lists what matches, and the
map keeps its shape and pushes back what does not, so you find an entry where
it stands. **Filter**, beside the search, opens the filters (Kind, Lane,
Vendor, Health, Confidence and Show unconfirmed). They narrow the map and the
catalogue alike, and each menu counts what picking a value would leave. The
button counts the filters in force, and each one is a chip under the toolbar
that you remove on its own, with **Clear all** beside them.

## The coverage line

Under the toolbar, one line says how many entries the filters leave and which
vendors they come from: "48 entries, from GitHub, Cloudflare, Gitlet and
Anthropic". It never says the atlas is complete, because it never is. When a
read could not read a source in full it says which and why (Gitlet, for a
person who does not use it), and it counts the values left out for looking
like a credential, which are never shown.

## The catalogue

Under the map, the same entries are rows, grouped by health with the most
urgent first: Alerting, Needs a look, then Quiet, each heading with its count.
A row is the entry (its disc, name and kind), its vendor, who answers for it,
its health, how many entries use it and how many it depends on, its activity
(its recorded lines a day for the last fourteen days, with their count), its
last change with what it was, and its confidence. Fifty rows load at a time,
with **Load more**; the groups stay whole because the engine reads the whole
atlas in that order.

Pressing a row selects the entry on the map and opens it beside the page.

## An entry

**Details** on the map, or a row in the catalogue, opens the entry beside the
page:

- its name, key, health, owner and confidence, and the one line a person gave
  it;
- what is wrong with it, in sentences;
- **What it touches**: how many entries rely on it and how many it relies on,
  over a small diagram in the map's own look of the entry among its
  neighbours (what uses it, what it depends on, what it runs on and what
  stands around it);
- its facts, each with where it came from, how far it is trusted and when it
  was last seen ("seen by Cloudflare · confirmed · 4 min ago", "observed by sre
  · likely · turn");
- **Recent changes**: the atlas's own log for this entry, newest first, each
  line with who recorded it.

**Open** takes you to its page, which has three tabs:

- **Overview**: the same, with the diagram drawn wide, how much the company
  uses it, which agents hold an account there, the keys merged into it, and
  every change on request.
- **Relations**: what it depends on, what depends on it, the seats, units and
  pages it names, and everything that depends on it hop by hop, with the way to
  show the same on the map.
- **Sources**: what each source says about it. Where two sources disagree, both
  values are shown first, with which one the atlas shows and why (a person's
  value over a newer report, or the newest of two equally trusted sources); the
  entry then waits for a person, and nothing is settled for you. The tab is
  marked while a disagreement stands.

The confidence words are the engine's, never a model's: **Confirmed** (the
company settings, the engine's own records, a person, or an integration's read
with the company's credential), **Likely** (an agent backed by a call or a
file it cites) and **Unconfirmed** (an agent with nothing the engine could
check).

**Internal** beside an address marks one on a private network. The atlas keeps
these, since they are the company's own map, and says so rather than hiding
them.

## Asking about it

[Ask Crewlet](../concepts/assistant.md) reads the atlas as you, through the
same questions these screens read, so it answers exactly what you could open
here and nothing more. It can search the entries, read one entry in full and
trace what depends on an entry or what it depends on, along the relations the
map draws:

- "What do we use for monitoring?" finds the vendor accounts and reads the one
  whose capabilities say so.
- "Where does our code live?" lists the repositories.
- "What depends on the database?" finds the database and traces up from it,
  hop by hop. Where two entries could be meant, it asks which, naming both.

Name an entry as you would here, by its name or its key ("orders-db", "the
web-1 server"); it is looked up through your own search of the atlas. An
entry the answer names is a chip: open it for the entry's facts, and **Show
on the map** opens this page with that entry open beside it, lit along the trace
where the answer was one. A person without the Atlas permission is told so,
as the sidebar tells them, and is never handed an entry. Every link comes
from the engine's own route table, never from a fact's value: an address in
the atlas is a fact to read, not a place Ask Crewlet sends you.

## What the screens never do

Nothing leaves the screen: there is no export, print or share. The screens ask
again every thirty seconds, so a change to the settings or a tool server going
down appears without a reload.
