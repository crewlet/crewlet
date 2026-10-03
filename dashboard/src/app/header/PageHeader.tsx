/**
 * The page header: where the reader is, what they can do here, and the
 * sections of the workspace they are in.
 *
 * # Two rows, one banner
 *
 * The first row is the kit's top bar — the drawer toggle below the shell
 * breakpoint, the breadcrumb, an object page's lenses beside it (`PageLenses`),
 * and what this page can do: who is working right now (Home's bar only), the
 * star and the link, and LAST the screen's own controls (portalled in through
 * `PageActions`) — because a screen's primary action ("New task") belongs at
 * the bar's end, where the approved designs put it and where the eye finishes
 * the row; the frame's quiet controls come before it. The second row is the
 * workspace's SECTIONS, drawn
 * as tabs, on a section's own page only: on an object page the crumbs are the
 * way back out, and a strip of tabs over a task page would say the task is a
 * fourth kind of list.
 *
 * Settings draws its sections as a COLUMN instead (`SectionColumn`), because
 * it has eight of its own and a cross-link to Budgets, in three groups, and a
 * tab strip of nine is a row nobody reads to the end of.
 *
 * # A section is a path
 *
 * Every tab is a link to its own address, never a `tab=` on this one: a
 * section is a place, and a place has a URL a reader can paste. What travels
 * between sections is the workspace's own query (`WorkspaceRow.keep` — whose
 * day My work is reading), and nothing else: a filter on the Turns list is not
 * a filter on the Event log.
 *
 * # A figure on a tab is the screen's
 *
 * A section's count is published by the screen that has it (`useSectionCounts`
 * in `Shell.tsx`), because only that screen knows whether the number is a
 * total, a floor or not yet known; the header draws what it is given and draws
 * nothing for a section the screen did not count.
 */

import {
  useCallback,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type CSSProperties,
  type ReactNode,
  type RefObject,
} from "react";
import {
  AppShell,
  AvatarStack,
  Button,
  Menu,
  SidebarNav,
  cx,
  useToast,
  type MenuEntry,
} from "@crewlethq/ui";
import {
  ArrowUpRightGlyph,
  ChevronDownGlyph,
  ChevronRightGlyph,
  CopyGlyph,
  EllipsisGlyph,
  KeyGlyph,
  StarGlyph,
} from "@crewlethq/icons/glyphs";
import { href, samePath, useNavigator, useRoute } from "../router.tsx";
import type { Crumb } from "../crumbs.ts";
import { resolves } from "../routes.ts";
import { grantWords, grantsOpen, sectionOf, type Section, type WorkspaceRow } from "../nav.ts";
import type { SectionFigure } from "../sidebar/settingsFigures.tsx";
import { PAGE_ACTIONS_SLOT, PAGE_LENSES_SLOT } from "../frame/PageActions.tsx";
import { glyphFor } from "~/ui/glyph.tsx";
import { SeatAvatar, seatBadge } from "~/ui/SeatAvatar.tsx";
import { useReader } from "~/lib/reader.ts";
import { MaxStars, starredIn, useStarred, useToggleStar, type Star } from "~/lib/starred.ts";
import { useAgents } from "~/lib/store-hooks.ts";
import { plural } from "~/lib/format.ts";
import { useScrollEdges } from "~/lib/useScrollEdges.ts";
import { foldTabs } from "../frame/tabFit.ts";

/** How many working seats the header draws before "+n". */
const WORKING_FACES = 4;

/** One action a screen folds into the page bar's "More" menu on a phone. */
export interface PageMenuEntry {
  key: string;
  label: string;
  onSelect: () => void;
  disabled?: boolean;
  /** Why it is disabled, or what the label alone does not say. */
  description?: string;
  /** The glyph the inline control wears, so the menu reads the same. */
  icon?: ReactNode;
}

export function PageHeader({
  crumbs,
  row,
  counts,
  title,
  workspace,
  working = false,
  menu = [],
  children,
}: {
  crumbs: Crumb[];
  /** The workspace the reader is in, or undefined on a route nothing owns. */
  row: WorkspaceRow | undefined;
  /** Each section's figure, as the screen published it. */
  counts: Record<string, string>;
  /** The page's own name, for the star. */
  title: string;
  workspace: string;
  /** Whether this page draws who is working — Home's only (`useWorkingNow`). */
  working?: boolean;
  /** The screen's secondary actions, folded into "More" on a phone. */
  menu?: readonly PageMenuEntry[];
  /** What sits under the rows: the state bar. */
  children?: ReactNode;
}) {
  const route = useRoute();
  const controls = useRef<HTMLDivElement>(null);
  const edges = useScrollEdges(controls);
  return (
    <div className="page-head">
      <AppShell.Topbar className="page-bar">
        <Breadcrumb crumbs={crumbs} />
        {/* AN OBJECT'S LENSES, beside the name they are lenses on — see
            `PageLenses`. Empty on every other page, where it takes no room
            (`.page-lenses:empty`). */}
        <div className="page-lenses" id={PAGE_LENSES_SLOT} />
        <div className="page-controls" ref={controls} {...edges}>
          {/* THE SLOT IS ALWAYS RENDERED, whether or not a screen has
              controls: a portal needs a node to land in, and one that appears
              only when the frame already knows there are controls could never
              be found by the screen that has them. The gap is the bar's own,
              so a screen's controls sit as far apart as the frame's. */}
          {working && <WorkingNow />}
          {/* THE FRAME'S OWN QUIET PAIR, drawn inline on a wide bar and folded
              into "More" on a phone (see `PageMore`), where the bar keeps one
              action in view. */}
          <span className="page-frame-actions">
            <StarPage path={route.path} label={title} workspace={workspace} />
            <CopyLink />
          </span>
          {/* BEFORE THE SLOT IN THE DOCUMENT, so the screen's controls stay the
              bar's last element on a wide bar (where this is not drawn); on a
              phone the slot leads the line (`order: -1`) and this ends it. */}
          <PageMore path={route.path} label={title} workspace={workspace} entries={menu} />
          <div className="row gap-2 wrap page-actions" id={PAGE_ACTIONS_SLOT} />
        </div>
      </AppShell.Topbar>
      {row && row.renderer === "tabs" && (
        <SectionTabs row={row} path={route.path} query={route.query} counts={counts} />
      )}
      {children}
    </div>
  );
}

/**
 * Who is working right now: their faces, and how many — the company's pulse,
 * one click from what they are running. Drawn on Home only (`useWorkingNow`):
 * everywhere else the sidebar's Agents badge carries the same count.
 *
 * THE ENGINE'S WORD, off the agents push. Nothing is drawn while nobody is
 * working: a header reading "0 agents working" is a line a reader learns to
 * skip, and then skips on the day it says something.
 */
export function WorkingNow() {
  const agents = useAgents();
  const working = agents.filter((a) => a.activity === "working");
  if (working.length === 0) return null;
  return (
    <a className="working-now" href={href(["live"])}>
      <AvatarStack
        size="xs"
        max={WORKING_FACES}
        decorative
        // THE SEAT'S OWN BADGE, the initials its crumb and its profile draw
        // (`seatBadge`): handed the role as written, "Agent SWE" was `AS`
        // here beside `SW` everywhere else the seat is drawn.
        members={working.map((a) => ({
          id: a.id,
          ...seatBadge(a.role, "agent"),
          ring: "info" as const,
        }))}
      />
      <span>{plural(working.length, "agent")} working</span>
    </a>
  );
}

/**
 * The workspace's sections, as tabs that are links.
 *
 * A NAVIGATION, NOT A TABLIST. Each tab goes to an address, so it is an anchor
 * inside a labelled `nav` with `aria-current` on the section the reader is
 * on; a `tablist` promises panels on this page and arrow keys between them,
 * which a set of addresses cannot keep. Drawn only on a section's own page and
 * only for a workspace with more than one section, because a strip of one tab
 * is a heading wearing a control's clothes.
 *
 * # What does not fit goes into "More"
 *
 * A strip wider than the header used to scroll, with its bar hidden and a
 * fade at the edge — and a fade is too quiet to read as "there is more": at
 * 1280 My work cut "Checklist 0" at the edge and nothing said a section was
 * past it. So the strip draws the tabs that FIT, in order, and folds the rest
 * into a "More" menu at its end, each entry with its figure. The tab the
 * reader is on is always drawn: it takes the last place that fits rather than
 * disappearing into the menu, because "where am I" is the one question the
 * strip must answer at every width. Nothing is dropped and nothing moves: the
 * order is the workspace's, and a tab leaves the strip only from the end.
 *
 * MEASURED, NOT GUESSED. The widths are the tabs' own, read after layout
 * ([useTabFit]) — a folded tab stays in the document, out of the flow and
 * invisible, so it can be measured again when the strip grows — and they are
 * read again whenever the strip or any tab changes size: a figure arriving, a
 * font finishing loading, the density preference.
 */
export function SectionTabs({
  row,
  path,
  query,
  counts,
}: {
  row: WorkspaceRow;
  path: string[];
  query: URLSearchParams;
  counts: Record<string, string>;
}) {
  const nav = useNavigator();
  const tabs = row.sections.filter((s) => s.tab !== false && !s.elsewhere);
  const current = tabs.find((s) => samePath(s.path, path));
  const strip = useRef<HTMLElement>(null);
  const fit = useTabFit(
    strip,
    tabs.map((s) => s.key),
    current?.key,
  );
  const folded = fit.folded;
  if (tabs.length < 2 || !current) return null;
  const kept: Record<string, string> = {};
  for (const key of row.keep ?? []) {
    const value = query.get(key);
    if (value) kept[key] = value;
  }
  const out = tabs.filter((s) => folded.includes(s.key));
  return (
    <nav ref={strip} className="section-tabs" aria-label={`${row.label} sections`}>
      {tabs.map((s) => {
        const Glyph = glyphFor(s.icon);
        const figure = counts[s.key];
        const off = folded.includes(s.key);
        return (
          <a
            key={s.key}
            className="section-tab"
            data-section={s.key}
            href={href(s.path, kept)}
            aria-current={s === current ? "page" : undefined}
            // FOLDED INTO "MORE": still here to be measured, and out of reach
            // of the pointer, the keyboard and a screen reader, which meet it
            // in the menu instead.
            {...(off ? { "data-folded": "", "aria-hidden": true, inert: true } : {})}
          >
            <Glyph size="sm" aria-hidden="true" />
            <span>{s.label}</span>
            {/* A PLAIN FIGURE in the tab's own quiet ink, as every approved
                artboard draws it: a pill here was a second control-shaped
                thing inside each tab. */}
            {figure ? <span className="section-tab-count t-num">{figure}</span> : null}
          </a>
        );
      })}
      {/* ALWAYS IN THE DOCUMENT, so its width is known before it is needed;
          folded itself while every tab fits. */}
      <span
        className="section-tabs-more"
        data-section-more=""
        {...(out.length === 0 ? { "data-folded": "", "aria-hidden": true, inert: true } : {})}
      >
        <Menu
          label={`More sections of ${row.label}`}
          // THE CHEVRON SAYS IT OPENS, as a select's does: a bare "More" at
          // the end of a row of links read as one more link.
          trigger={
            <span className="row gap-1">
              More
              <ChevronDownGlyph size="xs" aria-hidden="true" />
            </span>
          }
          triggerVariant="ghost"
          align="end"
          // NO GLYPHS: a section's glyph at the head of a menu row sits where
          // a menu draws its check, and Checklist's IS a check — the row read
          // as a chosen answer. The name and its figure are the row.
          items={out.map((s) => ({
            key: s.key,
            label: s.label,
            hint: counts[s.key] || undefined,
            onSelect: () => nav.to(s.path, kept),
          }))}
        />
      </span>
      {/* THE WORKSPACE'S READING NOTE comes after the tabs and gives way to
          them: it is folded (out of the flow, still measured) whenever the
          strip has no room for it beside every tab — see [noteFits]. */}
      {row.note && (
        <span
          className="section-tabs-note"
          data-section-note=""
          {...(fit.note ? {} : { "data-folded": "", "aria-hidden": true })}
        >
          {row.note}
        </span>
      )}
    </nav>
  );
}

/**
 * Whether the workspace's reading note has room at the strip's end BESIDE
 * EVERY TAB.
 *
 * THE TABS COME FIRST. The note is a sentence about how to read the
 * workspace; a tab is a place. So the note takes no room a tab would need: it
 * is drawn only where every tab fits with it, and gives way before a single
 * tab folds into "More". A note measured at no width is one the sheet is not
 * drawing (below the shell breakpoint), and has nothing to fit.
 */
export function noteFits({
  widths,
  space,
  gap,
  note,
}: {
  widths: readonly number[];
  space: number;
  gap: number;
  note: number;
}): boolean {
  if (note <= 0) return false;
  const tabs = widths.reduce((sum, w) => sum + w, 0) + gap * Math.max(0, widths.length - 1);
  return tabs + gap + note <= space;
}

/**
 * The keys of the tabs that fold into "More", and whether the reading note is
 * drawn, measured after layout and again whenever the strip or anything in it
 * changes size — see [SectionTabs].
 */
function useTabFit(
  strip: RefObject<HTMLElement | null>,
  keys: readonly string[],
  current: string | undefined,
): { folded: string[]; note: boolean } {
  const [folded, setFolded] = useState<string[]>([]);
  const [note, setNote] = useState(true);
  const signature = keys.join("\n");
  useLayoutEffect(() => {
    const node = strip.current;
    if (!node) return;
    const measure = () => {
      const style = getComputedStyle(node);
      const space =
        node.clientWidth -
        (parseFloat(style.paddingLeft) || 0) -
        (parseFloat(style.paddingRight) || 0);
      const gap = parseFloat(style.columnGap) || 0;
      const width = (el: Element | null) => (el ? el.getBoundingClientRect().width : 0);
      const widths = keys.map((key) => width(node.querySelector(`[data-section="${key}"]`)));
      const more = width(node.querySelector("[data-section-more]"));
      const next = foldTabs({
        widths,
        current: current ? keys.indexOf(current) : -1,
        space,
        gap,
        more,
      })
        .map((i) => keys[i]!)
        .filter(Boolean);
      setFolded((was) => (was.join("\n") === next.join("\n") ? was : next));
      setNote(
        noteFits({ widths, space, gap, note: width(node.querySelector("[data-section-note]")) }),
      );
    };
    measure();
    // THE STRIP AND EVERY ITEM IN IT: the strip for the window, each item for
    // what changes its own width without resizing the strip — a figure
    // arriving, a web font replacing its fallback, a density step.
    const watch = new ResizeObserver(measure);
    watch.observe(node);
    for (const item of node.querySelectorAll(
      "[data-section], [data-section-more], [data-section-note]",
    )) {
      watch.observe(item);
    }
    return () => watch.disconnect();
    // `keys` is read through its signature: a new array with the same tabs is
    // the same strip.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [strip, signature, current]);
  return { folded, note };
}

/**
 * Settings' sections, as a column of groups beside the screen.
 *
 * A SECTION THE READER HOLDS NONE OF THE GRANTS FOR DRAWS ITS LOCK AND STAYS,
 * for the reason the sidebar's Settings row does: a section that vanished for
 * a reader without its grant is one they cannot know exists. The lock names
 * the grant, because that is what the reader would ask somebody for — and it
 * is drawn only once the viewer has answered, since a lock on every section
 * while the first read is out claims a refusal nobody has made. A CROSS-LINK
 * (Budgets, which lives once, under Spend) draws an arrow, because pressing it
 * leaves Settings. A figure beside a section is handed in whole, in the
 * column's own two shapes (`settingsFigures.tsx` decides them and asks for
 * what they need), so the column itself asks nothing.
 */
export function SectionColumn({
  row,
  path,
  grants,
  figures,
}: {
  row: WorkspaceRow;
  path: string[];
  /** What the viewer holds, or null while nobody has said. */
  grants: readonly string[] | null;
  figures: Record<string, SectionFigure>;
}) {
  // AN ADDRESS THAT NAMES NO SCREEN IS IN NO SECTION. The landing's path is
  // the workspace root, which prefixes every Settings address, so without this
  // a mistyped `#/settings/general` drew "Not found" under a column (and a
  // phone picker) saying the reader was on General.
  const current = resolves(path) ? sectionOf(path) : undefined;
  const groups: { name: string; sections: Section[] }[] = [];
  for (const s of row.sections) {
    const name = s.group ?? "";
    const last = groups[groups.length - 1];
    if (last && last.name === name) last.sections.push(s);
    else groups.push({ name, sections: [s] });
  }
  // ON A PHONE THE COLUMN IS A PICKER NAMING THE SECTION. Stacked whole above
  // the section it took about 520px, so on Nodes and Secrets the section's own
  // content began below the fold on every visit. Below the phone breakpoint
  // the list is folded behind one row saying where the reader is, and it folds
  // again once they pick a section (the path moves); at every other width the
  // toggle is not drawn and the list always is (frame.css).
  const [open, setOpen] = useState(false);
  const [openedAt, setOpenedAt] = useState(path.join("/"));
  if (openedAt !== path.join("/")) {
    setOpenedAt(path.join("/"));
    setOpen(false);
  }
  const listID = useId();
  const CurrentGlyph = current ? glyphFor(current.icon) : undefined;
  // THE KIT'S OWN NAVIGATION LIST, which is what this column is: labelled
  // groups of rows, one of which is where the reader is — raised with a
  // hairline and no hue, the same mark the sidebar beside it uses, so "where
  // am I" reads one way in both columns.
  return (
    <div className="section-column section-picker" data-open={open || undefined}>
      <button
        type="button"
        className="section-picker-toggle"
        aria-expanded={open}
        aria-controls={listID}
        onClick={() => setOpen((was) => !was)}
      >
        {CurrentGlyph && <CurrentGlyph size="sm" aria-hidden="true" />}
        <span className="section-picker-name">
          {current ? (
            <>
              <span className="sr-only">{row.label} section: </span>
              {current.label}
            </>
          ) : (
            `${row.label} sections`
          )}
        </span>
        <ChevronDownGlyph size="xs" aria-hidden="true" className="section-picker-chevron" />
      </button>
      <div id={listID} className="section-picker-list">
        <SidebarNav label={`${row.label} sections`}>
          {groups.map((g) => (
            <SidebarNav.Group key={g.name} label={g.name}>
              {g.sections.map((s) => {
                const Glyph = glyphFor(s.icon);
                const locked =
                  grants !== null && s.grants !== undefined && !grantsOpen(s.grants, grants);
                const figure = figures[s.key];
                return (
                  <SidebarNav.Item
                    key={s.key}
                    href={href(s.path)}
                    current={!s.elsewhere && current?.key === s.key}
                    icon={<Glyph size="sm" />}
                    label={
                      <span className="section-column-label">
                        {s.label}
                        {locked && (
                          <>
                            <KeyGlyph size="xs" aria-hidden="true" />
                            <span className="sr-only">, needs {grantWords(s.grants ?? [])}</span>
                          </>
                        )}
                        {s.elsewhere && (
                          <>
                            <ArrowUpRightGlyph size="xs" aria-hidden="true" />
                            <span className="sr-only">, under {s.path[0]}</span>
                          </>
                        )}
                      </span>
                    }
                    badge={figure?.badge ?? figure?.attention}
                    count={figure?.count}
                    // A STATE WAITING ON THE READER is the kit's pill — its
                    // size, its place and its spoken figure — repainted in
                    // the warning tone (see `SectionFigure.attention`).
                    className={figure?.attention ? "section-row-attention" : undefined}
                  />
                );
              })}
            </SidebarNav.Group>
          ))}
        </SidebarNav>
      </div>
    </div>
  );
}

/**
 * The trail, and it is a SCROLLPORT — so while it overflows it is a tab stop.
 *
 * `.crumbs` scrolls sideways past its floor rather than clipping the ancestry
 * as a box (see frame.css), and its scrollbar is not drawn, because on a bar
 * this short it would sit on the baseline the trail is written on. Both of
 * those together are what make the tab stop load-bearing: a pointer can drag
 * a hidden scrollport and a trackpad can swipe it, but a keyboard reaches a
 * scroll container's arrow keys only once it can be FOCUSED, and there is no
 * other route to the overflowed crumbs — the links inside it are focusable
 * and scroll into view, but the deepest trail this route table builds
 * overflows on its ancestor SPANS, which are not links and take no focus of
 * their own.
 *
 * ONLY WHILE IT OVERFLOWS. A trail that fits — nearly every trail, at any
 * width the frame draws a sidebar beside — has nowhere for the arrow keys to
 * go, and a stop on it was a Tab press on every page that did nothing. So the
 * stop is measured (`scrollWidth > clientWidth`), again whenever the trail's
 * box or its crumbs change, and the `aria-label` is what makes it announce
 * itself as the breadcrumb rather than as a bare group when it is there.
 *
 * EACH LABEL IS ITS OWN BOX (`.crumb-text`). The part that takes the cut is a
 * flex container, and text directly inside one is an anonymous flex item that
 * `text-overflow` does not reach: the last crumb was clipped mid-word at the
 * edge with no ellipsis at all. A span is a real flex item, blockified, which
 * is the box an ellipsis applies to.
 *
 * ITS FLOOR IS THE WAY BACK OUT (`--crumb-floor`, [useCrumbFloor]): the
 * ancestors at their width and the last crumb at its stub. The bar breaks a
 * line when the trail's floor cannot sit beside the controls, and a floor of
 * 20ch alone let a seat's trail share a phone's line with its one button at
 * two hundred pixels — "Agents › Engineering · Co" cut mid-letter under
 * Message, the seat's own crumb scrolled out of sight. Measured, the floor is
 * what the trail must show, so a trail that cannot show it beside the
 * controls takes a line of its own, and it scrolls only when its ancestry
 * alone is wider than the whole bar.
 */
export function Breadcrumb({ crumbs }: { crumbs: Crumb[] }) {
  const trail = useRef<HTMLElement>(null);
  const overflows = useOverflows(trail, crumbs);
  const floor = useCrumbFloor(trail, crumbs);
  return (
    <nav
      ref={trail}
      className="crumbs"
      aria-label="Breadcrumb"
      tabIndex={overflows ? 0 : undefined}
      style={floor > 0 ? ({ "--crumb-floor": `${floor}px` } as CSSProperties) : undefined}
    >
      {crumbs.map((crumb, i) => {
        const last = i === crumbs.length - 1;
        const Glyph = crumb.icon ? glyphFor(crumb.icon) : undefined;
        const face = cx(crumb.mono && "mono");
        const words = (
          <>
            {Glyph && <Glyph size="sm" className="crumb-glyph" aria-hidden="true" />}
            {crumb.tag && <span className="project-key mono crumb-tag">{crumb.tag}</span>}
            {crumb.seat && (
              <SeatAvatar
                name={crumb.seat.name}
                kind={crumb.seat.kind}
                size="xs"
                decorative
                className="crumb-seat"
              />
            )}
            <span className="crumb-text">{crumb.label}</span>
          </>
        );
        return (
          <span key={`${crumb.label}-${i}`} className="crumb-part">
            {i > 0 && <ChevronRightGlyph size="xs" className="crumb-sep" aria-hidden="true" />}
            {crumb.path && !last ? (
              <a className={cx("crumb-link", face)} href={href(crumb.path, crumb.query)}>
                {words}
              </a>
            ) : last ? (
              // THE OBJECT IS THE PAGE'S HEADING. The bar is the page's banner
              // and outside `main`, and the name of the place the reader is at
              // is what a reader navigating by heading lands on first — so the
              // last crumb is the `h1`, and every screen's own headings start
              // at level 2 under it.
              <h1 className={cx("crumb-here", face)} aria-current="page">
                {words}
              </h1>
            ) : (
              <span className={cx("crumb-here", face)}>{words}</span>
            )}
          </span>
        );
      })}
    </nav>
  );
}

/**
 * The narrowest the trail can be and still show the way back out: where the
 * last crumb's name starts, measured from the trail's own start, plus the stub
 * that part keeps (its `min-width`) — or 0 before anything is laid out.
 *
 * NEVER MORE THAN THE ROOM BESIDE WHAT LEADS THE BAR. Below the shell
 * breakpoint the kit's drawer toggle opens the bar's first line, and a floor
 * wider than what the toggle leaves pushed the trail under it — a line holding
 * the toggle alone, then the trail, then the controls. Capped at that room, a
 * trail whose ancestry is wider still scrolls beside the toggle, as it always
 * has past its floor.
 *
 * STABLE UNDER ITS OWN EFFECT. The ancestors never shrink (`frame.css`), so
 * where the last part starts does not depend on how wide the trail is drawn,
 * and neither the bar's width nor the toggle's does either — so the floor it
 * sets cannot move what it was measured from. Measured again whenever a crumb
 * or the bar changes size: a screen publishing its object's name, a font
 * finishing loading, the window.
 */
function useCrumbFloor(el: RefObject<HTMLElement | null>, content: unknown): number {
  const [floor, setFloor] = useState(0);
  useLayoutEffect(() => {
    const node = el.current;
    if (!node) return;
    const bar = node.parentElement;
    const measure = () => {
      const last = node.querySelector(
        ".crumb-part:last-child > .crumb-here, .crumb-part:last-child > .crumb-link",
      );
      if (!last) return setFloor(0);
      const start = node.getBoundingClientRect().left - node.scrollLeft;
      const offset = last.getBoundingClientRect().left - start;
      const stub = parseFloat(getComputedStyle(last).minWidth) || 0;
      const wanted = Math.max(0, Math.ceil(offset + stub));
      setFloor(bar ? Math.min(wanted, roomBeside(bar, node)) : wanted);
    };
    measure();
    const watch = new ResizeObserver(measure);
    for (const part of node.querySelectorAll(".crumb-part")) watch.observe(part);
    if (bar) watch.observe(bar);
    return () => watch.disconnect();
  }, [el, content]);
  return floor;
}

/**
 * How wide `item` can be on its bar's first line: the bar's content box less
 * everything drawn before it (the drawer toggle, below the shell breakpoint)
 * and the gap after each. A bar not laid out yet has no room to report, and
 * caps nothing.
 */
function roomBeside(bar: HTMLElement, item: HTMLElement): number {
  const style = getComputedStyle(bar);
  const content =
    bar.clientWidth - (parseFloat(style.paddingLeft) || 0) - (parseFloat(style.paddingRight) || 0);
  if (content <= 0) return Infinity;
  const gap = parseFloat(style.columnGap) || 0;
  let before = 0;
  for (let sib = item.previousElementSibling; sib; sib = sib.previousElementSibling) {
    const width = sib.getBoundingClientRect().width;
    if (width > 0) before += width + gap;
  }
  return Math.max(0, Math.floor(content - before));
}

/**
 * Whether `el` is wider inside than it is drawn: a scrollport with somewhere
 * to scroll. Measured after layout, and again whenever the box is resized or
 * `content` (what it holds) changes — a trail grows when a screen publishes a
 * name for its object, which resizes nothing.
 */
function useOverflows(el: RefObject<HTMLElement | null>, content: unknown): boolean {
  const [overflows, setOverflows] = useState(false);
  useLayoutEffect(() => {
    const node = el.current;
    if (!node) return;
    const measure = () => setOverflows(node.scrollWidth > node.clientWidth);
    measure();
    const watch = new ResizeObserver(measure);
    watch.observe(node);
    return () => watch.disconnect();
  }, [el, content]);
  return overflows;
}

/**
 * Copy this page's own address.
 *
 * THE WHOLE URL, not the hash: what a reader pastes into chat has to open on
 * somebody else's machine, and a bare `#/work/ENG-42` opens nothing.
 */
export function CopyLink({ label = "Copy link" }: { label?: string }) {
  const [said, setSaid] = useState("");
  const copy = useCallback(() => {
    void copyPageLink().then((word) => {
      setSaid(word);
      window.setTimeout(() => setSaid(""), 1_600);
    });
  }, []);
  // NOT `Copyable`, which is the right part for an identifier: it DRAWS the
  // value it copies, and what this copies is the page's whole URL. A page bar
  // that rendered `http://host:8000/#/work/ENG-42?peek=seat:ada` where "Copy
  // link" is would push the breadcrumb off the bar.
  return (
    <Button
      size="small"
      variant="ghost"
      leadingIcon={<CopyGlyph size="sm" />}
      onClick={copy}
      title={label}
    >
      {said || label}
    </Button>
  );
}

/**
 * Keep a shortcut to this page, or drop the one you have.
 *
 * THE ONLY PRODUCER OF A STAR. `lib/starred.ts` was written whole — the cap,
 * the refusal at it, the storage guards, its own suite — and nothing in the
 * product could create one or read one back, so every workspace sidebar's
 * Starred section had an empty list by construction.
 *
 * ONE BUTTON, one gesture: a filled star means "not this one", which is why
 * `toggleStar` is the only verb. It reports, including the refusal at the cap
 * — reaching fifty is not "it worked" and must not render as a filled star.
 */
export function StarPage({ path, label, workspace }: Omit<Star, "at">) {
  const stars = useStarred();
  const toggle = useToggleStar();
  const reader = useReader();
  const [said, setSaid] = useState("");
  const kept = starredIn(stars, path);
  // NOR BY NOBODY: stars are kept per reader (`lib/starred.ts`), so a tab
  // that has not learned who reads it has no list to keep one in — and one
  // kept in a list nobody owns would be offered to whoever signs in next.
  if (reader === null) return null;
  // A PAGE THAT IS NOT ONE CANNOT BE KEPT: on Not Found the star was offered
  // and pressed filled, and the store — which drops a kept path that no longer
  // resolves — then took it back on the next read, so the reader's star
  // vanished with nothing said.
  if (!resolves(path)) return null;
  // A WORKSPACE'S OWN PAGE IS ALREADY A SIDEBAR ROW, so a star on it would be
  // a second row to the same place. But the star is DRAWN there, unavailable
  // and saying why, rather than left out: it was drawn on every section of a
  // workspace but its first, so moving between two tabs of one workspace moved
  // the header's controls, and the reader had no way to learn why the star was
  // sometimes missing.
  if (path.length < 2) {
    return (
      <Button
        size="small"
        variant="ghost"
        leadingIcon={<StarGlyph size="sm" />}
        disabledReason="Already in the sidebar"
        title="Already in the sidebar"
        aria-label="Keep in Starred"
      />
    );
  }
  const onClick = () => {
    const what = toggle({ path, label, workspace });
    if (what !== "full") return;
    setSaid(`${MaxStars} is the limit`);
    window.setTimeout(() => setSaid(""), 2_400);
  };
  // THE KEPT STATE IS THE SAME STAR FILLED. `star` is the one glyph the
  // design system lists as `FILLABLE` — a closed silhouette whose inside
  // reads as the mark — and it is listed for exactly this toggle, so the
  // pressed state is `filled` rather than a second drawing. `pressed` says
  // the same thing to assistive technology.
  return (
    <Button
      size="small"
      variant="ghost"
      onClick={onClick}
      title={said || (kept ? "Remove from Starred" : "Keep in Starred")}
      pressed={kept}
      leadingIcon={<StarGlyph filled={kept} size="sm" />}
    >
      {/* `undefined` RATHER THAN `""` WHEN THERE IS NOTHING TO SAY: their
          Button falls back to `title` for the accessible name only when it has
          no children at all, and an empty label span is children. Without this
          the star is an unnamed button for as long as it is not refusing. */}
      {said || undefined}
    </Button>
  );
}

/**
 * Copy this page's whole address, and say what the browser did with it.
 *
 * The clipboard is refused outright on an insecure origin and by permission
 * policy, and a control that silently did nothing would read as a broken
 * button rather than as a browser that said no. BOTH REFUSALS ARE SAID:
 * permission policy rejects the write, but an insecure origin — this engine
 * serves plain HTTP on `api.port` unless something terminates TLS in front of
 * it — has no `navigator.clipboard` at all, and an optional call on it skipped
 * the whole chain, so the ordinary deployment was exactly the one where the
 * button did nothing and said nothing.
 */
export async function copyPageLink(): Promise<"Copied" | "Blocked"> {
  if (!navigator.clipboard) return "Blocked";
  try {
    await navigator.clipboard.writeText(location.href);
    return "Copied";
  } catch {
    return "Blocked";
  }
}

/**
 * "More": the page bar's secondary actions, in one menu, on a phone.
 *
 * DRAWN ONLY UNDER THE PHONE'S WIDTH (`.page-more`), where the frame's star
 * and Copy link — and whatever a screen folds with [usePageMenu] — leave the
 * bar for this one control, so the screen's primary action stays in view
 * beside it and an object's lenses and its actions share a line. On a wider
 * bar every one of them is drawn inline and this is not drawn at all, so no
 * action is ever in two places a reader can see at once.
 *
 * What a press did is said in a toast, because the menu closes on it: the
 * inline controls say "Copied" or "50 is the limit" on themselves, and a menu
 * item has no self left to say it on.
 */
export function PageMore({
  path,
  label,
  workspace,
  entries,
}: Omit<Star, "at"> & { entries: readonly PageMenuEntry[] }) {
  const stars = useStarred();
  const toggle = useToggleStar();
  const toast = useToast();
  const reader = useReader();
  const kept = starredIn(stars, path);
  const items: MenuEntry[] = entries.map((e) => ({
    key: e.key,
    label: e.label,
    icon: e.icon,
    onSelect: e.onSelect,
    disabled: e.disabled,
    description: e.description,
  }));
  if (items.length > 0) items.push({ kind: "separator", key: "frame" });
  // THE STAR AS THE INLINE ONE DRAWS IT: absent on a page that is not one and
  // for a tab that has not learned who reads it, unavailable and saying why on
  // a workspace's own page.
  if (reader !== null && resolves(path)) {
    items.push(
      path.length < 2
        ? {
            key: "star",
            label: "Keep in Starred",
            disabled: true,
            description: "Already in the sidebar",
            onSelect: () => {},
          }
        : {
            key: "star",
            label: kept ? "Remove from Starred" : "Keep in Starred",
            icon: <StarGlyph filled={kept} size="sm" />,
            onSelect: () => {
              if (toggle({ path, label, workspace }) === "full") {
                toast.show({ variant: "warning", title: `${MaxStars} is the limit` });
              }
            },
          },
    );
  }
  items.push({
    key: "copy-link",
    label: "Copy link",
    icon: <CopyGlyph size="sm" />,
    onSelect: () => {
      void copyPageLink().then((word) =>
        word === "Copied"
          ? toast.ok("Copied the link to this page")
          : toast.show({
              variant: "warning",
              title: "The browser refused the clipboard",
              message: "Copy the address from the address bar instead.",
            }),
      );
    },
  });
  return (
    <span className="page-more">
      <Menu
        label="More on this page"
        icon={<EllipsisGlyph size="sm" />}
        triggerVariant="ghost"
        align="end"
        items={items}
      />
    </span>
  );
}
