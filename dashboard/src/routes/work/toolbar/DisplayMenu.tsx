/**
 * One button for what is left of the arrangement once the bar holds the rest.
 *
 * # What moved out, and why this menu still exists
 *
 * The five SHAPES are tabs in the bar again, and GROUP BY and SORT sit at the
 * bar's far end: a reader changes those constantly, and a menu button cannot
 * say what three things are set to without being opened. What stays here is
 * what a reader sets once and forgets — the SECOND axis a list nests inside the
 * first, the grid's COLUMNS, and the board LANES somebody put away — plus the
 * way back to a saved view's own shape.
 *
 * # Columns are offered on both grid shapes
 *
 * The list and the table are ONE grid drawn in two column sets, so both have
 * columns to choose and this menu offers whichever set is active. See
 * `routes/work/shapes/Grid.tsx`.
 *
 * # What each shape does not offer, it does not draw
 *
 * A board's second axis is a swimlane grid rather than a band in a band, so it
 * has no Then by; a board, a calendar and a timeline have no columns; and only
 * a board has cards whose facts can be chosen and lanes to hide. A control that wrote a key its shape drops is a
 * control whose effect the reader cannot see.
 */

import { Button, Popover, Select, Tag } from "@crewlethq/ui";
import { Settings2Glyph } from "@crewlethq/icons/glyphs";
import { secondAxisOptions, type Shape } from "~/lib/work.ts";
import { usePanelAlign } from "~/lib/media.ts";
import { ColumnChooser } from "~/app/frame/DataGrid.tsx";
import { columnChoices, isGridShape } from "../shapes/Grid.tsx";
import { CARD_FACTS, type CardFact } from "~/components/work.tsx";

export interface DisplayMenuProps {
  shape: Shape;
  /** The company's list rather than one project's, which decides whether the
   *  grid has a Project column to offer. */
  workspace: boolean;
  /**
   * The running SAVED view's own shape, or nothing where no saved view runs.
   * A builtin's shape is not "this view's own" to go back to: offered over a
   * plain board, the way back pointed at a view nobody had open.
   */
  viewShape?: Shape;
  /**
   * THE EFFECTIVE FIRST AXIS, never the URL key — the second is offered only
   * beside one, and never as the same axis.
   */
  groupBy: string;
  /** The effective second axis. `""` out of its callback means OFF. */
  groupBy2: string;
  /**
   * The ACTIVE SET's chosen columns, as `cols.<shape>=` holds them.
   *
   * The screen reads the key for the shape that is on and hands the value
   * here, because `cols=` is keyed per shape: the two sets differ in default
   * and in ORDER, and a value written against one and read against the other
   * draws a row nobody arranged. See `routes/work/shapes/Grid.tsx`.
   */
  cols: string;
  /** The board lanes put away, each with the name its heading carries. */
  hidden: { key: string; label: string }[];
  onShape: (shape: Shape) => void;
  onGroupBy2: (axis: string) => void;
  onCols: (cols: string) => void;
  /** Bring one lane back, or every one with no key. */
  onShowLane: (key?: string) => void;
  /** The board card facts put away (`card_hide=`). */
  cardHidden: ReadonlySet<CardFact>;
  onCardHidden: (hidden: ReadonlySet<CardFact>) => void;
}

export function DisplayMenu(props: DisplayMenuProps) {
  // END-ALIGNED under a toolbar's right edge, START on a phone, where the
  // toolbar wraps this trigger to the left edge — see [usePanelAlign].
  const align = usePanelAlign("end");
  return (
    <Popover
      role="dialog"
      label="Display"
      align={align}
      trigger={(open, toggle) => (
        <Button
          size="small"
          variant="secondary"
          leadingIcon={<Settings2Glyph size="sm" />}
          aria-expanded={open}
          aria-haspopup="dialog"
          onClick={toggle}
        >
          Display
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
  cols,
  hidden,
  onShape,
  onGroupBy2,
  onCols,
  onShowLane,
  cardHidden,
  onCardHidden,
}: DisplayMenuProps) {
  const nested = isGridShape(shape);
  // THE ACTIVE SET'S OWN CHOICES. Both grid shapes have columns and they are
  // not the same columns in the same order, so the set is asked for by shape
  // rather than derived once — see `shapes/Grid.tsx`.
  const columns = isGridShape(shape) ? columnChoices(shape, workspace) : [];
  const board = shape === "board";
  const lanes = board ? hidden : [];
  const empty = !(nested && groupBy !== "") && columns.length === 0 && !board;

  return (
    <div className="work-menu work-display">
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

      {/* THE SHARED CHOOSER, which is where the two rules that are easy to get
          wrong now live once — an empty `cols=` is the DEFAULT set rather than
          an empty grid, and the value it writes is in DECLARATION order rather
          than click order. See [ColumnChooser]. */}
      <ColumnChooser choices={columns} value={cols} onChange={onCols} />

      {/* WHAT A CARD SHOWS — the board's own arrangement, as a grid's is its
          columns. Only the descriptive facts: the states a board is triaged
          by are not a reader's to hide (see [CARD_FACTS]). */}
      {board && (
        <>
          <div className="grid-cols-label">Card shows</div>
          <div className="grid-cols-choices" data-card-facts="">
            {CARD_FACTS.map((fact) => (
              <label key={fact.key} className="grid-cols-choice">
                <input
                  type="checkbox"
                  checked={!cardHidden.has(fact.key)}
                  onChange={() => {
                    const next = new Set(cardHidden);
                    if (next.has(fact.key)) next.delete(fact.key);
                    else next.add(fact.key);
                    onCardHidden(next);
                  }}
                />
                <span>{fact.label}</span>
              </label>
            ))}
          </div>
        </>
      )}

      {/* THE LANES PUT AWAY, named, each one press from coming back — a
          hidden lane is otherwise a column of work nobody can tell is missing,
          on an address somebody else opened. */}
      {lanes.length > 0 && (
        <div className="work-display-lanes">
          <span className="work-display-label">Hidden lanes</span>
          <span className="work-display-chips">
            {lanes.map((lane) => (
              <Tag
                key={lane.key || "-"}
                size="sm"
                appearance="outline"
                onRemove={() => onShowLane(lane.key)}
                removeAriaLabel={`Show the ${lane.label} lane`}
              >
                {lane.label}
              </Tag>
            ))}
          </span>
          <Button size="small" variant="ghost" onClick={() => onShowLane()}>
            Show every lane
          </Button>
        </div>
      )}

      {empty && (
        <p className="work-menu-empty">
          Nothing more to arrange on this shape — its grouping and order are in the bar.
        </p>
      )}

      {/* WHAT A SAVED VIEW ITSELF SAYS, and the way back to it. A reader who
          has overridden its shape has no other way to tell that they have: the
          strip shows which view is running, not which drawing it was saved
          with. */}
      {viewShape !== undefined && shape !== viewShape && (
        <div className="work-display-foot">
          <Button size="small" variant="ghost" onClick={() => onShape(viewShape)}>
            Back to this view&rsquo;s own shape
          </Button>
        </div>
      )}
    </div>
  );
}
