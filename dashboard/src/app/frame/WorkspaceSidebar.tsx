/**
 * The workspace sidebar: one workspace's tree, and nothing else.
 *
 * # Every row is a destination, and a destination is a path AND a query
 *
 * Most rows differ from the current page by their PATH, and this file used to
 * say that was the rule — that a row differing only by a query would be "a tab
 * wearing a sidebar row's clothes". One group does exactly that and is right
 * to: Activity's seats are eight rows on `activity/turns` that differ only by
 * `?seat=`, because a seat is a FILTER on one log rather than eight logs.
 *
 * The rule the doc stated and the rows it described disagreed, and selection
 * believed the doc: it compared paths alone, so every seat matched at once.
 * All eight drew the current-row tint and all eight carried
 * `aria-current="page"` — a screen reader hearing eight current pages, and a
 * reader seeing the accent spent on a whole list where the frame spends it
 * exactly twice. It was invisible under a 10%-alpha tint on a grey page and
 * obvious the moment the palette changed, which is how it was found.
 *
 * So selection compares BOTH, and a row with no query is still COMPATIBLE with
 * a route that carries one — `activity/turns` is still where you are when a seat
 * narrows it.
 *
 * # EXACTLY ONE ROW IS CURRENT, and the sidebar decides which
 *
 * Comparing the query too made the seats one match each and left the rule where
 * it cannot be enforced: in a predicate every row answers about itself. Two
 * sections holding one destination then answer yes twice, and that is the
 * ordinary case rather than a corner — Starred and Recent repeat the tree's own
 * rows by design, so a project the reader had opened drew the accent twice and
 * read out as two current pages, and `Turns` did the same over every seat row
 * beneath it.
 *
 * So the sidebar picks ONE row across every section it draws: the narrowest
 * compatible row, ties going to the first in reading order. [currentRowOf]
 * carries the rest of that reasoning.
 *
 *
 * # Selection is derived, never held
 *
 * Which row is current comes from the route. Held in state it would survive a
 * navigation the reader made some other way — the palette, a breadcrumb, the
 * Back button — and mark a row they are not on.
 *
 * # Counts are honest
 *
 * A count is either a maintained total the engine keeps or it is a count over
 * what was loaded, and in that case the row says so in its title. A badge that
 * renders `rows.length` as though it were a total is how a sidebar comes to
 * claim a company has 50 notices when the page size is 50.
 */

import { useMemo, useState, type ReactNode } from "react";
import { href, useRoute, samePath } from "../router.tsx";
import { Input, StatusDot, cx } from "@crewlethq/ui";
import { ChevronRightGlyph, KeyboardArrowDownGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
// A ROW'S MARK IS DATA — it comes from `app/nav.ts`'s destinations table by
// way of `workspaces/sidebars.tsx`, and from each workspace's own builder — so
// it travels as a NAME and is resolved here. See the registry's own doc for
// why that entry point is separate from the glyphs themselves.
import { markByName, type MarkName } from "~/ui/glyph.tsx";
import { type Tone } from "~/ui/primitives.tsx";
import { uiletTone } from "~/ui/primitives.tsx";

export interface SidebarRow {
  key: string;
  label: string;
  /** The destination. A row without one is not a row. */
  path: string[];
  /** Query carried with the path — allowed only where the ROW IS the list. */
  query?: Record<string, string>;
  icon?: MarkName;
  /** A state mark, never an identity colour. */
  tone?: Tone;
  /**
   * A number beside the label, and WHAT IT COUNTS — one field, because the two
   * halves were a number and an optional sibling and the optional half is the
   * one that went missing. "Leadership 5" in this rail was a unit's whole
   * subtree while the org chart's block under the same name read "2 seats" and
   * the roster's group head a third figure, none of them saying which
   * question they had answered. `of` is the title a reader gets on the badge;
   * it is the same rule [FacetRail] makes a required prop for a chip.
   */
  count?: { value: number; of: string };
  /** A second line under the label — a lead's handle, an owner. */
  sub?: string;
  children?: SidebarRow[];
}

export interface SidebarSection {
  key: string;
  label?: string;
  rows: SidebarRow[];
  /** Collapsible sections remember nothing: a tree is read, not configured. */
  collapsible?: boolean;
  /** Rendered in place of rows when there are none — never a blank section. */
  empty?: ReactNode;
}

/** A row's own mark, resolved from the name the destinations table carries. */
function RowGlyph({ name }: { name: MarkName }) {
  const Glyph = markByName(name);
  return <Glyph size="sm" />;
}

/** Whether this row, or anything under it, is the page being read. */
function containsRoute(row: SidebarRow, path: string[]): boolean {
  if (samePath(row.path, path)) return true;
  return (row.children ?? []).some((child) => containsRoute(child, path));
}

function matches(row: SidebarRow, needle: string): boolean {
  if (!needle) return true;
  if (row.label.toLowerCase().includes(needle)) return true;
  if (row.sub?.toLowerCase().includes(needle)) return true;
  return (row.children ?? []).some((child) => matches(child, needle));
}

/**
 * Whether this row's destination is COMPATIBLE with the route — not whether it
 * is the one row that is current, which is [currentRowOf]'s answer.
 *
 * A row's query has to match for a group like Activity's seats, which are one
 * path and eight queries. A row that carries NO query is compatible on path
 * alone — `activity/turns` is still where you are when `?seat=` narrows it, and
 * demanding an exact query there would leave a reader inside a filtered log with
 * nothing in the tree marked at all.
 */
function compatible(row: SidebarRow, route: { path: string[]; query: URLSearchParams }): boolean {
  if (!samePath(row.path, route.path)) return false;
  for (const [key, value] of Object.entries(row.query ?? {})) {
    if (route.query.get(key) !== value) return false;
  }
  return true;
}

/**
 * THE row that is the page being read — one for the whole sidebar.
 *
 * WHY THIS IS THE SIDEBAR'S ANSWER AND NOT EACH ROW'S. "Exactly one row is
 * current" is a statement about the SET of rows, and a predicate a row evaluates
 * about itself cannot make it: every compatible row answers yes, and the frame
 * spends the accent and writes `aria-current="page"` once per yes. More than one
 * section holds the same destination BY DESIGN — a project the reader starred is
 * under Starred and under Projects, a goal they opened is under Recent and under
 * Goals, and Activity's `Turns` row sits above eight seat rows that are the same
 * path with a query.
 *
 * DEDUPING THE SECTIONS IS THE OBVIOUS ALTERNATIVE AND IT IS WRONG. A star is a
 * shortcut somebody chose precisely because the object is otherwise one row of
 * two hundred in the tree; dropping it from Starred because the tree also lists
 * it removes the only reason to keep one. The repetition is the feature — what
 * has to be singular is the MARK.
 *
 * THE NARROWEST COMPATIBLE ROW WINS, and `>` rather than `>=` is the tie-break:
 * the first row in reading order keeps it. A row with no query still wins where
 * nothing narrower is compatible — a `?seat=` for a handle the roster no longer
 * has leaves `Turns` marked rather than leaving the tree with nothing marked at
 * all. Order decides a tie because a workspace's own tree is drawn above the
 * sections built from what this reader kept: the tree row is the address,
 * Starred and Recent are shortcuts to it.
 */
function currentRowOf(
  sections: SidebarSection[],
  route: { path: string[]; query: URLSearchParams },
): SidebarRow | null {
  let best: SidebarRow | null = null;
  let narrowest = -1;
  const walk = (rows: SidebarRow[]): void => {
    for (const row of rows) {
      const narrowness = Object.keys(row.query ?? {}).length;
      if (narrowness > narrowest && compatible(row, route)) {
        best = row;
        narrowest = narrowness;
      }
      walk(row.children ?? []);
    }
  };
  for (const section of sections) walk(section.rows);
  return best;
}

function Row({
  row,
  depth,
  filter,
  current,
}: {
  row: SidebarRow;
  depth: number;
  filter: string;
  /**
   * The one row this sidebar marks, from [currentRowOf]. Compared by IDENTITY
   * rather than by `key`: a key is unique only within the section that built it,
   * and two sections naming one destination is the case this exists for.
   */
  current: SidebarRow | null;
}) {
  const route = useRoute();
  const here = row === current;
  const onPath = containsRoute(row, route.path);
  const hasChildren = (row.children ?? []).length > 0;
  // OPEN BECAUSE THE READER IS INSIDE IT, or because they searched into it, or
  // because they opened it. A tree that collapsed the branch you are standing
  // on is a tree that loses you on every navigation.
  //
  // WHY THE HAND STATE IS FORGOTTEN RATHER THAN CONSULTED FIRST. `openedByHand
  // ?? forced` alone made one collapse permanent: `??` falls through on null
  // and a twist writes a boolean, so neither force-open could ever apply
  // again. Collapse a unit, then type a filter its child matches — the
  // parent row stays (it matches THROUGH its children) and the child renders
  // nowhere, which is indistinguishable from a search that found nothing.
  // Navigate into the branch and it is worse: `onPath` is true, the branch is
  // shut, and the row marked `current` is not drawn at all.
  //
  // So the hand state is cleared whenever the REASON to be open changes —
  // which page the reader is on, when it is inside this branch, or what they
  // have typed. The path rather than the whole hash, because opening a peek
  // changes the query and must not reopen a branch somebody folded away.
  // Adjusted during render rather than in an effect, so the row is never
  // painted once in the state the reader already left.
  const forcedBy = onPath ? route.path.join("/") : filter !== "" ? `?${filter}` : "";
  const [openedByHand, setOpenedByHand] = useState<boolean | null>(null);
  const [lastForcedBy, setLastForcedBy] = useState(forcedBy);
  if (lastForcedBy !== forcedBy) {
    setLastForcedBy(forcedBy);
    if (forcedBy !== "") setOpenedByHand(null);
  }
  const open = openedByHand ?? forcedBy !== "";

  return (
    <>
      <div className={cx("side-row", here && "current")} style={{ "--depth": depth } as never}>
        {hasChildren ? (
          // OURS RATHER THAN `IconButton`: the twist is a 16px slot in a tree
          // row, and `IconButton`'s smallest step is a 24px square — the WCAG
          // 2.2 pointer floor, which is right for a row action and would make
          // every row in every workspace tree eight pixels taller here. The
          // chevrons inside it are theirs.
          <button
            className="side-twist"
            onClick={() => setOpenedByHand(!open)}
            aria-label={open ? `Collapse ${row.label}` : `Expand ${row.label}`}
            aria-expanded={open}
          >
            {open ? <KeyboardArrowDownGlyph size="xs" /> : <ChevronRightGlyph size="xs" />}
          </button>
        ) : (
          <span className="side-twist" aria-hidden="true" />
        )}
        <a
          className="side-link"
          href={href(row.path, row.query)}
          aria-current={here ? "page" : undefined}
        >
          {row.icon && <RowGlyph name={row.icon} />}
          {/* THEIRS, and it is the same 6px mark in the same tones — with the
              neutral one measured: ours drew it on `--text-faint`, which is
              2.33:1 against a light page, where `StatusDot` takes the tertiary
              step at 4.87:1. It hides itself from assistive technology, so the
              `aria-hidden` that used to be spelled here is theirs now. */}
          {row.tone && <StatusDot tone={uiletTone(row.tone)} />}
          <span className="col" style={{ gap: 0, minWidth: 0, flex: 1 }}>
            <span className="truncate">{row.label}</span>
            {row.sub && <span className="side-sub truncate">{row.sub}</span>}
          </span>
          {/* `!== undefined`, NOT a truthy test. `count` holds a record now, so
              `row.count &&` is safe — but it reads exactly like the mistake
              `app/source.test.ts` exists to catch (`{rows.length && …}` renders
              a bare "0"), and the gate flags it on the field's NAME because no
              text scan can see a type. A reader has the same doubt the gate
              does, and this is what answers both. */}
          {row.count !== undefined && (
            <span className="side-count t-num" title={row.count.of}>
              {row.count.value}
            </span>
          )}
        </a>
      </div>
      {open &&
        (row.children ?? [])
          .filter((child) => matches(child, filter))
          .map((child) => (
            <Row key={child.key} row={child} depth={depth + 1} filter={filter} current={current} />
          ))}
    </>
  );
}

export function WorkspaceSidebar({
  title,
  sections,
  open,
  onClose,
  /** Rendered above the sections — a workspace's own summary line. */
  head,
}: {
  title: string;
  sections: SidebarSection[];
  /** Drawer state, for the narrow layout. */
  open?: boolean;
  onClose?: () => void;
  head?: ReactNode;
}) {
  const route = useRoute();
  const [filter, setFilter] = useState("");
  const needle = filter.trim().toLowerCase();
  const shown = useMemo(
    () =>
      sections
        .map((section) => ({
          ...section,
          rows: section.rows.filter((row) => matches(row, needle)),
        }))
        // A SECTION WITH NOTHING LEFT DISAPPEARS WHILE FILTERING, and comes
        // back when the filter clears: a heading over nothing reads as a
        // section that is broken rather than as one that does not match.
        .filter((section) => section.rows.length > 0 || (!needle && section.empty)),
    [sections, needle],
  );
  // OVER WHAT IS OFFERED rather than over everything this sidebar was handed: a
  // filter that hides the tree's row leaves the reader looking at the copy under
  // Recent, and a mark on a row nothing draws marks nothing at all.
  const current = useMemo(() => currentRowOf(shown, route), [shown, route]);

  return (
    <>
      {open && <div className="drawer-veil" onClick={onClose} />}
      <aside className="workspace-side" data-open={open} aria-label={`${title} navigation`}>
        <div className="side-head">
          <span className="side-title">{title}</span>
        </div>
        {head}
        <div className="side-filter">
          <Input
            type="search"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            aria-label={`Filter ${title}`}
            placeholder="Filter"
            inputSize="sm"
            leading={<SearchGlyph size="sm" />}
          />
        </div>
        <div className="side-tree">
          {shown.map((section) => (
            <div key={section.key} className="side-section">
              {section.label && <div className="side-section-label">{section.label}</div>}
              {section.rows.length > 0
                ? section.rows.map((row) => (
                    <Row key={row.key} row={row} depth={0} filter={needle} current={current} />
                  ))
                : section.empty && <div className="side-empty">{section.empty}</div>}
            </div>
          ))}
          {shown.length === 0 && <div className="side-empty">Nothing here matches “{filter}”.</div>}
        </div>
      </aside>
    </>
  );
}
