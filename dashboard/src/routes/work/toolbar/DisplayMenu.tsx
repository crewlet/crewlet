/**
 * One button that decides how the answer is DRAWN, as against what is in it.
 *
 * # Shape, grouping, order and columns are one question
 *
 * They were four places. The five shapes were tabs in the view strip, mixed in
 * with the views somebody had saved — so a saved view and a way of drawing one
 * read as the same kind of thing, and a company with four saved views had a
 * strip of nine. Group by and Sort were selects in the filter bar, appearing
 * on some shapes and not others, which moved every control beside them. The
 * grid's own column set was a URL key with no control at all.
 *
 * All four are the same question — how do I want to look at this — and none of
 * them narrows the answer, which is why they are here and not among the chips.
 *
 * # Columns are offered on both grid shapes
 *
 * The list and the table are ONE grid drawn in two column sets, so both have
 * columns to choose and this menu offers whichever set is active. It offered
 * them on the table alone while the two were two components, under a rule that
 * read "the column set belongs to the table, which is the one shape with
 * columns" — a sentence that described the implementation rather than the
 * product, and the only thing making it true was that the list had no columns
 * to give. See `routes/work/shapes/Grid.tsx`.
 *
 * # It writes `shape=`, never `view=`
 *
 * A view is the saved QUERY and the shape is the drawing, so changing the
 * drawing must not throw away the view: `view=` keeps naming what somebody
 * saved, `shape=` overrides the type that view was saved with, and a reader
 * who switches a saved board to a list still has the board's filters.
 *
 * # What each shape does not offer, it does not draw
 *
 * A calendar's axis IS the date, so it has no grouping to set; a board's
 * second axis is a swimlane grid rather than a band in a band, so it has no
 * Then by; and a board, a calendar and a timeline have no columns to choose
 * between. A control that wrote a key its shape drops is a control whose
 * effect the reader cannot see.
 */

import { Button, Popover, Select } from "@crewlethq/ui";
import {
  CalendarTodayGlyph,
  DashboardGlyph,
  ListGlyph,
  TimelineGlyph,
  ViewColumnGlyph,
  VisibilityGlyph,
} from "@crewlethq/icons/glyphs";
import { GROUP_AXES, SORTS, groupAxisOptions, secondAxisOptions, type Shape } from "~/lib/work.ts";
import { columnChoices, isGridShape } from "../shapes/Grid.tsx";

/** The five shapes, each with the mark it is drawn as in the strip. */
const SHAPES: { value: Shape; label: string; Glyph: typeof ListGlyph }[] = [
  { value: "list", label: "List", Glyph: ListGlyph },
  { value: "board", label: "Board", Glyph: DashboardGlyph },
  { value: "table", label: "Table", Glyph: ViewColumnGlyph },
  { value: "calendar", label: "Calendar", Glyph: CalendarTodayGlyph },
  { value: "timeline", label: "Timeline", Glyph: TimelineGlyph },
];

export interface DisplayMenuProps {
  shape: Shape;
  /**
   * The axis the QUERY was sent on, which is not always the one in the URL.
   *
   * A board is grouped by status whether or not anybody said so, and a saved
   * view may carry `group_by` with nothing on the address — so a button built
   * from the URL key alone said "Board" over a board whose columns were
   * statuses, and said nothing at all about a saved view's own grouping.
   */
  axis: string;
  /** The company's list rather than one project's, which decides whether the
   *  grid has a Project column to offer. */
  workspace: boolean;
  /** What the shape would be with no `shape=` on the address — a view's own. */
  viewShape: Shape;
  /**
   * THE EFFECTIVE ARRANGEMENT, never the URL key.
   *
   * The reader's own `group_by` where they set one, the saved view's where
   * they did not, and `""` where neither says anything — which is what
   * [effectiveArrangement] answers and what the query was built from. Read off
   * the address instead, these controls sat on "No grouping" over a list the
   * engine had grouped, and a second grouping the view carried was never
   * offered a way off at all.
   *
   * `""` OUT OF THE CALLBACKS MEANS OFF, and turning a view's own arrangement
   * off is a value rather than a deletion — which is the screen's to write,
   * since only it knows what the view carries.
   */
  groupBy: string;
  groupBy2: string;
  sort: string;
  /**
   * The ACTIVE SET's chosen columns, as `cols.<shape>=` holds them.
   *
   * The screen reads the key for the shape that is on and hands the value
   * here, because `cols=` is keyed per shape: the two sets differ in default
   * and in ORDER, and a value written against one and read against the other
   * draws a row nobody arranged. See `routes/work/shapes/Grid.tsx`.
   */
  cols: string;
  onShape: (shape: Shape) => void;
  onGroupBy: (axis: string) => void;
  onGroupBy2: (axis: string) => void;
  onSort: (sort: string) => void;
  onCols: (cols: string) => void;
}

export function DisplayMenu(props: DisplayMenuProps) {
  const shapeName = SHAPES.find((s) => s.value === props.shape)?.label ?? "List";
  const axisName = GROUP_AXES.find((a) => a.value === props.axis)?.label;
  return (
    <Popover
      role="dialog"
      label="Display"
      align="end"
      trigger={(open, toggle) => (
        <Button
          size="small"
          variant="tertiary"
          leadingIcon={<VisibilityGlyph size="sm" />}
          aria-expanded={open}
          aria-haspopup="dialog"
          onClick={toggle}
        >
          {/* THE BUTTON SAYS WHAT IS ON, so the arrangement is readable without
              opening anything — which is what the strip of shape tabs used to
              do and what a bare "Display" would have taken away. */}
          {axisName ? `${shapeName} · ${axisName}` : shapeName}
        </Button>
      )}
    >
      <DisplayPanel {...props} />
    </Popover>
  );
}

function DisplayPanel({
  shape,
  workspace,
  viewShape,
  groupBy,
  groupBy2,
  sort,
  cols,
  onShape,
  onGroupBy,
  onGroupBy2,
  onSort,
  onCols,
}: DisplayMenuProps) {
  // THE BOARD IS ALWAYS GROUPED — it is what a board IS — so its picker offers
  // no "none" and shows the status axis where nothing chose one, exactly as
  // the query does; see [groupAxisOptions] for the row it no longer lists
  // twice.
  const grouped = shape !== "calendar";
  const nested = isGridShape(shape);
  const chosen = new Set(cols ? cols.split(",").filter(Boolean) : []);
  // THE ACTIVE SET'S OWN CHOICES. Both grid shapes have columns and they are
  // not the same columns in the same order, so the set is asked for by shape
  // rather than derived once — see `shapes/Grid.tsx`.
  const columns = isGridShape(shape) ? columnChoices(shape, workspace) : [];

  return (
    <div className="work-menu work-display">
      <div className="work-display-shapes">
        {SHAPES.map(({ value, label, Glyph }) => (
          <button
            key={value}
            type="button"
            className="work-display-shape"
            aria-pressed={shape === value}
            onClick={() => onShape(value)}
          >
            <Glyph size="sm" />
            <span>{label}</span>
          </button>
        ))}
      </div>

      {grouped && (
        <label className="work-display-row">
          <span>Group by</span>
          <Select
            width="auto"
            value={shape === "board" ? groupBy || "status" : groupBy}
            onChange={(value) => onGroupBy(String(value))}
            ariaLabel="Group by"
            active={groupBy !== ""}
            options={groupAxisOptions(shape, workspace)}
          />
        </label>
      )}

      {nested && groupBy !== "" && (
        <label className="work-display-row">
          <span>Then by</span>
          <Select
            width="auto"
            value={groupBy2}
            onChange={(value) => onGroupBy2(String(value))}
            ariaLabel="Then by"
            active={groupBy2 !== ""}
            // NEVER THE AXIS ALREADY CHOSEN: the engine refuses that pair,
            // because every row would be alone in its own band.
            options={secondAxisOptions(groupBy, workspace)}
          />
        </label>
      )}

      {shape !== "calendar" && (
        <label className="work-display-row">
          <span>Order by</span>
          <Select
            width="auto"
            value={sort}
            onChange={(value) => onSort(String(value))}
            ariaLabel="Order by"
            active={sort !== ""}
            options={[{ value: "", label: "Default order" }, ...SORTS]}
          />
        </label>
      )}

      {columns.length > 0 && (
        <>
          <div className="work-display-label">Columns</div>
          <div className="work-display-cols">
            {columns.map((column) => {
              // AN EMPTY `cols=` IS THE DEFAULT SET, not an empty grid —
              // `DataGrid` reads it that way, so the checkboxes show the
              // default until somebody moves one.
              const on = chosen.size === 0 ? !column.optional : chosen.has(column.key);
              return (
                <label key={column.key} className="work-display-col">
                  <input
                    type="checkbox"
                    checked={on}
                    onChange={() => {
                      const next = new Set(
                        chosen.size === 0
                          ? columns.filter((c) => !c.optional).map((c) => c.key)
                          : chosen,
                      );
                      if (on) next.delete(column.key);
                      else next.add(column.key);
                      // THE DECLARATION ORDER, never the click order: `cols=`
                      // is read as the order to DRAW them in, so a set built
                      // from a Set's insertion order would rearrange the grid
                      // every time somebody ticked a box. It is also why the
                      // key is per shape — the order it carries is the ACTIVE
                      // set's, and the other set declares another one.
                      onCols(
                        columns
                          .filter((c) => next.has(c.key))
                          .map((c) => c.key)
                          .join(","),
                      );
                    }}
                  />
                  <span>{column.label}</span>
                </label>
              );
            })}
          </div>
        </>
      )}

      {/* WHAT THE VIEW ITSELF SAYS, and the way back to it. A reader who has
          overridden the shape has no other way to tell that they have: the
          strip shows which view is running, not which drawing it was saved
          with. */}
      {shape !== viewShape && (
        <div className="work-display-foot">
          <Button size="small" variant="tertiary" onClick={() => onShape(viewShape)}>
            Back to this view&rsquo;s own shape
          </Button>
        </div>
      )}
    </div>
  );
}
