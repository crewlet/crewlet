/**
 * A teammate's profile in the person shell — an agent's or a person's: who
 * they are, what they are doing, the work they hold, and the three things a
 * reader does about them — message them, give them work, pause an agent.
 *
 * # Overview and Work, and how it runs is the console's
 *
 * A head with the seat's badge, name, kind and state and the line that places
 * it (handle, unit, manager); a tab strip of the two tabs the person shell
 * draws for every seat (`seat/profile.ts`'s `PERSON_TABS`); and the tab's
 * panel. Its turns, memory, schedules and settings are how the machine RUNS
 * the seat, and they are the console's Agent setup
 * (`routes/admin/agents/Seat.tsx`) — the same seat, one door away for an admin
 * (`nav.ts`'s `adminPathOf`), and no tab of its own here for anybody.
 *
 * # The actions are the page bar's
 *
 * Message (a task that ASKS the seat, filed through the New task sheet),
 * Assign task (an existing task handed over conditionally, or a new one),
 * and Pause / Resume — each a `WriteButton`, so a reader who cannot make the
 * change sees the control and the reason rather than a page with no way to
 * act. Which seats a reader may pause is the tool's rule over the chart, and
 * its refusal names who may.
 *
 * ON A PHONE THE BAR KEEPS ONE ACTION IN VIEW, the frame's rule: Message, or
 * Resume on a paused seat — the one thing to do about a seat that is holding
 * its mail — and the rest fold into the frame's one "More" (`usePageMenu`),
 * disabled there with the same sentence the inline control is held with.
 *
 * A person is messaged and assigned work like anybody else and is never
 * paused: the engine runs no turn for them, so there is nothing to hold.
 *
 * # Tabs are sections
 *
 * They push a history entry, because the reader called them, and the tab is
 * in the URL so a colleague can be sent the exact view. A `tab=` naming a tab
 * this page has not got lands on Overview (`useTab`).
 */

import { useId, useMemo, useState } from "react";
import { Button, Callout, EmptyState, Tag, tabId } from "@crewlethq/ui";
import { MessageSquareGlyph, PauseGlyph, PlusGlyph, UserGlyph } from "@crewlethq/icons/glyphs";
import { href, useNavigator } from "~/app/router.tsx";
import { seatKindKey, seatPlaceKey, seatUnitKey } from "~/app/crumbs.ts";
import { ObjectTabs } from "~/app/frame/ObjectTabs.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { usePageLabels, usePageMenu } from "~/app/page.tsx";
import { useOpenNewTask } from "~/app/newTask.ts";
import { AssignToSeatButton, MessageSeatButton, PauseSeatButton } from "~/components/writes.tsx";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
import { unitPath } from "~/lib/orgchart.ts";
import {
  activityOf,
  handleLabel,
  indexOrg,
  nameOfIn,
  pausedAt,
  pauserOf,
  ringOf,
  seatPath,
  seatResolvers,
  stoppedLine,
  toneOf,
  type Seat,
} from "~/lib/seats.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { menuHold, useWriteAccess } from "~/lib/useWriteAccess.ts";
import type { RowChrome } from "~/components/work.tsx";
import type { AgentRow } from "~/protocol/index.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { Overview } from "./seat/Overview.tsx";
import {
  PERSON_TABS,
  TAB_LABELS,
  liveRow,
  pillWord,
  sentence,
  type SeatTab,
} from "./seat/profile.ts";
import { Work } from "./seat/Work.tsx";

/** How many open tasks the Work tab's list, and the Overview's card, are read from. */
const WORK_PAGE = 50;

export function SeatScreen({ handle }: { handle: string }) {
  const nav = useNavigator();
  const org = useOrg();
  const agents = useAgents();
  const now = useNow();
  const index = useMemo(() => indexOrg(org), [org]);
  // BY THE HANDLE ALONE (`OrgIndex.byHandle`): a name, a runtime id or the
  // handle in another case is no address, and lands on "Nobody called".
  const seat = index.byHandle.get(handle);
  const human = seat?.kind === "human";
  // THE TRAIL NAMES THE SEAT, not the slug the URL addresses it by — keyed on
  // the RAW SEGMENT, which is what `crumbsFor` looks up. And where it sits:
  // its unit, as a way back to the team, and its kind, which the crumb's badge
  // draws as the outline every other seat badge wears. Nothing for a seat
  // with no name, which would title the tab " · Crewlet".
  usePageLabels(
    seat?.name
      ? {
          [handle]: seat.name,
          [seatKindKey(handle)]: human ? "human" : "agent",
          ...(seat.unit
            ? { [seatUnitKey(handle)]: seat.unit.name, [seatPlaceKey(handle)]: unitPath(seat.unit) }
            : {}),
        }
      : {},
  );
  const [tab, setTab] = useTab<SeatTab>("tab", PERSON_TABS);
  const panelId = useId();
  const agent = liveRow(agents, seat);
  // THE OPEN WORK ON THE SEAT, asked once for the strip's count, the
  // Overview's card and the Work tab's list — three readings of one answer,
  // so they can never name three numbers.
  //
  // EVERY ROW FILTERED ON ITS OWN (`subtasks: separate`): the grammar's
  // default filters a ROOT and lets its whole subtree ride along, which on a
  // seat's page listed the sub-tasks of an epic it owns under other people's
  // names as its own.
  const work = useQuery(
    "work_items",
    {
      assignee: seat?.handle ?? "",
      status_group: "not_started,active",
      subtasks: "separate",
      sort: "-priority,updated",
      limit: WORK_PAGE,
    },
    { enabled: !!seat?.handle, pollMs: 30_000 },
  );
  const chrome: RowChrome = useMemo(() => seatResolvers(index), [index]);
  const nameOf = useMemo(() => nameOfIn(index), [index]);

  // WHAT THE PHONE'S "MORE" HOLDS: every action but the one the bar keeps in
  // view (see the file's doc), each opening what its inline control opens.
  const openNewTask = useOpenNewTask();
  const [assigning, setAssigning] = useState(false);
  const [pausing, setPausing] = useState(false);
  const paused = activityOf(agent) === "stopped" && agent?.stopped_reason === "paused";
  const messageAccess = useWriteAccess("create_work_item");
  const assignAccess = useWriteAccess("update_work_item");
  const pauseAccess = useWriteAccess("pause_seat");
  usePageMenu(
    seat
      ? [
          ...(paused
            ? [
                {
                  key: "message",
                  label: "Message",
                  icon: <MessageSquareGlyph size="sm" />,
                  onSelect: () => openNewTask({ assignee: seat.handle, ask: seat.handle }),
                  ...menuHold(messageAccess),
                },
              ]
            : []),
          {
            key: "assign",
            label: "Assign task",
            icon: <PlusGlyph size="sm" />,
            onSelect: () => setAssigning(true),
            ...menuHold(assignAccess),
          },
          ...(human || paused
            ? []
            : [
                {
                  key: "pause",
                  label: "Pause",
                  icon: <PauseGlyph size="sm" />,
                  onSelect: () => setPausing(true),
                  ...menuHold(pauseAccess),
                },
              ]),
        ]
      : [],
  );

  if (!seat) {
    return (
      <EmptyState
        icon={<UserGlyph size={32} />}
        title={`Nobody called “${handle}”`}
        description="Teammates are addressed by handle. If the org chart just changed, this one may have been renamed or removed."
        action={
          <Button variant="primary" onClick={() => nav.to(["team", "people"])}>
            Everybody
          </Button>
        }
      />
    );
  }

  const openWork = work.data?.total_hint;
  return (
    <div className="prof-frame">
      <PageActions>
        {/* FOLDED ON A PHONE into the bar's "More" (published above), all
            but the one action the bar keeps in view. */}
        <span className={paused ? "page-action-folds" : undefined}>
          <MessageSeatButton handle={seat.handle} />
        </span>
        <span className="page-action-folds">
          <AssignToSeatButton
            handle={seat.handle}
            name={seat.name}
            open={assigning}
            onOpenChange={setAssigning}
          />
        </span>
        {!human && (
          <span className={paused ? undefined : "page-action-folds"}>
            <PauseSeatButton
              handle={seat.handle}
              name={seat.name}
              paused={paused}
              working={activityOf(agent) === "working"}
              open={pausing}
              onOpenChange={setPausing}
            />
          </span>
        )}
      </PageActions>

      <header className="prof-head">
        <SeatHead seat={seat} agent={agent} />
        <ObjectTabs
          ariaLabel={`${seat.name}'s sections`}
          className="prof-tabs"
          value={tab}
          onValueChange={(next) => setTab(next as SeatTab)}
          panelId={panelId}
          items={PERSON_TABS.map((value) => ({
            value,
            label: TAB_LABELS[value],
            // THE ENGINE'S COUNT of the seat's open work, the same answer the
            // Overview's card and the Work tab read.
            ...(value === "work" && openWork !== undefined ? { count: openWork } : {}),
          }))}
        />
      </header>

      <div className="prof-body">
        <Notices seat={seat} agent={agent} now={now} nameOf={nameOf} />
        {/* THEIR STRIP, OUR PANEL: the kit's `TabPanel` spreads nothing, so
            it cannot take the `tabIndex={0}` that puts the content in the tab
            order — and without it a reader who picks a tab and presses Tab
            leaves the widget past everything they chose. */}
        <div
          className="tabpanel"
          role="tabpanel"
          id={panelId}
          aria-labelledby={tabId(panelId, tab)}
          tabIndex={0}
        >
          {tab === "overview" && (
            <Overview
              seat={seat}
              agent={agent}
              index={index}
              work={work}
              nameOf={nameOf}
              now={now}
            />
          )}
          {tab === "work" && <Work seat={seat} work={work} chrome={chrome} now={now} />}
        </div>
      </div>
    </div>
  );
}

/** The badge, the name, the kind and state, and the line that places the seat. */
function SeatHead({ seat, agent }: { seat: Seat; agent: AgentRow | undefined }) {
  const human = seat.kind === "human";
  const state = human ? undefined : activityOf(agent);
  const ring = human ? undefined : ringOf(state);
  const place = unitPath(seat.unit);
  return (
    <div className="prof-id">
      <SeatAvatar
        name={seat.name}
        kind={human ? "human" : "agent"}
        size={60}
        {...(ring ? { ring } : {})}
        decorative
      />
      <div className="prof-who">
        <div className="prof-title">
          {/* LEVEL TWO: the page's `h1` is the trail's last crumb, which
              names the same seat — two `h1`s reading the same words are two
              page titles to a reader navigating by heading. */}
          <h2 className="prof-name">{seat.name}</h2>
          <Tag appearance="outline">{human ? "Person" : "Agent"}</Tag>
          {!human && (
            <Tag variant={toneOf(state)} dot>
              {pillWord(agent)}
            </Tag>
          )}
        </div>
        <p className="prof-line">
          {[handleLabel(seat.handle), place || "above every unit"].filter(Boolean).join(" · ")}
          {seat.manager ? (
            <>
              {" · reports to "}
              <a className="prof-line-link" href={href(seatPath(seat.manager, "person"))}>
                {seat.manager.name}
              </a>
            </>
          ) : null}
          {human && seat.availability ? ` · ${seat.availability}` : ""}
        </p>
      </div>
    </div>
  );
}

/**
 * What stops the seat, above whichever tab is open: a pause — who, when and,
 * where the reader is sent it, why — or another stop. What a coding run is
 * waiting on reaches the person it asks through their own decisions, on
 * Today and in the Inbox, and the company-wide list of them is the console's.
 */
function Notices({
  seat,
  agent,
  now,
  nameOf,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  now: number;
  nameOf: (key: string) => string;
}) {
  if (seat.kind === "human" || !agent || activityOf(agent) !== "stopped") return null;
  const state = activityOf(agent);
  if (agent.stopped_reason === "paused") {
    const by = pauserOf(agent);
    const at = pausedAt(agent);
    const reason = agent.paused?.reason;
    return (
      <div className="prof-notices">
        <Callout variant={toneOf(state)}>
          <strong>{by ? `Paused by ${nameOf(by)}` : "Paused"}</strong>
          {at ? ` ${relTime(at, now)}` : ""}
          {reason ? ` — “${sentence(reason)}”` : "."} It starts no new work; what is sent to it
          waits in order, and its routines are skipped.
        </Callout>
      </div>
    );
  }
  return (
    <div className="prof-notices">
      <Callout variant={toneOf(state)}>
        This agent is stopped: {stoppedLine(agent, nameOf)}.
      </Callout>
    </div>
  );
}
