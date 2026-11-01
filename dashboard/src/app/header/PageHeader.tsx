/**
 * The page header: where the reader is, what they can do here, and the
 * sections of the workspace they are in.
 *
 * # Two rows, one banner
 *
 * The first row is the kit's top bar — the drawer toggle below the shell
 * breakpoint, the breadcrumb, an object page's lenses beside it (`PageLenses`),
 * and what this page can do: who is working right now (Home's bar only), the
 * star and the link, the screen's own controls (portalled in through
 * `PageActions`), and LAST the Ask button that opens the assistant, which
 * stands above the panel it opens (decision D1): a screen's primary action
 * ("New task") is the last of the screen's own, and the frame's quiet
 * controls come before them. The second row is the
 * workspace's SECTIONS, drawn
 * as tabs, on a section's own page only: on an object page the crumbs are the
 * way back out, and a strip of tabs over a task page would say the task is a
 * fourth kind of list.
 *
 * WORKLET DRAWS NO SECOND ROW. Its sections are the lenses of the scope its
 * trail names (`renderer: "lenses"` in `nav.ts`), drawn as chips beside the
 * trail's switcher ([AllProjectsLenses]) the way a project draws its own, so
 * the bar reads one way in either scope. The frame's star is not drawn there:
 * the scope is switched in the trail, and a shortcut to one of its lenses is
 * a second way to the same place.
 *
 * Settings draws its sections as a COLUMN instead (`SectionColumn`), because
 * its own row in WORKSPACES holds more sections, in four groups, than a tab
 * strip can: a row nobody reads to the end of.
 *
 * # A section is a path
 *
 * Every tab is a link to its own address, never a `tab=` on this one: a
 * section is a place, and a place has a URL a reader can paste. What travels
 * between sections is the workspace's own query (`WorkspaceRow.keep`: whose
 * day My work is reading, the window Settings' Spend is reading over), in the
 * tabs and the column alike ([keptQuery]), and nothing else: a filter on the
 * Turns list is not a filter on the Event log.
 *
 * # A figure on a tab is the screen's
 *
 * A section's count is published by the screen that has it (`useSectionCounts`
 * in `Shell.tsx`), because only that screen knows whether the number is a
 * total, a floor or not yet known; the header draws what it is given and draws
 * nothing for a section the screen did not count.
 */

import { useRef, useState, type ReactNode } from "react";
import {
  AppShell,
  AvatarStack,
  Breadcrumb,
  Button,
  LockedLabel,
  Menu,
  SectionFrame,
  SidebarNav,
  Tabs,
  Tag,
  useToast,
  type BreadcrumbCrumb,
  type MenuEntry,
  useMediaMatch,
} from "@crewlethq/ui";
import { EllipsisGlyph, StarGlyph } from "@crewlethq/icons/glyphs";
import { href, samePath, useNavigator, useRoute } from "../router.tsx";
import type { Crumb } from "../crumbs.ts";
import { resolves } from "../routes.ts";
import { groupLock, lockReason } from "~/lib/access.ts";
import {
  grantsMissing,
  isSidebarRow,
  sectionOf,
  serviceShown,
  type Section,
  type WorkspaceRow,
} from "../nav.ts";
import type { ServicesRun } from "~/lib/services.ts";
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
import { AssistantTrigger } from "../assistant/AssistantTrigger.tsx";
import { ScopeSwitcher, useScopeFigures } from "./ProjectSwitcher.tsx";
import { glyphFor } from "@crewlethq/icons/glyphs/registry";
import { SeatAvatar, seatBadge } from "~/ui/SeatAvatar.tsx";
import { useReader } from "~/lib/reader.ts";
import { MaxStars, starredIn, useStarred, useToggleStar, type Star } from "~/lib/starred.ts";
import { useAgents } from "~/lib/store-hooks.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { plural } from "~/lib/format.ts";
import { useScrollEdges } from "~/lib/useScrollEdges.ts";
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
  runs,
  grants,
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
  /** Which services the company runs: a tab reading one it does not is not drawn. */
  runs: ServicesRun;
  /** What the reader holds, or null before the frame has read it, for the tabs' locks. */
  grants: readonly string[] | null;
  /** What sits under the rows: the state bar. */
  children?: ReactNode;
}) {
  const route = useRoute();
  const controls = useRef<HTMLDivElement>(null);
  const edges = useScrollEdges(controls);
  // WORKLET'S SCOPE PAGES: All projects' lenses, or a project's.
  const at = scopeAt(route.path, route.query);
  // THE FRAME'S STAR: not on Worklet's scope pages (see the module doc), not
  // where the screen's own menu carries it, and not in a workspace that draws
  // none (`WorkspaceRow.starless`).
  const framesStar = star && !at && !row?.starless;
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
              action in view, wherever `framesStar` says the frame draws one. */}
          {framesStar && (
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
            star={framesStar}
          />
          <div className="row gap-2 wrap page-actions" id={PAGE_ACTIONS_SLOT} />
          {/* THE ASK BUTTON ENDS THE BAR, above the panel it opens (D1): the
              one frame control that stays inline at a phone's width, since it
              is how a reader reaches help on a screen they do not follow. */}
          <AssistantTrigger />
        </div>
      </AppShell.Topbar>
      {row && row.renderer === "tabs" && (
        <SectionTabs
          row={row}
          path={route.path}
          query={route.query}
          counts={counts}
          runs={runs}
          grants={grants}
        />
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
        // (`seatBadge`, whose initials the kit takes past an agent's leading
        // "agent" word): "Agent SWE" was `AS` here beside `SW` everywhere else
        // the seat is drawn.
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
 * THE KIT'S ROW, in router mode: `Tabs` with the underline chrome, whose root
 * is that `nav` and whose items are these links, so a workspace's sections and
 * an object's own tabs are one control. "More" and the reading note are the
 * kit's, inside the landmark with the links they continue.
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
 * MEASURED, NOT GUESSED, by the kit (`overflow="menu"`): a folded tab stays in
 * the document, out of reach, so it can be measured again when the strip
 * grows, and the widths are read again whenever anything in the strip changes
 * size. The folded links take the kit's `foldProps`, which is what puts them
 * out of reach.
 */
export function SectionTabs({
  row,
  path,
  query,
  counts,
  runs,
  grants,
}: {
  row: WorkspaceRow;
  path: string[];
  query: URLSearchParams;
  counts: Record<string, string>;
  /** Which services the company runs: a tab reading one it does not is not drawn. */
  runs: ServicesRun;
  /**
   * What the reader holds, or null before the frame has read it: a tab the
   * reader cannot open is locked like the sidebar's rows and the Settings
   * column's (`grantsMissing`), never drawn as open to then refuse.
   */
  grants: readonly string[] | null;
}) {
  const nav = useNavigator();
  const tabs = row.sections.filter((s) => s.tab !== false && serviceShown(s, runs));
  const current = tabs.find((s) => samePath(s.path, path));
  if (tabs.length < 2 || !current) return null;
  const kept = keptQuery(row, query);
  const paths = new Map(tabs.map((s) => [s.key, s.path]));
  return (
    <Tabs
      overflow="menu"
      ariaLabel={`${row.label} sections`}
      moreMenuLabel={`More sections of ${row.label}`}
      variant="underline"
      className="section-tabs"
      value={current.key}
      // A SECTION PICKED FROM "MORE" is the address its tab links to.
      onValueChange={(key) => nav.to(paths.get(key) ?? row.path, kept)}
      // THE WORKSPACE'S READING NOTE, after the tabs: it gives way to them,
      // drawn only where every tab fits beside it.
      note={row.note}
      items={tabs.map((s) => {
        const Glyph = glyphFor(s.icon);
        const needs = grants === null ? [] : grantsMissing(s, grants);
        return {
          value: s.key,
          label: <LockedLabel reason={lockReason(needs)}>{s.label}</LockedLabel>,
          menuLabel: s.label,
          icon: <Glyph size="sm" />,
          count: counts[s.key],
        };
      })}
      renderItem={({ item, className, isActive, children, foldProps }) => (
        <a
          className={className}
          data-section={item.value}
          href={href(paths.get(item.value) ?? row.path, kept)}
          aria-current={isActive ? "page" : undefined}
          // FOLDED INTO "MORE": still here to be measured, and out of reach
          // of the pointer, the keyboard and a screen reader, which meet it
          // in the menu instead.
          {...foldProps}
        >
          {children}
        </a>
      )}
    />
  );
}

/**
 * The workspace's own query on the address the reader is on
 * ([WorkspaceRow.keep]), for a link to another of its sections to carry.
 */
export function keptQuery(row: WorkspaceRow, query: URLSearchParams): Record<string, string> {
  const kept: Record<string, string> = {};
  for (const key of row.keep ?? []) {
    const value = query.get(key);
    if (value) kept[key] = value;
  }
  return kept;
}

/**
 * Settings' sections, as a column of groups beside the screen.
 *
 * A SECTION CLOSED TO THE READER DRAWS ITS LOCK AND STAYS, for the reason no
 * sidebar row is ever hidden: a section that vanished for a reader without its
 * access is one they cannot know exists. Closed is `nav.ts`'s `sectionOpen`,
 * the one rule every lock is drawn by — the reader's own sections (their
 * Account, Appearance) and General never, every other without Admin. The lock
 * names what is missing, because that is what the reader would ask an admin
 * for, and it is drawn only once somebody has said what the reader holds,
 * since a lock on every section while the first read is out claims a refusal
 * nobody has made. Each row's title is its hint. A section reading a service
 * the company does not run is not drawn (`serviceShown`): Expensive tasks,
 * where it runs no Worklet. A figure beside a section is handed in
 * whole, in the column's own two shapes (`settingsFigures.tsx` decides them
 * and asks for what they need), so the column itself asks nothing.
 */
export function SectionColumn({
  row,
  path,
  query,
  grants,
  figures,
  runs,
}: {
  row: WorkspaceRow;
  path: string[];
  /** The address's query, for the workspace's own keys a row carries ([keptQuery]). */
  query: URLSearchParams;
  /** Which services the company runs: a section reading one it does not is not drawn. */
  runs: ServicesRun;
  /** What the viewer holds, or null while nobody has said. */
  grants: readonly string[] | null;
  figures: Record<string, SectionFigure>;
}) {
  // AN ADDRESS THAT NAMES NO SCREEN IS IN NO SECTION. The landing's path is
  // the workspace root, which prefixes every Settings address, so without this
  // a mistyped `#/settings/general` drew "Not found" under a column (and a
  // phone picker) saying the reader was on General.
  const current = resolves(path) ? sectionOf(path) : undefined;
  const kept = keptQuery(row, query);
  const groups: { name: string; sections: Section[] }[] = [];
  for (const s of row.sections.filter((section) => serviceShown(section, runs))) {
    const name = s.group ?? "";
    const last = groups[groups.length - 1];
    if (last && last.name === name) last.sections.push(s);
    else groups.push({ name, sections: [s] });
  }
  // ON A PHONE THE COLUMN IS A PICKER NAMING THE SECTION (the kit's
  // `SectionFrame.Nav`, folded where the Shell says the frame is). Stacked
  // whole above the section it took about 520px, so on Nodes and Secrets the
  // section's own content began below the fold on every visit.
  //
  // FOLDED WHERE THE READER CHOSE A SECTION, OPEN WHERE THEY HAVE NOT. A deep
  // link (`#/settings/people`) arrives folded, which is the reason the picker
  // folds at all: the section the link named starts right under it. But the
  // workspace's own address names no section anybody picked (it is where the
  // sidebar's Settings row lands), so arriving there on a phone opens the
  // list: folded, a phone reader saw one row naming the first section and
  // never the groups under it. The kit folds it again on every pick.
  const phone = useMediaMatch(`(width < ${PHONE_BREAKPOINT}px)`);
  const atRoot = samePath(path, row.path);
  const CurrentGlyph = current ? glyphFor(current.icon) : undefined;
  // THE KIT'S OWN NAVIGATION LIST, which is what this column is: labelled
  // groups of rows, one of which is where the reader is, raised with a
  // hairline and no hue, the same mark the sidebar beside it uses, so "where
  // am I" reads one way in both columns.
  return (
    <SectionFrame.Nav
      scope={row.label}
      current={
        current && CurrentGlyph
          ? { label: current.label, icon: <CurrentGlyph size="sm" /> }
          : undefined
      }
      location={href(path, kept)}
      openOnArrival={phone && atRoot}
    >
      {groups.map((g) => {
        // BY THE ONE RULE every lock the navigation draws is decided by
        // (`nav.ts`'s `sectionOpen`): the reader's own sections need nothing,
        // and every other names its own access. A group the reader holds
        // none of carries the key once, on its label, as the sidebar's do.
        const locks = g.sections.map((s) =>
          lockReason(grants === null ? [] : grantsMissing(s, grants)),
        );
        return (
          <SidebarNav.Group key={g.name} label={g.name} locked={groupLock(locks)}>
            {g.sections.map((s, i) => {
              const Glyph = glyphFor(s.icon);
              const figure = figures[s.key];
              return (
                <SidebarNav.Item
                  key={s.key}
                  href={href(s.path, kept)}
                  current={current?.key === s.key}
                  icon={<Glyph size="sm" />}
                  // THE ROW'S HINT AS ITS TITLE, the words the palette shows
                  // under it.
                  title={s.hint}
                  label={s.label}
                  locked={locks[i]}
                  badge={figure?.badge}
                  count={figure?.count}
                />
              );
            })}
          </SidebarNav.Group>
        );
      })}
    </SectionFrame.Nav>
  );
}

/**
 * The trail, as the kit's `Breadcrumb` draws it, from the route table's
 * crumbs (`crumbs.ts`). Its floor, its tab stop while it overflows and the
 * page's `h1` are the kit's; what is here is how a crumb of this product
 * becomes one of the kit's.
 *
 * WORKLET'S LAST CRUMB NAMES A SCOPE THE READER CAN SWITCH ([Crumb.switch]), and
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
 * All projects' lenses: Worklet's own sections, drawn as the chips a project's
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
   * Whether the frame's star is offered here: not on Worklet's scope pages, nor
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
