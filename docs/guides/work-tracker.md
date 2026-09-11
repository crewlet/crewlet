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
| **type** | a per-project catalogue — task, bug, epic, whatever the company declared. |
| **title**, **body** | the body is versioned; every save writes an immutable revision. |
| **status** | one of `todo`, `in_progress`, `in_review`, `done`, `cancelled`, `closed`. |
| **priority** | `none`, `low`, `normal`, `high`, `urgent`. |
| **assignee** | one seat or person. |
| **reporter** | who filed it. |
| **collaborators**, **watchers** | who is on the thread and who is listening. |
| **parent**, **subtasks** | a tree, with a depth cap. |
| **start / due**, **estimate**, **points** | scheduling and sizing. |
| **tags** | from a per-project tag set. |
| **custom fields** | declared per project, typed, with option lists. |
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
  want when the question is "what is moving".
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

Five tools, and they are deliberately few:

| Tool | What it does |
|---|---|
| `list_work_items` | the query surface above, filtered any way a view can be |
| `get_work_item` | one task with its thread, history and links |
| `create_work_item` | file a task or a subtask |
| `update_work_item` | change any field, with an optional `if_match` |
| `comment_on_work_item` | add to the thread |

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

## What a person can do

**The dashboard** renders the board, the list and the calendar over the same
queries a seat's tools use, against this node's own copy. Every answer says how
far behind that copy is.

**Your own AI assistant** can reach the same tracker over MCP, at
`/operator/mcp`. It serves the same five work tools above, two more no seat is
given — `list_work_views` and `save_work_view` — the five page tools beside
them and knowledge search — the seat's own implementations, with one
field different: a write carries the **token's** own name as its author and the
author kind `operator`. There is deliberately no way for the caller to name a seat to act
as — a tracker whose author field is chosen by the writer is not an audit
trail.

**The REST API** serves the read side at `/work`, `/work/{id}` and
`/work/views`. Writes go through a seat's tools or the operator MCP, both of
which are attributed to somebody.

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
