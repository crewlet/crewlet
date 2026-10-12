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
 * WORK DRAWS NO SECOND ROW. Its sections are the lenses of the scope its
 * trail names (`renderer: "lenses"` in `nav.ts`), drawn as chips beside the
 * trail's switcher ([AllProjectsLenses]) the way a project draws its own, so
 * the bar reads one way in either scope. The frame's star is not drawn there:
 * the scope is switched in the trail, and a shortcut to one of its lenses is
 * a second way to the same place.
 *
 * Settings draws its sections as a COLUMN instead (`SectionColumn`), because
 * its own row in WORKSPACES holds more sections, and cross-links to the
 * reader's Account and to Budgets, in its groups, than a tab strip can — a row
 * nobody reads to the end of.
 *
 * # A section is a path
 *
 * Every tab is a link to its own address, never a `tab=` on this one: a
 * section is a place, and a place has a URL a reader can paste. What travels
 * between sections is the workspace's own query (`WorkspaceRow.keep` — whose
 * day My work is reading, the window Spend is reading over), and nothing
 * else: a filter on the Turns list is not a filter on the Event log.
 *
 * # A figure on a tab is the screen's
 *
 * A section's count is published by the screen that has it (`useSectionCounts`
 * in `Shell.tsx`), because only that screen knows whether the number is a
 * total, a floor or not yet known; the header draws what it is given and draws
 * nothing for a section the screen did not count.
 */

import { useId, useLayoutEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import {
  AppShell,
  AvatarStack,
  Breadcrumb,
  Button,
  Menu,
  SidebarNav,
  Tabs,
  Tag,
  useToast,
  type BreadcrumbCrumb,
  type MenuEntry,
} from "@crewlethq/ui";
import {
  ArrowUpRightGlyph,
  ChevronDownGlyph,
  EllipsisGlyph,
  KeyGlyph,
  StarGlyph,
} from "@crewlethq/icons/glyphs";
import { href, samePath, useNavigator, useRoute } from "../router.tsx";
import type { Crumb } from "../crumbs.ts";
import { resolves } from "../routes.ts";
import {
  grantWords,
  grantsMissing,
  isSidebarRow,
  sectionOf,
  WORKSPACES,
  type Section,
  type WorkspaceRow,
} from "../nav.ts";
import {
  ALL_PROJECTS_LENSES,
  LENS_LABELS,
  allProjectsPath,
  scopeAt,
  type AllProjectsLens,
} from "../scope.ts";
import { preload } from "../lazyScreen.ts";
import type { SectionFigure } from "../sidebar/settingsFigures.tsx";
import { PAGE_ACTIONS_SLOT, PAGE_LENSES_SLOT, lensLabel } from "../frame/PageActions.tsx";
import { ScopeSwitcher, useScopeFigures } from "./ProjectSwitcher.tsx";
import { glyphFor } from "~/ui/glyph.tsx";
import { SeatAvatar, seatBadge } from "~/ui/SeatAvatar.tsx";
import { useReader } from "~/lib/reader.ts";
import { MaxStars, starredIn, useStarred, useToggleStar, type Star } from "~/lib/starred.ts";
import { useAgents } from "~/lib/store-hooks.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { plural } from "~/lib/format.ts";
import { useScrollEdges } from "~/lib/useScrollEdges.ts";
import { foldTabs } from "../frame/tabFit.ts";
import { useMediaQuery } from "~/lib/media.ts";
import { PHONE_BREAKPOINT } from "../layout.ts";

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
  star = true,
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
  /** The frame draws the star; false where the screen's own menu carries it. */
  star?: boolean;
  /** What sits under the rows: the state bar. */
  children?: ReactNode;
}) {
  const route = useRoute();
  const controls = useRef<HTMLDivElement>(null);
  const edges = useScrollEdges(controls);
  // WORK'S SCOPE PAGES: All projects' lenses, or a project's.
  const at = scopeAt(route.path, route.query);
  return (
    <div className="page-head">
      <AppShell.Topbar className="page-bar">
        <Trail crumbs={crumbs} />
        {/* A SCOPE'S LENSES, beside the name they are lenses on: All
            projects' drawn here, a project's portalled in by its page (see
            `PageLenses`). Empty on every other page, where it takes no room
            (`.page-lenses:empty`). */}
        <div className="page-lenses" id={PAGE_LENSES_SLOT}>
          {row?.renderer === "lenses" && at?.scope.kind === "all" ? (
            <AllProjectsLenses lens={at.lens as AllProjectsLens} />
          ) : null}
        </div>
        <div className="page-controls" ref={controls} {...edges}>
          {/* THE SLOT IS ALWAYS RENDERED, whether or not a screen has
              controls: a portal needs a node to land in, and one that appears
              only when the frame already knows there are controls could never
              be found by the screen that has them. The gap is the bar's own,
              so a screen's controls sit as far apart as the frame's. */}
          {working && <WorkingNow />}
          {/* THE FRAME'S OWN STAR, drawn inline on a wide bar and folded
              into "More" on a phone (see `PageMore`), where the bar keeps one
              action in view. Not on Work's scope pages (see the module doc),
              and not where the screen's own menu carries the star. */}
          {star && !at && (
            <span className="page-frame-actions">
              <StarPage path={route.path} label={title} workspace={workspace} />
            </span>
          )}
          {/* BEFORE THE SLOT IN THE DOCUMENT, so the screen's controls stay the
              bar's last element on a wide bar (where this is not drawn); on a
              phone the slot leads the line (`order: -1`) and this ends it. */}
          <PageMore
            path={route.path}
            label={title}
            workspace={workspace}
            entries={menu}
            star={star && !at}
          />
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
 * everywhere else the sidebar's Live row carries the same count, and its
 * Running group names each turn.
 *
 * THE ENGINE'S WORD, off the agents push. Nothing is drawn while nobody is
 * working: a header reading "0 agents working" is a line a reader learns to
 * skip, and then skips on the day it says something.
 */
export function WorkingNow() {
  const agents = useAgents();
  const badgeOf = useSeatBadgeOf();
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
          // NO STATUS ON A MEMBER: each overlaps the one before it, and
          // the sentence beside the stack already says they are working.
          ...seatBadge(a.role, "agent", badgeOf(a.handle || a.role).avatar),
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
 * What a screen reader is told a CROSS-LINK leads to, after its label: the
 * workspace it lives under by NAME — it read the raw path segment, so Budgets
 * said "under spend" — or, for a cross-link to a workspace's own landing (the
 * reader's Account, which is a workspace of its own), that it is outside this
 * one, since "Account, under Account" says nothing.
 */
export function elsewhereWords(row: WorkspaceRow, s: Section): string {
  if (s.path.length <= 1) return `, outside ${row.label}`;
  const home = WORKSPACES.find((w) => w.key === s.path[0]);
  return `, under ${home?.label ?? s.path[0]}`;
}

/**
 * Settings' sections, as a column of groups beside the screen.
 *
 * A SECTION CLOSED TO THE READER DRAWS ITS LOCK AND STAYS, for the reason no
 * sidebar row is ever hidden: a section that vanished for a reader without its
 * grant is one they cannot know exists. Closed is `nav.ts`'s `sectionOpen`,
 * the one rule every lock is drawn by — the reader's own sections (their
 * Account, Appearance) never, every other without the company's grant or its
 * own. The lock names what is missing, because that is what the reader would
 * ask somebody for — and it is drawn only once somebody has said what the
 * reader holds, since a lock on every section while the first read is out
 * claims a refusal nobody has made. Each row's title is its hint. A CROSS-LINK
 * (the reader's Account, and Budgets, which lives once, under Spend) draws an
 * arrow, because pressing it leaves Settings. A figure beside a section is handed in whole, in the
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
  // the list is folded behind one row saying where the reader is; at every
  // other width the toggle is not drawn and the list always is (frame.css).
  //
  // FOLDED WHERE THE READER CHOSE A SECTION, OPEN WHERE THEY HAVE NOT. A deep
  // link (`#/settings/people`) arrives folded, which is the reason the picker
  // folds at all: the section the link named starts right under it. But the
  // workspace's own address names no section anybody picked — it is where the
  // sidebar's Settings row lands — so arriving there opens the list: folded,
  // a phone reader saw one row reading "General" and never the You group with
  // their Account in it. Every pick folds it again, the section the reader is
  // already on included, which moves no path.
  const phone = useMediaQuery(`(width < ${PHONE_BREAKPOINT}px)`);
  const at = path.join("/");
  const atRoot = samePath(path, row.path);
  const [open, setOpen] = useState(phone && atRoot);
  const [openedAt, setOpenedAt] = useState(at);
  // THE ADDRESS A PICK IN THIS LIST IS ON ITS WAY TO, so the arrival it causes
  // is told from one the reader did not make here — the Settings row, Back.
  const [picked, setPicked] = useState<string | null>(null);
  if (openedAt !== at) {
    setOpenedAt(at);
    setOpen(picked !== at && phone && atRoot);
    setPicked(null);
  }
  const pick = (s: Section) => () => {
    setOpen(false);
    const to = s.path.join("/");
    if (to !== at) setPicked(to);
  };
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
              {/* SAID, NOT ONLY SPOKEN: the row read "General" and nothing
                  else, which a reader takes for the section's title rather
                  than for a list of the others. */}
              <span className="section-picker-prefix">
                <span className="sr-only">{row.label} </span>Section
                <span aria-hidden="true"> · </span>
                <span className="sr-only">: </span>
              </span>
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
                // BY THE ONE RULE every lock the navigation draws is decided
                // by (`nav.ts`'s `sectionOpen`): the reader's own sections
                // need nothing, and every other needs the company's grant too.
                const needs = grants === null ? [] : grantsMissing(s, grants);
                const figure = figures[s.key];
                return (
                  <SidebarNav.Item
                    key={s.key}
                    href={href(s.path)}
                    current={!s.elsewhere && current?.key === s.key}
                    icon={<Glyph size="sm" />}
                    onClick={pick(s)}
                    // THE ROW'S HINT AS ITS TITLE, the words the palette shows
                    // under it — for a cross-link the hint of the row it leads
                    // to (`crossLink`), so the reader's Account is described
                    // alike wherever it is offered.
                    renderLink={({ className, ...link }) => (
                      <a className={className} {...link} title={s.hint} />
                    )}
                    label={
                      <span className="section-column-label">
                        {s.label}
                        {needs.length > 0 && (
                          <>
                            <KeyGlyph size="xs" aria-hidden="true" />
                            <span className="sr-only">, needs {grantWords(needs)}</span>
                          </>
                        )}
                        {s.elsewhere && (
                          <>
                            <ArrowUpRightGlyph size="xs" aria-hidden="true" />
                            <span className="sr-only">{elsewhereWords(row, s)}</span>
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
 * The trail, as the kit's `Breadcrumb` draws it, from the route table's
 * crumbs (`crumbs.ts`). Its floor, its tab stop while it overflows and the
 * page's `h1` are the kit's; what is here is how a crumb of this product
 * becomes one of the kit's.
 *
 * WORK'S LAST CRUMB NAMES A SCOPE THE READER CAN SWITCH ([Crumb.switch]), and
 * is drawn as the switcher's button ([ScopeSwitcher]): the heading is the
 * control, so the name a reader lands on by heading is the one they change.
 */
export function Trail({ crumbs }: { crumbs: Crumb[] }) {
  const last = crumbs[crumbs.length - 1];
  return (
    <Breadcrumb
      className="page-trail"
      crumbs={crumbs.map(kitCrumb)}
      current={
        last?.switch ? (
          <ScopeSwitcher
            trigger={{
              label: last.label,
              ...(last.tag ? { keyChip: last.tag } : {}),
              ...(last.mono ? { mono: true } : {}),
              ...(last.archived
                ? {
                    tag: (
                      <Tag size="xs" appearance="outline">
                        Archived
                      </Tag>
                    ),
                  }
                : {}),
            }}
          />
        ) : undefined
      }
    />
  );
}

/** One crumb of this product as the kit draws it. */
function kitCrumb(crumb: Crumb): BreadcrumbCrumb {
  const Glyph = crumb.icon ? glyphFor(crumb.icon) : undefined;
  return {
    label: crumb.label,
    ...(crumb.path ? { href: href(crumb.path, crumb.query) } : {}),
    ...(Glyph ? { icon: <Glyph size="sm" /> } : {}),
    ...(crumb.tag ? { keyChip: crumb.tag } : {}),
    ...(crumb.seat
      ? {
          badge: (
            <SeatAvatar
              name={crumb.seat.name}
              kind={crumb.seat.kind}
              avatar={crumb.seat.avatar}
              size="xs"
              decorative
            />
          ),
        }
      : {}),
    ...(crumb.mono ? { mono: true } : {}),
  };
}

/**
 * All projects' lenses: Work's own sections, drawn as the chips a project's
 * lenses are, beside the scope's name in the trail.
 *
 * LINKS, each to its section's address: the kit's router mode, a `nav` with
 * `aria-current` on the lens the reader is on, because each of these lenses
 * is a page with its own URL rather than a panel of this one. Hovering or
 * focusing one starts fetching where it leads, as the sidebar's rows do.
 *
 * THE FIGURES ARE THE LISTING'S ([useScopeFigures]): how many projects are
 * active, and how much work is open across them, the same open figure the
 * switcher's All projects row carries.
 */
function AllProjectsLenses({ lens }: { lens: AllProjectsLens }) {
  const figures = useScopeFigures();
  const figure = (key: AllProjectsLens) =>
    key === "projects"
      ? lensLabel(LENS_LABELS[key], figures.projects, "active")
      : key === "tasks"
        ? lensLabel(LENS_LABELS[key], figures.open, "open")
        : LENS_LABELS[key];
  return (
    <Tabs
      variant="chip"
      ariaLabel="Lens"
      value={lens}
      items={ALL_PROJECTS_LENSES.map((key) => ({ value: key, label: figure(key) }))}
      renderItem={({ item, className, isActive, children }) => {
        const path = allProjectsPath(item.value as AllProjectsLens);
        const warm = () => preload(path);
        return (
          <a
            className={className}
            href={href(path)}
            aria-current={isActive ? "page" : undefined}
            onPointerEnter={warm}
            onFocus={warm}
          >
            {children}
          </a>
        );
      }}
    />
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
  // sometimes missing. WHETHER IT IS ONE is `isSidebarRow`'s, never the
  // path's length: the reader's Account is one segment long and has no row.
  if (isSidebarRow(path)) {
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
 * "More": the page bar's secondary actions, in one menu, on a phone.
 *
 * DRAWN ONLY UNDER THE PHONE'S WIDTH (`.page-more`), where the frame's star
 * and whatever a screen folds with [usePageMenu] leave the
 * bar for this one control, so the screen's primary action stays in view
 * beside it and an object's lenses and its actions share a line. On a wider
 * bar every one of them is drawn inline and this is not drawn at all, so no
 * action is ever in two places a reader can see at once.
 *
 * What a press did is said in a toast, because the menu closes on it: the
 * inline star says "50 is the limit" on itself, and a menu
 * item has no self left to say it on.
 */
export function PageMore({
  path,
  label,
  workspace,
  entries,
  star = true,
}: Omit<Star, "at"> & {
  entries: readonly PageMenuEntry[];
  /**
   * Whether the frame's star is offered here: not on Work's scope pages, nor
   * where the screen's own menu carries it.
   */
  star?: boolean;
}) {
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
  // THE STAR AS THE INLINE ONE DRAWS IT: absent on a page that is not one,
  // whose own entries carry it, or for a tab that has not learned who reads
  // it, and unavailable and saying why on a sidebar row's own page — by the
  // one rule the inline star asks (`isSidebarRow`).
  const frameStar = star && reader !== null && resolves(path);
  if (items.length > 0 && frameStar) items.push({ kind: "separator", key: "frame" });
  if (frameStar) {
    items.push(
      isSidebarRow(path)
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
  // NOTHING TO OFFER IS NO MENU: a screen whose entries carry the star has
  // none until its page has answered.
  if (items.length === 0) return null;
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
