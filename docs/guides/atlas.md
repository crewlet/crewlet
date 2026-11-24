# Reading and correcting the Atlas

How to open the company's atlas in the dashboard, read its map and catalogue, follow dependencies and sources, and add, correct and confirm what it says. What the atlas *is* (entries, facts, relations, provenance, merging of sources) is the [atlas](../concepts/atlas.md) concept page; scripts use the [API reference](../reference/api-endpoints.md#the-atlas). This page is the screens.

---

## Who can open it

**Company › Atlas** sits after Integrations in the sidebar (`g` then `o`). Reading it takes the **Atlas** permission (`atlas`): an administrator holds it, anybody else is given it on purpose from Settings › People, because a map of servers, addresses and vendors is what an intruder looks for first ([who reads it](../concepts/atlas.md#who-reads-it)). A person without it sees the row locked, with the permission it needs, never hidden.

The atlas never bypasses another permission: a person who does not use Gitlet sees no repository from it, an integration's standing or a server's connection test is shown only to an administrator, and the work touching an entry only to a person who uses Worklet.

## What it shows today

The atlas draws what the engine already knows plus what people holding Atlas add and correct, on these screens, via Ask Crewlet, or through the operator tools ([changing the atlas](../concepts/atlas.md#changing-the-atlas)):

| Lane | What fills it in this version |
|---|---|
| Entry | the zones the Cloudflare integration declares, the host names the engine answers at (`api.external_url`, `api.public.external_url`), and from the Cloudflare account every zone, every name an A, AAAA or CNAME record gives and what it points at, and every certificate pack ([what integrations read](../concepts/atlas.md#what-integrations-read)) |
| Services | Crewlet itself on its nodes, the Datadog service catalogue and the services its monitors' `service:` tags name, the containers on each server you [let the atlas read](#reading-inside-a-server), the services a Gitlet repository's deploy files declare (shown as likely; [what a repository's files declare](../concepts/atlas.md#what-a-repositorys-files-declare)), and what people add |
| Runtime | each node of the engine's fleet (address for administrators only, marked internal), the servers the SSH integration declares with the open ports of each one you let the atlas read, and the hosts reporting to Datadog |
| Data | the engine's own event stream and object store as its nodes say them, the databases, queues and buckets a Gitlet repository's compose files, manifests and charts name (likely), and what people add |
| Platforms | each connected integration's account, and the vendors a Gitlet repository's dependency and Terraform files name (likely) |
| Code and delivery | Gitlet's repositories and checks, the repositories the GitHub and GitLab integrations name, and from the GitHub organisation every repository the agents' apps cover, its workflows and deploy environments ([what integrations read](../concepts/atlas.md#what-integrations-read)) |
| Observability | from the Datadog organisation every monitor with its state and what it watches ([what integrations read](../concepts/atlas.md#what-integrations-read)), and what people add |
| Tools and models | the tool servers of Settings › MCP & tools and the model entries of Settings › Models & Fleets |

An empty cluster says on the map what would fill it. Anybody holding Atlas changes it from these screens ([adding and correcting](#adding-and-correcting)); Ask Crewlet can propose the same changes for you to approve, or they can be made through the operator tools.

## Reading inside a server

A server the SSH integration declares is on the map from the start, with its address. What runs on it is not, because finding out means running commands on it, which you decide per server.

An administrator opens **Company › Integrations › SSH › Settings** and, under **Servers**, switches **Read for Atlas** on for it. The dialog shows the exact commands before anything is saved: one listing the listening ports and, where the server's container reads are on, one listing the running containers. Nothing else can be run this way; the engine runs them itself, not through an agent, and every run is recorded in the SSH audit. Then run the server's setup on it again so it accepts the engine's inventory.

From the next refresh the server shows its open ports, and each container appears in the Services lane by name and image, running on that server. A container's command line is never kept. If the server cannot be read (down, or setup not rerun), the SSH source says which server and what to do, and what the atlas already showed stays.

Switching it off stops the commands. Details: [let the atlas read a server](../integrations/ssh.md#let-the-atlas-read-a-server).

## The page

Company › Atlas is one page, top to bottom: a band of four tiles, the search with one Filter control, the map, and the catalogue under it. Map and catalogue show the same entries and share one search, one set of filters and one selection.

## The band

- **The company runs on**: how many entries the filters leave, as a ring split into Quiet, Needs a look and Alerting, each with its count.
- **Needs attention now**: the two most urgent entries, each with its reason in one sentence ("A standing alarm is raised", "Two sources disagree, and nobody has settled it"; a second signal is named, more are counted) on up to two lines, all of it in a tooltip on hover and keyboard focus. Pressing a card selects that entry on the map and brings the map into view (on a phone, below the band). Beside the title, how many are **Alerting** and how many **Need a look**, each a link that narrows the map and catalogue to them. The tile shows a placeholder until its list is read, and says "Nothing needs attention." only when nothing does.
- **Recorded this week**: how many lines the atlas's own log took in the last seven days, over its lines a day for the last fourteen. It counts what people, integrations and agents recorded; what the engine reads from the settings and its own records at every read leaves no line. **Every change** opens [Changes](#changes).
- **Where it comes from**: the vendors the entries come from, and how many entries are Confirmed, Likely and Unconfirmed. Under it, **Review** says how many things wait for a person and opens [Review](#review), and **Sources** opens [each integration's last read](#sources-and-refresh-now).

Nothing on the page is a request rate, a latency or an error budget: the engine holds none. Health is the engine's word for an entry's open signals; activity is a count of recorded lines.

## The map

The map reads the way a request travels. Row one: **Entry** (domains, host names, certificates), **Services** and **Data**. Row two, what those stand on: **Runtime** and **Platforms**. Row three, what stands around them: **Code and delivery**, **Observability**, **Tools and models**. The rows are arranged for the space the map has: on a wide screen a row's lanes take more columns and wider cards, and rows may stand side by side; on a phone a row splits into one or two lanes at a time. The order always reads left to right, top to bottom. Inside Services, a service stands one column right of the service that calls it. Each entry is a card: its kind's glyph, its vendor's mark on the corner, its name.

- **A notch means something needs attention**: red with a badge for Alerting (a tool server that is down, a model whose last call failed, an integration whose last pass failed, a Datadog monitor in Alert and everything it watches), amber for Needs a look (a monitor in Warn or No Data, and a pipeline whose last run or deployment failed with everything it deploys). A quiet entry draws none. Each cluster's title counts its entries and how many need attention.
- **A blue notch means it is being worked on**: a quiet entry that an open Worklet task names ([work on an entry](#work-on-an-entry)). An entry needing attention keeps its red or amber notch, and its card says how many open tasks name it. Drawn only for a reader who uses Worklet.
- **A dashed edge means unconfirmed**: an entry only an agent reported, with nothing the engine could check. These are hidden until somebody confirms them; **Show unconfirmed** under Filter shows them and says how many it would add.
- **There are three families of line**, each switched from the legend. *Calls* are drawn at rest, with dashes moving the way the call points (direction, never volume). *Runs on* is drawn at rest as one trunk between two clusters with its count ("7 run on"). *Around it* (the account an entry belongs to, the monitor on it, the repository it is built from) is drawn for the entry you stand on.
- **Selecting an entry lights its whole path**, everything it depends on and everything that depends on it, and lets the rest recede. A bar at the foot of the map names the selection and counts the path over every hop ("Over every hop, 7 entries rely on it and it relies on 1"), with **Details** to open the entry beside the page. Its sheet and page count one step out instead, under the same words: what relies on it, what it relies on, what stands around it. Press the entry again, the ground or Escape to clear.
- **Resting the pointer on an entry** lights its neighbours and shows a card with kind, vendor, health and the reason it is not quiet. The same entry is lit in the catalogue, and a row under the pointer is lit on the map.
- **A large company opens with entries grouped by vendor, closed**, each group one card saying what it holds; press to open. A company bigger than the engine draws at once arrives with some groups closed by the engine, named above the map with a link to read each in the catalogue.

**The map opens with all of it in view**, centred and zoomed to fit, so a large company opens zoomed out and a phone shows the whole map small. It fits again when a filter changes what it draws. When its space changes shape (fullscreen, a wider window), it is arranged again and fits, unless an entry is selected, so a sheet opening for it never moves the entries; a smaller change fits it again only while you have not moved it. Once you zoom or pan, it keeps your view on refresh. A link to one entry (**Show it on the map** from its page or sheet, or an address with the entry selected) opens the map on that entry instead, in the middle, at a size its card reads.

Drag to pan, scroll or use the zoom bar to zoom, press `0` to fit the whole map, and use the fullscreen button in the zoom bar. From half size down an entry is a tile: its kind's glyph filling the cell, its name cut to what fits, the whole cell in its health colour when not quiet. Zoomed out further than a name fits, each entry is a filled mark in its health colour, and every cluster keeps its title, count and tally of entries needing a look, so the picture reads as how much of each cluster is quiet. An empty lane is a dashed outline until half size, where it says what would fill it. A tile is pressed like a card; zoom in to press a mark. With the keyboard, Tab onto the map, move with the arrow keys, Enter selects an entry or opens a group, Escape clears; typing a name finds it. The moving dashes stand still for a reader who asked the system for less motion.

## The search and the filters

The search takes a name or a key. The catalogue lists matches, and the map keeps its shape and pushes back what does not match. **Filter**, beside the search, opens the filters (Kind, Lane, Vendor, Health, Confidence, Show unconfirmed and Show retired entries only). They narrow map and catalogue alike, and each menu counts what picking a value would leave. The button counts the filters in force; each is a chip under the toolbar that you remove alone, with **Clear all** beside them. A retired entry is found with **Show retired entries only** and restored from its page. An address naming a filter value this dashboard does not know (an old bookmark, a newer version) is not applied: the atlas is drawn without it, and a note says so, with **Remove it**.

## The coverage line

Under the toolbar, one line says how many entries the filters leave and which vendors they come from: "48 entries, from GitHub, Cloudflare, Gitlet and Anthropic". It never says the atlas is complete. When a read could not read a source in full it says which and why (Gitlet, for a person who does not use it), and it counts the values left out for looking like a credential, which are never shown.

## The catalogue

Under the map, the same entries are rows grouped by health, most urgent first: Alerting, Needs a look, Quiet, each heading with its count. A row shows the entry (disc, name, kind), vendor, who answers for it, health, how many entries use it and how many it depends on, activity (recorded lines a day for the last fourteen days, with their count), last change and what it was, and confidence. Fifty rows load at a time, with **Load more**; groups stay whole because the engine reads the whole atlas in that order.

A row whose entry an open Worklet task names says so beside its health ("2 open tasks"), for a reader who uses Worklet; the words open the entry's [Work tab](#work-on-an-entry). Pressing a row selects the entry on the map and opens it beside the page.

## An entry

**Details** on the map, or a row in the catalogue, opens the entry beside the page:

- its name, key, health, owner and confidence, and the one line a person gave it;
- what is wrong with it, in sentences;
- **What it touches**: how many entries rely on it, how many it relies on and how many stand around it, counted off the small diagram under the sentence, which draws the entry among its neighbours in the map's look (what uses it, depends on, runs on, stands around it);
- its facts, each with its source, how far it is trusted and when last seen ("seen by Cloudflare · confirmed · 4 min ago", "observed by sre · likely · turn");
- **Work**: the first five open tasks that name it, the rest on its page;
- **Recent changes**: the atlas's log for this entry, newest first, each line with who recorded it.

**Open** takes you to its page, which has five tabs:

- **Overview**: the same, with the diagram drawn wide, how much the company uses it, which agents hold an account there, the keys merged into it, and its latest changes.
- **Relations**: what relies on it, what it relies on and what stands around it (counted as the sentence over the diagram counts them), the seats, units and pages it names, and everything that relies on it over every hop, with the way to show the same on the map.
- **Work**: the Worklet tasks that name it ([work on an entry](#work-on-an-entry)).
- **Sources**: what each source says about it. Where two sources disagree, both values are shown first, with which one the atlas shows and why (a person's value over a newer report, or the newest of two equally trusted sources); the entry then waits for a person, and nothing is settled for you. Each disagreement has buttons: keep the value shown, or take the other. The tab is marked while a disagreement stands.
- **History**: every line the atlas's log holds for the entry, newest first, with who recorded it, a page at a time. Kept for `atlas.retention_days` (400 days unless the settings say otherwise).

The confidence words are the engine's, never a model's: **Confirmed** (the company settings, the engine's own records, a person, or an integration's read with the company's credential), **Likely** (an agent backed by a call or file it cites, or a repository's deploy and dependency files) and **Unconfirmed** (an agent with nothing the engine could check).

**Internal** beside an address marks one on a private network. The atlas keeps these, being the company's own map, and says so.

## Work on an entry

A Worklet task can name the entries it touches (the server a change rolls out to, the database a migration runs on). The link is made on the task, under **Atlas entries** in its side panel (**Link an atlas entry**), or by an agent working on it ([the Worklet guide](worklet.md#the-atlas-entries-a-task-touches)). Nothing is written to the atlas, and a link never changes an entry's health or confidence.

- **The Work tab** of an entry lists the open tasks that name it, most recently changed first, then those finished in the last 30 days, each with key, title and status, and opens the task. The tab counts the open ones.
- **The map and the catalogue mark an entry with open work**: a blue notch on a quiet entry's card, and the count on its row.
- **Worklet decides who sees work.** A person who holds Atlas and does not use Worklet sees the Work tab locked, with the permission it needs, and no mark. Where the company keeps its work outside Worklet, the tab says so.
- **Atlas decides who sees entries.** On the task, a person holding Atlas sees each entry as a chip that opens it. A person who does not sees how many entries the task names, locked with the permission it needs, and no name.
- **An entry that left the atlas.** A task naming an entry since retired still opens it; one whose entry is gone from the atlas says "1 entry is no longer in the atlas" on the task, where the link can be removed.

## Adding and correcting

Every change is recorded in your own name on the atlas's log. Your value outranks what an integration or agent reports and never erases it: if a source later reports something else, both are shown and the entry waits in [Review](#review). Nothing settles in silence. Each control is drawn for every reader, and held with the reason for one who may not press it.

**Add entry**, at the end of the toolbar, records something no source reports: its kind, key, a name, who owns it (a seat or a unit of the chart), a one-line purpose and, for anything longer, the Doclet page that documents it.

- An account takes its **vendor**; one the atlas does not know is **Other**, with a short name. Every other kind may name the **vendor account** it belongs to, whose vendor it is then shown under.
- The key is the entry's identity, written as its kind takes it (a service's slug, a host name, an address with its scheme); the form says how under the field. A tool server, a model, a server keyed `ssh/` and a Gitlet repository are declared in the settings or in Gitlet, and enter the atlas from there.
- A key or value shaped like a credential is refused, never stored masked, and the engine's refusal is shown under the form in its own words.

On an entry's sheet and page, under its header:

| Control | What it records |
|---|---|
| **Confirm** | the entry as your own, so one only an integration or an agent reported is confirmed by you |
| **Correct** | one fact's value as you know it. The dialog shows the value the atlas holds and every other value a source reports. For a fact with several values it adds one. The checkbox records that the value shown is wrong instead |
| **More › Set owner** | one seat or one unit of the chart, or no owner |
| **More › Add relation**, **Remove relation** | a relation to another entry, offered only where the vocabulary lets this kind draw it, or one recorded as not there |
| **More › Link a page** | the Doclet page that documents it, found by title |
| **More › Merge with** | that two entries of one kind are the same thing. The dialog first shows both side by side and every fact they hold different values of |
| **More › Split** | that two keys are not the same thing; always wins over a shared key and over a merge. The dialog says what will be shown apart before it applies |
| **More › Retire**, **Restore** | the entry taken off the map, kept in its history, and brought back |
| **More › Purge** | an administrator's alone: the entry, or one value wherever it appears, erased on every node with its history, and kept from being recorded again. Nothing undoes it, so it is typed back to confirm and the engine asks you to confirm who you are first |

What the company's settings, Gitlet or the engine's own records declare is changed where it is declared: the engine refuses it here and says where.

Every change is recorded under the seat of the person who made it, so a sign-in with no seat (an API token, or an account bound to none) changes nothing in the atlas, whatever access it holds. The controls stay drawn and held with that sentence, and a note under them says it once with a link to Company › Structure, where a person joins and takes a seat. The one exception is Sources › Refresh now: any sign-in with Atlas access may ask.

A change waits only on the sources that describe its entry. While a source cannot be read (the coverage line says which), you still add and correct what it does not describe: with Gitlet unreachable, a service, server or vendor account changes as usual and only a change to a Gitlet repository waits. When a change waits, the message under the form names the source that could not be read.

## Review

**Review** (from the band, `#/company/atlas/review`) lists what waits for a person. The engine works it out on every read and stores none of it, so an item is gone the moment its cause is:

- **Disagreements**: a fact or relation the sources disagree on, with the value shown and every other value beside it. **Keep** records the value shown as yours again; **Take** records the other one as yours. Where one side is what the company's settings or the engine's own records declare, no button is drawn: it is settled where declared, and the item says so.
- **Unconfirmed**: an entry only an agent reported, with nothing the engine could check. It stays off the map until you **Confirm** or **Retire** it. Under it is what the agent said: which agent, in which turn (the link opens the run, where the call's own answer is), the tool whose call it cited, and what a call of that tool is evidence of.
- **Likely**: an entry an agent backed with a call or file, that no person, integration or setting stands behind yet. It stays here, even after somebody corrects one fact on it, until somebody confirms the entry itself. What only a repository's files declare is likely as well and does not wait here: **Confirm** it on its own page whenever you choose.

Pick several with their boxes to decide them together: **Accept as shown** confirms each entry and keeps each shown value, and **Retire** retires the picked entries. A bulk decision lists what it decides in a dialog and sends only when you press again there; what the engine refused stays picked, with its reason. An entry an agent reported is listed with who reported it. An agent's value beside a person's, in a disagreement, names the call it cited the same way.

**One thing in Review also reaches your Inbox:** a newer report that differs from a value you set or confirmed yourself. It appears under **Needs you** with both values and opens the entry's Sources tab, where you keep one, and leaves the Inbox as soon as somebody decides. Everything else waits in Review for every holder of Atlas together, so the Inbox does not fill with infrastructure changes.

**An agent that owns an entry is woken about it.** When an entry whose owner is an agent turns to Alerting, or its sources come to disagree, the engine wakes that agent to look: at most once a day per entry, and only about a change it has not already been told. The agent reads the entry itself and acts within its role; a disagreement is still yours to settle in Review. A paused agent, or one out of budget, is not woken until it runs again. To turn this off for the company, set `atlas.owner_wakes: false` in the company document ([The owning agent is woken when its entry drifts](../concepts/atlas.md#the-owning-agent-is-woken-when-its-entry-drifts)).

**What has waited too long reaches the Setup Advisor.** A disagreement nobody has decided in 7 days is listed on the [Advisor](advisor.md#what-the-atlas-shows) page with the entries it is on, beside a service nobody owns, an owner that left the chart, a service open to the internet that nothing watches, and a source that stopped reading. Each is fixed from there or from the entry's page: both send the same change.

## Sources and Refresh now

**Sources** (from the band, `#/company/atlas/sources`) lists each integration the atlas reads for the company: Cloudflare, Datadog, GitHub, GitLab and the company's servers among them, once connected. DigitalOcean and Expo are read only once you give the atlas an optional read-only identity of its own there ([DigitalOcean](../integrations/digitalocean.md#let-the-atlas-read-the-team), [Expo](../integrations/expo.md#let-the-atlas-read-the-account)); until then their cards say Not read yet, and nothing else is affected. Each is read whole on a timer (every hour unless [`atlas.refresh_interval_seconds`](../getting-started/configuration.md) says otherwise; an administrator changes it at the top of **Sources**, in place, no shorter than five minutes), and its card says how the last read went:

- **Its state**: Read, Partly read (the read finished and was refused some part), Last read failed, or Not read yet, with when it was read and when next. A failed read is retried five minutes later, and the atlas keeps what the last whole read found.
- **Its last whole read in figures**: entries held, calls made at the vendor (for servers, commands run), changes recorded, and **Values withheld**: how many values it left out for being shaped like a credential. They are counted and never shown anywhere.
- **What it could not read**: each listing the vendor refused, with the permission that restores it, and each server it could not reach or whose command failed. What an earlier read found there stands. A read that failed outright says so in a sentence, with the way to the integration it read with; administrators can open what the read said in the engine's own words.
- **Entries it stopped seeing**: how many entries the last read did not list. Nothing is retired on one miss: an entry is retired only when a second whole read, a day later, does not list it either.
- **Each server**, on the servers' card: when it was last read whole, and how many of its parts (ports, containers) the last read could not read. Only a server whose inventory is switched on is listed ([let the atlas read a server](../integrations/ssh.md#let-the-atlas-read-a-server)).

**Refresh now** asks for a read of that source without waiting for the timer. The node that reads for the atlas makes it within a minute, and the card says who asked, by name, while it waits. The engine reads one source at most once every five minutes, whoever asks: inside that time the button is held and says when it can be pressed again. Anyone holding Atlas may ask; an agent never does.

## Changes

**Changes** (`#/company/atlas/changes`) is the atlas's own log for the whole company, newest first: its lines a day for the last fourteen days, then each line with the entry it is about and who recorded it. It lists what people, integrations and agents recorded; what the engine reads from the settings and its own records at every read leaves no line.

## On Home

**Atlas attention** is a card you can add to your Home (Edit Home, under Atlas). It shows how many entries are Alerting and how many Need a look, and lists the four most urgent with each reason in one sentence. Pressing an entry opens it; **Open** goes to the atlas, and a line under the list says how many more the page holds.

- When nothing is wrong it says **Nothing is alerting**, with how many entries the atlas holds, only over a read that reached every source: if one was not read, the card names it and says the list may be short.
- It is offered only to people who hold Atlas. If your access is taken away, the card stays on your Home, says you no longer have access, and shows and reads nothing.

## Asking about it

[Ask Crewlet](../concepts/assistant.md) reads the atlas as you, through the same questions these screens read, so it answers exactly what you could open here. It can search entries, read one in full and trace what depends on an entry or what it depends on, along the relations the map draws:

- "What do we use for monitoring?" finds the vendor accounts and reads the one whose capabilities say so.
- "Where does our code live?" lists the repositories.
- "What depends on the database?" finds the database and traces up from it, hop by hop. Where two entries could be meant, it asks which, naming both.

Name an entry as you would here, by name or key ("orders-db", "the web-1 server"); it is looked up through your own search of the atlas. An entry the answer names is a chip: open it for its facts, and **Show on the map** opens this page with that entry open beside it, lit along the trace where the answer was one. A person without the Atlas permission is told so, as the sidebar tells them, and is never handed an entry. Every link comes from the engine's own route table, never from a fact's value: an address in the atlas is a fact to read, not a place Ask Crewlet sends you.

## What the screens never do

Nothing leaves the screen: there is no export, print or share. The screens ask again every thirty seconds, and at once after your own change, so a settings change or a tool server going down appears without a reload. A write is never drawn before the engine has answered it.
