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
 * So selection compares BOTH, and a row with no query still matches a route
 * that carries one — `activity/turns` stays current when a seat narrows it,
 * because the seat row is the narrower answer and both are true.
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
import { Icon, type IconName } from "~/ui/Icon.tsx";
import { SearchInput, cx, type Tone } from "~/ui/primitives.tsx";

export interface SidebarRow {
  key: string;
  label: string;
  /** The destination. A row without one is not a row. */
  path: string[];
  /** Query carried with the path — allowed only where the ROW IS the list. */
  query?: Record<string, string>;
  icon?: IconName;
  /** A state mark, never an identity colour. */
  tone?: Tone;
  count?: number;
  /** What the count is over, when it is not a maintained total. */
  countTitle?: string;
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
 * Whether this row IS the page being read.
 *
 * A row's query has to match for a group like Activity's seats, which are one
 * path and eight queries. A row that carries NO query matches on path alone —
 * `activity/turns` is still where you are when `?seat=` narrows it, and
 * demanding an exact query there would leave a reader inside a filtered log
 * with nothing in the tree marked at all.
 */
function isCurrent(row: SidebarRow, route: { path: string[]; query: URLSearchParams }): boolean {
  if (!samePath(row.path, route.path)) return false;
  for (const [key, value] of Object.entries(row.query ?? {})) {
    if (route.query.get(key) !== value) return false;
  }
  return true;
}

function Row({ row, depth, filter }: { row: SidebarRow; depth: number; filter: string }) {
  const route = useRoute();
  const current = isCurrent(row, route);
  const onPath = containsRoute(row, route.path);
  const hasChildren = (row.children ?? []).length > 0;
  // OPEN BECAUSE THE READER IS INSIDE IT, or because they searched into it, or
  // because they opened it. A tree that collapsed the branch you are standing
  // on is a tree that loses you on every navigation.
  //
  // WHY THE HAND STATE IS FORGOTTEN RATHER THAN CONSULTED FIRST. `openedByHand
  // ?? forced` alone made one collapse permanent: `??` falls through on null
  // and a twist writes a boolean, so neither force-open could ever apply
  // again. Collapse a project, then type a filter its sprint matches — the
  // project row stays (it matches THROUGH its children) and the sprint renders
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
      <div className={cx("side-row", current && "current")} style={{ "--depth": depth } as never}>
        {hasChildren ? (
          <button
            className="side-twist"
            onClick={() => setOpenedByHand(!open)}
            aria-label={open ? `Collapse ${row.label}` : `Expand ${row.label}`}
            aria-expanded={open}
          >
            <Icon name={open ? "chevronDown" : "chevronRight"} size="xs" />
          </button>
        ) : (
          <span className="side-twist" aria-hidden="true" />
        )}
        <a
          className="side-link"
          href={href(row.path, row.query)}
          aria-current={current ? "page" : undefined}
        >
          {row.icon && <Icon name={row.icon} size="sm" />}
          {row.tone && <i className={cx("side-dot", row.tone)} aria-hidden="true" />}
          <span className="col" style={{ gap: 0, minWidth: 0, flex: 1 }}>
            <span className="truncate">{row.label}</span>
            {row.sub && <span className="side-sub truncate">{row.sub}</span>}
          </span>
          {row.count != null && (
            <span className="side-count t-num" title={row.countTitle}>
              {row.count}
            </span>
          )}
        </a>
      </div>
      {open &&
        (row.children ?? [])
          .filter((child) => matches(child, filter))
          .map((child) => <Row key={child.key} row={child} depth={depth + 1} filter={filter} />)}
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

  return (
    <>
      {open && <div className="drawer-veil" onClick={onClose} />}
      <aside className="workspace-side" data-open={open} aria-label={`${title} navigation`}>
        <div className="side-head">
          <span className="side-title">{title}</span>
        </div>
        {head}
        <div className="side-filter">
          <SearchInput
            value={filter}
            onChange={setFilter}
            ariaLabel={`Filter ${title}`}
            placeholder="Filter"
          />
        </div>
        <div className="side-tree">
          {shown.map((section) => (
            <div key={section.key} className="side-section">
              {section.label && <div className="side-section-label">{section.label}</div>}
              {section.rows.length > 0
                ? section.rows.map((row) => (
                    <Row key={row.key} row={row} depth={0} filter={needle} />
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
