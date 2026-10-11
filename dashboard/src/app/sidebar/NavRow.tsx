/**
 * One row of either sidebar: a plain anchor, drawn by the kit's own row.
 *
 * SHARED BY BOTH SHELLS' SIDEBARS, so a row is one element whichever side of
 * the door it is drawn on: its figure, its prefetch and its link are the same
 * rule in the person sidebar and in the console's (`console/ConsoleSidebar`).
 */

import type { ReactNode } from "react";
import { SidebarNav } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import type { WorkspaceRow } from "~/app/nav.ts";
import { glyphFor } from "~/ui/glyph.tsx";
import { preload } from "../lazyScreen.ts";

export interface NavRowProps {
  label: ReactNode;
  icon?: WorkspaceRow["icon"];
  glyph?: ReactNode;
  lead?: string;
  path: string[];
  /** The address's query, for a row that is a question rather than a place. */
  query?: Record<string, string>;
  current: boolean;
  badge?: { value: number | string; label: string };
  count?: { value: number | string; label: string; mark?: ReactNode };
  /** A tooltip naming what the row's figure is made of. */
  title?: string;
  /** Nested rows, and the control that opens them — the Work row's projects. */
  children?: ReactNode;
  expanded?: boolean;
  onExpandedChange?: (expanded: boolean) => void;
  expandLabel?: string;
  className?: string;
}

/**
 * One row, a plain anchor: a hash link is a real link, so ⌘-click opens a tab.
 *
 * HOVER OR FOCUS STARTS FETCHING WHERE IT LEADS (`lazyScreen.ts`'s
 * `preload`), so the chunk is usually in by the time the click lands — the
 * pointer's hover and the keyboard's focus are both the reader deciding.
 */
export function NavRow({
  label,
  icon,
  glyph,
  lead,
  path,
  query,
  current,
  badge,
  count,
  title,
  children,
  expanded,
  onExpandedChange,
  expandLabel,
  className,
}: NavRowProps) {
  const Glyph = icon ? glyphFor(icon) : undefined;
  const warm = () => preload(path);
  return (
    <SidebarNav.Item
      label={label}
      icon={glyph ?? (Glyph ? <Glyph size="sm" /> : undefined)}
      lead={lead}
      href={href(path, query)}
      current={current}
      badge={badge}
      count={count}
      expanded={expanded}
      onExpandedChange={onExpandedChange}
      expandLabel={expandLabel}
      className={className}
      // THE KIT'S OWN ELEMENT, with two listeners added: its class, its
      // `href` and its `aria-current` pass through untouched, so the row is
      // drawn exactly as the kit draws it.
      renderLink={({ className: kit, ...link }) => (
        <a className={kit} {...link} title={title} onPointerEnter={warm} onFocus={warm} />
      )}
    >
      {children}
    </SidebarNav.Item>
  );
}
