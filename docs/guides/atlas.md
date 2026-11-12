# Reading the Atlas

How to open the company's atlas in the dashboard, read its map and its list,
follow what depends on what, and read where each fact came from. What the atlas
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
| Services | the services of the Datadog service catalogue and the ones its monitors' `service:` tags name, and what people add |
| Runtime | the servers the SSH integration declares, and the hosts that report to Datadog |
| Data | what people add: databases, queues and buckets |
| Platforms | each connected integration's account |
| Code and delivery | Gitlet's repositories and their checks, and the repositories the GitHub and GitLab integrations name |
| Observability | what the engine reads from the Datadog organisation: every monitor with its state, and what each one watches ([what integrations read](../concepts/atlas.md#what-integrations-read)), and what people add |
| Tools and models | the tool servers of Settings › MCP & tools and the model entries of Settings › Models |

An empty lane says this on the map itself, so a new company sees what it would
take to fill each one. These screens only read: every change goes through Ask
Crewlet's proposals, which you approve, or the operator tools. What agents
record arrives with later versions.

## The map

The atlas opens on the map, read left to right the way a request travels:
Entry, Services, Runtime, Data and Platforms, with Code and delivery,
Observability, and Tools and models in the band beneath. Each entry is a card:
its kind's glyph, its name, and its key (the address, the account id, the
repository path), or how long since a source last saw it. Entries of one vendor
stand in a group inside their lane.

- **A dot means something needs attention**: red for Alerting (a tool server
  that is down, a model whose last call failed, an integration whose last pass
  failed, a Datadog monitor in Alert and everything it watches), amber for
  Needs a look (a monitor in Warn or No Data among them). A quiet entry draws no dot. The legend in
  the corner counts each, and pressing one pushes the other cards back.
- **A dashed edge means unconfirmed**: an entry only an agent reported, with
  nothing the engine could check. These are hidden until somebody confirms
  them; **Show unconfirmed** in the toolbar shows them, and says how many it
  would add.
- **The lines are dependencies**, from the entry that relies to the entry
  relied on: a host name in its zone, a service on its server. Selecting an
  entry also lights its other relations, such as the account it belongs to.
- **Trace** (Off, Depends on, Used by, Both) lights everything the selected
  entry reaches, and says how many: "Used by" answers "what depends on this
  server".
- **A large company opens with its groups closed**, each one card saying what
  it holds; press a group to open it. A company bigger than the engine draws at
  once arrives with some groups closed by the engine, named above the map with
  a link to read each in the list.

Drag to pan, scroll or use the zoom bar to zoom, and press `0` to fit the whole
map. With the keyboard, Tab onto the map, move with the arrow keys, press Enter
to select an entry or open a group and Escape to clear; typing a name finds it.

## The coverage line

Under the toolbar, one line says how many entries the filters leave and which
vendors they come from: "48 entries, from GitHub, Cloudflare, Gitlet and
Anthropic". It never says the atlas is complete, because it never is. When a
read could not read a source in full it says which and why (Gitlet, for a
person who does not use it), and it counts the values left out for looking
like a credential, which are never shown.

## The list and the filters

**List** in the toolbar draws the same entries as rows, fifty at a time with
**Load more**: each entry's kind, vendor, health, confidence and when it was
last seen. **Order** reads them by the map's order, by name or by last seen,
across the whole atlas. The filters (search, Kind, Lane, Vendor, Health,
Confidence and Show unconfirmed) narrow the map and the list alike, and each
menu counts what picking a value would leave.

## An entry

Selecting an entry on the map, or a row in the list, opens it beside the page:
its health, its facts, what it depends on and what depends on it. **Open** takes
you to its page, which has three tabs:

- **Overview**: its open signals, then its facts, each with where it came from,
  how far it is trusted and when it was last seen ("seen by Cloudflare ·
  confirmed · 4 min ago", "observed by sre · likely · turn"); how much the
  company uses it, which agents hold an account there, and the keys merged into
  it.
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
on the map** opens this map with that entry's panel open, lit along the trace
where the answer was one. A person without the Atlas permission is told so,
as the sidebar tells them, and is never handed an entry. Every link comes
from the engine's own route table, never from a fact's value: an address in
the atlas is a fact to read, not a place Ask Crewlet sends you.

## What the screens never do

Nothing leaves the screen: there is no export, print or share. The screens ask
again every thirty seconds, so a change to the settings or a tool server going
down appears without a reload.
