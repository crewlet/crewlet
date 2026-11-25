# Worklet

Worklet is Crewlet's own work tracker, and the default. This page is the
product guide: what a company can record, what a seat can do with it, what a
person can do with it, and what each screen answers.

The architecture (one ordered log, N identical copies) is in
[The Tracker](../concepts/task-engine.md) and
[Replication](replication.md). Nothing here needs it.

## Projects and keys

Work is filed into **projects**. A project has a key (`ENG`, `OPS`), a name,
and a unit of the org chart that owns it. Every task filed into it gets a key
like `ENG-412`, minted from the project's own counter.

**Projects come from the org chart.** Declaring a unit with a project key in
the company config creates the project on the next apply, on every data node,
with no gesture from anybody, so a fresh company files its first task in its
first minute.

A project's name, purpose and unit follow the configuration **activated last**,
whichever node applies what when. Each project is stamped with the instant its
configuration was activated (`chart_epoch`); an earlier chart never overwrites
a later one, so a node restarting on a replaced revision, or applying an older
one late during a rollout, leaves the newer names alone.

- Re-applying an activation that has landed writes nothing (every restart of
  every node on an unedited company). When several nodes apply one activation
  at once, they contend per project and exactly one writes it.
- A row whose fields match but whose stamp is older is re-stamped, so a slow
  node cannot apply an older activation over it. Re-activating an unchanged
  revision (the [credential-rotation gesture](../concepts/secret-store.md))
  therefore records one quiet "org chart re-applied" change per project.
- The instant is the activating node's clock, except that an activation is
  never published at an instant no later (to the millisecond) than the one it
  replaces: the activation pointer moves it to a millisecond after, so an
  activation from a node whose clock runs behind is still applied everywhere.
- A node never boots on a company file: one it imports is applied by the
  control plane's first reconcile, with the activation's own instant, before
  the node claims any seat.
- **Every project is attempted on every pass.** One the node could not write (a
  full stream, a write of unknown outcome) does not stop the rest. A pass that
  left any behind logs `tracker_chart_not_applied` naming each project that
  failed and each it wrote, and is retried by the next apply or restart. Until
  then a project the chart names but no node has written refuses work filed
  into it.

A numbering **gap** is normal and permanent. `ENG-7` exists, `ENG-8` never did,
`ENG-9` is next: the counter moves before the task lands, so a crash between
the two costs a number rather than risking two tasks sharing a key.

**Two tasks under one key** is the one way a key can still be ambiguous: a key
counter restored beside work it had already numbered mints numbers that work
holds, and nothing can refuse the second task. The task that **claimed** the
key first keeps it (every link, chat message and tool call naming the key opens
that one, on every node); every other holder is flagged `key_collision`
(`flag=key_collision` lists them) and is reached by its **id**.

- Moving one to another project gives it a key of its own; the key it leaves
  goes on opening the claimant.
- **Purging the claimant** hands the key to the task with the lowest id among
  those still holding it as their current key, and clears that task's flag. A
  duplicate that moved away is never handed the key it left.
- Only a task's create, a move and a purge change which task a key opens; an
  edit never does.

Everything that names a task to somebody who will act on it uses its key,
unless the flag is set, and then its id:

- every row a list, a search, an item or a feed returns carries the flag
  beside the key;
- a tool's receipt names the task in `item` (the reference to hand to the next
  call), with `key` beside it and `key_collision` where the two differ. A
  create or move whose new key another task already holds says so. Every
  argument that takes a task says to use the id for a flagged row, and a write
  of unknown outcome says which task to read by the same rule;
- a wake about a flagged task names its id under **Task:** and in the block
  saying what to read, with a **Key:** line saying which key it holds and why
  that key opens another task; an excerpt naming one (a lead's priorities, a
  purge) gives the id with the key beside it.

### What a project row carries about its work

Every project carries two maintained facts, so a project directory answers
"how much" and "how recently" without reading a task:

| | What it is |
|---|---|
| **task counts** | `todo`, `active`, `done` and `closed`: how many of the project's tasks are in each of the four status **groups**. `todo` is work nobody has started, `active` work somebody has; they are separate numbers because a queue to triage and a team at full stretch are different situations. Moved by the commit that moves a task between groups, files one, moves one to another project, or removes or purges one. Add the first two for every unfinished item. |
| **last change** | when the project's work last changed and **who** changed it: a handle and the author kind (`agent`, `human`, `operator`, `system`). |

Both are **maintained, never counted on the fly**, written by the same commit
that changes the work, so drawing thirty projects costs thirty rows.

**The last change is the newest entry of the project's own activity feed**
(`task_activity`): same commit, instant and actor, so the two never disagree.
That decides what counts:

- Every commit about a task in the project moves it: filing, changing any
  field, commenting, archiving, removing, restoring, purging, loud or quiet.
- An agent's **turn** does not: it records what the work *cost*, not a change.
- Re-ordering the **board** does not: the manual order changes no task and the
  feed does not carry it.
- Editing the **project itself** does not (a new default assignee, a field
  declaration, an archive).
- A task **moved between projects** counts against the project it moved *to*;
  both projects' counts move.

A project nobody has filed work into reports **no last change at all**, not an
instant borrowed from its creation.

**Moving a task between projects.** A move (`move_work_item`) is allowed to the item's project **lead** or an
administrator (`admin`). Over HTTP it is `POST /worklet/items/{key}/move` with
`{"project": "OPS"}`. It carries the task's **whole subtree**: only a root task
moves, and every task beneath it follows, re-keyed in the new project with its
old key still resolving. The people on the root are told.

- **Tags.** The subtree's tags are declared in the new project as the old one
  declares them. A tag the new project already has under the same slug is used.
  A tag whose label the new project already gives a **different** tag (ENG's
  `api` and OPS's `backend-api`, both labelled "API") refuses the move before
  anything is written, naming both tags, because two tags nobody could tell
  apart would result; so does a new project already holding its 512 tags. A
  move is never rewritten onto a tag it did not carry: put the items under the
  target's tag first if they mean the same, or declare the slug in the target
  under a label of its own (`write_project`'s `tags_add`), and move again.
- **Trash.** A task in the trash anywhere in the subtree refuses the move until
  it is restored or purged, because a removed task is frozen and the move could
  not carry it.
- **While a move is walking**, the part of the subtree not yet carried is held
  still. Filing a subtask under a task the walk has not reached, re-parenting
  one of those tasks (out of the subtree or within it), and a merge with
  `move_subtasks` whose duplicate or survivor is one of them all answer
  **unavailable** rather than a refusal: the same call a moment later is
  decided in the new project. A task the walk *has* carried is ordinary in its
  new project.
- **A move that stops part-way** (its node died, or the stream refused an
  append) leaves its root **marked mid-move**. The `tracker` duty finishes it
  on its first pass (every 15 minutes) after the move's claim has lapsed (a
  minute after its last heartbeat, never while it still runs); tasks still in
  the old project follow on fresh keys, leaving a numbering gap like any
  interrupted write. A task removed during the move is waited for: the root
  stays marked until it is restored, and the next pass carries it.
- **A task filed behind the walk** is carried too. One filed under a task the
  walk read earlier keeps the mark up (the walk's last append checks that
  nothing under the root is still in the old project) and the walk runs one
  more pass. One decided on a node that had not yet heard of the move, whose
  record landed after the mark came down, is found by the `tracker` duty's
  `tracker_move_stragglers` job on its next pass: every live task still in
  another project than its root, under a root nothing is walking, follows on a
  fresh key under the move's own claim, so never beside a running move of that
  root. `tracker_move_stragglers_carried` names the root and how many followed;
  `tracker_move_stragglers_failed` a repair retried next pass. A straggler in
  the trash stays until restored, and its restore brings it to the duty.
- Until then, every task still in the old project is in the attention queue as
  `flag=inconsistent_project`: a subtask filed in another project than its root
  is not drawn under that root on either board. A merge never leaves one: a
  fold that would re-parent a duplicate's subtasks onto an item in another
  project is refused before it starts, naming the move that makes it possible.

A move that stops part-way **after the item itself moved** is not answered as
a failure. The item is in the new project under its new key, and the answer is
its receipt with the walk's count (`subtree_followed` of `subtree_total` tasks
made it across) and a `move_stopped` sentence saying what finishes it (an agent
reads that sentence; a person is told the same as a `move_unfinished` caveat,
or `move_waiting` while a task in the trash holds it).

- What finishes it (or a move that answered `unknown`) is the **same** call again, never a new one: the same
  `Idempotency-Key`, which is the `op_id` every answer carries, sent back as
  that header (the request's key, whether it sent one or the node minted it).
- A task in the trash that holds the walk is named as `move_waits_for` and has
  to be restored or purged first.
- Where the node cannot vouch for the task the walk stopped at
  (`move_unvouched`), the same call there stops at the same place, so leave it
  to the `tracker` duty or make it on another node.
- The repeated call is answered by the move itself even once its first attempt
  has carried the item over: it reports the move that landed under the key the
  item left (`moved_from`) and finishes what has not followed. The lead check
  is on the move *out* of a project, so the old project's lead finishing their
  own move is not sent to the new lead. A move into the project an item is
  already in that no earlier attempt of that call made is refused as
  somebody else's, which is why a *new* call cannot finish a stopped move.

### A project's target date

A project may carry a **target date**: the day its lead means it to be
finished. It is a **date, not an instant** (`2026-12-18`), a day on the
company's own clock (`timezone`), because a target is a day named in a planning
meeting and an instant would invent the hour and zone that day ends in.

- It is the **lead's**, set with `write_project(target_date: "2026-12-18")` by
  the project's lead or a person acting through their own credential, and
  refused for any other seat. It is not chart-owned: a config apply that
  renames the project leaves it untouched.
- Given an **instant**, the write stores the day it falls on on the company's
  clock and says so in the answer's `warnings` (a dashboard user is told in its
  own words, see
  [What it answers](../reference/api-endpoints.md#what-it-answers)). Anything
  that is neither is refused, naming both spellings.
- `null` or `""` clears it; a project with none reports no `target_date`, and
  `sort=target` puts it last in both directions ("no target" is not the
  earliest one).

### Which team an item belongs to

Every item carries two units that answer different questions.

- **Filed into**: the team the work belongs to. Set once at the create and
  never rewritten: it records what was true, so it may name a team the chart
  has since dropped, or one given another key. It is what `unit=` filters on
  and what a board's `unit` axis groups by; a board heads that column with the
  team's current name.
- **Routes to**: whose lead hears about the item *now*. It starts equal to the
  filed unit and moves when somebody re-routes the item (`update_work_item`
  with `routing_unit`).

**Re-routing is the project lead's.** It decides who owns the work, so it takes
the authority that declares a project's fields: whoever leads the project the
item is filed in, or an administrator (`admin`), each with the `worklet` app.
Every other caller is refused, and the refusal names the rule and the
permission that would admit them. A person is asked about as the seat the
[identity
directory](../concepts/identity-and-access.md#the-binding-has-two-ends-and-only-one-of-them-arbitrates)
binds them to, so a founder's own assistant re-routes as the founder's seat; a
credential bound to no seat reaches it only through `admin`. A node that cannot
read its chart answers that it cannot tell rather than refusing.

**You rarely state either.** An item filed with no `unit` is filed into the
team that owns its project (the one the org chart gave it), so a task in `ENG`
belongs to whichever unit declared `project: ENG`, whoever filed it: an agent
in that team or another, a person on the board, or
[your own AI assistant](#what-a-person-can-do). Name `unit` only when the work
belongs to a different team than its project.

A project the chart gave no unit files work into no unit; the project's lead is
then the item's only lead fallback.

### Naming a team: its key or its name

A unit is named by its **key** (the `id` the org chart gives it, minted from
the unit's name when a company file declares none) or by its name, where
exactly one unit carries it; see
[the org chart's unit keys](../concepts/organization-model.md#a-units-key-is-its-identity).
**Both spellings work everywhere a unit is named**, in any case: the `unit`
argument on `create_work_item`, `routing_unit` on `update_work_item`, the
`unit=` and `routing_unit=` filters, the `unit` a project listing is narrowed
by, and `GET /worklet/workload?unit=`. `unit: engineering` reaches the team
called `Engineering`. A name two units share names neither; where a spelling is
one unit's key and another's name, the key wins.

The engine **stores** the unit's key, so renaming a team does not move its
work. A screen **shows** the team's current name, resolved through the chart:
an item's **Filed into** and **Routes to** read as the name and link to that
team's work, a board's unit column is headed with it, and a project's directory
row carries it. A reference the chart no longer has is **marked** on each of
those rather than printed as a name, since it is a team that has left the chart
and needs correcting. A team is one column whichever way you name it; see
[what a board groups on](#what-a-board-groups-on).

**Changing a team's key does not rewrite the work already filed.** A document
giving a unit another key removes it and adds a new one, so earlier rows keep
the old key, marked as above. Project rows carry the unit too and are
chart-owned: the next time the node brings its projects up to the chart it
rewrites each to the key of the unit that declares the project.

A unit reference that names no team in the chart is **refused at a write**
(naming the team, so a typo is visible) and **matches nothing at a read**,
never an error: a filter naming a team nobody has is answerable, and the answer
is no such work.

## Tasks

| Field | What it is |
|---|---|
| **key** | `ENG-412`. Unique in practice, never reused. |
| **type** | from the workspace catalogue, see [below](#the-catalogue). A type the company has not declared is REFUSED at the create. |
| **title**, **body** | the body is versioned; every save writes an immutable revision. |
| **status** | one of `todo`, `in_progress`, `in_review`, `done`, `cancelled`, `closed`. |
| **priority** | `none`, `low`, `normal`, `high`, `urgent`. |
| **assignee** | one seat or person. |
| **reporter** | who filed it. |
| **unit** | the team the work belongs to, and the team it routes to; see [Which team an item belongs to](#which-team-an-item-belongs-to). |
| **collaborators**, **watchers** | who is on the thread and who is listening. |
| **parent**, **subtasks** | a tree with a depth cap, inside ONE project: a create or re-parent that would put a subtask outside its parent's project is refused (it would be on no board). A parent in the trash takes no new subtask, a purged one is refused as gone, and a task is never filed under itself or its own subtasks. Two re-parents at the same moment on two nodes can still close a loop; it is applied, flagged `cycle` (`flag=cycle` lists it), and a gesture on one of its tasks' subtrees (a move, a removal, a restore) takes the loop's other tasks and everything hanging from them, each once. A query filters ROOTS by default and lets subtrees ride along; `subtasks=separate` filters every task on its own. In the trash the unit is the removal rather than the tree (see [Views](#views)), and a task whose root (in the trash, whose removal) is on the other side of the trash is filtered as the root of what is left. Rolled up, the question is asked of each row's root by its key. `parent` (one item's subtasks) and `has_children` (items that have some) read a tree index, costing only those subtasks. |
| **start / due**, **estimate**, **points** | scheduling and sizing. |
| **tags** | from a per-project tag set; see [Tags](#tags). A tag the project has not declared is REFUSED at the write. |
| **custom fields** | declared per project and at the workspace, typed, with option lists; see [the catalogue](#the-catalogue). Filter on one with `f.<slug>`, and see [the operators](#filtering-a-custom-field). |
| **checklists** | up to 16 named lists holding up to 64 items each and 256 between them, each item with its own assignee; see [Checklists](#checklists). |
| **relations**, **dependencies** | links between tasks, and blocking edges: `linked` (related, no blocking meaning), `waiting_on` and `blocking` (one edge seen from its two ends), each a set changed with `{add, remove}` on `update_work_item`, and `duplicate_of` (one task the item duplicates, cleared with `null` or `""`). |
| **linked pages** | knowledge-base pages the task names, set with `linked_pages` on `update_work_item`. `linked_page=<page id>` lists the tasks that name a page. |
| **atlas entries** | the entries of the company's [atlas](atlas.md) the task touches (servers, services, databases, domains and accounts the work changes), set with `linked_entries` on `update_work_item`. Up to 32, each kept by the entry's id. See [The atlas entries a task touches](#the-atlas-entries-a-task-touches). |
| **references** | other tasks the description mentions by key (`ENG-12`), derived by the applier on every data node from the first 64 distinct keys named, where a former key resolves like the current one. `references=<key>` lists the tasks whose description mentions it. A comment's mentions are not references. |
| **spend** | turns, rounds, tokens, cache, wall-clock, delegated workers and review send-backs this task has cost; see [Spend is on the task](#spend-is-on-the-task). |
| **reopens** | how many times the task left a finished status for an unfinished one. |
| **updated** | when the task last changed: the fleet-agreed instant of the newest record that changed it (the same instant its newest history entry carries), never a writer's clock. `sort=-updated`, `updated=` and every list's Updated column read it. |

### Status groups

Six statuses, four groups: `not_started`, `active`, `done`, `closed`. Filters
and boards work on the group, so a company that adds a status does not have to
re-teach every query what "finished" means.

`done` and `cancelled` both stamp the task finished: the group decides, not the
slug. A cancelled task is finished work that produced nothing, a different fact
from an open one and the same fact for anything counting.

A task that leaves `done` or `closed` for an unfinished status is **reopened**
and counts it (`reopens`). Done to closed is not a reopen (the work stayed
finished), nor is `todo` to `in_progress`. It is counted by group, so an added
or renamed status does not change what a reopen is. Sort with `sort=reopens`,
total with `totals=reopens:sum`.

### Spend is on the task

Every turn an agent spends on a task adds to that task's own counters, so
"what did this cost" is a question about a piece of work rather than a seat's
month.

**One turn, one task.** A turn is charged to the one work item it is on, or to
nothing, never split. The item is the one its wake was about (a task
notification, or a Jira, GitHub or GitLab event about one issue), the one the
colleague who asked for help was on, or the one its executor was launched on
when it resumes from it; failing all three, the one task its own writes
touched, if exactly one. A chat message leading to no task write is charged to
nothing. Only the engine's own tasks carry counters: a turn on a Jira issue is
attributed on its events and adds to no row here.

**What a turn's tokens include.** Its own phases, delegated workers, the
round-cap judge, executor launches, and the **auxiliary** model calls it made
(turn-start memory filter, knowledge query, episode summary, ledger and tool
rewrites, card condensing), in input and output with the prompt cache's share
beside them. That includes the engine's spend on the executor while no part of
the turn runs (its tool-bridge calls, and condensing a collected launch's
report, failure or question, paid by the part of the turn that resumes from
it). Learning afterwards (reflection, diary, skills) is the seat's, not the
task's, but is on the seat's day in the [spend
history](budgets-and-spend.md#auxiliary-spend). `spend_workers` (delegated
tasks its turns ran) and `spend_sent_back` (reviews that returned the work) say
*why* a task was expensive.

**A turn that parks is charged per segment.** A turn launching its executor
completes when it parks and again each time a launch is collected (often
minutes later, on another node). Each segment adds its spend, only the first
counts as a turn, and the segment collecting a launch pays for it. A turn
charged only for what it wrote is charged when it ends, for every earlier
segment too.

**Counted once, whichever node runs the turn.** Each segment has its own id and
the counters move only when its row is new, so a retried segment or a record
delivered twice adds nothing. A seat on a node without the `data` role records
its charge through a data node under the same id, so one asked again of the
next data node is still counted once.

**Removed versus purged.** A task in the trash is still charged (removing
destroys nothing). A **purged** task is not: the charge is refused, and one in
flight when the purge landed is dropped on every data node. The tokens stay on
the seat's counters and spend history; only the task's share goes.

**A task lists its own turns.** Every charged segment is a row the task keeps
for good in the replicated estate, so its page lists turns long after the
thirty days a node keeps its event history, on any node, including from a node
that has left. The list is `work_item_turns`: one entry per TURN, segments
folded (tokens, rounds, wall time summed; phases in first-run order; outcome,
summary and review of the newest segment), newest first, paged by each turn's
first segment so a turn gaining a segment mid-read never moves between pages.
Each entry is numbered **"Turn n"**: the position of its counted segment among
the task's counted segments, so the newest turn's number IS `spend_turns`. A
turn none of whose segments on this task counted carries no number.

**What a turn did rides on its record:** the turn's account of what it did (the
reviewer's summary or the executor's artifact), the notes of the newest review
that sent the work back, and each executor tool call with a count; each account
at most 600 bytes and at most sixteen tools, bounds the writer refuses to
exceed. A longer account is **condensed** for the card by the seat's auxiliary
model and marked `(condensed)`, never cut; where no rewrite can be had the card
says how long it is and to open the turn, and, for a segment that failed
because a phase did, which phase. Call arguments and results stay on the turn's
trace, which the card links to.

The counters are `spend_turns`, `spend_rounds`, `spend_input`, `spend_output`,
`spend_cache_read`, `spend_cache_write`, `spend_wall_ms`, `spend_tokens`
(input plus output; the `sort=spend_tokens` key, named after the column like
every total), `spend_workers` and `spend_sent_back`. Every one can be totalled
over a board, including as a `median` or `p90`, which answer the value a task
actually holds rather than an average of two.

## The catalogue

Two declarations make the company's own vocabulary: what a task may **be**, and
what it may **carry**.

**Types.** Seven ship with the engine (`task`, `bug`, `epic`, `story`,
`spike`, `chore` and `milestone`), so a fresh company files its first task in
its first minute. A declared catalogue **adds** to them; a declaration sharing
a builtin's slug **renames** it, so a company can call a bug a defect without
losing tasks filed under `bug`.

A type is **refused at the create** if the company does not declare it, which
stops `Bug`, `bugfix` and `BUG` filing three types beside `bug` that every
board then groups on as if real. An **archived** type takes no new work and
leaves existing tasks alone, which is why a type is archived rather than
deleted.

**Fields.** Custom fields are declared at the workspace and on a project; a
task's effective set is the union. A value is keyed by the field's **id**, not
its slug, so a field can move between the two keeping every stored value.

**A field's values are filterable.** Each is a row keyed on the field's id, in
the column its declared **type** says (a number in the numeric column, an
instant in the date one, a choice's *id* in the reference one). That makes
`f.effort=gt:9` a numeric comparison rather than a lexical one, and keeps every
task that chose an option when somebody renames it. A multi-valued field is one
row per member, so `f.areas=api` is a seek rather than a scan.

**A required field is required of the tasks it applies to.** `applies_to`
names the types that carry a field, and a field that does not apply to a task
cannot be missing from it (its value would be hidden the moment it was set). A
field required at the **workspace** is required in every project; one required
on a project, only there. A subtask is judged by the second toggle
(`required_in_subtasks`, off by default) so one required field does not block
every small piece of work split off a task.

**A name is a resolution key, not a label.** A type, a field and an option are
each resolvable by id, by slug, and by **name**, so a model can write
`severity: High` after reading "High" off a board. Two declarations whose names
differ only in case or spacing are therefore refused: one lookup could not tell
them apart. A type's name is also checked against the **shipped** types, though
renaming a builtin by declaring its own slug is what the override is for.

**A field's configuration is checked against its type.** A setting its type
does not read (a precision on a checkbox, a time flag on a number, a rollup on
a text field) is refused naming which types use it. A minimum above its maximum
is refused at the declaration. The settings live under `config` on the
declaration, in the catalogue read and the catalogue tools:

| Setting | Type | The types that read it | What it does |
|---|---|---|---|
| `options` | list | `dropdown`, `labels`, `relationship` | The choices. A value stores the option's **id**, so renaming one keeps every task that chose it |
| `unit` | string | `number`, `progress`, `rollup` | Rendered inline beside every value (`8 h`), so at most 16 bytes |
| `precision` | integer, 0–6 | `number`, `progress`, `rollup` | How many decimal places a value may carry. Default **0**, whole numbers only. A value carrying more is refused naming the rule, never rounded |
| `min` / `max` | number | `number`, `progress`, `rollup` | The range a value must fall in. Each is optional on its own, and `min: 0` is a floor; leaving it out means "no floor". A minimum above its maximum is refused |
| `time` | boolean | `date` | True holds a time of day as well as a day, and then a bare date is refused rather than given an invented midnight. False truncates a timestamp to its date and says so |
| `progress` | `manual` | `progress` | How the bar is filled. `auto`, and the `tracking` list it would count, are refused (see below) |
| `multi` | boolean | `dropdown` | Lets one task carry more than one value, each its own filterable row. `labels`, `people` and `relationship` already hold several, so need no flag |

On the declaration itself, `applies_to` names the **type slugs** that carry the
field; empty means every type. A task of a type a field does not apply to
cannot hold a value for it, nor be required to. Changing `applies_to`, or
archiving the field, settles the values tasks already hold (out of every
filter, total and board where the field no longer applies, back in where it
does), reading only what can move: a field restricted to some types re-reads
its own values, an archived or unrestricted one only those on the side it moves
them from, never any other field's.

**A field records who declared it.** `created_by` and `created_at` are the
*write's*, never the document's (as for a tag), so a later edit keeps the name
of whoever added the field.

**Three settings are refused because nothing fills them.** A `rollup:` block,
`progress: auto` and a `tracking:` list promise a self-updating value, and this
build computes none of it; accepted, the field would hold whatever somebody
last typed under a name saying otherwise. `tracking` is refused **on a progress
field too**, since only `auto` counts sources. The field TYPES still work: a
`progress` or `rollup` field with `progress: manual` (or no configuration) is a
number somebody writes, and every filter on this page applies to it.

**An option is one value however it is written.** A choice field stores the
option's *id*; a write naming it by slug or name resolves to the id first, the
same three spellings a filter accepts.
### What a field value may be

Every value is checked against its declaration at the **write** and refused naming the rule. The check is at the write because by apply time the value is durable, and the applier salvages: a value it cannot decode writes no row and the change carries on, since refusing would stop that task's every later edit on every data node.

| Type | Accepts | Refuses |
|---|---|---|
| `number`, `progress`, `rollup` | a number, or a string that is one | non-numeric text, anything outside `min`/`max`, or more decimals than `precision` |
| `checkbox` | `true` or `false` | a string (readers disagree about `"false"`) |
| `date` | a date; a timestamp when the field holds no time, **truncated with a warning** to the day it falls on **on the company's clock** (`timezone`): the evening of the 4th in Los Angeles is the 4th, not the UTC 5th | a non-date, and a bare date on a field that holds a time |
| `dropdown`, `labels` | the option's slug, name or id, stored as the **id** | an option the field does not declare |
| `relationship` | a key or an id, stored as the **id** | an item that does not exist |
| `people` | anything that resolves to exactly **one** colleague | a spelling that names nobody, or more than one |
| `url` | an absolute url | no scheme or no host |
| `email` | a parseable address, stored as the **address**, not the display name | anything `Ana <ana@example.com>` cannot be read as |
| `text`, `textarea` | text within the type's byte cap | longer text, named rather than cut |

`null` clears any field. **Nothing is rounded to fit**: a field exact to two places refuses `3.14159` rather than store `3.14`, a number nobody typed.

Date truncation is the one change the engine makes, and it says so in the result's `warnings` (a toast in the dashboard), so the board never shows what the writer did not write.

Required fields are enforced at **every** write: an update may not clear one. ClickUp enforces them at creation only; there is no toggle.

Archiving a field is **one-way**. Its values leave the *filterable* set and stay on the task; a field returning under its old id would re-admit them against a forgotten definition. To bring one back, declare a **new** field: a new id, the same slug.

Read the catalogue at `GET /worklet/catalogue` or with `get_work_catalogue`, which every seat holds. Writing it, `write_work_catalogue` on the operator's assistant, takes **`admin`**, the permission that edits the company's own document. It is declared once for the whole workspace and names no project, so no lead could own it: it is configuration. No seat holds `admin` or the tool: a seat adding a type to make its own create succeed would be editing the rules it is judged by.

**A write is the read, edited.** The `fields` list REPLACES the declared set and every declaration is whole: a key left out is *cleared*, `config` and options included. Read the catalogue, change what you mean to, send it back. Settings are spelled as the read spells them. The `types` list works the same, read from the answer's `declared_types` (the company's own declarations, archived ones included whatever `archived` asked), not from `types`, the effective set: sending that back would copy every builtin into the company's words.

**In the dashboard**, a project's About lists the types under *What this project calls things*, with **Edit the company's types**. Its dialog lists every type with **Rename** and **Archive** (asks first, keeps the type on tasks filed as it), archived ones apart with **Restore**, and **Add type**, which shows the slug tasks will hold for good. Each press writes `PUT /worklet/catalogue` with the declarations whole and the one change applied. Every control is drawn for every reader and held with the reason for anybody without `admin`.

A **project's own** field declarations belong to the project **lead** (or an administrator), written with `write_project(fields: [...])`. That list REPLACES the project's declarations and leaves the workspace's alone; a task's effective set is their union. Send the whole set, and read `describe_project` first: its `own_fields` is exactly that set, archived included, where its `fields` is the effective one, and sending that back would copy every workspace field into the project. A project declaration sharing a workspace field's id **shadows** it, which `describe_project` names.

## Tags

Tags are the one catalogue any seat may add to. They live **per project**.

A tag has a **slug** and a **label**. The slug is what a task's row holds and a filter compares; it is normalised on the way in (`Regression`, `regression` and `needs design` arrive as `regression` and `needs-design`) and never changes. A filter normalises the same way, so `tag=Needs Design` finds `needs-design`; a value normalising to no slug is refused naming it, not answered as an empty board. A filter on tags (`any:` or `all:`) reads the work that carries them (inside a project, that project's) rather than every task in scope, so a rare tag is as cheap to ask about at the workspace as on one board; `none:`, about tags a task does *not* carry, reads each task's own. The label is what a person reads; renaming a tag moves only that.

**A tag is declared before it is used.** A write naming a label the project lacks is refused, listing the project's labels and the nearest match; otherwise any invented string became a tag, and `Bug`, `bugfix` and `BUG` sat beside `bug` on every board (as happened to task types). Two deliberate ways past the refusal:

- `write_project(tags_add: [...])` declares one, which **any seat** may do.
- `labels_create_missing: true` on `create_work_item` or `update_work_item` declares what that write is about to use, in one append before the task's own, and only once every other argument has been checked, so a write refused over its ask, a blocker or a re-route declares nothing. The answer lists what it created under `labels_created`, so a typo set by habit shows now, not on a board in three weeks. A declaration refused, or with unknown outcome, stops the write before the task's own append, and the answer says so under the tool called: the item was not filed, or the change not made. Repeating the call (a seat's with the same arguments, your assistant's with the `op_id` the answer carried) answers the declaration and then makes the write, once. Where the node's ledger cannot vouch for the declaration, the repeat stops at the same step until it reaches the node, and the answer says to declare the tags with `write_project` first, harmless if they landed. The next repeat skips the declaration, but the write dates from the same instant, so the node cannot vouch for that either: it answers with the item an earlier attempt filed, or the change where it still holds its record, else `unknown` again. That second `unknown` means the write may exist where this node cannot see it: look (`list_work_items`) or read (`get_work_item`) rather than write another way, or, from your assistant, repeat the call with its `op_id` through another node, whose ledger may reach back that far.

A slug within a **typo** of an existing one is accepted with a warning naming the nearest three: advisory, because a lead can merge two tags and a refusal with no override would block `apis` behind `api` for ever. A **label** colliding with another tag's label or slug, case-insensitively, *is* refused: indistinguishable tags split work at random.

**Renaming and archiving are the project lead's**, or an administrator's: a rename changes the word on every filed task and an archive takes a filter off everybody's board. Adding stays open to every colleague, needing only the ordinary `worklet` app that `write_project` takes. An archive is **one-way**, like a field's: tasks keep the tag and every filter on it still answers; it only takes no *new* work. To bring one back, declare it again under its own name.

A task carries at most **40** tags, and a project declares at most **512**.

## Views

A **view** is a saved query with a shape. Five shapes:

- **`list`**: rows, sorted and grouped. "What is there".
- **`board`**: columns by status (or status group, priority, when the work is due, or any field). "What is moving". A board is `group_by=`, and what comes back is **columns**: each count is over the whole set, never the rows it carries, so a column of four hundred says four hundred and hands you twenty. `group=<value>` loads one further and narrows the whole query, totals included; the column of everything nobody filled in is loaded by naming `group` and leaving it empty. On a closed axis (status, status group, priority, the due bands) **every column the query admits is drawn, empty ones included**, since a board is the shape of the process: a young company's open work is three lanes with one card, and a Done lane appears only when finished work was asked for. An open axis (an assignee, a tag) draws only values present.
- **`calendar`**: by date. "What is due". Its axis IS the `due` key, so the grid's window spends the grammar's one key for it: the fetch is bounded to the days on screen, the Overdue chip is not offered (it could narrow nothing), and the toolbar says what it counted: "6 items due in this window" where other shapes say "17 items".
- **`timeline`**: bars down a date axis. "How does this lay out". The one shape that shows a task **spanning** time and the only one that draws dependencies, as a line between two tasks.
- **`table`**: one field per column. "Which is the biggest", "who holds the overdue ones", "what is unestimated": questions about a *field*. A table puts every value of a column in one place and sorts at its head; the sort it writes is the query's own `sort=`, so it orders the **whole set**, not the loaded page.

  The list and the table are **the same grid with two column sets**: both sort at their heads, draw grouping bands, and become one labelled card per row on a phone. They differ in which columns are on, their order, and how a value is drawn (the list's priority is the mark opening its row, the table's a word in a sortable column). **Display → Columns** offers the active set's choices on either, remembered per shape.

  The Key, Status, Type and Assignee heads sort by meaning: a key by project then number (`ENG-9` before `ENG-10`), a status in the board's order from To do to Closed, a type in your catalogue's order (builtins, then yours as declared), an assignee by the seat's name in the org chart, unassigned last in both directions. Names follow the Unicode root collation ("Émile" beside "Emma", not after "Zoe"); case or accent orders only otherwise-equal names. On **My work → Queue** every item is held by the person whose day it is, so the Assignee column starts off; **Display → Columns** turns it on.

**Every container has six views without anybody saving one**: one per shape, plus `trash`, a table carrying `removed=true` and `show_closed=true`. The trash is a builtin *view*, not a sixth shape, because the parameter makes it the trash, not a way of drawing. The builtins carry no id, so they are defaults, not destinations: a client offering shapes as an arrangement (the dashboard) shows only what somebody saved in its view strip.

### Recent work, a task's place, and a card's facts

Three keys serve screens that draw a board:

- **`closed_since=<date>`** is the open work **plus** whatever finished (done or cancelled) at or after a date. It takes every date token `due=` takes (`sow`, `som`, `-7d`, `2031-04-14`, a timestamp), resolved on the **company's clock**: `closed_since=sow` begins at Monday midnight in the company's zone, the week `due=range:sow..eow` and the `due:bucket` bands mean, so a Done lane built on it empties when the week ends rather than dropping a task an hour. It is the dashboard's **Recent** scope and the third way to say which finished work is in an answer, beside `show_closed=true` and `show_closed=recent:<duration>`: naming it beside `show_closed` is refused, and an `any=` branch may not carry it. A date resolving to the zero instant (`0001-01-01T00:00:00Z`) bounds nothing and is refused: every finished task is `show_closed=true`.
- **`around=<task>`** asks where one task (its key, a former key or its id) sits in this answer. The answer carries `around: {position, prev, next,
total_hint}`: its 1-based place, the keys of the tasks drawn before and after it, and the order's length.
The order is the **drawing** order over the whole answer, not the page: a list's own sort, or a board read column by column, lane by lane inside a column, rows in row order. So the task after the last card in To do is the first in In progress, even when each column carried twenty rows. On a label board, where a task is on several columns, the order counts **cards** and a task is placed at the first column it is on. A task the answer does not hold (filtered out, finished, or in a column the board did not draw) is `around: null`, never a refusal, and `position` is null past the 10 000 the total stops counting at. Neighbours and position are found by seeking the order outwards from the task, through the order's index wherever one serves it, so stepping through a long board reads the cards beside the open one, not the board. A saved view cannot carry `around`: where somebody stands is theirs.
- **`fields=tags,dependents_count,open_asks,spend`** puts facts a board CARD draws on each row, only on an answer that asked: its labels; how many live tasks wait on it (the authored `waiting_on` edges `blocking=` reads); how many of its questions are unanswered (the test `has_open_asks=` makes); and `spend: {tokens, turns, workers, sent_back,
reopens}`. A fact that was asked for is always present, `0` and `[]` included.
They are opt-in because an agent reads the same row through `list_work_items`, where every byte is context on every listing; `list_work_items` never asks for them, even through a saved view that does. `sort=-spend_tokens` with `fields=spend` is the most expensive work.

**A priority list is paged in its own order.** `priorities=<person>` answers the tasks on somebody's list in the order they arranged; the whole list (at most 32 tasks) is read on every page and the cursor is a place in it.

**Every other answer resumes where its last page stopped.** `next_cursor`, handed back as `cursor=`, names the last row's place in the order; the next page reads from there: tasks tied with that row on every sort column and later by id, then those tied on all but the last column, and so on out to tasks past it on the first, each a seek of the index serving the order. So page fifty of a board, the workspace, a project by last update, the company by spend or the trash costs what page one does, and paging through a tie (every task at zero spend, or updated in the same instant) returns each task once, in the order one read draws. An order no index serves reads its scope for every page, as its first page does. A cursor belongs to the order that minted it, and re-sorting starts from the first page. So does an edit to what an order is built from: `sort=type` follows the type catalogue and `sort=assignee` the seats' names in the org chart, and a cursor minted before either changed is refused as `bad_params`. On that refusal the list's **Load more** starts again from the top and says why, and `list_work_items` tells an agent to ask again without `cursor`. Every configuration apply keeps the seats' order on the data nodes, as it does the chart's projects, so `sort=assignee` reads each task's place with one indexed lookup and pages cost what `sort=title`'s do however many seats the chart holds; the type order is bounded by the catalogue's 64 declared types.

### What a board groups on

Any of these as `group_by=`, and a second as `group_by2=`, which splits each column into swimlanes:

| Axis | Columns |
|---|---|
| `status` · `status_group` · `priority` | The closed sets, in the order they mean rather than by size, as columns and as swimlanes |
| `assignee` · `type` · `tag` · `project` · `unit` · `routing_unit` · `parent` | A column per distinct value, biggest first |
| `f.<slug>` | A custom field's own values |
| `due:day` · `due:week` · `start:week` | The calendar day or week a date falls in, headed `2031-04-16` or `2031-W16` |
| `due:bucket` | **When** the work is due, relative to today |

Every axis draws the absent value as its own labelled column: "nobody is assigned" is a question a board answers.

**`parent` groups on the parent task's id**, each column headed with the parent's key and title (`ENG-12 · Rotate the build keys`), read off the parent's own row, so a column is named even when the filter admits only the subtasks. Work no task holds is the "(no parent)" column. Ask it with `subtasks=separate`, which filters each task on its own; the default filters the roots and lets their subtrees ride along.

**`unit` and `routing_unit` group on the team's key**, which every write stores, each column headed with the team's current name. Loading a column further takes either spelling: `group=eng` and `group=Engineering` reach the same one. A stored unit the chart no longer has keeps its own column under the literal the items hold, marked as a team that has left the chart.

**`due:bucket` reads a calendar rather than a column.** Its six bands:

| Band | What is in it |
|---|---|
| **Overdue** | Still open, and past its due date |
| **Earlier** | Past its due date and *finished* |
| **Today** | Due at any hour of today |
| **This week** | Due after today, through the end of this week |
| **Later** | Due after that |
| **No due date** | Nobody set one |

**Earlier** exists because "overdue" means open *and* past its date: late-delivered work is not overdue, and filing it under Overdue would claim it is still owed. It is empty on the default open-only list, and appears when finished work is asked for. **This week** ends where the week does, Monday-anchored like `due=range:sow..eow`: on a Sunday it holds nothing, rather than rolling seven days into next week.

The day is cut at **the company's own midnight, on the company's [clock](../getting-started/configuration.md#the-companys-clock)** (the top-level `timezone`, UTC when absent): the instant a row's overdue mark derives from, every `due=` filter resolves against, and a person's own day and the workload's overdue counts are cut on, whether a seat, an operator's assistant or the dashboard asked. That is why the bands are the engine's, not each screen's: cut in a browser they used the *reader's* midnight and week, so a task could sit under Earlier on a row the same answer marked due today and not overdue.

"Midnight" means the first moment of the company's date, which in a few zones is not 00:00. Chile, Cuba, Lebanon and Egypt start summer time by moving the clock from 00:00 to 01:00, so that day (and "today", and an all-day due date) begins at 01:00; where a zone sets the clock back from 01:00 to 00:00, as Cuba does, midnight happens twice and the day begins at the first. The week is the ISO week, Monday to Monday.

The dashboard draws due and start dates as the company's date for every reader: a task due on the 30th reads Nov 30 in its rail, list row, card, history and calendar cell, never the 29th for a reader west of the company's clock.

### The timeline

A task's `start` and `due` are its bar. A task with only a `due` is a **milestone**, a filled diamond on its day in its status's tone; one with only a `start` is a one-day marker with a dashed edge, since "due on the 20th", "began on the 20th" and "a day's work on the 20th" are different facts. A task with neither sits in an **Unscheduled** band under the axis with a count, never placed on today, which would invent a deadline.

The window is derived from the drawn rows, padded to a fortnight so a single task has something to read against and capped at a year and a fortnight so one task due in 2031 cannot compress a fortnight into four pixels. Grouping applies (`group_by=`), and **each band gets its own axis**, so a team planning six months out does not squeeze the others into a corner.

Dependencies are arrows from the blocker to the waiting task, dashed when the edge is **one-sided** (the blocker does not list the waiting task back, which the repair duty is working on). An edge whose blocker is not on the axis (filtered out, or undated) is **counted beneath the chart**, not dropped silently, or a reader would take the arrows for every dependency.

It is **read-only**: no dragging or resizing, since what a drag means for a task with a dependency, owned by somebody else, has no good answer, and the tracker's edit surfaces already say it. A bar is a link.

Views belong to a **container** (the workspace, a project, a unit, a person), written the grammar's way: `workspace`, `project:ENG`, `unit:engineering`, `person:ana`. Each is stored as the engine keys it: a project key upper-cased, a unit under [its key](#naming-a-team-its-key-or-its-name), so a team's strip is one strip whichever spelling asks. A personal view is private to its owner. One view per container can be the default; the applier settles that in the write's transaction, so two can never both claim it. A protected view can be edited only by its owner, so a shared board is not rearranged under everybody.

Saving and pinning a view are a person's gestures, kept under the caller's own record: the seat a person holds, or the login of a token or service account the identity directory binds to none. That is also the one name the strip is read under. No parameter names whose strip to read; your own views and pins are on the strip you ask for, and a pipeline token's strip shows the pins its assistant set.

**The six builtins are not objects**, so a fresh project needs no setup, a container is never without a way to look at it, and nothing guards against deleting the last view.

**The trash is a query, not a shape.** Any view you save with `removed=true` is one too. `show_closed` travels with it because a removed task is often a finished one, and without it the trash would hide every deletion of finished work. It is ordered by **when work was removed**, newest first; `sort=removed` asks for the other end. (A removed task's rank is its position on a board it left, a stale number.) Every read of it (page, count, totals, a board drawn over it) goes through an index holding only removed work, one for the workspace and one inside each project, so opening the trash costs what is in it, and a column you sort it by sorts the trash alone. In the default subtask mode it lists each **removal** under the task the removal was made on, with what went with it riding along (what a restore of that task brings back); a subtask removed on its own is a removal of its own whatever became of its parent, and a task whose removal was restored without it is listed on its own too. `subtasks=separate` lists every removed task on its own row. What a removal, a deletion and a purge each mean is [below](#removing-deleting-and-purging).

**A view is a set of defaults, never a lock.** Opening one loads its parameters and every key you then set overrides them: a different assignee on a saved board gives that board with one key changed. A **preset** is the same mechanism for a question asked often enough that a screen puts it on a tab; a view beats a preset, because somebody saved the view. There are five:

| Preset | What it answers |
|---|---|
| `my_queue` | **what can I pick up**: a disjunction of the work you hold and the work in *your own project* nobody holds, open and unblocked, most important first. Both arms matter: "assigned to me" alone would show a seat with an empty queue a company with nothing for it while its project's unclaimed backlog sits there |
| `priorities` | your own ordered list, open tasks only, **in the order somebody arranged it**. That order is what was decided, so no sort overrides it, and a finished task drops out without the list being rewritten |
| `triage` | the open work **nobody has picked up**. With one fixed status set there is no intake status to filter on, so this is the honest definition of "needs somebody to decide" |
| `blocked` | open work that cannot move: the list a lead reads before a stand-up |
| `overdue` | open work past its due date. Defined on the status **groups**, not `show_closed`, so a task delivered yesterday is not reported late for ever |

`my_queue` and `priorities` need to know who is asking; the surface supplies that from its own credential, never the query. That is also why `f.<slug>=me` means the reader, not whoever saved the view: `me` resolves after the view's saved parameters and yours are merged, so a view saved with `f.reviewers=me` shows each person their own reviews.

### Filtering a custom field

`f.<slug>=<value>` compares a field; the comparisons a field admits belong to its **type**:

| Type | Operators |
|---|---|
| `text`, `url`, `email` | `eq` `ne` `contains` `startswith` `in` |
| `textarea` | `contains` `startswith` |
| `number`, `progress` | `eq` `ne` `lt` `lte` `gt` `gte` `range` |
| `date` | `eq` `lt` `lte` `gt` `gte` `range` |
| `dropdown` | `eq` `ne` `any` `not_any` |
| `labels` | `any` `all` `not_any` `not_all` |
| `checkbox` | `eq` |
| `relationship` | `any` `all` `not_any` |
| `people` | `any` `all` `not_any` `me` |
| `rollup` | `lt` `gt` `range`, applied after the rows are read |

`null` and `not_null` are on every type, since "is this set" is about the **row**, not the value. An operator a type does not admit is **refused naming the ones it does**; it used to be silent: `eq` on a `labels` field made a clause matching nothing, and an empty board reads as "no task has this label".

A bare `f.areas=api` is the type's natural comparison: `any` on a set (naming a value is not claiming the set *is* it), `eq` everywhere else. Explicitly, `f.<slug>=<op>:<value>`; a set operator takes a comma-separated list (`any:api,ui`) and `range` both ends (`range:3..8`; one end is `gte` or `lte`). A value whose own text begins `<scheme>://` is a value, not an operator call, so a `url` field can be filtered by what it holds.

A filter finds tasks holding a value from that field's own values, so a rare value is cheap to ask about anywhere. **Sorting, grouping or totalling by a field** asks what the listed tasks hold and reads each listed task's own value, costing the board or list in front of you, not every value ever set on the field.

**A saved view's query is parsed when saved**, not opened: a save runs the parameters through the grammar `list_work_items` uses and refuses what does not parse, naming the key, so nobody finds a broken view weeks later unable to tell a typo from a grammar change.

**A view's parameters are the query grammar's own**, which `list_work_items` compiles its arguments into. Four are spelled differently: `project` is `container` (takes `project:ENG` or `workspace`), `text` is `q`, `label` is `tag`, and `open_only` is `status_group=not_started,active`. Everything else is the same word, and a custom field is `f.<ref>`. A view may not carry `view`, `cursor`, `read_level`, `max_lag_seconds`, `max_lag_seq` or `min_position` at all: they concern the caller's own read (where it resumes, how fresh), not the rows.

**A saved view is run by its id**, passed as `view` to `list_work_items` or as `?view=` on the REST route. Anything else the caller passes overrides the view's own, so a seat can open somebody's board and narrow it without editing it.

Views are read at `GET /worklet/views?container=project:ENG` and written through the operator MCP surface with `save_work_view` and `delete_work_view`, **not** by a seat: a view is furniture, and a seat's job is the work.

**Who may save one follows what the view is**, never who is asking; the payload decides, so there is no second verb to ask the easier question:

| The view | Who may save it |
|---|---|
| **Personal** (saved with `personal: true`) | its owner is always the **caller**, under their own record (seat when bound, login when not); naming somebody else's is impossible. Replacing one by `id` takes that owner, or `admin`. Not their lead: only its owner sees the strip |
| **Shared on a project** (`project:ENG`) | the project's lead, or `admin` |
| **Shared on a unit** (`unit:engineering`) | whoever leads that unit, directly or from anywhere above it, or `admin` |
| **Shared on a person's page** (`person:ana`) | that person, whoever leads them, or `admin`: it is a tab on a page other people read |
| **Shared on the workspace** | `admin` alone: every person in the company lands on it |

**Replacing a view is decided on the view it replaces, too.** A save naming an `id` replaces that view whole, so the rule is asked twice: for the view being written, and for the view stored under the id, its owner and container as they are *now*. Otherwise anybody who may keep a personal view could name a project's shared tab, or a colleague's personal view, and overwrite it. The write refuses, as a conflict to re-read on, a save whose stored view moved or changed hands after that decision.

**An `id` names a view that exists**, one `list_work_views` lists or a save answered; a save naming an id no view holds is refused `not_found` rather than creating a view under whatever was typed. A new view is saved with no `id`: its id is minted from the call and answered back.

**A view is deleted with `delete_work_view`**, naming its `id` (or `DELETE /worklet/views/{id}`, or **Delete** on the view's page and in the Saved views list). Who may delete is who may replace it, decided on the view as stored now: the table above, by owner and container, and a protected view only its owner. A deleted view leaves every strip, the Saved views list and every pin, its `default` goes with it, and running it by `view` is refused rather than answered as an unfiltered list. A pin of a deleted view is dropped the next time its person's pins change. The builtins have no `id` and are never deleted. The view's row stays, stamped with who deleted it and when, so a late-delivered save cannot bring it back, and a save decided on it before the delete is refused as a conflict.

`default: true` takes the container's landing tab from whichever view held it, so a shared view is its container's decision. **Only a shared view can be the default**: the landing tab is everybody's, and a personal view saved with `default: true` is refused rather than clearing every other reader's landing tab for a row they cannot see. A person wanting their own view first pins it with `set_pins`.

### Manual order

A board is drag-ordered, and the order is a real value on the task. A person drags a card with `place_work_item`, through the dashboard or their own assistant; no seat holds it, because where a card sits is a person's arrangement, and a seat moves work between lanes with `update_work_item`. The call names the card it was dropped **beside** (`before` or `after`), never a position: the engine mints the new key inside its own write, between that card and the next as the board stands when the move lands, so two people dragging in one project at once both land where they dropped. The key stays on the task: its next edit keeps it, and reading the task returns it.

A drag within a lane writes one record, on the project's order, and wakes nobody. It never changes the card's `version`, so the board's next `if_match` on it still holds. A drag **across** lanes also changes the card's status, an ordinary status change (history, and a wake for the people on the card), written first as its own record, before the place. It is refused `stale_version` before anything lands if the card changed since the board was drawn. If somebody changes the card between the two halves, the new lane stands and the answer says `placed: false`, with a `lane_changed_not_placed` caveat (drawn beside the board in the dashboard; in `warnings` for an agent): the card is in the lane it was dropped into, at the place it already held there, and can be dragged again.

A drag is retried as the same call. Each half is a step of the call's one operation, so a retry is answered from the ledger step by step, the first attempt's lane change included. So a drag always sends the lane it was **dropped into**, whatever the card reads now: a drop into the lane the card is already in, on a card nobody changed, writes nothing on the task, so sending it is always safe. A drag is never a move to another project, which re-keys what it carries and is `move_work_item`.

**On the dashboard** a drag is made as the principal your session resolves to, and only in the manual order (a project's default, or **Sort → Manual order**), since in any other a dropped card would jump back to where its date or priority puts it; a drag in another order says so. The card is drawn where you dropped it until the engine answers: a refusal puts it back and says why, and a `placed: false` answer leaves it in the lane it went to and says the place was not taken. `Alt` with an arrow key moves a focused card the same way: up and down past its neighbours of the same project, left and right to the top of the next lane.

Repeated insertion at one point grows keys. A drop that would mint a key past 64 characters re-spreads the cards around it in the same record, widening the window until keys are short again, up to 256 cards. Past that the drag still lands and the engine **re-spreads** the project in the background, in batches, order-preserving; every intermediate state is the original order, so an interrupted re-spread leaves a correct board. A card dragged to the very bottom never takes the key the project's next new task will be filed at, so the two never share a place.
## What a seat can do

Nineteen tools, and they are deliberately few: ten that act on a task, three that read the container it is filed into, one that writes the one part of that container a seat owns, one about CHANGE rather than state, and four over the project's [files](#a-projects-files):

| Tool | What it does |
|---|---|
| `list_work_items` | the query surface above, filtered any way a view can be, with every row filtered **on its own** whatever a named view's shape says (the grammar's `collapsed` default would filter only the ROOT and let its subtree ride along, which draws a board and misreports a list). Subtasks are asked for with `parent`, which that mode never applied to, including `preset=my_queue` (the seat's own open work). Filters: `type`, `priority`, `parent`, `reporter`, `watcher`, `unit`, the date keys `due`, `updated`, `created`, `sort`, `cursor` for the next page, and **`field_filters`** keyed by field slug, which reaches the company's custom fields |
| `get_work_item` | one task with its recent comments, history, links and **custom fields**. `include` narrows to parts (`body`, `comments`, `history`, `links`, `fields`; all five by default), so each part alone fits one answer; a read naming other parts says the description was left out and how large it is. `comments_cursor` pages back through a long thread; **`comment`** answers one comment by id and nothing else. **Nothing is cut**: a comment page holds as many whole comments as fit 16 KiB (at least one), `comments_cursor` continues exactly where it stopped, and the description is whole. Each field value comes with the slug, name and type that explain it, and says when it is **hidden** (its declaration was archived), **foreign** (mirrored from another tracker) or **undeclared** (a value this company explains nowhere) |
| `create_work_item` | file a task or subtask. A subtask is filed in its **parent's** project when none is named (the caller's team is the default only for an item with no parent). It lands in `todo` unless `status` names another (a board lane's **+** files into that lane in one record; an unknown status is refused by name). Without an `assignee` it goes to the project's **default assignee** (the lead's `write_project` setting, read inside the create); it lands in triage, where the lead is told, only when the project names none or names a seat that has left the chart, which the answer's `warnings` say. The answer's `assignee` is who it was filed to; filed to the caller, it wakes nobody, the lead included ([who hears](#which-notices-start-a-turn)). `fields` sets custom fields by **slug**; the create is refused naming any the project requires and the call leaves out. It also takes the four **scheduling** arguments below, and `ask` (optional `decision`) to file the item **as a question**: see [Asking for a decision](#asking-for-a-decision) |
| `update_work_item` | change any field, with an optional `if_match`. `routing_unit` points the item at another team and belongs to the **lead** of the project the item is filed in (or an administrator), because it decides which team hears about somebody else's work: see [Which team an item belongs to](#which-team-an-item-belongs-to). `watch: true`/`false` is about the CALLER only; the engine resolves it against the current watchers in its own transaction, so following never removes an existing follower. `waiting_on`, `blocking`, `linked`, `linked_pages` and `linked_entries` are **set-valued** (see below); `fields` sets custom fields by slug, checked against each field's declaration. It takes the four **scheduling** arguments, where `null` CLEARS one. An `assignee` may carry a **`reason`**: one line, at most 500 characters, which the new assignee is woken with and the history shows beside the hand-off; a `reason` without an `assignee` is refused (explaining any other change is a comment). **`checklist`** makes one change to the item's checklists: see [Checklists](#checklists) |
| `create_work_item` and `update_work_item` | both take `fields`, keyed by field **slug** — see "What a field value may be" above |
| `comment_on_work_item` | add to the thread, optionally as a **question** somebody owes an answer to (`ask`, with a `decision` when they must choose) or as the **answer** that closes one (`answers`, with a `choice` naming an option; `body` is then optional) |
| `request_access` | ask a person for one more kind of access, named from a closed list (watching the engine's health, or any permission the seat's GitHub App can hold, at any level the Agents tab offers), as a decision on the item that needs it: the grant or declining; or one permission at an integration beside its level (`integration`, `permission`, `scope`, `hours`), as grant for this change, grant permanently or not now. The person sees the exact change before it is made, and nothing changes until they apply it ([ADR-0118](../../adr/0118-an-agent-asks-for-access-as-one-grant-a-person-previews-and-sends.md), [ADR-0150](../../adr/0150-an-agents-access-at-an-integration-is-its-level-plus-grants-a-person-makes.md)) |
| `search_work_items` | find an item by what it **says**: ranked over every title *and description*, which no filter reaches. `list_work_items`' `text` is a substring of the key or title only, so the two are different questions (narrow a board vs rank a corpus). A `text` that is exactly one task key, in any case, answers that task first (the current key or one a move or rename left, opened as a point read), then the ranking without it, because the words index splits a key into pieces that would rank every mention above the task itself. `mode`: `hybrid` (default) fuses the words and, where the company has an embeddings provider, the meaning; `keyword` ranks by words alone, for an exact name or error text; `semantic` by meaning alone (see [Search](search.md#three-modes-and-what-an-answer-says-it-served)). `text` is at most 400 bytes, the bound every search shares; a longer one is refused naming the limit. The answer says the `mode` it **served** and carries a `degraded` sentence whenever that is not the one asked for (a hybrid search with nothing to rank by meaning ranks by words and says so; a `semantic` one runs nothing and says that this tells the seat nothing about whether the work exists). A node still building its index says so rather than answering empty, since "there is nothing" gets a duplicate filed; an answer over part of the fleet carries a `partial` sentence for the same reason |
| `merge_work_item` | fold a duplicate into the surviving item: the duplicate is linked to it, its **subtasks are re-parented onto it** (`move_subtasks`, true unless you say otherwise), and it is closed as `cancelled`. Nothing is destroyed; both histories stay readable. (Closing by hand leaves subtasks under a closed parent where nobody finds them.) A subtask lives in its parent's project and is drawn on neither board under an item in another project, so a merge whose duplicate **has subtasks to carry** into another project is refused before writing unless `move_subtasks` is false: first `move_work_item` the duplicate (subtasks go with it), or merge with `move_subtasks: false`. A duplicate with no subtasks merges across projects either way. Also refused: moving the subtasks onto one of them (a subtask would end up under itself); both items must be live, so a merge of an item in the **trash**, or into one, is refused either way (restore first), and a trashed subtask is frozen and stays under the duplicate, where a restore finds it. It answers **unavailable** while a **cross-project move has not yet carried** the duplicate or survivor (the walk is about to change that end's project; the same call a moment later is decided on where it went). A merge that stops part-way (node died, stream refused an append) is finished by the `tracker` duty on its first pass (every 15 minutes) after the merge's claim has lapsed (a minute after its holder's last heartbeat), never beside a running merge. Merges only a person can unblock wait, named on every tick with what releases them, without holding up the rest: a survivor trashed since (`tracker_merges_waiting_on_the_trash`) until restored, a duplicate trashed since (`tracker_merges_of_items_in_the_trash`) until restored (the trash takes no write), and a survivor since filed under one of the duplicate's subtasks (`tracker_merges_waiting_on_a_cycle`) until filed elsewhere. A merge whose survivor was purged has nothing to merge into: its mark is cleared and the duplicate stays open |
| `move_work_item` | move a top-level item, with everything under it, to another project: each task in the subtree is re-keyed (ENG-7 becomes OPS-3) with its old key still resolving, the subtree's tags are declared in the new project (a label the new project already gives a different tag refuses the move, naming both, before anything is written), and the people on the item are told. Authority is `routing_unit`'s: the **lead** of the item's project, or an administrator. A subtask does not move on its own; move its root. A move that stops part-way is finished by the same call, or by the `tracker` duty (see [how a move is carried out](#what-a-project-row-carries-about-its-work)) |
| `get_work_catalogue` | the types a task may be and the fields it may carry |
| `list_projects` | every project work is filed into, with `task_counts` (`{todo, active, done, closed}`), when its work last changed and who changed it, its `target_date`, and its lead. `archived` picks the set: `false` (default) live, `only` retired alone, `true` both. `sort` orders the whole company before paging (`key`, `name`, `unit`, `todo`, `active`, `done`, `closed`, `last_change`, `target`, each with an optional leading `-`; `-todo` is where the pile is). A seat's answer carries **50** and says `total` beside `truncated` (narrow with `q` or `unit`), because a tool answer is read out of the turn's context window |
| `describe_project` | one project in full: the six statuses and what each means, the types it files, the fields grouped by type (required first, with options), its tags and its lead. Omitting the project means the seat's own |
| `write_project` | a project's own settings. Declaring a **tag** is open to every seat; renaming or archiving one, declaring project fields, setting the default assignee, the **target date** and the [stuck window](#a-projects-stuck-window) are the project **lead's**; archiving the project is the lead's too, and never an agent's: see [Who may do what](#who-may-do-what) |
| `task_activity` | what HAPPENED, in log order: every change to one task or one project, who made it and exactly which fields moved |
| `my_work` | everything this seat is expected to look at, in one call; see below |
| `list_project_files` | a project's files in path order, a page at a time; `folder` narrows to the paths under one |
| `read_project_file` | one file's text, at most 48 KiB from `offset` with `next_offset` to continue; a file that is not text is described unless `encoding: base64` asks for its bytes |
| `write_project_file` | put a file at a path (created, or content replaced), with `if_version` to refuse the write if somebody changed it since you read it |
| `remove_project_file` | take a file out of a project; its content is deleted from storage |

### Whose words a read hands over

Most of what these tools answer was written by somebody other than the reading seat, and every read labels it (see [Text somebody else wrote](../concepts/agent-runtime.md#text-somebody-else-wrote)):

- **A change's text** (a feed row in `task_activity`, a history entry in `get_work_item`, a notice in `work_inbox`) is the comment, description or note (a hand-off's reason, a purge's line) the change carried, fenced under **whoever made the change** as `work_comment`, `work_description` or `change_note` by kind. A line summing up an answer that chose an option quotes the asker's option too, so it is never the answerer's words alone.
- **An ask** in `my_work` is fenced under the asker, and its decision is theirs; **a description and a comment** in `get_work_item` under who wrote them.
- **A name** (a task's title, a project's name and purpose, a view's name and saved filter, a file's path, a checklist line, what a change moved) is somebody's words too, and no row records who wrote it (anybody may have edited a title). It is defused in place, not fenced, and the answer's `provenance` envelope reads it as a colleague's with nobody named. So a task the seat filed and commented on alone is never labelled wholly its own: its title may not be.

The catalogue (`get_work_catalogue`) is the company's own configuration, written only by an administrator: defused, not labelled as anybody's.

### When a task is due and how big it is

Both write tools take four scheduling arguments, each the input to something the tracker already reports:

| Argument | What it sets |
|---|---|
| `due` | when the task is due. A date (`2031-04-16`), an instant, or a relative word the `due=` FILTER reads: `today`, `tomorrow`, `eow` (midnight ending Sunday; a week starts Monday), `eom` (midnight ending the month), or an offset like `+7d` (refused outside the years 0000–9999). One grammar for filter and write, so a seat that can ask for "due this week" can say it about one task. A token naming a DAY sets the all-day flag, so a renderer shows "16 April", not "16 April, 00:00". |
| `start` | when work on it should start, in the same spellings. |
| `estimate_minutes` | how long it is expected to take. |
| `points` | how big it is on the team's own scale. |

`update_work_item` also takes a **wait until a date**: `wait_until`, in the same spellings, sets the item's `start`, with a required `wait_reason` (one line, at most 200 bytes, saying what it waits for; the item reads it as `wait_reason`). The seat holding it is not handed it before the date (unless the reason waits on a person nobody asked, below) and is handed it once when the date comes. Refused with nothing written: a wait without a reason, a reason without a wait, a wait beside `start`, a date that has passed. `wait_until: null` ends the wait. A `start` moved or cleared without a reason drops the old reason. The item's page shows the reason under its start date, as **Waits for** until the date and **Waited for** after.

A date is for a real date (a release window, an agreed start). **A wait on a person is an ask**, never a date: the date passes whether or not they answered and nothing reaches their Inbox. So an agent's `wait_until` whose `wait_reason` names a person is refused with nothing written unless the agent already holds an open ask to that person on the item; ask with `comment_on_work_item` and `ask` instead, which holds the item until they answer ([ADR-0122](../../adr/0122-a-pickup-is-answered-with-a-typed-outcome-and-a-wait-on-a-person-is-an-ask.md)). A reason names a person by `@handle`, in any case, or by the org chart's name as a whole phrase in the chart's case; a bare handle or a name in another case is not a person (the release "will be cut Friday" waits on nobody, whoever holds the handle `will`). A person whose access the directory withholds is not matched. A person's own wait is written as given.

**A wait on a person nobody asked comes back.** An agent's wait whose reason names a person the setting agent has not asked on the item is ready for the item's holder before its date, however and whenever it was written: the [work pickup](../concepts/scheduling.md#the-work-pickup) hands it over naming the person, and the holder asks them, or restates the reason without naming them where the date is real and waits on nobody. The item is still handed once more when its date comes.

**A review names its reviewer.** An agent that moves an item it keeps into `in_review` names `reviewer` with `update_work_item`, since a seat never reviews what it submitted and a review nobody holds waits on nobody ([ADR-0124](../../adr/0124-a-question-and-a-review-say-whom-they-wait-on.md)). A colleague seat named `reviewer` is handed the item as an assignment. A person named `reviewer` must already have the agent's open ask on the item (`comment_on_work_item` with `ask` and a `decision`), which is how the review reaches their Inbox; the item stays with the agent until they answer. Handing the item to a colleague seat in the same call names its reviewer by itself. Refused with nothing written: a move with no reviewer, the agent itself, a person not asked, or a reviewer beside a different `assignee`; a person's own move is written as given. An item in review that its holder submitted with nobody reviewing it comes back to the holder through the work pickup, to act on the review or name one.

`estimate_minutes` and `points` are the two halves of a size, both carried because which one a team uses is its own habit; the totals (`totals=points:sum`, `totals=estimate_min:sum`) and the workload answer both.

On an **update**, `null` clears the value ("leave the due date alone" and "no due date any more" are different edits). On a **create** there is nothing to clear, so `null` is ignored.

A value this grammar cannot read is **refused naming the argument**, never dropped: a silently ignored due date reports `applied` and a model reads it as set. The zero instant (`0001-01-01T00:00:00Z`) is no date either (stored, it would be the most overdue task ever), and the tracker refuses it on a create, an update and a comment's resolved time, from whichever node. A cleared date is said by its own flag (`clear_start`, `clear_due`), never by a date.

`my_work` is the call a turn opens with. It answers **eight lists**, each a different claim on attention: the seat's priorities in the order somebody put them, the work it holds, the questions waiting on its answer (each with the literal call that answers it), the checklist items it claimed on **other people's** tasks, the work it was brought onto without owning, what moved on what it follows, what just became workable, and **what it is holding up** (open work it holds that other open work waits on, the most waited on first). One call, because assembled separately a seat could see a task in `assigned` that had moved out of it by the time `priorities` was read. It takes **no handle**: the seat is the turn's own, since a tool naming whose day to read could read a colleague's queue.

Each list is a **page of at most 20**, and the answer carries `totals`: one `{total, capped}` per list, counted by the same predicate in the same read (a count from a list's length would say 20 for the seat holding two hundred). `capped` means the count stopped at 10,000, so the number is a floor. `priorities` is cut **after** finished and removed entries are dropped, so it still shows its first twenty open entries in order.

A `holding_up` row is the task plus:

- `blocks`: how many open items wait on it, directly or through another waiting item, each counted once; `blocks_capped` says the count is a floor, past 512 waiting items or past the 2,048 items one read walks (shared by the whole list).
- `holders`: who holds that waiting work, the reader left out, at most eight.
- `held_since`: when it last changed hands (its filing, an assignment, or a status change that handed it over; for an item held by an open question, when that question was put; absent where neither is recorded).
- `agent_waiting`: how many of the items waiting through it agent seats hold.

It is the one list about who waits on the reader, and is the same for a person, whose dashboard shows it under My work. The reader holds two kinds of item: one **assigned** to them that open work waits on, and one whose holder put an **open question** to them (`asked`, the question's comment id), where the item and everything waiting on it waits on their answer ([ADR-0124](../../adr/0124-a-question-and-a-review-say-whom-they-wait-on.md)). The list ranks the first 100 of each (assigned in the queue's order, asked newest question first) by how much waits on them. Beside it, `holding_agents` sums over agent work across every item walked, not the page: `items` (held items that agent items wait on), `agent_items` (distinct agent items waiting, each once however many chains reach it) and `capped` (a bound was reached, so both are floors). The dashboard's Inbox leads with "You hold N items that M agent items wait on" when M is above zero. What to do with a row is the tracker's own actions, taken by the reader.

`task_activity` is the only way to ask what CHANGED (a board shows what is there now; a task reassigned twice and back looks untouched). Its order is the **log's**, so `since` and the cursor are log positions, which lets a cursor span a reanchor with no gap and no repeat. `q` needs either a task, or a project **and** a `since` inside 90 days (an unscoped text search reads every change ever made and has no cheaper mode). `kinds` are the change kinds below and nothing else: an unknown one is **refused** naming every kind the tracker has, never answered with an empty feed. A change carries one kind, so one write moving several fields is filed under one of them (handing an item over and setting its status in one update is a `status` change whose `fields` carry the assignee too). A kind that names a field (`status`, `assignee`, `collaborators`, `watchers`, `tags`, `routed` (the team), `relations` (any link)) therefore keeps every change that moved that field, whatever kind it was filed under; a creation stays `created`, though its `fields` carry every value it was filed with.

**Every change carries its own before and after.** A row says the KIND of change (`status`, `view_saved`, `project_updated`, …) and, in `fields`, which values moved and from what to what: `{"status": {"from": "todo", "to":
"in_progress"}}`; for a **set** (watchers, a kind of link, tags, a catalogue's declarations) which members joined and left:
`{"watchers": {"from": "", "to": "", "added": ["ada"], "removed": ["bo"]}}`, each list sorted and every member whole. This holds for every kind, not only task ones (a project reconciled from the org chart names the purpose or unit that moved, a saved view the query parameters that changed, a re-ordered priority list the order before and after, a dependency the item it now waits on or no longer does), and for **quiet** changes too (most project, view, catalogue and tag edits wake nobody; the row still says what they did).

**What a task's own row can carry**, whatever kind it was filed under (one commit may move several; the row names each):

| Field | What it says |
|---|---|
| `title` `status` `assignee` `priority` `project` `type` `tags` | the columns a board is read by |
| `due` `due_all_day` `start` `estimate` `points` | the schedule and sizing. The flag is recorded beside the instant because an all-day date is stored as the company's own midnight, so making a midnight due date all-day moves the flag and leaves the instant |
| `wait_reason` | why the item waits until its start, whole; recorded on its own when a wait is restated on the same date with another reason |
| `reporter` `watchers` `muted` `collaborators` | who filed it, follows it, opted out of hearing, is doing it with you (the last three as handles that joined and left) |
| `parent` `routing_unit` `archived` `removed_with` | where it sits, whose lead hears about it, whether it is filed away, and (when a removal cascaded) the item it went with |
| `waiting_on` `linked` `duplicates` `page` `blocking` | the four kinds of link it authors and the mirror a blocker carries, each as the items that joined and left |
| `checklists` | one entry per named list, as `Setup: 3 of 5 done`. A list that arrived is on the `to` side alone, one deleted on the `from` side alone |
| `fields` | custom values that moved, by **slug**, as `severity=high`: a choice by its option's slug, a multi-valued field's members joined with `/`, a value past 150 bytes as its size (`severity=4096 bytes`), and a count of any whose field this project no longer declares |
| `body` | that the description changed and its new size (`980 bytes → 1204 bytes`, `— → 1204 bytes` written, `980 bytes → —` cleared), never the prose |

Values are deliberately the **stored** form (a status slug, a whole timestamp, an item's id), not what a screen shows: every data node writes the same row, and a rendering would depend on the reader's time zone and the company's current vocabulary; the dashboard resolves them. Nothing is **cut**: a set records only what joined and left it; an ordered list (a priority queue, the checklists) is carried whole because its order is the change; a list whose members a person does not read (an inbox) is recorded as its size; free text past 600 bytes (a project's purpose) is described by its size. The two largest things a task holds are **marked rather than carried**: a description (up to 32 KiB) and a checklist tree (256 items) are recorded as moved and by how much, and the change record stored beside the row holds the rest. A row records **every** field a change moved; only a notification card stops at 32, and says how many more there were.

**A notification card carries all of that but the custom fields.** The card is built at the write; naming a custom field takes the project's catalogue, which only the applying node has open. So a wake for a custom-field edit says a field was edited, and the history row says which and what it became. Everything else (a watcher added, a checklist ticked, a description rewritten) reads the same on both.

**And the woken seat reads them.** A change wake's prompt carries a **What changed** block: one `field: from → to` line per delta (`field: added …; removed …` for a set) in the engine's field names, an em dash for an empty side, a closing line counting fields the card had no room for, and the change's own excerpt where there is one. (Without it the seat learned only "The status changed by ana." and the value moved **from** was nowhere on the task.) A comment is the exception: its body is the comment, quoted **whole** to every seat it reached, whether it named, asked or answered them or simply landed on a task they hold, filed or watch. The card in an inbox carries a 600-byte excerpt, marked where cut, because an inbox is a list read to choose what to open; the wake is the one message a seat acts on, so it is given the comment (or a new task's description) whole from the change record, never the card's excerpt. An excerpt somebody stated (a hand-off's reason, a purge's line) is shown as the text it is.

A dependency, a re-parent and a cascade removal record the other item by its **id**, because its key belongs to that item's own row and a history row is corrected by nothing. So the **answer** carries a `keys` map naming the items its deltas point at (every link a task authors except `page`, which names a knowledge-base page, plus the `blocking` mirror, a person's priority queue, and the `parent` and `removed_with` scalars), resolved when asked, not when the change was made. That lets a screen draw "Waiting on: added ENG-2" or "Parent: — → ENG-2". An item this node has not applied is missing from the map, and a reader falls back to the id.

The three project reads are a seat's for the same reason the catalogue read is: a create refuses a project the company does not have, an undeclared type and a required field left empty, and a model that cannot **read** that can only guess.

`write_project` is the one project **write** a seat holds, for one facet: declaring a tag. A project's name, purpose and owning unit are **chart-owned** (written only by the epoch apply from the org chart), so nothing writes them here; field declarations, the default assignee, the target date and renaming or archiving a tag are the **lead's** (or an administrator's); archiving the project is the lead's and **never an agent's** (a seat that leads the project is still refused). Each facet is decided inside the verb, and each refusal names what it needed.

The **default assignee** is who a task filed into the project with nobody named goes to; every create reads it inside its own snapshot, beside the archived flag and required fields, so a person's sheet, a seat's tool and an operator's assistant land the same task in the same place. Empty is the project's setting for **triage**, where the lead is told. A default naming a seat the chart no longer holds is not applied: the task goes to triage and the create's answer warns the setting wants changing, since a task filed to a seat nobody runs wakes nobody.

**An archived project stays reachable, and it is SELECTED rather than let through.** Archiving stops new items and keeps existing ones, so a directory hiding it would hide half the company. Every reader of the listing names one of three sets: live, retired alone, or both (`archived=false|only|true` on the API, `list_projects`' `archived` argument, and the **Active / Archived / All** segment on the dashboard's Projects screen). The middle one is what a two-valued flag could not express: asking for both and narrowing is a filter over a PAGE (the answer stops at 200 rows), so a company with more live projects than that showed no archived row at all. The listing's `total` counts the set asked for, never a wider one.

An empty answer says the *set* is empty, not whether the *company* is, so the listing also carries a **census**: `active` and `archived`, counted under the same `q` and `unit` as the rows and without the archival term. That lets a screen tell "no projects yet" from "every project archived" without asking twice, and tell a reader exactly how many projects wait under Archived.

An operator holds the same fourteen and more that no seat does, including `place_work_item` (see [Manual order](#manual-order)) and the two below. `remove_work_item` puts an item in the **trash** and `restore_work_item` takes it out, at any age. Both are decided by the item's **own** project and its **reporter**: whoever filed the item, the lead of the project it is filed under, or an administrator, each with the `worklet` app. A reporter tidies their own item and never `subtree: true`, which takes the items under it too and stays the lead's or an administrator's. The tool names an item, so it reads the project off the item and asks then. A project changes only through a move, so the write checks the project the decision was made on against the item inside its own snapshot; a decision made on any other project is refused as a **conflict** naming the item's project. A re-route through `update_work_item` is held to the same check. A removal hides an item from every list and board and destroys nothing: history is untouched and `list_work_items` with `removed: true` is the only thing that shows it. No seat holds either, because a seat that could hide work it did not want to do would mark its own homework in the one way that leaves no trace. Removing with `subtree: true` takes the children, each child's tombstone naming the removal that took it, so restoring the parent brings back exactly what that gesture removed, never a child already in the trash for its own reasons. Removing an item **alone** (all a reporter may do) leaves its children where they are: each stays on every list and board it is on, as the root of what is left and filtered on its own, until its parent comes back.

Both are one commit per item, parent first, so a subtree gesture can stop part-way (a step whose outcome is unknown, or the stream refusing an append). It then stops rather than carrying on, and the answer says so: the named item is removed (or restored), with `subtree_followed` and `subtree_total` counting how many of the items that go with it followed, and `subtree_stopped` saying why and what finishes it (a person is told the same as a `subtree_unfinished` caveat). The same call again finishes it: a task already where the gesture leaves it is nothing to do, so a second removal takes what the first did not reach, and a second restore brings back whatever is still in the trash with a parent already back. A restore of an item not in the trash with nothing in the trash with it says there is nothing to restore.

Neither is `purge`, which destroys every row on every data node and has no inverse. It is an administrator's own gesture, with a typed confirmation and a required reason: `crewlet worklet purge`, the dashboard's **Delete permanently** in the trash, or `POST /worklet/items/{key}/purge` on the human write surface. It is deliberately not a tool.

Five of those count as a **delivery**: create, update, comment, merge and move. A turn woken by an assignment answers by moving the task, commenting, filing the follow-up, or merging it into the item it duplicates, and the delivery gate knows that, so such a turn is not corrected and looped for "having done nothing". Reading is not delivering, which is exactly the turn the gate catches.

Seat tools read at the **`linearizable`** level: a seat that files a task and then lists its project sees it, because every read establishes the log's end first. Every write's answer carries the `position` its record landed at, which a client outside the engine (the dashboard, an operator's assistant) hands back as `min_position` to read the write back at a cheaper level. See [Read consistency](consistency.md).

### Optimistic concurrency

`update_work_item` takes an optional `if_match` carrying the version the caller read. Without it, the last write wins, which is right for a field an agent sets from its own work. With it, a concurrent edit is refused and the refusal carries the current version, so the caller can re-read and decide.

The body is different: a save must always state the version it edited, since no per-field merge makes overwriting prose safe.

Watching is not a field a caller sets either. The watcher list is a set, and a tool writing it whole would have to know every name on it, so `watch: true` says only "add me" and the engine resolves it against the current watchers in the same transaction. An item with more than 64 watchers refuses the next one: past that it is an announcement, and a comment on it wakes the company.

**Commenting makes you a watcher, and never fails because of it.** The commenter's watch is the same one-handle gesture, resolved against the current row (a whole-set write would discard somebody who watched or unwatched meanwhile). At the cap the comment lands and the watch is skipped: an explicit sixty-fifth watch is refused because the person asked and can be told; an automatic one is not, since refusing it would fail a comment for a reason unrelated to what they wrote.

**Leaving is never refused**, whatever the set holds. Only joining is capped; applying it to an unwatch would strand a task that grew past the cap.
## Who is carrying how much

**The workload** answers what everybody is holding. It counts **open** work: every task assigned to them, whatever its dates, because "who is carrying the most" is about a whole queue.

**Both sizes are carried.** A team sizes in `points` or `estimate_min` by its own habit, so choosing one would be wrong for the other.

- **No invented ceiling.** No number is "full": thirty tasks is heavy in one company and a quiet week in another. The dashboard's bar is drawn against the **heaviest queue on screen**, a comparison between people.
- **No re-ranking.** The engine answers heaviest first and a screen keeps that order, so two screens never disagree about who is on top.

Beside the totals it carries **blocked**, **overdue** and **unscheduled** work, since a person whose whole queue is blocked has a different problem from one who is busy.

**Load is the work that wants the seat now.** Each row counts its in-progress items by [flow state](#why-an-item-in-progress-is-not-moving), `flow:
{working, blocked, waiting, held, due, stuck, moving}`, and `load` is
`working + moving + due + stuck`. An item parked until tomorrow's health check, a blocked one or one held by a stopped seat is open work and no load: it would not move whatever the seat did.

`GET /worklet/workload`, optionally narrowed to one `unit`: the people whose open work sits in projects that unit owns.

## Why an item in progress is not moving

An `in_progress` item nobody is moving reads like one somebody is. So every item in progress carries a **flow**, derived by the engine and never stored as a status ([ADR-0162](../../adr/0162-an-item-in-progress-says-why-it-is-not-moving.md)). The first state that holds wins:

| State | Holds when | `cause` | `since` / `until` |
|---|---|---|---|
| `working` | a turn or an executor run of the assignee is on the item now | `turn`, `run` | since it started |
| `blocked` | the item has an open blocker | | since the [flow duty](#notices) recorded it blocked |
| `waiting` | its start is in the future, the assignee's own question on it is unanswered, or the assignee's run is parked on it waiting on a person | `start`, `question`, `parked` | `until` the start, with the wait's words as `reason` |
| `held` | the assignee cannot take a turn: paused, placed on no node, out of token budget, out of model limit, parked on another item waiting on a person, gone from the chart, or no assignee | `paused`, `unplaced`, `budget`, `limit`, `parked_elsewhere`, `departed`, `nobody` | since the stop or park began; with nobody assigned, since it entered progress unassigned or lost its assignee; `until` the limit's or budget's reset, where known |
| `due` | its start has passed and nothing of the assignee's is on it since: no turn of theirs on it ended and they wrote nothing on it | | since the start |
| `stuck` | nothing changed on it for its project's [stuck window](#a-projects-stuck-window) | | since the last change |
| `moving` | none of the above: the ordinary case | | since the last change |

`waiting` comes before `held`: a hold matters only to an item that wants a turn now, so an item parked until tomorrow on a seat limited until noon is waiting, and held once its start passes with the limit still on.

- **Where it is read.** `flow: {state, cause, since, until, reason}` is on every board row, the item's detail, `my_work` and a seat's ready work, and `null` on an item not in progress. [Workload](#who-is-carrying-how-much) counts it per seat. It is one SQL expression inside each statement those already run, so no screen or tool derives its own.
- **The live facts.** Which items a turn or run is on, which item a run waits on a person over, and why a seat cannot take a turn come from the live projection, in the ranking the seat's own row states (`paused`, then `unplaced`, `budget`, `limit`), so an item and its seat never disagree. Only a node serving the API (the `ingress` role, with an `api.port`) folds the projection, and it reads these once the seat placement, pause record, run record and a budget report have each been read. Every other answer says `flow_live: false`: its rows read no live fact, rather than reading every stopped seat's work as moving. A model limit is learned from its park: a node that started while a seat was parked reads it at the engine's next park or probe, as the seat's row does.
- **On the dashboard.** A card, a list row and the task's page draw state, cause and instants as sent: a badge for every state but `moving`, a **Flow** row in the task's rail, and load beside open work on a seat's Open work and the person picker.
- **In the agents' tools.** `list_work_items`, `get_work_item` and `my_work` carry each item's flow and the answer's `flow_live`, read by the data node
  that answers the call, and their descriptions say what both mean.
- **`departed` needs a chart.** An assignee is gone once the chart's seats have been applied to the tracker and do not name them; a tracker never told the chart departs nobody.
- **A departed seat's work is re-homed a day later.** The item keeps the seat's handle through the 24 hours a removal can be undone in, then the seat's retirement hands each open item to its lead or returns it to `todo`, unassigned, with a history note naming the seat. Each lead hears of the items it returned unassigned once, in one notice listing them: see [Seat Ownership § The removed seat](../concepts/seat-ownership.md#the-removed-seat).

### A project's stuck window

`stuck_after_hours` is how long an in-progress item may go unchanged before it reads `stuck`: a whole number of hours from 1 to 168, default 8 (a working day). It is the lead's, like the [target date](#a-projects-target-date): `write_project(stuck_after_hours: 24)`; `null` or `0` restores the default. `work_project` answers the window in force and `stuck_after_hours_set`.

### Notices

A flow state is never a status, so no item change wakes anybody for it. The **flow duty** records it and tells a person when it means nobody is moving the item ([ADR-0162](../../adr/0162-an-item-in-progress-says-why-it-is-not-moving.md)). Every minute it reads every item in progress, a page at a time, and:

- **records each change** of an item's flow state (`working` aside: working time is the turns' own record) as a `flow` record on the item's own flow subject. It never touches the item's row, version, history or `updated_at`, so recording wakes nobody. The item's detail carries the records as `flow_history`, newest first, its [past before the duty](#the-flow-before-the-duty) among them; a blocked item reads blocked since its change was recorded;
- **tells a person** when the state has lasted its threshold, as a notice under the reason `flow` in their [inbox](#a-persons-own-state):

| State | First told | Then |
|---|---|---|
| `due` | 30 minutes after the start passed | the project's lead, 30 minutes later |
| `stuck` | when it becomes stuck | the project's lead, a stuck window later |
| `held` | the project's lead, only when the hold costs something: the item is urgent (told at once), or it would be due past the 30 minutes or quiet past its window were it not held | nobody else |

The first person told is the assignee when the assignee is a person. **An agent assignee is never told**: its notice is the work pickup, which hands it the item on its priority lane with the typed answer a notice cannot carry. So the first person told about an agent's item, or one whose assignee has left the org chart, is the project's lead (the item's team lead where the project has none), and a lead is never told of its own item. The lead is told only if the item is still in the same state since the same instant. A hold that ends on its own on an item nothing waits for asks nobody to act.

Each notice is told **once per item and transition**: its key derives from the item, the state, the instant it entered it and the person told, so rereading every minute or a move to another node tells it once, while an item that leaves the state and re-enters it may be told again. The 30 minutes are the inbox's acknowledgement window (a wait plus a worst-case turn), so a due item handed to its seat at the start has had every turn the engine expects to run first.

**A lead's notice that no longer applies starts no turn.** An agent lead is woken by a flow notice as by any other, in the background; at dispatch the notice is dropped once its item was finished, removed or left the state it told of, and absorbed once a completed turn on the item read past it.

**Where the duty runs.** It needs the live projection, which only a node serving the API folds; without it a stopped seat's work reads due or stuck and a working item moving. So a node that serves the API and holds the tracker's data holds the duty, whatever its roles: it asks for the duty's lease only while its projection is live and gives it back the moment it is not, so a fleet whose API and workers run on different nodes still records and tells flow.

### The flow before the duty

An item in progress before the flow duty first ran has a past the duty never read. The **backfill** records it, once, as the duty would have: after each tick the duty walks a page of items in id order, and for each one in progress before the duty's first recorded change (and within the 30 days before it, the event store's history), it evaluates the same flow expression at every instant its past could change: each write to it, each turn on it starting or ending, its start passing, its stuck window running out, a blocker finishing, a question opening or closing, and its seat's pauses and model limits beginning or ending. Each change is recorded under the key the duty derives, marked `backfilled`, never `working` and never a notice, up to the moment the duty first read the item.

- **Dated by its past.** A backfilled change's `at` is the instant its past gives (the write, the start, the stop), not when the backfill ran, so durations hold for an item older than the duty.
- **It says which facts it had.** The item's history and turn segments are kept for good. Seats' pauses and model limits come from the events the fleet's event stores retain, read from every live data node; a change whose instant those events do not cover, or read while a data node did not answer, says `past.seats: false`, and a seat stopped then reads as free. No chart is read for the past (a roster keeps no history), so nobody is `departed` there.
- **Once.** The cursor is the highest item recorded, so a restart or hand-over resumes there and nothing is published again; once the walk reaches the last item it costs nothing. The duty's own reads never take a backfilled change for an item's latest, so nothing already recorded or told is recorded or told again.

## How the work has moved

**The flow** (`GET /worklet/flow`, `work_flow`) is the company's work as a series: for each of the last N days or weeks on the company's clock, how many tasks sat in each status group at the window's end, and how many were **completed** in it: a change from not delivered to delivered, so a cancellation is not a completion and moving done work to closed is not a second one. It walks the task history BACKWARD from today's census, undoing each status change, create, removal, restore, purge and project move, so its cost is the window's changes, not the whole history. Home's "Tasks in progress", "Completed" and "Tasks completed per day" all read it.

**Blocked has no history.** Whether a task is blocked depends on its blockers' rows and nothing records when it became so; the answer says `blocked_history: false` and carries today's blocked and overdue counts only.

## What the company did

**The company feed** (`GET /feed`, `company_feed`) is one feed of what the company did, newest first: work **completed** (with the task's tokens and turns, and whether a reviewer ever sent it back), work **filed**, work **handed on** (the hand-off count against its budget), pages published and schedules run. A create says where it was filed from, its **origin** (the chat surface and conversation the filing turn was woken on, "from Slack"), which the create record states. One cursor resumes every source where the last page stopped.

A schedule's entries are its **runs**, the ticks the scheduler dispatched. A tick deliberately skipped (a missed run outside the catch-up window, or one that came due while its seat was paused) is not something the company did, so the feed omits it; it stays in the schedule's own ledger (`GET /schedules/{scope_type}/{scope_id}/{name}/runs`, Company › Schedules), where "why did the standup not run" is answered. Runs of one schedule for one runner with nothing else between them are **one row** ("ran 12 times since 02:40").

## What a person can do

**The dashboard** renders the same queries a seat's tools use, against this node's own copy, and every answer says how far behind that copy is. Worklet's trail names its **scope**, All projects or one project, and that crumb is a button: press it, or **G** then **P** from any screen, to switch project, with the keys first and the start of a word in a name matching what you type. Beside the scope sit its **lenses**: All projects' are Projects, Tasks and History; a project's are Tasks, About, History and Files. A switch keeps Tasks and History open and keeps a list's shape (list, board, timeline, calendar or table); a saved view and filters stay with the scope they were set in. **New project** (on the Projects lens and in the switcher) takes an owner and a key and continues in Company › Structure with the owner's editor open, where saving the key creates the project. A project is a key the company document declares on a unit or a seat, so setting one takes `admin`, a unit's lead included; New project is drawn for everybody and held with that reason for anybody without it.

- **Tasks** (`#/worklet/tasks`) is the list, and opens as one: a container with no saved default view lands on the list shape, because a board's value is the comparison across lanes, worst on a company with three items in one status. The board is one press away. Controls are one row, work right under it:
  - **What is drawn**: the five shapes (List, Board, Timeline, Calendar, Table), then the views you pinned (the strip holds what somebody SAVED, never the shapes), **+ View**, which saves the query on screen under a name (shared, or kept to you), and a link to the whole inventory.
  - **What narrows it**: a removable chip per narrowing, ending in **+ Filter** (priority by *is*, *is not* or *≥* a step, labels by *any of*, *all of* or *not*, and four dates: **Due**, **Start**, **Created** and **Updated**, each by the ranges that ask its own question). A narrowing the shape on screen does not apply stays as a muted chip saying so: a board draws every column, so a narrowing to one is "not on a board", and the calendar's own month is its due window.
  - At the far end, **Show** (**Open**, **Recent**: open work and what finished this week, the board's default so its Done lane holds the week's deliveries, **Closed** and **All**) and **Group by**, each showing its setting, on a board the lanes past its edge, and the count; then the search box (`/` focuses it) and **Display**, which holds the order (**Sort**, on every shape but the calendar) and what you set once: a list's second grouping, the columns of the list and the table (one grid with a column set each), what a board's cards show and the board lanes you put away. On a narrow window the row wraps whole groups of controls, and on a phone Show and Group by open from Display.
  - Grouped on a status, a status group or a priority, the board and list draw every value the company declares and say which are empty, since those are closed sets whose order means something; grouped on an assignee, a tag or a label they draw only values work is in; grouped by **Parent**, each band is one task's subtasks, headed with its key and title. A band holding more than its page ends in a link to the list narrowed to it, on the timeline as on the list.
  - An archived project's own Tasks lens lists its archived work. A view saved on a team or a person opens as its own query from the sidebar and the views inventory. The trash is a filter, not a tab: a listing carrying `removed=true`.
  - An empty list says which of three things emptied it: a narrowing that matched nothing, a scope with nothing in it, or a tracker nothing has been filed into. A complete one closes by saying so; one with more ends in **Load more** and how many of how many are loaded.
  - A card says what it has cost in **tokens**, what it blocks, its labels and, while a seat's turn is on it, who is doing what ("SWE · executing", "SWE · reviewing · round 2 of 4"), matched on the task the engine charges that turn to. Labels give way to a "+N" before the mark row wraps, so the token count keeps its place.

  **Every task has a menu** (the ellipsis at the end of a list or table row, a board card's corner, beside a timeline row's name; a calendar day's task opens its peek, with the same menu in its header, and a task's own page has it in the header's **More**). It holds **Move to project…**, **Merge into…**, **Add to my priorities** (or **Remove from my priorities**), **Move to trash**, and in the trash **Restore** and **Delete permanently**. Everybody sees these; whoever cannot make one sees it held with the sentence saying why, and a refusal is said in the engine's own words.
  - A move takes one top-level task and its subtasks to the chosen project, re-keyed there with their old keys still opening them; a subtask's entry is held, since it moves with its parent, and the lead of the task's project decides it.
  - A merge folds the task into the one you find by key or words (`merge_work_item`): linked to it as its duplicate, its subtasks move onto it unless you keep them where they are, and it is closed as cancelled. The dialog says so before you press Merge, draws the engine's refusal in place, and takes you to the task that carries the work on.
  - Add to my priorities puts the task at the end of your own list (`set_priorities` under your record's version), where My work's Priorities orders it.
  - A list or table takes a **selection**: tick rows (Shift ticks a range) and the bar over the grid trashes them all, or restores them all in the trash. Trashing a task with subtasks asks first whether they go with it (`subtree`); a task without any goes at once, and the toast carries **Undo**, which restores exactly what the removal took.
  - **Delete permanently** is an administrator's, one task at a time: it names what is lost, asks for the reason the project's lead is told, and is made by typing the task's key, the purge's own confirmation.

  **A task's links are drawn and taken off on its page.** Link a task asks what this task is to the one you pick: **Related to**, **Blocking** or **Waiting on**, the last two with an optional line saying why. Each task in the rail's Relations has a remove control for a reader who may change the task: a dependency comes off from either end, and a related link or duplicate drawn from the other task is taken off that task.

  **What you change here is made as you.** Drag a card (in the manual order, a project's default) to reorder it or move it to another lane (`Alt` with an arrow does the same from the keyboard); set a row's status, priority or holder from the value itself on the list and table. Each is a tool call under your name, conditional on the version you were looking at, so a change somebody made a moment ago is refused and the sentence names them. A task opened from a list carries that list: its page says where it sits ("3 of 18"), with the tasks above and below a press (or `k` and `j`) away, in the whole list rather than the loaded page, whether opened from its row or the peek's **Open**.
- **A task** (`#/worklet/{KEY}`) is one page: title and description, checklists, sub-tasks, then its **activity**, with a rail of its fields beside it. The activity is **All**, **Comments**, **Agent turns** or **Changes**: who filed it and who handed it to whom (with the reason), what was said, and a card for every agent turn charged to it ("SWE ran turn 2"): the phases it ran (and for a failed turn, which phase failed), whether a reviewer sent it back and what the reviewer asked for, the tools it called, how long it took, its tokens, and a link to its trace. While a seat's turn is running on the task, the foot says so ("SWE is on turn 3 · executing") with a way to watch it live. Each announced change has **Who this reached**: the people it woke and why. The three histories are read a page at a time and **Earlier activity** reads further back; the merged list only shows a stretch complete in all three. A long comment is folded behind **Show more**, except the newest, and a thread of six entries or more offers **Jump to latest**. The rail ends with **what this task has cost** (turns, tokens and agent time, from the task's own counters) and its hand-offs against the engine's budget.

  **Everything on it is changed as you**: a field from its value in the rail (status, priority, labels, dates, the estimate, a custom field), the title and description in place, a checklist item by its box, a sub-task from the sub-tasks' **+**, a hand-off with its reason, following it with **Watch**, and a comment, or with **Ask…** a question put to one seat, with options to choose between where they are to choose. A field edit is conditional on the version you were looking at, and a lost race names who won; a checklist tick and following are applied to the task as it is when they land, so they never lose a race that was not one. A reader who cannot act sees every control disabled with the sentence saying why.
- **New task**, at the end of every work screen's bar and on a board lane, opens one sheet that files a whole task in one `create_work_item`: project, title and description, type, starting status, holder, priority, due date and labels.
  - A lane's **+** files into that lane: it presets the lane's status, type, priority, project, holder or label (a status-group lane, the group's first status). A lane no one value files into (a due band other than *No due date*, a unit, and the Cancelled and Closed lanes) has no **+**.
  - Only what you set is sent, so a field left alone is the engine's default. Without an assignee, the sheet says where the task lands (the project's default assignee, else triage for its lead).
  - The Assignee field completes against the org chart; when the engine's colleague match names exactly one seat, it leads the list as the **best match**, and why it matched ("part of the name matches") is said under the field once you take it. A name typed and not chosen holds **Create task** until you choose the seat or clear it, so it is never dropped.
  - Every write form sends one press at a time: Enter or ⌘Enter while the first answer is out sends nothing more, so nothing is filed twice.
  - The sheet opens the new task once applied; a refusal (a label the project does not declare, a field it requires) is said in the sheet, naming the argument.
  - A title past 256 bytes or a description past 32 KiB is refused rather than cut, so the sheet counts both in bytes as the engine does, marks the field over, and holds **Create task** with the reason beside it. The engine's refusal names the argument and sizes (`` `title` is 600 bytes and a task's title holds at most 256
``) — for a model and a person alike, and without quoting the id minted for a task never filed.
- **Projects** (`#/worklet`, where Worklet opens) is the directory: every project with its lead, owning unit, maintained counts, how far along its work is (one bar split into done, active and still to do, over exactly those counts), its lead's **target date**, and when its work last changed. A row opens the project **beside** the list, and that panel's `Open ↗` is the way to the project's page (⌘-click or middle-click goes straight there). The sentence over the grid is the company's own total, and when the engine answered fewer projects than the company has it says so.
- **History** (`#/worklet/history`) is the change log over a window you choose, narrowed from one bar: the window, then **Kind** and **By**, quiet pickers with captions inside ("Kind Any kind"), whose options are what the loaded pages hold, with how many of each, and at the far end what is loaded: "Counts are over the changes loaded · Showing the latest N", then **Load older**. Every row is a change: re-applying an activation that already landed writes nothing, and a new activation that leaves a project's name, purpose and unit as they were is one quiet "org chart re-applied" row per project, naming the activation it moved to. The window bounds what the engine is asked for and a page bounds what one ask answers, so **Load older changes** fetches the next page back rather than asking you to move the window, and loaded pages are held still while you page: change the window or a facet to pick up what has landed since. One project's changes are its own History lens, a switch away in the trail.
- **A project** (`#/worklet/{KEY}`) opens on its work (the **Tasks** lens, which says how many are open, with the lenses on the page bar beside the project's name), with an **About** lens (who leads it, the owning unit, its target, census, and what it declares: statuses, types, labels and fields) and a **History** lens narrowed to it.
  - About's first line is the `purpose` of the unit that declared its `project` key; a project declared on a seat has none, so the line says which unit owns it. A project nothing has been filed into says so and offers **New task**.
  - **Edit project** sets or clears the target date, picks the **default assignee** (a seat from the chart, or nobody, which leaves new work to the lead's triage) and archives or unarchives the project (`write_project{target_date, default_assignee, archived}`). It sends only what you changed, and archiving names itself on the button and says what it does before it is pressed. The project's lead and an admin may; archiving is a person's, never an agent's. Anybody else sees it held with "Editing this project needs Admin access, or leading it." and the engine refuses the change all the same.
  - About lists the project's **Labels** with **Add label** (any colleague may declare one), and **Rename** and **Archive** on each (the lead's); archived labels are listed under them, since tasks filed under one keep it. The **Fields** card lists the fields the project declares, with **Add field**, **Edit** and **Archive** (the lead's), beside the company's fields its tasks carry. A field's slug and type stay once it exists, a dropdown's removed choice is archived rather than dropped, and archiving a label or field asks first, since neither comes back. **Files** lists the project's files with **Upload**, and **Replace** and **Remove** on each row (see [A project's files](#a-projects-files)).
- **Search** (`#/worklet/search`) ranks the company's work against a phrase: **Hybrid** (the default: words and meaning, each ranked then fused), **Keyword** (the words, BM25) or **Meaning** (`semantic`: what the text is about); each mode's tooltip says what it matches. A company with no embeddings provider asking Hybrid is served Keyword, and the screen and the command palette's mode pill say so ("Asked for Hybrid, served Keyword — …").
- **Saved views** (`#/worklet/views`) is the inventory of what somebody saved, in every project and team as well as company-wide: each row says where the view lives, with a **Pin** and a **Delete**, which asks first. A view you **pinned** is in your sidebar, and that row **runs** it; the view running on a board can be pinned or unpinned from its own strip too.

**Your own AI assistant** can reach Worklet over MCP too, at `/operator/mcp`. It serves the same work tools above, twelve more no seat is given (`list_work_views`, `save_work_view`, `delete_work_view`, `write_work_catalogue`, `get_person`, `work_inbox`, `mark_inbox`, `set_pins`, `set_priorities`, `remove_work_item`, `restore_work_item` and `place_work_item`), the five page tools and knowledge search, all the seat's own implementations with one field different: a write carries the **caller's** own name as its author: the seat's handle with the author kind `human` for a person (every person holds a seat) or a token the identity directory binds to one, and otherwise the credential's whole login (`token:ops`, `svc:ci`) with the author kind `operator` (see [who a write is attributed to](../reference/api-endpoints.md#who-a-write-is-attributed-to)). No caller can name a seat to act as: a tracker whose author field is chosen by the writer is not an audit trail. The personal tools are the caller's own for the same reason: `my_work`, `mark_inbox` and `set_pins` take no name, and `list_work_views` renders **your** strip (the shared views, your personal ones and your pins) under the same name those are written with.

**Whether the call is allowed is decided exactly as a seat's is**: every tool on that surface goes through the same authority table, over the same chart, and a refusal names the rule. Only who is asking differs: a caller's permissions come from their credential, and the **lead relations** from the seat the [identity directory](../concepts/identity-and-access.md#the-binding-has-two-ends-and-only-one-of-them-arbitrates) binds them to, so the person holding the seat that leads `ENG` may re-route `ENG`'s work and declare its fields, and a token or service account bound to no seat reaches those only through `admin`.

**An argument a tool does not take is refused, naming it**, on this surface, in a seat's turn and on the HTTP routes alike, because the refusal is the tools' own. One sent as `null` is read as one nobody sent, and is not refused (see [how a call is read](tools-and-mcp.md#a-call-is-held-to-the-schema-its-tool-advertises)). A tool reads only what its schema declares, so an undeclared argument used to be dropped: an assistant still sending `save_work_view`'s retired `owner` had a personal view saved as a shared tab under an answer saying it had worked.

A credential the identity directory **binds** to a human seat acts as that seat everywhere a tool asks about the caller: who wrote, which project a create with no `project` files into, whose day `my_work` and `list_work_items` answer for, whose watch a `watch: true` records, and which projects it may re-route work out of. See [A person's own state](#a-persons-own-state).

**The dashboard writes through the same tools**, at `/operator/act/{tool}`, one tool per request, as the principal your session resolves to: the seat you hold, or for a session exchanged from an API token bound to no seat, that token's own login. Nobody is refused for being unbound; every tool is decided by the same authority table, and each request carries its operation in an `Idempotency-Key` header, so a retry is the same write. The record is the same either way, so a change on screen and one your assistant makes read alike in every history. See the [API reference](../reference/api-endpoints.md#operatoract--the-dashboards-write-surface).

**The REST API** serves the read side at `/worklet`, `/worklet/{id}` and `/worklet/views`. Writes go through a seat's tools, the operator MCP or [the human write surface](../reference/api-endpoints.md#the-human-write-surface), each attributed to somebody. `/worklet` and `/worklet/views` answer their personal parts (`preset=my_queue`, `preset=priorities`, the pins and personal views that order a strip) for the **caller's own record** and take no parameter naming anybody else's. A caller's own record is kept under the one name every write of theirs is made under (the seat a person holds, or the login of a token or service account bound to none), so a pipeline token's strip shows the pins its assistant set. The questions *about* one person (their record, day, inbox) do take a handle, and naming somebody else's is decided as reading their record is: that person, whoever leads them, or `admin`, with a `503` rather than a refusal from a node that cannot read its chart. A handle that is somebody's **login** names their record (the seat the directory binds them to, or the login for a service account bound to none), so a lead may name a report either way, and a login is never mistaken for the seat it resembles. The tools that take a handle (`get_person`, `work_inbox`, `set_priorities`) resolve it the same way.
## Who may do what

One table decides every gesture above, for a seat's own tools and the
operator's assistant alike. Worklet is one **app**; its permission is `worklet`.
Every agent seat holds it; a person holds it when an administrator ticks it
([Access](../concepts/identity-and-access.md#access-admin-and-one-permission-per-app)).
`admin` includes it and overrides every relation; no seat carries `admin`. A
**lead** is whoever the org chart says leads the thing: a unit's effective lead
(inherited from above when the unit names nobody), the seat whose own project it
is, or, for a person, anybody above them in the management chain. **Every
relation is asked beside the app**: a lead, reporter or record owner who does
not use Worklet is refused `[worklet]`.

| Gesture | Who may |
|---|---|
| Read the board, a project, the catalogue, the views | `worklet`, which every seat holds |
| Read one task | `worklet`, **or** the person an open ask on the task names (see below) |
| File, update, merge and relate tasks | `worklet`, which every seat holds |
| Comment on a task | `worklet`, **or**, for the answer to an open ask that names you and nothing else, the person asked |
| Declare a tag on a project | `worklet` (any colleague) |
| Point a task at another team (`routing_unit`) | the lead of the project the task is filed in, or `admin` |
| Move a task, with its subtree, to another project (`move_work_item`) | the lead of the project the task is in now, or `admin`; the same decision as a re-route without the re-key |
| Declare a project's fields, set its default assignee, rename or archive a tag | the project's lead, or `admin` |
| Archive the project | the project's lead or `admin`, **never an agent**, whatever it leads or holds |
| Declare task types and workspace fields (`write_work_catalogue`) | `admin` |
| Save a view | see [Views](#views): a personal view is its owner's, a shared one its container's |
| Read somebody's day: their inbox, queue and record (`work_inbox`, `my_work`, `get_person`) | that person, whoever leads them, or `admin`, with `worklet`, your own included, since it lists the company's tasks |
| Set somebody's priorities | that person, whoever leads them, or `admin`; a **seat** never sets anybody's but its own |
| Mark an inbox, set pins | the person whose record it is, or `admin`; never a lead. The two tools take no handle, so a caller only writes their own |
| Place a task in the manual order (`place_work_item`, the board drag) | `worklet`, **never an agent**: a seat files work and moves its status; the board's order is a person's |
| Remove and restore a task | the task's **reporter** (whoever filed it), the lead of its project, or `admin`. A reporter tidies their own task, never a subtree beneath it; no seat is given the tools, see [What a seat can do](#what-a-seat-can-do) |
| Purge a task | `admin`, with a recent proof, **never an agent**; see [below](#removing-deleting-and-purging) |
| Ask a colleague (`a2a_ask`) | an agent seat whose role offers it, with no permission, **never a person or a token**: it spends that colleague's turn |

**You may answer what you were asked without Worklet.** A colleague's
[structured ask](#asking-for-a-decision) names the person it is put to, who may
read the task and post their answer (the comment, or the choice that answers
that ask) whether or not they hold `worklet`. Nothing wider: the tool finds the
open ask naming the caller and admits only that answer, which Worklet's writer
checks again is theirs. A plain comment, a new ask and a decision of their own
stay Worklet's, and a task that does not exist is refused exactly as one they
were not asked on is. Once answered, the task is Worklet's again, except to send
that same answer once more: the repeat is admitted, answered from the answer
that landed, and writes nothing. Your own asks are always listed among your
decisions, with or without the app.

**A node that cannot read its chart does not say no.** Every relation is asked
of the org chart this node runs; a node that is booting or runs no company yet
cannot answer. That is reported as *cannot tell* (a tool's refusal says this
node could not decide; a route answers `503`), not "you do not lead this", which
would send a lead off to ask for authority they hold. `admin` is checked before
the chart, so an administrator never waits on it. The whole table and the rules
outside the tracker are in [Identity and
Access](../concepts/identity-and-access.md#the-authority-table-one-function-decides).

## A handle is checked before it is stored

Every argument naming a colleague (a task's assignee, a project's default
assignee, whose priority list is written) is resolved against the company's
roster before the write; a name nobody has is **refused**, listing the seats.

An unknown handle otherwise fails *silently and permanently*: it is stored,
rides the change's routing snapshot, becomes a candidate, and the wake path drops
it against the live roster with no error and no log. The write answers
`applied` and the named person never hears anything.

A name is **resolved**, not merely checked: a role's name typed instead of its
handle gets the handle back.

### A seat is named by its handle

The tracker **stores** a seat's handle in every value that names somebody: an
item's assignee, reporter, collaborators and watchers, a checklist line's owner,
a comment's author and the people it asks and mentions, whose inbox, priority
list and pins a person's record is, a view's owner, a project's default
assignee, and a `people` custom field. A handle is immutable
([ADR-0013](https://github.com/crewlet/crewlet/blob/main/adr/0013-a-seats-identity-is-derived.md)):
a document that changes one removes the seat and creates another, so work filed
under the old handle stays the old seat's and the new handle starts with an
empty day. A name no seat answers to (a person's login, a Tier A token, a seat
since removed) is stored and shown as written.

### Dependencies, and the set-valued arguments

A **dependency** is the one relation with two ends. `waiting_on` is authored on
the blocked task; the blocker carries the dependent's id, so the record that
closes it names every task whose edge it clears without scanning the company.
That record tells nobody they are unblocked; [what wakes a
seat](#what-wakes-a-seat) says what does.

Both ends are written, in order: the blocker's row is read first, so a blocker
that is gone, tombstoned or already at its 64 dependents refuses the edge before
anything is published. Then the authored edge lands, then the mirror. The mirror
is **best effort** (the edge is durable without it), so one that lost its race
leaves a **one-sided** edge and the `tracker` duty writes the missing commit 30
seconds later. That repair is the only wake the blocker's side gets: the
authored commit routes to the *dependent's* watchers.

When the mirror can never be written (blocker gone, removed since, or full), the
edge is stamped **permanently** one-sided and never retried. Both states are in
the attention queue: `flag=one_sided` is the repair still pending,
`flag=one_sided_final` the one a person has to resolve. The `flag` filter takes
any number of values and matches a task carrying **any** of them. The duty logs
under `component=worklet`: `tracker_one_sided_repaired` with how many edges a
pass settled (`edges`), split into those it mirrored (`mirrored`) and those it
stamped permanently one-sided (`final`); `tracker_one_sided_final` for each edge
stamped, with the reason; and `tracker_one_sided_repair_failed` (a warning) for
an edge whose repair failed and is retried next pass.

**A task can wait on a pull request** on the company's GitHub: give its URL in
`waiting_on` (any tab of its page reads as the page itself). It must be under an
owner the company's GitHub configuration names, its organization or the owner of
a repository a seat works in; any other owner is refused naming them. The task
reads blocked until the pull request merges or closes. The engine records that
with no agent's turn: a `change_settled` record keeps the pull request among the
task's `changes` (its state, when, and whether the webhook or the poll learned
it), unblocks the task, and tells whoever holds it to update it, addressed. The
task's status is never moved. The engine learns it from GitHub's webhook, or
else from a fleet poll that asks GitHub about at most fifty open pull requests
every 15 minutes, through the agents' apps' cached tokens, once the pull request
settled more than ten minutes ago. A rate limit ends a pass where it stood and
the next resumes there. A pull request GitHub will not show the apps (deleted,
moved, out of reach) is logged once as `change_poll_unreadable` and asked again
after a wait that grows from an hour to a day; its tasks stay blocked until a
person removes the wait. Each settle is written once however many deliveries and
polls saw it. A wait on a pull request has no mirror, is never one-sided, and a
pull request cannot wait on a task. When the poll keeps settling what no
delivery did, the advisor says so to the owner as `integration.inbound_silent`
([ADR-0121](../../adr/0121-a-pull-request-a-task-waits-on-reaches-it-as-the-engines-record.md)).

**A wait nothing moves is a dead wait, and it comes back.** The same poll reads
whether anybody or anything is moving each open pull request a task waits on: a
reviewer or team asked for, a review submitted on its current head by anybody
but its author, a merge armed on GitHub, or an `ask_to_merge` call held for it
and still waiting on its answer. Where none holds, and the task has waited on it
at least 15 minutes (so a turn that waits and then asks for review is never
caught), the engine records a `dead_wait` on the task: the pull request, the
head it read, who opened it and whether it is a draft, kept among the task's
`dead_waits`. The record notifies nobody. A `dead_wait_ended` record follows once
something moves the pull request again; the record goes with the wait when the
pull request settles or the task stops waiting on it. A row's `waiting_on` edge
carries `dead: true`, and the board reads **Nobody is reviewing PR #12**. An item
whose every open wait is dead is ready work for whoever holds it, and the [work
pickup](../concepts/scheduling.md#the-work-pickup) hands it back once per head.
A pull request the engine cannot read (GitHub refuses it or its reviews, or the
held merge asks cannot be read) records nothing either way; the wait stays as it
was until a pass can read it
([ADR-0128](../../adr/0128-a-wait-nothing-moves-is-handed-back-once-per-head.md)).

**Every row says what it waits on.** A listed task carries `blocked` (one bit:
something holds this up) and `waiting_on`, the same edges carrying *which* task,
whether that blocker is still `open` (stated on every edge, `false` once the
blocker finishes), and whether the edge is `one_sided` or `one_sided_final`. Both
are computed from one set of rows in one statement, so `blocked` is exactly "some
entry in `waiting_on` is open". A blocker your own filter excluded is an id you
hold no row for, though the edge still names it by its `key` (with
`key_collision` where another task claimed that key first), so a project's list
says which task in another project holds a row up. Cleared edges stay on the row
when the blocker finishes: a timeline is for what a plan looked like once
executed.

`waiting_on`, `blocking`, `linked`, `linked_pages` and `linked_entries` on
`update_work_item` take one of two explicit shapes, never a bare list:

```json
{"waiting_on": {"add": ["ENG-7"], "remove": ["ENG-2"]}}
{"waiting_on": {"set": ["ENG-7", "ENG-9"]}}
```

A bare list is refused naming both, because its two readings are opposite: as a
delta it adds one edge, as a set it silently drops every edge not repeated.
`create_work_item` keeps a plain `waiting_on` list, since a new item has no
dependencies to replace.

### The atlas entries a task touches

Work that changes a server, service, database, domain or vendor account can say
so: `linked_entries` on `update_work_item` names the entries of the company's
[atlas](atlas.md) the task touches, in the same two shapes as every set-valued
argument.

```json
{"item": "ENG-42", "linked_entries": {"add": ["orders-db"]}}
{"item": "ENG-42", "linked_entries": {"remove": ["7f3c2a10-5b1e-5c3d-9a4f-0d2e6b8c1a57"]}}
```

- **Name an entry by its id, its exact name or its key.** The engine resolves it
  through your own read of the atlas and keeps the entry's id on the task, never
  its name. A name two entries answer to is refused, naming each by id.
- **A task names at most 32 entries.** Work touching more systems is about the
  company at large; link its project.
- **Adding takes the Atlas permission as well as Worklet's**, because nobody
  links what they may not read; an agent adds one only in a turn whose requester
  may read the atlas. Removing a link by its id takes Worklet's alone, so a link
  to an entry the atlas no longer holds can always be taken off.
- **Who sees them.** A reader who holds Atlas sees each entry on the task as a
  chip that opens it. A reader who does not is told how many entries the task
  names and is handed none, on the dashboard, over the API and in
  `get_work_item` alike. The task's history says how many entries it named
  before and after a change, never which.
- **Where it shows in the atlas.** The entry's Work tab lists the open tasks that
  name it and those finished in the last 30 days, and the map and catalogue mark
  an entry with open work, for a reader who uses Worklet
  ([reading the atlas](atlas.md#work-on-an-entry)).
- **What an agent is told.** A turn on a task that names entries is handed a few
  lines about each before it starts, ahead of anything its ask names
  ([Systems in this work](../concepts/agent-runtime.md#systems-in-this-work)).

The link is the task's own: recorded on the tracker's log with the task, nothing
written to the atlas
([ADR-0140](../../adr/0140-a-task-names-the-atlas-entries-it-touches-by-id-on-its-own-record.md)).

### A question on a task

A comment can carry an `ask`: a colleague's handle, meaning this comment is a
question that person owes an answer to. They are woken **asking for one**, not
told about activity, and start following the item. An ask does **not** hand the
item over and does **not** block a close. They follow only while a question here
is theirs to answer: once their last open question on it is answered or
resolved, or the item finishes, the watch the question added ends. A watch they
chose stays: watching or unwatching the item themselves makes it theirs, and
somebody already following it was never added by the question.

The answer closes it. `answers` names the question's comment id, and is
**inferred** when exactly one open question on the item is addressed to the
caller; with several it is required, and the refusal lists them. Answering wakes
the person who **asked**, not the one who just replied.

A question has **one** answer. An `answers` naming a question that already has
one is refused `already_answered`, naming who answered and when; read that
answer. The check is made in the write's own snapshot, so of two answers sent at
once exactly one lands and the other is refused.

Its author may correct a question's wording afterwards
(`PATCH /worklet/items/{key}/comments/{cid}`); that changes its words and
nothing else: an answered question stays answered, nobody is asked again, and
the person asked, the answer it closed and the decision it carries stay as its
post made them. A remark that asks or answers nothing can also be taken down
(`DELETE` on the same address), by its author or by an admin moderating: the row
stays on the thread with its words blanked so replies still read in order, and
neither the item's history nor anybody's inbox goes on quoting it. A question or
an answer is never taken down, since the ask would be left with no question or
closed with no answer: withdraw the question, or edit the words. A remark is
posted once: a re-run of the call that posted it finds it on the thread and
changes nothing (not its words, its answer, or who follows the item), whatever
has happened since, the item having been trashed included. It is answered as
that remark: the question it answered, with its choice, is the one its post
closed, never one asked since, and a remark that answered nothing still answers
nothing, so an answer sent again after it landed (a retry, or an approval
retried once its outcome was unknown) is neither refused for a question it closed
itself nor told it closed another.

`my_work` reads both sides: `asked_of_me` is the questions waiting on this seat,
and the `has_open_asks` filter finds the items carrying any. The board asks the
other two ways round: `asked_of=` is whose answer an item's open ask waits on,
and `asked_by=` is whose question it is (one name, since asks put through your
own assistant are written as your seat). Each row carries `answer_with`, the
literal call that answers it, and `open`.

An open question the facts overtook stops waiting. When its asker asks the same
person again on the same item, the older is `superseded`; when the item reaches a
finished status, its open questions are `moot`, and reopening the item makes them
wait again. An overtaken question leaves `asked_of_me`, the person's decisions,
`has_open_asks`, `asked_of=` and `asked_by=`, and an answer without `answers` is
inferred among the questions still waiting. Its card carries `overtaken` with the
reason, and an answer that names it is still recorded. A question somebody
answered is never overtaken. The Inbox says on a notice about it that the
question no longer needs an answer, because it was asked again or its item is
finished.

Only its asker closes a question without an answer, as a **withdrawal**:
`withdraw_ask` posts the reason as a reply in the question's thread and resolves
the question with a record naming that reply and, in `replaced_by`, what now
covers it, another work item or a link such as a pull request
([ADR-0124](../../adr/0124-a-question-and-a-review-say-whom-they-wait-on.md)).
The record is checked in the resolve's own snapshot: the reason must be the
asker's reply to the question, and a replacement must be an existing item other
than the question's own, or an http or https link. The question's card (`ask` on
a notice, `answer` on an answered decision) carries `withdrawn`: the reason's
words as they stand, its comment id, and the item or link that covers it. A
question resolved again later keeps the withdrawal. A seat's own open asks to the
person it is talking with are handed to its turn at the start
([Agent Runtime](../concepts/agent-runtime.md#your-open-questions-to-the-people-in-the-turn)).

### Asking for a decision

A question that needs somebody to **choose** carries a **decision** on the ask:
the question in one sentence, two to four options, the option the asker
recommends and why, the evidence it looked at, and the role the person is asked
in. The answer names the option it chose (`choice`, by the option's id) and may
say why in its body.

**Every question an agent seat puts to a person carries one.** An `ask` from an
agent seat that names a person (any seat the org chart does not hold as an
agent) is refused without a `decision`, on both asking tools, and nothing is
posted. So is an agent's comment that asks (`asks` true) and names a person's
seat in its words without making them its `ask`: the question would reach them as
a mention, never among the decisions waiting on them. A request for a person's
approval, review, input or decision is asking, so it is `asks: true` and needs
the `ask`. Naming a team, or a person in a comment that asks nothing, is a
reference and is posted. The advisor's `person.requests_without_ask` counts
comments by the same rule
([ADR-0122](../../adr/0122-a-pickup-is-answered-with-a-typed-outcome-and-a-wait-on-a-person-is-an-ask.md)).
A page comment that asks a person it names is refused the same way, pointing at
the work item the question concerns, since a page comment carries no decision. A
request an agent would make in words ("the merge is yours", "please run the
script") is a choice between the answers it expects, with the one it recommends:
the person answers with one press, or in their own words through **Something
else**. A question between two agent seats, and a person's own question, may
still be asked in words. A question asked with no decision is still drawn and
answered in words.

| Part | Rule |
|---|---|
| `question` | required, at most 300 bytes: one sentence on a card; context goes in the comment's body |
| `options` | 2 to 4, each `{id, label, detail?}`. An `id` is 1–32 of `a-z 0-9 _ -` because it is typed back in `choice`; a `label` is at most 80 bytes and a `detail` 500. One option is an approval: ask *yes* or *no*. Answering in words needs no option: the person always has **Something else** |
| `recommended` | required; one of the option ids. Shown marked **Recommended**, accepted with one press |
| `rationale` | required, at most 1,500 bytes: why the asker recommends that option |
| `evidence` | at most 8, each `{kind, ref, label?}`: `task` (by key or id, stored as the task's id since a key moves; the Inbox shows its current key before its label, so a label that starts with one of the task's keys is stored without it), `page` (by id or `CONTAINER/Title`; must exist and not be in the trash, stored as its id since a rename moves the title; a company not on Doclet cites a page as a `url`), `turn`, `run`, `artifact` (by id; the `url` an artifact tool answered for one is stored as this kind, so it is a link on any deployment's URL), or `url` (an absolute `https://` address) |
| `role` | `approver` (the answer **is** the decision) or `contributor` (an input to one somebody else makes) |
| `inform` | optional `{surface, channel}`: the chat channel the asker will report the outcome in, a promise the engine keeps (see below). Only an **agent seat** may ask with one (a person has no turn to hold to it): refused `forbidden` for an operator and `invalid` at the tracker for any author that is not an agent. `surface` is `mattermost`, `slack` or `chat` (the engine's own), and must be one the company runs **and** the asking seat can post on (a bot it holds on a vendor surface, or the seat itself on `chat`); `channel` must be one a unit in the org chart declares (`units[].channel`). A leading `#` is accepted and the chart's own spelling is stored |

The engine enforces only that a decision is well formed and recommends one of its
options, that an agent seat's question to a person carries one, that it rides
only the comment that **asks** (a remark cannot carry one, and it is never
changed after, since options edited under an answer would change its meaning),
and that a `choice` names an option **of the ask it answers**, checked against
that ask's own row. There is no approval chain, quorum or state machine: a
decision is one question to one person, answered once, and what happens next is
the asker's turn.

The asker is woken with the choice **by its label** (the card reads
`Chose “Ship Friday”: <the note>`), and the ask's row in `asked_of_me` carries
the decision with the recommendation already filled in as the `choice` of its
`answer_with` call, so a reader who agrees sends it as written.

**Asking.** `comment_on_work_item` takes `ask` and `decision` together (a
`decision` without `ask` is refused, since options put to nobody wake nobody),
and a comment either asks a decision or answers one, never both.
`create_work_item` takes the same two to file an item **as a question**: the
title is the question, the body its context, and the task and the ask land in
**one record**, so a crash never leaves an item with nobody asked. Whoever a
question asks **starts following** the item, on either tool, unless they muted
it, the one gesture that says they chose not to; the question still reaches them
under `asked`. An edit of the question does not re-watch anybody.

**Answering.** `comment_on_work_item` with `answers` and `choice` (the option's
id) answers it, and `body` is then optional: the choice is the answer, the body
its reason. The answer's comment **says what it chose** in words, opening with
the option's label and then the body (`Chose “Ship Friday”: the audit can
wait`), so every reader of the thread (an agent's `get_work_item`, the
dashboard, a notice) sees the choice without the ask's options beside it. The
tracker refuses a choosing answer that does not open that way, an edit that
drops it, and any new comment with no words. Leave `choice` out to answer in
prose when no option is right. A `choice` with no question to answer (nothing
named, and no open ask on the item addressed to you) is refused.

**The wakes.** The person asked is woken with the options listed by id, the
recommendation marked, the evidence, and the answering call written out whole
(item, the ask's comment id and a `choice`), so a seat edits one value and sends
it. The asker is woken with the question, the option chosen by its label, and,
when the decision named an `inform` channel, where it said it would report.

**An inform is enforced.** The asker's `answered` wake **owes** the chat surface
its decision named: the notification carries `owes` (`mattermost`, `slack` or
`chat`), and the turn it wakes is held to a delivery on that surface rather than
on the tracker. A round that only comments on the item is sent back with a
correction naming the surface, and the turn ends once a tool there has posted,
because the person who answered was told the outcome would be posted there. Only
the asker's copy owes it; a watcher woken by the same answer owes nothing. An
answer that owes a surface is its own inbox partition of the item's conversation
(the conversation key is unchanged), so two answers promising different surfaces
are never merged into one turn that could keep only one. If the seat has since
lost every tool on that surface, the obligation falls back to any delivery, like
every other surface the seat cannot reach.

A comment from somebody who is not the assignee, naming nobody and asking nobody,
leaves the assignee unasked, and the result says so in a `warnings` line, so a
commenter expecting an answer is not left with silence. Another agent's remark
starts no turn: it waits in the assignee's inbox until they next open the item
(see [Which notices start a turn](#which-notices-start-a-turn)). A reply in a
thread the assignee wrote in wakes them, unaddressed, which a turn may absorb
without replying. The warning tells the agent to pass `ask` or mention them, and
is read off the routing for the very record the comment publishes, so it cannot
disagree with who is woken. A person's remark carries no warning: it wakes the
assignee, who owes an answer. An answer to a question never warns, since it
expects no reply.

**What waits on a person** is one question (`GET /worklet/decisions`,
`decisions`): the open asks put to them (under the one name their record is kept
under: the seat they hold, which every person does, or a token's own login where
the identity directory binds it to none) beside the executors parked on a
question to them, with how many there are in all and when the longest began. With
no `handle` it is the caller's own (your own asks are listed whether or not you
hold `worklet`, since [you may answer what you were asked](#who-may-do-what));
naming somebody else's is a lead's or an administrator's to ask, with `worklet`.
The dashboard's Home answers them in place: each option of a structured ask is a
button that sends the choice, and a parked run is answered by its turn.

**What a person decided lately** is the other half (`GET
/worklet/decisions/answered`, `answered_decisions`): the asks put to them and
closed in the last seven days, newest closing first, each with who closed it (a
colleague may answer for you), when, the option chosen or the words, and the
asker's first record on the task since, which says whether the answer has been
acted on. An executor's question is listed for the person who answered it, and
only them (it may have been put to a whole team), in the same shape: what it
asked, its options and the one chosen, and when ([Mid-run
clarification](../concepts/code-sandbox.md#mid-run-clarification-ask_and_wait)).
It takes the same scope rule. The dashboard's Inbox keeps these in a closed
**Answered** group at the end of the list; an older answer is on its task's
thread.

### What an answer may weigh

A tool answer is read by a **model**: every byte lands in a context window. One
answer is capped at **64 KiB**, about a quarter of the smallest context the
shipped models offer.

Every collection which grows is paged:

- **Comments** come back newest page first, **every one whole**: a page holds as
  many as fit 16 KiB (at most twenty, at least one), and `comments_cursor` reads
  the page before. A long thread is more pages, never shorter comments.
- **The description** comes back whole, a part of `include` (`body`) like the
  collections beside it. A description and one comment are each capped at 32 KiB,
  so every part asked for alone fits.
- **Open asks** in `my_work` come back whole until the block holds 16 KiB of
  them, the first always; the rest carry their decision, author and the call
  that answers them, plus `body_not_included` naming the body's size and the
  `get_work_item` read that returns it, never a body cut to fit.
- **History** is the fifty most recent changes. `work_activity` is what pages
  properly, with a cursor that survives a reanchor.
- **Options** page by whole fields: a field whose list does not fit comes back
  with **none** of its options, because half a list would be taken for the set.
  `options_total` beside `options_shown` says otherwise.

An answer that still exceeds the ceiling (a long body, a full thread and
sixty-four links) is **refused** naming what would narrow it, never truncated: a
model handed half a JSON object fails to parse it or reads the half as the whole.

## What wakes a seat

A change that concerns somebody becomes a **notice** in their inbox and, when it
asks something of them, a **wake**: a turn on that seat, with a prompt written
for the reason it reached them (see [Which notices start a
turn](#which-notices-start-a-turn)). Most wakes are about a task: you were
assigned it, mentioned on it, asked on it, blocked by it.

**One wake per person per change**, whatever number of reasons name them: the
first reason in the precedence order wins and the rest are dropped. Somebody
mentioned on a task they watch hears that they were mentioned.

**An assignment to whoever already holds the task wakes nobody** when nothing
else changed with it. A real reassignment wakes the new holder once.

**A change is told by what it moved, not by the one kind its record names.** A
write moving several fields is recorded as one kind: `update_work_item` records
an assignment written with a status as a status change, and a watch written with
a priority, a due date or a custom value as a change of who watches. Each move
still reaches whom it concerns as if written alone. The seat the task was taken
from and its reporter hear of the hand-off, and the new holder is asked to answer
for it and told the task was assigned to it. The seats that answer for the task
are woken by a change written beside a watch. A dropped watcher, the new team's
lead, a parent's assignee and the holder of a checklist item each hear their
part, and a lead is offered unowned work whose status or team the write moved.
The wake's change line names each move of the assignee, status, collaborators,
watchers, tags, team and the task's relations ("The status changed by ana. The
assignee changed too."), and its "What changed" block lists every field the write
moved, as the engine stored it, read when the write lands, so a link, a custom
value and a change somebody else made a moment before are told as they happened.
An assignment to the seat that already holds the task hands nothing over,
whatever else it was written with.

**And nothing wakes you for your own write.** The author of a change is dropped
from its wake. A person the identity directory binds to a seat writes **as** that
seat, so the author a change carries is the same handle as the watch it leaves
and the assignment it names; a founder filing work through their assistant is not
woken by every item filed and comment left.

The single exception is **`unblocked`**, because it is about a *different* task:
that your own other work became workable is news however it came about, your own
close of its blocker included. The close itself does not carry that news. The
tracker's unblocked repair tells each dependent's assignee in a record of its
own, on its next pass after the close (every fifteen minutes), and the wake says
it comes from a repair.

Every wake names its author under the actor key every integration stamps, so the
notification service applies the same rule a second time before waking a seat,
and lets through the same one reason, read off the copy each person was sent
rather than the kind of change.

**A pipeline's token bounds what agents do for its work.** A task a CI token or a
service account files, assigns or comments on wakes its seats in turns worked for
that credential: the agent uses only the apps the token holds, so a `[worklet]`
token's task has the agent comment on it, update it and file follow-ups, and
never read or write a page or an artifact for it. The apps the token held where
it wrote ride with the task's record, and the node running the turn holds the
agent to whichever of those and its own answer for the token is narrower. Give a
pipeline's token the apps its agents should use for it, and declare it the same
on every node
([An agent uses only the apps of the person it works
for](../concepts/identity-and-access.md#an-agent-uses-only-the-apps-of-the-person-it-works-for)).
A token another node declares and this node does not, like a credential nobody
can place, is worked for as a stranger.

**A task's wakes are one conversation, named by the task's id.** Wakes about one
task that queue up while a seat is busy are merged into one digest turn, and
every turn a seat takes on a task serves that task's
[conversation](../concepts/conversation-sessions.md), so its next turn continues
the last session. Both are keyed on the task's **id** (`work:task:<id>`), never
its key: two tasks can hold one key (see `key_collision`), and a move to another
project gives a task a new key without ending its thread. A `prioritised` wake
joins the conversation of the task it put first.

The order puts **what this change did to you** ahead of **the role you hold**:
being @-mentioned, asked a question, having your question answered, and learning
that somebody's work now waits on yours all outrank being the assignee. Below
those come assignee, then reporter, then collaborator, watcher, and last the lead
fallback, which reaches a lead only when the change named nobody else at all.

**The lead fallback is about ownership, not audience.** It wakes the lead for
work with no owner: filed unassigned, left unassigned by whoever took the
assignee off (the previous assignee is told, and is no longer its audience), or
held by a seat the company no longer employs. A hand-off wakes only the new
assignee. A change the task's owner (its assignee
or a collaborator, never its reporter) makes to work it keeps, with nobody else
to tell, tells the lead in the inbox and starts no turn: filing work for
yourself, moving its status, commenting on it. A move into another project
still wakes the receiving project's lead, who has never heard of the work. A
task that needs the lead's decision says so with `ask` or an @-mention.

**What a task wake asks follows its reason.** Eight reasons ask something:
mentioned, asked, answered, a thread reply, assignee, reporter, blocking and
`prioritised`. The [turn
engine](../concepts/turn-engine.md#who-is-waiting-and-how-they-get-an-answer)
holds each to an answer, so none of their prompts tells the seat to stay out.
Only a wake nobody is waiting on (a watcher, a collaborator and the rest) is told
to stay silent when the task's assignee is outside its team. Only a seat that
**holds** the task (its assignee, or the assignee of the blocker somebody now
waits on) is told the tracker's own decline: reassign it to the right teammate,
or to its manager when it cannot tell. A seat asked about somebody else's task is
not handed that gesture, since the task is not its to move; it declines with a
reply naming who should answer.

One wake is not about a task, because the thing that moved is not a row on a
board:

| Wake | Who hears it | What it asks |
|---|---|---|
| `prioritised` | the person whose queue somebody else wrote | **an answer**: take it up, or say why you cannot |

A second is about a task and is listed apart because of what it says:

| Wake | Who hears it | What it asks |
|---|---|---|
| `purged` | the lead of the project the task was filed in | nothing: the task is gone from every data node and nothing restores it |

`purge_task` is the one operation with no inverse. The wake names the key, who
ran it and their stated reason, and nothing else: it quotes neither title nor
body, because the record outlives the rows and an excerpt would keep a copy of
what was purged on the log for its whole retention window.

`prioritised` **addresses** its recipient: being told what to do next by somebody
above you is an instruction, and silence on it is indistinguishable from a lost
message. It names the tool that answers its own question, `my_work`, rather than
pointing at the object it is written on, because a person's handle is not a task
key and a pointer at one costs a round and a failed tool call. A `purged` wake
sends you nowhere: the row is gone from every data node, so the tool would answer
`not_found`.
### Which notices start a turn

Every seat a change names gets its notice in the inbox it reads when it next
opens the task, and counts as having heard the change when the engine decides
whether a lead must hear it instead. Whether the notice also starts a turn
follows its reason and, for six reasons, whether the change asks something of
the seat
([ADR-0113](../../adr/0113-an-agents-words-wake-only-the-seats-they-address.md)):

| Reason | Starts a turn |
|---|---|
| `mention` | always, except an agent named in another agent's comment that states it asks nothing (`asks: false`): that @-mention is a reference; a person it names is addressed as ever |
| `prioritised`, `asked`, `answered` | always: the change was put to this seat, or answers its question |
| `unblocked`, `blocking`, `routed_to`, `parent_assignee`, `checklist`, `purged` | always: work the seat answers for moved |
| `flow` | always: nobody is moving an item in a project the seat leads ([Notices](#notices)) |
| `assignee`, `collaborator`, `reporter`, `lead_fallback` | for every change except another agent's comment, a person's comment that names somebody, and a change to who watches the task and nothing else; `lead_fallback` also not for a change the task's owner made to work it keeps |
| `watcher` | for a person's comment that names nobody |
| `thread` | for a seat with a stake in the thread (an open ask it made there, or one made of it that it still owes) on any reply by somebody else, whatever it names, except a reply by a co-asker (agent or person) whose only stake is awaiting the same seat; and for every participant on a person's comment that names nobody |
| `unassigned`, `unwatched` | never |

- **A person's comment that names nobody wakes everyone on the task.**
- **A comment that names somebody is addressed to them**: an @-mention, an `ask` or an answer wakes the seats it names; everyone else reads it in their inbox.
- **Another agent's comment wakes only the seats it asks**: by @-mention with `asks: true`, with `ask`, or by answering their question. An @-mention with `asks: false` is a reference, read in the inbox, waking nobody. A reply in a thread wakes only seats with a stake there (the asker of an open ask and the seat that owes it), so two agents' thanks to each other end at the first. An agent that needs the assignee or reporter to act names them; the comment tool's description says so and warns when a comment reaches the assignee's inbox and no further.
- A **reporter** hears of its task finishing or reopening, being reassigned or removed, and of every comment, and is woken by all but another agent's comment (those are when it verifies the outcome).
- A **watcher** gets every change in its inbox; only a person's comment starts its turn.

The line sits where turns were spent for nothing: seats woken by another agent's status note, or by an acknowledgement of their own note, changed nothing or answered with an acknowledgement that woke the first back. A seat woken by somebody else's change to a task it answers for (status moved back, new dependency, reassignment) acted on it.

## A person's own state

A human has a record of their own beside the work: an **inbox**, a **queue** and their **pins**, three parts of one document with three different authorities, so three separate writes. Writing your own takes no permission beyond the app; READING or writing any of it, yours included, takes `worklet`, because a notice and a queue name the company's tasks.

| Part | Who may write it |
|---|---|
| **inbox** — read, unread, snoozed, and how far you have read | only on behalf of the person whose it is, or `admin` (so a departed person's queue can be unstuck) |
| **pins** — pinned views and starred things | the same |
| **queue** — the order you mean to work in | yours, **or** whoever leads you, or `admin`; never a seat's for anybody but itself |

Somebody else marking your work read would remove it from the only place you would look, with nothing saying who did. A pin somebody else can set moves under you. A lead gets no way into either: re-ordering a report's work is a lead's job, marking their mail read is not. `mark_inbox` and `set_pins` take no handle, so a caller only ever writes their own.

The **queue is the exception**: telling somebody what to do next is what a lead is for. The lead relation is any ancestor in the management chain, so a founder leads everybody. A **seat** may not write somebody else's queue, even one it leads: that is a hand-off in disguise, bypassing the guarded take and the reassignment budget. A person who is neither owner nor lead is refused too, unless they hold `admin`.

Somebody else's write is **stamped** with who made it; the owner's own next change clears the stamp (taking your queue back says you have seen it). The stamp names the **person**: a lead reordering a report's queue from the dashboard or their assistant writes as their seat, so the stamp and history row name the seat, and the credential used is recorded beside it as the write's operator id.

It also **wakes the seat**, since a seat has no screen to see a stamp. The wake names the task now at the top and who put it there, and is the one notification here that **asks for an answer**: take it up, or say why you cannot. Silence looks like a lost message.

**A mark is one move, and it moves nothing else.** `mark_inbox` names each notice by the `record_id` `work_inbox` gave it:

| Argument | What it does |
|---|---|
| `read` | Marks notices read (the Inbox's *Mark as read*). Lifts a snooze on them. |
| `unread` | Marks notices unread again, including ones already read past. |
| `snooze` | `[{record_id, until}]` — puts notices off until an instant in the future, at most a year away (a longer snooze is a delete that does not say so). |
| `unsnooze` | Brings snoozed notices back now. |
| `read_through` | Reads everything up to a log position, `<stream>@<generation>:<sequence>` (the newest notice you have seen). Only moves **forward**: a position at or behind the current one changes nothing, so a stale "mark all read" cannot un-read newer reads. |
| `primary_reasons` | Which wake reasons are yours to act on, see below. Omitted, your choice stands; an empty list restores the default. |
| `seen` | `<stream>@<generation>:<sequence>`, the `position` of the newest notice you were shown. Optional; changes nothing on your record. The mark waits for it: the node deciding a mark may not yet have applied a notice your inbox page was read with elsewhere, so it waits for `seen`, and if it cannot reach it in time answers that it is behind (the same call clears it) rather than refusing a notice you were shown. A position on another log is refused. |

Everything you do not name stays as it was. The engine applies the move to your record as the write finds it, so two screens marking two notices at once both land. It reads where each notice sits from the notice's own record, which is why a mark names only the notice.

**In the dashboard's Inbox these are an update's own controls.** *Mark as read* marks the open update read and nothing else, and becomes *Mark as unread*. *Snooze* offers an hour, tomorrow at 09:00 and next Monday at 09:00 on the company's clock (each moment once: on a Sunday, next Monday is tomorrow, so it is offered once), or a moment you pick, only within the engine's bound: the person answer serves `max_snooze_ahead`, so a preset the write would refuse is never shown. *Mark all read* is `read_through` at the newest notice the screen LOADED, so a later arrival stays unread. Every one sends the notices' own position as `seen`. A question, a parked executor, a stopped seat and a condition carry none of these: each leaves the list when answered or cleared, and answering a question in the Inbox marks the notices that asked it read.

**Your read position is the rule, and the lists are the exceptions.** Everything at or below it reads as read, everything above as unread; the record keeps only notices *above* it marked read out of order and notices *at or below* it marked unread again. Anything the position already answers is dropped on the next write, keeping the record small without a cap that discards. Moving the position forward clears the unread exceptions. A list that would pass 256 entries is refused `inbox_full`, naming `read_through`: a full list is notices marked one at a time that one read-through would cover.

The position is a *triple* (stream, generation, sequence) because a number from a recreated stream compares as current, and an inbox reading "nothing unread" for ever is a bug nobody reports. Keeping the generation means an inbox read past a reanchor stays read past it.

**Pins are moves too.** `set_pins` takes `views` and `favorites` each as `{add: [...]}` and `{remove: [...]}` against what is there now, or `{set: [...]}` for the whole list, never both, so starring a project from one screen never drops a view pinned from another. Caps (32 pinned views, 64 starred things) are held against the list the move would leave.

**A pin can carry its count.** Asked for a strip with `counts=true`, every view the caller pinned carries `count`: how many tasks the view selects when run, with its saved parameters, in the container it was saved in, `me` as the viewer, on the company's clock. It is the same expansion and statement that answer the board's own total, so a pin's number is the number on the board it opens. It counts every task on its own (`subtasks=separate`), as every surface that runs a view does (the dashboard's shapes and `list_work_items` overrule a view's subtask mode to it); in the grammar's default a root's subtree rides along unfiltered, and an open-work view counted finished subtasks of open epics (39 over a list of 36). At most 32 counts per strip read. A view that no longer compiles (it filters on a since-archived field, say) carries `count_refused` naming why, rather than no count.

**A queue is the one list written whole**, because an order is a statement about every entry at once. `set_priorities` takes the whole list, and `if_match` (the `version` `get_person` answered) makes it conditional: a reorder from a screen that read an older record is refused `stale_version`. The dashboard's reorder is this call: the list it sends is the person's **stored** one with the moved entry at its new place, so entries a screen does not draw (finished ones, open ones past the twentieth) keep theirs.

**A due snooze is reported, never promoted.** Putting one back in the unread list is a write, and a read must not change fleet state with no operation id, arbitration or record. A read says which snoozes are due; your next inbox write moves them.

**The feed and the marks are two reads.** `work_inbox` is what the company *asked of you*: one entry per routed change, newest first, with the single reason it reached you under, the subject, who made it, and an excerpt. A change **you** made is never one, since your assistant writes as your seat, except under `unblocked`: finishing a blocker of your own still tells you the task it held up is workable, because that notice is the repair's change rather than your close. It is written by the applier when the change lands, so it exists whether or not anybody was online. `get_person` is your own *marks over that feed*: what you read, snoozed, and how far you got. Neither is derived from the other, so an inbox entry carries a record id and a position and no content.

**What a page narrows, it narrows in the scan.** `work_inbox` takes `unread`, `primary_only`, `reasons` and `snoozed` (`exclude` by default, `include` to keep what you put off, `only` for just that), each a predicate on the rows the engine reads, not a filter over the page. A page holds fifty notices whenever the scope does: a person whose newest fifty are all snoozed sees the fifty after them. A snooze whose time has come counts as awake under every scope. Each notice also carries what it is *about*: the comment and turn the change came from, and for `asked` or `answered` the ask as it stands now, with its decision, whether it is open, and who answered with which option.

**The primary half is yours to declare.** `mark_inbox` takes `primary_reasons`; `work_inbox` labels every notice with it and returns the rest as context. Saying nothing takes the default: `mention`, `asked`, `answered`, `assignee`, `unassigned`, `reporter`, `unblocked` and `prioritised`, every reason that changes what you should do next. An empty list means *you have not said*, never *nothing is primary*.

**A person reads theirs at `#/inbox`**, one click from the landing screen: the notices `work_inbox` returns, each labelled with the one reason of eighteen that routed it, beside what is waiting on a decision. Whose it is is decided by **who signed in**: the engine resolves the browser's session or presented token to a principal, and the seat that principal holds (every person holds one; a token holds the one the identity directory binds it to). `#/me` is that person's own work: one section per claim on their attention (the Queue, Asked of me, Asked by me, Unblocked, Collaborating, Watching, Checklist), each tab carrying the engine's own total. A section shows at most twenty rows and, when it holds fewer than its total, says which twenty ("the 20 most recently changed of 130"). Collaborating, Watching and Checklist are read from the person's own rows (items they collaborate on, watch, or own a checklist item of), as are the `collaborator`, `watcher` and `checklist_assignee` filters on a listing, so cost follows what the person is on, not what the company holds. The Queue is read by due date or in stored order (`order=due|priorities`). Above the list a band says whose day is on screen, links to that person's seat, and carries the one thing that asks to be answered: a queue somebody *else* put in order, named by the person who did, with a flag on the Priorities choice. A caller the directory binds to no seat (an API token's session, a service account) is an ordinary state: its day is the one kept under its own login, the name its assistant writes marks and priorities under, and the screen says what binding it to a seat would add. A person signed in as themselves always has a seat. See [Humans
in the org](../concepts/humans-in-the-org.md#acting-as-your-seat-on-the-dashboard-and-the-api)
for the binding.

**No notice names a credential.** A token or service account bound to no seat files and watches items like anybody, and the item lists it as reporter or watcher, but no notice is written for it under any reason: it holds no turn and nobody reads its inbox. The writer leaves every credential out of a change's recipients; everybody else hears as before. A token bound to a seat writes as that seat and hears as it.

**Asked of me is answered where it stands.** Each question put to the person is drawn as the landing screen's "Needs your decision" draws it: grouped under who asked, with the role they are asked in and one line naming what the asker recommends. **Accept** sends that option; **Change** opens every option with its detail, plus **Something else** for a reply in your own words, and **Submit** sends the choice (`comment_on_work_item{answers, choice}` or `{answers, body}`). A question with no options offers **Reply**. **Asked by me** is the other end: work this person asked a question on that still waits for its answer (`asked_by=`, every task status, since an open question on finished work is still waited on). It asks under their seat, which their assistant also writes as, so questions put through it appear beside dashboard ones.

**Somebody else's day is read, not worked.** Whoever may read a person's day (whoever leads them, or an administrator) opens it with the whose-day picker; every change on the page is disabled with a sentence saying whose day it is. Questions put to them are theirs to answer (the engine refuses anybody else), and their work is changed from the work screens. The one exception: somebody **above them in the chart** may reorder their queue, and the page says before the press that they are told.

**The Queue is the work list, narrowed to one person.** It is the same screen as `#/worklet/tasks` (the five shapes, the Filter and Display menus, the scope switch, Group by, the order, the columns, the count line) with the assignee fixed and every other choice yours and in the address. What is fixed is not a filter: no chip to remove and no `assignee=` on the URL. Whose day it is stays `handle=`, and reading somebody else's is decided as their queue is: theirs, whoever leads them, or `admin`.

It **opens** grouped by [`due:bucket`](#what-a-board-groups-on), soonest first, over unfinished work. All three are defaults; group by status, sort by priority or look at the week as a board and the address keeps your choice.

The bands are the **engine's**, cut on the company's midnight rather than your browser's, so a task's band and its overdue mark never disagree. There are six; two need a word: **Earlier** is work finished late (past its date, not overdue), empty until you ask for finished work; **No due date** means nobody set one. It is a band rather than an omission because an unscheduled task is the one most likely forgotten, and it is the one band no `due=` filter can reach, since every date comparison in the grammar is over a date that exists.

The **Priorities** reading is numbered in stored order. That order is the content, so nothing re-sorts it, and the place is drawn. It can hold a colleague's assigned task as well as your own, so it is not what the Queue's tab counts: its first line says how many open tasks it holds and how many are assigned to you, and the Queue's count (the figure the sidebar's My work row carries) is only work assigned to you. **Drag a row to a new
place**, or press `Alt` with the up or down arrow on a focused row, and the dashboard sends `set_priorities` as you, conditional on the version it drew: the row shows at its new place with a pending mark until the engine answers, and a refusal (the list changed since you looked) puts it back and says so. Nothing moves before the engine does.

**Your own writes are your seat's.** A person the directory binds to a seat acts AS that seat, with author kind `human`: what they file through their assistant records the seat as reporter, the watch a `watch: true`, comment or create leaves is the seat's, and their own change does not wake them. Colleagues assign work to your seat, and every personal question asks under it. The credential is recorded **beside** the seat, not in its place: `pat:<id>` for a machine token, `session:<lineage>` for a browser session, so history says how a change arrived without letting the writer choose whose name it carries.

**Your own marks, pins and queue are that same record.** `mark_inbox`, `set_pins` and `set_priorities` write the record of whoever the request resolves to, the one every screen reads: the seat for a person (and a token bound to one), the login for everybody else. A credential nobody is bound through (a pipeline's token, a service account, the deployment's break-glass token) writes under its whole login with author kind `operator` and keeps its record under that login, which is ordinary.

**The dashboard writes them as you.** Done, Snooze and **Mark all read** on `#/inbox` are `mark_inbox` (`read`, `snooze`, and `read_through` at the newest notice loaded), sent through
[`/operator/act`](../reference/api-endpoints.md#operatoract--the-dashboards-write-surface),
the dashboard's write transport, which records the write as your assistant's would be: under the seat you hold
([ADR-0024](https://github.com/crewlet/crewlet/blob/main/adr/0024-the-dashboard-acts-as-the-principal-its-session-resolves-to.md)).
The list is re-read at the position the write answered with, so a row moves only once the engine says so. Your assistant makes the same call over `/operator/mcp`, and a script as `PUT /worklet/people/{handle}/inbox`, the same verb over [the human write surface](../reference/api-endpoints.md#the-human-write-surface), attributed to whoever the request resolves to.

**The same rows answer the other way round.** `work_inbox` reads by recipient (one person, every change). `work_routing` reads by *record* (one change, every person): "did my comment reach the person I meant". The primary key is `(record_id, recipient)`, so both are index reads.

Read it at `GET /worklet/routing/{record_id}`, or open an item's **Woke** tab in the dashboard. Every recipient names the one reason of nineteen that found them, whether the notice *asks* something of them, and whether they were reached only because nobody better was found (a lead hearing of a departed report's task, the rank saying which substitute).

**An empty recipient list is three different facts**, and the answer's `delivery` field says which:

- `nobody`: routing resolved to no one (every candidate was the person making the change, or has left).
- `swept`: the change is older than the retention horizon below, so rows may have existed and been cleared; absence is not evidence.
- `unknown`: the caller stated no horizon, so nothing can date the absence.
- `quiet`: the commit carried no notification at all (most field edits, and every subtask a merge re-parents).

The history row's `notified` flag cannot tell these apart: it says the commit *carried* a notification, never that anybody was woken, because the applier holds no roster (two nodes briefly on different epochs would write different rows for one record).

**Inbox rows age out; the history does not.** The company's `inbox.retention_days` (default 365, 30..3650) is how long an entry lives; it sits at the top of the company document rather than Worklet's, because a person's inbox is every native domain's notices under one horizon. A sweep on every data node (each applies the log into its own copy) deletes what is past it. It is **live**: a revision reaches every data node's applier at its next batch and its sweep at its next tick, with no restart, and both read one horizon, because a record older than it writes no inbox row on replay and the sweep deletes exactly the rows a replay would not write. A notice is a *pointer* at a history row, and history answers for ever: "what was I told about in 2024" is a `work_activity` question.

Read at `GET /worklet/people/{handle}`, and through the operator MCP with `work_inbox`, `get_person`, `mark_inbox`, `set_pins` and `set_priorities`, never by a seat. A seat is not a human: it has a **mailbox**, the durable subscription the engine attaches when it acquires the seat, and nothing on a person's record describes one.

## Hand-offs are bounded on the task

Agents can reassign a task a limited number of times before the engine stops and asks a human. The budget is on the **task**, not a call depth (an assignment is an ownership transfer down a chart of known height, not a nested ask), and catches the loop where two seats hand one task back and forth.

The budget is **8**: an agent moving the assignee spends one, re-asserting the current assignee spends nothing, and a person or an operator touching the task at all (any field) returns the whole budget. The ninth agent hand-off is refused `reassignment_budget`, and the refusal says to explain on the item what is blocking it instead.

The limit is **served**: the item answer (`GET /worklet/{id}`, `get_work_item`) carries `reassignment_budget` beside the task's `reassignments`, so a screen saying "hand-off 7 of 8" reads the engine's figure. Every task history row carries **`reassignments` as that change left it** (the count after a hand-off, the zero after a person's touch). An assignment made with a `reason` shows it as that row's excerpt.

## Checklists

A task carries named checklists: up to **16** lists, **64** items in a list and **256** items between them, a list's name at most 128 bytes and an item's at most 256. Each item can be ticked or given to somebody (which puts it in their `my_work` answer under `checklist_items`). An item is never promoted into a subtask: a line that grew into work of its own is filed with `create_work_item` and removed from the list.

`update_work_item` changes them with a **`checklist` gesture**, one change per call, stated as what somebody did:

| `op` | Arguments | What it does |
|---|---|---|
| `add_list` | `name`, optional `items` (names, in order) | adds a list, with its first lines |
| `remove_list` | `list` | removes a list and everything in it |
| `rename_list` | `list`, `name` | renames a list |
| `add_item` | `list`, `name`, optional `assignee` | adds a line at the end of a list |
| `remove_item` | `item` | removes a line and the lines nested under it |
| `rename_item` | `item`, `name` | renames a line |
| `set_done` | `item`, `done` | ticks or unticks a line |
| `assign_item` | `item`, `assignee` (`""` for nobody) | gives a line to somebody |

Lists and items are named by the ids `get_work_item` shows; item ids are unique across the task, so an item gesture names the item alone. The collection is carried whole on the record, so with whole-collection writes two people working one checklist would have the second to land silently undo the first. The engine instead applies the gesture to the checklists as they are when it lands, so both changes survive in either order. Ids a gesture adds derive from the operation, so a retry names the same list rather than adding a second.

A gesture that would grow the checklists past a cap is refused naming it; one that removes, renames, ticks or assigns is never refused for a size the task already has.

## A project's files

A project keeps **files** beside its work: the report a task asked for, the spec, the notes a seat left for the next one. A file lives at a **path** inside its project (`reports/2026/q3.md`), and a path is one file: two writers putting the same path contend, and exactly one wins.

Seats reach them with the four tools in [What a seat can do](#what-a-seat-can-do); a person on the dashboard's project page, through their own assistant (the same four tools), or over REST. On the project's **Files** lens, **Upload** puts each picked file at the project's top level under its own name (a name already kept is refused, since replacing is the row's), **Replace** on a row writes a new copy over that file and **Remove** takes it out, both naming the version on screen so a file changed since is refused rather than overwritten. A file past the size cap is refused before its bytes are sent. Over REST, [`/worklet/files`](../reference/api-endpoints.md#project-files) takes an upload and streams a download without either passing through a model.

**The bytes are not in the tracker.** A file is a row saying where it lives, what it is, its size and SHA-256, and which **object** holds its bytes; the object is in the [object store](../concepts/object-store.md), one store the whole fleet shares (a bucket on the fleet's own broker or an S3 bucket), so a node holding no data reads and writes files like any other. Every upload stores its bytes as a new object **before** writing the row naming it, so a listed file can always be read. A read of the whole file (a download, or a copy into an artifact with `from_project_file`) is checked against the row's SHA-256 and size. A page a seat reads (`read_project_file`) is checked against the length the row's size implies, since part of a file cannot be held to the whole hash
([Writing and reading](../concepts/object-store.md#writing-and-reading)).

| | |
|---|---|
| Largest file | 1 GiB; larger artefacts belong in a store built for them, with a link in the project |
| Path | up to 1 024 bytes, `/` between folders; no empty folder, no `.` or `..`, no backslash or control character. A leading `/` and surrounding spaces are dropped |
| Version | every write moves it; `if_version` (tools) or `If-Match` (REST) refuses a write when the file changed since it was read |
| Removing | the file leaves the listing and history records who removed it and when; its content is deleted by the object store's [hourly collection](../concepts/object-store.md#collection-and-audit) once no file names it and it was last written more than a day ago, so write it again to bring it back |
| A full store | a write the object store has no room for (the fleet's NATS bucket holding as many bytes as it [reserves](../concepts/object-store.md#its-byte-reservation), or an S3-compatible bucket whose provider [refuses the write for room](../concepts/object-store.md#an-s3-bucket--s3)) is refused `store_full` (over REST [`507`](../reference/api-endpoints.md#project-files)) and nothing is written. It stays refused until an operator makes room, so a seat says so rather than retrying |

A file's changes are history rows, `file_written` and `file_removed`, naming who made them. They wake nobody: a file is read from its project, not delivered to an inbox, so a seat that finished a report comments on the task that asked for it.

**A move is a write and a removal.** There is no rename: write the file at its new path and remove the old one. The write uploads the content again (every upload is its own object), so a move costs the file's size in storage until the first hourly collection after the old object is more than a day old.

## Removing, deleting and purging

Three different gestures:

- **Remove** hides a task. Its rows stay and a restore brings it back. `removed=true` finds one to restore: every other query excludes removed work, so the trash is a **filter**, not a screen, and any listing carrying that parameter is a trash listing. Every container ships a builtin `trash` **view** carrying it ([Views](#views)). On the dashboard it is the Filter menu's **Removed items**, the same parameter reached the same way.
- **Delete** writes a marker. Every data node drops every record about the task for ever, which stops a later redelivery resurrecting it, and a write naming the task is told it is **gone for good**: an update, a removal, a restore, a second purge, a dependency of it or on it, a subtask filed under it and a merge naming it are all refused as purged, never as "not on this node" (the answer for a task this node has not applied yet, which a client is right to retry).
- **Purge** removes the rows, and answers the three-valued outcome every write here has: `applied`, `pending` (the record is durable and this node has not reached it yet; do not run it again) or `unknown`, the one to retry.

None of the three is a seat's. Removing and restoring are a person's (the task's reporter, the lead of its project or an administrator, through their assistant, the dashboard or the human write surface) as [above](#what-a-seat-can-do). The deletion marker is written by the purge itself, in the same apply that removes the rows. A purge is an **operator gesture**: `admin`, with a recent proof where a person makes it, never an agent and never the engine, because it is the one operation with no inverse:

```
crewlet worklet purge <item> -reason "why" -confirm <item-key> [-op-id ID]
```

`<item>` is the task's key or its id, and `-confirm` repeats its **key**, which the node checks against the task `<item>` resolves to; a mismatch destroys nothing. There is no project to name: the task's row says which project it is filed under. On `unknown` the command prints an operation id, and that id goes back in `-op-id` so the retry is the same operation rather than a second purge. See [the CLI reference](../reference/cli.md#crewlet-worklet-purge).

Its **children move rather than being destroyed**: each direct child re-parents onto the purged task's parent, or becomes a root when the purged task was one.

**What its own records wrote goes with it**: its history's content, the inbox notices that history routed, its turn records and its dependency mirror, leaving the purge's own history row and the lead's notice as the one account. Of its history only a **skeleton** stays: the rows that moved a count (creation, a removal or restore, each change of status, assignee or project), each holding its kind, instant and those changes and nothing else: no title, body, comment or excerpt. They are what [the flow](#how-the-work-has-moved) walks backward to answer the past; a purge removes a task from that series only from the moment it happened, so a board two weeks ago still shows it.

**Every other task that named it stops naming it**, for good: a task that waited on it is no longer blocked by it, a task it waited on no longer lists it among the work it unblocks, a link to it is gone, and a child's parent is the one it moved onto. Each keeps that through its own next edit. A purge is a gate on the log, so a node on a build that cannot apply it stops its tracker rather than guess: do not purge in the middle of a rolling upgrade ([What a rolling upgrade blocks](replication.md#what-a-rolling-upgrade-blocks)).

**The project's lead is told**, and nobody else: no assignee is left and a notification naming watchers would copy the content the purge exists to remove, kept on the log for its retention window. What survives is that it happened, to which key, by whom, and the operator's reason.

The purge gives no time guarantee: an offline or evicted disk keeps its copy until it replays, adopts a snapshot, is replaced, or is destroyed. See [Retention](retention.md).

## See also

- **[The Tracker](../concepts/task-engine.md)** — the two shapes a company's work can take, and why this one is not a mirror.
- **[Read consistency](consistency.md)** — what a seat's tools see and when.
- **[Retention](retention.md)** — what the log keeps, and what a purge reaches.
- **[API endpoints](../reference/api-endpoints.md)** — the read routes and the operator MCP surface.
