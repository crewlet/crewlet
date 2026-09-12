# The Work Tracker

Crewlet ships its own tracker, and it is the default. This page is the product
guide: what a company can record, what a seat can do with it, what a person
can do with it, and what each screen answers.

The architecture — one ordered log, N identical copies — is in
[The Tracker](../concepts/task-engine.md) and
[Replication](replication.md). Nothing here needs it.

## Projects and keys

Work is filed into **projects**. A project has a key (`ENG`, `OPS`), a name,
and a unit of the org chart that owns it. Every task filed into it gets a key
of the form `ENG-412`, minted from the project's own counter.

**Projects come from the org chart.** Declaring a unit with a project key in
the company config creates the project on the next apply, on every node, with
no gesture from anybody. That is what lets a fresh company file its first task
in its first minute.

A numbering **gap** is normal and permanent. `ENG-7` exists, `ENG-8` never did,
`ENG-9` is next: the counter moves before the task lands, so a crash between
the two costs a number rather than risking two tasks sharing a key. A key is
what people paste into chat, so it can never be ambiguous.

## Tasks

| Field | What it is |
|---|---|
| **key** | `ENG-412`. Unique in practice, never reused. |
| **type** | from the workspace catalogue — see [below](#the-catalogue). A type the company has not declared is REFUSED at the create. |
| **title**, **body** | the body is versioned; every save writes an immutable revision. |
| **status** | one of `todo`, `in_progress`, `in_review`, `done`, `cancelled`, `closed`. |
| **priority** | `none`, `low`, `normal`, `high`, `urgent`. |
| **assignee** | one seat or person. |
| **reporter** | who filed it. |
| **collaborators**, **watchers** | who is on the thread and who is listening. |
| **parent**, **subtasks** | a tree, with a depth cap. A query filters ROOTS by default and lets their subtrees ride along; `subtasks=separate` filters every task on its own. |
| **start / due**, **estimate**, **points** | scheduling and sizing. |
| **tags** | from a per-project tag set — see [Tags](#tags). A tag the project has not declared is REFUSED at the write. |
| **custom fields** | declared per project and at the workspace, typed, with option lists — see [the catalogue](#the-catalogue). Filter on one with `f.<slug>`, and see [the operators](#filtering-a-custom-field). |
| **checklist** | items with their own assignees. |
| **relations**, **dependencies** | links between tasks, and blocking edges. |
| **linked pages**, **references** | into the knowledge base and out to third-party systems. |
| **spend** | turns, rounds, tokens and wall-clock this task has cost. |

### Status groups

Six statuses, four groups: `not_started`, `active`, `done`, `closed`. Filters
and boards work on the group, so a company that adds a status does not have to
re-teach every query what "finished" means.

`done` and `cancelled` both stamp the task finished — the group decides, not
the slug. A cancelled task is finished work that produced nothing, which is a
different fact from an open one and the same fact for anything counting.

### Spend is on the task

Every turn an agent spends on a task adds to that task's own counters. That is
what makes "what did this cost" a question about a piece of work rather than
about a seat's month, and it is the number a founder actually wants when a
task has been reopened four times.

## The catalogue

Two declarations, and they are the company's own vocabulary: what a task may
**be**, and what it may **carry**.

**Types.** Seven ship with the engine — `task`, `bug`, `epic`, `story`,
`spike`, `chore` and `milestone` — and every company has them before it declares anything, which is what
lets a fresh company file its first task in its first minute. A declared
catalogue **adds** to them; a declaration sharing a builtin's slug **renames**
it, so a company can call a bug a defect without losing the tasks already filed
under `bug`.

A type is **refused at the create** if the company does not declare it. That is
what stops `Bug`, `bugfix` and `BUG` from filing three types beside `bug` that
every board then groups and filters on as if they were real. An **archived**
type takes no new work and leaves the tasks already under it alone — which is
the whole reason a type is archived rather than deleted.

**Fields.** Custom fields are declared at the workspace and on a project, and a
task's effective set is the union. A value is keyed by the field's **id**, not
its slug, which is what lets a field move between the two keeping every stored
value.

**A field's values are filterable.** Each one is a row keyed on the field's id,
in the column its declared **type** says — a number in the numeric column, an
instant in the date one, a choice's *id* in the reference one — which is what
makes `f.effort=gt:9` a numeric comparison rather than a lexical one, and what
keeps every task that chose an option when somebody renames it. A multi-valued
field is one row per member, so `f.areas=api` is a seek rather than a scan.

**A required field is required of the tasks it applies to.** `applies_to`
names the types that carry a field, and a field that does not apply to a task
cannot be missing from it — its value would be hidden the moment it was set. A
field required at the **workspace** is required in every project, and one
required on a project only there; a subtask is judged by the second toggle
(`required_in_subtasks`, off by default) so one required field does not block
every checklist item anybody promotes.

**An option is one value however it is written.** A choice field stores the
option's *id*, and a write naming the option by its slug or its name resolves
to that id before it is stored — the same three spellings a filter accepts. A
value that stored the word instead would be invisible to every filter,
grouping and total on the field it had just set.

Archiving a field is **one-way**. Its values leave the *filterable* set and stay on
the task, so a field that came back under its old id would silently re-admit
them against a definition nobody has seen for a year. Bringing one back means
declaring a **new** field — a new id, and the same slug, because the slug is
the word the company uses.

Read it at `GET /work/catalogue`, or with `get_work_catalogue`, which every
seat holds: a model that cannot read the catalogue can only guess at a type.
Writing it is an **operator** gesture — `write_work_catalogue` — because a seat
adding a type to make its own create succeed is a seat editing the rules it is
judged by, and the refusal it was working around is the signal a person needs
to see.

A **project's own** field declarations are the project **lead's**, written with
`write_project(fields: [...])`. That list REPLACES the project's declarations
and leaves the workspace's alone — the two are separate scopes and a task's
effective set is their union — so send the whole set, and read
`describe_project` first. A project declaration sharing a workspace field's id
**shadows** it, which `describe_project` names so a reader can see which
definition is in force.

## Tags

Tags are the one catalogue any seat may add to, and they live **per project** —
a tag is how work is grouped for a week, and a company whose tags could only be
declared by a person would be a company whose tags were never declared.

A tag has a **slug** and a **label**. The slug is what every task's row holds
and what a filter compares, so it is normalised on the way in — `Regression`,
`regression` and `needs design` arrive as `regression` and `needs-design` — and
it never changes. The label is what a person reads, and renaming a tag moves
only that.

**A tag is declared before it is used.** A write naming a label the project does
not have is refused, listing the ones it does and the nearest match, because
the alternative is what happened to the task-type catalogue before its own
check existed: any string a model invented became a type, and `Bug`, `bugfix`
and `BUG` came to sit beside `bug` on every board. There are two ways past the
refusal, and both are deliberate rather than automatic:

- `write_project(tags_add: [...])` declares one, which **any seat** may do.
- `labels_create_missing: true` on `create_work_item` or `update_work_item`
  declares what that write is about to use, in one append before the task's
  own. The answer lists what it created under `labels_created`, so a caller
  that set the flag out of habit still sees a typo now rather than on a board
  three weeks later.

A slug within a **typo** of an existing one is accepted with a warning naming
the nearest three — advisory, never a refusal, because a lead can merge two
tags and a refusal with no override would block `apis` behind `api` for ever.
A **label** that collides with another tag's label or slug, case-insensitively,
*is* refused: two tags a person cannot tell apart split the work between them
at random.

**Renaming and archiving are the lead's.** A rename changes the word on every
task already filed under the tag, and an archive takes a filter off everybody's
board — both are decisions about how the company groups its work rather than
about one task. An archive is **one-way**, like a field's: the tasks keep the
tag and every filter on it still answers, and what the archive buys is a tag
that takes no *new* work. Bringing one back means declaring it again under its
own name.

A task carries at most **40** tags, and a project declares at most **512**.

## Sprints and goals

**Sprints** are numbered per project, with a name and dates. A task can be in
more than one — which is what a carry-over is — and the sprint's own record
carries what it started with, so a burn-down is a fact rather than a
reconstruction.

**The engine runs the cadence.** A project's sprint policy says how long a
sprint is, which weekday it starts on and how many to keep minted ahead, and
the engine's own duty does the rest on a one-minute tick:

| Step | When | Gated on |
|---|---|---|
| **mint** | whenever the project is short of its `ahead` count | — |
| **start** | at the sprint's own `start_at` | `auto_start` |
| **close** | at the sprint's own `end_at` | **nothing** — a sprint always closes at its end |
| **rollover** | on the same tick as the close | `auto_roll` |
| **archive** | once `archive_after` newer sprints have closed | `archive_after` |

The close is not a setting, and that is deliberate: every figure in a sprint
report is a predicate over the sprint's own window, so a sprint that ran past
its end is a number nobody can report on. It changes **no task's status** — the
close is about the sprint.

What happens to the unfinished work *is* the setting. With `auto_roll` the same
tick carries it into the next sprint, minting one if the project has none.
Without it the sprint closes with the spillover **pending** — a state a lead
settles with `manage_sprint`, and one `sprint_report` reports so somebody can
see there is a decision waiting. The four choices are `next`, `backlog` (clear
each task's sprint and leave it in the project — which is what the Backlog
*is* here), `close` (cancel every open task: abandoned, not delivered) or a
sprint **number**. Rolling into a closed sprint, or into the sprint being
rolled, is refused — either would put the work back where the rollover was
called to take it out of.

The policy itself is the project **lead's**, written with
`write_project(sprints: {...})`. Sending `enabled: false` stops the cadence and
does **not** close a running sprint — the pointer belongs to the lifecycle, and
a settings change that ended a team's commitment as a side effect would be the
worst kind of surprise. A sprint is between 1 and 92 days long and a project
keeps at most 12 minted ahead; every one of those is refused at the write
rather than at the duty, which runs on another node where nobody is watching.

`manage_sprint` is an **operator** tool and is additionally gated on leading
the project: a sprint is a commitment a team made together, so starting one
changes what everybody is expected to work on and closing one decides what
counted. A seat that could do either would be deciding its own team's plan.

Membership is a **stay**: the pair of instants a task was in one sprint for,
recorded by the engine rather than carried by whoever moved it. That is what
lets `sprint=` mean more than a number. It takes:

| Value | What it selects |
|---|---|
| `4` | that sprint, by number |
| `Kickoff` | that sprint, by name |
| `active` / `future` / `closed` | every sprint of the project in that state — resolved when the query runs, so a saved board keeps meaning "the sprint that is active now" |
| `next` | the earliest future sprint, which is a different question from "any future one" |
| `none` | the **backlog**: unfinished work in no sprint |

Values are comma-separated and OR together, so `sprint=active,next` is what
somebody planning asks for. Every one of them but `none` names a sprint of one
project — they are numbered and named per project — so all of them are refused
at company scope naming the container key that fixes it. `none` means the same
thing everywhere and is accepted there.

`none` is an absence of an **open** stay rather than of every stay, which is
what keeps a carry-over out of the backlog: a task pulled forward into the next
sprint is in that sprint, while one pulled back out of every sprint is
unplanned work again.

**Goals** are the tier above projects: a name, owners, dates, a health value
and a free-text group label. A goal has **targets**, and a target references
work — a project, a set of tasks, a number somebody moves by hand. Goals
deliberately have no *stored* percentage: what a goal is at is what its targets
say, and a stored number would be a second answer that drifts the moment a task
in a target closes without anybody editing the goal.

So the number is **computed on every read**, from the rows the read already
holds:

| Target type | What it is at |
|---|---|
| `tasks` | the fraction of the tasks it reaches that are **finished** — `done` and `cancelled` alike, because the status *group* decides. A removed task is not counted on either side, so tidying up never moves a goal backwards. |
| `number`, `percent` | the fraction of the distance from its start to its goal, **clamped** to 0–100%: a target routinely overshoots, and a progress bar at 130% is a rendering bug in every screen that draws one. The raw number is on the target. |
| `binary` | one or zero. |

A goal's own progress is the **unweighted mean** of its targets. Weighting
would be a second set of numbers somebody has to maintain and nobody would, and
an unweighted mean is wrong *visibly* rather than quietly: two targets, one
enormous and one trivial, read as half done when the trivial one lands, and the
reader can see exactly which it was.

A goal with **no targets has no progress at all** — not zero. "Nothing has
happened" and "there is nothing to measure" are different facts, and a goal
rendered at 0% because nobody set a target is one somebody escalates.

**Health is not inferred.** The targets say what has moved; only an owner knows
whether the movement is on track. A goal at 90% with a week left and one at 90%
with a day left are different situations, and no arithmetic separates them.

**A health update is what somebody wrote.** `write_work_goal(update: {health,
text})` appends one — it never rewrites an existing one, and the author and
the instant are stamped by the engine rather than sent by the caller, because
an update is an assessment filed under somebody's name on a date. The history
is carried forward on every save: a goal save is a whole post-state replace,
and one that dropped the updates would destroy the only part of a goal written
in prose.

Goals are **read** by every seat with `list_work_goals` and at
`GET /work/goals`, and on the dashboard's Goals screen. **Writing** one is an
operator gesture — `write_work_goal`, not a seat's: a seat setting its own
goals is a seat marking its own homework. The read is a seat's for the same
reason reading the catalogue is, and for one more: a goal names owners and
members, and **every one of them is woken when the commitment moves** — so a
seat told its goal changed and holding no verb to read a goal would have been
handed a riddle.

## Views

A **view** is a saved query with a shape. Three shapes:

- **`list`** — rows, sorted and grouped, which is what you want when the
  question is "what is there".
- **`board`** — columns by status group (or by any field), which is what you
  want when the question is "what is moving". A board is `group_by=`, and what
  comes back is **columns**: each one's count is over the whole set, never over
  the rows it carries, so a column of four hundred says four hundred and hands
  you twenty. Loading one further is `group=<value>`, which narrows the whole
  query — including its totals.
- **`calendar`** — by date, which is what you want when the question is "what
  is due".

Views belong to a **container** — the workspace, a project, a unit, a person —
and a personal view is private to its owner. One per container can be the
default, and the applier settles that in the same transaction as the write, so
two views can never both claim it. A protected view cannot be edited by anyone
but its owner, which is what stops a shared board being rearranged under
everybody.

**Three of them exist without anybody saving one.** Every container has a list,
a board and a calendar, and none of the three is an object: a fresh project
needs no setup gesture, a container can never be left without a way to look at
it, and nothing has to guard against somebody deleting the last view. A project
running sprints has two more — a sprint board and a backlog — because those are
the two questions a sprint creates and neither is expressible as a default.

**A view is a set of defaults, never a lock.** Opening one loads its
parameters and every key you then set overrides them, so picking a different
assignee on a saved board gives you that board with one key changed. A
**preset** is the same mechanism for a question people ask often enough that a
screen puts it on a tab — and a view beats a preset, because somebody saved
the view. There are five:

| Preset | What it answers |
|---|---|
| `my_queue` | **what can I pick up** — a disjunction of the work you hold and the work in *your own project* nobody holds, open and unblocked, most important first. Both arms matter: written as "assigned to me" alone, a seat with an empty queue reads the company as having nothing for it while its project's unclaimed backlog sits there |
| `priorities` | your own ordered list, open tasks only, **in the order somebody arranged it**. That order is the answer — it is what was decided — so there is no sort to override it, and a finished task drops out of the answer without the list being rewritten |
| `triage` | the open work **nobody has picked up**. With one fixed status set there is no intake status to filter on, so this is the honest definition of "needs somebody to decide" |
| `blocked` | open work that cannot move — the list a lead reads before a stand-up |
| `overdue` | open work past its due date. Defined on the status **groups** rather than on `show_closed`, so a task delivered yesterday is not reported late for ever |

`my_queue` and `priorities` both need to know who is asking, and the surface
supplies that from its own credential — never the query. That is also what
makes `f.<slug>=me` mean the reader rather than whoever saved the view.

### Filtering a custom field

`f.<slug>=<value>` compares a field, and the comparison a field admits is a
property of its **type**:

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

`null` and `not_null` are on every type, because "is this set" is a question
about the **row** rather than about the value. An operator a type does not
admit is **refused naming the ones it does** — the failure that replaces is
silent: `eq` on a `labels` field produced a clause that matched nothing, and a
board that came back empty reads as "no task has this label".

A bare `f.areas=api` is the type's natural comparison — `any` on a set,
because naming a value is not claiming the set *is* it, and `eq` everywhere
else. Written explicitly, `f.<slug>=<op>:<value>`; a set operator takes a
comma-separated list (`any:api,ui`) and `range` takes both ends
(`range:3..8`), because a range with one end is `gte` or `lte`. A value whose
own text begins `<scheme>://` is a value rather than an operator call, so a
`url` field can be filtered by what it holds.

**A saved view's query is parsed when it is saved**, not when it is opened. A
view that cannot be run is otherwise discovered by whoever opens it, weeks
later, with no way to tell a typo from a grammar change — so a save runs the
parameters through the same grammar `list_work_items` uses and refuses what
does not parse, naming the key.

Views are read at `GET /work/views?container=project:ENG`, and written through
the operator MCP surface with `save_work_view` — **not** by a seat. A view is
furniture, and a seat's job is the work rather than the furniture around it.

### Manual order

A board is drag-ordered, and the order is a real value on the task rather than
a position in a list. Dragging one task writes one record; the key it mints
sits between its new neighbours.

Repeated insertion at the same point makes keys grow — about one character per
sixty-two placements — and past a threshold the engine **re-spreads** the
affected range in the background, in batches, order-preserving. Every
intermediate state is the original order, so an interrupted re-spread leaves a
correct board.

## What a seat can do

Thirteen tools, and they are deliberately few — six that act on a task, three
that read the container it is filed into, one that writes the one part of that
container a seat owns, one that reads the company's goals, and two about
CHANGE rather than about state:

| Tool | What it does |
|---|---|
| `list_work_items` | the query surface above, filtered any way a view can be — including `preset=my_queue`, which is the seat's own open work |
| `get_work_item` | one task with its thread, history and links |
| `create_work_item` | file a task or a subtask |
| `update_work_item` | change any field, with an optional `if_match`. `watch: true`/`false` is a gesture about the CALLER and nobody else — the engine resolves it against the item's current watchers inside its own transaction, so following a task never removes whoever was already following it |
| `comment_on_work_item` | add to the thread |
| `get_work_catalogue` | the types a task may be and the fields it may carry |
| `list_projects` | every project work is filed into, with how much open work each holds, who leads it and which sprint is running |
| `describe_project` | one project in full: the six statuses with what each means, the types it files, the fields grouped by which type they apply to (required first, with their options), its tags, its lead and its active sprint. Omitting the project means the seat's own |
| `write_project` | a project's own settings. Declaring a **tag** is open to every seat; renaming or archiving one, declaring project fields, setting the sprint policy and setting the default assignee are the project **lead's**; archiving the project takes a person |
| `list_work_goals` | the company's goals, what each is at, and the health updates written against them. A read only — setting a goal is a person's |
| `sprint_report` | how a project's recent sprints went — committed, added, removed, done and remaining, per sprint and per person, in the project's own measure |
| `task_activity` | what HAPPENED, in the order the log made it happen: every change to one task or one project, with who made it and exactly which fields moved |
| `my_work` | everything this seat is expected to look at, in one call — see below |

`my_work` is the call a turn opens with, and it answers **seven lists** rather
than one: the seat's priorities in the order somebody put them, the work it
holds, the questions waiting on its answer (each with the literal call that
answers it), the checklist items it claimed on **other people's** tasks, the
work it was brought onto without owning, what moved on what it follows, and
what just became workable. Each is a different claim on the reader's
attention, and a seat that saw only its assignments would miss six of them.
One call rather than seven because assembled separately a seat could see a
task in `assigned` that had already moved out of it by the time `priorities`
was read — and spend its turn on work somebody else had taken. It takes **no
handle**: the seat is the turn's own, because a tool that named whose day to
read could read a colleague's queue.

`task_activity` is the only way to ask what CHANGED. A board is about what is
there now, and a task that was reassigned twice and back looks exactly like
one nobody touched. Its order is the **log's**, not a clock's, so its `since`
and its cursor are log positions — which is what lets a cursor span a reanchor
with no gap and no repeat. Its `q` needs either a task, or a project **and** a
`since` inside 90 days: an unscoped text search reads every change the company
has ever made, and it has no cheaper mode to fall back to.

The three project reads are a seat's for the same reason the catalogue read
is: a create refuses a project the company does not have, a type it has not
declared and a required field left empty, and a model that cannot **read** any
of that can only guess. A refusal that names the valid values is only half an
answer if there was no way to look them up first.

`write_project` is the one project **write** a seat holds, and it holds it for
one facet: declaring a tag. A project's name, purpose and owning unit are
**chart-owned** — written by the epoch apply from the org chart and by nothing
else — so nothing writes them here at all; its field declarations, its sprint
policy and its default assignee are the **lead's**, and archiving the project
itself takes a person's own credential. Every one of those is gated inside the
verb, and each refusal names who can.

An operator holds the same thirteen and more that no seat does:
`manage_sprint` (above), and the two below. `remove_work_item` puts an item
in the **trash** and `restore_work_item` takes it out again, at any age. A
removal hides an item from every list and board and destroys nothing — its
history is untouched and `list_work_items` with `removed: true` is the only
thing that shows it. No seat holds either, because a seat that could hide work
it did not want to do would be marking its own homework in the one way that
leaves no trace: the board simply has one fewer item on it. Removing an item
with `subtree: true` takes its children with it, and each child's tombstone
names the removal that took it — so restoring the parent brings back exactly
what that gesture removed, and never a child that was already in the trash for
its own reasons.

Neither is `purge`, which destroys every row on every node and has no inverse.
That one is `crewlet work purge`, with a typed confirmation and a required
reason, and it is deliberately not a tool at all.

Three of those count as a **delivery**: create, update and comment. A turn
woken by an assignment answers by moving the task, commenting on it, or filing
the follow-up — and the delivery gate knows that, so such a turn is not
corrected and looped for "having done nothing". Reading is not delivering,
which is exactly the turn the gate exists to catch.

Seat tools read at the **`session`** level: a seat that files a task and then
lists its project sees the task it just filed. See
[Read consistency](consistency.md).

### Optimistic concurrency

`update_work_item` takes an optional `if_match` carrying the version the caller
read. Without it, the last write wins — which is right for a field an agent is
setting from its own work. With it, a concurrent edit is refused and the
refusal carries the current version, so the caller can re-read and decide.

The body is different: a save must state the version it edited, always. There
is no per-field merge that makes overwriting prose safe.

Watching is not a field a caller sets either. The watcher list is a set, and a
tool that could write it whole would have to know every name already on it —
so `watch: true` says only "add me" and the engine resolves it against the
task's current watchers in the same transaction that writes them. An item with
more than 64 watchers refuses the next one: past that it is an announcement
rather than something people follow, and a comment on it wakes the company.

**Commenting makes you a watcher, and never fails because of it.** The
commenter's watch is the same one-handle gesture, resolved against the current
row — a comment does not write the watcher set whole, which would silently
discard somebody who watched or unwatched while the comment was being written.
And at the cap the comment lands and the watch is skipped: an explicit
sixty-fifth watch is refused, because the person asked for it and can be told;
an automatic one is not, because refusing it would fail somebody's comment for
a reason that has nothing to do with what they wrote.

**Leaving is never refused**, whatever the set holds. Only joining is capped —
the check is about growth, and applying it to an unwatch would leave a task
that had somehow grown past the cap as one nobody could leave.

## What a person can do

**The dashboard** renders the board, the list and the calendar over the same
queries a seat's tools use, against this node's own copy. Every answer says how
far behind that copy is.

**Your own AI assistant** can reach the same tracker over MCP, at
`/operator/mcp`. It serves the same work tools above, more no seat is
given — `list_work_views`, `save_work_view`, `write_work_goal`,
`write_work_catalogue`, `get_person`, `mark_inbox`, `set_pins`,
`set_priorities`, `manage_sprint`, `remove_work_item` and `restore_work_item`
— the five page tools beside them and knowledge
search — the seat's own implementations, with one
field different: a write carries the **token's** own name as its author and the
author kind `operator`. There is deliberately no way for the caller to name a seat to act
as — a tracker whose author field is chosen by the writer is not an audit
trail.

**The REST API** serves the read side at `/work`, `/work/{id}` and
`/work/views`. Writes go through a seat's tools or the operator MCP, both of
which are attributed to somebody.

## A handle is checked before it is stored

Every argument that names a colleague — a task's assignee, a goal's owners and
members, a project's default assignee, whose priority list is being written —
is resolved against the company's own roster before the write, and a name
nobody has is **refused**, listing the seats.

That is not tidiness. An unknown handle fails *silently and permanently*: it is
stored, it rides the change's routing snapshot, it becomes a candidate — and
the wake path drops it against the live roster with no error and no log. The
write answers `applied`, and the person it named never hears anything. A
misspelling is indistinguishable from a colleague who is simply quiet. On a
goal it is worse still, because a goal update reaches its owners and *nobody
else*: there is no lead fallback to catch the fall, so one typo silences that
goal for everybody, for ever.

A name is **resolved**, not merely checked — so a caller that typed a role's
name rather than its handle gets the handle back rather than a refusal.

**A handle already on the object is kept.** People leave, and a save that
carries a departed owner is not adding a typo — it is carrying what is there.
A whole-document write is refused only when its set of unresolvable names
*grows*, because the alternative is a goal whose owner left being permanently
unsaveable, including the one edit that takes them off it.

## What wakes a seat

A change that concerns somebody becomes a **wake** — a turn on that seat, with
a prompt written for the reason it reached them. Most wakes are about a task:
you were assigned it, mentioned on it, watching it, blocked by it. Four are
not, and they exist because the thing that moved is not a row on a board:

| Wake | Who hears it | What it asks |
|---|---|---|
| `goal_updated` | every owner and every member of the goal | nothing — re-plan your own work if the health moved |
| `sprint_started` | the seats with work in the sprint, and the project lead | nothing — look at what you committed to |
| `sprint_closed` | the same | nothing — find where your unfinished work went |
| `prioritised` | the person whose queue somebody else wrote | **an answer**: take it up, or say why you cannot |

A fifth is about a task and is listed apart from the others because of what it
says rather than what it is about:

| Wake | Who hears it | What it asks |
|---|---|---|
| `purged` | the lead of the project the task was filed in | nothing — the task is gone from every node and nothing restores it |

`purge_task` is the one operation in this engine with no inverse, and for a
long time it told **nobody**: a task, its comments, its revisions, its history
and its turn records were destroyed on every node and the person accountable
for that project heard nothing. The wake names the key, who ran it and their
stated reason — and nothing else. It quotes neither the title nor the body,
because the record outlives the rows: an excerpt of what was purged would keep
a copy of exactly that, on the log, for its whole retention window.

Only `prioritised` **addresses** its recipient. A goal's owners are told so they can
act in the work, not so they can reply — a wake that asked for an answer on
every health update would fill the tracker with "noted, thanks". Being told
what to do next by somebody above you is a different thing, and silence on it
is indistinguishable from a message that was lost.

None of the four sends you to `get_work_item` for the object it is about: a
goal's id and a sprint's number are not task keys, and a pointer at one costs a
round and a failed tool call to discover. Each names the tool that answers its
own question — `list_work_goals`, `sprint_report`, `my_work`. A `purged` wake
sends you nowhere for the sharper version of the same reason: the row is gone
from every node, so the tool would answer `not_found`.

A **sprint close changes no task's status.** The close is about the sprint;
what happens to the unfinished work is the rollover, and the wake says which of
`next`, the backlog, cancelled, or still pending it was.

## A person's own state

A human has a record of their own beside the work: an **inbox**, a **queue**
and their **pins**. Three parts of one document, with three different
authorities over them — which is why they are three separate writes.

| Part | Who may write it |
|---|---|
| **inbox** — read, unread, snoozed, and how far you have read | only on behalf of the person whose it is |
| **pins** — pinned views and starred things | the same |
| **queue** — the order you mean to work in | yours, **or** a lead's for somebody in their line, a human's, or an operator's |

Somebody else marking your work read is the one thing an inbox must never
allow: the item is then gone from the only place you would have looked for it,
and nothing anywhere says who removed it. A pin is the same rule for the
plainer reason — one somebody else can set is one that moves under you.

The **queue is the exception**, and deliberately: telling somebody what to do
next is what a lead is for. A **seat** is the one party that may not write
somebody else's — an agent re-ordering a colleague's list is a hand-off in
disguise, and it bypasses the guarded take and the reassignment budget that a
real hand-off goes through.

Somebody else's write is **stamped** with who made it, so a person who starts
the day on work they did not choose can see who chose it, and their own next
change clears the stamp — taking your queue back is the gesture that says you
have seen it. The lead relation is any ancestor in the management chain, not
just the direct manager: a founder leads everybody.

It also **wakes the seat**, and that is the other half of the same authority:
a stamp is seen by somebody who opens a screen, and a seat has no screen. The
wake names the task now at the top of the list, and it is the one notification
in this section that **asks for an answer** — take it up, or say why you
cannot. Going silent on it looks exactly like a message that was lost.

**An entry you have read past is pruned on the next write.** That is what keeps
the record small without a cap that discards: an entry at or below your
seen-through position is one no screen will ever render. The position is a
*triple* — stream, generation and sequence — because a number from a recreated
stream compares as current, and an inbox that read "nothing unread" for ever is
not a bug anybody reports.

**A due snooze is reported, never promoted.** Putting one back in the unread
list is a write, and a read that performed one would change the fleet's state
from a path with no operation id, no arbitration and no record. So a read says
which snoozes are due and your next inbox write is what moves them.

Read at `GET /work/people/{handle}`, and written through the operator MCP with
`mark_inbox`, `set_pins` and `set_priorities` — never by a seat. A seat is not
a human: it has a **mailbox**, which is the durable subscription the engine
attaches when it acquires the seat, and nothing on a person's record describes
one.

## Hand-offs are bounded on the task

A task can be reassigned by agents a limited number of times before the engine
stops and asks a human. The budget is on the **task**, not on a call depth, and
any human touch resets it.

That is deliberate: an assignment is an ownership transfer down a chart of
known height, not a nested ask, so bounding it by delegation depth would bound
the wrong thing. What it catches is the loop where two seats hand one task back
and forth, which is the failure mode that actually happens.

## Removing, deleting and purging

Three different gestures, and the difference matters:

- **Remove** hides a task. Its rows stay and a restore brings it back — and
  `removed=true` is how you find one to restore: every other query excludes
  removed work, which is what a board means, so the trash is a filter rather
  than a screen.
- **Delete** writes a marker. Every node drops every record about that task for
  ever, which is what stops a redelivery months later resurrecting it.
- **Purge** removes the rows. Its report comes back in **three groups**: what
  was purged, what could not be reached, and what is stale.

Only the first two are a seat's. A purge is an **operator gesture** — a person
or an operator token, never an agent and never the engine — because it is the
one operation with no inverse and nothing else can be asked to confirm it:

```
crewlet work purge <task-id> -project KEY -reason "why" -confirm <task-key>
```

Its **children move rather than being destroyed**: each direct child
re-parents onto the purged task's own parent, or becomes a root when the purged
task was one. Destroying the subtree would destroy work nobody confirmed.

**The project's lead is told**, and nobody else. There is no assignee left to
tell and no watcher list worth carrying — a notification naming them would be
a copy of exactly the content the purge exists to remove, kept on the log for
its whole retention window. What survives is that it happened, to which key,
by whom, and the reason the operator gave.

The purge report gives no time guarantee, and that is honest rather than
evasive: an offline or evicted disk keeps its copy until it replays, adopts a
snapshot, is replaced, or is destroyed. There is no duration to state. See
[Retention](retention.md).

## See also

- **[The Tracker](../concepts/task-engine.md)** — the two shapes a company's
  work can take, and why this one is not a mirror.
- **[Read consistency](consistency.md)** — what a seat's tools see and when.
- **[Retention](retention.md)** — what the log keeps, and what a purge reaches.
- **[API endpoints](../reference/api-endpoints.md)** — the read routes and the
  operator MCP surface.
