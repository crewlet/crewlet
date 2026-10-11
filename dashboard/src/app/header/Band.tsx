/**
 * The band: the top arm of the L-shaped frame — where the reader is, and the
 * way to everything else.
 *
 * # The frame is an L, and the page is a panel inside it
 *
 * The sidebar's ground does not stop at the sidebar: it runs across the top
 * of the window as this band, so the chrome reads as ONE frame and the page
 * sits inside it as a rounded panel. The frame is where you are and how you
 * move; the panel is the thing you are working on. That is why the trail and
 * search live here and a screen's own controls do not: they are about the
 * page, and the page is the panel's (`PageHeader`, at the panel's top).
 *
 * # One `h1`, and it is the trail's last crumb
 *
 * The band is the page's banner (the kit's top bar is a `header` outside
 * `main`), and the trail's last crumb is the page's heading, as it was when
 * the trail sat in the sheet's own bar. The panel draws no second title over
 * it: a heading a line under the trail saying the trail's last word again is
 * two level-one headings to a screen reader, one repeating the other. An
 * object page's own title block (`ObjectHeader`) is a level two inside the
 * screen, as it always was.
 *
 * # The console wears its name, and its way back
 *
 * In the console the trail starts "Admin ›" — a label rather than a crumb,
 * because the console's own first screen is its first crumb and a link to the
 * page the reader is on would be a crumb that goes nowhere — and the band
 * carries "Open in {company} ↗", the person twin of what is open
 * (`personPathOf`), beside search. The person shell's band carries search and
 * nothing else: an admin crosses from it through the sidebar's door, `g .`
 * and the Admin group at the end of an object's ⋯ menu.
 *
 * Below the shell breakpoint the band is the sheet's own top bar, as the kit
 * draws it — the drawer toggle, the trail, and search folded to its glyph —
 * because there is no frame beside the sheet for it to stand on.
 */

import { AppShell, Kbd, SearchTrigger } from "@crewlethq/ui";
import { ArrowUpRightGlyph } from "@crewlethq/icons/glyphs";
import { href, useRoute } from "../router.tsx";
import { personPathOf, type Shell } from "../nav.ts";
import type { Crumb } from "../crumbs.ts";
import { capsOf, keyRow } from "../keymap.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { Breadcrumb } from "./PageHeader.tsx";

export function Band({
  crumbs,
  shell,
  onSearch,
}: {
  crumbs: Crumb[];
  shell: Shell;
  onSearch: () => void;
}) {
  const org = useOrg();
  const company = org?.name || "Crewlet";
  const console = shell === "admin";
  return (
    <AppShell.Topbar className="frame-band">
      <Breadcrumb crumbs={crumbs} prefix={console ? "Admin" : undefined} />
      <div className="band-end">
        {console && <OpenInPerson company={company} />}
        <SearchTrigger
          variant="toolbar"
          className="band-search"
          label={console ? `Search ${company} or jump to…` : `Search ${company}`}
          title="Search everything"
          onClick={onSearch}
          keyshortcuts="Meta+K Control+K"
          shortcut={<Kbd keys={capsOf(keyRow("palette").presses[0]!)} subtle />}
        />
      </div>
    </AppShell.Topbar>
  );
}

/**
 * The console's way to the person twin of what is open — the same crossing
 * the sidebar's door back and `g .` make, so a console object names where it
 * lives for a person. Quiet, because it is a way out rather than the page's
 * business.
 */
function OpenInPerson({ company }: { company: string }) {
  const route = useRoute();
  const twin = personPathOf(route.path, route.query);
  return (
    <a className="band-cross" href={href(twin.path, twin.query)}>
      Open in {company}
      <ArrowUpRightGlyph size="sm" aria-hidden="true" />
    </a>
  );
}
