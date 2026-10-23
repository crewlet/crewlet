/**
 * The one sidebar: where a reader can go, and the few things they keep.
 *
 * # What is on it, and what is not
 *
 * The WORKSPACES, from `nav.ts`'s one table, in its groups (`SIDEBAR_GROUPS`):
 * the three that are about you (Home, Inbox, My work), then Company (Overview,
 * Policies, Org chart, Schedules, Integrations, Budgets), then Workspace
 * (Work, Knowledge, Artifacts, Live), and Settings at the foot.
 * Then three LIVE SHORTCUTS, each a real object with its own path: the views
 * this reader pinned, the pages they starred, and the turns running now.
 * Nothing else. A workspace's own sections are tabs in its page header and its
 * objects are rows on the section that lists them; a sidebar that carried
 * either becomes a tree with a different shape on every screen, which is the
 * two-column navigation this replaced. The company's projects are no group
 * here for that reason: a project is a scope of Work, switched where Work's
 * trail names it (`header/ProjectSwitcher.tsx`).
 *
 * # Running is the way to a turn from anywhere
 *
 * Who is working was a figure on the Agents row, whose link is the org chart —
 * so the reader who followed it to the running turn went chart, card, profile,
 * tab, and found the turn there with nothing to say it was moving. The figure
 * is on the LIVE row now, because Live › Now running is what holds the running
 * turns, and under the shortcuts each one is a row of its own that WATCHES it
 * (`lib/turns.ts`' `watchLink`). That group is LAST, so nothing above it moves
 * when a turn starts or ends, and its rows are oldest first, so a turn that
 * starts is appended and the rows a reader is aiming at never reorder.
 *
 * # Every figure says what it counts
 *
 * The kit's `NavItem` takes a figure only with the words that name it, and
 * two kinds of figure are two props: a BADGE is how many things are waiting
 * on the reader (the Inbox's unread primary notices, the one accent fill in
 * the chrome), and a COUNT is how many of something a destination holds (the
 * turns Live is running, a pinned view's total). A
 * figure the engine did not answer is absent, never a zero.
 *
 * # A lock goes where the closed thing is, and a row is hidden only for a service
 *
 * A ROW THAT READS A SERVICE THE COMPANY DOES NOT RUN IS NOT DRAWN: Work and
 * My work where it runs no Crewlet Works, Knowledge where it runs no Crewlet
 * Knowledge (`nav.ts`'s `serviceShown`, over `lib/services.ts`). There is
 * nothing to ask anybody for, and a locked row would say there was.
 *
 * A ROW IS LOCKED ONLY WHEN NOTHING IN ITS WORKSPACE IS OPEN to the reader
 * (`nav.ts`'s `workspaceOpen`, over the one rule `sectionOpen`), and stays: a
 * row that vanished when access is absent is indistinguishable from one that
 * does not exist. Its title names what would open it — the access a reader
 * would ask an admin for: Inbox, My work and Work say Tracker, Knowledge says
 * Knowledge. A workspace only PART of which is closed is not locked as a
 * whole: Settings draws its locks per section, in its column, and is open to
 * every reader through Spend's readings.
 *
 * AND WHAT A LOCKED ROW WOULD SHOW IS NOT ASKED FOR: the Inbox's badge, My
 * work's count and the pinned views are tracker answers, so a reader without
 * Tracker asks none of them (`lib/useInboxCounts.ts`, `lib/useQueueCount.ts`,
 * [PinnedSection]) — a figure beside a lock would be a refusal drawn as a
 * number.
 */

import { useMemo, type ReactNode } from "react";
import {
  AppShell,
  ArtifactKindGlyph,
  BrandLockup,
  Kbd,
  SearchTrigger,
  SidebarNav,
  StatusDot,
  useAppShell,
} from "@crewlethq/ui";
import { ActivityGlyph, KeyGlyph, PinGlyph, StarGlyph } from "@crewlethq/icons/glyphs";
import { href, samePath, useRoute } from "~/app/router.tsx";
import {
  SIDEBAR_GROUPS,
  WORKSPACES,
  pathShown,
  sectionOpen,
  serviceShown,
  workspaceMissing,
  workspaceOf,
  workspaceRow,
  type WorkspaceRow,
} from "~/app/nav.ts";
import { useServicesKnown, useServicesRun } from "~/lib/services.ts";
import { useNeedsYouCount } from "~/lib/decisions.ts";
import { accessNames, type Grant } from "~/lib/access.ts";
import { glyphFor } from "~/ui/glyph.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useHeldGrants, useViewer } from "~/lib/viewer.ts";
import { useAgents, useConnection, useEngineHealth, useOrg } from "~/lib/store-hooks.ts";
import type { AgentAvatar } from "~/lib/avatar.ts";
import { activityOf, indexOrg, statusOf, turnItemKey } from "~/lib/seats.ts";
import { runningShortList, turnIdOf, watchLink } from "~/lib/turns.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { AgentRow } from "~/protocol/index.ts";
import { pageCount, viewRun } from "~/lib/work.ts";
import { useStarred } from "~/lib/starred.ts";
import { artifactPath } from "~/app/routes.ts";
import { usePins } from "~/lib/artifacts/pins.ts";
import { capsOf, keyRow, useKeymap } from "../keymap.ts";
import { inboxFigure, useInboxCounts } from "~/lib/useInboxCounts.ts";
import { useOwnQueueCount } from "~/lib/useQueueCount.ts";
import { EnginePanel, engineReading } from "./EnginePanel.tsx";
import { RailBoundary } from "../boundaries.tsx";
import { preload } from "../lazyScreen.ts";
import { UserBlock } from "./UserBlock.tsx";
import { goSignIn } from "~/lib/session.ts";
import { useKeyHints } from "~/app/layout.ts";

export function Sidebar({ onSearch }: { onSearch: () => void }) {
  const route = useRoute();
  // THE FRAME'S READINGS, not the sidebar's own: see `FrameReadings`.
  const viewer = useViewer();
  // WHAT THE READER HOLDS, for the locks — the frame's one reading of it
  // (`useHeldGrants`), which Settings' column and the route's own refusal read
  // too. Null while nobody has said: a lock drawn on no answer claims a
  // refusal nobody has made.
  const held = useHeldGrants();
  // THE ALARMS' NAMES ARE THE RETENTION REPORT'S, asked only of a reader the
  // section that lists them opens for — by the rule every lock is drawn by —
  // so the panel never asks a question the engine would refuse.
  const readsReport = useMemo(() => {
    const backups = workspaceRow("settings")?.sections.find((s) => s.key === "backups");
    return held !== null && backups !== undefined && sectionOpen(backups, held);
  }, [held]);

  // BELOW THE SHELL BREAKPOINT THE SIDEBAR IS A DRAWER, and Mod+\ opens it —
  // the reader's way to it without reaching for the toggle in the bar. Only
  // there: above the breakpoint the sidebar is always on screen and there is
  // nothing to open. It OPENS rather than toggles, because an open drawer is
  // a modal layer that owns the keyboard (`lib/keys.ts` stands aside for
  // one), and the drawer's own Escape is what closes it.
  const shell = useAppShell();
  useKeymap({ drawer: { run: shell.openDrawer, when: shell.narrow } });
  const keyHints = useKeyHints();
  const org = useOrg();
  const agents = useAgents();
  const health = useEngineHealth();
  const { connected, authRejected, accessRefused, lostAt, everConnected } = useConnection();
  // THE ENGINE REFUSED THIS BROWSER (a seat taken out of the chart) and
  // nothing of the company is offered: the live sections, whose answers an
  // earlier socket may have left behind, are not drawn.
  const unserved = accessRefused !== null;
  const inbox = useInboxCounts();
  const runs = useServicesRun();
  const known = useServicesKnown();
  const needsYou = useNeedsYouCount(viewer.owner !== "" && known && !runs.tracker);
  const shown = (place: WorkspaceRow["place"]) =>
    WORKSPACES.filter((ws) => ws.place === place && serviceShown(ws, runs));
  const here = workspaceOf(route.path);
  const at = route.path.join("/");

  // THE ENGINE'S WORD for working, off the agents push — never a projection's
  // own reading of a seat's call state — in the order every list of running
  // turns draws, read once for the Live row's figure and the Running group —
  // the short list Home's Live now draws too (`runningShortList`).
  const running = useMemo(() => runningShortList(agents), [agents]);
  const working = running.working;

  // THE FIGURE THE QUEUE TAB CARRIES on the reader's own day, read once by
  // the frame for both: see `lib/useQueueCount.ts`.
  const queue = useOwnQueueCount();

  const index = useMemo(() => indexOrg(org), [org]);
  const seatName = index.byHandle.get(viewer.handle)?.name ?? "";
  const names = useMemo(
    () => (row: AgentRow) => index.byHandle.get(row.handle ?? "")?.name ?? row.role,
    [index],
  );
  const avatars = useMemo(
    () => (row: AgentRow) => index.byHandle.get(row.handle ?? "")?.avatar ?? null,
    [index],
  );

  const figures: Partial<Record<WorkspaceRow["key"], Pick<NavRowProps, "badge" | "count">>> = {
    // WHERE THE COMPANY RUNS NO CREWLET WORKS no notice reaches the Inbox,
    // and its badge counts what still does: what waits on the reader's
    // decision ([useNeedsYouCount]).
    inbox: runs.tracker
      ? inbox.waiting !== null && inbox.waiting > 0
        ? { badge: { value: inboxFigure(inbox), label: "unread, waiting on you" } }
        : {}
      : needsYou !== null && needsYou > 0
        ? { badge: { value: String(needsYou), label: "waiting on your decision" } }
        : {},
    // NOTHING ON YOU IS NO FIGURE, as nothing waiting is no badge: a zero
    // beside the row is a mark a reader checks, and it would be there for
    // good on a quiet day.
    me:
      queue && queue.total > 0
        ? // WRITTEN AS THE TAB WRITES IT (`pageCount`), grouped and with a
          // `+` where the engine stopped counting: the row said "10000+"
          // beside a tab that said "10,000+".
          {
            count: {
              value: pageCount(queue.total, queue.capped === true),
              label: "open, assigned to you",
            },
          }
        : {},
    // ON LIVE, NOT AGENTS: a count says how many of something the DESTINATION
    // holds, and Live › Now running holds the running turns, one row each. On
    // the Agents row it pointed a reader looking for a running turn at the
    // org chart.
    live:
      working.length > 0
        ? {
            count: {
              value: working.length,
              label: "working",
              // THE KIT'S STEADY-STATE PULSE, which holds still under reduced
              // motion — the one animated mark in the chrome, on the one state
              // that is ongoing. The Running group's rows carry none of their
              // own: one pulse says "something is running", and a pulse per
              // row would be a column of motion beside the reader's work.
              mark: <StatusDot tone="info" pulse />,
            },
          }
        : {},
  };

  const row = (ws: WorkspaceRow) => {
    const needs = held === null ? [] : workspaceMissing(ws, held);
    return (
      <NavRow
        key={ws.key}
        label={
          needs.length > 0 ? (
            <Locked
              label={ws.navLabel ?? ws.label}
              needs={needs}
              said={`needs ${accessNames(needs)} access`}
            />
          ) : (
            (ws.navLabel ?? ws.label)
          )
        }
        icon={ws.icon}
        path={ws.path}
        current={here === ws.key}
        {...figures[ws.key]}
      />
    );
  };

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
        </div>
      }
      footer={
        <>
          <SidebarNav label="Settings">
            {/* THE LOCK IS ON THE ROW, NOT INSTEAD OF IT — see the module's
                note — and drawn by the same rule as every other row's. */}
            {shown("foot").map(row)}
          </SidebarNav>
          <RailBoundary label="Engine" resetKey={at}>
            <EnginePanel
              reading={engineReading({
                connected,
                authRejected,
                accessRefused,
                lostAt,
                everConnected,
                health,
              })}
              readsReport={readsReport}
              onSignIn={goSignIn}
            />
          </RailBoundary>
          <RailBoundary label="You" resetKey={at}>
            <UserBlock viewer={viewer} seatName={seatName} />
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
          // A KEYCAP ONLY FOR A READER WITH KEYS (`useKeyHints`): in the
          // phone's drawer "Ctrl + K" named a key nobody there has, and cut
          // the trigger's own label to "Ask or jump t…".
          shortcut={
            keyHints ? <Kbd keys={capsOf(keyRow("palette").presses[0]!)} subtle /> : undefined
          }
        />
      </div>
      <SidebarNav label="Navigation">
        {SIDEBAR_GROUPS.map((group) => (
          <SidebarNav.Group key={group.place} {...(group.label ? { label: group.label } : {})}>
            {shown(group.place).map(row)}
          </SidebarNav.Group>
        ))}
        {/* EACH LIVE SECTION HAS ITS OWN BOUNDARY: they draw rows the
            engine or the browser's storage sent, and a malformed one must
            cost that list and never the navigation above it. */}
        {!unserved && (
          <RailBoundary label="Pinned" resetKey={at}>
            <PinnedSection
              path={route.path}
              viewKey={route.query.get("view") ?? ""}
              owner={viewer.owner}
              tracker={viewer.tracker && runs.tracker}
            />
          </RailBoundary>
        )}
        <RailBoundary label="Starred" resetKey={at}>
          <StarredSection path={route.path} />
        </RailBoundary>
        {/* LAST, so a turn starting or ending moves nothing above it. */}
        <RailBoundary label="Running" resetKey={at}>
          <RunningSection path={route.path} running={running} names={names} avatars={avatars} />
        </RailBoundary>
      </SidebarNav>
    </AppShell.Rail>
  );
}

/**
 * A row's label with the lock beside it: its title names what would open it,
 * and a screen reader hears `said` after the label.
 */
function Locked({ label, needs, said }: { label: string; needs: readonly Grant[]; said: string }) {
  return (
    <span className="side-locked" title={`Needs ${accessNames(needs)} access`}>
      {label}
      <KeyGlyph size="xs" aria-hidden="true" />
      <span className="sr-only">, {said}</span>
    </span>
  );
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
 * What THIS READER pinned: their saved views, each with the total it selects,
 * then the artifacts they pinned.
 *
 * THE CALLER'S OWN, and the engine says whose: `work_saved_views` counts the
 * pins of whoever asks — a `viewer=` that named anybody else was a free choice
 * of whose personal views to read, and the engine takes none — so the section
 * asks only once the reader has a record to keep pins under (`owner`), and
 * asks nothing of a reader with none. A saved view is the tracker's, so the
 * section is mounted only for a reader holding Tracker.
 *
 * AND ACROSS EVERY CONTAINER (`work_saved_views`), never the workspace strip: a
 * view pinned from a project board lives in that project, and read from the
 * workspace strip it was in nobody's sidebar. A project's view carries the
 * project's key as its lead, and runs on that project's list.
 * `counts=true` makes the engine run each pinned view's own count in the same
 * read; a view that no longer compiles carries a refusal instead, and draws no
 * figure rather than a wrong one.
 *
 * AN ARTIFACT PIN NEEDS NO GRANT: a share alone admits a reader to Artifacts,
 * so the artifacts the reader pinned are listed for every reader the engine
 * serves, from the one listing the gallery and the page read (`usePins`), and
 * a press anywhere moves this list at once.
 */
function PinnedSection({
  path,
  viewKey,
  owner,
  tracker,
}: {
  path: string[];
  /** The `view=` the reader is on, which is what marks a pinned row current. */
  viewKey: string;
  /** The name the reader's own record is kept under, or "" for nobody's. */
  owner: string;
  /** Whether the reader holds Tracker, which saved views need. */
  tracker: boolean;
}) {
  const views = useQuery(
    "work_saved_views",
    { counts: true },
    {
      enabled: tracker && owner !== "",
      pollMs: 120_000,
    },
  );
  const artifacts = usePins();
  const pinned = tracker ? (views.data?.views ?? []).filter((v) => v.pinned && v.id) : [];
  const pinnedArtifacts = artifacts.data?.items ?? [];
  if (pinned.length === 0 && pinnedArtifacts.length === 0) return null;
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
      {pinnedArtifacts.map((item) => {
        const at = artifactPath(item.id);
        return (
          <NavRow
            key={`artifact:${item.id}`}
            label={item.title}
            glyph={<ArtifactKindGlyph kind={item.kind} size="sm" />}
            path={at}
            current={samePath(path, at)}
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
  // NOT A PLACE THE COMPANY DOES NOT RUN THE SERVICE FOR: its row is drawn
  // nowhere, and its star is kept until it runs it again.
  const runs = useServicesRun();
  const stars = useStarred().filter((s) => pathShown(s.path, runs));
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

/**
 * The turns running now, one row per seat the engine says is WORKING: the
 * seat's badge with its status, its name, the key of the item the turn is
 * charged to, and a link that WATCHES the turn — its Transcript, with the
 * phase it is on open, or a parked turn's coding run live above its phases.
 *
 * THE SAME SET AS THE FIGURE ON LIVE, and THE SAME ROWS AS HOME'S LIVE NOW:
 * both are `runningShortList`, in the order every list of running turns draws
 * (`workingLongestFirst`) — oldest first, so a turn that starts is appended
 * and never shuffles the row a reader is reaching for. A seat that NEEDS a
 * person is not here — it is running nothing, and what it waits on is the
 * Inbox's and Now running's "Waiting on a person" — and a working seat whose
 * turn has published no id yet is skipped rather than linked to nothing, and
 * counted in the last row, which is Now running, where it is listed. That row
 * says "N more running" under rows and "N running" when it stands alone,
 * because "more" than nothing reads as rows that failed to draw.
 *
 * NOTHING HERE TICKS. No elapsed clock and no per-row pulse: the chrome is
 * read on every screen, and a column of moving marks beside a reader's work
 * is motion on every data push. The Live row's one pulsing dot says that
 * something runs; these rows say which, and where to watch it.
 */
function RunningSection({
  path,
  running,
  names,
  avatars,
}: {
  path: string[];
  running: ReturnType<typeof runningShortList>;
  names: (row: AgentRow) => string;
  /** How each running agent is drawn, off the chart; null where it holds no such seat. */
  avatars: (row: AgentRow) => AgentAvatar | null;
}) {
  const { working, shown, more } = running;
  if (working.length === 0) return null;
  return (
    <SidebarNav.Group label="Running">
      {shown.map((row) => {
        const name = names(row);
        const watch = watchLink(turnIdOf(row));
        const item = turnItemKey(row);
        return (
          <NavRow
            key={row.id}
            label={name}
            glyph={
              <SeatAvatar
                name={name}
                kind="agent"
                avatar={avatars(row)}
                size="xs"
                status={statusOf(activityOf(row))}
                decorative
              />
            }
            lead={item || undefined}
            path={watch.path}
            query={watch.query}
            current={samePath(path, watch.path)}
          />
        );
      })}
      {more > 0 && (
        <NavRow
          key="more"
          label={shown.length ? `${more} more running` : `${more} running`}
          glyph={<ActivityGlyph size="sm" />}
          path={["live"]}
          current={false}
        />
      )}
    </SidebarNav.Group>
  );
}
