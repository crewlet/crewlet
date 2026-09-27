/**
 * The page header: where the reader is, what they can do here, and the
 * sections of the workspace they are in.
 *
 * # Two rows, one banner
 *
 * The first row is the kit's top bar — the drawer toggle below the shell
 * breakpoint, the breadcrumb, and what this page can do: who is working right
 * now, the star and the link, and LAST the screen's own controls (portalled
 * in through `PageActions`) — because a screen's primary action ("New task")
 * belongs at the bar's end, where the approved designs put it and where the
 * eye finishes the row; the frame's quiet controls come before it. The second row is the workspace's SECTIONS, drawn
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
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
  type RefObject,
} from "react";
import { AppShell, AvatarStack, Button, SidebarNav, cx } from "@crewlethq/ui";
import {
  ArrowUpRightGlyph,
  ChevronRightGlyph,
  CopyGlyph,
  KeyGlyph,
  StarGlyph,
} from "@crewlethq/icons/glyphs";
import { href, samePath, useRoute } from "../router.tsx";
import type { Crumb } from "../crumbs.ts";
import { resolves } from "../routes.ts";
import { sectionOf, type Section, type WorkspaceRow } from "../nav.ts";
import { PAGE_ACTIONS_SLOT } from "../frame/PageActions.tsx";
import { glyphFor } from "~/ui/glyph.tsx";
import { MaxStars, starredIn, useStarred, useToggleStar, type Star } from "~/lib/starred.ts";
import { useAgents } from "~/lib/store-hooks.ts";
import { plural } from "~/lib/format.ts";

/** How many working seats the header draws before "+n". */
const WORKING_FACES = 4;

export function PageHeader({
  crumbs,
  row,
  counts,
  title,
  workspace,
  workingIn = "",
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
  /** The project the page is about, whose working seats the header draws. */
  workingIn?: string;
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
        <div className="page-controls" ref={controls} {...edges}>
          {/* THE SLOT IS ALWAYS RENDERED, whether or not a screen has
              controls: a portal needs a node to land in, and one that appears
              only when the frame already knows there are controls could never
              be found by the screen that has them. The gap is the bar's own,
              so a screen's controls sit as far apart as the frame's. */}
          <WorkingNow project={workingIn} />
          <StarPage path={route.path} label={title} workspace={workspace} />
          <CopyLink />
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
 * on every page, one click from what they are running.
 *
 * THE ENGINE'S WORD, off the agents push. Nothing is drawn while nobody is
 * working: a header reading "0 agents working" on every page is a line a
 * reader learns to skip, and then skips on the day it says something.
 */
export function WorkingNow({ project }: { project: string }) {
  const agents = useAgents();
  // ON A PROJECT'S PAGE, THE SEATS ON ITS WORK — the item the engine charges
  // each running turn to, never a guess from what the seat is called.
  const working = agents.filter(
    (a) =>
      a.activity === "working" &&
      (!project ||
        (a.live_call?.work_item?.project || a.turn?.work_item?.project || "") === project),
  );
  if (working.length === 0) return null;
  return (
    <a className="working-now" href={href(["live"])}>
      <AvatarStack
        size="xs"
        max={WORKING_FACES}
        decorative
        members={working.map((a) => ({
          id: a.id,
          name: a.role,
          kind: "agent" as const,
          ring: "info" as const,
        }))}
      />
      <span>
        {plural(working.length, "agent")} {project ? `on ${project}` : "working"}
      </span>
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
  const tabs = row.sections.filter((s) => s.tab !== false && !s.elsewhere);
  const current = tabs.find((s) => samePath(s.path, path));
  const strip = useRef<HTMLElement>(null);
  const edges = useScrollEdges(strip);
  if (tabs.length < 2 || !current) return null;
  const kept: Record<string, string> = {};
  for (const key of row.keep ?? []) {
    const value = query.get(key);
    if (value) kept[key] = value;
  }
  return (
    <nav ref={strip} className="section-tabs" aria-label={`${row.label} sections`} {...edges}>
      {tabs.map((s) => {
        const Glyph = glyphFor(s.icon);
        const figure = counts[s.key];
        return (
          <a
            key={s.key}
            className="section-tab"
            href={href(s.path, kept)}
            aria-current={s === current ? "page" : undefined}
          >
            <Glyph size="sm" aria-hidden="true" />
            <span>{s.label}</span>
            {figure ? <span className="count-chip">{figure}</span> : null}
          </a>
        );
      })}
    </nav>
  );
}

/**
 * Settings' sections, as a column of groups beside the screen.
 *
 * A GUARDED SECTION DRAWS ITS LOCK AND STAYS, for the reason the sidebar's
 * Settings row does: a section that vanished for a reader without an operator
 * credential is one they cannot know exists. A CROSS-LINK (Budgets, which
 * lives once, under Spend) draws an arrow, because pressing it leaves
 * Settings. A figure beside a section is the health push's — the fleet's size,
 * the epoch this node applied — so the column polls nothing.
 */
export function SectionColumn({
  row,
  path,
  operator,
  figures,
}: {
  row: WorkspaceRow;
  path: string[];
  operator: boolean;
  figures: Record<string, string>;
}) {
  const current = sectionOf(path);
  const groups: { name: string; sections: Section[] }[] = [];
  for (const s of row.sections) {
    const name = s.group ?? "";
    const last = groups[groups.length - 1];
    if (last && last.name === name) last.sections.push(s);
    else groups.push({ name, sections: [s] });
  }
  // THE KIT'S OWN NAVIGATION LIST, which is what this column is: labelled
  // groups of rows, one of which is where the reader is — raised with a
  // hairline and no hue, the same mark the sidebar beside it uses, so "where
  // am I" reads one way in both columns.
  return (
    <div className="section-column">
      <SidebarNav label={`${row.label} sections`}>
        {groups.map((g) => (
          <SidebarNav.Group key={g.name} label={g.name}>
            {g.sections.map((s) => {
              const Glyph = glyphFor(s.icon);
              const locked = s.guarded && !operator;
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
                          <span className="sr-only">, needs an operator credential</span>
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
                  count={figure ? { value: figure, label: FIGURE_WORDS[s.key] ?? "" } : undefined}
                />
              );
            })}
          </SidebarNav.Group>
        ))}
      </SidebarNav>
    </div>
  );
}

/** What each column figure counts, read after it as part of the row's name. */
const FIGURE_WORDS: Record<string, string> = {
  nodes: "nodes live",
  config: "the configuration this node applied",
};

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
 */
export function Breadcrumb({ crumbs }: { crumbs: Crumb[] }) {
  const trail = useRef<HTMLElement>(null);
  const overflows = useOverflows(trail, crumbs);
  return (
    <nav
      ref={trail}
      className="crumbs"
      aria-label="Breadcrumb"
      tabIndex={overflows ? 0 : undefined}
    >
      {crumbs.map((crumb, i) => {
        const last = i === crumbs.length - 1;
        const Glyph = crumb.icon ? glyphFor(crumb.icon) : undefined;
        const face = cx(crumb.mono && "mono");
        const words = (
          <>
            {Glyph && <Glyph size="sm" className="crumb-glyph" aria-hidden="true" />}
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
 * Which ends of a sideways scroller have more past them, as the two data
 * attributes the sheet fades an edge on (`data-more-start`, `data-more-end`).
 *
 * A ROW THAT SCROLLS WITH NO SCROLLBAR HAS TO SAY SO. On a phone the page's
 * controls and the section tabs each take one line and scroll rather than
 * stack, with the bar hidden because it would sit on the text — and a control
 * cut at the edge ("All wo") read as a broken label rather than as more to
 * swipe to. A fade on the edge that has more is the hint; measured again on
 * every scroll and resize, and absent entirely on a row that fits.
 */
function useScrollEdges(el: RefObject<HTMLElement | null>): {
  "data-more-start"?: "";
  "data-more-end"?: "";
} {
  const [edges, setEdges] = useState({ start: false, end: false });
  useLayoutEffect(() => {
    const node = el.current;
    if (!node) return;
    const measure = () => {
      const start = node.scrollLeft > 1;
      const end = node.scrollLeft + node.clientWidth < node.scrollWidth - 1;
      setEdges((was) => (was.start === start && was.end === end ? was : { start, end }));
    };
    measure();
    const watch = new ResizeObserver(measure);
    watch.observe(node);
    node.addEventListener("scroll", measure, { passive: true });
    return () => {
      watch.disconnect();
      node.removeEventListener("scroll", measure);
    };
  });
  return {
    ...(edges.start ? { "data-more-start": "" as const } : {}),
    ...(edges.end ? { "data-more-end": "" as const } : {}),
  };
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
    const url = location.href;
    const done = (word: string) => {
      setSaid(word);
      window.setTimeout(() => setSaid(""), 1_600);
    };
    // The clipboard is refused outright on an insecure origin and by
    // permission policy, and a control that silently did nothing would read
    // as a broken button rather than as a browser that said no. BOTH REFUSALS
    // ARE SAID: permission policy rejects the write, but an insecure origin —
    // this engine serves plain HTTP on `api.port` unless something terminates
    // TLS in front of it — has no `navigator.clipboard` at all, and an
    // optional call on it skipped the whole chain, so the ordinary deployment
    // was exactly the one where the button did nothing and said nothing.
    if (!navigator.clipboard) {
      done("Blocked");
      return;
    }
    navigator.clipboard.writeText(url).then(
      () => done("Copied"),
      () => done("Blocked"),
    );
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
  const [said, setSaid] = useState("");
  const kept = starredIn(stars, path);
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
