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
| **tags** | from a per-project tag set. |
| **custom fields** | declared per project and at the workspace, typed, with option lists — see [the catalogue](#the-catalogue). Filter on one with `f.<slug>`. |
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

**Types.** Six ship with the engine — `task`, `bug`, `epic`, `story`, `spike`,
`chore` — and every company has them before it declares anything, which is what
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

## Sprints and goals

**Sprints** are numbered per project, with a name and dates. A task can be in
more than one — which is what a carry-over is — and the sprint's own record
carries what it started with, so a burn-down is a fact rather than a
reconstruction.

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

Goals are read at `GET /work/goals` and on the dashboard's Goals screen, and
written through the operator MCP with `write_work_goal` — not by a seat. A
seat setting its own goals is a seat marking its own homework.

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
**preset** is the same mechanism for a question the engine's own indexes are
named after — `my_queue`, `blocked`, `overdue` — and a view beats a preset,
because somebody saved the view.

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

Six tools, and they are deliberately few:

| Tool | What it does |
|---|---|
| `list_work_items` | the query surface above, filtered any way a view can be — including `preset=my_queue`, which is the seat's own open work |
| `get_work_item` | one task with its thread, history and links |
| `create_work_item` | file a task or a subtask |
| `update_work_item` | change any field, with an optional `if_match`. `watch: true`/`false` is a gesture about the CALLER and nobody else — the engine resolves it against the item's current watchers inside its own transaction, so following a task never removes whoever was already following it |
| `comment_on_work_item` | add to the thread |
| `get_work_catalogue` | the types a task may be and the fields it may carry |

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

## What a person can do

**The dashboard** renders the board, the list and the calendar over the same
queries a seat's tools use, against this node's own copy. Every answer says how
far behind that copy is.

**Your own AI assistant** can reach the same tracker over MCP, at
`/operator/mcp`. It serves the same six work tools above, nine more no seat is
given — `list_work_views`, `save_work_view`, `list_work_goals`,
`write_work_goal`, `write_work_catalogue`, `get_person`, `mark_inbox`,
`set_pins` and `set_priorities` — the five page tools beside them and knowledge
search — the seat's own implementations, with one
field different: a write carries the **token's** own name as its author and the
author kind `operator`. There is deliberately no way for the caller to name a seat to act
as — a tracker whose author field is chosen by the writer is not an audit
trail.

**The REST API** serves the read side at `/work`, `/work/{id}` and
`/work/views`. Writes go through a seat's tools or the operator MCP, both of
which are attributed to somebody.

## A person's own state

A human has a record of their own beside the work: an **inbox**, a **queue**
and their **pins**. Three parts of one document, with three different
authorities over them — which is why they are three separate writes.

| Part | Who may write it |
|---|---|
| **inbox** — read, unread, snoozed, and how far you have read | only on behalf of the person whose it is |
| **pins** — pinned views and starred things | the same |
| **queue** — the order you mean to work in | yours, **or a lead's** for somebody in their line |

Somebody else marking your work read is the one thing an inbox must never
allow: the item is then gone from the only place you would have looked for it,
and nothing anywhere says who removed it. A pin is the same rule for the
plainer reason — one somebody else can set is one that moves under you.

The **queue is the exception**, and deliberately: telling somebody what to do
next is what a lead is for. A lead's write is **stamped** with who made it, so
a person who starts the day on work they did not choose can see who chose it,
and their own next change clears the stamp — taking your queue back is the
gesture that says you have seen it. The lead relation is any ancestor in the
management chain, not just the direct manager: a founder leads everybody.

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
