/**
 * The global rail: the workspaces, and nothing else.
 *
 * 80 px (`--rail-w`, which is where the width is actually set), an icon over
 * an 11 px label, always legible without hover. A rail of
 * bare icons is a rail whose rows have to be learnt, and the thing being
 * learnt is the product's own vocabulary — so the label is not an affordance
 * that can be traded for width.
 *
 * COLLAPSING IS PER-VIEWER AND LOCAL. It writes `localStorage` under one key,
 * in a try/catch, and defaults to expanded: the accessor throws in a private
 * window and returns nothing on a cleared profile, and a rail that failed to
 * render because a preference could not be read would be the worst possible
 * trade for a preference.
 */

import { useCallback, useState } from "react";
import { RAIL, type Workspace } from "../nav.ts";
import { href } from "../router.tsx";
import { cx } from "@crewlethq/ui";
import { ChevronLeftGlyph, ChevronRightGlyph, KeyGlyph } from "@crewlethq/icons/glyphs";
// A ROW'S MARK IS DATA, so it arrives as a NAME and is resolved here — which
// is the one case `@crewlethq/icons/glyphs/registry` exists for, and why it is
// a separate entry point: importing it pulls in every drawing, and the three
// literal glyphs above cost only themselves.
import { markByName } from "~/ui/glyph.tsx";
import { useKeyChords } from "~/lib/keys.ts";

const COLLAPSE_KEY = "crewlet.rail.collapsed";

/** Read the collapsed preference, or false when storage will not answer. */
function storedCollapsed(): boolean {
  try {
    return localStorage.getItem(COLLAPSE_KEY) === "1";
  } catch {
    return false;
  }
}

export function useRailCollapsed(): [boolean, () => void] {
  const [collapsed, setCollapsed] = useState(storedCollapsed);
  const toggle = useCallback(() => {
    setCollapsed((was) => {
      const next = !was;
      try {
        localStorage.setItem(COLLAPSE_KEY, next ? "1" : "0");
      } catch {
        // A preference that cannot be saved is still a preference for this
        // session — the rail collapses either way.
      }
      return next;
    });
  }, []);
  return [collapsed, toggle];
}

export interface RailBadge {
  /** The number or word on the pip. */
  text: string;
  /** The ONE badge in the chrome allowed a status hue. */
  attention?: boolean;
  /** What the count is over, said out loud where it is not a total. */
  title?: string;
}

export function AppRail({
  active,
  badges,
  collapsed,
  onToggle,
  footer,
  locked,
}: {
  active: Workspace | "";
  /** Per-workspace badge, keyed on the rail row. */
  badges?: Partial<Record<Workspace, RailBadge | null>>;
  collapsed: boolean;
  onToggle: () => void;
  /** The engine pill, theme and density — composed by the shell. */
  footer?: React.ReactNode;
  /** Whether the guarded rows draw their lock. */
  locked?: boolean;
}) {
  return (
    <nav className="rail" data-collapsed={collapsed || undefined} aria-label="Workspaces">
      <a className="rail-brand" href={href(["inbox"])} title="Crewlet">
        <img src="/static/crewlet-icon.svg" alt="Crewlet" />
      </a>

      <div className="rail-rows">
        {RAIL.map((row) => {
          const badge = badges?.[row.key] ?? null;
          const Glyph = markByName(row.icon);
          return (
            <a
              key={row.key}
              className={cx("rail-row", active === row.key && "active")}
              href={href(row.path)}
              aria-current={active === row.key ? "page" : undefined}
              title={collapsed ? `${row.label} — ${row.hint}` : row.hint}
            >
              <span className="rail-glyph">
                <Glyph size="md" />
                {badge && (
                  <span
                    className={cx("rail-badge", badge.attention && "attention")}
                    title={badge.title}
                  >
                    {badge.text}
                  </span>
                )}
                {/* THE LOCK IS ON THE ROW, not instead of it. */}
                {row.guarded && locked && (
                  <span className="rail-lock" title="needs an operator credential">
                    <KeyGlyph size="xs" />
                  </span>
                )}
              </span>
              {!collapsed && <span className="rail-label">{row.label}</span>}
            </a>
          );
        })}
      </div>

      <div className="rail-foot">
        {footer}
        {/* OURS RATHER THAN `IconButton`: this is not a square. It is a
            full-width 22px strip across the foot of an 80px rail, and
            `IconButton` is a control-step square whose smallest size floors at
            24px for WCAG 2.2 — which is the right floor for a row action and
            the wrong shape for a rail's own hinge. The glyph inside it is
            theirs. */}
        <button
          className="rail-collapse"
          onClick={onToggle}
          title={collapsed ? "Expand the rail ([)" : "Collapse the rail ([)"}
          aria-label={collapsed ? "Expand the rail" : "Collapse the rail"}
        >
          {collapsed ? <ChevronRightGlyph size="sm" /> : <ChevronLeftGlyph size="sm" />}
        </button>
      </div>
    </nav>
  );
}

/**
 * `g` then a letter jumps to a workspace.
 *
 * A CHORD RATHER THAN A MODIFIER, because every single-modifier combination
 * worth having is already the browser's. The prefix times out after a second
 * so a stray `g` does not swallow the next key the reader meant for a field.
 */
export function useWorkspaceChords(go: (path: string[]) => void): void {
  useKeyChords(RAIL.map((row) => ({ after: "g", key: row.chord, run: () => go(row.path) })));
}
