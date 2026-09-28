/**
 * The one sidebar: where a reader can go, and the few things they keep.
 *
 * # What is on it, and what is not
 *
 * The WORKSPACES, from `nav.ts`'s one table — the three that are about you
 * (Home, Inbox, My work), then the company's five, and Settings at the foot.
 * Then three LIVE SHORTCUTS, each a real object with its own path: the
 * company's projects, the views this reader pinned, and the pages they
 * starred. Nothing else. A workspace's own sections are tabs in its page
 * header and its objects are rows on the section that lists them; a sidebar
 * that carried either becomes a tree with a different shape on every screen,
 * which is the two-column navigation this replaced.
 *
 * # Every figure says what it counts
 *
 * The kit's `NavItem` takes a figure only with the words that name it, and
 * two kinds of figure are two props: a BADGE is how many things are waiting
 * on the reader (the Inbox's unread primary notices, the one accent fill in
 * the chrome), and a COUNT is how many of something a destination holds (the
 * agents working, a project's open tasks, a pinned view's total). A figure
 * the engine did not answer is absent, never a zero.
 *
 * # Settings is never hidden
 *
 * It carries a lock for a reader without an operator credential, and stays: a
 * section that vanished when a credential is absent is indistinguishable from
 * one that does not exist.
 */

import { useMemo, type ReactNode } from "react";
import {
  AppShell,
  BrandLockup,
  IconButton,
  Kbd,
  SearchTrigger,
  SidebarNav,
  StatusDot,
  useAppShell,
} from "@crewlethq/ui";
import { KeyGlyph, PinGlyph, PlusGlyph, StarGlyph } from "@crewlethq/icons/glyphs";
import { href, samePath, useRoute } from "~/app/router.tsx";
import { WORKSPACES, workspaceOf, type WorkspaceRow } from "~/app/nav.ts";
import { glyphFor } from "~/ui/glyph.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useAgents, useConnection, useEngineHealth, useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { unfinished, viewRun } from "~/lib/work.ts";
import { routeProject } from "../palette/hits.ts";
import { useOpenNewTask } from "../newTask.ts";
import { useStarred } from "~/lib/starred.ts";
import { capsOf, keyRow, useKeymap } from "../keymap.ts";
import { inboxFigure, useInboxCounts } from "~/lib/useInboxCounts.ts";
import { HealthCard, healthReading } from "./HealthCard.tsx";
import { RailBoundary } from "../boundaries.tsx";
import { preload } from "../lazyScreen.ts";
import { UserBlock } from "./UserBlock.tsx";

/** How many projects the sidebar asks for: the engine's own page of them. */
const PROJECTS_PAGE = 200;

export function Sidebar({
  onSearch,
  onSetToken,
}: {
  onSearch: () => void;
  onSetToken: () => void;
}) {
  const route = useRoute();
  // THE FRAME'S READINGS, not the sidebar's own: see `FrameReadings`.
  const viewer = useViewer();

  // BELOW THE SHELL BREAKPOINT THE SIDEBAR IS A DRAWER, and Mod+\ opens it —
  // the reader's way to it without reaching for the toggle in the bar. Only
  // there: above the breakpoint the sidebar is always on screen and there is
  // nothing to open. It OPENS rather than toggles, because an open drawer is
  // a modal layer that owns the keyboard (`lib/keys.ts` stands aside for
  // one), and the drawer's own Escape is what closes it.
  const shell = useAppShell();
  useKeymap({ drawer: { run: shell.openDrawer, when: shell.narrow } });
  const org = useOrg();
  const agents = useAgents();
  const openNewTask = useOpenNewTask();
  const health = useEngineHealth();
  const { connected, authRejected } = useConnection();
  const inbox = useInboxCounts();
  const here = workspaceOf(route.path);
  const at = route.path.join("/");

  // THE ENGINE'S WORD for working, off the agents push — never a projection's
  // own reading of a seat's call state.
  const working = agents.filter((a) => a.activity === "working").length;

  const mine = useQuery("work_my_work", viewer.handle ? { handle: viewer.handle } : undefined, {
    enabled: viewer.handle !== "",
    pollMs: 60_000,
  });
  const asks = mine.data?.totals?.asked_of_me;

  const seatName = useMemo(
    () => indexOrg(org).byHandle.get(viewer.handle)?.name ?? "",
    [org, viewer.handle],
  );

  const figures: Partial<Record<WorkspaceRow["key"], Pick<NavRowProps, "badge" | "count">>> = {
    inbox:
      inbox.waiting !== null && inbox.waiting > 0
        ? { badge: { value: inboxFigure(inbox), label: "unread, waiting on you" } }
        : {},
    // NOTHING ASKED IS NO FIGURE, as nothing waiting is no badge: a zero
    // beside the row is a mark a reader checks, and it would be there for
    // good on a quiet day.
    me: asks && asks.total > 0 ? { count: { value: figure(asks), label: "asked of you" } } : {},
    agents:
      working > 0
        ? {
            count: {
              value: working,
              label: "working",
              // THE KIT'S STEADY-STATE PULSE, which holds still under reduced
              // motion — the one animated mark in the chrome, on the one state
              // that is ongoing.
              mark: <StatusDot tone="info" pulse />,
            },
          }
        : {},
  };

  const row = (ws: WorkspaceRow) => (
    <NavRow
      key={ws.key}
      label={ws.label}
      icon={ws.icon}
      path={ws.path}
      current={here === ws.key}
      {...figures[ws.key]}
    />
  );

  return (
    <AppShell.Rail
      header={
        <div className="side-head">
          <BrandLockup
            // THE COMPANY IS THE TITLE, beside the product's mark — the lockup
            // the approved design draws ("Nimbus", no second line). The kit's
            // own default puts the product's name first and the company under
            // it, reasoning that a home link named by the product reads the same
            // on every deployment; on this screen the company is the subject,
            // one browser is one company, and the product is in the mark and the
            // tab. "Crewlet" stands in only while no company has been sent.
            name={org?.name || "Crewlet"}
            href={href(["home"])}
            // A LITERAL PATH under the dashboard's own `base`, which the dev
            // server serves itself — written out so `protocol/proxy.test.ts`
            // can read it and hold it against the proxy table.
            mark={<img src="/static/dashboard/crewlet-icon.svg" alt="" />}
          />
          {/* THE ONE CREATE THE CHROME CARRIES, beside the company's name as
              the approved design draws it: filing work is the thing a person
              does from anywhere, so its door is on every screen. It files
              into the project on screen when there is one — the sheet says
              where before anything is sent. NOT GATED HERE: the sheet's own
              Create is the write control, and it says why a reader who
              cannot file cannot. */}
          <IconButton
            size="sm"
            variant="ghost"
            label="New task"
            icon={<PlusGlyph size="sm" />}
            onClick={() => openNewTask({ project: routeProject(route.path) || undefined })}
          />
        </div>
      }
      footer={
        <>
          <SidebarNav label="Settings">
            {WORKSPACES.filter((ws) => ws.place === "foot").map((ws) => (
              <NavRow
                key={ws.key}
                label={
                  // THE LOCK IS ON THE ROW, NOT INSTEAD OF IT. General is the
                  // charter, readable by anybody; every other section needs an
                  // operator credential, and the lock says so before a click.
                  !viewer.operator ? (
                    <span className="side-locked">
                      {ws.label}
                      <KeyGlyph size="xs" aria-hidden="true" />
                      <span className="sr-only">, most sections need an operator credential</span>
                    </span>
                  ) : (
                    ws.label
                  )
                }
                icon={ws.icon}
                path={ws.path}
                current={here === ws.key}
              />
            ))}
          </SidebarNav>
          <RailBoundary label="Engine" resetKey={at}>
            <HealthCard
              reading={healthReading({ connected, authRejected, health })}
              onSetToken={onSetToken}
            />
          </RailBoundary>
          <RailBoundary label="You" resetKey={at}>
            <UserBlock viewer={viewer} seatName={seatName} onSetToken={onSetToken} />
          </RailBoundary>
        </>
      }
    >
      <div className="side-search">
        <SearchTrigger
          variant="rail"
          label="Ask or jump to…"
          title="Search everything"
          onClick={onSearch}
          keyshortcuts="Meta+K Control+K"
          shortcut={<Kbd keys={capsOf(keyRow("palette").presses[0]!)} subtle />}
        />
      </div>
      <SidebarNav label="Navigation">
        <SidebarNav.Group>
          {WORKSPACES.filter((ws) => ws.place === "you").map(row)}
        </SidebarNav.Group>
        <SidebarNav.Group label="Workspace">
          {WORKSPACES.filter((ws) => ws.place === "workspace").map(row)}
        </SidebarNav.Group>
        {/* EACH LIVE SECTION HAS ITS OWN BOUNDARY: they draw rows the
            engine or the browser's storage sent, and a malformed one must
            cost that list and never the navigation above it. */}
        <RailBoundary label="Projects" resetKey={at}>
          <ProjectsSection path={route.path} />
        </RailBoundary>
        <RailBoundary label="Pinned" resetKey={at}>
          <PinnedSection
            path={route.path}
            viewKey={route.query.get("view") ?? ""}
            viewer={viewer.handle}
          />
        </RailBoundary>
        <RailBoundary label="Starred" resetKey={at}>
          <StarredSection path={route.path} />
        </RailBoundary>
      </SidebarNav>
    </AppShell.Rail>
  );
}

/** A figure the engine counted, written as a floor where it stopped counting. */
function figure(claim: { total: number; capped?: boolean }): string {
  return claim.capped ? `${claim.total}+` : String(claim.total);
}

interface NavRowProps {
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
}

/**
 * One row, a plain anchor: a hash link is a real link, so ⌘-click opens a tab.
 *
 * HOVER OR FOCUS STARTS FETCHING WHERE IT LEADS (`lazyScreen.ts`'s
 * `preload`), so the chunk is usually in by the time the click lands — the
 * pointer's hover and the keyboard's focus are both the reader deciding.
 */
function NavRow({ label, icon, glyph, lead, path, query, current, badge, count }: NavRowProps) {
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
      // THE KIT'S OWN ELEMENT, with two listeners added: its class, its
      // `href` and its `aria-current` pass through untouched, so the row is
      // drawn exactly as the kit draws it.
      renderLink={({ className, ...link }) => (
        <a className={className} {...link} onPointerEnter={warm} onFocus={warm} />
      )}
    />
  );
}

/**
 * The company's projects, each as its key and name, with its open work.
 *
 * THE ENGINE'S MAINTAINED CENSUS, not a count over a page: the project row
 * carries `task_counts`, so the sidebar sums what is still to do — waiting and
 * started — with no aggregation of its own. A project the answer carries no
 * census for draws no figure rather than a zero.
 */
function ProjectsSection({ path }: { path: string[] }) {
  const projects = useQuery("work_projects", { limit: PROJECTS_PAGE }, { pollMs: 120_000 });
  const openNewTask = useOpenNewTask();
  const rows = projects.data?.projects ?? [];
  if (rows.length === 0) return null;
  // THE PROJECT A READER IS INSIDE is the one marked — its page, or an item
  // filed in it, whose key names the project — and the one this group's `+`
  // files into. THE ROUTE'S OWN RESOLVER, the one the rail's head `+` asks:
  // the path's second segment with a number stripped read every other Work
  // page as a project, so on `#/work/views` this `+` offered "New task in
  // views" and opened the sheet on a project called `views`, and on a task
  // opened by its uuid it named the uuid.
  const inside = routeProject(path);
  return (
    <SidebarNav.Group
      label="Projects"
      // A TASK IN A PROJECT — the one the reader is inside, preselected, or
      // the sheet's own choice when they are in none. Not "New project": a
      // project is minted by the company configuration, never by hand.
      action={{
        label: inside ? `New task in ${inside}` : "New task in a project",
        icon: <PlusGlyph size="sm" />,
        onClick: () => openNewTask(inside ? { project: inside } : {}),
      }}
    >
      {rows.map((p) => (
        <NavRow
          key={p.key}
          label={p.name || p.key}
          lead={p.key}
          path={["work", p.key]}
          current={inside === p.key}
          count={
            p.task_counts == null ? undefined : { value: unfinished(p.task_counts), label: "open" }
          }
        />
      ))}
    </SidebarNav.Group>
  );
}

/**
 * The views THIS READER pinned, each with the total it selects.
 *
 * ASKED WITH THE VIEWER, which is the whole of the fix this row owes: a pin is
 * a person's, and the strip asked for without one has no pins in it — so the
 * section this replaced asked the shared strip and never drew a pinned row.
 *
 * AND ACROSS EVERY CONTAINER (`work_saved_views`), never the workspace strip: a
 * view pinned from a project board lives in that project, and read from the
 * workspace strip it was in nobody's sidebar. A project's view carries the
 * project's key as its lead, and runs on that project's list.
 * `counts=true` makes the engine run each pinned view's own count in the same
 * read; a view that no longer compiles carries a refusal instead, and draws no
 * figure rather than a wrong one.
 */
function PinnedSection({
  path,
  viewKey,
  viewer,
}: {
  path: string[];
  /** The `view=` the reader is on, which is what marks a pinned row current. */
  viewKey: string;
  viewer: string;
}) {
  const views = useQuery("work_saved_views", viewer ? { viewer, counts: true } : undefined, {
    enabled: viewer !== "",
    pollMs: 120_000,
  });
  const pinned = (views.data?.views ?? []).filter((v) => v.pinned && v.id);
  if (pinned.length === 0) return null;
  return (
    <SidebarNav.Group label="Pinned">
      {pinned.map((v) => {
        // A PIN RUNS THE VIEW. It opened the view's inventory page, which
        // describes the view and offers a button to run it — so the row a
        // reader pinned to get to their work one click away was two clicks
        // away, through a page about the pin. The inventory is still
        // `#/work/views`, one tab over.
        const run = viewRun(v);
        return (
          <NavRow
            key={v.id}
            label={v.name}
            glyph={<PinGlyph size="sm" />}
            lead={v.container.kind === "project" ? v.container.id : undefined}
            path={run.path}
            query={run.query}
            current={samePath(path, run.path) && viewKey === v.key}
            count={
              v.count === undefined
                ? undefined
                : { value: v.count_capped ? `${v.count}+` : v.count, label: "tasks" }
            }
          />
        );
      })}
    </SidebarNav.Group>
  );
}

/**
 * The pages this reader starred, in the order they starred them.
 *
 * DRAWN ONLY WHEN THERE IS ONE. There is nothing to say about a reader who has
 * kept nothing, and the control that fills it is the star in the page header,
 * not here. A stored star whose path no longer resolves is dropped when the
 * list is read (`lib/starred.ts`), so every row here leads somewhere.
 */
function StarredSection({ path }: { path: string[] }) {
  const stars = useStarred();
  if (stars.length === 0) return null;
  return (
    <SidebarNav.Group label="Starred">
      {stars.map((s) => (
        <NavRow
          key={s.path.join("/")}
          label={s.label}
          glyph={<StarGlyph size="sm" />}
          path={s.path}
          current={samePath(path, s.path)}
        />
      ))}
    </SidebarNav.Group>
  );
}
