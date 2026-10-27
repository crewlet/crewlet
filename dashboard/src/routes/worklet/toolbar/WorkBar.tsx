/**
 * The work list's two rows of controls, as the approved board draws them.
 *
 * # The first row is WHAT is drawn: the shape, then the saved views
 *
 * The five shapes are a segmented row of the DRAWING, never mixed with the
 * views: they are two rows, each in its own well, because a saved view is a
 * query somebody arranged and a shape is how any query is drawn. Both are the
 * kit's `SegmentedControl`, the switch the Artifacts gallery draws too. They were in
 * the Display menu for a while, which made the board — the shape a team lives
 * in — two presses away and said nothing about which was on until the menu was
 * opened. The views after the divider are the ones this reader PINNED, plus
 * whichever one is running; the rest are one link away in the inventory, and
 * "+ View" saves the query on screen as a new one.
 *
 * The row ends in the substring box — always drawn, with the `/` that focuses
 * it — and the Display menu, which holds what is left of the arrangement (the
 * second axis, the columns, the hidden lanes). The Filter menu is not here: it
 * is the "+ Filter" that ends the chip row below.
 *
 * # The second row is HOW the answer is cut — and it is the only other row
 *
 * The chips (what narrows it) ending in "+ Filter", and at the far end the
 * choices a reader makes constantly: which work is shown, the grouping and,
 * on a board or a timeline, the order. Group by and Sort were inside Display;
 * lifted into the bar they say what is on without anything being opened,
 * which is the one thing a menu button cannot.
 *
 * A LIST AND A TABLE ARE ORDERED AT THEIR COLUMN HEADS, so the bar draws no
 * Sort for them: the head that orders the rows shows the order where the
 * values are, and a press on it writes the same `sort=` the picker did. The
 * orders no column carries (the manual order, the newest, the most tokens)
 * stay one press away in Display's Sort.
 *
 * The lanes out of a board's view are named at the same end (see
 * `shapes/Board.tsx`), so the board's lanes start directly under this row, as
 * the approved board draws them.
 *
 * NOTHING ELSE SITS BETWEEN THE BAR AND THE WORK. There was an introduction
 * above the tabs and a hint line above the lanes; each cost the board a row on
 * every visit to say something a reader needs once. What the screen is, is in
 * the docs and in its empty state; why a card will not move is said when
 * somebody tries to move it, and in the Sort control's own title.
 */

import { useRef, type RefObject } from "react";
import { Input, Kbd, SegmentedControl, Select, cx } from "@crewlethq/ui";
import {
  CalendarGlyph,
  ChartNoAxesGanttGlyph,
  Columns3Glyph,
  LayersGlyph,
  LayoutDashboardGlyph,
  ListGlyph,
  PinGlyph,
  SearchGlyph,
} from "@crewlethq/icons/glyphs";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { href } from "~/app/router.tsx";
import {
  MANUAL_ORDER,
  RECENT_ORDER,
  SORTS,
  TIMELINE_ORDER,
  TRASH_ORDER,
  defaultOrderOf,
  groupAxisOptions,
  type Scope,
  type Shape,
} from "~/lib/work.ts";
import { isGridShape } from "../shapes/Grid.tsx";
import type { WorkView } from "~/protocol/index.ts";

/** The five shapes, in the order the approved board draws them. */
export const SHAPES: { value: Shape; label: string; Glyph: typeof ListGlyph }[] = [
  { value: "list", label: "List", Glyph: ListGlyph },
  { value: "board", label: "Board", Glyph: LayoutDashboardGlyph },
  { value: "timeline", label: "Timeline", Glyph: ChartNoAxesGanttGlyph },
  { value: "calendar", label: "Calendar", Glyph: CalendarGlyph },
  { value: "table", label: "Table", Glyph: Columns3Glyph },
];

/**
 * The shapes, as the kit's segmented row: the same switch the Artifacts
 * gallery draws its List and Cards with, so the two view switches in the
 * product are one control.
 *
 * A RADIO GROUP, NOT A TABLIST, because each choice redraws THE SAME panel
 * below rather than showing one of five: a `tablist` promises a panel per tab,
 * and a screen reader would announce a structure the page does not have.
 *
 * MANUAL ACTIVATION, because the shape is a section in the URL: a change
 * pushes a history entry and redraws the list, so the arrows move focus and
 * Enter or Space chooses, and walking the row leaves nothing for Back to walk
 * back through.
 *
 * EACH SHAPE IS ITS GLYPH, named for a screen reader and titled for a
 * pointer, as the gallery's are. Drawn as glyphs the five fit on one line at a
 * phone's width, which the worded row did not.
 */
export function ShapeSwitch({ shape, onShape }: { shape: Shape; onShape: (shape: Shape) => void }) {
  return (
    <SegmentedControl<Shape>
      semantics="radio"
      activate="manual"
      size="sm"
      label="Draw as"
      value={shape}
      onValueChange={onShape}
      options={SHAPES.map(({ value, label, Glyph }) => ({
        value,
        icon: <Glyph size="sm" />,
        srLabel: label,
        title: label,
      }))}
    />
  );
}

/**
 * The views in the strip: the container's own list, every view this reader
 * pinned, and the one running if it is not pinned.
 *
 * THE CONTAINER'S OWN LIST STAYS FIRST, because a strip of saved views with no
 * way back to the unsaved list is a strip a reader gets stuck in.
 *
 * THE SAME SEGMENTED ROW AS THE SHAPES, with the same manual activation for
 * the same reason: a view is a URL change that re-runs the list. A strip with
 * more pinned views than the row holds wraps inside its well, as every kit
 * segmented row does, and a long name is cut at a width the row can hold with
 * its title carrying the rest.
 */
export function ViewSwitch({
  views,
  chosen,
  containerLabel,
  onView,
}: {
  views: WorkView[];
  chosen: string;
  containerLabel: string;
  onView: (key: string) => void;
}) {
  const strip = views.filter((v) => !v.builtin && (v.pinned || v.key === chosen));
  const current = strip.some((v) => v.key === chosen) ? chosen : "";
  return (
    <SegmentedControl
      semantics="radio"
      activate="manual"
      size="sm"
      label="Saved views"
      value={current}
      onValueChange={onView}
      options={[
        { value: "", label: containerLabel },
        ...strip.map((v) => ({
          value: v.key,
          icon: v.pinned ? <PinGlyph size="sm" /> : undefined,
          label: <span className="work-view-name truncate">{v.name}</span>,
          title: v.pinned ? `${v.name} (pinned)` : v.name,
        })),
      ]}
    />
  );
}

/**
 * The link to every saved view, which is what the strip does not draw.
 *
 * A GLYPH NAMED "All saved views", not the words: the strip's end is the
 * narrowest room on the row and the one place a reader does not look for a
 * sentence. Written out ("All views →", 70px) it was the item that pushed the
 * search box and both menus onto a second line at 1280px, where the approved
 * board holds them on the row. Still a real anchor — middle-clickable like
 * every other way out of a screen — named for a screen reader and titled for
 * a pointer.
 */
export function AllViewsLink() {
  return (
    <a
      className="work-strip-more"
      href={href(["worklet", "views"])}
      aria-label="All saved views"
      title="All saved views"
    >
      <LayersGlyph size="sm" aria-hidden="true" />
    </a>
  );
}

/**
 * The substring box: key or title, over the rows on screen — `q=`.
 *
 * NOT THE SEARCH SCREEN, and it says so by its placeholder: `#/worklet/search`
 * ranks the whole company against a phrase the way an agent does, where this
 * is an escaped substring over two columns. `/` focuses it, because it is the
 * search of the screen the reader is on.
 */
export function SubstringBox({
  value,
  onChange,
  scopeLabel,
}: {
  value: string;
  onChange: (value: string) => void;
  /** What the box narrows, for its placeholder: `ENG`, or "work". */
  scopeLabel: string;
}) {
  const box = useRef<HTMLInputElement>(null);
  useSearchTarget(box as RefObject<HTMLElement | null>);
  return (
    <span className="work-bar-find">
      <Input
        ref={box}
        type="search"
        width="full"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        aria-label="Narrow these rows by key or title"
        leading={<SearchGlyph size="sm" />}
        trailing={value ? undefined : <Kbd subtle keys={["/"]} />}
        placeholder={`Search ${scopeLabel}`}
      />
    </span>
  );
}

/**
 * The order picker: the shape's default, named, then every order the engine
 * takes. ONE CONTROL in two places, the bar's end on a board or a timeline and
 * Display's Sort on a list or a table, so the two can never offer different
 * orders or name the default differently.
 */
export function OrderSelect({
  shape,
  workspace,
  trash = false,
  sort,
  onSort,
}: {
  shape: Shape;
  workspace: boolean;
  /** A trash listing, whose default is the removal order. */
  trash?: boolean;
  /** The effective order, never the URL key. */
  sort: string;
  onSort: (sort: string) => void;
}) {
  return (
    <Select
      size="sm"
      width="auto"
      value={sort}
      onChange={(value) => onSort(String(value))}
      ariaLabel="Sort"
      active={sort !== ""}
      // THE DEFAULT SAYS WHAT IT IS, because it is a different order per
      // container and shape (the engine's manual order inside a project, the
      // most recently touched across the company, the start date on a
      // timeline), and "Default" alone names none of them.
      options={[{ value: "", label: defaultOrderLabel(shape, workspace, trash) }, ...SORTS]}
    />
  );
}

/**
 * Group by and, where no column head orders the rows, Sort, in the bar.
 *
 * THE EFFECTIVE ARRANGEMENT, never the URL key — a saved view carries its own
 * `group_by` and `sort` and the address overrides them — so the pickers show
 * what the engine was asked, and `""` out of a callback means OFF, which the
 * screen writes as a value where a view supplies one ([EXPLICIT_NONE]).
 */
export function ArrangeControls({
  shape,
  workspace,
  trash = false,
  groupBy,
  sort,
  onGroupBy,
  onSort,
  manual = true,
}: {
  shape: Shape;
  workspace: boolean;
  /** A trash listing, whose default order is the removal order. */
  trash?: boolean;
  groupBy: string;
  sort: string;
  /** Whether the order on screen is the manual one a drag changes. */
  manual?: boolean;
  onGroupBy: (axis: string) => void;
  onSort: (sort: string) => void;
}) {
  // A CALENDAR'S AXIS IS THE DATE and its order is the date: neither control
  // has anything to change there, so neither is drawn.
  if (shape === "calendar") return null;
  return (
    <span className="work-arrange">
      <label className="work-arrange-row">
        <span className="t-caption">Group by</span>
        <Select
          size="sm"
          width="auto"
          // THE BOARD IS ALWAYS GROUPED — it is what a board IS — so its
          // picker offers no "none" and shows status where nothing chose one.
          value={shape === "board" ? groupBy || "status" : groupBy}
          onChange={(value) => onGroupBy(String(value))}
          ariaLabel="Group by"
          active={groupBy !== ""}
          options={groupAxisOptions(shape, workspace)}
        />
      </label>
      {/* A LIST AND A TABLE SORT AT THEIR HEADS: see the module doc. */}
      {!isGridShape(shape) && (
        <label
          className={cx("work-arrange-row")}
          title={shape === "board" && !manual ? MANUAL_ORDER_HINT : undefined}
        >
          <span className="t-caption">Sort</span>
          <OrderSelect
            shape={shape}
            workspace={workspace}
            trash={trash}
            sort={sort}
            onSort={onSort}
          />
        </label>
      )}
    </span>
  );
}

/**
 * Which work is shown: open, open and this week's finished, finished, or all.
 *
 * A PICKER BESIDE GROUP BY AND SORT rather than four segments opening the
 * row: it is the same kind of choice as those two — how this answer is cut,
 * made often and read at a glance — and as four segments it took the start of
 * the row the chips belong in, pushing the narrowings a reader reads first
 * behind a control they set once. A listbox also chooses on Enter rather than
 * on every arrow, so walking the options asks the engine nothing.
 */
export function ScopeControl({
  scope,
  onScope,
}: {
  scope: Scope;
  onScope: (scope: string) => void;
}) {
  return (
    <label className="work-arrange-row">
      <span className="t-caption">Show</span>
      <Select
        size="sm"
        width="auto"
        value={scope}
        onChange={(value) => onScope(String(value))}
        ariaLabel="Which work"
        options={SCOPE_OPTIONS}
      />
    </label>
  );
}

/**
 * The four scopes, each with the sentence its option carries. RECENT is the
 * board's default, because a board's last lane is Done and it should hold the
 * week's deliveries rather than the company's whole history.
 */
export const SCOPE_OPTIONS: { value: Scope; label: string; description: string }[] = [
  { value: "open", label: "Open", description: "Work nobody has finished" },
  { value: "recent", label: "Recent", description: "Open work, and what finished this week" },
  { value: "closed", label: "Closed", description: "Finished and cancelled work" },
  { value: "all", label: "All", description: "Everything, finished or not" },
];

/**
 * Why a card may not move, as the Sort control's title: the drag is a place in
 * the manual order, so any other order is one a drag cannot change. Said
 * where the order is chosen, and on the board only.
 */
export const MANUAL_ORDER_HINT = "Choose this to drag cards.";

/** Each default order, in the words the options use. */
const DEFAULT_ORDER_WORDS: Readonly<Record<string, string>> = {
  [TIMELINE_ORDER]: "Start date (default)",
  [TRASH_ORDER]: "Recently removed (default)",
  [RECENT_ORDER]: "Recently updated (default)",
  [MANUAL_ORDER]: "Manual (default)",
};

/** What the order is when nobody chose one ([defaultOrderOf]), in the words the options use. */
export function defaultOrderLabel(shape: Shape, workspace: boolean, trash = false): string {
  return DEFAULT_ORDER_WORDS[defaultOrderOf(shape, workspace, trash)] ?? "Default";
}
