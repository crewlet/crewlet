/**
 * The one sidebar: where a reader can go, and the few things they keep.
 *
 * # What is on it, and what is not
 *
 * The WORKSPACES, from `nav.ts`'s one table, in its groups (`SIDEBAR_GROUPS`):
 * the three that are about you (Home, Inbox, My work), then Company
 * (Principles, Brand, Org chart, Schedules, Integrations, Budgets), then
 * Workspace (Worklet, Chatlet, Doclet, Artifacts, Live), and Settings at the foot.
 * Then three LIVE SHORTCUTS, each a real object with its own path: the views
 * this reader pinned, the pages they starred, and the turns running now.
 * Nothing else. A workspace's own sections are tabs in its page header and its
 * objects are rows on the section that lists them; a sidebar that carried
 * either becomes a tree with a different shape on every screen, which is the
 * two-column navigation this replaced. The company's projects are no group
 * here for that reason: a project is a scope of Worklet, switched where
 * Worklet's trail names it (`header/ProjectSwitcher.tsx`).
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
 * A ROW THAT READS A SERVICE THE COMPANY DOES NOT RUN IS NOT DRAWN: Worklet
 * and My work where it runs no Worklet, Doclet where it runs no Doclet
 * (`nav.ts`'s `serviceShown`, over `lib/services.ts`). There is
 * nothing to ask anybody for, and a locked row would say there was.
 *
 * A ROW IS LOCKED ONLY WHEN NOTHING IN ITS WORKSPACE IS OPEN to the reader
 * (`nav.ts`'s `workspaceOpen`, over the one rule `sectionOpen`), and stays: a
 * row that vanished when access is absent is indistinguishable from one that
 * does not exist. Its title names what would open it — the access a reader
 * would ask an admin for: Inbox, My work and Worklet say Worklet, Doclet says
 * Doclet, Chatlet says Chatlet. A workspace only PART of which is closed is not
 * locked as a whole: Settings draws its locks per section, in its column, and
 * is open to every reader through Spend's readings.
 *
 * AND WHAT A LOCKED ROW WOULD SHOW IS NOT ASKED FOR: the Inbox's badge, My
 * work's count and the pinned views are Worklet's answers, so a reader without
 * Worklet asks none of them (`lib/useInboxCounts.ts`, `lib/useQueueCount.ts`,
 * [PinnedSection]): a figure beside a lock would be a refusal drawn as a
 * number.
 *
 * # Two shapes from one list
 *
 * Above the shell breakpoint the reader can fold the sidebar to the kit's
 * compact rail and back, with the kit's fold control or `Mod+\`, and the
 * choice is theirs to keep (`railShape.ts`). FOLDED, it is the kit's
 * `CompactNav`: Search, then the places about you, each its glyph over its
 * name, then each named group folded into ONE tile (Company, Workspace) whose
 * flyout lists its places; Settings and the reader's face stand under the
 * foot's rule, and the engine's status at the top of the foot. Both shapes
 * are drawn from [placeOf], so they list the same places with the same
 * figures, links and locks, and a group's tile carries its places' badges and
 * says when the page on screen is one of them. The live shortcuts (Pinned,
 * Starred, Running) are the full sidebar's: the folded rail is the places,
 * and one press unfolds the rest.
 *
 * Below the breakpoint the sidebar is the kit's drawer, which has the room
 * for the full list, so it is always the full shape there and the key opens
 * it.
 */

import { Fragment, useMemo } from "react";
import {
  AppShell,
  ArtifactKindGlyph,
  BrandLockup,
  CompactNav,
  CompactNavGroup,
  CompactNavItem,
  CompactNavSeparator,
  Kbd,
  SearchTrigger,
  SidebarNav,
  StatusDot,
  useAppShell,
  type NavItemProps,
} from "@crewlethq/ui";
import { ActivityGlyph, PinGlyph, SearchGlyph, StarGlyph } from "@crewlethq/icons/glyphs";
import { href, samePath, useRoute } from "~/app/router.tsx";
import {
  SIDEBAR_GROUPS,
  WORKSPACES,
  landingPath,
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
import { groupLock, lockReason } from "~/lib/access.ts";
import { glyphFor } from "@crewlethq/icons/glyphs/registry";
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
import { ariaKeysOf, capsOf, keyRow, pressText, useKeymap } from "../keymap.ts";
import { inboxFigure, useInboxCounts } from "~/lib/useInboxCounts.ts";
import { attentionFigure, useChatAttention } from "~/lib/chatRooms.ts";
import { useOwnQueueCount } from "~/lib/useQueueCount.ts";
import { EnginePanel, engineReading } from "./EnginePanel.tsx";
import { RailBoundary } from "../boundaries.tsx";
import { preload } from "../lazyScreen.ts";
import { UserBlock, type FootPlace } from "./UserBlock.tsx";
import { goSignIn } from "~/lib/session.ts";
import { useKeyHints } from "~/app/layout.ts";
import { toggleRailShape } from "./railShape.ts";

/**
 * What the search control is called in either shape: the full sidebar's field
 * and the compact rail's Search tile open the same palette, so they name it
 * with the same words.
 */
const SEARCH_LABEL = "Search everything";

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

  // MOD+\ IS THE SIDEBAR'S KEY IN EITHER SHAPE OF THE FRAME. Where the
  // sidebar stands beside the page it folds it and unfolds it, as the kit's
  // fold control does. Below the shell breakpoint the sidebar is a drawer, and
  // the key OPENS it rather than toggling it, because an open drawer is a
  // modal layer that owns the keyboard (`lib/keys.ts` stands aside for one),
  // and the drawer's own Escape is what closes it; the stored shape is left
  // alone there, since the drawer has nothing to fold.
  const shell = useAppShell();
  useKeymap({ sidebar: () => (shell.narrow ? shell.openDrawer() : toggleRailShape()) });
  // THE SHAPE DRAWN NOW, which is the kit's to say: the reader's choice above
  // the breakpoint, and always the full list in the drawer.
  const compact = shell.rail === "compact";
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
  const chat = useChatAttention();
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

  const figures: Partial<Record<WorkspaceRow["key"], Pick<NavItemProps, "badge" | "count">>> = {
    // WHERE THE COMPANY RUNS NO WORKLET no notice reaches the Inbox,
    // and its badge counts what still does: what waits on the reader's
    // decision ([useNeedsYouCount]).
    inbox: runs.tracker
      ? inbox.waiting !== null && inbox.waiting > 0
        ? { badge: { value: inboxFigure(inbox), label: "unread, waiting on you" } }
        : {}
      : needsYou !== null && needsYou > 0
        ? { badge: { value: String(needsYou), label: "waiting on your decision" } }
        : {},
    // WHAT WAITS ON THE READER IN CHATLET: the unread messages that name
    // them, and every unread direct message, in the rooms they have not muted
    // (`lib/chatRooms.ts`). A badge, because each of them is addressed to the
    // reader; none where the counts were not measured or nothing waits.
    chatlet: chat
      ? {
          badge: {
            value: attentionFigure(chat),
            label: "unread mentions and direct messages",
          },
        }
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

  // ONE CURRENT ROW. A pin, a star or a running turn that opens exactly this
  // page is the row the reader followed, and the workspace row containing it
  // gives the mark up: both carried the current fill and `aria-current`.
  // FOLDED, NO SHORTCUT IS DRAWN, so none takes the mark from its place, and
  // the pins are not asked for until the sidebar unfolds to draw them.
  const viewKey = route.query.get("view") ?? "";
  const pinned = usePinned({
    owner: viewer.owner,
    worklet: viewer.worklet && runs.tracker,
    enabled: !unserved && !compact,
  });
  // NOT A PLACE THE COMPANY DOES NOT RUN THE SERVICE FOR: its star's row is
  // drawn nowhere, and the star is kept until it runs it again.
  const stars = useStarred().filter((s) => pathShown(s.path, runs));
  const shortcutHere =
    !compact &&
    ((!unserved && pinnedHere(pinned, route.path, viewKey)) ||
      stars.some((s) => samePath(route.path, s.path)) ||
      running.shown.some((row) => samePath(route.path, watchLink(turnIdOf(row)).path)));

  const lockOf = (ws: WorkspaceRow) => lockReason(held === null ? [] : workspaceMissing(ws, held));
  // ONE PLACE, AS EVERY SHAPE DRAWS IT: the full row, the compact tile and a
  // folded group's flyout link all spread this, so the two shapes cannot list
  // a place with a different link, lock or figure.
  const placeOf = (ws: WorkspaceRow) => ({
    label: ws.label,
    // LOCKED, NEVER HIDDEN, by the one rule every lock is drawn by; in a
    // group the reader holds none of, the group's label carries the key.
    locked: lockOf(ws),
    icon: <WorkspaceGlyph icon={ws.icon} />,
    // WHERE THE ROW OPENS for this reader ([landingPath]): Settings' row
    // opens the first of its sections the reader may open.
    ...linkTo(landingPath(ws, held, runs)),
    current: here === ws.key && !shortcutHere,
    ...figures[ws.key],
  });
  const footPlace = (ws: WorkspaceRow): FootPlace => {
    const { label, locked, icon, href, onPrefetch, current } = placeOf(ws);
    return {
      key: ws.key,
      label,
      icon,
      href,
      onPrefetch,
      current,
      locked,
      title: locked ? `${label}. ${locked}` : label,
    };
  };
  const row = (ws: WorkspaceRow) => <SidebarNav.Item key={ws.key} {...placeOf(ws)} />;
  // THE NAMED GROUPS ARE PARTED FROM THE PLACES ABOUT YOU BY ONE RULE, named
  // for the groups it introduces.
  const named = SIDEBAR_GROUPS.filter((group) => group.label !== "");
  const palette = keyRow("palette").presses[0]!;

  return (
    <AppShell.Rail
      // THE COMPANY IS THE TITLE, beside the product's mark: the lockup the
      // approved design draws ("Nimbus", no second line). The kit's own
      // default puts the product's name first and the company under it,
      // reasoning that a home link named by the product reads the same on
      // every deployment; on this screen the company is the subject, one
      // browser is one company, and the product is in the mark and the tab.
      // "Crewlet" stands in only while no company has been sent. The rail's
      // head gives the lockup its width, so a long name truncates.
      header={
        <BrandLockup
          name={org?.name || "Crewlet"}
          href={href(["home"])}
          // A LITERAL PATH under the dashboard's own `base`, which the dev
          // server serves itself, written out so `protocol/proxy.test.ts`
          // can read it and hold it against the proxy table.
          mark={<img src="/static/dashboard/crewlet-icon.svg" alt="" />}
        />
      }
      // THE RAIL'S FIELD, in the kit's own slot on the nav's columns. The
      // compact rail draws no field: its Search tile opens the same palette.
      search={
        <SearchTrigger
          variant="rail"
          label={SEARCH_LABEL}
          onClick={onSearch}
          keyshortcuts="Meta+K Control+K"
          // A KEYCAP ONLY FOR A READER WITH KEYS (`useKeyHints`): in the
          // phone's drawer "Ctrl + K" named a key nobody there has, and cut
          // the trigger's own label short.
          shortcut={keyHints ? <Kbd keys={capsOf(palette)} subtle /> : undefined}
        />
      }
      // THE ENGINE'S STATE ON THE NAME'S LINE, compact: a dot and the
      // alarms, its words in its name and title, the panel below it. In the
      // compact rail the kit stands it at the top of the foot, and the panel
      // opens above it.
      status={
        <RailBoundary label="Engine" resetKey={at} compact>
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
            side={compact ? "top" : "bottom"}
          />
        </RailBoundary>
      }
      // THE FOOT IS THE READER'S OWN ROW, and the places it reaches by their
      // glyphs (Settings) stand beside it, before Preferences: each a real
      // link to where its row opened, saying so when the reader is there,
      // fetching it on the pointer, and naming its lock, never hidden.
      footer={
        <RailBoundary label="You" resetKey={at}>
          <UserBlock
            viewer={viewer}
            seatName={seatName}
            places={shown("foot").map(footPlace)}
            compact={compact}
          />
        </RailBoundary>
      }
    >
      {compact ? (
        <CompactNav label="Navigation">
          {/* SEARCH FIRST, where the full sidebar's field stands: the same
              palette, named by the same words, with its key in the tip. */}
          <CompactNavItem
            label="Search"
            icon={<SearchGlyph size="sm" />}
            title={keyHints ? `${SEARCH_LABEL} (${pressText(palette)})` : SEARCH_LABEL}
            keyshortcuts={ariaKeysOf(palette)}
            onClick={onSearch}
          />
          {SIDEBAR_GROUPS.map((group) =>
            group.label === "" || !group.icon ? (
              shown(group.place).map((ws) => <CompactNavItem key={ws.key} {...placeOf(ws)} />)
            ) : (
              <Fragment key={group.place}>
                {group === named[0] && (
                  <CompactNavSeparator label={named.map((g) => g.label).join(" and ")} />
                )}
                <CompactNavGroup
                  label={group.label}
                  icon={<WorkspaceGlyph icon={group.icon} />}
                  links={shown(group.place).map((ws) => ({ key: ws.key, ...placeOf(ws) }))}
                />
              </Fragment>
            ),
          )}
        </CompactNav>
      ) : (
        <SidebarNav label="Navigation">
          {SIDEBAR_GROUPS.map((group) => (
            <SidebarNav.Group
              key={group.place}
              {...(group.label ? { label: group.label } : {})}
              // ONE KEY FOR A GROUP THE READER HOLDS NONE OF, on its label.
              locked={groupLock(shown(group.place).map(lockOf))}
            >
              {shown(group.place).map(row)}
            </SidebarNav.Group>
          ))}
          {/* EACH LIVE SECTION HAS ITS OWN BOUNDARY: they draw rows the
            engine or the browser's storage sent, and a malformed one must
            cost that list and never the navigation above it. */}
          {!unserved && (
            <RailBoundary label="Pinned" resetKey={at}>
              <PinnedSection path={route.path} viewKey={viewKey} pinned={pinned} />
            </RailBoundary>
          )}
          <RailBoundary label="Starred" resetKey={at}>
            <StarredSection path={route.path} stars={stars} />
          </RailBoundary>
          {/* LAST, so a turn starting or ending moves nothing above it. */}
          <RailBoundary label="Running" resetKey={at}>
            <RunningSection path={route.path} running={running} names={names} avatars={avatars} />
          </RailBoundary>
        </SidebarNav>
      )}
    </AppShell.Rail>
  );
}

/** A workspace's glyph, by its name in the table. */
function WorkspaceGlyph({ icon }: { icon: WorkspaceRow["icon"] }) {
  const Glyph = glyphFor(icon);
  return <Glyph size="sm" />;
}

/**
 * Where a row goes, as the kit's row takes it: a plain hash link (a real
 * link, so a modified press opens a tab), and the fetch of where it leads,
 * started when the pointer reaches the row or the keyboard focuses it
 * (`lazyScreen.ts`'s `preload`), so the chunk is usually in by the press.
 */
function linkTo(path: string[], query?: Record<string, string>) {
  return { href: href(path, query), onPrefetch: () => preload(path) };
}

/**
 * What THIS READER pinned: their saved views, each with the total it selects,
 * then the artifacts they pinned.
 *
 * THE CALLER'S OWN, and the engine says whose: `work_saved_views` counts the
 * pins of whoever asks — a `viewer=` that named anybody else was a free choice
 * of whose personal views to read, and the engine takes none — so the section
 * asks only once the reader has a record to keep pins under (`owner`), and
 * asks nothing of a reader with none. A saved view is Worklet's, so the
 * section is mounted only for a reader holding Worklet.
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
function usePinned({
  owner,
  worklet,
  enabled,
}: {
  /** The name the reader's own record is kept under, or "" for nobody's. */
  owner: string;
  /** Whether the reader holds Worklet and the company runs it, which saved views need. */
  worklet: boolean;
  /** False where the engine serves this browser nothing at all. */
  enabled: boolean;
}) {
  const views = useQuery(
    "work_saved_views",
    { counts: true },
    {
      enabled: enabled && worklet && owner !== "",
      pollMs: 120_000,
    },
  );
  const artifacts = usePins(enabled);
  return {
    views: worklet ? (views.data?.views ?? []).filter((v) => v.pinned && v.id) : [],
    artifacts: artifacts.data?.contents ?? [],
  };
}

type Pinned = ReturnType<typeof usePinned>;

/** Whether one of the reader's pins opens exactly the page they are on. */
function pinnedHere(pinned: Pinned, path: readonly string[], viewKey: string): boolean {
  return (
    pinned.views.some((v) => samePath(path, viewRun(v).path) && viewKey === v.key) ||
    pinned.artifacts.some((item) => samePath(path, artifactPath(item.id)))
  );
}

function PinnedSection({
  path,
  viewKey,
  pinned: { views: pinned, artifacts: pinnedArtifacts },
}: {
  path: string[];
  /** The `view=` the reader is on, which is what marks a pinned row current. */
  viewKey: string;
  /** What the reader pinned ([usePinned]). */
  pinned: Pinned;
}) {
  if (pinned.length === 0 && pinnedArtifacts.length === 0) return null;
  return (
    <SidebarNav.Group label="Pinned">
      {pinned.map((v) => {
        // A PIN RUNS THE VIEW. It opened the view's inventory page, which
        // describes the view and offers a button to run it — so the row a
        // reader pinned to get to their work one click away was two clicks
        // away, through a page about the pin. The inventory is still
        // `#/worklet/views`, one tab over.
        const run = viewRun(v);
        return (
          <SidebarNav.Item
            key={v.id}
            label={v.name}
            icon={<PinGlyph size="sm" />}
            lead={v.container.kind === "project" ? v.container.id : undefined}
            {...linkTo(run.path, run.query)}
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
          <SidebarNav.Item
            key={`artifact:${item.id}`}
            label={item.title}
            icon={<ArtifactKindGlyph kind={item.kind} size="sm" />}
            {...linkTo(at)}
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
function StarredSection({ path, stars }: { path: string[]; stars: ReturnType<typeof useStarred> }) {
  if (stars.length === 0) return null;
  return (
    <SidebarNav.Group label="Starred">
      {stars.map((s) => (
        <SidebarNav.Item
          key={s.path.join("/")}
          label={s.label}
          icon={<StarGlyph size="sm" />}
          {...linkTo(s.path)}
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
          <SidebarNav.Item
            key={row.id}
            label={name}
            icon={
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
            {...linkTo(watch.path, watch.query)}
            current={samePath(path, watch.path)}
          />
        );
      })}
      {more > 0 && (
        <SidebarNav.Item
          key="more"
          label={shown.length ? `${more} more running` : `${more} running`}
          icon={<ActivityGlyph size="sm" />}
          {...linkTo(["live"])}
          current={false}
        />
      )}
    </SidebarNav.Group>
  );
}
