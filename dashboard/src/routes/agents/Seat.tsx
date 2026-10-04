/**
 * A seat's profile: who it is, what it is doing, and the three things a
 * reader does about it — message it, give it work, pause it.
 *
 * THE APPROVED AGENT ARTBOARD. A head with the seat's badge, name, kind and
 * state and the line that places it (handle, unit, manager); a tab strip
 * under it whose set is decided by the seat's KIND (`seat/profile.ts`); and
 * the tab's panel below, each tab its own module in `seat/`.
 *
 * # The actions are the page bar's
 *
 * Message (a task that ASKS the seat, filed through the New task sheet),
 * Assign task (an existing task handed over conditionally, or a new one),
 * and Pause / Resume — each a `WriteButton`, so a reader who cannot make the
 * change sees the control and the reason rather than a page with no way to
 * act. Pause asks whether to stop the current turn too, and says that
 * without it the turn finishes first. "Events" (an agent's only: a person
 * publishes no events under a seat id for `seat=` to narrow by) and "Edit in
 * org" are the secondary pair, in the bar's "More" menu.
 *
 * ON A PHONE THE BAR KEEPS ONE ACTION IN VIEW, the frame's rule: Message, or
 * Resume on a paused seat — the one thing to do about a seat that is holding
 * its mail — and the rest fold into the frame's one "More" (`usePageMenu`),
 * disabled there with the same sentence the inline control is held with.
 * Three buttons and the header's "who is working" chip were 453px on a 366px
 * line, and the "More" that held Events and Edit in org sat off screen.
 *
 * A person is messaged and assigned work like anybody else and is never
 * paused: the engine runs no turn for them, so there is nothing to hold.
 *
 * # Tabs are sections
 *
 * They push a history entry, because the reader called them, and the tab is
 * in the URL so a colleague can be sent the exact view. A `tab=` naming a tab
 * this kind has not got lands on Overview (`useTab`).
 */

import { useId, useMemo, useState } from "react";
import { Button, Callout, EmptyState, Menu, Tag, tabId } from "@crewlethq/ui";
import {
  ChartNoAxesGanttGlyph,
  CircleQuestionMarkGlyph,
  EllipsisGlyph,
  MessageSquareGlyph,
  PauseGlyph,
  PencilGlyph,
  PlusGlyph,
  UserGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator } from "~/app/router.tsx";
import { seatKindKey, seatPlaceKey, seatUnitKey } from "~/app/crumbs.ts";
import { ObjectTabs } from "~/app/frame/ObjectTabs.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { usePageLabels, usePageMenu } from "~/app/Shell.tsx";
import { useOpenNewTask } from "~/app/newTask.ts";
import {
  AnswerRunButton,
  AssignToSeatButton,
  MessageSeatButton,
  PauseSeatButton,
} from "~/components/writes.tsx";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
import { unitPath } from "~/lib/orgchart.ts";
import {
  activityOf,
  awaitingPerson,
  handleLabel,
  indexOrg,
  nameOfIn,
  ringOf,
  seatPath,
  seatResolvers,
  stoppedLine,
  toneOf,
  useSeatSetup,
  type Seat,
} from "~/lib/seats.ts";
import { useAgents, useOrg, useSandboxes } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { menuHold, useWriteAccess } from "~/lib/useWriteAccess.ts";
import type { RowChrome } from "~/components/work.tsx";
import type { AgentRow } from "~/protocol/index.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { Memory } from "./seat/Memory.tsx";
import { Overview } from "./seat/Overview.tsx";
import {
  AGENT_TABS,
  HUMAN_TABS,
  TAB_LABELS,
  findSeat,
  liveRow,
  pillWord,
  sentence,
  type SeatTab,
} from "./seat/profile.ts";
import { Schedules } from "./seat/Schedules.tsx";
import { Settings } from "./seat/Settings.tsx";
import { Turns } from "./seat/Turns.tsx";
import { Work } from "./seat/Work.tsx";

/** How many open tasks the Work tab's list, and the Overview's card, are read from. */
const WORK_PAGE = 50;

export function SeatScreen({ handle }: { handle: string }) {
  const nav = useNavigator();
  const org = useOrg();
  const agents = useAgents();
  const now = useNow();
  const index = useMemo(() => indexOrg(org), [org]);
  const seat = findSeat(index, handle);
  const human = seat?.kind === "human";
  // THE TRAIL NAMES THE SEAT, not the slug the URL addresses it by — keyed on
  // the RAW SEGMENT, which is what `crumbsFor` looks up (a seat the engine
  // reported no handle for is addressed by name). And where it sits: its
  // unit, as a way back to the team, and its kind, which the crumb's badge
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
  const [tab, setTab] = useTab<SeatTab>("tab", human ? HUMAN_TABS : AGENT_TABS);
  const panelId = useId();
  const agent = liveRow(agents, handle, seat);
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
  // THE GUARDED HALF, read once for every tab that draws it.
  const setup = useSeatSetup(seat ? seat.handle || seat.name : "");
  const chrome: RowChrome = useMemo(() => seatResolvers(index), [index]);
  const nameOf = useMemo(() => nameOfIn(index), [index]);

  // WHAT THE PHONE'S "MORE" HOLDS: every action but the one the bar keeps in
  // view (see the file's doc), each opening what its inline control opens.
  const openNewTask = useOpenNewTask();
  const [assigning, setAssigning] = useState(false);
  const [pausing, setPausing] = useState(false);
  const paused = !!agent?.paused;
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
          ...(human
            ? []
            : [
                {
                  key: "events",
                  label: "Events",
                  icon: <ChartNoAxesGanttGlyph size="sm" />,
                  onSelect: () => nav.to(["live", "events"], { seat: seat.handle }),
                },
              ]),
          {
            key: "edit",
            label: "Edit in org",
            icon: <PencilGlyph size="sm" />,
            onSelect: () => nav.to(["agents", "edit"], { seat: seat.handle || seat.name }),
          },
        ]
      : [],
  );

  if (!seat) {
    return (
      <EmptyState
        icon={<UserGlyph size={32} />}
        title={`No seat called “${handle}”`}
        description="Seats are addressed by handle. If a company revision was just applied, this seat may have been renamed or removed."
        action={
          <Button variant="primary" onClick={() => nav.to(["agents", "roster"])}>
            All seats
          </Button>
        }
      />
    );
  }

  const tabs = human ? HUMAN_TABS : AGENT_TABS;
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
        <span className="page-action-folds">
          <Menu
            label={`More on ${seat.name}`}
            icon={<EllipsisGlyph size="sm" />}
            triggerVariant="ghost"
            align="end"
            items={[
              ...(human
                ? []
                : [
                    {
                      key: "events",
                      label: "Events",
                      description: "Everything the engine published about this seat",
                      icon: <ChartNoAxesGanttGlyph size="sm" />,
                      onSelect: () => nav.to(["live", "events"], { seat: seat.handle }),
                    },
                  ]),
              {
                key: "edit",
                label: "Edit in org",
                description: "Change this seat in the org editor",
                icon: <PencilGlyph size="sm" />,
                onSelect: () => nav.to(["agents", "edit"], { seat: seat.handle || seat.name }),
              },
            ]}
          />
        </span>
      </PageActions>

      <header className="prof-head">
        <SeatHead seat={seat} agent={agent} />
        {/* WHAT DOES NOT FIT FOLDS INTO "MORE", the tab the reader is on
            always drawn: scrolled, a phone's strip hid Settings past its edge
            with nothing to say it was there. */}
        <ObjectTabs
          ariaLabel={`${seat.name}'s sections`}
          className="prof-tabs"
          value={tab}
          onValueChange={(next) => setTab(next as SeatTab)}
          panelId={panelId}
          items={tabs.map((value) => ({
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
              reading={setup.reading}
              nameOf={nameOf}
              now={now}
            />
          )}
          {tab === "work" && (
            <Work seat={seat} index={index} work={work} chrome={chrome} now={now} />
          )}
          {tab === "turns" && !human && <Turns seat={seat} agent={agent} now={now} />}
          {tab === "memory" && !human && <Memory seat={seat} now={now} />}
          {tab === "schedules" && !human && <Schedules seat={seat} now={now} />}
          {tab === "settings" && <Settings seat={seat} agent={agent} setup={setup} />}
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
              <a className="prof-line-link" href={href(seatPath(seat.manager))}>
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
 * What stops the seat or waits on somebody, above whichever tab is open: the
 * last error, a pause (who, when, why), another stop, and a coding run parked
 * on a question — with the answer to it one press away.
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
  const sandboxes = useSandboxes();
  if (seat.kind === "human" || !agent) return null;
  const state = activityOf(agent);
  const paused = agent.paused ?? null;
  const sandbox = sandboxes.find((s) => s.agent_handle === seat.handle) ?? null;
  const asking = !!sandbox && awaitingPerson(sandbox.status);
  if (!agent.last_error && state !== "stopped" && !asking) return null;
  return (
    <div className="prof-notices">
      {agent.last_error && (
        <Callout
          variant="danger"
          action={
            agent.last_error.event_id ? (
              <a className="t-link" href={href(["live", "events", agent.last_error.event_id])}>
                The event
              </a>
            ) : undefined
          }
        >
          <strong>{agent.last_error.kind || "error"}</strong> — {agent.last_error.message}
          {agent.last_error.phase && ` (during ${agent.last_error.phase})`}
          {agent.last_error.at && ` · ${relTime(agent.last_error.at, now)}`}
        </Callout>
      )}
      {/* THE STATE'S OWN TONE, the pill's and the ring's: a stopped seat is
          the one red state in the seat vocabulary, and a warning callout
          under a danger pill named one fact in two colours. */}
      {state === "stopped" &&
        (agent.stopped_reason === "paused" && paused ? (
          <Callout variant={toneOf(state)}>
            <strong>Paused by {nameOf(paused.by)}</strong> {relTime(paused.at, now)}
            {paused.reason ? ` — “${sentence(paused.reason)}”` : "."} It starts no new turn; what is
            sent to it waits on its inbox, in order, and its scheduled runs are skipped.
            {paused.stop_running ? " The turn it was on was stopped." : ""}
          </Callout>
        ) : (
          <Callout variant={toneOf(state)}>
            This seat is stopped: {stoppedLine(agent, nameOf)}.
          </Callout>
        ))}
      {sandbox && asking && (
        <Callout
          variant="warning"
          icon={<CircleQuestionMarkGlyph size="md" />}
          action={
            <AnswerRunButton
              turnId={sandbox.turn_id}
              seat={seat.name}
              question={sandbox.question ?? ""}
            />
          }
        >
          A coding run is waiting on a question: {sandbox.question || "(no question recorded)"}
        </Callout>
      )}
    </div>
  );
}
