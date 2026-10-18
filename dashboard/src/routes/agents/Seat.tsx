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
 * act. Where the company runs the engine's own chat and the reader holds it,
 * Direct message opens a conversation with the seat beside Message — a remark
 * rather than an ask (`DirectMessage.tsx`), so it never replaces it. Pause
 * asks whether to stop the current turn too, and says that without it the
 * turn finishes first. "Events" (an agent's only: a person publishes no events
 * under a seat id for `seat=` to narrow by) and "Edit in org" are the
 * secondary pair, in the bar's "More" menu.
 *
 * WHILE IT IS WORKING the bar leads with "Watch live" — the running turn's
 * watch link, its Transcript with the phase it is on open — and the Turns tab
 * carries the pulsing mark that says a turn is running behind it, in words as
 * well as the dot. A reader who came to see what the seat is doing reached it
 * through the Overview's card alone, two screens down on a phone.
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
 * # Who holds a human seat
 *
 * A human seat is held by whoever the identity directory binds to it, and the
 * company document cannot say who that is. So a person's page states it from
 * `GET /iam/seats` through the one [SeatHolding] the peek and the chart's card
 * menu share: the holder's name (and, to an admin and the holder, their login
 * and stage); or the open invitation that holds it, with its cancellation (an
 * admin's to see); or "Nobody holds this seat", with Invite and Create — the
 * two ways a person comes to exist, since every person holds a human seat.
 * Every signed-in reader is answered the listing, masked for them, and the
 * gestures are drawn only for an admin. A read that failed is said rather
 * than drawn as nothing, because the gestures live here and a page without
 * them would say nothing about why.
 *
 * # Status for everybody, the rest by access
 *
 * The page is the OVERVIEW's: anybody signed in reads a seat's state, its
 * figures and its schedules. What it is working on is Worklet's, so the
 * open work (the strip's count, the Overview's card, the Work tab) is asked
 * only of a reader holding Worklet. What it SAID (the Turns and Memory tabs,
 * a call's prompt and answer) is its trail, which the engine answers to an
 * admin and to whoever leads the seat (`seat/profile.ts`' `mayReadTrail`);
 * every other reader finds those tabs drawn and locked, saying what would
 * open them, and nothing is asked.
 *
 * # Tabs are sections
 *
 * They push a history entry, because the reader called them, and the tab is
 * in the URL so a colleague can be sent the exact view. A `tab=` naming a tab
 * this kind has not got lands on Overview (`useTab`).
 */

import { useServicesRun, useServicesAsk } from "~/lib/services.ts";
import { ORG_EDIT_PATH } from "~/app/routes.ts";
import { useId, useMemo, useState } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  EmptyState,
  Menu,
  Skeleton,
  Tabs,
  Tag,
  tabId,
} from "@crewlethq/ui";
import {
  ActivityGlyph,
  ChartNoAxesGanttGlyph,
  CircleQuestionMarkGlyph,
  EllipsisGlyph,
  MessageSquareGlyph,
  PauseGlyph,
  PencilGlyph,
  PlusGlyph,
  SendGlyph,
  UserGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator } from "~/app/router.tsx";
import {
  avatarLabel,
  seatAvatarKey,
  seatKindKey,
  seatPlaceKey,
  seatUnitKey,
} from "~/app/crumbs.ts";
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
import { TechnicalDetails } from "~/components/common.tsx";
import { RaiseBudgetButton } from "~/components/budgetWrite.tsx";
import { waitedOn } from "~/lib/budget.ts";
import { DirectMessageSeatButton } from "./DirectMessage.tsx";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
import { unitPath } from "~/lib/orgchart.ts";
import {
  activityOf,
  awaitingPerson,
  handleLabel,
  indexOrg,
  isOwnSeat,
  nameOfIn,
  statusOf,
  seatPath,
  seatResolvers,
  seatWord,
  shownStateOf,
  stoppedLine,
  failureWords,
  toneOf,
  useSeatSetup,
  type Seat,
} from "~/lib/seats.ts";
import { useAgents, useOrg, useSandboxes } from "~/lib/store-hooks.ts";
import { useViewer } from "~/lib/viewer.ts";
import { ADMIN, WORKLET } from "~/lib/access.ts";
import { canManagePeople, SeatHolding, useHumanSeats } from "~/components/people.tsx";
import type { HumanSeat } from "~/contract/identity.ts";
import type { RestResult } from "~/lib/useRest.ts";
import { GrantRequired } from "~/app/frame/GrantRequired.tsx";
import { turnIdOf, watchHref, watchLink } from "~/lib/turns.ts";
import { useQuery } from "~/lib/useQuery.ts";
import {
  menuHold,
  useDirectSeatAccess,
  useMessageSeatAccess,
  useWriteAccess,
} from "~/lib/useWriteAccess.ts";
import type { RowChrome } from "~/components/work.tsx";
import type { AgentRow } from "~/protocol/index.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { Memory } from "./seat/Memory.tsx";
import { Overview } from "./seat/Overview.tsx";
import {
  seatTabs,
  TAB_LABELS,
  TRAIL_WORDS,
  findSeat,
  liveRow,
  mayReadTrail,
  seatRun,
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
  const viewer = useViewer();
  const org = useOrg();
  const agents = useAgents();
  const now = useNow();
  const index = useMemo(() => indexOrg(org), [org]);
  const seat = findSeat(index, handle);
  const human = seat?.kind === "human";
  // THE TRAIL NAMES THE SEAT, not the slug the URL addresses it by — keyed on
  // the RAW SEGMENT, which is what `crumbsFor` looks up (a link built from a
  // document entry declaring no handle addresses the seat by name). And where it sits: its
  // unit, as a way back to the team, and its kind, which the crumb's badge
  // draws as the outline every other seat badge wears. Nothing for a seat
  // with no name, which would title the tab " · Crewlet".
  usePageLabels(
    seat?.name
      ? {
          [handle]: seat.name,
          [seatKindKey(handle)]: human ? "human" : "agent",
          ...(seat.avatar ? { [seatAvatarKey(handle)]: avatarLabel(seat.avatar) } : {}),
          ...(seat.unit
            ? { [seatUnitKey(handle)]: seat.unit.id, [seatPlaceKey(handle)]: unitPath(seat.unit) }
            : {}),
        }
      : {},
  );
  const runs = useServicesRun();
  const ask = useServicesAsk();
  const [tab, setTab] = useTab<SeatTab>("tab", seatTabs(human, runs));
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
    // WORKLET'S QUESTION, asked only of a reader holding it.
    { enabled: !!seat?.handle && viewer.worklet && ask.tracker, pollMs: 30_000 },
  );
  // WHAT THE SEAT SAID is its lead's and an admin's to read — see the file's
  // doc.
  const trail = !!seat && mayReadTrail(viewer, index, seat.handle);
  // THE GUARDED HALF, read once for every tab that draws it.
  const setup = useSeatSetup(seat?.handle ?? "");
  const chrome: RowChrome = useMemo(() => seatResolvers(index), [index]);
  const nameOf = useMemo(() => nameOfIn(index), [index]);
  // WHAT HOLDS A HUMAN SEAT, read once for the holding callout and the
  // Overview: a seat nobody holds has no person to have a day or open work,
  // so the Overview draws neither. Nothing asked before somebody has said who
  // this is.
  const readsSeats = human && !viewer.asking && !viewer.anonymous;
  const humanSeats = useHumanSeats(readsSeats);
  const holding = humanSeats.data?.find((s) => s.handle === seat?.handle);
  const unheld = holding !== undefined && !holding.holder;

  // WHAT THE PHONE'S "MORE" HOLDS: every action but the one the bar keeps in
  // view (see the file's doc), each opening what its inline control opens.
  const openNewTask = useOpenNewTask();
  const [assigning, setAssigning] = useState(false);
  const [pausing, setPausing] = useState(false);
  const paused = !!agent?.paused;
  // THE TURN TO WATCH, while the engine says the seat is working on one with
  // an id: a working seat a frame ahead of its turn id has nothing to watch.
  const watching = !human && activityOf(agent) === "working" ? turnIdOf(agent) : "";
  // THE BUTTON'S OWN RULE, own-seat hold included, so the folded entry is
  // held in the words the inline control is.
  const messageAccess = useMessageSeatAccess(seat?.handle ?? "");
  const directAccess = useDirectSeatAccess(seat?.handle ?? "");
  const assignAccess = useWriteAccess("update_work_item");
  const pauseAccess = useWriteAccess("pause_seat");
  usePageMenu(
    seat
      ? [
          ...(watching
            ? [
                {
                  key: "watch",
                  label: "Watch live",
                  icon: <ActivityGlyph size="sm" />,
                  onSelect: () => {
                    const watch = watchLink(watching);
                    nav.to(watch.path, watch.query);
                  },
                },
              ]
            : []),
          ...(paused && runs.tracker
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
          ...(directAccess
            ? [
                {
                  key: "direct",
                  label: "Direct message",
                  icon: <SendGlyph size="sm" />,
                  onSelect: () => nav.to(["chatlet"], { dm: seat.handle }),
                  ...menuHold(directAccess),
                },
              ]
            : []),
          // FILING OR HANDING OVER A TASK is Worklet's, and offered only
          // where the company runs it, as the inline buttons are.
          ...(runs.tracker
            ? [
                {
                  key: "assign",
                  label: "Assign task",
                  icon: <PlusGlyph size="sm" />,
                  onSelect: () => setAssigning(true),
                  ...menuHold(assignAccess),
                },
              ]
            : []),
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
            onSelect: () => nav.to(ORG_EDIT_PATH, { seat: seat.handle }),
          },
        ]
      : [],
  );

  if (!seat) {
    return (
      <EmptyState
        icon={<UserGlyph size={32} />}
        title={`No seat called “${handle}”`}
        description="It may have been renamed or removed."
        action={
          <Button variant="primary" onClick={() => nav.to(["company", "org", "roster"])}>
            All seats
          </Button>
        }
      />
    );
  }

  const tabs = seatTabs(human, runs);
  const openWork = work.data?.total_hint;
  return (
    <div className="prof-frame">
      <PageActions>
        {/* WATCH LIVE ONLY WHILE IT IS WORKING, first, as a task page's is.
            Folded on a phone like every action but the one kept in view. */}
        {watching && (
          <span className="page-action-folds">
            <ButtonLink
              size="small"
              variant="secondary"
              leadingIcon={<ActivityGlyph size="sm" />}
              href={watchHref(watching)}
            >
              Watch live
            </ButtonLink>
          </span>
        )}
        {/* FOLDED ON A PHONE into the bar's "More" (published above), all
            but the one action the bar keeps in view. */}
        {/* WORKLET'S TWO WRITES, Message here and Assign task after
            chat's door, and their wrappers with them: where the company runs
            no Worklet the buttons draw nothing, and their empty spans
            still took the bar's gap. */}
        {runs.tracker && (
          <span className={paused ? "page-action-folds" : undefined}>
            {/* HELD ON YOUR OWN SEAT BY ITSELF (`useMessageSeatAccess`), as
                everywhere the button is drawn. */}
            <MessageSeatButton handle={seat.handle} />
          </span>
        )}
        {/* CHATLET'S DOOR, beside Message and never inside the tracker's pair:
            it opens wherever the company runs the engine's own chat, whatever
            it runs for work, and its span is drawn only where the door is. */}
        {directAccess && (
          <span className="page-action-folds">
            <DirectMessageSeatButton handle={seat.handle} />
          </span>
        )}
        {runs.tracker && (
          <span className="page-action-folds">
            <AssignToSeatButton
              handle={seat.handle}
              name={seat.name}
              open={assigning}
              onOpenChange={setAssigning}
            />
          </span>
        )}
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
                onSelect: () => nav.to(ORG_EDIT_PATH, { seat: seat.handle }),
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
        <Tabs
          overflow="menu"
          variant="underline"
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
            // A TURN RUNNING BEHIND THE TAB, by the engine's word for the
            // seat: the strip's pulsing mark, said in words as well.
            ...(value === "turns" && !human && activityOf(agent) === "working"
              ? { live: "running now" }
              : {}),
          }))}
        />
      </header>

      <div className="prof-body">
        {/* A SEAT NOBODY HOLDS says so in its Overview's empty state, with
            the ways to fill it; the notice is for a seat somebody holds, a
            listing that could not say, and the seat's other tabs. */}
        {readsSeats && seat.handle && (!unheld || tab !== "overview") && (
          <Holder seat={{ handle: seat.handle, name: seat.name }} seats={humanSeats} />
        )}
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
              unheld={unheld}
              humanSeats={humanSeats}
            />
          )}
          {tab === "work" &&
            (viewer.loading ? (
              <AccessPending />
            ) : viewer.worklet ? (
              <Work seat={seat} index={index} work={work} chrome={chrome} now={now} />
            ) : (
              <GrantRequired size="compact" grants={[WORKLET]} reader={viewer.login} />
            ))}
          {tab === "turns" &&
            !human &&
            (viewer.loading ? (
              <AccessPending />
            ) : trail ? (
              <Turns seat={seat} agent={agent} now={now} />
            ) : (
              <GrantRequired
                size="compact"
                grants={[ADMIN]}
                reader={viewer.login}
                also={TRAIL_WORDS}
              />
            ))}
          {tab === "memory" &&
            !human &&
            (viewer.loading ? (
              <AccessPending />
            ) : trail ? (
              <Memory seat={seat} now={now} />
            ) : (
              <GrantRequired
                size="compact"
                grants={[ADMIN]}
                reader={viewer.login}
                also={TRAIL_WORDS}
              />
            ))}
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
  const state = human ? undefined : shownStateOf(agent);
  const status = human ? undefined : statusOf(state);
  const place = unitPath(seat.unit);
  return (
    <div className="prof-id">
      <SeatAvatar
        name={seat.name}
        kind={human ? "human" : "agent"}
        avatar={seat.avatar}
        size={60}
        status={status}
        decorative
      />
      <div className="prof-who">
        <div className="prof-title">
          {/* LEVEL TWO: the page's `h1` is the trail's last crumb, which
              names the same seat — two `h1`s reading the same words are two
              page titles to a reader navigating by heading. */}
          <h2 className="prof-name">{seat.name}</h2>
          {/* ONE PILL, THE STATE: the kind is a fact, said first on the line
              under the name, where an outline "Agent" beside the state's pill
              read as two states. */}
          {!human && (
            <Tag variant={toneOf(state)} dot>
              {seatWord(agent)}
            </Tag>
          )}
        </div>
        <p className="prof-line">
          {[human ? "Person" : "Agent", handleLabel(seat.handle), place || "above every unit"]
            .filter(Boolean)
            .join(" · ")}
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
 * What holds this human seat, for every signed-in reader — see the file's
 * doc. Nothing at all, and nothing asked, before somebody has said who this
 * is.
 */
function Holder({
  seat,
  seats,
}: {
  seat: { handle: string; name: string };
  seats: RestResult<HumanSeat[]>;
}) {
  const viewer = useViewer();
  return (
    <div className="prof-notices">
      <SeatHolding
        seat={seat}
        seats={seats}
        manages={canManagePeople(viewer.grants)}
        held={viewer.grants}
        you={isOwnSeat(viewer, seat.handle)}
      />
    </div>
  );
}

/**
 * What stops the seat or waits on somebody, above whichever tab is open: a
 * pause (who, when, why), another stop, a failed last turn, and a coding run
 * parked on a question, with the answer to it one press away.
 *
 * ONE BANNER PER CAUSE. A stop for a fault and the failed turn that came with
 * it were two banners ("server: llm chain exhausted ..." over "This seat is
 * stopped: its model provider is unreachable"), one cause said twice and the
 * first in the engine's machine words. A fault stop now carries its failure in
 * its own banner; a pause is a person's decision and a failed turn beside it
 * is a second cause, so it keeps a banner of its own. Every headline is plain
 * words (`failureWords`); the engine's raw message is behind the kit's
 * disclosure ([TechnicalDetails]).
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
  const state = shownStateOf(agent);
  const paused = agent.paused ?? null;
  const failure = agent.last_error ?? null;
  const sandbox = seatRun(sandboxes, seat);
  const asking = !!sandbox && awaitingPerson(sandbox.status);
  if (!failure && state !== "stopped" && state !== "paused" && !asking) return null;
  // The failed turn's own facts: when, in which phase, and the event that
  // recorded it. Said inside whichever banner owns the cause.
  const facts = failure
    ? [failure.phase && `during ${failure.phase}`, failure.at && relTime(failure.at, now)]
        .filter(Boolean)
        .join(" · ")
    : "";
  const failureFacts = failure && (
    <>
      {facts && ` (${facts})`}
      <TechnicalDetails text={failure.detail} />
    </>
  );
  const eventLink = failure?.event_id ? (
    <a className="t-link" href={href(["live", "events", failure.event_id])}>
      The event
    </a>
  ) : undefined;
  return (
    <div className="prof-notices">
      {/* THE STATE'S OWN TONE, the pill's and the badge's: a stopped seat is
          the one red state in the seat vocabulary, and a warning callout
          under a danger pill named one fact in two colours. */}
      {state === "paused" && (
        <Callout variant={toneOf(state)}>
          {paused ? (
            <>
              <strong>Paused by {nameOf(paused.by)}</strong> {relTime(paused.at, now)}
              {paused.reason ? `: “${sentence(paused.reason)}”` : "."}
            </>
          ) : (
            <>This seat is paused.</>
          )}
        </Callout>
      )}
      {state === "stopped" && (
        <Callout
          variant={toneOf(state)}
          action={
            // A SPENT BUDGET IS RAISED HERE, on the scope refusing the
            // seat, by whoever may; anybody else is told who can.
            agent.stopped_reason === "budget" ? (
              <RaiseBudgetButton
                handle={seat.handle}
                name={seat.name}
                window={waitedOn(agent.budget?.windows, "refusing")}
              />
            ) : (
              eventLink
            )
          }
        >
          This seat is stopped: {stoppedLine(agent, nameOf)}.
          {failure && (
            <>
              {" "}
              {failureWords(failure.kind).sentence}
              {failureFacts}
            </>
          )}
        </Callout>
      )}
      {failure && state !== "stopped" && (
        <Callout variant="danger" action={eventLink}>
          <strong>The last turn failed.</strong> {failureWords(failure.kind).sentence}
          {failureFacts}
        </Callout>
      )}
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

/**
 * A tab whose access is not decided yet — the viewer has not answered — waits
 * for it, rather than drawing a lock on a guess (a refusal nobody made, naming
 * nobody as the reader) or a tab whose reads were never sent.
 */
function AccessPending() {
  return <Skeleton variant="text" rows={4} label="Checking your access" />;
}
