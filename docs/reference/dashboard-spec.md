# Dashboard Spec

A `dashboard` artifact is a board of figures over live data. Its version holds
one spec, `dashboard.json`, and any data files its datasets read. The
dashboard draws the board natively with its own components: the spec carries
no author code, and every figure names the dataset it was drawn from and when
that was true. This page is the whole contract; a spec outside it is refused
at publish with every finding, each naming the field (and, for a data file,
the line), never dropped on read.

Every live figure is decided **as its viewer**
([ADR-0043](../../adr/0043-a-dashboards-live-data-is-read-as-its-viewer.md)):
one board shows each person what they themselves may read, so sharing a board
never shares the data behind it.

## The spec

```json
{
  "v": 1,
  "title": "Delivery",
  "params": [
    {"key": "scope", "label": "Whose work", "default": "mine",
     "options": [{"value": "mine", "label": "Mine"}, {"value": "all", "label": "Everybody"}]}
  ],
  "datasets": [
    {"id": "work", "title": "Open work", "source": "engine",
     "engine": {"name": "tracker.items", "params": {"scope": {"param": "scope"}}}},
    {"id": "deals", "title": "Pipeline", "source": "store", "store": {"collection": "deals"},
     "columns": [{"key": "amount", "type": "number"}, {"key": "region", "type": "text"}]},
    {"id": "by_region", "title": "Pipeline by region", "source": "calc",
     "calc": {"from": "deals", "steps": [
       {"op": "group", "by": ["region"], "aggregates": [{"column": "amount", "agg": "sum", "of": "amount"}]}
     ]}}
  ],
  "widgets": [
    {"id": "open", "type": "stat", "title": "Open items", "dataset": "work", "agg": "count", "w": 3},
    {"id": "regions", "type": "bars", "title": "Pipeline by region", "dataset": "by_region",
     "label": "region", "value": "amount", "format": "compact", "w": 6},
    {"id": "mine", "type": "table", "title": "My work", "dataset": "work",
     "columns": ["key", "title", "status", "due"], "w": 12}
  ]
}
```

| Field | What it is |
|---|---|
| `v` | The format version, `1` |
| `title` | The board's title, at most 120 bytes |
| `params` | The board's filters, at most 8 |
| `datasets` | 1 to 32 named sets of rows |
| `widgets` | 1 to 64 figures, a group's children counted |

An id (a dataset's, a widget's, a filter's key) is a lowercase letter, then
lowercase letters, digits and `_`, at most 40 characters. A field the format
does not have is refused naming it, so a misspelt `widgets` is an error rather
than an empty board.

## Datasets

Every dataset has an `id`, a `title` and a `source`, and exactly the one block
its source names.

| `source` | Block | Rows | Kept current |
|---|---|---|---|
| `store` | `"store": {"collection": "deals"}` | A collection of the artifact's own datastore, read under the collection's read rule for each viewer | Live: every open board reads it again when the datastore pushes a change |
| `engine` | `"engine": {"name": "...", "params": {...}}` | A named read from the [catalogue](#the-engine-source-catalogue), answered as the viewer | On the source's change signal, else on the declared `refresh`, else when the board opens or the viewer refreshes |
| `file` | `"file": {"path": "data/targets.csv"}` | A CSV (header first) or JSON array of objects, a file of the version | Fixed with the version |
| `calc` | `"calc": {"from": "...", "steps": [...]}` | Rows derived from another dataset by [steps](#calculations) | Whenever an input is read again |

A `store` or `file` dataset declares its `columns`, each `{"key", "type",
"label", "words"}` with a type of `text`, `number`, `date` (a `YYYY-MM-DD` day
or an RFC 3339 instant) or `boolean`, and optionally the `key` column naming a
row. A text column may name the vocabulary its ids are drawn in (`words`):
`tracker_status`, `tracker_status_group`, `tracker_priority` or
`agent_activity`. The values stay the ids (`in_progress`), which a filter or a
calculation compares, and a reader sees the words the domain's own screens use
(In progress). The catalogue's own columns already name theirs.

A day is drawn on a chart as that calendar day wherever the reader is, and a
chart over days labels its axis with dates.
A store dataset always carries `_id`, the document id, and is keyed by it
unless it names another key. An engine dataset's columns are the catalogue's
and a calculation's are its steps', so neither declares any.

A **file** dataset's file is read at publish and held to its columns: every
declared column must be in the header (or in some object), every value must
be one of its column's type, a key column holds no value twice, and the file
is at most 8 MB and 5,000 rows. A column the spec does not declare is not
read. A value that breaks a column is refused naming the file and the line.

A **store** dataset reads whatever the collection holds at the time: a value
that is not one of its column's type is answered as no value, since the rows
are written after the board is published. Write the collection's schema with
`set_artifact_schema` and its rows with `write_artifact_data`; integration data
(GitHub, Jira, Slack) reaches a board this way, written by an agent.

An engine dataset may declare `"refresh": {"every_seconds": N}`, 60 to 86,400.

Every dataset answers at most 5,000 rows, and says `truncated` when there were
more.

## The engine source catalogue

`list_dashboard_sources`, `crewlet artifacts sources` and
`GET /artifacts/sources` list it, with each parameter's type and bounds.
`GET /artifacts/sources/preview/{name}` reads one source's rows as the caller
before any board exists, which is how the dashboard's New dashboard page draws
its suggested figures.

| Source | Reads | Who may read it | Parameters | Columns | Refreshes on |
|---|---|---|---|---|---|
| `tracker.items` | Work items, one row each | People and agents holding `worklet` | `scope`: `all`, `mine` (the viewer's own), `queue`, `priorities`, `triage`, `blocked`, `overdue`; `project`; `status_group` (absent reads the open work); `limit` 1 to 500, default 100 | `key`, `title`, `status`, `status_group`, `priority`, `assignee`, `project`, `due`, `updated`, `points`, `blocked`, `overdue` | Its interval |
| `tracker.workload` | Open work per assignee | People and agents holding `worklet` | `unit`, a team's key or name | `handle`, `open`, `points`, `blocked`, `overdue`, `unscheduled` | Its interval |
| `spend.seats` | Token spend per seat | Everybody the company admits | `days` 1 to 90; absent reads the live window | `handle`, `name`, `person`, `total_tokens`, `input_tokens`, `output_tokens`, `calls` | Every spend push |
| `spend.daily` | Token spend per company day and band | Everybody the company admits | `days` 1 to 90, default 14; `group`: `seat`, `phase`, `model`, `provider`, `unit`, `worker` | `day`, `band`, `total_tokens` | Every spend push |
| `agents.status` | Every agent seat and what it is doing | Everybody the company admits | none | `handle`, `name`, `activity`, `phase`, `paused`, `last_turn_at`, `last_outcome` | Every agents push |

**Each source is the question the dashboard's own screens ask.**
`tracker.items` is the board's `work_items`, `tracker.workload` is
`work_workload`, `spend.seats` is `tokens`, `spend.daily` is `token_series`,
and `agents.status` is `GET /agents`, each asked on the viewer's own request.
A viewer the source refuses sees the figure say so, with the permission that
would open it; the rest of the board draws.

**The spend sources read a window of company days**, and the catalogue marks
them `windowed`. A figure over a windowed source that answered no rows says
"Nothing in this window"; a figure over any other source says "Nothing to
show", because open work and the agents are read as they stand now.

**A count or a sum of no rows is 0.** The source answered, and a total of
nothing is a measured nought, so a stat with `agg: count` or `agg: sum` over
an empty answer draws 0, as a calculated dataset's group does. A fold that
picks or averages a value (`first`, `last`, `min`, `max`, `avg`) over no
rows, and any fold over rows none of which holds a value, draws the empty
mark: there is no value to show, which is not the same as a value of 0.

A parameter is a literal (`"scope": "mine"`, `"limit": 50`) or bound to a
filter (`"scope": {"param": "scope"}`), in which case every choice of that
filter must be a value the parameter takes; a choice whose value is empty
reads the parameter's default.

## Filters

```json
{"key": "region", "label": "Region", "default": "",
 "options": [{"value": "", "label": "All"}, {"value": "emea", "label": "EMEA"}]}
```

A filter is a closed set of 1 to 50 choices, and its `default` is one of them.
A viewer picks among the choices; a value the filter does not offer, or a key
the board has no filter for, is refused before any source is read, so nothing
a viewer types reaches a read the author did not write.

## Calculations

A `calc` dataset reads its `from` dataset and applies 1 to 16 steps, each one
of a closed set, never a program. It may stand on at most 4 other
calculations, and never on itself. Every step reads columns its input has and
says which it answers, so a widget over a calculation is held to them at
publish.

| `op` | Fields | Does |
|---|---|---|
| `filter` | `where`: conditions `{"column", "cmp", "value"}`, all of which hold | `cmp` is `eq`, `ne`, `lt`, `le`, `gt`, `ge`, `present` or `absent`; `value` a literal of the column's type, or `{"param": "<filter>"}`, which holds for every row when the filter's value is empty |
| `group` | `by` columns, `aggregates` `{"column", "agg", "of"}` | One row per distinct `by` value, ordered by it; `agg` is `sum`, `avg`, `min`, `max`, `count` (rows, or rows with `of`), `first` or `last`. With no `by`, one row over the whole set |
| `compute` | `column`, `fn`, `left`, `right` | `add`, `sub`, `mul`, `div` (no value where the right side is zero) over number columns or numbers; `day`, `week` (its Monday) and `month` cut a date column (`left` only) in UTC |
| `join` | `with`, `on`, `columns` | Adds `columns` of the `with` dataset to each row whose `on` value matches (the first match in its order); a row with none keeps no value |
| `sort` | `order`: `{"column", "desc"}` | Orders the rows, stably |
| `limit` | `count` | Keeps the first rows |
| `select` | `columns` | Keeps the named columns, in order |

A calculation is **deterministic**: it reads no clock or locale, a group and a
sort order totally, a sum adds in row order, so one input answers one table on
every node. A calculation over an input its viewer may not read is refused
whole, never answered in part.

## Widgets

Every widget has an `id`, a `type`, a `title` (a note's is optional), a width
`w` of 1 to 12 grid columns and optionally a height `h` of 2 to 60 rows
(absent for its content's height). It names only the fields its type reads;
any other is refused.

| `type` | Draws | Fields |
|---|---|---|
| `stat` | One number | `dataset`, `value` (a number column; optional for a `count`), `agg` (the last row's by default), `format`, `unit` |
| `meter` | A number against a maximum | as `stat`, plus exactly one of `max` (a number) or `max_field` (a column folded as `value` is) |
| `timeseries` | Lines over time | `dataset`, `x` (a date column), `y` (1 to 6 number columns), `by` (a text column splitting one `y` into a series per value), `format`, `unit` |
| `columns` | Stacked columns over time | as `timeseries` |
| `activity` | One value over time, as a strip | `dataset`, `x`, exactly one `y`, `format`, `unit` |
| `bars` | A ranked list | `dataset`, `label` (text), `value` (number), `limit` 1 to 50 (default 10), `format`, `unit` |
| `share` | The parts of a whole | as `bars`, `limit` at most 8 |
| `table` | The rows | `dataset`, `columns` (1 to 16), `limit` 1 to 500 (default 100) |
| `note` | The author's words | `text`, at most 2,000 bytes, and no dataset |
| `group` | Widgets under a title | `title`, `tone` (`none`, `purple`, `cyan`, `green`, `amber`, `rose`, `blue`), `children` (no group inside a group) |

`format` is `number` (grouped, up to two decimals), `integer`, `percent` (a
ratio: 0.25 is 25%) or `compact` (12.3k). `unit` is at most 16 bytes. A stat's
or a meter's number is read at a glance, so it is `compact` unless `format`
says otherwise; a chart's scale is shortened from a thousand (`7.5k`), without
the unit, and its tooltip reads each value in the widget's format and unit. A
meter is drawn as the number, what it is out of, and a bar against the maximum
that turns to the warning hue from three quarters of it and to the danger hue
at it.

A chart tells four series apart by hue and draws a fifth in the residual grey.
Past five, the first four keep their hues and the rest are summed into one
series named Other: the `y` columns in the order written, a `by` split's values
the biggest over the window first, and a `share`'s parts in the order its rows
come. The dataset's rows are untouched, so its CSV and preview carry every
series. A chart of no rows says so in a sentence in its place.

A `table` draws its rows in the order its dataset answers them, and a reader
does not re-sort them: sort and cut the rows in the dataset (a calculation's
`sort` and `limit`), since sorting the first eight rows by another column would
read as an order over every row. Each column is as wide as what it holds; the
one column of free text in words, the longest such (a title), takes the room
the others leave and ends in an ellipsis when there is less. A date is written
as a reader writes one (`Sep 1`, the year only when it is not this one, and an
instant with its time), a boolean as Yes or No.

## Reading a board

- `GET /artifacts/{id}/versions/{n}/dashboard` answers the board: the spec,
  each dataset's columns and key, how it is kept current, and the catalogue's
  words for an engine source. It carries no row.
- `GET /artifacts/{id}/versions/{n}/sources/{dataset}` answers one dataset as
  its viewer, the board's filters in the query string. See the
  [API reference](api-endpoints.md) for the answer and its statuses.
- `get_artifact` with `view: "outline"` gives an agent the filters, datasets
  and widgets, never a figure.

The gallery's cover for a dashboard shows its first stats' labels and their
datasets' titles; a figure is always the viewer's, so no number is drawn on a
card from anybody else's data. The board's figures are exported in the
browser: a widget's rows as CSV (a text cell a spreadsheet would run as a
formula is written as text), and a chart as PNG.

## Comments and presence

A reader looking at a widget is in the presence region `widget:<id>`. A
comment (when comments ship) anchors to a widget, or to one data point of it:
the row of its dataset by the dataset's key, and a column.

## Bounds

| Bound | Value |
|---|---|
| Widgets, a group's children counted | 64 |
| Datasets | 32 |
| Filters, choices per filter | 8, 50 |
| Calculation steps, calculations stood on | 16, 4 |
| Rows one dataset answers | 5,000 |
| A file dataset's file | 8 MB |
| An engine dataset's interval | 60 seconds to a day |
| An engine answer kept per viewer | 30 seconds |
| Live reads per viewer on one board, per node | 2 a second, a burst of 64 |
| Live reads by everybody on one board, per node | 20 a second, a burst of 320 |
