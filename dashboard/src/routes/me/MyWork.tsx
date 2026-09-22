/**
 * One person's day — the seven claims on their attention, as tabs.
 *
 * # Seven claims, and each is a different one
 *
 * A person who saw only their assignments would miss six other things asking
 * for their time: what a lead put at the top of their list, the questions
 * waiting on an answer, the sub-items they claimed on somebody else's task,
 * the work they were brought onto without owning, what moved on what they
 * follow, and what became workable while they were not looking.
 *
 * # Why they are tabs now, and what keeps the old promise
 *
 * They were seven stacked cards under four stat tiles that restated the counts
 * of the cards below them, and each card was ABSENT when empty — so the page's
 * shape changed with the day and the block that mattered was wherever it
 * happened to fall. A person with two hundred assignments never saw their
 * asks.
 *
 * The promise the stacking was making — that no block can crowd out another —
 * is kept by the STRIP rather than by the page: every tab carries its count,
 * always, so an unanswered question is visible as a number on a tab nobody has
 * opened. A tab with nothing in it is drawn and says so, which stacking could
 * not do without seven "nothing here" panels burying the one that had
 * something.
 *
 * # A sparse day is still somebody's day
 *
 * THE STRIP DRAWS SEVEN TABS AND SEVEN COUNTS WHATEVER THE DAY HOLDS, zeros
 * included, and that does not change on a company's first week. A tab that
 * vanished when it was empty is what this screen was rebuilt to stop, and the
 * Inbox settles the same question the same way one workspace up: its pulse
 * strip draws every figure whatever the queue holds, so an empty band below
 * MEANS something rather than looking like a product that does not have that.
 * A branch keyed on the seven counts summing to zero would also be wrong about
 * its own subject — the sum is ONE PERSON's day, so a quiet seat inside a busy
 * company would be told the company was empty.
 *
 * What a sparse day gets instead is a page that says something TRUE before its
 * first count: the banner above the strip names whose day this is, their seat
 * and the way to it, and carries the one thing on this screen that asks for an
 * acknowledgement. Each empty tab names what would fill it, in that person's
 * voice. And a count is WITHHELD rather than drawn as a zero while its read is
 * in flight, because "nothing is on you" is a claim and it is false until an
 * answer arrives.
 *
 * # The Inbox is not one of them
 *
 * What REACHED somebody is a different question from what is ON them, and it
 * is the landing screen of this product (`#/inbox`). The card that drew it
 * here was the inbox in a narrower column with a smaller bound.
 *
 * # Assigned is the work list, narrowed to one person
 *
 * It is `routes/work/ItemsView.tsx` — the same component `#/work` and a
 * project's Items lens are — held here by an [ItemsHost] that fixes the one
 * thing this tab IS (the assignee) and what it opens on, and leaves the shape,
 * the grouping, the order, the columns and every other filter to the reader,
 * in the URL, exactly as they are on the other two screens. Written as a
 * second renderer it had no Filter menu, no Display menu, no scope switch, no
 * chips, no count line and no way past its two hundredth row, and each of
 * those was a rule the work list already kept.
 *
 * IT OPENS BANDED BY WHEN. "What have I missed, what is today, what is this
 * week" is the question somebody opens their own work to ask, where a status
 * grouping answers one nobody asked — every task they hold is in progress or
 * about to be. The bands are the ENGINE's `due:bucket` axis, cut against the
 * COMPANY's day start like the row's own overdue flag and every `due=` filter,
 * so the heading a task is under and the flag beside it can never disagree.
 * They were computed here, from the browser's own midnight and the browser's
 * own week, and only the bands that held rows were drawn — so a reader west of
 * the company saw a task banded Earlier that the same answer called due today,
 * and nothing said which of the six bands a quiet day was missing.
 *
 * # Whose day it is decides the pronoun, everywhere
 *
 * Including the wake reasons. `reasonPhrase` is the second person — "assigned
 * to you" — and on an operator reading a report's day that is a sentence about
 * the reader, who is not on the list.
 *
 * # Read-only, like every other work screen
 *
 * Nothing here writes. A priority list is reordered through the engine's own
 * write path, attributed to whoever did it; a button here would write as "the
 * dashboard", which is not a person and cannot be asked why.
 */

import { useMemo, type ReactNode } from "react";
import { renderMarkdown } from "~/lib/markdown.ts";
import { href, useParam } from "~/app/router.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { Coverage, RowList, type RowChrome } from "~/components/work.tsx";
import {
  Callout,
  EmptyState,
  InlineCode,
  Select,
  Skeleton,
  Tabs,
  Tag,
  type SelectOption,
} from "@crewlethq/ui";
import { FlagGlyph, KeyGlyph, PersonGlyph } from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, type OrgIndex, type Seat } from "~/lib/seats.ts";
import { plural, relTime } from "~/lib/format.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useNow } from "~/lib/clock.ts";
import { pageCount, type Scope } from "~/lib/work.ts";
import { ItemsView, type ItemsHost } from "~/routes/work/ItemsView.tsx";
import type {
  WorkAskRow,
  WorkChecklistRow,
  WorkMyWork,
  WorkPersonState,
  WorkloadAnswer,
  WorkSummary,
} from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

/**
 * The engine's bound on each block of `work_my_work`.
 *
 * `tracker.MyWorkRows`. It is not a page size this screen chose and it is not
 * one it can change — which is why a count drawn from one of these blocks says
 * `20+` at the bound rather than `20`: the figure is the ceiling, not the
 * company's. The Assigned tab is the one that escapes it, by asking the
 * tracker's own question instead.
 */
const BLOCK_ROWS = 20;

/**
 * The scope the Assigned tab's own COUNT is taken over.
 *
 * The tracker's default — unfinished work — because a day is about what is
 * still to do, and it is spelled here rather than read off the list's scope
 * segment on purpose: the strip's number says how much is on this person, and
 * a reader who presses Closed to check something finished has not emptied
 * their day. `SCOPE_GROUPS.open` is the same two groups on the list's side.
 */
const ASSIGNED_SCOPE = "not_started,active";

/**
 * What the Assigned tab OPENS on, as the list's own parameters.
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

/** The tabs, in the order the strip draws them; the first is the default. */
const TABS = [
  "assigned",
  "priorities",
  "asks",
  "unblocked",
  "collaborating",
  "watching",
  "checklist",
] as const;
type Tab = (typeof TABS)[number];

export function MyWork() {
  const org = useOrg();
  const now = useNow();
  const [handle, setHandle] = useParam("handle", "");
  const [tab, setTab] = useTab("tab", TABS);
  // EVERY SEAT AND EVERY PERSON the chart names, so the screen can be reached
  // with nobody chosen and still offer somebody.
  const index = useMemo(() => indexOrg(org), [org]);
  const chrome: RowChrome = {
    seatName: (h) => index.byHandle.get(h)?.name ?? h,
  };
  // WHOSE DAY THIS IS, resolved rather than guessed.
  //
  // This fell back to the ALPHABETICALLY FIRST SEAT, so a screen titled "My
  // work" rendered a stranger's day to everybody. The `viewer` question walks
  // the binding that has always existed: a presented token resolves to an
  // operator id, and a seat names that id in `contact.crewlet_operator_id`.
  //
  // An explicit choice still wins — an operator reading a report's day is a
  // real thing to do, and the header says whose day it is either way.
  const viewer = useViewer();
  const whose = handle || viewer.handle;
  const ownDay = whose !== "" && whose === viewer.handle;
  const they = ownDay ? "you" : "them";

  // WHO IS CARRYING HOW MUCH, for the picker. One read over every handle at
  // once — the alternative is a `work_my_work` per colleague, which is a round
  // trip per option — and it is the whole company's rather than this person's,
  // so it does not move when the day being read does.
  //
  // POLLED ONLY WHERE THE PICKER IS DRAWN, which is the same condition: an
  // anonymous reader is offered no picker, and a read over every handle in the
  // company once a minute for a control nobody can see is a minute's work per
  // minute for nothing. The FIRST read still goes out, because until the
  // viewer answers this reader is indistinguishable from one who gets a
  // picker — and delaying the counts for everybody to spare that one read is
  // the wrong trade. 60s is the interval both other readers of this question
  // already take (`routes/inbox/Inbox.tsx`, `routes/company/People.tsx`): a
  // load is read, not watched.
  const workload = useQuery("work_workload", undefined, {
    enabled: !viewer.anonymous,
    pollMs: 60_000,
  });

  // NOT UNTIL SOMEBODY IS CHOSEN — `whose` is empty until the chart has
  // loaded, and the engine refuses this question without a handle.
  const state = useQuery("work_my_work", whose ? { handle: whose } : undefined, {
    enabled: whose !== "",
    pollMs: 30_000,
  });
  const mine = state.data;

  // AND THE ASSIGNMENTS AS THE TRACKER'S OWN QUESTION, asked whichever tab is
  // open: its total is what the Assigned tab's count says, and a count that
  // appeared only once its tab was opened would be a strip that changes as you
  // walk it.
  //
  // THE SAME QUESTION THE TAB'S OWN LIST ASKS, and deliberately not the same
  // READ: this one is the strip's count and carries no arrangement at all, so
  // it stays put while the reader groups, sorts and narrows the list under it.
  // A count that moved with the filters would answer "how much is on this
  // person" with "how much is on screen", which is what the other six tabs'
  // own bounded blocks would then disagree with.
  const assigned = useQuery(
    "work_items",
    whose ? { container: "workspace", assignee: whose, status_group: ASSIGNED_SCOPE } : undefined,
    { enabled: whose !== "", pollMs: 30_000 },
  );

  // AND THIS PERSON'S OWN RECORD, read ONCE for the page rather than inside
  // the tab that draws part of it.
  //
  // IT USED TO LIVE IN THE PRIORITIES PANEL, which is the one place it cannot:
  // the stamp it carries says somebody ELSE ordered this queue, and that is
  // the one thing on this screen asking to be acknowledged — visible only to a
  // reader who had already acknowledged it by opening the tab, while their own
  // next write to the queue cleared it for good. Lifted here it feeds the
  // banner above the strip, the mark on the tab nobody opened, and the
  // coverage the frame publishes.
  const person = useQuery("work_person", whose ? { handle: whose } : undefined, {
    enabled: whose !== "",
    pollMs: 60_000,
  });

  // AND THE FRAME GETS THE COVERAGE OF THE READ THAT IDENTIFIES THIS OBJECT.
  // Only a SCREEN publishes — the frame holds one slot with one setter, so a
  // body that published from inside a screen fights that screen's answer
  // (`routes/work/History.tsx` records what that cost) — and `#/me`'s object
  // is a PERSON, so the person's own record is the answer whose honesty
  // belongs in the state bar. The other two reads on this screen state their
  // own beside the rows they drew: `work_my_work`'s under the strip it fills,
  // and the list's inside the list.
  usePageCoverage(person.data);

  // WHAT THE ASSIGNED TAB IS, handed to the list that draws it — see
  // [ItemsHost]. Memoised on the two values it reads, because the list asks
  // the engine again whenever the lock moves and a fresh object every render
  // would be a fresh question every render.
  const host = useMemo<ItemsHost>(
    () => ({
      assignee: whose,
      opens: ASSIGNED_OPENS,
      empty: (scope: Scope) => assignedEmpty(scope, they),
    }),
    [whose, they],
  );

  return (
    <>
      {/* THE BAR HOLDS WHAT YOU CAN DO. Whose day this is is what the object
          IS, so it is the band above the strip rather than a pill up here —
          see [WhoseDay]. */}
      <PageActions>
        {/* IN THE PAGE BAR RATHER THAN IN A SIDEBAR. Whose day this is a
            FILTER on the screen you are on, and the product's grammar puts a
            filter in the page's own bar and keeps the sidebar for destinations
            with paths of their own (`app/nav.ts`). The bar is where a screen's
            own controls go, and choosing whose day to read is something you
            DO — which is the other half of the same rule that moved the
            whose-day pill out of it.

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
            active={whose !== ""}
            options={whoseDayOptions(index, viewer.handle, workload.data)}
          />
        )}
        <a className="t-link" href={href(["inbox"])}>
          Inbox →
        </a>
        <a className="t-link" href={href(["work"])}>
          All work →
        </a>
      </PageActions>
      <PageNote>
        Everything one person is expected to look at — what they hold, the order somebody put it in,
        the questions waiting on them, and what became workable while they were away.
      </PageNote>

      {/* THREE STATES, and they are not one empty state. A reader with no
          token, a reader whose token names no seat, and a reader who simply has
          not chosen somebody need three different sentences — and only the last
          of them is a choice anybody can make on this screen. */}
      {!whose &&
        (viewer.anonymous ? (
          <EmptyState
            icon={<KeyGlyph size={32} />}
            title="No credential is presented"
            description="A day belongs to a person, and this browser has not said who it is. Set an API token: a day is somebody's own record, so reading one — yours or anybody's — is a question the engine answers for a caller it can name."
          />
        ) : viewer.unbound ? (
          <EmptyState
            icon={<PersonGlyph size={32} />}
            title="This token is not bound to a person"
            description={`Give a human seat contact.crewlet_operator_id: ${viewer.operatorID} in the company configuration and this becomes their day. Until then, pick somebody above.`}
          />
        ) : (
          <EmptyState
            icon={<PersonGlyph size={32} />}
            title="Nobody chosen"
            description="A day belongs to somebody."
          />
        ))}

      {/* WHOSE DAY THIS IS, above the strip, before any count.
          Drawn from the chart and from this person's own record — see
          [WhoseDay] for why an identity band belongs here rather than in the
          page bar. */}
      {whose && (
        <WhoseDay
          handle={whose}
          seat={index.byHandle.get(whose)}
          ownDay={ownDay}
          they={they}
          stamp={person.data ?? undefined}
          onPriorities={tab === "priorities" ? undefined : () => setTab("priorities")}
          now={now}
          chrome={chrome}
        />
      )}

      {whose && (
        <QueryState error={state.error} loading={state.loading}>
          {state.loading && !mine && <Skeleton variant="text" rows={6} label="Loading the day" />}
          {mine && (
            <>
              <Tabs
                ariaLabel="Which claim"
                value={tab}
                onValueChange={(value) => setTab(value as Tab)}
                items={TABS.map((key) => ({
                  value: key,
                  // EVERY COUNT, ALWAYS, which is what replaces the stacking:
                  // a tab nobody has opened still says how much is on it. The
                  // count rides in the LABEL rather than in the strip's own
                  // `count` slot because that slot is `number | null` and six
                  // of these seven are a CEILING — `pageCount` writes `20+`,
                  // and a bare 20 there would report the engine's page size as
                  // a fact about somebody's day.
                  label: countedTab(key, mine, assigned.data?.total_hint),
                  // AND THE ONE MARK ON THE STRIP is the queue somebody else
                  // ordered — see [WhoseDay]. It is a FLAG rather than a
                  // warning glyph: a lead putting an order on your list is not
                  // a fault, it is a decision somebody made that you have not
                  // seen yet.
                  icon:
                    key === "priorities" && person.data?.priorities_set_by ? (
                      <FlagGlyph size="sm" />
                    ) : undefined,
                }))}
              />

              {/* AND `work_my_work`'S OWN COVERAGE, beside what it drew. The
                  strip above and six of the seven panels below come off this
                  one answer, so its honesty belongs here rather than in the
                  state bar, which carries the person's own record. The list on
                  the Assigned tab states its own inside itself. */}
              <Coverage answer={mine} />

              {/* THE WORK LIST, NARROWED TO ONE PERSON — not a second
                  renderer. `ItemsView` brings its own Filter and Display
                  menus, its chips, its scope switch, its count line and its
                  five shapes; what this screen supplies is the one narrowing
                  the tab IS and what it opens on. */}
              {tab === "assigned" && <ItemsView host={host} />}
              {tab === "priorities" && (
                <Priorities mine={mine} now={now} chrome={chrome} they={they} />
              )}
              {tab === "asks" && (
                <Asks
                  rows={mine.asked_of_me}
                  now={now}
                  chrome={chrome}
                  ownDay={ownDay}
                  whenEmpty={
                    <EmptyState
                      size="compact"
                      title={`Nothing is waiting on ${they}`}
                      description="A question is a comment that asks something of somebody rather than informing them, and the engine records which."
                    />
                  }
                />
              )}
              {tab === "unblocked" && (
                <Block
                  rows={mine.unblocked_recent}
                  now={now}
                  chrome={chrome}
                  hint="Work whose blockers have all finished — the one tab about a change rather than a state."
                  empty={`Nothing that was waiting on something else has become workable for ${they}.`}
                />
              )}
              {tab === "collaborating" && (
                <Block
                  rows={mine.collaborating}
                  now={now}
                  chrome={chrome}
                  hint="Brought on without owning."
                  empty={`Nobody has brought ${they} onto a task they do not own.`}
                />
              )}
              {tab === "watching" && (
                <Block
                  rows={mine.watching_recent}
                  now={now}
                  chrome={chrome}
                  hint="Followed, and changed recently."
                  empty={`Nothing ${ownDay ? "you follow" : "they follow"} has moved lately.`}
                />
              )}
              {tab === "checklist" && (
                <Checklist
                  rows={mine.checklist_items}
                  whenEmpty={
                    <EmptyState
                      size="compact"
                      title="Nothing is claimed"
                      description={`A checklist item on somebody else's task can name ${they} as the one who does it.`}
                    />
                  }
                />
              )}
            </>
          )}
        </QueryState>
      )}
    </>
  );
}

/**
 * Whose day this is, above the strip, and what has been decided about it.
 *
 * # It is what the object IS, so it is not in the page bar
 *
 * A page bar holds what you can DO and an object's own identity goes in the
 * page — which is the rule the turn screen records after portalling five of
 * its marks into the bar and finding the reader's eye travelling to the far
 * corner and back for a fact named forty pixels below. The whose-day pill was
 * in the bar; it is here, beside the seat it names.
 *
 * ONE NEUTRAL BAND, WHATEVER IT SAYS. Colour carries state and never identity,
 * and whose day this is is identity — so the two readings are told apart by
 * the WORDS and by the name beside them, not by a hue. The reader who must not
 * miss this is an operator on somebody ELSE's day.
 *
 * # And it carries the stamp, on every tab
 *
 * A queue somebody else ordered is the one thing on this screen that asks for
 * an acknowledgement, and the acknowledgement is the person's own next change
 * to it — which clears the stamp for good. Drawn inside the Priorities panel
 * it was visible only to a reader who had already opened that tab, so the one
 * fact that needed announcing was the one fact a busy person never saw. The
 * `Priorities →` control is withheld on the Priorities tab itself, because a
 * link to the page you are on is a lie.
 *
 * # A sparse day still has one
 *
 * This band is TRUE on a company's first morning, when every count under it is
 * zero: somebody is who they are, their seat is where it is, and the way to it
 * is the same link. See this file's head for the rest of that decision.
 */
function WhoseDay({
  handle,
  seat,
  ownDay,
  they,
  stamp,
  onPriorities,
  now,
  chrome,
}: {
  handle: string;
  /** The chart's own row, absent for a handle it does not name. */
  seat?: Seat;
  ownDay: boolean;
  they: string;
  /** This person's own record, where it has answered. */
  stamp?: WorkPersonState;
  /** Opens the Priorities tab, or absent where that tab is already open. */
  onPriorities?: () => void;
  now: number;
  chrome: RowChrome;
}) {
  const setBy = stamp?.priorities_set_by;
  return (
    <>
      <div className="row gap-2 wrap">
        {/* THE CHIP IS THE WAY TO THE SEAT PAGE, and the product's one way a
            person appears in a list — a second anchor to the same address
            beside it would be chrome duplicating chrome. A handle the chart
            does not name still gets one: the name falls back to the handle,
            which is what the seat page resolves on too. */}
        <SeatChip
          name={seat?.name ?? handle}
          handle={handle}
          human={seat?.kind === "human"}
          size="md"
        />
        <span className="mono t-caption">{handle}</span>
        <Tag>{ownDay ? "yours" : "their day"}</Tag>
      </div>
      {setBy && (
        <Callout variant="info">
          {chrome.seatName?.(setBy) ?? setBy} put this order in place
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
 * deletes, so for a BOUND reader "Pick somebody" resolved straight back to
 * their own seat and the control re-labelled itself with their name — a row
 * that silently refuses. Their way back is the "Yours" row above. For a reader
 * whose credential names no seat it is the state they are in, and a real row
 * rather than the control's `placeholder`, because a placeholder is a label
 * over an unset value with nothing to select.
 */
export function whoseDayOptions(
  index: OrgIndex,
  viewerHandle: string,
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

  const mine = viewerHandle ? index.byHandle.get(viewerHandle) : undefined;
  const line =
    index.hierarchy && mine
      ? mine.reports.filter((s) => s.handle && s.handle !== mine.handle).sort(byName)
      : [];
  const inLine = new Set(line.map((s) => s.handle));

  const out: SelectOption[] = [];
  // NO EMPTY ROW FOR A READER WITH A DAY OF THEIR OWN — see the head.
  if (!mine) out.push({ value: "", label: "Pick somebody" });
  if (mine) out.push(row(mine, "Yours"));
  for (const seat of line) out.push(row(seat, "Your line"));
  for (const seat of named.sort(byName)) {
    if (seat.handle === mine?.handle || inLine.has(seat.handle)) continue;
    out.push(row(seat, "Anybody"));
  }
  return out;
}

/** What each tab is called. */
const TAB_LABEL: Record<Tab, string> = {
  assigned: "Assigned",
  priorities: "Priorities",
  asks: "Asks",
  unblocked: "Unblocked",
  collaborating: "Collaborating",
  watching: "Watching",
  checklist: "Checklist",
};

/**
 * What a tab's count says, and what it is a count OF.
 *
 * THE BLOCKS ARE BOUNDED AND THE ASSIGNMENTS ARE NOT. Six of these come back
 * capped at [BLOCK_ROWS], so a bare length is the CEILING on anybody busy —
 * `pageCount` is this product's own idiom for that and writes `20+`. Assigned
 * asks the tracker, whose `total_hint` is a count over the matching set, so it
 * needs no hedge; while that read is in flight there is no number to draw and
 * the tab carries none rather than a zero, which would read as an empty day.
 */
function countedTab(tab: Tab, mine: WorkMyWork, assignedTotal: number | undefined): string {
  const count = countFor(tab, mine, assignedTotal);
  // NO TRAILING SPACE ON A TAB WITH NO COUNT. The accessible name is the
  // label, and "Assigned " is a name with a word nobody wrote at the end of
  // it — which is what a reader of the strip hears while the read is in
  // flight.
  return count ? `${TAB_LABEL[tab]} ${count}` : TAB_LABEL[tab];
}

/** How many things are behind one tab, as the strip spells it. */
function countFor(tab: Tab, mine: WorkMyWork, assignedTotal: number | undefined): string {
  if (tab === "assigned") return assignedTotal === undefined ? "" : String(assignedTotal);
  const rows =
    tab === "priorities"
      ? mine.priorities
      : tab === "asks"
        ? mine.asked_of_me
        : tab === "unblocked"
          ? mine.unblocked_recent
          : tab === "collaborating"
            ? mine.collaborating
            : tab === "watching"
              ? mine.watching_recent
              : mine.checklist_items;
  return pageCount(rows.length, rows.length >= BLOCK_ROWS);
}

/**
 * What the Assigned tab says when it holds nothing and nothing is narrowing it.
 *
 * IN THIS PERSON'S VOICE, and one sentence per scope, because the list's own
 * three container sentences cannot say either half: they are about what has
 * been filed in a project or opened in a company, and this tab is about one
 * desk inside both. "Nothing has been filed yet" over a company with four
 * hundred tasks and one idle seat is false about the only subject the reader
 * came for.
 *
 * THE SECOND PERSON IS NOT THE DEFAULT. `they` is "you" on somebody's own day
 * and "them" on a report's, which is the same rule every other claim on this
 * screen keeps — an operator reading a colleague's day must not be told their
 * own queue is empty.
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
 * The order somebody means to work in — theirs, or a lead's for them.
 *
 * IN THE STORED ORDER, never re-sorted: the order is the content — it is what
 * somebody decided — and sorting it discards the decision. Which is why the
 * rows are NUMBERED: a decision nothing on screen shows is a decision the
 * reader cannot act on, and there is no drag here for the same reason nothing
 * else on this screen writes — a rank is a value on the task, and dragging one
 * would be the dashboard deciding a team's order. `set_priorities` is the
 * gesture, and it is somebody's own.
 */
function Priorities({
  mine,
  now,
  chrome,
  they,
}: {
  mine: WorkMyWork;
  now: number;
  chrome: RowChrome;
  they: string;
}) {
  // THE STAMP IS NOT DRAWN HERE. It is the banner above the strip, on every
  // tab, because it is the one thing on this screen that asks to be
  // acknowledged and a reader who has already opened this tab has answered it
  // — and a second copy of it under the banner would be the same sentence
  // twice on the one tab where both would be on screen at once.
  return (
    <>
      {mine.priorities.length === 0 ? (
        <EmptyState
          size="compact"
          title={`Nothing is at the top of ${they === "you" ? "your" : "their"} list`}
          description="A queue is written with set_priorities — by the person whose it is, or by a lead in their line."
        />
      ) : (
        <div className="work-list">
          {/* NUMBERED, because the order IS the content here. Every other tab
              on this screen is a SET somebody has a claim on; this one is a
              SEQUENCE somebody decided, and drawn as an ordinary run of rows
              it reads exactly like the Watching list beside it — the one thing
              the tab is about, invisible. */}
          <RowList
            rows={mine.priorities}
            now={now}
            chrome={chrome}
            hrefOf={(row) => href(["work", row.key])}
            ordinals
          />
        </div>
      )}
    </>
  );
}

/** One of the six bounded blocks, drawn as the same list as everything else. */
function Block({
  rows,
  now,
  chrome,
  hint,
  empty,
}: {
  rows: WorkSummary[];
  now: number;
  chrome: RowChrome;
  hint: string;
  empty: string;
}) {
  if (rows.length === 0) {
    // A TAB IS DRAWN EVEN WHEN IT IS EMPTY, which is the whole difference from
    // the stacked cards this replaces: they vanished, taking their own name
    // with them, so a reader could not tell "nothing here" from "this product
    // does not have that".
    return <EmptyState size="compact" title="Nothing here" description={empty} />;
  }
  return (
    <>
      <p className="t-caption">{hint}</p>
      <div className="work-list">
        <RowList rows={rows} now={now} chrome={chrome} hrefOf={(row) => href(["work", row.key])} />
      </div>
    </>
  );
}

/**
 * The questions waiting on this person.
 *
 * THE ONE TAB WHERE SOMEBODY ELSE IS BLOCKED ON THIS PERSON rather than the
 * other way round, which is why each row carries the literal call that answers
 * it: a model handed a comment id still has to compose the call, and every one
 * it composes differently is a round spent being refused.
 *
 * ITS COUNT IS NOT TONED, and the sentence above used to say it was. A `Count`
 * is never tinted — the design system states the rule and gives the reason,
 * that three of something is not a warning — and the word "Asks" already
 * carries the meaning a tint would repeat.
 */
export function Asks({
  rows,
  now,
  chrome,
  // WHOSE QUESTIONS THESE ARE, and the DEFAULT IS SOMEBODY ELSE'S. This block
  // is rendered on a report's day here and on every seat page, which is written
  // in the third person throughout — so a caller that says nothing is a caller
  // that has not claimed the rows are the reader's.
  ownDay = false,
  // AND WHAT AN EMPTY ONE DRAWS IS THE CALLER'S DECISION, never a default that
  // suits one of them: a TAB must say something, because a tab that rendered
  // nothing would leave its own name on the strip over a blank panel; a block
  // STACKED among other cards must say nothing, because three "nothing here"
  // panels on a quiet seat bury the one card that has something.
  whenEmpty,
}: {
  rows: WorkAskRow[];
  now: number;
  chrome?: RowChrome;
  ownDay?: boolean;
  whenEmpty?: ReactNode;
}) {
  if (rows.length === 0) return <>{whenEmpty ?? null}</>;
  return (
    <div className="col gap-3">
      {rows.map((ask) => (
        // THE CAUTION RAIL, the same mark a question wears in a thread.
        <div key={ask.comment} className="comment work-ask">
          <div className="row gap-2 wrap">
            <a className="mono t-link" href={href(["work", ask.key])}>
              {ask.key}
            </a>
            <span className="truncate">{ask.title}</span>
            <span className="spacer" />
            <SeatChip
              name={chrome?.seatName?.(ask.asked_by) ?? ask.asked_by}
              handle={ask.asked_by}
            />
            <span className="muted">{relTime(ask.asked_at, now)}</span>
          </div>
          <div className="prose md">{renderMarkdown(ask.body)}</div>
          {/* THE CALL THAT ANSWERS IT, pre-filled. The dashboard writes
              nothing, so what it offers is the gesture somebody's own
              assistant makes on their behalf. */}
          {ask.answer_with && <InlineCode>{ask.answer_with}</InlineCode>}
        </div>
      ))}
    </div>
  );
}

/**
 * Sub-items claimed on somebody else's task.
 *
 * Its own tab because no assignee filter over tasks reaches one: a person
 * holding six checklist items and no assignment reads their queue as empty.
 */
export function Checklist({
  rows,
  whenEmpty,
}: {
  rows: WorkChecklistRow[];
  /** As [Asks]'s: what an empty one draws is the caller's decision. */
  whenEmpty?: ReactNode;
}) {
  if (rows.length === 0) return <>{whenEmpty ?? null}</>;
  return (
    <div className="col">
      {rows.map((item) => (
        <div key={`${item.task}:${item.item}`} className={`work-check${item.done ? " done" : ""}`}>
          <a className="mono t-link" href={href(["work", item.task_key])}>
            {item.task_key}
          </a>
          <span className="work-check-name">{item.name}</span>
          <span className="spacer" />
          <span className="muted truncate">{item.task_title}</span>
        </div>
      ))}
    </div>
  );
}
