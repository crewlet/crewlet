/**
 * The work list's one row of controls, read in the order a reader asks.
 *
 * # What is drawn, what narrows it, how it is arranged, then the tools
 *
 * The row opens with the DRAWING: the five shapes, a segmented row of their
 * glyphs, then (on a screen that is this list, never on a host whose own tabs
 * name the page) the saved views this reader pinned, a pin for the one
 * running, "+ View" to save the query on screen and the inventory. Shapes and
 * views are two wells, because a saved view is a query somebody arranged and a
 * shape is how any query is drawn.
 *
 * Then what NARROWS it: a chip per filter in force, ending in "+ Filter"
 * (`FilterChips.tsx`). Then how it is ARRANGED, at the row's far end: which
 * work is shown and the grouping, drawn as the kit's quiet pickers with their
 * captions inside ("Show Open", "Group by Due") so the arrangement says what
 * is on without anything being opened and still fits the one row, the lanes
 * out of a board's view (`shapes/Board.tsx`), and the count. The substring box
 * and the Display menu close the row.
 *
 * Each group is one cluster, so a row too narrow for everything wraps whole
 * clusters, the far end's right-aligned, and never strands the count or the
 * Display button alone on a line. The row is the screen's `toolbar`: see
 * `lib/useStickyBand.ts` for how what sticks under it learns its height.
 *
 * # The order is in Display, on every shape
 *
 * A list and a table are ordered at their column heads, where a press writes
 * the same `sort=` the picker does. Every shape's full list of orders (the
 * manual order a drag changes, the newest, the most tokens) is Display's
 * Sort, so the row keeps to what a reader changes constantly.
 *
 * NOTHING ELSE SITS BETWEEN THE ROW AND THE WORK. What the screen is, is in
 * the docs and in its empty state; why a card will not move is said when
 * somebody tries to move it, and in the Sort control's own title.
 */

import { useRef, type ComponentProps, type RefObject } from "react";
import { Input, Kbd, SegmentedControl, Select } from "@crewlethq/ui";
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
        inputSize="sm"
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
 * takes. Display's Sort on every shape but the calendar, whose order is its
 * dates.
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
 * One picker of a toolbar row, drawn two ways. In the bar it is the kit's
 * quiet picker with its caption inside the trigger ("Group by Status"), so it
 * shares the one toolbar row; in a panel (the Work list's Display on a phone)
 * it is a caption beside a bordered picker, the panel's form. History's
 * dimensions are drawn by it too.
 */
export function ArrangePicker({
  caption,
  inline,
  ...select
}: {
  caption: string;
  /** Drawn in the bar rather than in a panel. */
  inline: boolean;
} & Pick<
  ComponentProps<typeof Select>,
  "value" | "onChange" | "ariaLabel" | "active" | "options"
>) {
  if (inline) {
    return (
      <Select
        size="sm"
        width="auto"
        appearance="quiet"
        renderTrigger={(option) => (
          <span className="work-arrange-value">
            <span className="t-caption">{caption}</span>
            <span className="truncate">{option?.label}</span>
          </span>
        )}
        {...select}
      />
    );
  }
  return (
    <label className="work-arrange-row">
      <span className="t-caption">{caption}</span>
      <Select size="sm" width="auto" {...select} />
    </label>
  );
}

/**
 * Group by, at the bar's far end (or in Display on a phone).
 *
 * THE EFFECTIVE ARRANGEMENT, never the URL key (a saved view carries its own
 * `group_by` and the address overrides it), so the picker shows what the
 * engine was asked, and `""` out of the callback means OFF, which the screen
 * writes as a value where a view supplies one ([EXPLICIT_NONE]). The order is
 * Display's Sort on every shape: see the module doc.
 */
export function ArrangeControls({
  shape,
  workspace,
  groupBy,
  onGroupBy,
  inline = false,
}: {
  shape: Shape;
  workspace: boolean;
  groupBy: string;
  onGroupBy: (axis: string) => void;
  /** Drawn in the bar rather than in the Display panel. */
  inline?: boolean;
}) {
  // A CALENDAR'S AXIS IS THE DATE: there is nothing to group it by.
  if (shape === "calendar") return null;
  return (
    <ArrangePicker
      caption="Group by"
      inline={inline}
      // THE BOARD IS ALWAYS GROUPED, which is what a board IS, so its
      // picker offers no "none" and shows status where nothing chose one.
      value={shape === "board" ? groupBy || "status" : groupBy}
      onChange={(value) => onGroupBy(String(value))}
      ariaLabel="Group by"
      active={groupBy !== ""}
      options={groupAxisOptions(shape, workspace)}
    />
  );
}

/**
 * Which work is shown: open, open and this week's finished, finished, or all.
 *
 * A PICKER BESIDE GROUP BY rather than four segments opening the row: it is
 * the same kind of choice (how this answer is cut, made often and read at a
 * glance), and as four segments it took the start of the row the chips belong
 * in. A listbox also chooses on Enter rather than on every arrow, so walking
 * the options asks the engine nothing.
 */
export function ScopeControl({
  scope,
  onScope,
  inline = false,
}: {
  scope: Scope;
  onScope: (scope: string) => void;
  /** Drawn in the bar rather than in the Display panel. */
  inline?: boolean;
}) {
  return (
    <ArrangePicker
      caption="Show"
      inline={inline}
      value={scope}
      onChange={(value) => onScope(String(value))}
      ariaLabel="Which work"
      options={SCOPE_OPTIONS}
    />
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
