# Reading and correcting the Atlas

How to open the company's atlas in the dashboard, read its map and its
catalogue, follow what depends on what, read where each fact came from, and
add, correct and confirm what it says.
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
holding Atlas add and correct: on these screens, by asking Ask Crewlet, or
through the operator tools
([changing the atlas](../concepts/atlas.md#changing-the-atlas)):

| Lane | What fills it in this version |
|---|---|
| Entry | the zones the Cloudflare integration declares, the host names the engine answers at (`api.external_url`, `api.public.external_url`), and what the engine reads from the Cloudflare account: every zone, every name an A, AAAA or CNAME record gives and what it points at, and every certificate pack ([what integrations read](../concepts/atlas.md#what-integrations-read)) |
| Services | Crewlet itself, on the nodes it runs on, the services of the Datadog service catalogue and the ones its monitors' `service:` tags name, the containers running on each server you [let the atlas read](#reading-inside-a-server), the services a Gitlet repository's deploy files declare, shown as likely ([what a repository's files declare](../concepts/atlas.md#what-a-repositorys-files-declare)), and what people add |
| Runtime | each node of the engine's own fleet (its address for administrators only, marked internal), the servers the SSH integration declares, with the open ports of each one you let the atlas read, and the hosts that report to Datadog |
| Data | the engine's own event stream and object store, as its nodes say them, the databases, queues and buckets a Gitlet repository's compose files, manifests and charts name, shown as likely, and what people add |
| Platforms | each connected integration's account, and the vendors a Gitlet repository's dependency and Terraform files name, shown as likely |
| Code and delivery | Gitlet's repositories and their checks, the repositories the GitHub and GitLab integrations name, and what the engine reads from the GitHub organisation: every repository the agents' apps cover, its workflows and the environments it deploys to ([what integrations read](../concepts/atlas.md#what-integrations-read)) |
| Observability | what the engine reads from the Datadog organisation: every monitor with its state, and what each one watches ([what integrations read](../concepts/atlas.md#what-integrations-read)), and what people add |
| Tools and models | the tool servers of Settings › MCP & tools and the model entries of Settings › Models |

An empty cluster says this on the map itself, so a new company sees what it would
take to fill each one. Anybody holding Atlas changes it from these screens
([adding and correcting](#adding-and-correcting)); the same changes can be
proposed by Ask Crewlet, which you approve, or made through the operator tools.

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
  line. **Every change** opens [Changes](#changes).
- **Where it comes from**: the vendors the entries come from, and how many
  entries are Confirmed, Likely and Unconfirmed. Under it, **Review** says how
  many things wait for a person and opens [Review](#review), and **Sources**
  opens [each integration's last read](#sources-and-refresh-now).

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
- **What it touches**: how many entries rely on it, how many it relies on and
  how many stand around it, counted off the small diagram under the sentence,
  which draws the entry among its neighbours in the map's own look (what uses
  it, what it depends on, what it runs on and what stands around it);
- its facts, each with where it came from, how far it is trusted and when it
  was last seen ("seen by Cloudflare · confirmed · 4 min ago", "observed by sre
  · likely · turn");
- **Recent changes**: the atlas's own log for this entry, newest first, each
  line with who recorded it.

**Open** takes you to its page, which has four tabs:

- **Overview**: the same, with the diagram drawn wide, how much the company
  uses it, which agents hold an account there, the keys merged into it, and
  its latest changes.
- **Relations**: what it depends on, what depends on it, the seats, units and
  pages it names, and everything that depends on it hop by hop, with the way to
  show the same on the map.
- **Sources**: what each source says about it. Where two sources disagree, both
  values are shown first, with which one the atlas shows and why (a person's
  value over a newer report, or the newest of two equally trusted sources); the
  entry then waits for a person, and nothing is settled for you. Each
  disagreement carries its decisions as buttons: keep the value shown, or take
  the other. The tab is marked while a disagreement stands.
- **History**: every line the atlas's log holds for the entry, newest first,
  with who recorded it, a page at a time. History is kept for the company's
  `atlas.retention_days` (400 days unless the settings say otherwise).

The confidence words are the engine's, never a model's: **Confirmed** (the
company settings, the engine's own records, a person, or an integration's read
with the company's credential), **Likely** (an agent backed by a call or a
file it cites, or a repository's deploy and dependency files) and
**Unconfirmed** (an agent with nothing the engine could check).

**Internal** beside an address marks one on a private network. The atlas keeps
these, since they are the company's own map, and says so rather than hiding
them.

## Adding and correcting

Every change here is recorded in your own name, on the atlas's own log. Your
value outranks what an integration or an agent reports and never erases it: if
a source later reports something else, both are shown and the entry waits in
[Review](#review). Nothing settles in silence. Each control is drawn for every
reader, and held with the reason for one who may not press it.

**Add entry**, at the end of the toolbar, records something no source reports:
its kind, its key, a name, who owns it (a seat or a unit of the chart), a
one-line purpose and, for anything longer, the Doclet page that documents it.

- An account takes its **vendor**; one the atlas does not know is **Other**,
  with a short name. Every other kind may name the **vendor account** it
  belongs to, whose vendor it is then shown under.
- The key is the entry's identity, written as its kind takes it (a service's
  slug, a host name, an address with its scheme); the form says how under the
  field. A tool server, a model, a server keyed `ssh/` and a Gitlet repository
  are declared in the settings or in Gitlet, and enter the atlas from there.
- A key or a value shaped like a credential is refused, never stored masked,
  and the engine's refusal is shown under the form in its own words.

On an entry's sheet and page, under its header:

| Control | What it records |
|---|---|
| **Confirm** | the entry as your own, so one only an integration or an agent reported is confirmed by you |
| **Correct** | one fact's value as you know it. The dialog shows the value the atlas holds and every other value a source reports beside the field. For a fact with several values it adds one. The checkbox records that the value shown is wrong instead |
| **More › Set owner** | one seat or one unit of the chart, or no owner |
| **More › Add relation**, **Remove relation** | a relation to another entry, offered only where the vocabulary lets this kind draw it, or one recorded as not there |
| **More › Link a page** | the Doclet page that documents it, found by its title |
| **More › Merge with** | that two entries of one kind are the same thing. Before it applies, the dialog shows both side by side and every fact they hold different values of |
| **More › Split** | that two keys are not the same thing, which always wins over a shared key and over a merge. The dialog says what will be shown apart before it applies |
| **More › Retire**, **Restore** | the entry taken off the map, kept in its history, and brought back |
| **More › Purge** | an administrator's alone: the entry, or one value wherever it appears, erased on every node with its history, and kept from being recorded again. Nothing undoes it, so it is typed back to confirm and the engine asks you to confirm who you are first |

What the company's settings, Gitlet or the engine's own records declare is
changed where it is declared: the engine refuses it here and says where.

## Review

**Review** (from the band, `#/company/atlas/review`) lists what waits for a
person. The engine works it out on every read and stores none of it, so an item
is gone the moment its cause is:

- **Disagreements**: a fact or a relation the sources disagree on, with the
  value shown and every other value beside it. **Keep** records the value shown
  as yours again; **Take** records the other one as yours. Where one side
  is what the company's settings or the engine's own records declare, no button
  is drawn: it is settled where it is declared, and the item says so.
- **Unconfirmed**: an entry only an agent reported, with nothing the engine
  could check. It stays off the map until you **Confirm** it, or **Retire** it.
  Under it is what the agent said: which agent, in which turn (the link opens
  the run, where the call's own answer is), the tool whose call it cited, and
  what a call of that tool is evidence of.
- **Likely**: an entry an agent backed with a call or a file, that no person,
  integration or setting stands behind yet. It stays here, even after
  somebody corrects one fact on it, until somebody confirms the entry itself.
  What only a repository's files declare is likely as well and does not wait
  here: **Confirm** it on its own page whenever you choose to.

Pick several with their boxes to decide them together: **Accept as shown**
confirms each entry and keeps each shown value, and **Retire** retires the
picked entries. A bulk decision lists what it decides in a dialog and sends
only when you press again there; what the engine refused stays picked, with
its reason. An entry an agent reported is listed there with who reported it.

An agent's value beside a person's, in a disagreement, names the call it cited
in the same way.

**One thing in Review also reaches your Inbox:** a newer report that differs
from a value you set or confirmed yourself. It appears under **Needs you**
with both values and opens the entry's Sources tab, where you keep one. It
leaves the Inbox as soon as somebody decides. Everything else waits in Review
for every holder of Atlas together, so the Inbox does not fill with
infrastructure changes.

**What has waited too long reaches the Setup Advisor.** A disagreement nobody
has decided in 7 days is listed in [Settings › Advisor](advisor.md#what-the-atlas-shows)
with the entries it is on, beside a service nobody owns, an owner that left the
chart, a service open to the internet that nothing watches, and a source that
stopped reading. Each is fixed from there or from the entry's own page: both
send the same change.

## Sources and Refresh now

**Sources** (from the band, `#/company/atlas/sources`) lists each integration
the atlas reads for the company: Cloudflare, Datadog, GitHub, GitLab and the
company's servers, once each is connected. Each is read whole on a timer (every hour
unless [`atlas.refresh_interval_seconds`](../getting-started/configuration.md)
says otherwise), and its card says how the last read went:

- **Its state**: Read, Partly read (the read finished and was refused some
  part), Last read failed, or Not read yet, with when it was read and when it
  is read next. A read that failed is tried again five minutes later, and the
  atlas keeps what the last whole read found.
- **Its last whole read in figures**: the entries it held, the calls it made
  at the vendor (for the servers, the commands it ran), the changes it
  recorded, and **Values withheld**: how many values it left out because they
  were shaped like a credential. They are counted and never shown, here or
  anywhere else.
- **What it could not read**: each listing the vendor refused, with the
  permission that restores it, and each server it could not reach or whose
  command failed. What an earlier read found there stands. Why a read failed
  outright is shown to administrators; everyone else is told that it failed.
- **Entries it stopped seeing**: how many entries the last read did not list.
  Nothing is retired on one miss: an entry is retired only when a second whole
  read, a day later, does not list it either.
- **Each server**, for the servers' card: when the server was last read whole,
  and how many of its parts (its ports, its containers) the last read could
  not read. Only a server whose inventory is switched on is listed
  ([let the atlas read a server](../integrations/ssh.md#let-the-atlas-read-a-server)).

**Refresh now** asks for a read of that source without waiting for the timer.
The node that reads for the atlas makes it within a minute, and the card says
who asked while it waits. The engine reads one source at most once every five
minutes, whoever asks: inside that time the button is held and says when it
can be pressed again. Anyone holding Atlas may ask; an agent never does.

## Changes

**Changes** (`#/company/atlas/changes`) is the atlas's own log for the whole
company, newest first: its lines a day for the last fourteen days, then each
line with the entry it is about and who recorded it. It lists what people,
integrations and agents recorded; what the engine reads from the settings and
its own records at every read leaves no line.

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
again every thirty seconds, and at once after your own change, so a change to
the settings or a tool server going down appears without a reload. A write is
never drawn before the engine has answered it.
