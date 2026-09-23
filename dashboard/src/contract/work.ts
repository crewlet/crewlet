/**
 * The work tracker's closed sets, as the dashboard's own copies: the shapes a
 * view is drawn in, the sort keys a header click sends, the change kinds a
 * history row names, and the axes a board is grouped on.
 *
 * Each is the ENGINE's — the query grammar refuses what it does not know, and
 * the applier writes what it writes — and each is held against it in both
 * directions by `internal/tracker/client_gate_test.go`. A separate build in a
 * separate language cannot import a Go identifier, so the copy is kept here,
 * in one place, where the gate can find it.
 */

/**
 * The renderings a view may be drawn in, and there are no others.
 *
 * HELD AGAINST THE ENGINE'S OWN CLOSED SET by a Go gate —
 * `internal/tracker/client_gate_test.go` — because this is a copy the
 * dashboard has to keep: it is a separate build in a separate language and
 * cannot import `tracker.ViewTypes`. A shape the engine mints that this union
 * does not name is a tab that renders a blank body, and one named here the
 * engine refuses is a branch nothing can reach. Both are silent.
 *
 * `timeline` draws the rows against a DATE AXIS, which is the one arrangement
 * the others cannot express: a list orders by a column, a board groups by one,
 * and a calendar puts a task on the day it is due — none of them can show that
 * a task spans three weeks, or that it cannot start until another finishes.
 * `table` puts one field per column, which is what a question about a FIELD
 * rather than about a task needs.
 *
 * THE TRASH IS NOT ONE OF THESE. It is a table carrying `removed=true`,
 * because what makes a listing the trash is the query rather than the drawing
 * — so every view saved with that parameter is one.
 */
export type WorkViewShape = "list" | "board" | "calendar" | "timeline" | "table";

/**
 * Every column a header click orders by — and each name IS one of the query
 * grammar's own sort keys.
 *
 * `DataGrid` writes the column's key straight into `sort=`, which the work
 * grid (`routes/work/shapes/Grid.tsx`) sends to the engine, and `ParseQuery`
 * REFUSES a sort key it does not know rather than ignoring it. So one wrong
 * header does not mis-sort a column: it takes the whole board down with a
 * refusal, on the click.
 *
 * Held against the grammar by `internal/tracker/client_gate_test.go`, because
 * this is a copy the dashboard has to keep — a separate build in a separate
 * language cannot import a Go identifier. The grid's `SortKey` union over it
 * makes a typo a compile error; the gate makes a RENAME in the engine one. It covers BOTH sets, which
 * is what collapsing them bought: the list's heads are sortable now, and they
 * are the same declaration the table's are held against.
 */
export const COLUMN_SORT_KEYS = [
  "title",
  "priority",
  "due",
  "start",
  "points",
  "estimate",
  "updated",
  "removed",
] as const;

/**
 * The orderings the ENGINE takes for the projects directory, as the
 * dashboard's own copy.
 *
 * It is a copy by necessity — this is a separate build in a separate language
 * and cannot import `tracker.ProjectSorts` — so a Go gate holds it against
 * that list in both directions (`internal/tracker/client_gate_test.go`). The
 * drift it catches is silent and total: a header carrying a key the engine
 * refuses turns one click into a `bad_params` refusal over the whole screen,
 * and an ordering the engine grew that no header offers is one nobody can
 * reach.
 */
export const PROJECT_SORT_KEYS = [
  "key",
  "name",
  "unit",
  "open",
  "done",
  "closed",
  "last_change",
] as const;

/**
 * The engine's change kinds, each with the mark it is drawn as and the phrase a
 * person reads.
 *
 * A MARK RATHER THAN A HUE, which is `lib/work.ts`'s own rule for a task type
 * (`TYPE_ICON`): a kind is identity, and identity is carried by a name, a mark
 * and a position. The item's history drew ONE mark on every row, so a
 * comment and a field edit were the same picture — while the cross-item feed,
 * which renders the kind as a tag in a column of its own, never had the problem.
 * This is that column, in the only form a dense one-line row has room for.
 *
 * AND A PHRASE, because `kind.replaceAll("_", " ")` after an actor's name reads
 * "ada watchers". A kind carries deltas exactly when an APPLY CAN COMPARE TWO
 * DOCUMENTS for it, and `tracker.TaskDeltas` now compares every field a patch
 * can move — so `watchers`, `checklist`, `archived` and `reparented` say what
 * moved and this phrase says what happened. What still reaches the reader
 * through this column ALONE is what no comparison can produce: the comment
 * family, which is a thread rather than a field of the task; `purged`, whose
 * subject no longer exists to compare; and a plain `removed` or `restored`,
 * where the only delta a tombstone holds is `removed_with` and a task removed
 * on its own went with nothing. The count is deliberately not stated — the
 * rule is, because a field added to the comparison tomorrow would falsify a
 * number nothing here gates.
 *
 * ONE DECLARATION, held against `tracker.ChangeKinds` by
 * `internal/tracker/client_gate_test.go`: this is a closed set the engine owns
 * and the dashboard cannot import, and a kind that lands in Go without landing
 * here draws the fallback mark and a bare word for ever, silently.
 *
 * `as const`, because a contract module imports nothing and so cannot name
 * the glyph type: every mark stays its own literal, and `lib/work.ts` returns
 * them as `MarkName`, which is where a mark the glyph set does not draw fails
 * to compile.
 */
export const CHANGES = [
  { kind: "created", mark: "add", phrase: "created it" },
  { kind: "fields", mark: "tune", phrase: "changed a field" },
  { kind: "status", mark: "cached", phrase: "moved it" },
  { kind: "assignee", mark: "person", phrase: "reassigned it" },
  { kind: "collaborators", mark: "group", phrase: "changed the collaborators" },
  { kind: "watchers", mark: "visibility", phrase: "changed the watchers" },
  { kind: "tags", mark: "tag", phrase: "changed the tags" },
  { kind: "relations", mark: "link", phrase: "changed a relation" },
  { kind: "routed", mark: "fork_right", phrase: "routed it" },
  { kind: "moved", mark: "move_item", phrase: "moved it to another project" },
  { kind: "reparented", mark: "account_tree", phrase: "changed its parent" },
  { kind: "checklist", mark: "list", phrase: "changed a checklist" },
  { kind: "archived", mark: "package_2", phrase: "archived it" },
  { kind: "comment", mark: "chat", phrase: "commented" },
  { kind: "comment_edited", mark: "edit", phrase: "edited a comment" },
  { kind: "comment_resolved", mark: "check", phrase: "resolved a comment" },
  { kind: "comment_removed", mark: "close", phrase: "removed a comment" },
  { kind: "removed", mark: "remove", phrase: "removed it" },
  { kind: "restored", mark: "settings_backup_restore", phrase: "restored it" },
  { kind: "purged", mark: "delete", phrase: "purged it" },
  { kind: "project_created", mark: "create_new_folder", phrase: "created the project" },
  { kind: "project_updated", mark: "folder", phrase: "changed the project" },
  { kind: "policy_changed", mark: "shield", phrase: "changed the project's policy" },
  { kind: "view_saved", mark: "save", phrase: "saved a view" },
  { kind: "catalogue_updated", mark: "settings", phrase: "changed the catalogue" },
  { kind: "prioritised", mark: "arrow_upward", phrase: "reordered somebody's priorities" },
  { kind: "person_updated", mark: "inbox", phrase: "changed their own bookkeeping" },
] as const satisfies readonly { kind: string; mark: string; phrase: string }[];

/**
 * The axes a board may be cut on, with what each is called in the picker.
 *
 * A COPY OF THE ENGINE'S `groupKeys`, held against it by
 * `internal/tracker/client_gate_test.go` in both directions: a value here the
 * grammar refuses takes the whole board down with a refusal, and a key the
 * engine takes that this list never names is an arrangement only a hand-edited
 * URL can reach — which is what `project` was until the gate named it. The
 * keys the menu deliberately leaves out are listed THERE, each with its
 * reason, so a grouping added to the grammar lands here or in that list.
 *
 * `workspace` marks an axis the engine takes only at workspace scope: inside a
 * project every row is in one project, and the grammar refuses the question.
 */
export const GROUP_AXES: readonly { value: string; label: string; workspace?: boolean }[] = [
  { value: "status", label: "Status" },
  { value: "status_group", label: "Status group" },
  { value: "assignee", label: "Assignee" },
  { value: "priority", label: "Priority" },
  { value: "type", label: "Type" },
  { value: "tag", label: "Tag" },
  { value: "project", label: "Project", workspace: true },
  { value: "unit", label: "Unit" },
  // WHEN THE WORK IS DUE, which is the one axis that is not a stored value:
  // the engine cuts it against the COMPANY's day rather than the reader's, so
  // the bands, a row's overdue flag and every `due=` filter agree about a
  // task. Offered on every shape — "what is late, what is today" is a
  // question somebody asks of a list as readily as of a board.
  { value: "due:bucket", label: "Due" },
];
