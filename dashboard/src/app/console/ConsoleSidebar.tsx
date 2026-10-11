/**
 * The admin console's sidebar: the way back to being a person, the console's
 * seven workspaces, what is running, and the engine's own state.
 *
 * # The door back is first
 *
 * "← Nimbus" heads the rail, where the person sidebar draws the company's
 * lockup, and it goes to the PERSON TWIN of what is open (`personPathOf`) —
 * Agent setup's seat to the teammate's profile, the Charter to About, a task's
 * turns to the task — falling back to Today. Beside it, the reader's own
 * Needs-you count: the console is where an admin runs the machine, and the
 * badge is what is waiting on them as a person while they do.
 *
 * # Every section wears its chord
 *
 * The console is a place an admin works fast in, so each workspace row says
 * its `g` chord at its end — the same letters the table assigns (`nav.ts`),
 * read through the key table, never written here a second time.
 *
 * # Running is the way to a turn
 *
 * Under the sections, one row per seat the engine says is WORKING, each a link
 * that watches its turn. The group is LAST, so nothing above it moves when a
 * turn starts or ends, and its rows are oldest first, so a turn that starts
 * is appended and the rows a reader is aiming at never reorder.
 *
 * IN THE CONSOLE'S OWN CHUNK (`app/console/index.ts`), like everything an
 * admin's screens draw: a member's browser never downloads it.
 */

import { useMemo } from "react";
import { AppShell, Kbd, SearchTrigger, SidebarNav, StatusDot, useAppShell } from "@crewlethq/ui";
import { ActivityGlyph, ChevronLeftGlyph, KeyGlyph } from "@crewlethq/icons/glyphs";
import { href, samePath, useRoute } from "~/app/router.tsx";
import { personPathOf, workspaceOf, workspacesOf, type WorkspaceRow } from "~/app/nav.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useAgents, useConnection, useEngineHealth, useOrg } from "~/lib/store-hooks.ts";
import { activityOf, indexOrg, ringOf, turnItemKey } from "~/lib/seats.ts";
import { runningShortList, turnIdOf, watchLink } from "~/lib/turns.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { AgentRow } from "~/protocol/index.ts";
import { inboxFigure, useInboxCounts, waitingWords } from "~/lib/useInboxCounts.ts";
import { capsOf, keyRow, useKeymap } from "~/app/keymap.ts";
import { RailBoundary } from "~/app/boundaries.tsx";
import { HealthCard, healthReading } from "~/app/sidebar/HealthCard.tsx";
import { NavRow } from "~/app/sidebar/NavRow.tsx";
import { UserBlock } from "~/app/sidebar/UserBlock.tsx";

export function ConsoleSidebar({
  onSearch,
  onSetToken,
}: {
  onSearch: () => void;
  onSetToken: () => void;
}) {
  const route = useRoute();
  const viewer = useViewer();
  const shell = useAppShell();
  // THE DRAWER'S KEY, as the person sidebar binds it — whichever sidebar is
  // mounted is the one Mod+\ opens.
  useKeymap({ drawer: { run: shell.openDrawer, when: shell.narrow } });
  const org = useOrg();
  const agents = useAgents();
  const health = useEngineHealth();
  const { connected, authRejected } = useConnection();
  const inbox = useInboxCounts();
  const here = workspaceOf(route.path);
  const at = route.path.join("/");

  // THE ENGINE'S WORD for working, off the agents push, in the order every list
  // of running turns draws — read once for the Live row's figure and the
  // Running group, the short list the Overview's Live now draws too.
  const running = useMemo(() => runningShortList(agents), [agents]);
  const working = running.working;

  const index = useMemo(() => indexOrg(org), [org]);
  const seatName = index.byHandle.get(viewer.handle)?.name ?? "";
  const names = useMemo(
    () => (row: AgentRow) => index.byHandle.get(row.handle ?? "")?.name ?? row.role,
    [index],
  );
  const back = personPathOf(route.path, route.query);
  const company = org?.name || "Crewlet";

  const row = (ws: WorkspaceRow) => (
    <NavRow
      key={ws.key}
      label={
        <span className="console-row">
          <span className="console-row-label">{ws.label}</span>
          <Kbd keys={["g", ws.chord]} subtle />
        </span>
      }
      icon={ws.icon}
      path={ws.path}
      current={here === ws.key}
      // ON LIVE: a count says how many of something the DESTINATION holds, and
      // Now running holds the running turns, one row each.
      {...(ws.key === "live" && working.length > 0
        ? {
            count: {
              value: working.length,
              label: "working",
              // THE KIT'S STEADY-STATE PULSE, which holds still under reduced
              // motion — the one animated mark in the chrome, on the one state
              // that is ongoing.
              mark: <StatusDot tone="info" pulse />,
            },
          }
        : {})}
    />
  );

  return (
    <AppShell.Rail
      header={
        <div className="side-head">
          <a
            className="console-back"
            href={href(back.path, back.query)}
            title={
              inbox.waiting
                ? `${inboxFigure(inbox)} ${waitingWords(inbox)} — back to ${company}`
                : `Back to ${company}`
            }
          >
            <ChevronLeftGlyph size="sm" aria-hidden="true" />
            <span className="console-back-name truncate">{company}</span>
            {inbox.waiting !== null && inbox.waiting > 0 && (
              <span className="console-back-badge t-num">
                {inboxFigure(inbox)}
                <span className="sr-only"> {waitingWords(inbox)}</span>
              </span>
            )}
          </a>
        </div>
      }
      footer={
        <>
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
      <p className="console-title">
        <KeyGlyph size="sm" aria-hidden="true" />
        Admin console
      </p>
      <div className="side-search">
        <SearchTrigger
          variant="rail"
          label={`Search ${company} or jump to…`}
          title="Search everything"
          onClick={onSearch}
          keyshortcuts="Meta+K Control+K"
          shortcut={<Kbd keys={capsOf(keyRow("palette").presses[0]!)} subtle />}
        />
      </div>
      <SidebarNav label="Admin console">
        <SidebarNav.Group>{workspacesOf("admin").map(row)}</SidebarNav.Group>
        {/* LAST, so a turn starting or ending moves nothing above it. */}
        <RailBoundary label="Running" resetKey={at}>
          <RunningSection path={route.path} running={running} names={names} />
        </RailBoundary>
      </SidebarNav>
    </AppShell.Rail>
  );
}

/**
 * The turns running now, one row per seat the engine says is WORKING: the
 * seat's badge with its ring, its name, the key of the item the turn is
 * charged to, and a link that WATCHES the turn — its Transcript, with the
 * phase it is on open, or a parked turn's coding run live above its phases.
 *
 * THE SAME SET AS THE FIGURE ON LIVE, and THE SAME ROWS AS THE OVERVIEW'S LIVE
 * NOW: both are `runningShortList`, oldest first. A seat that NEEDS a person
 * is not here — it is running nothing — and a working seat whose turn has
 * published no id yet is skipped rather than linked to nothing, and counted
 * in the last row, which is Now running. That row says "N more running" under
 * rows and "N running" when it stands alone, because "more" than nothing
 * reads as rows that failed to draw.
 *
 * NOTHING HERE TICKS: no elapsed clock and no per-row pulse. The Live row's
 * one pulsing dot says something runs; these rows say which.
 */
function RunningSection({
  path,
  running,
  names,
}: {
  path: string[];
  running: ReturnType<typeof runningShortList>;
  names: (row: AgentRow) => string;
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
                size="xs"
                ring={ringOf(activityOf(row))}
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
          path={["admin", "live"]}
          current={false}
        />
      )}
    </SidebarNav.Group>
  );
}
