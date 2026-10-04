/**
 * One person's day — the claims on their attention, as the sections of My
 * work.
 *
 * # Seven claims, and each is a different one
 *
 * A person who saw only their assignments would miss six other things asking
 * for their time: what a lead put at the top of their list, the questions
 * waiting on an answer from them, the questions THEY are waiting on, the
 * sub-items they claimed on somebody else's task, the work they were brought
 * onto without owning, what moved on what they follow, and what became
 * workable while they were not looking.
 *
 * # Why they are sections, and what keeps the old promise
 *
 * They were seven stacked cards under four stat tiles that restated the counts
 * of the cards below them, and each card was ABSENT when empty — so the page's
 * shape changed with the day and the block that mattered was wherever it
 * happened to fall. A person with two hundred assignments never saw their
 * asks.
 *
 * The promise the stacking was making — that no block can crowd out another —
 * is kept by the page header's SECTION TABS rather than by the page: every
 * section carries its count, always, so an unanswered question is visible as a
 * number on a section nobody has opened. A section with nothing in it is drawn
 * and says so, which stacking could not do without seven "nothing here" panels
 * burying the one that had something. Each is a PATH (`#/me/asked-of-me`),
 * because a place a reader goes is an address rather than a `tab=` on another
 * one; the queue and the order somebody put it in are one section, `#/me`,
 * read `order=due` or `order=priorities`.
 *
 * # A sparse day is still somebody's day
 *
 * THE HEADER DRAWS EVERY SECTION AND ITS COUNT WHATEVER THE DAY HOLDS, zeros
 * included, and that does not change on a company's first week. A tab that
 * vanished when it was empty is what this screen was rebuilt to stop. A branch
 * keyed on the counts summing to zero would also be wrong about its own
 * subject — the sum is ONE PERSON's day, so a quiet seat inside a busy company
 * would be told the company was empty.
 *
 * What a sparse day gets instead is a page that says something TRUE before its
 * first count: the banner names whose day this is, their seat and the way to
 * it, and carries the one thing on this screen that asks for an
 * acknowledgement. Each empty section names what would fill it, in that
 * person's voice. And a count is WITHHELD rather than drawn as a zero while its
 * read is in flight, because "nothing is on you" is a claim and it is false
 * until an answer arrives.
 *
 * # The Inbox is not one of them
 *
 * What REACHED somebody is a different question from what is ON them, and it
 * has a workspace of its own (`#/inbox`).
 *
 * # Two sections are the work list, narrowed to one person
 *
 * The Queue and Asked by me are `routes/work/ItemsView.tsx` — the same
 * component `#/work` and a project's Items lens are — held here by an
 * [ItemsHost] that fixes the one thing the section IS (who holds the work; who
 * is waiting on an answer about it) and what it opens on, and leaves the shape,
 * the grouping, the order, the columns and every other filter to the reader,
 * in the URL, exactly as on the other two screens. Written as a second
 * renderer the Queue had no Filter menu, no Display menu, no scope switch, no
 * chips, no count line and no way past its two hundredth row.
 *
 * THE QUEUE OPENS BANDED BY WHEN. "What have I missed, what is today, what is
 * this week" is the question somebody opens their own work to ask. The bands
 * are the ENGINE's `due:bucket` axis, cut against the COMPANY's day start like
 * the row's own overdue flag, so the heading a task is under and the flag
 * beside it can never disagree.
 *
 * # It writes, as the reader — on their own day
 *
 * The questions put to a person are answered on their row (`DecisionRow`), and
 * their priorities are reordered by dragging a row (`Priorities.tsx`), each as
 * the person signed in (ADR-0024). On SOMEBODY ELSE's day every
 * control here is HELD, disabled with the sentence that says whose day it is
 * (`HoldWrites`): what is asked of Rui is Rui's to answer — the engine refuses
 * anybody else — and the work on Rui's queue is changed from the work screens,
 * where it is the company's rather than one person's. The ONE exception is the
 * one the tracker grants a lead: reordering the queue of somebody in their
 * line, which is released exactly where it is drawn.
 *
 * # Whose day it is decides the pronoun, everywhere
 *
 * Including the wake reasons and the decision lines. "You are the approver" on
 * an operator reading a report's day is a sentence about the reader, who is not
 * on the list.
 */

import { useMemo } from "react";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import type { MeSection } from "~/app/routes.ts";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { usePageCoverage, usePageMenu, useSectionCounts } from "~/app/Shell.tsx";
import { ChecklistClaims, Coverage, RowList, type RowChrome } from "~/components/work.tsx";
import { AskList, decisionsHref } from "~/components/DecisionRow.tsx";
import { Callout, EmptyState, Select, Skeleton, Tag, type SelectOption } from "@crewlethq/ui";
import { KeyGlyph, UserGlyph } from "@crewlethq/icons/glyphs";
import { Segmented } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, leadsInLine, seatResolvers, type OrgIndex, type Seat } from "~/lib/seats.ts";
import { plural, relTime } from "~/lib/format.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { HoldWrites } from "~/lib/useWriteAccess.ts";
import { useNow } from "~/lib/clock.ts";
import { askedByParams, pageCount, type Scope, itemPath } from "~/lib/work.ts";
import { ItemsView, type ItemsHost } from "~/routes/work/ItemsView.tsx";
import type {
  WorkClaimTotal,
  WorkMyWork,
  WorkPersonState,
  WorkloadAnswer,
  WorkSummary,
} from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PriorityQueue } from "./Priorities.tsx";
import { listTotal, useOwnQueueCount, useQueueCountRead } from "~/lib/useQueueCount.ts";

/**
 * What the Queue OPENS on, as the list's own parameters.
 *
 * A MODULE CONSTANT rather than a literal in the render, which
 * [ItemsHost.opens] requires: the list memoises its question on this object,
 * so a new one every render is a new question every render.
 *
 * `due:bucket` is the engine's relative due axis — Overdue, Earlier, Today,
 * This week, Later, No due date — cut against the company's own day. `due`
 * orders what is inside each band by the same date the band was cut on, so a
 * band reads soonest first. Both are DEFAULTS: the Display menu overrides
 * either, and the address keeps whichever the reader chose.
 */
const ASSIGNED_OPENS: Record<string, string> = { group_by: "due:bucket", sort: "due" };

/**
 * What Asked by me OPENS on: every status, most recently changed first.
 *
 * EVERY STATUS, because the filter the section IS — a question this person
 * asked that is still waiting for its answer — is already the "still open"
 * test, and it is about the QUESTION rather than the task: a question left
 * unanswered on a task somebody finished is exactly the one most likely to be
 * forgotten, and the Open segment would hide it. The scope switch still
 * narrows it, like every other default here.
 *
 * MOST RECENTLY CHANGED FIRST, because a task somebody touched is the one
 * where an answer may be about to arrive.
 */
const ASKED_OPENS: Record<string, string> = { show_closed: "true", sort: "-updated" };

/**
 * A COUNT READ ASKS FOR ONE ROW. The two tabs the tracker counts (the Queue and
 * Asked by me) read `total_hint`, which the engine counts over the whole
 * matching set whatever the page is — so the rows are waste. The Queue's count
 * asked for a full default page (fifty rows, with every card fact) every thirty
 * seconds to read one number off it. The Queue's own question is
 * `lib/useQueueCount.ts`'s, shared with the sidebar.
 */
const COUNT_ONLY = 1;

/**
 * What the Queue section draws: the assignments banded by when they are due,
 * or the order somebody put them in. A SECTION switch (`order=` pushes), so
 * Back walks out through it like any other place the reader called.
 */
const ORDERS = ["due", "priorities"] as const;
type Order = (typeof ORDERS)[number];

/** Which panel is drawn: a section of My work, with the queue split by order. */
type Tab = "assigned" | "priorities" | Exclude<MeSection, "queue">;

export function MyWork({ section }: { section: MeSection }) {
  const org = useOrg();
  const now = useNow();
  const nav = useNavigator();
  const [handle, setHandle] = useParam("handle", "");
  const [orderParam, setOrder] = useParam("order", "due", "section");
  const order: Order = (ORDERS as readonly string[]).includes(orderParam)
    ? (orderParam as Order)
    : "due";
  const tab: Tab =
    section === "queue" ? (order === "priorities" ? "priorities" : "assigned") : section;
  // EVERY SEAT AND EVERY PERSON the chart names, so the screen can be reached
  // with nobody chosen and still offer somebody.
  const index = useMemo(() => indexOrg(org), [org]);
  const chrome: RowChrome = seatResolvers(index);
  // WHOSE DAY THIS IS, resolved rather than guessed.
  //
  // This fell back to the ALPHABETICALLY FIRST SEAT, so a screen titled "My
  // work" rendered a stranger's day to everybody. The `viewer` question
  // answers it: the engine resolves this browser to a principal, and the
  // identity directory binds that principal to a seat.
  //
  // An explicit choice still wins — a lead reading a report's day is a real
  // thing to do, and the header says whose day it is either way.
  //
  // YOURS IS `owner`, NOT `handle`: the record their priorities, their asks
  // and their assistant's marks are kept under is their seat when the
  // directory binds them to one and their login when it does not. Read by
  // `handle`, an unbound reader had no day here at all while their assistant
  // wrote their priorities under their login. And the record that hands
  // somebody work, asks them something or reorders their queue is the record
  // that writes them a notice, so the frame that moves the reader's inbox
  // (`inbox_changed`) asks their day again (`refetchOnInboxOf`); on a
  // report's day no frame arrives — the frame watches the viewer's own record
  // — and the poll keeps it current.
  const viewer = useViewer();
  const whose = handle || viewer.owner;
  const ownDay = whose !== "" && whose === viewer.owner;
  const they = ownDay ? "you" : "them";
  const whoseName = index.byHandle.get(whose)?.name ?? whose;

  // WHICH CHANGES THIS READER MAY MAKE HERE — see the file head. On their own
  // day, all of them; on anybody else's, none but a lead's reorder.
  const holds = dayHolds({
    ownDay,
    name: whoseName,
    lead: ownDay ? null : leadsInLine(index, viewer.handle, whose),
  });

  // WHO IS CARRYING HOW MUCH, for the picker. One read over every handle at
  // once — the alternative is a `work_my_work` per colleague, which is a round
  // trip per option — and it is the whole company's rather than this person's,
  // so it does not move when the day being read does.
  //
  // POLLED ONLY WHERE THE PICKER IS DRAWN, which is the same condition: an
  // anonymous reader is offered no picker. The FIRST read still goes out,
  // because until the viewer answers this reader is indistinguishable from one
  // who gets a picker. 60s is the interval both other readers of this question
  // already take (`routes/inbox/Inbox.tsx`, `routes/agents/People.tsx`): a load
  // is read, not watched.
  const workload = useQuery("work_workload", undefined, {
    enabled: !viewer.anonymous,
    pollMs: 60_000,
  });

  // NOT UNTIL SOMEBODY IS CHOSEN — `whose` is empty until the viewer has
  // answered, and the engine refuses this question without a handle.
  const state = useQuery("work_my_work", whose ? { handle: whose } : undefined, {
    enabled: whose !== "",
    pollMs: 30_000,
    refetchOnInboxOf: whose,
  });
  const mine = state.data;

  // THE TWO SECTIONS THAT ARE THE WORK LIST, counted as the tracker's own
  // question, asked whichever section is open: a count that appeared only once
  // its section was opened would be a strip that changes as you walk it.
  //
  // THE SAME QUESTION EACH SECTION'S LIST ASKS, and deliberately not the same
  // READ: this one carries no arrangement at all, so the number stays put
  // while the reader groups, sorts and narrows the list under it — a count
  // that moved with the filters would answer "how much is on this person" with
  // "how much is on screen". And EVERY TASK ON ITS OWN, as the list counts
  // them (`subtasks=separate`): in the grammar's default a root this person
  // holds brings its whole subtree along unfiltered, so the count took in
  // sub-tasks held by somebody else, and finished ones, the list never drew.
  //
  // THE QUEUE'S IS THE FRAME'S ON THE READER'S OWN DAY — the one reading the
  // sidebar's My work figure draws, so the two can never name two numbers —
  // and this screen's own read of the same question on anybody else's.
  const ownQueue = useOwnQueueCount();
  const theirQueue = useQueueCountRead(whose, !ownDay);
  const askedBy = useQuery(
    "work_items",
    whose
      ? {
          container: "workspace",
          ...askedByParams(whose),
          show_closed: ASKED_OPENS.show_closed,
          subtasks: "separate",
          limit: COUNT_ONLY,
        }
      : undefined,
    // AN ANSWER TO ONE OF THEIR QUESTIONS IS A NOTICE TO THEM, so the frame
    // that moves their inbox moves this count too.
    { enabled: whose !== "", pollMs: 30_000, refetchOnInboxOf: whose },
  );

  // AND THIS PERSON'S OWN RECORD, read ONCE for the page: the stamp it carries
  // (somebody ELSE ordered this queue) feeds the banner on every section and
  // the mark on the order switch, and its stored list and version are what a
  // reorder is made from and conditional on.
  const person = useQuery("work_person", whose ? { handle: whose } : undefined, {
    enabled: whose !== "",
    pollMs: 60_000,
    refetchOnInboxOf: whose,
  });

  // AND THE FRAME GETS THE COVERAGE OF THE READ THAT IDENTIFIES THIS OBJECT.
  // Only a SCREEN publishes — the frame holds one slot with one setter — and
  // `#/me`'s object is a PERSON, so the person's own record is the answer whose
  // honesty belongs in the state bar. `work_my_work` states its own beside the
  // sections it fills, and the lists theirs inside themselves.
  usePageCoverage(person.data);

  // WHAT THE TWO LIST SECTIONS ARE, handed to the list that draws them — see
  // [ItemsHost]. Memoised on the values they read, because the list asks the
  // engine again whenever the lock moves and a fresh object every render would
  // be a fresh question every render.
  const queueHost = useMemo<ItemsHost>(
    () => ({
      lock: { assignee: whose },
      opens: ASSIGNED_OPENS,
      empty: (scope: Scope) => assignedEmpty(scope, they),
    }),
    [whose, they],
  );
  const askedHost = useMemo<ItemsHost>(
    () => ({
      lock: { asked_by: whose },
      opens: ASKED_OPENS,
      empty: (scope: Scope) => askedEmpty(scope, they),
    }),
    [whose, they],
  );

  // EVERY COUNT, ALWAYS, on the section tabs the page header draws — which is
  // what replaces the stacking: a section nobody has opened still says how
  // much is on it. A count is a STRING, because a count the engine stopped at
  // its ceiling is a floor — `pageCount` writes it with a `+`, which a bare
  // number would report as a fact about somebody's day — and it is absent,
  // never "0", while its read is in flight.
  const listed = { assigned: ownDay ? ownQueue : theirQueue, askedBy: listTotal(askedBy.data) };
  useSectionCounts(
    mine
      ? figures({
          queue: countFor("assigned", mine, listed),
          "asked-of-me": countFor("asked-of-me", mine, listed),
          "asked-by-me": countFor("asked-by-me", mine, listed),
          unblocked: countFor("unblocked", mine, listed),
          collaborating: countFor("collaborating", mine, listed),
          watching: countFor("watching", mine, listed),
          checklist: countFor("checklist", mine, listed),
        })
      : {},
  );

  // THE PRIORITIES ARE THE QUEUE, READ IN SOMEBODY'S ORDER, so the way to them
  // from any section is the queue with that order — whose day it is travels
  // with it, since it is the one thing on this page that is not the section's.
  const showPriorities = () =>
    nav.to(["me"], { order: "priorities", ...(handle ? { handle } : {}) });
  // THE TWO WAYS OUT, as the bar's "More" offers them on a phone — the same
  // two links the bar draws inline everywhere wider.
  usePageMenu([
    { key: "inbox", label: "Inbox", onSelect: () => nav.to(["inbox"]) },
    { key: "all-work", label: "All work", onSelect: () => nav.to(["work"]) },
  ]);
  const setBy = person.data?.priorities_set_by;
  const setByName = setBy ? (chrome.seatName?.(setBy) ?? setBy) : "";
  const workHref = (row: WorkSummary) => href(itemPath(row));

  return (
    <>
      {/* THE BAR HOLDS WHAT YOU CAN DO. Whose day this is is what the object
          IS, so it is the band above the sections rather than a pill up here —
          see [WhoseDay]. */}
      <PageActions>
        {/* IN THE PAGE BAR RATHER THAN IN A SIDEBAR. Whose day this is a
            FILTER on the screen you are on, and the product's grammar puts a
            filter in the page's own bar and keeps the sidebar for destinations
            with paths of their own (`app/nav.ts`).

            NOT OFFERED TO A READER THE ENGINE WILL REFUSE. `work_my_work` and
            `work_person` are scoped: naming anybody's handle needs a
            credential, and a caller presenting none gets `errNotYours` — so
            for an anonymous reader every row here is a refusal, and the empty
            state below names the one remedy there is instead. */}
        {!viewer.anonymous && (
          <Select
            width="auto"
            value={whose}
            onChange={(value) => setHandle(String(value))}
            ariaLabel="Whose day"
            placeholder="Pick somebody"
            // THE ACCENT SAYS A FILTER IS ON, and reading your own day is the
            // page's default rather than a filter: it was violet at rest, so
            // the page always looked narrowed. Somebody else's day is.
            active={handle !== "" && handle !== viewer.owner}
            options={whoseDayOptions(index, viewer, workload.data)}
            // TWO LINES AN OPTION — the name, then the handle and the open
            // count — where the kit's panel is sized for six ONE-line rows.
            menuClassName="whose-day-menu"
          />
        )}
        {/* THE QUEUE'S TWO READINGS. A section switch rather than a filter:
            the priorities are a different panel, the order a lead put the
            queue in, and the flag on it is the one mark on this page that
            asks to be seen — somebody else ordered this queue. The title
            names the PERSON, as the banner does, never the handle. */}
        {section === "queue" && (
          <Segmented<Order>
            ariaLabel="Order the queue by"
            value={order}
            onChange={setOrder}
            options={[
              { value: "due", label: "Due", icon: "calendar" },
              {
                value: "priorities",
                label: "Priorities",
                icon: setBy ? "flag" : "list",
                title: setBy ? `Ordered by ${setByName}` : undefined,
              },
            ]}
          />
        )}
        {/* FOLDED ON A PHONE into the bar's "More" (published above): the
            whose-day picker and the Queue's order switch are what a reader
            uses here, and with the two links beside them the bar ran past a
            390px screen's edge. */}
        <span className="page-action-folds row gap-3">
          <a className="t-link" href={href(["inbox"])}>
            Inbox →
          </a>
          <a className="t-link" href={href(["work"])}>
            All work →
          </a>
        </span>
      </PageActions>
      {/* THREE STATES, and they are not one empty state. A reader nobody has
          signed in, a reader the directory binds to no seat, and a reader who
          simply has not chosen somebody need three different sentences. The
          unbound reader is not an EMPTY state: they have a day of their own,
          kept under their login, and what they lack is the work the chart
          addresses to a seat — which is what the note says, above the day
          rather than instead of it. */}
      {ownDay && viewer.unbound && (
        <Callout variant="info">
          You are <code className="inline">{viewer.login}</code> and not bound to a seat, so this is
          the day kept under that login: what you follow, what names you, and your own priorities.
          Work the org chart hands to a seat reaches you once an administrator binds you to one (
          <code className="inline">crewlet iam bind</code>).
        </Callout>
      )}
      {!whose &&
        (viewer.anonymous ? (
          <EmptyState
            icon={<KeyGlyph size={32} />}
            title="Nobody is signed in"
            description="A day belongs to a person, and this browser has not said who it is. Sign in: a day is somebody's own record, so reading one — yours or anybody's — is a question the engine answers for a caller it can name."
          />
        ) : (
          <EmptyState
            icon={<UserGlyph size={32} />}
            title="Nobody chosen"
            description="A day belongs to somebody."
          />
        ))}

      {whose && (
        <WhoseDay
          handle={whose}
          seat={index.byHandle.get(whose)}
          ownDay={ownDay}
          login={ownDay && viewer.unbound}
          they={they}
          stamp={person.data ?? undefined}
          setByName={setByName}
          onPriorities={tab === "priorities" ? undefined : () => showPriorities()}
          now={now}
        />
      )}

      {whose && (
        <QueryState error={state.error} refusal={state.refusal} loading={state.loading}>
          {state.loading && !mine && <Skeleton variant="text" rows={6} label="Loading the day" />}
          {mine && (
            // EVERY CHANGE ON SOMEBODY ELSE'S DAY IS HELD, with the sentence
            // that says whose it is — see the file head. `null` on your own.
            <HoldWrites reason={holds.all}>
              {/* AND `work_my_work`'S OWN COVERAGE, beside what it drew. The
                  section counts and five of the panels come off this one
                  answer, so its honesty belongs here rather than in the state
                  bar, which carries the person's own record. The two lists
                  state their own inside themselves. */}
              <Coverage answer={mine} />

              {/* THE WORK LIST, NARROWED TO ONE PERSON — not a second
                  renderer. `ItemsView` brings its own Filter and Display
                  menus, its chips, its scope switch, its count line and its
                  five shapes; what this screen supplies is the one narrowing
                  the section IS and what it opens on. */}
              {tab === "assigned" && <ItemsView host={queueHost} />}
              {tab === "priorities" && (
                // THE ONE CHANGE A LEAD MAKES ON A REPORT'S DAY is released
                // here and nowhere else — see [dayHolds].
                <HoldWrites reason={holds.reorder}>
                  <PriorityQueue
                    rows={mine.priorities}
                    stored={
                      person.data
                        ? { ids: person.data.priorities ?? [], version: person.data.version }
                        : undefined
                    }
                    whose={whose}
                    they={they}
                    theirs={ownDay ? undefined : whoseName}
                    total={mine.totals?.priorities.total}
                    now={now}
                    chrome={chrome}
                    hrefOf={workHref}
                  />
                </HoldWrites>
              )}
              {tab === "asked-of-me" && (
                <AskedOfMe
                  mine={mine}
                  whose={whose}
                  name={ownDay ? undefined : whoseName}
                  held={holds.all}
                  they={they}
                  now={now}
                />
              )}
              {tab === "asked-by-me" && <ItemsView host={askedHost} />}
              {tab === "unblocked" && (
                <Block
                  rows={mine.unblocked_recent}
                  total={mine.totals?.unblocked_recent}
                  now={now}
                  chrome={chrome}
                  hint="Work whose blockers have all finished — the one section about a change rather than a state."
                  empty={`Nothing that was waiting on something else has become workable for ${they}.`}
                />
              )}
              {tab === "collaborating" && (
                <Block
                  rows={mine.collaborating}
                  total={mine.totals?.collaborating}
                  now={now}
                  chrome={chrome}
                  hint="Brought on without owning."
                  empty={`Nobody has brought ${they} onto a task they do not own.`}
                />
              )}
              {tab === "watching" && (
                <Block
                  rows={mine.watching_recent}
                  total={mine.totals?.watching_recent}
                  now={now}
                  chrome={chrome}
                  hint="Followed, and changed recently."
                  empty={`Nothing ${ownDay ? "you follow" : "they follow"} has moved lately.`}
                />
              )}
              {tab === "checklist" &&
                (mine.checklist_items.length === 0 ? (
                  <EmptyState
                    size="compact"
                    title="Nothing is claimed"
                    description={`A checklist item on somebody else's task can name ${they} as the one who does it.`}
                  />
                ) : (
                  <>
                    <ChecklistClaims
                      rows={mine.checklist_items}
                      hrefOf={(address) => href(["work", address])}
                    />
                    <PageFoot
                      shown={mine.checklist_items.length}
                      claim={mine.totals?.checklist_items}
                      order="by-task"
                    />
                  </>
                ))}
            </HoldWrites>
          )}
        </QueryState>
      )}
    </>
  );
}

/**
 * Which changes a reader may make on the day they are reading.
 *
 * `all` holds every control on the page; `reorder` is what the priorities are
 * held with INSIDE that — `null` releases them.
 *
 * YOUR OWN DAY HOLDS NOTHING. SOMEBODY ELSE'S HOLDS EVERYTHING — the questions
 * put to them are theirs to answer, which the engine enforces, and their work
 * is changed from the work screens — except the reorder a lead may make. A lead
 * is anybody above them in the chart (`leadsInLine`), which is the tracker's
 * own reading of "somebody in their line"; where the engine did not report its
 * hierarchy that is unknown, and the sentence says so rather than telling a
 * reader who may well lead the person that they do not.
 */
export function dayHolds({
  ownDay,
  name,
  lead,
}: {
  ownDay: boolean;
  name: string;
  /** Whether the reader leads the person in the chart, or null for unknown. */
  lead: boolean | null;
}): { all: string | null; reorder: string | null } {
  if (ownDay) return { all: null, reorder: null };
  const all = `This is ${name}’s day — what is asked of them is theirs to answer, and their work is changed from the work screens.`;
  if (lead === true) return { all, reorder: null };
  return {
    all,
    reorder:
      lead === null
        ? `Only somebody in ${name}’s line reorders their queue, and this engine did not report who reports to whom.`
        : `Only ${name}, or somebody they report to, reorders their queue.`,
  };
}

/**
 * Whose day this is, above the sections, and what has been decided about it.
 *
 * # It is what the object IS, so it is not in the page bar
 *
 * A page bar holds what you can DO and an object's own identity goes in the
 * page — which is the rule the turn screen records after portalling five of
 * its marks into the bar and finding the reader's eye travelling to the far
 * corner and back for a fact named forty pixels below.
 *
 * ONE NEUTRAL BAND, WHATEVER IT SAYS. Colour carries state and never identity,
 * and whose day this is is identity — so the two readings are told apart by
 * the WORDS and by the name beside them, not by a hue.
 *
 * # And it carries the stamp, on every section
 *
 * A queue somebody else ordered is the one thing on this screen that asks for
 * an acknowledgement, and the acknowledgement is the person's own next change
 * to it — which clears the stamp for good. Drawn inside the Priorities panel
 * it was visible only to a reader who had already opened it. The
 * `Priorities →` control is withheld on the Priorities reading itself, because
 * a link to the page you are on is a lie.
 */
function WhoseDay({
  handle,
  seat,
  ownDay,
  login = false,
  they,
  stamp,
  setByName,
  onPriorities,
  now,
}: {
  handle: string;
  /** The chart's own row, absent for a handle it does not name. */
  seat?: Seat;
  ownDay: boolean;
  /**
   * The day is kept under a LOGIN rather than a seat — an unbound reader's
   * own. A login has no seat page, so it is named rather than drawn as a chip
   * linking to one that would answer "no such seat".
   */
  login?: boolean;
  they: string;
  /** This person's own record, where it has answered. */
  stamp?: WorkPersonState;
  /** Who set the order, by name, or "" where nobody else did. */
  setByName: string;
  /** Opens the Priorities reading, or absent where it is already open. */
  onPriorities?: () => void;
  now: number;
}) {
  return (
    <>
      <div className="row gap-2 wrap">
        {/* THE CHIP IS THE WAY TO THE SEAT PAGE, and the product's one way a
            person appears in a list. A handle the chart does not name still
            gets one: the name falls back to the handle, which is what the
            seat page resolves on too. */}
        {!login && (
          <SeatChip name={seat?.name ?? handle} handle={handle} kind={seat?.kind} size="md" />
        )}
        <span className="mono t-caption">{handle}</span>
        <Tag>{ownDay ? "yours" : "their day"}</Tag>
      </div>
      {setByName && (
        <Callout variant="info">
          {setByName} put this order in place
          {stamp?.priorities_set_at ? ` ${relTime(stamp.priorities_set_at, now)}` : ""}. The next
          change {they === "you" ? "you make" : "they make"} to it clears the stamp.{" "}
          {onPriorities && (
            <button type="button" className="t-link" onClick={onPriorities}>
              Priorities →
            </button>
          )}
        </Callout>
      )}
    </>
  );
}

/**
 * The whose-day picker's rows: yours, then your line, then anybody.
 *
 * # Why it is grouped at all
 *
 * Flat and alphabetical it answered "which of the company's forty seats" with
 * forty seats, in an order that has nothing to do with the question. The two
 * days a reader actually opens are their own and one of their reports', and
 * both were somewhere in the middle of the alphabet. The line comes from the
 * chart the dashboard already holds — the ENGINE's derived reports, explicit
 * and automatic, so it matches who the company thinks reports to whom rather
 * than a second reading of `manages` in this language.
 *
 * # And why each row carries a count
 *
 * A picker over people is chosen from by asking "who is loaded" — which is the
 * one fact that makes the control worth opening and the one it did not carry.
 * `work_workload` answers every handle's open work in one read.
 *
 * ORDERED BY NAME INSIDE EACH GROUP, NEVER BY THE COUNT. A row ordered by a
 * live figure re-orders itself on the next poll, so the row a reader is
 * reaching for moves between the decision to press it and the press.
 *
 * ZERO AND UNKNOWN ARE DIFFERENT. `work_workload` returns a row only for
 * somebody holding open work, so an absent handle is a real zero — but only
 * where the answer is COMPLETE and did not stop at its handle cap. Short of
 * that the row says nothing at all rather than claiming an empty desk.
 *
 * # The line is UNKNOWN without the engine's own hierarchy
 *
 * `OrgIndex.hierarchy` false means every reporting line is unknown rather than
 * absent — an older engine sends no `derived` block — so the group is not
 * drawn at all there. An empty "Your line" would say this reader leads nobody,
 * which is a claim this client cannot make.
 *
 * # And the empty row is only for a reader with no day of their own
 *
 * `setHandle("")` writes the parameter's own fallback, which the router
 * deletes, so for a reader WITH a day "Pick somebody" resolved straight back
 * to it and the control re-labelled itself — a row that silently refuses.
 * Their way back is the "Yours" row above, and an UNBOUND reader has one too:
 * their day is kept under their login (`owner`), which no seat in the chart
 * names, so it is offered as itself. Only a reader with no record at all gets
 * the empty row, and as a real row rather than the control's `placeholder`,
 * because a placeholder is a label over an unset value with nothing to select.
 */
export function whoseDayOptions(
  index: OrgIndex,
  viewer: Pick<ViewerState, "handle" | "owner">,
  load: WorkloadAnswer | null | undefined,
): SelectOption[] {
  // THE COUNTS ARE A FACT OR THEY ARE NOTHING — see the head.
  const known = !!load && load.complete !== false && !load.truncated;
  const open = new Map((load?.rows ?? []).map((r) => [r.handle, r.open]));
  const describe = (handle: string) => {
    if (!known) return handle;
    const held = open.get(handle) ?? 0;
    return `${handle} · ${held === 0 ? "nothing open" : plural(held, "open item")}`;
  };

  // A SEAT WITH NO HANDLE CANNOT BE PICKED. The engine reports one for every
  // seat it runs; a projection with no derived block reports none, and such a
  // row used to be offered with an empty value — which is the SAME value as
  // the empty row below, so picking a colleague landed on "nobody chosen".
  const named = index.seats.filter((s) => s.handle);
  const byName = (a: Seat, b: Seat) => a.name.localeCompare(b.name);
  const row = (seat: Seat, group: string): SelectOption => ({
    value: seat.handle,
    label: seat.name,
    description: describe(seat.handle),
    group,
    // WHAT THE SEARCH MATCHES. A reader types either the name they know or
    // the handle they saw in a URL, and the label carries only the first.
    text: `${seat.name} ${seat.handle}`,
  });

  const mine = viewer.handle ? index.byHandle.get(viewer.handle) : undefined;
  const line =
    index.hierarchy && mine
      ? mine.reports.filter((s) => s.handle && s.handle !== mine.handle).sort(byName)
      : [];
  const inLine = new Set(line.map((s) => s.handle));

  const out: SelectOption[] = [];
  // NO EMPTY ROW FOR A READER WITH A DAY OF THEIR OWN — see the head.
  if (mine) out.push(row(mine, "Yours"));
  else if (viewer.owner) {
    out.push({
      value: viewer.owner,
      label: viewer.owner,
      description: describe(viewer.owner),
      group: "Yours",
      text: viewer.owner,
    });
  } else out.push({ value: "", label: "Pick somebody" });
  for (const seat of line) out.push(row(seat, "Your line"));
  for (const seat of named.sort(byName)) {
    if (seat.handle === mine?.handle || inLine.has(seat.handle)) continue;
    out.push(row(seat, "Anybody"));
  }
  return out;
}

/**
 * The sections that have a figure, and no others: a section whose count has
 * not answered is ABSENT from what the header is handed, never an empty string
 * it has to know to skip.
 */
function figures(all: Record<string, string>): Record<string, string> {
  return Object.fromEntries(Object.entries(all).filter(([, figure]) => figure !== ""));
}

/**
 * How many things are behind one section, as its tab spells it.
 *
 * THE BLOCKS ARE PAGES AND THE COUNTS ARE NOT. Each block comes back capped at
 * the engine's `tracker.MyWorkRows`, so a bare length is the CEILING on anybody
 * busy; the engine counts every block in full beside its page (`totals`), by
 * the predicate that drew it, and that is the number a tab carries. The Queue
 * and Asked by me ask the tracker, whose `total_hint` is the count over the
 * list the section opens onto and whose `total_capped` is the same ceiling.
 * `pageCount` writes a `+` only where the engine's own count stopped; while a
 * read is in flight there is no number and the tab carries none rather than a
 * zero, which would read as an empty day.
 */
function countFor(
  tab: Exclude<Tab, "priorities">,
  mine: WorkMyWork,
  listed: { assigned?: WorkClaimTotal; askedBy?: WorkClaimTotal },
): string {
  // `totals` is optional on the wire: a node from before the engine counted
  // the blocks answers without it, and a tab with no count is honest where a
  // page length would be a ceiling read as a total.
  const totals = mine.totals;
  const claim: WorkClaimTotal | undefined = {
    assigned: listed.assigned,
    "asked-by-me": listed.askedBy,
    "asked-of-me": totals?.asked_of_me,
    unblocked: totals?.unblocked_recent,
    collaborating: totals?.collaborating,
    watching: totals?.watching_recent,
    checklist: totals?.checklist_items,
  }[tab];
  if (claim === undefined) return "";
  return pageCount(claim.total, claim.capped === true);
}

/**
 * What one of `work_my_work`'s bounded blocks says under itself when it holds
 * less than its count — "The 20 most recently changed of 130" — and nothing
 * where it holds all of them.
 *
 * A PAGE IS LABELLED AS A PAGE, never as the whole: the tab above says 130, and
 * twenty rows under it with nothing said would read as the tab being wrong. The
 * ORDER is named because it is what decided which twenty these are — the
 * engine's own, per block (`internal/tracker/mywork.go`).
 */
function PageFoot({
  shown,
  claim,
  order,
}: {
  shown: number;
  claim?: WorkClaimTotal;
  /** How the engine ordered the block: by last change, or by task and place. */
  order: "recent" | "by-task";
}) {
  if (!claim || claim.total <= shown) return null;
  const of = pageCount(claim.total, claim.capped === true);
  return (
    <p className="t-caption me-page-foot">
      {order === "recent"
        ? `The ${shown} most recently changed of ${of}.`
        : `The first ${shown} of ${of}, by task.`}
    </p>
  );
}

/**
 * What the Queue says when it holds nothing and nothing is narrowing it.
 *
 * IN THIS PERSON'S VOICE, and one sentence per scope, because the list's own
 * three container sentences cannot say either half: they are about what has
 * been filed in a project or opened in a company, and this section is about
 * one desk inside both.
 *
 * THE SECOND PERSON IS NOT THE DEFAULT. `they` is "you" on somebody's own day
 * and "them" on a report's, which is the same rule every other claim on this
 * screen keeps.
 */
function assignedEmpty(scope: Scope, they: string): { title: string; description: string } {
  if (scope === "closed") {
    return {
      title: `Nothing assigned to ${they} has been finished`,
      description:
        "Open is what is still to do, and All shows both. The scope switch in the bar is what moves between them.",
    };
  }
  if (scope === "all") {
    return {
      title: `Nothing is assigned to ${they}`,
      description:
        "Not open, and nothing finished either. A lead assigns work with the tracker's own tools, and a webhook or a schedule is usually what starts them.",
    };
  }
  return {
    title: `Nothing is assigned to ${they}`,
    description:
      "A lead assigns work with the tracker's own tools, and a webhook or a schedule is usually what starts them. Finished work is under Closed.",
  };
}

/**
 * What Asked by me says when it holds nothing and nothing is narrowing it —
 * in this person's voice, per scope, for the reason [assignedEmpty] gives.
 */
function askedEmpty(scope: Scope, they: string): { title: string; description: string } {
  const who = they === "you" ? "You are" : "They are";
  const subject = they === "you" ? "you" : "they";
  if (scope === "all") {
    return {
      title: `${who} not waiting on an answer`,
      description: `A question ${subject} put to somebody on a task stays here until it is answered or resolved.`,
    };
  }
  return {
    title: `${who} not waiting on an answer here`,
    description: `Nothing ${subject} asked is still open on work in this scope. All shows every task a question is still waiting on.`,
  };
}

/**
 * The questions waiting on this person — each a decision they answer in
 * place, on their own day.
 *
 * THE ONE SECTION WHERE SOMEBODY ELSE IS BLOCKED ON THIS PERSON rather than the
 * other way round, which is why it is the shared [DecisionRow] Home draws: the
 * question, who asked and in what role, what they recommend, and the answer —
 * an option, or a reply — sent from the row. On somebody else's day the row is
 * written about them and its controls are held (see the file head).
 */
function AskedOfMe({
  mine,
  whose,
  name,
  held,
  they,
  now,
}: {
  mine: WorkMyWork;
  whose: string;
  /** Set on somebody else's day, for the third person. */
  name?: string;
  /** Why the answers are held here, or null where they are the reader's. */
  held: string | null;
  they: string;
  now: number;
}) {
  const rows = mine.asked_of_me;
  if (rows.length === 0) {
    return (
      <EmptyState
        size="compact"
        title={`Nothing is waiting on ${they}`}
        description="A question is a comment that asks something of somebody rather than informing them, and the engine records which."
      />
    );
  }
  const claim = mine.totals?.asked_of_me;
  return (
    <div className="col gap-2">
      {/* WHY THE ANSWERS DO NOT PRESS, said once above them — each button
          carries it too, but only on hover or to a screen reader, and a row
          of dimmed options with nothing said reads as a broken screen. */}
      {held && <p className="t-caption work-write-note">{`Answering is off. ${held}`}</p>}
      <div className="me-asks">
        <AskList rows={rows} decider={{ handle: whose, name }} now={now} />
      </div>
      {claim && claim.total > rows.length && (
        <p className="t-caption me-page-foot">
          {`The newest ${rows.length} of ${pageCount(claim.total, claim.capped === true)}.`}{" "}
          {/* THE REST ARE THE INBOX'S DECISIONS, which page past this block —
              on your own day. Somebody else's are theirs to read. */}
          {!name && (
            <a className="t-link" href={decisionsHref()}>
              All of them in the Inbox →
            </a>
          )}
        </p>
      )}
    </div>
  );
}

/** One of the bounded blocks, drawn as the same list as everything else. */
function Block({
  rows,
  total,
  now,
  chrome,
  hint,
  empty,
}: {
  rows: WorkSummary[];
  total?: WorkClaimTotal;
  now: number;
  chrome: RowChrome;
  hint: string;
  empty: string;
}) {
  if (rows.length === 0) {
    // A SECTION IS DRAWN EVEN WHEN IT IS EMPTY, which is the whole difference
    // from the stacked cards this replaces: they vanished, taking their own
    // name with them, so a reader could not tell "nothing here" from "this
    // product does not have that".
    return <EmptyState size="compact" title="Nothing here" description={empty} />;
  }
  return (
    <>
      <p className="t-caption">{hint}</p>
      <div className="work-list">
        <RowList rows={rows} now={now} chrome={chrome} hrefOf={(row) => href(itemPath(row))} />
      </div>
      <PageFoot shown={rows.length} claim={total} order="recent" />
    </>
  );
}
