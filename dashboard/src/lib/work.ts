/**
 * The tracker's vocabulary and arithmetic, over values.
 *
 * NO REACT AND NO DOM, for the reason `textindex`'s ranking and the tracker's
 * own coercion table are pure on the engine's side: a rule that can only be
 * exercised by rendering a screen is a rule nobody re-measures, and every
 * judgement here — what a status is called, which column a filter builds, which
 * day a due date lands on — has a wrong form that reads as a different fact
 * rather than as a missing one.
 *
 * It also exists because the screens now SHARE these answers. A board, a list,
 * a calendar, a peek panel, an item page and My work all draw the same task,
 * and the previous screen's helpers lived inside the one file that happened to
 * need them first — which is how one screen came to import a change renderer
 * from the board.
 */

// THE ABSENT MARK IS THE DESIGN SYSTEM'S, for the reason `lib/format.ts`
// gives at its own copy of this import: spelled here as a literal it is a
// SECOND glyph beside `EmptyValue`'s en dash, and the work screens drew both
// at once. The constant rather than the component, because this module has
// no React in it and must not acquire any.
import { EMPTY_VALUE } from "@crewlethq/ui";
import { browserDay, fmtDate, fmtDateTime, humanize, parseUTC, plural } from "./format.ts";
// THE ONE IMPORT THAT REACHES A RENDERING FILE, and it is not a breach of the
// rule above: `plainText` is a pure string→string function that happens to live
// beside the grammar it has to agree with. A private stripper here instead would
// be a second copy of markdown's rules, which is the drift `textcut` and `whsec`
// each record.
import { plainText } from "./markdown.ts";
import type { MarkName } from "~/ui/glyph.tsx";
import type {
  WorkActivityRecord,
  WorkChange,
  WorkFieldDef,
  WorkFieldValue,
  WorkGroup,
  WorkProjectRow,
  WorkProjectTag,
  WorkStatus,
  WorkStatusDef,
  WorkSummary,
  WorkTypeDef,
  WorkView,
  WorkViewShape,
} from "~/protocol/index.ts";

export type Tone = "neutral" | "positive" | "caution" | "critical" | "info";

// ---------------------------------------------------------------------------
// The closed sets
// ---------------------------------------------------------------------------

/**
 * The tracker's SIX statuses, in the order a board draws them.
 *
 * `blocked` is deliberately not among them: a blocker is DATA carried beside
 * the status, because a task can be both in progress and blocked and a single
 * field cannot say so. `cancelled` is here and reads as "finished without
 * being delivered", which is the whole of what a close reason used to say.
 */
export const STATUSES: { value: WorkStatus; label: string; group: string }[] = [
  { value: "todo", label: "To do", group: "not_started" },
  { value: "in_progress", label: "In progress", group: "active" },
  { value: "in_review", label: "In review", group: "active" },
  { value: "done", label: "Done", group: "done" },
  { value: "cancelled", label: "Cancelled", group: "done" },
  { value: "closed", label: "Closed", group: "closed" },
];

/** A status is a STATE, which is the one thing colour is spent on here. */
export const STATUS_TONE: Record<string, Tone> = {
  todo: "neutral",
  in_progress: "info",
  in_review: "caution",
  done: "positive",
  cancelled: "neutral",
  closed: "neutral",
};

/**
 * What a status is called, preferring the PROJECT's own label.
 *
 * A company may rename what it calls each of the six, and a screen that
 * rendered the slug would be showing a word the team does not use — while a
 * screen that rendered only the project's would have nothing to say on a board
 * that spans projects. So: the project's label, then the shipped one, then the
 * slug itself, which is what a status this build has never heard of renders as
 * rather than vanishing.
 */
export function statusLabel(status: string, defs?: WorkStatusDef[]): string {
  const declared = defs?.find((d) => d.status === status);
  if (declared?.label) return declared.label;
  return STATUSES.find((s) => s.value === status)?.label ?? humanize(status);
}

export const PRIORITIES = ["none", "low", "normal", "high", "urgent"] as const;

/**
 * The icon a task type is drawn with.
 *
 * AN ICON RATHER THAN A HUE, which is the product's own rule: a type is
 * identity, and identity is carried by a name, a mark and a position. Seven
 * slugs ship with the engine; a company's own type falls through to the
 * neutral box rather than to a generated anything.
 */
export const TYPE_ICON: Record<string, MarkName> = {
  task: "check_circle",
  bug: "bug_report",
  epic: "bolt",
  story: "book_2",
  spike: "explore",
  chore: "build",
  milestone: "flag",
};

export function typeIcon(slug: string | undefined): MarkName {
  return TYPE_ICON[slug ?? ""] ?? "package_2";
}

/** What a type is called, preferring the company's own declaration. */
export function typeName(slug: string | undefined, types?: WorkTypeDef[]): string {
  if (!slug) return "";
  return types?.find((t) => t.slug === slug)?.name ?? humanize(slug);
}

/**
 * The engine's twenty-seven change kinds, each with the mark it is drawn as and
 * the phrase a person reads.
 *
 * A MARK RATHER THAN A HUE, which is this file's own rule for a task type a few
 * declarations above: a kind is identity, and identity is carried by a name, a
 * mark and a position. The item's history drew ONE mark on every row, so a
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
 */
export const CHANGES: { kind: string; mark: MarkName; phrase: string }[] = [
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
];

const BY_KIND = new Map(CHANGES.map((change) => [change.kind, change]));

/**
 * The mark a change of this kind is drawn as.
 *
 * `difference` — "something changed", claimed by none of the twenty-seven — for a
 * kind a NEWER PEER wrote. The event envelope evolves additive-only, so a
 * rolling upgrade puts kinds this build has never heard of on the wire, and a
 * row that drew nothing for one would read as a rendering fault.
 */
export function changeMark(kind: string): MarkName {
  return BY_KIND.get(kind)?.mark ?? "difference";
}

/** What a change of this kind is called, after the actor's name. */
export function changePhrase(kind: string): string {
  // The raw kind for one from a newer peer — lower-cased and unpunctuated rather
  // than `humanize`d, because this lands mid-sentence after a name.
  return BY_KIND.get(kind)?.phrase ?? kind.replaceAll("_", " ");
}

/**
 * An estimate as a person reads it.
 *
 * A MARKED ABSENCE FOR AN ABSENT ONE, never "0m": zero is a measurement and an
 * unestimated task is one nobody has sized, and those are different facts.
 */
export function fmtMinutes(minutes: number | undefined): string {
  if (!minutes) return EMPTY_VALUE;
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  if (hours === 0) return `${rest}m`;
  if (rest === 0) return `${hours}h`;
  return `${hours}h ${rest}m`;
}

// ---------------------------------------------------------------------------
// Grouping
// ---------------------------------------------------------------------------

export interface LabelContext {
  statuses?: WorkStatusDef[];
  types?: WorkTypeDef[];
  tags?: WorkProjectTag[];
  seatName?: (handle: string) => string;
  /** The company's own fields, so a chip over one can say what it is called. */
  fields?: WorkFieldDef[];
  /**
   * What a UNIT key is called, where the caller can say.
   *
   * The one axis a client cannot name for itself: a row holds the unit's KEY,
   * which is its `id` on a company that set one, and the anonymous org
   * projection carries no ids — so the engine's own column `label` is the only
   * name for it, and a caller holding the answer passes it down here. Absent,
   * the key stands, which is what the address holds and what a filter takes.
   */
  unitName?: (key: string) => string;
  /**
   * WHO IS READING, as the seat handle a wake would have been delivered to.
   *
   * A change record is written ONCE and read by everybody, so a sentence the
   * engine addressed to the seat it woke — "…of your priorities" — is second
   * person to whoever happens to open the log. Only a surface knows who that
   * is, which is the same division `deltaValue` states for the reader's zone:
   * the state log's N nodes write one identical row, and anything that depends
   * on who is looking at it belongs here. Empty is the honest default and means
   * "nobody in particular", which renders the owner's name rather than "you".
   */
  viewer?: string;
  /**
   * What a task id is CALLED, for the fields whose value is another task.
   *
   * A relation delta carries the other task's id rather than its key, and for
   * the same reason `deltaValue` renders an instant rather than a day: the
   * record is the state log's, written identically by N nodes, and a key
   * belongs to the other task's own row — so a node that had not applied that
   * task would store a different string, for ever, in a table nothing repairs.
   * The answer resolves what it can (`WorkActivityAnswer.keys`) and this is how
   * it reaches the sentence.
   */
  taskKey?: (id: string) => string;
}

/**
 * What a board column is headed, per axis.
 *
 * THE EMPTY KEY IS A REAL COLUMN on every axis — "nobody is assigned" is a
 * question a board answers rather than a row it hides — and it is the one the
 * server cannot label, because the label depends on what the axis MEANS: an
 * empty assignee is "Unassigned" and an empty tag is "Untagged", and rendering
 * either as a blank heading leaves a column of work nobody can name.
 */
export function groupLabel(axis: string, group: WorkGroup, ctx: LabelContext = {}): string {
  // THE SERVER'S OWN WORD WINS, where it gave one — it is the only party that
  // can label a key this build has never heard of.
  if (group.label) return group.label;
  return axisLabel(axis, group.key, ctx);
}

/**
 * ONE VALUE OF ONE AXIS, in the company's own words.
 *
 * Split out of [groupLabel] because two surfaces now ask it and only one of
 * them holds a `WorkGroup`: a column head has the group the answer returned,
 * and a filter chip has nothing but the key out of the URL. Spelled a second
 * time for the chip, a board narrowed to one column would have been headed
 * "Ada Okonkwo" and chipped `ada-okonkwo` on the same screen.
 */
export function axisLabel(axis: string, key: string, ctx: LabelContext = {}): string {
  switch (axis) {
    case "status":
      return key ? statusLabel(key, ctx.statuses) : "No status";
    case "status_group":
      return key ? humanize(key) : "No group";
    case "assignee":
      return key ? (ctx.seatName?.(key) ?? key) : "Unassigned";
    case "priority":
      return key ? humanize(key) : "No priority";
    case "type":
      return key ? typeName(key, ctx.types) : "No type";
    case "tag":
      return key ? (ctx.tags?.find((t) => t.slug === key)?.label ?? key) : "Untagged";
    case "parent":
      return key || "No parent";
    case "project":
      return key || "No project";
    case "unit":
    case "routing_unit":
      // THE NAME WHERE THE CALLER HAS ONE, and the key where it does not.
      //
      // A unit key is its `id` on a company that set one — a word chosen so
      // that a rename moves nothing, and therefore a word nobody reads. Only
      // the ENGINE can turn one into a team's name: a unit's `id:` is guarded,
      // so the anonymous org projection this client reads carries names alone
      // and a key out of a URL resolves against nothing here. What the engine
      // does give is a column's own `label`, which [groupLabel] takes before
      // this is reached and which a caller holding the answer can pass down —
      // so a chip and the heading it was cut from say one word.
      return key ? ctx.unitName?.(key) || key : "No unit";
    case "due:bucket":
      // THE ENGINE NAMES THESE and a column head takes that name — the answer
      // carries a `label` per band, so [groupLabel] returns before it reaches
      // here. What reaches here is a CHIP, which holds the key out of the URL
      // and no answer, and the words are the engine's own list: copied here
      // they would be a second declaration nothing holds against `dueBands`,
      // which is the drift `clientsource` exists to catch. Humanised, the key
      // IS the word — `this_week` reads "This week" — so the chip and the
      // heading it came from say the same thing without a second table.
      return key ? humanize(key) : "No due date";
    default:
      return key || EMPTY_VALUE;
  }
}

/**
 * The groups a LIST draws: only the bands that hold something.
 *
 * A BOARD DRAWS EVERY LANE the scope admits — the engine mints the empty ones
 * on a closed axis, so a board is the shape of the process rather than of this
 * week's rows — and a list does not: a band is a heading between runs of
 * rows, and a heading over nothing is a rule across the page separating
 * nothing from nothing. Two drawings of one answer, which is what a shape is;
 * the fetch is the same, as the rule that a shape never fetches differently
 * requires.
 */
export function bandsOf(groups: WorkGroup[]): WorkGroup[] {
  return groups.filter((group) => group.count > 0 || (group.subgroups?.length ?? 0) > 0);
}

/**
 * What an ARRANGEMENT control writes when the reader chooses none of it.
 *
 * THE EMPTY STRING MEANS "INHERIT", not "off", and the two are different
 * answers wherever a saved view supplies a default. `buildItemsParams` resolves
 * an arrangement as "the reader's, or the view's" — so a control writing `""`
 * dropped the key and handed the question straight back to the view: a view
 * grouping by assignee could not be ungrouped at all, because every attempt
 * cleared the override and re-applied the view's own. The scope segment's
 * third value has a name for exactly this reason; so does this.
 *
 * It never reaches the wire. `none` is not an axis or a sort key the engine
 * has, and [buildItemsParams] resolves it to the absence of the key rather
 * than sending it.
 */
export const EXPLICIT_NONE = "none";

/**
 * What an arrangement is actually set to: the reader's choice, the view's, or
 * nothing.
 *
 * ONE RESOLVER for the grouping, the second grouping and the order, because
 * all three are the same three-way question and a spelling per key is how one
 * of them came to answer it differently. The PICKERS read this too, so the
 * control shows what the query was sent with rather than what the URL happens
 * to hold — which is the other half of the same defect: a saved view's
 * grouping was in force while its picker sat on "No grouping".
 */
export function effectiveArrangement(asked: string, inherited: unknown): string {
  if (asked === EXPLICIT_NONE) return "";
  if (asked) return asked;
  return typeof inherited === "string" ? inherited : "";
}

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
export const GROUP_AXES: { value: string; label: string; workspace?: boolean }[] = [
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

/**
 * The axes the Display menu's Group by offers, for one shape at one scope.
 *
 * A BOARD IS ALWAYS GROUPED — it is what a board is — and its default axis is
 * status, so on a board there is no "No grouping" row and "Status" is the
 * axis's own entry. The menu used to list both: a `""` row LABELLED "Status"
 * ahead of the `status` axis, two rows reading the same word with different
 * URL effects, one of which marked the control active and one of which did
 * not. Every other shape can be ungrouped, so it leads with that.
 */
export function groupAxisOptions(
  shape: Shape,
  workspace: boolean,
): { value: string; label: string }[] {
  const axes = GROUP_AXES.filter((a) => !a.workspace || workspace).map(({ value, label }) => ({
    value,
    label,
  }));
  return shape === "board" ? axes : [{ value: "", label: "No grouping" }, ...axes];
}

/**
 * The second axis's options: never the first, which the engine refuses because
 * every row would be alone in its own band.
 */
export function secondAxisOptions(
  first: string,
  workspace: boolean,
): { value: string; label: string }[] {
  return [
    { value: "", label: "No second grouping" },
    ...groupAxisOptions("board", workspace).filter((a) => a.value !== first),
  ];
}

/** What an axis is CALLED, or "" for one this build does not offer. */
export function axisName(axis: string): string {
  return GROUP_AXES.find((a) => a.value === axis)?.label ?? "";
}

/** The orderings a list may ask for. */
export const SORTS: { value: string; label: string }[] = [
  { value: "rank", label: "Manual order" },
  { value: "-updated", label: "Recently updated" },
  { value: "due", label: "Due soonest" },
  { value: "-priority", label: "Most important" },
  { value: "-created", label: "Newest" },
  { value: "title", label: "Title" },
];

// ---------------------------------------------------------------------------
// Rendering a change
// ---------------------------------------------------------------------------

/**
 * One commit in a sentence, for the activity feed.
 *
 * FROM THE DELTAS where there are any, because "todo → in_progress" is what
 * every reader of a change wants and the kind alone does not say it. The
 * excerpt is the fallback, and the kind is the last resort — a row with
 * neither is still a real commit, and rendering it blank would make it look
 * like a rendering bug.
 */
export function describeChange(record: WorkActivityRecord, ctx: LabelContext): string {
  const moved = deltaSentence(record.fields, ctx);
  if (moved) return moved;
  // THE PROSE THE BODY RENDERS TO, not its source. The excerpt is a cut of a
  // comment or a description, both markdown by contract, so an unflattened one
  // printed `## Understanding the work` with the hashes in it.
  //
  // RE-ADDRESSED, because a cross-item log is read by everybody and an excerpt
  // was written for one recipient. See [readdress].
  if (record.excerpt) return readdress(plainText(record.excerpt), record, ctx);
  return record.kind.replaceAll("_", " ");
}

/**
 * A sentence the engine addressed to ONE seat, re-addressed to whoever is
 * reading it.
 *
 * `tracker.Writer.prioritisedWake` writes "<actor> put ENG-1 at position 1 of
 * your priorities" — a notification CARD's text, correct for the seat the wake
 * was delivered to and second person to everybody else. The company-wide log
 * draws the same record for a founder, a lead and every other agent, so there
 * it says "your" to a reader whose queue it is not.
 *
 * ONLY A SURFACE CAN FIX THIS, which is the division `deltaValue` states for
 * the reader's zone one function below: the record is the state log's, written
 * identically by N nodes, so it cannot carry a rendering that depends on who
 * opens it. What the record DOES carry is whose record it is — a person
 * subject's id is the handle itself (`tracker.PersonSubject`) — so the owner is
 * always nameable, and second person survives exactly when the reader is that
 * owner.
 *
 * SCOPED TO A PERSON SUBJECT, because that is the only kind whose records are
 * addressed to somebody: a task's excerpt is a comment body or a description
 * and its "you" belongs to whoever wrote it.
 */
function readdress(text: string, record: WorkActivityRecord, ctx: LabelContext): string {
  if (record.subject_kind !== "person" || !text) return text;
  const owner = record.subject_id;
  // THE HANDLE IS THE ONLY IDENTITY THAT CAN MATCH HERE, and the other one is
  // named rather than compared. A viewer has two — `/viewer` returns the
  // operator id a token maps to AND the seat handle bound to it
  // (`lib/viewer.ts`) — but a person subject's id is the SEAT HANDLE
  // (`tracker.PersonSubject`), so a comparison against the operator id could
  // never be true and would be a branch with no reachable case. An operator
  // with no seat bound to them owns no queue for this to be about.
  if (!owner || owner === ctx.viewer) return text;
  const name = ctx.seatName?.(owner) ?? owner;
  // THE POSSESSIVE FIRST, so "your priorities" does not become "<name> s
  // priorities" by way of the bare pronoun. Case-insensitive because the
  // engine's sentences are prose and a rule that only matched one casing would
  // be one that silently stopped matching.
  return text.replace(/\byour\b/gi, `${name}'s`).replace(/\byou\b/gi, name);
}

/**
 * One entry of a task's own history, in a sentence.
 *
 * A DIFFERENT SHAPE FROM THE FEED'S, and that is not duplication: a history
 * entry's `fields` arrives typed `Record<string, unknown>`, so this renderer
 * proves the shape rather than assuming it — one that assumed wrong printed
 * `[object Object]`.
 *
 * THE BARE-VALUE ARM IS DEFENSIVE, NOT A SHAPE THE ENGINE WRITES, and the
 * comment that stood here said otherwise: it claimed the applier writes "the
 * notification's own fields" as plain scalars for a comment, a mention or an
 * ask. It does not, and never did. `fields_json` is `jsonOf` of a
 * `map[string]Delta` on both of its two paths (`internal/tracker/apply_history.go`),
 * and `Delta`'s `From`/`To` carry no `omitempty` — so every value this engine
 * has ever written to that column has BOTH keys and takes the pair arm. The
 * arm below is what stops a payload this build cannot type-check from
 * rendering as `[object Object]`, which is a different and smaller claim.
 *
 * THE EXCERPT IS DELIBERATELY NOT A RUNG. It used to be the one below the
 * deltas, and for the whole comment family the excerpt IS the comment body:
 * `tracker.Wake.excerpt` copies it verbatim, cut to six hundred bytes. So the
 * History tab printed the Thread tab back, one clipped line per comment, and the
 * row never said what the change WAS. What the item's history is for is what
 * MOVED; what was said is one click away on the tab beside it, whole and
 * threaded. The excerpt is still rendered where it is not a copy of something
 * already on the screen — the trash band reads `record.excerpt` for a purge line
 * no row survives to carry, and the Woke tab's routing pane shows what each
 * recipient was actually told.
 *
 * So the ladder is two rungs: what moved, else what happened. The KIND's own
 * mark is drawn beside it, which is what makes the two distinguishable at a
 * glance rather than only on a careful read.
 */
export function describeHistory(entry: WorkChange, ctx: LabelContext): string {
  return deltaSentence(entry.fields, ctx) || changePhrase(entry.kind);
}

/**
 * EVERY FIELD A RECORD SAYS IT MOVED, as one sentence — and "" where it moved
 * none.
 *
 * ONE FUNCTION FOR BOTH SURFACES, because the feed and an item's own history
 * are the same claim about the same column: a delta worded two ways on two
 * screens is the drift this module exists to end, and it had already started —
 * the feed read `fields` as a from/to map and the history read it as either
 * shape, so the two disagreed about what a comment's `mentions` said. They
 * differ on ONE rung and only one, the excerpt, which each states for itself.
 *
 * GENERIC OVER THE KIND, deliberately, and there is no per-kind sentence
 * anywhere below. `fields_json` is written by two producers — the applier's own
 * document comparison, and the notification's fields where no comparison was
 * possible — and neither tags its entries with what they are about, so a
 * renderer that switched on the kind would be guessing at the shape rather than
 * reading it. Which kinds carry fields at all is the ENGINE's answer and it
 * grows: a `relations`, `project_updated`, `view_saved` or `person_updated`
 * record that starts carrying deltas is rendered by this function on the day it
 * does, with nothing here to change.
 *
 * TWO SHAPES, because a history entry's `fields` is sometimes a from/to pair
 * and sometimes the state the change produced — the applier writes the deltas
 * where it can compare two documents and the notification's own fields where it
 * cannot (a comment, a mention, an ask). A renderer that assumed one printed
 * `[object Object]` on the other.
 */
function deltaSentence(fields: Record<string, unknown> | undefined, ctx: LabelContext): string {
  const said: string[] = [];
  for (const [field, raw] of Object.entries(fields ?? {})) {
    const delta = raw as { from?: unknown; to?: unknown } | null;
    if (delta && typeof delta === "object" && ("from" in delta || "to" in delta)) {
      said.push(deltaClause(field, scalar(delta.from), scalar(delta.to), ctx));
      continue;
    }
    const value = deltaValue(field, scalar(raw), ctx);
    said.push(value ? `${humanize(field)}: ${value}` : humanize(field));
  }
  return said.join(", ");
}

/**
 * ONE FIELD'S VALUE, in the word the rest of the product uses for it.
 *
 * A DELTA IS STORED AS THE LOG'S OWN TEXT. `tracker.TaskDeltas` writes
 * `in_review`, a bare handle and a whole RFC3339 instant; the item's header chip
 * reads "In review", its assignee chip reads the seat's name and its Due row
 * reads "Sep 19, 2026". The history was the ONE surface in this product printing
 * the stored form — six inches under the properties rail printing the other —
 * and a reader comparing the two lines could not tell they were about the same
 * value.
 *
 * WHY THE ENGINE CANNOT DO THIS. The delta is the state log's: N nodes write it
 * identically, so it may not carry a rendering that depends on the company's
 * live vocabulary or on this reader's zone. `internal/tracker/wake.go` says
 * exactly that from the other end — "rendering a day from it belongs to a
 * surface, which knows the zone". This is that surface.
 *
 * AN UNKNOWN FIELD RENDERS ITS VALUE UNTOUCHED, and `humanize` is deliberately
 * not the default: a title and a comment are prose, so "update config.yaml"
 * would come back "Update config yaml".
 */
function deltaValue(field: string, value: string, ctx: LabelContext): string {
  if (!value) return "";
  // THE ENGINE JOINS A LIST WITH ", " BEFORE IT STORES IT (wake.go), so it
  // splits back on the same separator.
  const people = (handles: string) =>
    handles
      .split(", ")
      .map((h) => ctx.seatName?.(h) ?? h)
      .join(", ");
  switch (field) {
    case "status":
      return statusLabel(value, ctx.statuses);
    case "type":
      return typeName(value, ctx.types);
    // A FIELD WHOSE VALUE IS PEOPLE, which travels as HANDLES: the one a task
    // is assigned to, the one who reported it, and the three sets
    // `tracker.TaskDeltas` sorts before it stores them — the watchers, the
    // handles that muted the task, and its collaborators. A handle nobody can
    // resolve renders as the handle, which is what `seatName` already does:
    // a seat the chart no longer holds still moved this field.
    case "assignee":
    case "reporter":
    case "watchers":
    case "muted":
    case "collaborators":
      return people(value);
    case "tags":
      return value
        .split(", ")
        .map((slug) => ctx.tags?.find((t) => t.slug === slug)?.label ?? slug)
        .join(", ");
    // [fmtDate], which is what the Plan group of the rail renders Start and Due
    // with. Not [fmtDateCompact]: dropping the year is for a COLUMN of dates,
    // where the one date not in this year is what has to stand out.
    case "due":
    case "start":
      return fmtDate(value);
    // THE STORED FORM IS `90m` (minutesText) and the rail reads "1h 30m". A form
    // this build does not recognise passes through rather than being guessed at.
    case "estimate":
      return /^\d+m$/.test(value) ? fmtMinutes(Number(value.slice(0, -1))) : value;
    // A FIELD WHOSE VALUE IS OTHER TASKS, which travels as their IDS: three of
    // `tracker.RelationKinds`, the `blocking` mirror, the ordered queue a
    // person record carries, the parent a re-parent moves a task between, and
    // the root a cascade removed it with. The last two are SCALARS rather than
    // lists and take the same arm, because one id splits into one member. NOT
    // `page`, which is the fourth relation kind and names a wiki page —
    // resolving it here would be claiming a page is a task, and the answer
    // deliberately leaves those ids out of its map.
    //
    // AN ID THE ANSWER DID NOT RESOLVE RENDERS AS THE ID. The map omits what
    // the answering node holds no row for — a record naming a counterparty it
    // has not applied, or anything past the answer's own cap — and a value
    // nobody can explain is still a value somebody set, where a blank reads as
    // a task with no name.
    case "waiting_on":
    case "linked":
    case "duplicates":
    case "blocking":
    case "priorities":
    case "parent":
    case "removed_with":
      return value
        .split(", ")
        .map((id) => ctx.taskKey?.(id) || id)
        .join(", ");
    default:
      return value;
  }
}

/**
 * One field's move, as one clause.
 *
 * AN EMPTY SIDE IS AN EM DASH on either end: "Assignee:  → Ada" reads as a
 * rendering bug where "Assignee: — → Ada" reads as an assignment.
 *
 * AND A DELTA NEVER RENDERS AS NO CHANGE. `tracker.TaskDeltas` compares the
 * WHOLE instant precisely so that pulling a due time from 09:00 to 17:00 is a
 * change at all; rendered as a day, both sides of that move print "Sep 19, 2026"
 * and the line claims a field moved to where it already was — the exact failure
 * that comment was written against, handed back from the rendering end. So a
 * pair that collapses onto one string is promoted: a date to its instant,
 * anything else to what the engine stored, which differs by construction because
 * the engine only records a delta when it does.
 */
function deltaClause(field: string, from: string, to: string, ctx: LabelContext): string {
  let a = deltaValue(field, from, ctx);
  let b = deltaValue(field, to, ctx);
  if (from !== to && a === b) {
    if (field === "due" || field === "start") {
      a = from ? fmtDateTime(from) : "";
      b = to ? fmtDateTime(to) : "";
    } else {
      a = from;
      b = to;
    }
  }
  return `${humanize(field)}: ${a || EMPTY_VALUE} → ${b || EMPTY_VALUE}`;
}

/** One value of a snapshot as text. Objects and arrays are rare and shallow. */
function scalar(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (Array.isArray(value)) return value.map(scalar).filter(Boolean).join(", ");
  if (typeof value === "object") return JSON.stringify(value);
  return String(value);
}

/**
 * A custom field's value, in the word a person reads.
 *
 * THE OPTION'S NAME RATHER THAN ITS ID. A choice field stores the option's id
 * — which is what lets a company rename an option without orphaning every task
 * that chose it — so a panel that rendered the stored value would print a uuid
 * under a field heading. The id is the fallback for an option the declaration
 * no longer carries, because a value nobody can explain is still a value
 * somebody set.
 */
export function fieldValueText(
  field: WorkFieldValue,
  defs: Map<string, WorkFieldDef>,
  seatName: (handle: string) => string = (h) => h,
): string {
  const value = field.value;
  if (value === null || value === undefined || value === "") return EMPTY_VALUE;
  const def = defs.get(field.id);
  const options = def?.config?.options ?? [];
  const named = (id: unknown) => options.find((o) => o.id === id)?.name ?? String(id);

  switch (field.type) {
    case "checkbox":
      return value === true ? "Yes" : "No";
    case "date":
      return def?.config?.time ? fmtDateTime(String(value)) : fmtDate(String(value));
    case "number":
    case "progress":
    case "rollup": {
      const unit = def?.config?.unit ? ` ${def.config.unit}` : "";
      return `${value}${unit}`;
    }
    case "dropdown":
      return named(value);
    case "labels":
      return Array.isArray(value) ? value.map(named).join(", ") : named(value);
    case "people":
      return Array.isArray(value)
        ? value.map((h) => seatName(String(h))).join(", ")
        : seatName(String(value));
    case "relationship":
      return Array.isArray(value) ? value.map(String).join(", ") : String(value);
    default:
      return typeof value === "object" ? JSON.stringify(value) : String(value);
  }
}

/** The one-word state a field value is in, or empty for an ordinary one. */
export function fieldValueState(field: WorkFieldValue): string {
  if (field.undeclared) return "undeclared";
  if (field.foreign) return "from another tracker";
  if (field.hidden) return "archived";
  return "";
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

/**
 * The renderings, which are the WIRE's — [WorkViewShape], where the gate that
 * holds them against the engine's closed set reads them.
 *
 * An alias rather than a second union: a view's `type` IS the shape, so two
 * spellings of one closed set would be two lists that can disagree, in one
 * build, about which tab renders anything.
 */
export type Shape = WorkViewShape;

/**
 * WHAT A CONTAINER OPENS ON when nothing else decides it.
 *
 * THE LIST, BECAUSE A BOARD'S INFORMATION IS THE COMPARISON ACROSS ITS LANES.
 * That makes it the worst shape at low N and the best at high N: four lanes
 * holding one card between them say nothing a lane could not say alone, and
 * the one card is a 292px object in a 1500px field. A list degrades to one
 * full-width row, which is still a list — the same drawing at one item and at
 * four hundred. So the landing shape is the one that never stops working, and
 * the board is one press away in the Display menu, named by what it is for.
 *
 * NOT CONDITIONAL ON HOW MUCH WORK EXISTS. A landing screen whose shape
 * changes as a company fills up is a screen nobody can learn, and the first
 * item somebody files would silently redraw the page.
 *
 * AND IT IS THE CLIENT'S FALLBACK, not a builtin marked `default` in the
 * engine: one view row may carry `default` and the applier settles that in the
 * same transaction as the write, so a builtin claiming it would collide with
 * whatever a company saved. This is what holds when nothing claims it.
 */
export const LANDING_SHAPE: Shape = "list";

/** The shape a view is drawn in. */
export function shapeOf(viewKey: string, views: WorkView[]): Shape {
  const view = views.find((v) => v.key === viewKey);
  if (view) return view.type;
  // A KEY NOTHING RESOLVES DRAWS [LANDING_SHAPE] rather than nothing: a strip
  // that has not arrived yet is the ordinary state of the first paint, and a
  // body that waited for it would flash empty on every navigation.
  return LANDING_SHAPE;
}

/**
 * The tab a container lands on.
 *
 * ONE ROW MAY CARRY `default` and the applier settles that in the same
 * transaction as the write, so there is never a second claim to fall back
 * from. Absent, the key of the shape every container has without anybody
 * saving one — see [LANDING_SHAPE] for why that is the list.
 */
export function defaultView(views: WorkView[]): string {
  return views.find((v) => v.default)?.key ?? LANDING_SHAPE;
}

/** A view's saved query, or an empty set of defaults. */
export function viewParams(viewKey: string, views: WorkView[]): Record<string, string> {
  return views.find((v) => v.key === viewKey)?.params ?? {};
}

// ---------------------------------------------------------------------------
// The query a screen builds
// ---------------------------------------------------------------------------

export interface TrackerFilters {
  q: string;
  status: string;
  type: string;
  priority: string;
  assignee: string;
  /** One of the PROJECT's own labels — the set is a project's, never the
   *  company's, so this narrows only where that set is known. */
  tag: string;
  /**
   * The TEAM the work is filed into, by the unit's id or by its name.
   *
   * URL-ONLY, and deliberately: it arrives from an item's own "Filed into"
   * line rather than from a control, because naming a team from a picker
   * would need a list of unit KEYS, and the anonymous org projection this
   * client reads carries names alone (a unit's `id:` is guarded). It still
   * carries a chip, which is how a reader sees it and takes it off.
   *
   * The engine matches the SET of the unit's spellings, so the one key an
   * item holds reaches every task of that team however it was filed — which
   * a `group=` narrowing on the unit axis cannot do, since a column IS one
   * stored spelling.
   */
  unit: string;
  /** One of [SCOPES]: `open` (the default), `closed` or `all`. */
  scope: string;
  groupBy: string;
  /** The SECOND axis, drawn as bands inside the first — see [buildItemsParams]. */
  groupBy2: string;
  /**
   * WHICH COLUMN THE WHOLE QUERY IS NARROWED TO, and it is THREE-VALUED.
   *
   * `undefined` is the whole board. `""` is the column holding the rows with
   * NO value on this axis — "Unassigned", "Untagged", "No parent" — and
   * anything else is that value. The engine reads the key's PRESENCE
   * (`Params.Has`) for exactly this reason: ITS key for the unset column IS
   * the empty string, on every axis, so "" cannot also mean "no narrowing".
   *
   * This is the same defect `EXPLICIT_NONE` records one field above, with the
   * opposite resolution. An ARRANGEMENT needed a NAME for "off", because its
   * empty string already meant "inherit the view's" and a word is the only way
   * to tell a choice from an absence. A column narrowing needs PRESENCE,
   * because its empty string already means a real column and no word could be
   * spelled that some axis will not one day hold as a value. So the URL carries
   * `group=` with nothing after it, which `URLSearchParams` round-trips, and
   * every reader asks whether the key is there rather than what it says.
   *
   * Spelled as a plain string it was unreachable end to end: the query builder
   * dropped an empty one on the way to the wire, the address writer deleted the
   * key, and the unset column's own "N more →" link loaded the whole board.
   */
  group: string | undefined;
  sort: string;
  blocked: boolean;
  /**
   * WHEN it is due, as the grammar's one `due` key.
   *
   * ONE KEY, NOT TWO. The bar carried an Overdue switch writing `overdue=true`
   * while the grammar has a single `due` parameter that the calendar's own
   * window also spends — so a screen offering both a switch and a date filter
   * would have had two URL keys competing for one wire parameter and a
   * precedence rule between them that nothing could state on screen. Overdue
   * is now what it is on the wire: one value of this filter
   * (`internal/tracker/dates.go` resolves it, carrying the open-status
   * condition with it), beside the engine's own aliases.
   */
  due: string;
  /**
   * The trash, AS A FILTER, because that is what the engine says it is.
   *
   * `internal/tracker/viewsread.go`: "what makes it the trash is `removed=true`
   * … any view carrying that parameter is a trash listing and a client may
   * treat it as one". It was a TAB in the view strip beside five shapes — so a
   * reader could reach removed work only by leaving whatever arrangement they
   * were in, and a trash of one project's bugs was not expressible at all.
   */
  removed: boolean;
  /**
   * The company's OWN fields, by `f.<slug>`, exactly as the grammar spells
   * them.
   *
   * A MAP RATHER THAN A KEY EACH, because the set is the company's: a build
   * that named them would offer the fields it shipped knowing about, which is
   * none of them. The screen collects every `f.` key off the address and hands
   * them through — so a saved view's custom-field narrowing, a pasted URL and
   * the Filter menu are one path rather than three.
   */
  fields: Record<string, string>;
}

export const NO_FILTERS: TrackerFilters = {
  q: "",
  status: "",
  type: "",
  priority: "",
  assignee: "",
  tag: "",
  unit: "",
  scope: "open",
  groupBy: "",
  groupBy2: "",
  // ABSENT, which is the whole board — see [TrackerFilters.group] for why the
  // empty string is a column rather than the lack of one.
  group: undefined,
  sort: "",
  blocked: false,
  due: "",
  removed: false,
  fields: {},
};

/**
 * Whether anything is narrowing the rows, so a Clear control can appear.
 *
 * THE ARRANGEMENT IS NOT A NARROWING. `groupBy`, `groupBy2` and `sort` decide
 * how the same answer is DRAWN — they are the Display menu's, not the filter
 * chips' — and counting them here made Clear appear over a board nobody had
 * filtered and then, pressed, flatten the arrangement the reader had chosen
 * while removing nothing. `group` stays, because narrowing a board to one
 * column narrows the whole query, totals included.
 */
export function anyFilter(f: TrackerFilters): boolean {
  return Boolean(
    f.q ||
    f.status ||
    f.type ||
    f.priority ||
    f.assignee ||
    f.tag ||
    f.unit ||
    // PRESENCE, NEVER TRUTH: `group=` with nothing after it is the unset
    // column, which narrows the query exactly as a named one does.
    f.group !== undefined ||
    f.blocked ||
    f.due ||
    f.removed ||
    Object.keys(f.fields).length > 0 ||
    f.scope !== "open",
  );
}

/**
 * WHICH CONTROL OWNS A KEY. A filter is a chip; an arrangement is a menu.
 *
 * The rule is the design's, stated once in
 * `docs/reference/dashboard-design.md`, and it is the boundary these screens
 * lost first: a bar of eleven controls, five of them pickers drawn whether or
 * not they were set, so the two that were narrowing looked exactly like the
 * nine that were not. What a key IS decides where it is set and where it is
 * taken off — a narrowing is added from the **Filter** menu and removed from
 * its own chip, and a drawing is chosen in the **Display** menu, whose button
 * says what is on.
 *
 * A key that is BOTH is two controls for one fact, and they disagree the
 * first time either writes; a key that is NEITHER is a state a reader can
 * reach only by editing the address, with nothing on screen to say it is on.
 * Three keys are deliberately neither, and each carries the reason below.
 */
export type ControlHome = "chip" | "menu" | "bar" | "strip" | "shape";

/**
 * Every key the work list carries on the address, and what draws it.
 *
 * KEYED BY THE URL SPELLING rather than by the [TrackerFilters] field that
 * holds it, because the address is what this states: `groupBy` is `group_by`
 * there, and a custom field is not one key at all. The two FAMILIES end in a
 * dot and stand for every key beneath them — the company's own fields, whose
 * set is the company's, and the per-shape column arrangements, whose set is
 * the grid shapes'.
 *
 * `routes/work/toolbar/grammar.test.tsx` READS THIS and holds it in both
 * directions: every key the screen puts on the address has a home here, and
 * every home here is a key the screen actually writes — so a key added later
 * with nowhere to live fails rather than shipping invisible.
 */
export const URL_HOMES: Record<string, ControlHome> = {
  // THE NARROWINGS. Each is a chip under the bar, and taking the chip off
  // clears exactly this key. Most are offered by the Filter menu; `q` is the
  // substring mark beside it and `unit` arrives from an item's own "Filed
  // into" line, which changes where a filter is SET and not what it is.
  q: "chip",
  status: "chip",
  type: "chip",
  priority: "chip",
  assignee: "chip",
  tag: "chip",
  unit: "chip",
  due: "chip",
  blocked: "chip",
  removed: "chip",
  // NARROWING A BOARD TO ONE COLUMN NARROWS THE WHOLE QUERY, totals included
  // — see [TrackerFilters.group], which is also why its chip is drawn on the
  // key's PRESENCE rather than on its value.
  group: "chip",
  /** The company's own fields: one key per declared field, `f.<slug>`. */
  "f.": "chip",

  // THE ARRANGEMENT. None of these narrows anything, which is exactly why
  // they are one menu rather than chips: they decide how the same answer is
  // DRAWN.
  shape: "menu",
  group_by: "menu",
  group_by2: "menu",
  sort: "menu",
  /** The chosen columns, per grid shape: `cols.<shape>` — see `shapes/Grid.tsx`. */
  "cols.": "menu",

  // AND THE THREE THAT ARE NEITHER, each for a reason the design states.
  //
  // THE SCOPE is always set to something: as a chip it would either be
  // permanently present, which is not a chip, or absent on its default, which
  // hides the one segment deciding whether finished work is on screen at all.
  // So it stays in the bar as a switch.
  scope: "bar",
  // A SAVED VIEW is a query somebody arranged and put somewhere, so it is a
  // tab in the strip — and a key of its own beside `shape=` precisely so that
  // redrawing a saved board does not throw the saved filters away.
  view: "strip",
  // AND THE CALENDAR'S WINDOW is that shape's own axis, stepped in its own
  // header: a month is not a narrowing somebody added and not another drawing
  // of one answer, it is WHICH answer that shape asks for.
  month: "shape",
};

/**
 * The `work_items` parameters one screen state asks for.
 *
 * # A view is a set of DEFAULTS and every explicit key overrides it
 *
 * Which is what makes picking a different assignee on a saved board give you
 * that board with one key changed, rather than a board that silently stops
 * being the saved one. Storing the view's id and letting it win instead would
 * make the filter controls lie about what is on screen the moment somebody
 * touched one.
 *
 * # The four shapes ask four different questions of one grammar
 *
 * A board is `group_by` and no cursor — across a set of columns there is no
 * single order to be after. A list is a page with a sort. A calendar is a DATE
 * RANGE and no grouping at all, because its own axis is the month it is
 * drawing and a column inside a day is not a thing. A timeline is a big
 * unpaged page ordered by start: its axis is derived from the rows present, so
 * a second page would redraw the first one's window.
 */
export function buildItemsParams(args: {
  container: string;
  shape: Shape;
  view: Record<string, string>;
  filters: TrackerFilters;
  /** The calendar's own bounds, as `YYYY-MM-DD` — see [gridRange]. */
  range?: { from: string; to: string };
  /**
   * THE HOST SCREEN'S OWN NARROWING, and it is the last word on the key it
   * names.
   *
   * `#/me`'s Assigned tab IS one person's work, so the assignee is not a
   * filter the reader set and can take off — it is what the screen is. Applied
   * after everything else and on every branch, so no saved default, no `f.`
   * key and no hand-edited address can widen the list past the person it is
   * about. It is deliberately NOT routed through [TrackerFilters]: counted
   * there it would make [anyFilter] true on an unfiltered day, and the empty
   * panel would blame a narrowing with no chip to take off.
   */
  lock?: { assignee?: string };
}): Record<string, unknown> {
  const params = itemsParams(args);
  // LAST, ON EVERY BRANCH. Three of them return early — the calendar, the
  // timeline and the grid pair — so a lock written inside would have to be
  // written three times, and the one that was forgotten is the one nobody
  // would see: a list drawing somebody else's work under this person's name.
  if (args.lock?.assignee) params.assignee = args.lock.assignee;
  return params;
}

/** [buildItemsParams] before its lock, which is every other rule it has. */
function itemsParams(args: {
  container: string;
  shape: Shape;
  view: Record<string, string>;
  filters: TrackerFilters;
  range?: { from: string; to: string };
}): Record<string, unknown> {
  const { container, shape, view, filters, range } = args;
  // A VIEW'S PARAMS GO STRAIGHT ONTO THE WIRE, and they can only ever be
  // QUERY keys. `cols`, `cols.list` and `cols.table` are DISPLAY keys this
  // screen keeps in the address, and the engine would refuse a read carrying
  // one — but no view can carry one to begin with, so there is nothing to
  // strip here: `internal/tracker/views.go:274` runs every saved view's params
  // through `ParseQuery` at the SAVE, whose `checkKeys`
  // (`internal/tracker/query.go:449`) refuses any key outside `QueryKeys`
  // (`query.go:429`) that is not an `f.<ref>` custom field, and `cols` is in
  // neither. `WriteView` (`views.go:66`) is the only write path and
  // `save_work_view` reaches it (`internal/agent/builtin/workviews.go:216`).
  // The builtin views are this package's own two params, held parseable by
  // `TestEveryImplicitViewsQueryParses`, and both spellings of the display key
  // are held refused by `TestAViewThatCouldNotBeRunIsRefusedAtTheSave`. A
  // client-side filter here would be a second, weaker copy of that refusal —
  // and the weaker one, since it would run after the engine had already
  // decided.
  const params: Record<string, unknown> = { ...view, container };

  const set = (key: string, value: string) => {
    if (value) params[key] = value;
  };
  // THE COLUMN NARROWING IS SENT ON ITS PRESENCE, never on its truth: `""` is
  // the unset column and the engine reads the key with `Params.Has`, so an
  // empty value has to reach the wire as `group: ""` rather than be dropped by
  // the writer above. Absent, the key is left exactly as it was — which is how
  // a saved view's own `group` keeps supplying the default nobody overrode.
  const setGroup = () => {
    if (filters.group !== undefined) params.group = filters.group;
  };
  set("q", filters.q);
  set("status", filters.status);
  set("type", filters.type);
  set("priority", filters.priority);
  set("assignee", filters.assignee);
  set("tag", filters.tag);
  set("unit", filters.unit);

  // OPEN AND CLOSED ARE STATUS GROUPS, not a boolean: the four groups are
  // what every rule in the tracker is written at, and `done` and `closed` are
  // two of them rather than one negation. The third segment deletes the key,
  // including one a view brought with it.
  //
  // AND THE TWO SEGMENTS THAT INCLUDE FINISHED WORK NEED `show_closed`, because
  // the group alone cannot widen the answer. `internal/tracker/read.go` ANDs an
  // unconditional `t.status_group IN ('not_started','active')` unless this key
  // is set, so sending `done,closed` without it asks for the intersection of
  // two disjoint halves — the Closed segment returned nothing at all, on every
  // company, and All returned exactly what Open did. Measured against a seeded
  // company through a running engine: 0 rows and 14 where the answers are 1
  // and 15.
  //
  // IT IS A FLOOR, NOT AN OVERRIDE. A saved view may carry its own
  // `show_closed` — `recent:168h` is "finished in the last week" — and that is
  // a NARROWER window its author chose, inside the same segment. Writing
  // `true` over it turns their view into "everything ever closed" the moment
  // the segment is derived from their own `status_group`, which is a scope
  // nobody picked. So the segment supplies the key only where nothing else
  // did, and never deletes one.
  //
  // Which is also why `open` touches it at all: under `not_started,active`
  // every value of `show_closed` is inert, since each of the three arms in
  // that switch either adds nothing or adds a predicate the group already
  // implies. Measured, all three answer identically.
  if (filters.scope === "open") {
    params.status_group = SCOPE_GROUPS.open;
  } else if (filters.scope === "closed") {
    params.status_group = SCOPE_GROUPS.closed;
    if (!params.show_closed) params.show_closed = "true";
  } else {
    delete params.status_group;
    if (!params.show_closed) params.show_closed = "true";
  }

  if (filters.blocked) params.blocked = true;
  // THE GRAMMAR HAS ONE `due`, and the calendar branch below spends it on its
  // own window — which is why that branch deletes this rather than relying on
  // assignment order, and why the Filter menu does not offer a due filter on
  // that shape at all.
  if (filters.due) params.due = filters.due;

  // A REMOVED TASK IS VERY OFTEN A FINISHED ONE, so this carries `show_closed`
  // exactly as the builtin trash view does — the group predicate is ANDed
  // unconditionally otherwise (`internal/tracker/read.go`), which is what hid
  // every removal of anything already done. As a FLOOR rather than an
  // override, for the reason the scope segment gives above: a view that
  // narrowed the window it widened to keeps its own value.
  if (filters.removed) {
    params.removed = "true";
    if (!params.show_closed) params.show_closed = "true";
  }

  // THE COMPANY'S OWN FIELDS, PASSED THROUGH VERBATIM. The grammar for a
  // custom field is the engine's (`f.<slug>=<op>:<value>`, with the field's own
  // natural comparison for a bare value), and a client that parsed it here
  // would be a second copy of a table the engine refuses against — where a
  // wrong operator is not an error but a clause that matches nothing, which
  // reads as a board with no work on it.
  for (const [key, value] of Object.entries(filters.fields)) {
    if (value) params[key] = value;
    else delete params[key];
  }

  const axis = effectiveArrangement(filters.groupBy, view.group_by);
  // THE SECOND AXIS IS SENT ONLY BESIDE A FIRST, which is the engine's own
  // refusal (`group_by2 was passed without group_by`) rather than a rule of
  // ours — and it is dropped where it EQUALS the first, which the engine also
  // refuses, because every row would then be alone in its own band. The
  // control cannot offer the first axis as the second, so this covers a
  // hand-edited URL rather than a reachable state.
  const axis2 = effectiveArrangement(filters.groupBy2, view.group_by2);
  // AND THE ORDER IS THE SAME THREE-WAY QUESTION. A view's own `sort` is
  // spread into `params` above, so the reader's choice has to overwrite it and
  // their explicit "default order" has to DELETE it — where an empty string
  // deleted the override instead and handed the view's sort back.
  const order = effectiveArrangement(filters.sort, view.sort);
  const setSubAxis = () => {
    if (axis && axis2 && axis2 !== axis) params.group_by2 = axis2;
    else delete params.group_by2;
  };

  if (shape === "board") {
    params.group_by = axis || "status";
    params.group_limit = 50;
    delete params.limit;
    delete params.cursor;
    delete params.group;
    // NO SECOND AXIS ON A BOARD. A board's second axis is a swimlane GRID —
    // cells, a cap that is the product of the two axes, and an overflow link
    // per cell — which is a different drawing rather than a deeper band, and
    // the Display menu does not offer it here. Sent anyway, by a view or by a
    // hand-edited URL, it would come back as `subgroups` this shape draws
    // nothing for: the rows would simply vanish from their columns.
    delete params.group_by2;
  } else if (shape === "calendar") {
    // THE CALENDAR HAS ITS OWN AXIS, so a grouping brought in by a view is
    // dropped rather than sent: a grouped answer replaces the rows with
    // columns, and a month drawn from columns has nothing in its cells.
    delete params.group_by;
    delete params.group_by2;
    delete params.group;
    delete params.group_limit;
    // AND ITS AXIS IS THE `due` KEY ITSELF, which the grammar has exactly one of
    // — so the grid's window and a due filter cannot both be asked for. The
    // window wins, and the filter is therefore not offered on this shape at all
    // (see `FilterMenu`): a chip whose narrowing is overwritten on the way to
    // the wire is how a reader concludes their filter matched everything.
    // Overdue work inside the window is already tinted in place.
    //
    // WRITTEN AS A DROP RATHER THAN LEFT TO THE OVERWRITE two branches above.
    // The old form worked only because `due = "overdue"` happened to be assigned
    // first; it is invisible to a reader of this function and to anyone who
    // reorders it.
    //
    // AND NO WINDOW MEANS NO CALENDAR QUESTION. Without one, `sort=due` over the
    // whole backlog returns the five hundred soonest-due tasks in the company —
    // which the grid cannot draw and the toolbar would count as if the reader
    // had asked for them.
    if (range) params.due = `range:${range.from}..${range.to}`;
    else delete params.due;
    params.limit = 500;
    params.sort = "due";
    return params;
  } else if (shape === "timeline") {
    // THE TIMELINE KEEPS ITS GROUPING, unlike the calendar: a band per
    // assignee or per status is one axis stacked on the other, which is the
    // arrangement the view is for — where a month drawn from columns has
    // nothing in its cells.
    if (axis) {
      params.group_by = axis;
      params.group_limit = 100;
      setGroup();
    } else {
      delete params.group_by;
      delete params.group;
    }
    // A BAND OF BANDS IS NOT A TIMELINE. Its own axis is the date, and a
    // second grouping would nest one band inside another down a shared axis —
    // which is a Gantt swimlane and not what this component draws.
    delete params.group_by2;
    // MORE ROWS THAN A LIST, because a bar is one line where a list row is
    // three or four and the window is derived from the rows present: a
    // timeline paged at a hundred would draw a different axis on every page.
    params.limit = 500;
    // AND IT ARRIVES IN THE AXIS'S OWN ORDER unless the reader asked
    // otherwise, so the bars descend rather than zig-zag. The builtin view
    // carries the same value; this is what holds when a saved view or a
    // filter change drops it — and what an explicit "default order" resolves
    // to here, because a timeline's default IS its date axis.
    params.sort = order || "start";
    return params;
  } else {
    // THE LIST AND THE TABLE ASK THE SAME QUESTION. They are two arrangements
    // of one answer — the same rows, the same grouping, the same page — and
    // the whole difference is where a field is drawn, so a second arm here
    // would be a second copy of one paging rule.
    if (axis) {
      params.group_by = axis;
      params.group_limit = 100;
      setGroup();
    } else {
      delete params.group_by;
      delete params.group;
    }
    // THE ONE SHAPE PAIR A SECOND AXIS IS DRAWN IN: a band inside a band, down
    // one column, which is what nesting means where every row is a line.
    setSubAxis();
    params.limit = 100;
  }

  // THE SORT IS OMITTED where nobody asked for one, so the engine's own
  // default applies — the manual order inside a project and the most recently
  // touched across the company, which are two different right answers and
  // neither is something a client should hard-code. A reader who chose that
  // default over a view's own sort deletes the key the view spread in, which
  // is what [EXPLICIT_NONE] is for.
  if (order) params.sort = order;
  else delete params.sort;
  return params;
}

/**
 * Where a board column's overflow lands: the same rows as a list.
 *
 * `shape`, NEVER `view`. The two are different keys now — a view is the saved
 * QUERY and the shape is how it is drawn — and writing `view=list` here threw
 * away whichever saved view the reader was on, so following "52 more" out of a
 * saved board landed them on the container's default filters with the column
 * narrowing applied to the wrong set.
 */
export function filterPatchForGroup(axis: string, key: string): Record<string, string> {
  // AND THE UNSET COLUMN'S KEY IS `""`, which the patch carries as a value
  // rather than as a deletion — see [TrackerFilters.group]. `patchedHref`
  // deletes on `null` for exactly this: written as "clears the key", the
  // "Unassigned" column's own overflow link loaded the whole board.
  return { shape: "list", group_by: axis, group: key };
}

// ---------------------------------------------------------------------------
// The filter chips
// ---------------------------------------------------------------------------

/**
 * One narrowing the reader applied, as the chip that says so and takes it off.
 *
 * WHY CHIPS AT ALL. The bar carried a control per field — a search box and five
 * selects, each drawn whether or not it was set — so eleven controls stood
 * between the reader and the rows, the two that were ON looked exactly like the
 * nine that were not, and a shape that hid a picker (the calendar has no
 * Group by) moved every control beside it. A chip is drawn only where a filter
 * is applied, so the bar is empty on an unfiltered board and says the whole
 * narrowing in one line on a filtered one.
 *
 * `param` IS THE URL KEY, which is what makes a chip removable without a table
 * of removers beside this one: the screen clears the key the chip names.
 */
export interface FilterChipSpec {
  /** The URL key this chip stands for, and what taking it off clears. */
  param: string;
  /** The field's own name — the whole chip on a switch, which has no value. */
  label: string;
  /** The relation, where there is one to state. */
  verb?: string;
  /** What it was set to, in the company's own words. */
  value?: string;
}

/**
 * Every applied filter, in one order, as the chips a screen draws.
 *
 * PURE, over values, for the reason this file's own preamble gives: a rule that
 * can only be exercised by rendering a screen is a rule nobody re-measures, and
 * every judgement here has a wrong form that reads as a different fact — a chip
 * saying `ada-okonkwo` where the company says Ada Okonkwo, or `in_progress`
 * where the team says Doing.
 *
 * THE SCOPE IS NOT A CHIP. Open / Closed / All is a three-valued switch that is
 * always set to something, drawn in the bar beside these: as a chip it would
 * either be permanently present (a chip that cannot be removed is not a chip)
 * or absent on its default, which hides the one segment that decides whether
 * finished work is on screen at all.
 */
export function filterChips(f: TrackerFilters, ctx: LabelContext = {}): FilterChipSpec[] {
  const out: FilterChipSpec[] = [];
  if (f.q) out.push({ param: "q", label: "Text", verb: "contains", value: f.q });
  if (f.status) {
    out.push({
      param: "status",
      label: "Status",
      verb: "is",
      value: statusLabel(f.status, ctx.statuses),
    });
  }
  if (f.type)
    out.push({ param: "type", label: "Type", verb: "is", value: typeName(f.type, ctx.types) });
  if (f.priority) {
    out.push({ param: "priority", label: "Priority", verb: "is", value: humanize(f.priority) });
  }
  if (f.assignee) {
    out.push({
      param: "assignee",
      label: "Assignee",
      verb: "is",
      // UNASSIGNED IS A VALUE the grammar spells `none`, and it is the one a
      // lead opens a board to ask for — printed raw it reads as a filter that
      // failed to resolve somebody's name.
      value: f.assignee === "none" ? "Unassigned" : (ctx.seatName?.(f.assignee) ?? f.assignee),
    });
  }
  if (f.tag) {
    out.push({
      param: "tag",
      label: "Tag",
      verb: "is",
      value: ctx.tags?.find((t) => t.slug === f.tag)?.label ?? f.tag,
    });
  }
  if (f.unit) {
    out.push({
      param: "unit",
      label: "Unit",
      verb: "is",
      // WHATEVER THE ADDRESS HOLDS, which on a company that gave its units
      // ids is a key rather than a word — see [axisLabel]'s unit arm for why
      // this client cannot turn one into a name on its own, and why the chip
      // says the key rather than inventing one.
      value: axisLabel("unit", f.unit, ctx),
    });
  }
  if (f.due) out.push({ param: "due", label: "Due", verb: "is", value: dueFilterLabel(f.due) });
  if (f.group !== undefined) {
    // THE COLUMN A BOARD WAS NARROWED TO, named by its own axis — the same
    // resolver the column head uses, so the chip and the heading it came from
    // say the same word. Including the UNSET one: `axisLabel` names an empty
    // key per axis ("Unassigned", "Untagged"), which is the whole reason it
    // takes the key rather than the group.
    out.push({
      param: "group",
      label: axisName(f.groupBy) || "Column",
      verb: "is",
      value: axisLabel(f.groupBy, f.group, ctx),
    });
  }
  // THE SWITCHES LAST, because they have no value to read: a chip that is only
  // a field name is a different shape from one that is a sentence, and mixing
  // the two through the row makes the row look ragged rather than ordered.
  if (f.blocked) out.push({ param: "blocked", label: "Blocked" });
  if (f.removed) out.push({ param: "removed", label: "Removed items" });
  // THE COMPANY'S OWN FIELDS, LAST AND IN THE ADDRESS'S OWN ORDER. Named from
  // the catalogue where it has arrived and from the SLUG where it has not —
  // which is a value a reader can still act on, rather than a chip that says
  // nothing while a second read is in flight.
  for (const [param, value] of Object.entries(f.fields)) {
    const slug = param.slice("f.".length);
    const def = ctx.fields?.find((d) => d.slug === slug || d.id === slug);
    out.push({ param, label: def?.name || slug, verb: "is", value: fieldFilterValue(value) });
  }
  return out;
}

/**
 * The due filters a person is offered, which are the ENGINE'S OWN ALIASES.
 *
 * Every value here is one `internal/tracker/dates.go` expands itself
 * (`DateAlias`, plus `overdue`, which carries an open-status condition with
 * it), so the control cannot compose a window the engine resolves differently
 * from the word on the chip. A comparison this list does not offer —
 * `gte:2026-01-01`, `range:sow..eom` — is still legal on the address and in a
 * saved view, and the chip prints it back verbatim rather than re-wording it.
 *
 * THERE IS NO "no due date". The grammar has no `due=null`: `due` compiles to
 * a comparison against a column, and `due_at IS NULL` is not one. Offering it
 * would mean offering a filter that is refused.
 */
export const DUE_FILTERS: { value: string; label: string }[] = [
  { value: "overdue", label: "Overdue" },
  { value: "earlier", label: "Before today" },
  { value: "thisweek", label: "This week" },
  { value: "next7", label: "Next 7 days" },
  { value: "thismonth", label: "This month" },
  { value: "last7", label: "Last 7 days" },
  { value: "lastmonth", label: "Last month" },
];

/** What a due filter reads as, or the value itself for one written by hand. */
export function dueFilterLabel(value: string): string {
  return DUE_FILTERS.find((d) => d.value === value)?.label ?? value;
}

/**
 * What a custom-field filter reads as, WITHOUT parsing the grammar.
 *
 * The engine's spelling is `<op>:<value>` with the field's own natural
 * comparison for a bare value, and the two forms a chip has to separate are
 * `null` / `not_null` — which are questions about the ROW rather than about a
 * value, and read as "nothing" and "anything" printed raw. Everything else is
 * shown as the reader wrote it: a chip that re-worded `range:3..8` would be a
 * second copy of a table the engine refuses against, and one wrong word there
 * is a filter that matches nothing while the chip claims otherwise.
 */
function fieldFilterValue(value: string): string {
  if (value === "null") return "not set";
  if (value === "not_null") return "set";
  return value;
}

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

/**
 * EVERY ROW ON SCREEN, grouped or flat.
 *
 * A grouped answer carries NO flat `items` by construction — `groups` replaces
 * them, because returning both would be the same rows twice — so everything
 * derived from `items` alone reported a fully populated board as empty: the
 * header read 0 shown, 0 in progress, 0 blocked, and the panel at the foot of
 * the screen drew "No work has been filed yet" underneath the board's own
 * columns.
 *
 * The top-level rows are enough and subgroup rows are deliberately NOT added:
 * a subgroup's rows are a slice of its own column's, so folding them in would
 * count the same task twice.
 */
export function shownRows(items: WorkSummary[], groups: WorkGroup[]): WorkSummary[] {
  if (groups.length === 0) return items;
  return groups.flatMap((group) => group.rows);
}

/**
 * The project filter's options: the COMPANY's own listing, falling back to
 * whatever is on the page.
 *
 * The listing is the honest set — a filter built from the rows can only offer
 * the projects that happen to be on them, so a board already narrowed to one
 * offered exactly one choice and no way back. The fallback is not decoration:
 * the listing is a separate poll, and a filter that empties while it is in
 * flight is a control that flickers every time the board is re-read.
 */
export function projectKeys(listed: WorkProjectRow[] | undefined, shown: WorkSummary[]): string[] {
  if (listed && listed.length > 0) return listed.map((p) => p.key);
  const keys = new Set<string>();
  for (const item of shown) keys.add(item.project);
  return [...keys].sort();
}

/** The engine's own count against what is on screen. */
export function totalHint(hint: number, shown: number, capped?: boolean): string {
  if (hint <= 0) return "";
  // THE ENGINE SAYS WHETHER IT COUNTED THAT FAR, and the threshold is not
  // repeated here: comparing the hint against a ceiling of our own was a
  // second copy of the engine's, wrong at exactly one value — a set of
  // exactly ten thousand is EXACT and read as "10000+".
  if (capped) return `of ${hint}+ matching`;
  // SILENT WHEN IT ADDS NOTHING. "15 items of 15 matching" states one
  // number twice; the hint exists to say there is MORE than what is on
  // screen, and when there is not, the row count already said it.
  if (hint <= shown) return "";
  return `of ${hint} matching`;
}

/**
 * WHAT THE COUNT IS A COUNT OF, and the one shape where that is not obvious.
 *
 * Four shapes draw this indicator in the same corner of the same bar, in the
 * same words, over the same question: how many items match the filters you set.
 * The calendar does not. [buildItemsParams] bounds its fetch to the days the
 * grid draws, and `internal/tracker/read.go` compiles that to `due_at IS NOT
 * NULL AND due_at >= ? AND due_at < ?` — so the same filters answered "6 items"
 * beside a list saying "17", with eleven tasks gone and nothing on screen saying
 * where. A number that means two things in one place is worse than no number: a
 * reader compares the two and concludes work disappeared.
 *
 * AND [totalHint] CANNOT COVER IT, which is why this is a second helper rather
 * than a wider one. The engine's `total_hint` is counted over the same predicate
 * as the rows (`countHint`, same `where`, same transaction), so on a windowed
 * answer it EQUALS the row count and `totalHint` correctly falls silent. It says
 * "there is more of what you asked for"; this says "you asked for less than you
 * think".
 *
 * DERIVED FROM THE PARAMS THAT WERE SENT, never from the shape. Which shapes
 * narrow is written in exactly one place, and a second copy of that list here is
 * a copy that stops matching — the day another shape bounds its own axis this
 * sentence goes quietly wrong again, in precisely the way it is being fixed for.
 * It also makes a SAVED view's own `due=range:` window say the same thing on a
 * list, which is true there too.
 *
 * It UNDER-states rather than over-states where it cannot tell: the engine's
 * date aliases (`due=thismonth`) resolve to a range server-side, and that table
 * belongs to `internal/tracker/dates.go` — a copy of it here is the drift
 * `internal/clientsource` exists to catch. A view carrying one gets the plain
 * count, which is vague rather than wrong.
 */
export function countedLabel(shown: number, params: Record<string, unknown>): string {
  const label = plural(shown, "item");
  const due = typeof params.due === "string" ? params.due : "";
  return due.startsWith("range:") ? `${label} due in this window` : label;
}

/**
 * THE END OF A LIST, SAID — and "" where it would be a reassurance.
 *
 * # What it separates
 *
 * A list that has reached its end and a list that was cut off end the same
 * way: rows, then page ground. [totalHint] is deliberately silent once
 * everything matching is on screen, and a page's own cursor is invisible — so
 * the reader of a hundred rows cannot tell whether the hundred-and-first
 * exists. This is the sentence that says it does not.
 *
 * # Why it is not the reassurance [pageNote] refuses
 *
 * `pageNote`'s rule — "a note that always drew would put 'and that is all of
 * them' under every healthy card in the product" — is about a CARD in a
 * column of cards, where the note is one of twenty and nobody reads the
 * twentieth. This is the foot of the page's own subject, drawn once, where the
 * question "is that everything?" is the reason somebody scrolled. It is silent
 * on an empty list (the empty state is the answer there) and on an incomplete
 * one (the count in the bar already says there is more).
 *
 * # It never says what a filter hides
 *
 * The design's own sentence was "Two items match. Widen the filters, or clear
 * them, to see the other six" — and "the other six" is a count over the
 * UNFILTERED set, which no answer carries: `total_hint` is `countHint` over
 * the SAME predicate as the rows (`internal/tracker/read.go`), so it counts
 * what matched and never what was excluded. Stating it would mean a second
 * query at a second instant, printing a difference nobody wrote. So the
 * narrowed form says only that these are the ones that match.
 */
export function endNote(args: {
  shown: number;
  /** The answer's `total_hint`. */
  hint: number;
  /** The answer's `total_capped` — a count that stopped rather than finished. */
  capped?: boolean;
  /** The answer's `next_cursor`: a page with one is not the end of anything. */
  cursor?: string;
  /** Whether a narrowing is on, which is the only thing that changes the noun. */
  narrowed: boolean;
}): string {
  if (args.shown <= 0 || args.cursor || args.capped || args.hint > args.shown) return "";
  const count = plural(args.shown, "item");
  // "1 item matches" / "2 items match": the verb agrees with the count, which
  // a single spelling gets wrong at exactly the number a sparse company has.
  return args.narrowed
    ? `That is all of it · ${count} match${args.shown === 1 ? "es" : ""}`
    : `That is all of it · ${count}`;
}

/**
 * The month the calendar draws, and a fallback for anything that is not one.
 *
 * `month=` is a URL parameter, so it is whatever the address bar holds — and
 * [calendarWeeks] over an unparseable one returns NO WEEKS AT ALL: `Number`
 * gives `NaN`, `??` does not catch `NaN`, the cell count is `NaN` and the loop
 * never runs. An empty grid then takes [buildItemsParams] down the calendar
 * branch with no range, which is the one state that branch must never be in. A
 * mangled month is a bad address, not a company whose work has no dates.
 */
export function monthOrNow(month: string, now: number): string {
  return /^\d{4}-(0[1-9]|1[0-2])$/.test(month) ? month : monthOf(now);
}

/**
 * A count over one PAGE of an answer, and whether that page is the whole of it.
 *
 * `Card.Header count=` renders through the design system's `Count`, which prints
 * its value verbatim and carries no scope of its own — so a caller handing it
 * `records.length` over a read IT capped draws a number a reader takes for a
 * total. The tracker's activity strip asked the engine for twenty commits and
 * drew "20" whether the company had made twenty changes or twenty thousand.
 *
 * THE SECOND ARGUMENT IS REQUIRED, and that is the whole of the rule.
 * `FacetRail` made "over what is loaded or over what exists" a required prop
 * rather than a caption somebody remembers, for exactly this hazard. A header
 * count is the same number in a different slot; an optional flag here would be a
 * rule nobody fills in, which is the state it was already in.
 *
 * `+` RATHER THAN A WORD, because this product already spells "at least this
 * many" that way: [totalHint] renders the tracker's own capped total as `of
 * 10000+ matching`. A second spelling would make one screen's `20+` and
 * another's `20 (more)` read as two different facts.
 */
export function pageCount(shown: number, more: boolean): string {
  return more ? `${shown.toLocaleString()}+` : shown.toLocaleString();
}

/**
 * What a capped page says for itself, and "" when it is the whole set.
 *
 * EMPTY RATHER THAN A REASSURANCE. A note that always drew would put "and that
 * is all of them" under every healthy card in the product, which is how the one
 * case that matters arrives as a changed word nobody reads.
 *
 * The caller passes it on to a `subtitle`, which must be `undefined` and never
 * `""`: the empty string still renders the subtitle's own element.
 */
export function pageNote(shown: number, more: boolean, one: string, many?: string): string {
  return more ? `The newest ${plural(shown, one, many)}; there are more.` : "";
}

/**
 * The three segments, and NONE OF THEM IS THE EMPTY STRING.
 *
 * "Everything" used to be spelled `""`, and a scope is a URL key: the router's
 * own writer deletes a key set to the empty string (`Navigator.filter`), so
 * choosing All wrote nothing, the parameter read back as its fallback — the
 * view's own seeded scope, `open` on almost every container — and the segment
 * snapped back to Open on the next render. The one segment whose whole job is
 * to show finished work could not be selected at all. A zero value has to be
 * meaningful or the type must refuse it; here it could not be meaningful, so
 * the value has a name.
 */
export const SCOPES = ["open", "closed", "all"] as const;
export type Scope = (typeof SCOPES)[number];

/**
 * WHICH STATUS GROUPS EACH SEGMENT ADMITS, as the wire spells it.
 *
 * ONE SPELLING FOR BOTH READERS OF IT. [buildItemsParams] writes this key and
 * [scopeOf] reads it back off a saved view — two places that must agree about
 * exactly the same fact, and they already held two copies of the two strings.
 * A mismatch is silent: a segment that writes a group nothing reads back snaps
 * the control to the wrong value over a query that is narrowing correctly.
 *
 * The lanes a board draws were a third reader until the ENGINE started padding
 * a closed axis to what its own predicate admits — the same narrowing, derived
 * from the predicate itself rather than from a copy of it.
 *
 * `all` IS THE EMPTY STRING here and only here: it is the absence of the key
 * rather than a fourth value, which is what [buildItemsParams] deletes and what
 * makes every group admitted.
 */
export const SCOPE_GROUPS: Record<Scope, string> = {
  open: "not_started,active",
  closed: "done,closed",
  all: "",
};

/**
 * WHICH SEGMENT A `scope=` OFF THE ADDRESS ACTUALLY IS.
 *
 * A URL key is whatever the address bar holds, and [buildItemsParams] already
 * reads anything the three do not name as `all` — it is the else-branch of one
 * switch. Every other reader of the segment needs the same answer: the control
 * that draws it, the padding that narrows a board's lanes with it, and the
 * empty state that names it. Spelled per reader, a hand-edited `?scope=opne`
 * drew an unset switch over a query showing every closed task, which is the one
 * combination this segment exists to make impossible.
 */
export function asScope(value: string): Scope {
  return SCOPES.includes(value as Scope) ? (value as Scope) : "all";
}

/** A view's `status_group` mapped back onto the three segments. */
export function scopeOf(group: string | undefined): Scope {
  if (group === SCOPE_GROUPS.open) return "open";
  if (group === SCOPE_GROUPS.closed) return "closed";
  return "all";
}

/**
 * The segment a view OPENS on, which is the other half of that round trip.
 *
 * TWO DIFFERENT ABSENCES land on the same segment for different reasons. A
 * view naming no `status_group` at all is not a view asking for everything —
 * the tracker opens on unfinished work — so it seeds `open`. A view naming a
 * group the three segments cannot express (`active` alone) seeds `open` too,
 * because [buildItemsParams] overwrites `status_group` from the segment
 * whichever one it is: there is no reading that answers such a view as saved,
 * only a choice of which wider set to show. `open` adds the rest of open work;
 * the empty segment would add every closed task as well. Both are honest —
 * the control and the query agree about which set is on screen — and `open`
 * is the one nearer to what the view's author asked for.
 *
 * EXCEPT WHERE THE VIEW ITSELF WIDENED. `show_closed` with no `status_group`
 * is a view whose author asked for finished work explicitly, and seeding
 * `open` over it writes `status_group=not_started,active`, which excludes
 * exactly the half they widened to. `recent:168h` — "and whatever finished
 * this week" — answered as open work alone; the builtin TRASH tab, which is
 * removed work whatever state it was in when it was removed, answered as the
 * removed tasks that were still open, silently hiding every removal of
 * anything already done. So a view in that shape opens on the empty segment,
 * which deletes the key and lets the view's own `show_closed` stand.
 *
 * `show_closed: "false"` is NOT that: it is an author excluding finished work
 * on purpose, so it keeps the `open` seed and the control agrees with the
 * query rather than saying "All" over a query that shows less.
 *
 * TAKES THE WHOLE PARAMS rather than the one key, because that is the
 * information the decision needs — passed `status_group` alone it could not
 * see the widening and this was not expressible at all.
 *
 * HERE RATHER THAN AT THE CALL SITE so it can be asserted over. Spelled
 * `scopeOf(group) || "open"` inside the screen, it was the one step of the
 * round trip no test could reach: deleting it left every suite green while the
 * tracker opened on every closed task the company has.
 */
export function seededScope(view: Record<string, string> | undefined): Scope {
  const group = view?.status_group;
  const scope = scopeOf(group);
  if (scope !== "all") return scope;
  const widened = (view?.show_closed ?? "").trim();
  if (!group && widened && widened !== "false") return "all";
  return "open";
}

// ---------------------------------------------------------------------------
// The calendar
// ---------------------------------------------------------------------------

export interface CalendarCell {
  /** The local `YYYY-MM-DD` this cell is, which is also its row key. */
  key: string;
  day: number;
  inMonth: boolean;
  today: boolean;
}

/** The month an instant falls in, locally, as `YYYY-MM`. */
export function monthOf(now: number): string {
  return browserDay(new Date(now)).slice(0, 7);
}

export function shiftMonth(month: string, by: number): string {
  const [y, m] = month.split("-").map(Number);
  const at = new Date(y ?? 1970, (m ?? 1) - 1 + by, 1);
  return browserDay(at).slice(0, 7);
}

export function monthLabel(month: string): string {
  const [y, m] = month.split("-").map(Number);
  return new Date(y ?? 1970, (m ?? 1) - 1, 1).toLocaleDateString(undefined, {
    month: "long",
    year: "numeric",
  });
}

/**
 * One cell's full date, for a reader the colour never reaches.
 *
 * THE GRID'S DISTINCTION IS A COLOUR and a colour is not available to
 * everybody: `31` in the Monday cell of April's first row is March's, and the
 * quieter ground and ink that say so reach nobody on a screen reader. A grid
 * also holds two cells with the SAME numeral — April 2031 runs 31 March to 4 May,
 * so `1` and `4` each appear twice — which is the case a label has to separate
 * and a tint cannot.
 *
 * Built with `new Date(y, m - 1, d)` like [calendarWeeks] rather than through
 * [fmtDate], which parses an INSTANT: `2031-03-31` read as UTC midnight and
 * rendered west of Greenwich is the 30th, so the spoken label would name a
 * different day from the numeral beside it.
 */
export function dayLabel(key: string): string {
  const [y, m, d] = key.split("-").map(Number);
  return new Date(y ?? 1970, (m ?? 1) - 1, d ?? 1).toLocaleDateString(undefined, {
    weekday: "long",
    day: "numeric",
    month: "long",
    year: "numeric",
  });
}

/** Monday first, because the engine's own relative week tokens start there. */
export const WEEKDAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];

/**
 * The month's grid, Monday first, with the neighbouring days that fill it.
 *
 * MONDAY because `internal/tracker/dates.go` pins the week's start there — the
 * relative tokens a saved view can carry (`sow`, `eow`) mean Monday, and a
 * calendar that started on Sunday would draw "this week" across two of its own
 * rows.
 *
 * Five rows or six, as the month needs. A fixed six leaves an empty row on
 * most months, and an empty row of a calendar reads as a week in which nothing
 * is due rather than as a week that is not there.
 */
export function calendarWeeks(month: string, todayKey: string): CalendarCell[][] {
  const [y, m] = month.split("-").map(Number);
  const year = y ?? 1970;
  const index = (m ?? 1) - 1;
  const first = new Date(year, index, 1);
  const lead = (first.getDay() + 6) % 7;
  const days = new Date(year, index + 1, 0).getDate();
  const cells = Math.ceil((lead + days) / 7) * 7;

  const weeks: CalendarCell[][] = [];
  for (let i = 0; i < cells; i++) {
    const at = new Date(year, index, 1 - lead + i);
    const key = browserDay(at);
    if (i % 7 === 0) weeks.push([]);
    weeks[weeks.length - 1]?.push({
      key,
      day: at.getDate(),
      inMonth: at.getMonth() === index,
      today: key === todayKey,
    });
  }
  return weeks;
}

/**
 * The half-open range the grid covers, as the date filter spells it.
 *
 * THE GRID'S OWN BOUNDS rather than the month's, and that is the whole point:
 * the first and last rows carry days of the neighbouring months, and a query
 * bounded by the month would draw those cells permanently empty. It also
 * absorbs the one offset nothing else can: the engine resolves a bare date in
 * the COMPANY's zone and the cells are bucketed in the READER's, so a due date
 * within a day of either edge is inside the asked-for range either way.
 */
export function gridRange(weeks: CalendarCell[][]): { from: string; to: string } {
  const first = weeks[0]?.[0]?.key ?? "";
  const lastRow = weeks[weeks.length - 1];
  const last = lastRow?.[lastRow.length - 1]?.key ?? "";
  const after = new Date(`${last}T00:00:00`);
  after.setDate(after.getDate() + 1);
  return { from: first, to: browserDay(after) };
}

/** The local `YYYY-MM-DD` an instant falls on, or empty when it is unreadable. */
export function dayKey(ts: string | undefined): string {
  const at = parseUTC(ts);
  return at ? browserDay(at) : "";
}

/**
 * The rows of each day, ordered the way a day is read.
 *
 * ROWS WITHOUT A DUE DATE ARE DROPPED rather than bucketed under today: a
 * calendar is about when work is due, and putting undated work on the current
 * day would invent a deadline nobody set. The screen says so beneath the grid,
 * because a reader who cannot see the omission reads the month as the whole
 * backlog.
 */
export function bucketByDay(rows: WorkSummary[]): Map<string, WorkSummary[]> {
  const out = new Map<string, WorkSummary[]>();
  for (const row of rows) {
    const key = dayKey(row.due);
    if (!key) continue;
    const day = out.get(key);
    if (day) day.push(row);
    else out.set(key, [row]);
  }
  for (const day of out.values()) {
    day.sort((a, b) => (a.due ?? "").localeCompare(b.due ?? "") || a.key.localeCompare(b.key));
  }
  return out;
}

/** How many chips a calendar cell draws before it folds the rest into a count. */
export const CALENDAR_CELL_CHIPS = 3;
