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

**A name is a resolution key, not a label.** A type, a field and an option are
each resolvable three ways — by id, by slug, and by **name** — which is what
lets a model write `severity: High` after reading "High" off a board. So two
declarations whose names differ only in case or spacing are refused: they are
two rows one lookup cannot tell apart, and the resolution would pick whichever
was read first. A type's name is checked against the **shipped** types too,
because a catalogue adds to them — though renaming a builtin by declaring its
own slug is exactly what the override is for.

**A field's configuration is checked against its type.** A precision on a
checkbox, a time flag on a number, a rollup on a text field: each is a setting
that would be stored, replicated and read by nothing, so the declaration is
refused naming which types use it. A minimum above its maximum is refused at
the declaration rather than at every write that then fails against it.

**Two settings are refused because nothing fills them.** A `rollup:` block and
`progress: auto` both say a value keeps itself up to date, and this build
computes neither — a rollup is a correlated aggregate over a relation and
automatic progress is a per-task count of subtasks, checklist items or asked
comments, and both are read-time work nothing does yet. Accepted, the field
would hold whatever somebody last typed under a name saying otherwise, which
is worse than a plain number because nobody knows to maintain it. The field
TYPES still work: a `progress` or `rollup` field with `progress: manual` (or
no configuration at all) is a number somebody writes, and every filter on this
page applies to it.

**An option is one value however it is written.** A choice field stores the
option's *id*, and a write naming the option by its slug or its name resolves
to that id before it is stored — the same three spellings a filter accepts. A
value that stored the word instead would be invisible to every filter,
grouping and total on the field it had just set.

### What a field value may be

Every value is checked against its own declaration at the **write**, and
refused naming the rule. The check is at the write because that is where it can
be refused: by the time a record is applied the value is durable, and the
applier deliberately salvages — a value it cannot decode writes no row and the
change carries on, because refusing it would let one malformed field stop that
task's every later edit on every node.

| Type | Accepts | Refuses |
|---|---|---|
| `number`, `progress`, `rollup` | a number, or a string that is one | text that is not a number, and anything outside `min`/`max` or carrying more decimals than `precision` |
| `checkbox` | `true` or `false` | a string — every rule for reading one disagrees about `"false"` |
| `date` | a date; a timestamp when the field holds no time, **truncated with a warning** | a value that is not a date, and a bare date on a field that holds a time |
| `dropdown`, `labels` | the option's slug, name or id — stored as the **id** | an option the field does not declare |
| `relationship` | a key, a former key or an id — stored as the **id** | an item that does not exist |
| `people` | anything that resolves to exactly **one** colleague | a spelling that names nobody, or more than one |
| `url` | an absolute url | one with no scheme or no host |
| `email` | a parseable address — the **address**, not the display name | anything `Ana <ana@example.com>` cannot be read as |
| `text`, `textarea` | text within the type's byte cap | anything longer, named rather than cut |

`null` clears any field. **Nothing is rounded to fit**: a field declared exact
to two places refuses `3.14159` rather than storing `3.14`, because the stored
value would be a number nobody typed under a declaration that says the field is
exact.

The one change the engine makes for you is the date truncation, and it says so
in the result's `warnings` — a value the engine altered is one the writer has
to be told about, or the board shows something they did not write.

Required fields are enforced at **every** write, not only at the create: an
update may not clear one. ClickUp enforces them at creation only; this is the
difference, and there is no toggle.

Archiving a field is **one-way**. Its values leave the *filterable* set and stay on
the task, so a field that came back under its old id would silently re-admit
them against a definition nobody has seen for a year. Bringing one back means
declaring a **new** field — a new id, and the same slug, because the slug is
the word the company uses.

Read it at `GET /work/catalogue`, or with `get_work_catalogue`, which every
seat holds: a model that cannot read the catalogue can only guess at a type.
Writing it — `write_work_catalogue`, on the operator's assistant — takes
**`config:write`**, the grant that edits the company's own document. The
catalogue is declared once for the whole workspace and names no project, so
there is no container whose lead could own it: it is configuration. No seat
holds `config:write` or the tool, because a seat adding a type to make its own
create succeed is a seat editing the rules it is judged by, and the refusal it
was working around is the signal a person needs to see.

A **project's own** field declarations are the project **lead's** (or a caller
holding `fleet:operate`), written with `write_project(fields: [...])`. That
list REPLACES the project's declarations and leaves the workspace's alone —
the two are separate scopes and a task's effective set is their union — so
send the whole set, and read `describe_project` first. A project declaration sharing a workspace field's id
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

**Renaming and archiving are the project lead's**, or a caller holding
`fleet:operate`. A rename changes the word on every task already filed under
the tag, and an archive takes a filter off everybody's board — both are
decisions about how the company groups its work rather than about one task.
Adding a tag stays open to every colleague: it needs nothing beyond the
ordinary `work:write` that `write_project` itself takes. An archive is
**one-way**, like a field's: the tasks keep the tag and every filter on it
still answers, and what the archive buys is a tag that takes no *new* work.
Bringing one back means declaring it again under its own name.

A task carries at most **40** tags, and a project declares at most **512**.

## Views

A **view** is a saved query with a shape. Five shapes:

- **`list`** — rows, sorted and grouped, which is what you want when the
  question is "what is there".
- **`board`** — columns by status group (or by any field), which is what you
  want when the question is "what is moving". A board is `group_by=`, and what
  comes back is **columns**: each one's count is over the whole set, never over
  the rows it carries, so a column of four hundred says four hundred and hands
  you twenty. Loading one further is `group=<value>`, which narrows the whole
  query — including its totals.
- **`calendar`** — by date, which is what you want when the question is "what
  is due". Its axis IS the `due` key, so the grid's own window spends the one
  key the grammar has for it: the fetch is bounded to the days on screen, the
  Overdue chip is not offered (pressed, it could narrow nothing at all), and
  the toolbar's count says what it counted — "6 items due in this window" where
  every other shape under the same filters says "17 items".
- **`timeline`** — bars down a date axis, which is what you want when the
  question is "how does this lay out". It is the one shape that can show a
  task **spanning** time rather than sitting on a day, and the only one that
  draws the dependencies between two tasks as a line from one to the other.
- **`table`** — one field per column, which is what you want when the question
  is about a *field* rather than about a task: "which of these is the biggest",
  "who holds the overdue ones", "what is unestimated". A list draws each task
  as a block you read one at a time, so comparing one field down it means
  finding the same badge at a different place on every row; a table puts every
  value of a column at one place and sorts at its head. The sort it writes is
  the query's own `sort=`, so it orders the **whole set** rather than the page
  that happens to be loaded.

### The timeline

A task's `start` and `due` are its bar. A task carrying only one of them gets
a one-day marker with a dashed edge, because "due on the 20th" and "a day's
work on the 20th" are different facts and drawing them the same way would
claim knowledge nobody entered. A task carrying neither sits in an
**Unscheduled** band under the axis with a count — never placed on today,
which would invent a deadline nobody set.

Its window is derived from the rows it is drawing, padded to a fortnight so a
single task has something to be read against and capped at a year and a
fortnight so one task due in 2031 cannot compress a fortnight into four pixels.
Grouping applies (`group_by=`), and **each band gets its own axis**: a team
planning six months out does not squeeze every other team into the corner of
its window.

Dependencies are drawn as arrows from the blocker to the task that waits.
An arrow is dashed when the edge is **one-sided** — the blocker does not list
the waiting task back, which the repair duty is still working on. An edge
whose blocker is not on the axis, because a filter excluded it or because it
carries no dates, is **counted beneath the chart** rather than dropped
silently: a reader who cannot see the omission reads the arrows as every
dependency there is.

It is **read-only**. There is no dragging and no resizing: what a drag would
mean for a task with a dependency, owned by somebody else, is a
question with no good answer, and the tracker's own edit surfaces already say
those things properly. A bar is a link.

Views belong to a **container** — the workspace, a project, a unit, a person —
and a personal view is private to its owner. One per container can be the
default, and the applier settles that in the same transaction as the write, so
two views can never both claim it. A protected view cannot be edited by anyone
but its owner, which is what stops a shared board being rearranged under
everybody.

**Six of them exist without anybody saving one.** Every container has a list,
a board, a calendar, a timeline, a table and a trash, and none of the six is an
object: a fresh project needs no setup gesture, a container can never be left
without a way to look at it, and nothing has to guard against somebody deleting
the last view.

**The trash is a query, not a shape.** It is a table carrying `removed=true`
and `show_closed=true`, which is why it is a builtin *view* rather than a sixth
rendering: what makes a listing the trash is the parameter, so any view you
save with `removed=true` is one too and is read the same way. `show_closed`
travels with it because a removed task is very often a finished one, and
without it the one tab whose job is "what did my assistant delete" would hide
every deletion of anything already done. It is ordered by **when work was
removed**, newest first, and `sort=removed` asks for the other end of it — a
removed task's rank is its position on a board it has left, so ordering the
trash by rank orders it by a stale number. What a removal, a deletion and a
purge each mean is [below](#removing-deleting-and-purging).

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

**A view's parameters are the query grammar's own**, which is what
`list_work_items` compiles its arguments into rather than what those arguments
are called. Four are spelled differently: `project` is `container` and takes
`project:ENG` or `workspace`, `text` is `q`, `label` is `tag`, and `open_only`
is `status_group=not_started,active`. Everything else is the same word, and a
custom field is `f.<ref>`. A view may not carry `view`, `cursor`,
`read_level`, `max_lag_seconds`, `max_lag_seq` or `min_position` at all: those
are about the caller's own read — where it resumes and how fresh it must be —
rather than about the rows.

**A saved view is run by its id**, passed as `view` to `list_work_items` or as
`?view=` on the REST route. Anything else the caller passes overrides the
view's own, so a seat can open somebody's board and narrow it without editing
it.

Views are read at `GET /work/views?container=project:ENG`, and written through
the operator MCP surface with `save_work_view` — **not** by a seat. A view is
furniture, and a seat's job is the work rather than the furniture around it.

**Who may save one follows what the view is**, never who is asking — the
payload decides, so there is no second verb a caller could pick to ask the
easier question:

| The view | Who may save it |
|---|---|
| **Personal** — it names an `owner` | that owner, or a caller holding `fleet:operate`. Not their lead: it is a strip only its owner sees, and rearranging it is not a gesture anybody asked a lead to make |
| **Shared on a project** (`project:ENG`) | the project's lead, or `fleet:operate` |
| **Shared on a unit** (`unit:engineering`) | whoever leads that unit, directly or from anywhere above it, or `fleet:operate` |
| **Shared on a person's page** (`person:ana`) | that person, whoever leads them, or `fleet:operate` — it is a tab on a page other people read |
| **Shared on the workspace** | `fleet:operate` alone: it is a tab every person in the company lands on |

**Replacing a view is decided on the view it replaces, too.** A save names an
`id` and replaces that view whole, so the rule above is asked twice: once for
the view being written, and once for the view already stored under the id —
its owner and its container as they are *now*. Without the second, anybody who
may keep a personal view could name a project's shared tab, or a colleague's
personal view, and overwrite it with their own. The write then refuses, as a
conflict to re-read on, a save whose stored view moved or changed hands after
that decision was taken.

`default: true` takes the container's landing tab from whichever view held it,
which is why a shared view is its container's decision rather than whoever
happened to write it first. **Only a shared view can be the default**: the
landing tab is everybody's, and nobody but its owner sees a personal view, so a
personal one saved with `default: true` is refused rather than clearing every
other reader's landing tab for a row they cannot see. A person who wants their
own view first pins it with `set_pins`.

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

Thirteen tools, and they are deliberately few — eight that act on a task,
three that read the container it is filed into, one that writes the one part of
that container a seat owns, and one about CHANGE rather than about state:

| Tool | What it does |
|---|---|
| `list_work_items` | the query surface above, filtered any way a view can be — and every row filtered **on its own**, whatever a named view's own shape says: the grammar's `collapsed` default makes the filter a predicate on the ROOT and lets its whole subtree ride along unfiltered, which draws a board and misreports a list. An item's subtasks are asked for with `parent`, which that mode never applied to — including `preset=my_queue`, which is the seat's own open work. Beside the obvious filters it takes `type`, `priority`, `parent` (an item's subtasks), `reporter`, `watcher`, `unit`, the three date keys (`due`, `updated`, `created`), `sort`, `cursor` for the next page, and **`field_filters`** keyed by field slug — which is how a seat reaches the custom fields its company declares |
| `get_work_item` | one task with its recent comments, history, links and **custom fields**. `include` narrows to the parts you need; `comments_cursor` pages back through a long thread; **`comment`** opens one comment by id with its body exactly as it was written; **`body: true`** returns the task's own description in full. Comment bodies in the thread are excerpts ending in `…`, because twenty at their full length is ten times what one tool answer may weigh — `comment` is how the rest is read, and on its own it answers the item and that comment and nothing else. The description is excerpted the same way and for the same reason, and `body` is its counterpart — each answers on its own, and naming both gets the comment, because it is the narrower ask. Each field value comes back with the slug, name and type that explain it, and says when it is **hidden** (its declaration was archived), **foreign** (mirrored in from another tracker) or **undeclared** (a value this company explains nowhere) |
| `create_work_item` | file a task or a subtask. `fields` sets custom fields by **slug**, and the create is refused naming any the project requires and this call leaves out. It also takes the four **scheduling** arguments below |
| `update_work_item` | change any field, with an optional `if_match`. **`routing_unit`** — pointing the item at a different team — is the one field that is not every caller's to set: it belongs to the lead of the project the item is filed in, because it decides which team hears about somebody else's work. `watch: true`/`false` is a gesture about the CALLER and nobody else — the engine resolves it against the item's current watchers inside its own transaction, so following a task never removes whoever was already following it. Its `waiting_on`, `blocking`, `linked` and `linked_pages` arguments are **set-valued** — see below — and `fields` sets custom fields by slug, checked against each field's own declaration. It takes the four **scheduling** arguments too, where `null` on any of them CLEARS it |
| `create_work_item` and `update_work_item` | both take `fields`, keyed by field **slug** — see "What a field value may be" above |
| `comment_on_work_item` | add to the thread, optionally as a **question** somebody owes an answer to (`ask`) or as the **answer** that closes one (`answers`) |
| `search_work_items` | find an item by what it **says** — ranked over every item's title *and description*, which no filter reaches. `list_work_items`' own `text` is a substring of the key or title and cannot see a description at all, so the two are different questions: one narrows a board, the other ranks a corpus. A node still building its index says so rather than answering empty, because "there is nothing" is what gets a duplicate filed |
| `merge_work_item` | fold a duplicate into the item that survives: the duplicate is linked to it, its **subtasks are re-parented onto it** (`move_subtasks`, true unless you say otherwise), and the duplicate is closed as `cancelled`. Nothing is destroyed and both histories stay readable. Closing a duplicate by hand instead leaves its subtasks under a closed parent, where nobody finds them |
| `get_work_catalogue` | the types a task may be and the fields it may carry |
| `list_projects` | every project work is filed into, with how much open work each holds and who leads it |
| `describe_project` | one project in full: the six statuses with what each means, the types it files, the fields grouped by which type they apply to (required first, with their options), its tags and its lead. Omitting the project means the seat's own |
| `write_project` | a project's own settings. Declaring a **tag** is open to every seat; renaming or archiving one, declaring project fields and setting the default assignee are the project **lead's**; archiving the project is the lead's too, and is never an agent's — see [Who may do what](#who-may-do-what) |
| `task_activity` | what HAPPENED, in the order the log made it happen: every change to one task or one project, with who made it and exactly which fields moved |
| `my_work` | everything this seat is expected to look at, in one call — see below |

### When a task is due and how big it is

Both write tools take four scheduling arguments, and every one of them is the
input to something the tracker already reports:

| Argument | What it sets |
|---|---|
| `due` | when the task is due. A date (`2031-04-16`), an instant, or one of the relative words the `due=` FILTER reads — `today`, `tomorrow`, `eow` (the week's end — midnight ending Sunday, since a week starts on Monday), `eom` (midnight ending the month), or an offset like `+7d`. One grammar for both, because a seat that can ask for "everything due this week" must be able to say "due this week" about one task. A token that named a DAY sets the all-day flag, so a renderer shows "16 April" rather than "16 April, 00:00" for a date nobody gave a time to. |
| `start` | when work on it should start, in the same spellings. |
| `estimate_minutes` | how long it is expected to take. |
| `points` | how big it is on the team's own scale. |

`estimate_minutes` and `points` are the two halves of a size, both carried
rather than one chosen: which one a team uses is a team's own habit, and the
totals (`totals=points:sum`, `totals=estimate_min:sum`) and the workload
answer both.

On an **update**, passing `null` clears the value: "leave the due date alone"
and "this task has no due date any more" are different edits, and a tool that
could only express the first would make a date impossible to take back off. On
a **create** there is nothing to clear, so `null` is ignored.

A value this grammar cannot read is **refused naming the argument**, never
dropped. Silently ignoring a due date is the worst of the three outcomes: the
write succeeds, the answer says `applied`, and the task simply has no due date
— which a model reads as having set one.

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

`write_project` is the one project **write** a seat holds, and every seat holds
it for one facet: declaring a tag. A project's name, purpose and owning unit
are **chart-owned** — written by the epoch apply from the org chart and by
nothing else — so nothing writes them here at all; its field declarations, its
default assignee and renaming or archiving a tag are the **lead's**, and
archiving the project itself is the lead's and **never an agent's** — a seat
that leads the project is still refused it. Each facet is decided inside the
verb once the call is read, and each refusal names what it needed.

An operator holds the same thirteen and more that no seat does, including the
two below. `remove_work_item` puts an item
in the **trash** and `restore_work_item` takes it out again, at any age. On the
assistant both take `fleet:operate`: the tool names an item rather than the
project it is filed in, so the project-lead half of the rule that governs them
has no project to ask about. A removal hides an item from every list and board and destroys nothing — its
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

Seat tools read at the **`linearizable`** level: a seat that files a task and
then lists its project sees the task it just filed, because every read
establishes the log's end before answering. Every write's answer also carries
the `position` its record landed at, which a client outside the engine — the
dashboard, an operator's assistant — hands back as `min_position` to read the
write back at a cheaper level. See [Read consistency](consistency.md).

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

## Who is carrying how much

**The workload** answers, for everybody at once, what they are holding. It
counts **open** work — every task assigned to them, whatever its dates —
because "who is carrying the most" is a question about a whole queue.

**Both sizes are carried, never one chosen.** Which of `points` and
`estimate_min` a team uses is that team's own habit, so an answer that picked
one would be wrong for everybody sizing in the other.

Two things it will not do:

- **It does not invent a ceiling.** There is no capacity to be over, because
  there is no number that is "full": a queue of thirty is heavy in one company
  and a quiet week in another. The dashboard's bar is drawn against the
  **heaviest queue on screen**, which is a comparison between people rather
  than against a target nobody set.
- **It does not re-rank.** The engine answers heaviest first and a screen keeps
  that order, because a second ordering would make two screens reading one
  answer disagree about who is at the top.

Beside the totals it carries the three shapes of work that is not simply in
progress — **blocked**, **overdue** and **unscheduled** — because a person
whose whole queue is blocked has a different problem from one who is simply
busy, and a total alone cannot tell them apart.

`GET /work/workload`, optionally narrowed to one `unit` — the people whose open
work sits in projects that unit owns.

## What a person can do

**The dashboard** renders the board, the list, the calendar, the timeline, the
table and the trash over the same queries a seat's tools use, against this
node's own copy. Every answer says how far behind that copy is.

**Your own AI assistant** can reach the same tracker over MCP, at
`/operator/mcp`. It serves the same work tools above, ten more no seat is
given — `list_work_views`, `save_work_view`,
`write_work_catalogue`, `get_person`, `work_inbox`, `mark_inbox`, `set_pins`,
`set_priorities`, `remove_work_item` and `restore_work_item`
— the five page tools beside them and knowledge
search — the seat's own implementations, with one
field different: a write carries the **caller's** own name as its author — the
seat's handle with the author kind `human` for a person the identity directory
binds to a seat, and otherwise the credential's whole login (`token:ops`,
`jane.doe`) with the author kind `operator` (see [who a write is attributed
to](../reference/api-endpoints.md#who-a-write-is-attributed-to)).
There is deliberately no way for the caller to name a seat to act
as — a tracker whose author field is chosen by the writer is not an audit
trail. The personal tools are the caller's own for the same reason:
`my_work`, `mark_inbox` and `set_pins` take no name at all, and
`list_work_views` renders **your** strip — the shared views, your personal
ones and your pins — under the same name those are written with, with no
argument that could name somebody else's.

**Whether the call is allowed is decided exactly as a seat's is**: every tool
on that surface goes through the same authority table, over the same chart,
and a refusal names the rule that refused it. What differs is only who is
asking. A caller's grants come from their credential, and the **lead
relations** come from the seat the [identity
directory](../concepts/identity-and-access.md#the-binding-has-two-ends-and-only-one-of-them-arbitrates)
binds them to — so a person bound to the seat that leads `ENG` may re-route
`ENG`'s work and declare its fields, and an unbound caller reaches those only
through `fleet:operate`.

**The REST API** serves the read side at `/work`, `/work/{id}` and
`/work/views`. Writes go through a seat's tools or the operator MCP, both of
which are attributed to somebody. `/work` and `/work/views` answer their
personal parts — `preset=my_queue`, `preset=priorities`, the pins and personal
views that order a strip — for the **caller's own seat**, and take no
parameter naming anybody else's: the engine already knows who is asking, and a
caller bound to no seat gets the shared strip. The questions that are *about*
one person — their record, their day, their inbox — do take a handle, and
naming somebody else's is decided as reading their record is: that person,
whoever leads them, or `fleet:operate`, with a `503` rather than a refusal
from a node that cannot read its chart.

## Who may do what

One table decides every gesture above, for a seat's own tools and for the
operator's assistant alike. "`fleet:operate`" below is the deployment's own
grant, which overrides every relation; no seat carries it. A **lead** is
whoever the org chart says leads the thing: a unit's effective lead (inherited
from above when the unit names nobody), the seat whose own project it is, or —
for a person — anybody above them in the management chain.

| Gesture | Who may |
|---|---|
| Read the board, a task, a project, the catalogue, the views | `state:read` — every seat holds it |
| File, update, comment on, merge and relate tasks; ask a colleague | `work:write` — every seat holds it |
| Declare a tag on a project | `work:write` — any colleague |
| Point a task at another team (`routing_unit`) | the lead of the project the task is filed in, or `fleet:operate` |
| Declare a project's fields, set its default assignee, rename or archive a tag | the project's lead, or `fleet:operate` |
| Archive the project | the project's lead or `fleet:operate` — **and never an agent**, whatever it leads or holds |
| Declare task types and workspace fields (`write_work_catalogue`) | `config:write` |
| Save a view | see [Views](#views): a personal view is its owner's, a shared one its container's |
| Set somebody's priorities | that person, whoever leads them, or `fleet:operate` — and a **seat** never sets anybody's but its own |
| Mark an inbox, set pins | the person whose record it is, or `fleet:operate`; never a lead. The two tools take no handle at all, so a caller only ever writes their own |
| Remove and restore a task | the operator's assistant only — see [What a seat can do](#what-a-seat-can-do) |
| Purge a task | a person or an operator token — see [below](#removing-deleting-and-purging) |

**A node that cannot read its chart does not say no.** Every relation above is
asked of the chart this node is running, and a node that is booting, applying a
revision or behind the chart log cannot answer it. That is reported as
*cannot tell* — a tool's refusal says this node could not decide, and a route
answers `503` — rather than "you do not lead this", which would send a lead
off to ask for an authority they already hold. `fleet:operate` is checked before the chart is
asked, so the deployment's own grant never waits on it. The whole table, and
the rules outside the tracker, are in [Identity and
Access](../concepts/identity-and-access.md#the-authority-table-one-function-decides).

## A handle is checked before it is stored

Every argument that names a colleague — a task's assignee, a project's default
assignee, whose priority list is being written — is resolved against the
company's own roster before the write, and a name nobody has is **refused**,
listing the seats.

That is not tidiness. An unknown handle fails *silently and permanently*: it is
stored, it rides the change's routing snapshot, it becomes a candidate — and
the wake path drops it against the live roster with no error and no log. The
write answers `applied`, and the person it named never hears anything. A
misspelling is indistinguishable from a colleague who is simply quiet.

A name is **resolved**, not merely checked — so a caller that typed a role's
name rather than its handle gets the handle back rather than a refusal.

### Dependencies, and the set-valued arguments

A **dependency** is the one relation with two ends. `waiting_on` is authored on
the task that is blocked; the blocker carries the dependent's id, so closing it
can say who it unblocks without scanning every task in the company.

Both ends are written, and in that order: the blocker's row is read first, so a
blocker that is gone, tombstoned or already at its 64 dependents refuses the
edge before anything is published. Then the authored edge lands, then the
mirror. The mirror is **best effort** — the edge is already durable without it
— so a mirror that lost its race leaves a **one-sided** edge, and the `tracker`
duty writes the missing commit 30 seconds later. That repair is the only wake
the blocker's side gets: the authored commit routes to the *dependent's*
watchers, so "the wake went out with the other commit" was never true.

When the mirror can never be written — the blocker is gone, was removed since,
or is full — the edge is stamped **permanently** one-sided and never retried.
Both states are in the attention queue: `flag=one_sided` is the repair still
pending, `flag=one_sided_final` the one a person has to resolve. The `flag`
filter takes any number of values and matches a task carrying **any** of them.

**Every row says what it waits on.** A listed task carries `blocked` — one bit,
"something is holding this up" — and `waiting_on`, the same edges carrying
*which* task, whether that blocker is still `open`, and whether the edge is
`one_sided` or `one_sided_final`. The two are computed from one set of rows in
one statement, so `blocked` is exactly "some entry in `waiting_on` is open" and
a screen can never show a blocked badge beside no dependencies. A blocker your
own filter excluded is an id you hold no row for — the honest answer, since the
edge exists and that page cannot draw it. Cleared edges stay on the row rather
than disappearing when the blocker finishes: what a plan looked like once it
was executed is the thing a timeline is for.

`waiting_on`, `blocking`, `linked` and `linked_pages` on `update_work_item`
take one of two explicit shapes and never a bare list:

```json
{"waiting_on": {"add": ["ENG-7"], "remove": ["ENG-2"]}}
{"waiting_on": {"set": ["ENG-7", "ENG-9"]}}
```

A bare list is refused naming both, because the two readings of it are
opposite: as a delta it adds one edge, and as a set it silently drops every
edge not repeated. `create_work_item` keeps a plain `waiting_on` list — a new
item has no dependencies to replace.

### A question on a task

A comment can carry an `ask`: a colleague's handle, meaning this comment is a
question that person owes an answer to. They are woken **asking for one**,
rather than told about activity, and they start following the item. An ask does
**not** hand the item over and does **not** block a close — a question nobody
has answered is not a reason to hold delivered work open.

The answer closes it. `answers` names the question's comment id, and is
**inferred** when exactly one open question on the item is addressed to the
caller; with several it is required, and the refusal lists them. Answering
wakes the person who **asked** — not the person who just replied, which is what
routing off the answering comment's author would have done.

`my_work` reads both sides: `asked_of_me` is the questions waiting on this
seat, and the `has_open_asks` filter finds the items carrying any.

A comment from somebody who is not the assignee, naming nobody and asking
nobody, still wakes the assignee — unaddressed, which a turn may absorb without
replying. The result says so in a `warnings` line, because a commenter
expecting an answer otherwise gets silence with nothing to explain it.

### What an answer may weigh

A tool answer is read by a **model**: every byte lands in a context window
beside the system prompt, the conversation and whatever the turn has already
accumulated. So one answer is capped at **64 KiB** — about a quarter of the
smallest context the shipped models offer, spent on a single call.

What keeps answers under it is that every collection which grows is paged:

- **Comments** come back twenty at a time, newest page first, each body cut to
  2 KiB with a marker. `comments_cursor` reads the page before. Twenty comments
  at their full length would be 640 KiB — ten times the ceiling — for a thread
  nobody asked to read in full.
- **The description** comes back cut to 4 KiB with a marker, and `body: true`
  returns it whole. It is the one value on a detail read that used to carry no
  bound: a description at its 32 KiB cap would have spent half the ceiling on a
  part nobody named, and `include` governs the collections *beside* the task,
  never the task itself — so an item with a long description met a refusal
  whose own advice could not help.
- **History** is the fifty most recent changes. `work_activity` is what pages
  properly, with a cursor that survives a reanchor.
- **Options** page by whole fields: a field whose list does not fit comes back
  with **none** of its options rather than some, because half a list is worse
  than none — a reader would choose from it and believe it was the set.
  `options_total` beside `options_shown` is what says otherwise.

When an answer still exceeds the ceiling — a task with a long body, a full
thread and sixty-four links — it is **refused** naming what would narrow it,
rather than truncated. A truncated JSON answer is not an answer: a model handed
half an object either fails to parse it or reads the half it got as the whole.

## What wakes a seat

A change that concerns somebody becomes a **wake** — a turn on that seat, with
a prompt written for the reason it reached them. Most wakes are about a task:
you were assigned it, mentioned on it, watching it, blocked by it. One is not,
and it exists because the thing that moved is not a row on a board:

**One wake per person per change**, whatever number of reasons name them: the
first reason in the precedence order wins and the rest are dropped. Somebody
mentioned on a task they are watching hears that they were mentioned.

The order puts **what this change did to you** ahead of **the role you hold**.
Being @-mentioned, being asked a question, having your question answered, and
learning that somebody's work now waits on yours all outrank being the
assignee — because each says something the standing role does not, about this
change. Below those come assignee, then reporter, then the following
reasons — collaborator, watcher, and last the lead fallback, which reaches a
lead only when the change named nobody else at all.


| Wake | Who hears it | What it asks |
|---|---|---|
| `prioritised` | the person whose queue somebody else wrote | **an answer**: take it up, or say why you cannot |

A second is about a task and is listed apart because of what it says rather
than what it is about:

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

`prioritised` **addresses** its recipient: being told what to do next by
somebody above you is an instruction, and silence on it is indistinguishable
from a message that was lost. It names the tool that answers its own question —
`my_work` — rather than pointing at the object it is written on, because a
person's handle is not a task key and a pointer at one costs a round and a
failed tool call to discover. A `purged` wake sends you nowhere for the sharper
version of the same reason: the row is gone from every node, so the tool would
answer `not_found`.

## A person's own state

A human has a record of their own beside the work: an **inbox**, a **queue**
and their **pins**. Three parts of one document, with three different
authorities over them — which is why they are three separate writes.

| Part | Who may write it |
|---|---|
| **inbox** — read, unread, snoozed, and how far you have read | only on behalf of the person whose it is — or `fleet:operate`, which exists so a departed person's queue can be unstuck |
| **pins** — pinned views and starred things | the same |
| **queue** — the order you mean to work in | yours, **or** whoever leads you, or `fleet:operate` — and never a seat's for anybody but itself |

Somebody else marking your work read is the one thing an inbox must never
allow: the item is then gone from the only place you would have looked for it,
and nothing anywhere says who removed it. A pin is the same rule for the
plainer reason — one somebody else can set is one that moves under you. A lead
gets no way in to either: re-ordering what a report works on is a lead's job,
and marking their mail read is not. `mark_inbox` and `set_pins` take no handle
at all, so through them a caller only ever writes their own.

The **queue is the exception**, and deliberately: telling somebody what to do
next is what a lead is for. A **seat** is the one party that may not write
somebody else's — not even a seat that leads them, because an agent
re-ordering a colleague's list is a hand-off in disguise, and it bypasses the
guarded take and the reassignment budget that a real hand-off goes through. A
person who is neither the owner nor a lead is refused too, unless they hold
`fleet:operate`: being a human is not, on its own, authority over somebody
else's day.

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

**The feed and the marks are two reads.** `work_inbox` is what the company
*asked of you*: one entry per routed change, newest first, carrying the single
reason it reached you under, the subject, who made it, and an excerpt. It is
written by the applier when the change lands — so it is there whether or not
anybody was online, and a person who has marked nothing still has an inbox.
`get_person` is your own *marks over that feed*: what you have read, what you
snoozed, how far you have got. Neither is derived from the other, which is why
an inbox entry carries a record id and a position and no content at all.

**The primary half is yours to declare.** `mark_inbox` takes `primary_reasons`
— which wake reasons are yours to act on — and `work_inbox` labels every notice
with it, returning the rest as context rather than hiding it. Saying nothing
takes the shipped default: `mention`, `asked`, `answered`, `assignee`,
`unassigned`, `reporter`, `unblocked` and `prioritised`, which is every reason
that changes what you should do next. An empty list means *you have not said*,
never *nothing is primary* — the other reading gives a fresh company an inbox
whose primary half is blank.

**A person reads theirs at `#/inbox`**, which is the dashboard's landing
screen: the notices `work_inbox` returns, each labelled with the one reason of
eighteen that routed it, beside what is waiting on a decision. Which person is
decided by **who signed in**: the engine resolves the browser's session — or
the token it presented — to a principal, and the seat the identity directory
binds that principal to (`crewlet iam bind`) is whose queue it is. `#/me` is
that same person's own work, their priorities, asks and checklists. A caller
the directory binds to no seat is an ordinary state, and both screens say what
to bind rather than guessing whose queue to show. See [Humans
in the org](../concepts/humans-in-the-org.md#acting-as-your-seat-on-the-dashboard-and-the-api)
for the binding.

The marks are the ASSISTANT'S. The dashboard is read-only, because every write
here is attributed to somebody and a button in a browser would write as "the
dashboard", which is nobody — so `mark_inbox` is what an assistant calls when
you ask it to, and the screen shows what the engine recorded. Each notice
offers the call that would mark it, pre-filled and copyable, rather than a
control that pretends to send it.

**And the same rows answer the other way round.** `work_inbox` reads them by
recipient — one person, every change. `work_routing` reads them by *record* —
one change, every person — which is the question "did my comment reach the
person I meant", and it has never been answerable anywhere else. The table's
primary key is `(record_id, recipient)`, so both directions are index reads and
neither costs the other anything.

Read it at `GET /work/routing/{record_id}`, or open an item's **Woke** tab in
the dashboard. Every recipient names the one reason of eighteen that found them,
whether the notice *asks* something of them, and whether they were reached only
because nobody better was found — a lead who hears about a report's task
because the report has left, with the rank saying which substitute they were.

**An empty recipient list is three different facts**, and the answer's
`delivery` field says which. `nobody` means the routing genuinely resolved to
no one: every candidate was the person making the change, or has left the
company. `swept` means the change is older than the retention horizon below, so
the rows may have existed and been cleared — their absence is not evidence.
`unknown` means the caller stated no horizon, so nothing can date the absence.
And `quiet` means the commit carried no notification at all, which is most
field edits and every bulk one. The history row's own `notified` flag cannot
tell these apart: it says the commit *carried* a notification, never that
anybody was woken — the applier deliberately holds no roster, because two nodes
briefly on different epochs would then write different rows for one record.

**Inbox rows age out; the history does not.** `tracker.native.inbox_retention_days`
(default 365, 30..3650) is how long an entry lives. A sweep on every node
deletes what is past the horizon — per node rather than once across the fleet,
because each node applies the log into its own copy. A notice is a *pointer* at
a history row, and the history answers for ever: "what was I told about in
2024" is a `work_activity` question, not an inbox one.

Read at `GET /work/people/{handle}`, and through the operator MCP with
`work_inbox`, `get_person`, `mark_inbox`, `set_pins` and `set_priorities` —
never by a seat. A seat is not a human: it has a **mailbox**, which is the
durable subscription the engine attaches when it acquires the seat, and nothing
on a person's record describes one.

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
  than a screen. It is a filter every container ships a **tab** for
  ([Views](#views)), because the one thing a person needs after an assistant
  removes the wrong subtree is to see what was removed.
- **Delete** writes a marker. Every node drops every record about that task for
  ever, which is what stops a redelivery months later resurrecting it.
- **Purge** removes the rows. Its report comes back in **three groups**: what
  was purged, what could not be reached, and what is stale.

None of the three is a seat's. Removing and restoring are the operator's
assistant's, as [above](#what-a-seat-can-do); the deletion marker is written by
the purge itself, in the same apply that removes the rows; and a purge is an
**operator gesture** — a person or an operator token, never an agent and never
the engine — because it is the one operation with no inverse and nothing else
can be asked to confirm it:

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
