/**
 * The Inbox: the place a person acts on what reached them, built around the
 * decisions waiting on them.
 *
 * # A list, and each item read in full beside it
 *
 * The left column is ONE list in a fixed order (`InboxColumn.tsx`): Needs
 * you first (the asks put to this person and the coding runs parked on a
 * question to them, from the engine's `decisions`; the seats stopped on a
 * spent budget; and the engine conditions a person decides,
 * `lib/attention.ts` with `where: seat`), then Updates, what reached them and
 * is not read yet. Decisions come first because they are what only this
 * person can unblock; they are never interleaved with the updates by time,
 * which would eventually rank "a task you watch moved" above the CEO asking
 * whether to hold a release. What was snoozed and what was read already are
 * two quiet links at the end, read when they are opened.
 *
 * The right column is the open row read in full, top to bottom, with the
 * answer to it (`InboxPane.tsx`). Stepping down the list REPLACES history, so
 * four rows read through one open pane are one place the reader has been (the
 * rule the frame's own peek follows), and `j`/`k` step through it from the
 * keyboard. A task named on a row or in a byline opens beside the inbox, in
 * the frame's side panel.
 *
 * # Where the company runs no Worklet
 *
 * Updates, Snoozed and Earlier are Worklet's notices, and Mark all read
 * marks them, so where the company runs another tracker or none
 * (`lib/services.ts`) none of them is asked for or drawn: one line says where
 * asks and mentions go instead ([elsewhereLine]). Needs you stays, because
 * what fills it without Worklet still arrives: the seats stopped on a
 * spent budget and the engine's conditions (the agents and budget pushes) and
 * the coding runs parked on a question (`decisions`, which answers them
 * whatever runs the tracker). So does Answered, for the runs' questions the
 * person answered (`answered_decisions`, which lists those on every company).
 *
 * # What is answered moves to Answered, and the next item opens
 *
 * An answered question or run leaves Needs you at once for the Answered
 * group, at its head, marked Answered with the answer under the question, and
 * the next thing to deal with opens in the pane in the same moment; one polite
 * announcement says both ("Answered. Next: …"). Held in place until the reader
 * left, a done row stood among the open ones, and the next item waited on a
 * timer. An answer the engine has accepted and not yet settled is the one
 * kept where it stands ([PaneProps.onSettled]'s `hold`), shown as sent. A
 * read or snoozed update shows its new state, and a moment later
 * (`ADVANCE_MS`) the next item opens, unless the reader has already moved
 * on.
 *
 * # What was answered before stays to be looked back at
 *
 * The asks put to the reader and closed in the last week
 * (`answered_decisions`, by them or by somebody else), and the coding runs'
 * questions they answered in it, are a closed, counted Answered group after
 * Updates. Opened, its rows step with `j` and `k` like any other; a question
 * answered on this visit is in Needs you instead, never twice, and one the
 * engine already lists as answered is not in Needs you as well. An answered
 * question opens in the same pane whichever asked it, with who answered it;
 * an ask's adds what the asker has done with it since, and a run's the
 * options it offered with the chosen one marked.
 *
 * # Every mark is a gesture, made as you
 *
 * Mark as read, Snooze and "Mark all read" each name exactly what they mark
 * (`mark_inbox{read}`, `{snooze}`, `{read_through}` at the newest update
 * loaded) as the person signed in (ADR-0024), on the record they are kept
 * under (`ViewerState.owner`, `iam.RecordOwner`). The engine changes that and
 * nothing else, and the list is re-read at the position the write answered
 * with, so a row moves only once the engine says it has.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Callout, InlineCode, announce, useMediaMatch, useScreenBelow } from "@crewlethq/ui";
import { CheckGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator, useParam } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { useKeymap } from "~/app/keymap.ts";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import { decisionSubjects, seatConditionsOf } from "~/lib/decisions.ts";
import { QueryState } from "~/components/common.tsx";
import { UnboundRemedy } from "~/components/people.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useBackends, useServicesRun, type OrgServices } from "~/lib/services.ts";
import { useNow } from "~/lib/clock.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { conditionsToDecide, watchedIn } from "~/lib/attention.ts";
import { useAttention } from "~/lib/useAttention.ts";
import { INBOX_PAGE } from "~/lib/useInboxCounts.ts";
import { useAssistantContext } from "~/app/assistant/useAssistantContext.ts";
import { useStableList } from "~/lib/useStableList.ts";
import {
  InboxColumn,
  rowSpoken,
  taskTitleOf,
  titleOf,
  type Settled as Given,
} from "./InboxColumn.tsx";
import type { AskAnswer } from "~/components/writes.tsx";
import { InboxPane } from "./InboxPane.tsx";
import { usePagedInbox } from "./pages.tsx";
import {
  answeredRows,
  earlierOf,
  inboxQuiet,
  inboxResources,
  inboxSelection,
  inboxRows,
  nextAfter,
  noticeRows,
  pairAnswered,
  readThrough,
  rowItem,
  rowKey,
  type InboxRow,
} from "./model.ts";

/**
 * The notices and the decisions are asked again on this beat. The notices are
 * pushed as well (`refetchOnInboxOf`), so for them this is the backstop for a
 * push lost to a reconnect; the decisions are not, so for them it is the beat.
 */
const INBOX_POLL_MS = 30_000;

/**
 * How long a settled item stays open, showing what happened, before the next
 * one opens: long enough to see the outcome land, short enough that a reader
 * working down the list never waits on it.
 */
export const ADVANCE_MS = 700;

/**
 * A row answered here: how and with what, and where the lists place it
 * ([SettledRow]). HELD where it stood in Needs you until the engine settles
 * it, at its place among the rows there; or MOVED into the Answered group,
 * settled for good, in the order it moved on this visit.
 */
type Settled = Given &
  ({ row: InboxRow; moved?: false; at: number } | { row: InboxRow; moved: true; order: number });

/**
 * Needs you with the rows answered here put back where they stood. The engine
 * drops an answered ask from `decisions` at the next read, and a row that
 * vanished the moment it was answered left the reader no sign the answer had
 * landed.
 */
/** A row's key, for [useStableList]. */
const rowKeyOf = (row: InboxRow) => row.key;

function keepSettled(
  needs: readonly InboxRow[],
  settled: ReadonlyMap<string, Settled>,
): InboxRow[] {
  const out = [...needs];
  const present = new Set(out.map((r) => r.key));
  // ONLY WHAT IS HELD stays where it stood: a settled answer has moved to
  // Answered ([pairAnswered]).
  const held = [...settled.values()]
    .flatMap((s) => (!s.moved && !present.has(s.row.key) ? [s] : []))
    .sort((a, b) => a.at - b.at);
  for (const s of held) out.splice(Math.min(s.at, out.length), 0, s.row);
  return out;
}

export function Inbox() {
  const viewer = useViewer();
  const now = useNow();
  const org = useOrg();
  const agents = useAgents();
  const nav = useNavigator();
  const index = useMemo(() => indexOrg(org), [org]);
  const zone = org?.timezone;
  const phone = useMediaMatch(`(width < ${PHONE_BREAKPOINT}px)`);
  // ONE PANE WHERE THE SCREEN IS NARROW, the list or the open row with Back:
  // on a phone, and beside an assistant the reader opened, which can leave a
  // desktop window's screen a phone's width. The SCREEN'S width, as the
  // stylesheet's `@container page` rule reads it, never the window's.
  const single = useScreenBelow(PHONE_BREAKPOINT);
  // THE LIST AND THE PANE EACH SCROLL ON THEIR OWN, so the screen takes the
  // window's height rather than growing the page, above a phone, where the
  // page scrolls as any other.
  useFillScreen(!phone);

  // THE OPEN ROW IS IN THE URL; what the quiet links opened is this visit's.
  const [open] = useParam("row", "");
  const [snoozedOpen, setSnoozedOpen] = useState(false);
  const [earlierOpen, setEarlierOpen] = useState(false);

  // A RECORD OF THEIR OWN: anybody signed in, bound to a seat or not, is
  // asked by the name their record is kept under (`ViewerState.owner`). Asked
  // by `handle`, an unbound reader had no inbox here at all while their
  // assistant wrote notices to their login. PUSHED AS WELL AS POLLED: the
  // engine sends `inbox_changed` when a committed batch moves that record
  // (`refetchOnInboxOf`), and the poll stays, because a frame lost to a
  // reconnect is never sent again.
  const owner = viewer.owner;
  const bound = owner !== "";
  // THE NOTICES ARE WORKLET'S, and asked only where the company runs it:
  // elsewhere the engine refuses every one of these reads.
  const runs = useServicesRun();
  const backends = useBackends();
  const notices = bound && runs.tracker;
  const updatesRead = useQuery(
    "work_inbox",
    notices ? { handle: owner, limit: INBOX_PAGE, unread: true, snoozed: "exclude" } : undefined,
    { enabled: notices, pollMs: INBOX_POLL_MS, refetchOnInboxOf: owner },
  );
  usePageCoverage(updatesRead.data);
  // THE SNOOZED COUNT IS READ WHETHER OR NOT THE LIST IS OPEN, because the link
  // says it; `only` IS WHAT WAS PUT OFF, and nothing else.
  const snoozed = usePagedInbox(
    notices ? { handle: owner, limit: INBOX_PAGE, snoozed: "only" } : undefined,
    INBOX_POLL_MS,
    owner,
  );
  // EARLIER IS READ ONLY ONCE IT IS OPENED: every notice not snoozed, of which
  // the read ones are listed (`earlierOf`).
  const earlier = usePagedInbox(
    notices && earlierOpen
      ? { handle: owner, limit: INBOX_PAGE, unread: false, snoozed: "exclude" }
      : undefined,
    INBOX_POLL_MS,
    owner,
  );
  const decisions = useQuery("decisions", undefined, {
    enabled: bound,
    pollMs: INBOX_POLL_MS,
  });
  // WHAT WAS ANSWERED LATELY, asked whatever runs the tracker: the asks are
  // Worklet's, but a coding run's question is answered here on every company,
  // and the engine lists those it took beside the asks.
  const answeredRead = useQuery("answered_decisions", undefined, {
    enabled: bound,
    pollMs: INBOX_POLL_MS,
  });
  const [answeredOpen, setAnsweredOpen] = useState(false);
  // THE SNOOZE BOUND, so a preset the engine would refuse is never offered.
  const person = useQuery("work_person", notices ? { handle: owner } : undefined, {
    enabled: notices,
  });
  const attention = useAttention();

  const rows = useMemo(
    () =>
      inboxRows({
        // EVERY SEAT STOPPED ON ITS BUDGET, not only the ones this reader can
        // raise or hand on: Home counts the rest as conditions that need a
        // look and sends the reader here, and a list that dropped them would
        // be the one place that count is not drawn. The row's own controls
        // say why a reader who cannot act cannot.
        subjects: bound ? decisionSubjects(decisions.data, seatConditionsOf(agents)) : [],
        conditions: conditionsToDecide(attention),
        notices: notices ? (updatesRead.data?.notices ?? []) : [],
      }),
    [bound, notices, decisions.data, agents, attention, updatesRead.data],
  );

  // THE ROWS DRAWN LAST, WHERE NOTHING IN THEM CHANGED. They are derived
  // from the seats' live state, which the engine pushes many times a second
  // while a seat works: rows rebuilt on each push, alike in every field, made
  // every row draw again, and a condition that came and went with each push
  // moved the rows below it (useStableList).
  const needsNow = useStableList(rows.needs, rowKeyOf);
  const updates = useStableList(rows.updates, rowKeyOf);
  const [settled, setSettled] = useState<ReadonlyMap<string, Settled>>(new Map());
  // THE ROW THAT HAS JUST LANDED IN ANSWERED, which washes the group's
  // heading: set by an answer, never on a first draw.
  const [arrived, setArrived] = useState<string | undefined>(undefined);
  // ANSWERED BEFORE THIS VISIT: a question answered here is in Needs you,
  // and one answered before it is under Answered, never in both places.
  const { needs, answered } = useMemo(
    () => pairAnswered(keepSettled(needsNow, settled), answeredRows(answeredRead.data), settled),
    [needsNow, settled, answeredRead.data],
  );
  const snoozedRows = snoozedOpen && snoozed.notices ? noticeRows(snoozed.notices) : null;
  const earlierRows =
    earlierOpen && earlier.notices ? noticeRows(earlierOf(earlier.notices)) : null;
  const order: InboxRow[] = [
    ...needs,
    ...updates,
    ...(answeredOpen ? answered : []),
    ...(snoozedRows ?? []),
    ...(earlierRows ?? []),
  ];
  // AN ANSWERED ROW THE ADDRESS NAMES OPENS ITS GROUP, so the open row is
  // never one the list is hiding: a link to an answer, or a return to one.
  // NOT ONE ANSWERED HERE: the next item opens with it, and the row it left
  // lands in the closed group's count rather than opening the group.
  useEffect(() => {
    if (
      open &&
      !answeredOpen &&
      !settled.get(open)?.moved &&
      answered.some((r) => r.key === open)
    ) {
      setAnsweredOpen(true);
    }
  }, [open, answeredOpen, answered, settled]);

  // THE OPEN ROW: the one the URL names, or, beside the list (never in one
  // pane, where the pane replaces it), the first one not yet dealt with, so the pane
  // is never an empty box beside a list with something in it. A row the URL
  // names that has just LEFT the list (an update read a moment ago) stays open
  // until the next one opens: what happened to it is what the pane shows.
  const held = useRef<InboxRow | null>(null);
  const named =
    order.find((r) => r.key === open) ??
    answered.find((r) => r.key === open) ??
    (open && held.current?.key === open ? held.current : null);
  const selected =
    named ??
    (single
      ? null
      : (order.find((r) => r.kind !== "answered" && !settled.has(r.key)) ?? order[0] ?? null));
  useEffect(() => {
    if (selected) held.current = selected;
  }, [selected]);
  // WHAT NEEDS YOU IS ABOUT, named for the assistant: the screen's list is
  // built here and no route answers it, so a question asked on this screen
  // reads these records rather than the task notices alone. The row the
  // reader opened is its selection, so a brief asked now is of that row.
  useAssistantContext({ resources: inboxResources(needs), selection: inboxSelection(named) });

  const openRow = useCallback((key: string) => nav.filter({ row: key }), [nav]);

  // THE NEXT ROW, once the open one is settled. Read from refs when the timer
  // fires rather than from this render: by then the list has been re-read.
  const live = useRef({ open, order, settled, index });
  live.current = { open, order, settled, index };
  // THE MOVES ON THIS VISIT, counted, so Answered's head is in the order the
  // rows moved ([pairAnswered]): a run's answer is held as sent before it
  // moves, and its entry was made before rows answered in the meantime.
  const moves = useRef(0);
  // WHAT HAS MOVED, written as each answer moves rather than read off the
  // last draw: two answers told in one tick both met a draw that had moved
  // neither, so each moved the row, announced it and opened the next.
  const moved = useRef(new Set<string>());
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);
  const settle = useCallback(
    (row: InboxRow, label: string | null, answer?: AskAnswer, hold?: boolean) => {
      if (hold) {
        // KEPT OPEN: marked with the label where it stands, or the label
        // taken back, and no move to the next row. See [PaneProps.onSettled].
        setSettled((was) => {
          const next = new Map(was);
          if (!label) {
            next.delete(row.key);
            return next;
          }
          const at = live.current.order
            .filter((r) => r.kind !== "notice" && r.kind !== "answered")
            .findIndex((r) => r.key === row.key);
          const given: Settled = { label, row, at: Math.max(0, at) };
          if (answer) given.answer = answer;
          return next.set(row.key, given);
        });
        return;
      }
      const before = live.current.order.map((r) => r.key);
      if (label) {
        // ONCE: a second word on a row already moved (a press repeated before
        // the list drew again) answers nothing and moves nobody on twice.
        if (moved.current.has(row.key)) return;
        moved.current.add(row.key);
        const given: Settled = { label, row, moved: true, order: ++moves.current };
        if (answer) given.answer = answer;
        setSettled((was) => new Map(was).set(row.key, given));
        setArrived(row.key);
        // THE NEXT THING TO DEAL WITH OPENS NOW, never one answered before
        // (the Answered group's rows), and the move is said once.
        const order = live.current.order;
        const next = nextAfter(
          row.key,
          before,
          before,
          new Set([
            ...live.current.settled.keys(),
            ...moved.current,
            ...order.filter((r) => r.kind === "answered").map((r) => r.key),
          ]),
        );
        const coming = next ? order.find((r) => r.key === next) : undefined;
        announce(
          coming
            ? `Answered. Next: ${rowSpoken(coming, live.current.index)}`
            : "Answered. Nothing else is waiting on you.",
        );
        window.clearTimeout(timer.current);
        if (next) nav.filter({ row: next });
        return;
      }
      // NAMED FIRST, so the row stays open while it leaves the list.
      if (live.current.open !== row.key) nav.filter({ row: row.key });
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => {
        const { open: now, order: after, settled: done } = live.current;
        // THE READER MOVED ON: the advance is theirs now, not this one's.
        if (now !== row.key) return;
        // NEVER INTO WHAT WAS ANSWERED BEFORE: the next item is the next
        // thing to deal with.
        const next = nextAfter(
          row.key,
          before,
          after.map((r) => r.key),
          new Set([
            ...done.keys(),
            ...after.filter((r) => r.kind === "answered").map((r) => r.key),
          ]),
        );
        if (next) nav.filter({ row: next });
      }, ADVANCE_MS);
    },
    [nav],
  );

  // j AND k STEP THROUGH THE LIST, opening each row in the pane.
  const at = selected ? order.findIndex((r) => r.key === selected.key) : -1;
  useKeymap({
    "list.next": {
      run: () => {
        const next = order[Math.min(order.length - 1, at + 1)];
        if (next) openRow(next.key);
      },
      when: order.length > 0,
    },
    "list.previous": {
      run: () => {
        const prev = order[Math.max(0, at - 1)];
        if (prev) openRow(prev.key);
      },
      when: order.length > 0,
    },
  });

  // THE TASKS' TITLES, in one read for every row that does not carry its own:
  // a notice names its task by key and id alone. The read is by key, and a key
  // two tasks hold answers both, so a title is filed under its task's ID and
  // under its key only for the task that claimed it ([titleOf]).
  const keys = useMemo(() => {
    const wanted = new Set<string>();
    for (const r of [...order, ...(selected ? [selected] : [])]) {
      const key = rowKey(r);
      if (key && !taskTitleOf(r)) wanted.add(key);
    }
    return [...wanted].sort();
  }, [order, selected]);
  const tasks = useQuery(
    "work_items",
    keys.length > 0 ? { key: keys.join(","), show_closed: true, limit: keys.length } : undefined,
    { enabled: keys.length > 0 },
  );
  const titles = useMemo(() => {
    const out = new Map<string, string>();
    for (const t of tasks.data?.items ?? []) {
      out.set(t.id, t.title);
      if (!t.key_collision) out.set(t.key, t.title);
    }
    return out;
  }, [tasks.data]);

  const loaded = updatesRead.data?.notices ?? [];
  const quiet = inboxQuiet({
    answer: updatesRead.data,
    bound: notices,
    error: Boolean(updatesRead.error || decisions.error),
    shown: needs.length + updates.length,
    watched: watchedIn("seat"),
  });
  const showList = !single || !named;
  const showPane = !single || named !== null;
  const given = useMemo(
    () =>
      new Map(
        [...settled].map(
          ([key, s]) =>
            [key, s.answer ? { label: s.label, answer: s.answer } : { label: s.label }] as const,
        ),
      ),
    [settled],
  );

  return (
    <div className="inbox-frame" data-phone-pane={single && named ? "true" : undefined}>
      {/* MARK ALL READ IS WORKLET'S: hidden where the company runs none,
          and otherwise drawn for every reader, held with its reason where
          there is nothing of theirs to mark. */}
      {runs.tracker && (
        <PageActions>
          <MarkAllRead through={notices ? readThrough(loaded) : null} />
        </PageActions>
      )}

      {/* THREE VIEWER STATES, three sentences, and an unbound reader's says
          what binding would add (`UnboundRemedy`): it is a service account or
          a token's session, ordinary, never a person, since every person holds
          a human seat. The conditions a person decides are listed for all
          three, which is why the screen is not simply locked. */}
      {viewer.anonymous ? (
        <Callout variant="warning" className="inbox-callout">
          Nobody is signed in. Sign in to see your updates and decisions.
        </Callout>
      ) : viewer.unbound ? (
        <Callout variant="info" className="inbox-callout">
          You are <InlineCode>{viewer.login}</InlineCode> and not bound to a seat, so these are the
          updates kept under that login. <UnboundRemedy />
        </Callout>
      ) : null}

      {snoozed.readers}
      {earlier.readers}
      <div className="inbox-panes">
        {showList && (
          <InboxColumn
            needs={needs}
            updates={updates}
            snoozed={{
              rows: snoozedRows,
              more: snoozed.more,
              open: snoozedOpen,
              onOpenChange: setSnoozedOpen,
            }}
            snoozedCount={
              snoozed.first
                ? { count: snoozed.first.notices.length, floor: Boolean(snoozed.first.next_cursor) }
                : null
            }
            earlier={{
              rows: earlierRows,
              more: earlier.more,
              open: earlierOpen,
              onOpenChange: setEarlierOpen,
            }}
            onEarlierMore={earlier.next}
            settled={given}
            answered={{
              rows: answered,
              open: answeredOpen,
              onOpenChange: setAnsweredOpen,
              arrived,
            }}
            reader={owner}
            selected={selected?.key ?? ""}
            onOpen={openRow}
            titles={titles}
            index={index}
            now={now}
            loading={notices && updatesRead.loading && !updatesRead.data}
            quiet={runs.tracker ? quiet : null}
            more={Boolean(updatesRead.data?.next_cursor)}
            bound={notices}
            elsewhere={runs.tracker ? null : elsewhereLine(backends?.tracker ?? null)}
            refusal={
              // A READ THAT FAILED IS SAID, never drawn as a quiet list: each
              // read's own refusal, where the rows it would have given go.
              (updatesRead.error ||
                decisions.error ||
                answeredRead.error ||
                snoozed.error ||
                earlier.error) && (
                <>
                  <QueryState
                    error={updatesRead.error}
                    refusal={updatesRead.refusal}
                    detail={updatesRead.detail}
                    loading={false}
                  >
                    {null}
                  </QueryState>
                  <QueryState
                    error={decisions.error}
                    refusal={decisions.refusal}
                    detail={decisions.detail}
                    loading={false}
                  >
                    {null}
                  </QueryState>
                  <QueryState
                    error={answeredRead.error}
                    refusal={answeredRead.refusal}
                    detail={answeredRead.detail}
                    loading={false}
                  >
                    {null}
                  </QueryState>
                  <QueryState
                    error={snoozed.error ?? earlier.error}
                    refusal={snoozed.error ? snoozed.refusal : earlier.refusal}
                    loading={false}
                  >
                    {null}
                  </QueryState>
                </>
              )
            }
          />
        )}
        {showPane && (
          <InboxPane
            row={selected}
            empty={runs.tracker ? undefined : paneElsewhere(backends?.tracker ?? null)}
            listEmpty={Boolean(quiet) && runs.tracker}
            index={index}
            now={now}
            zone={zone}
            maxSnoozeAhead={person.data?.max_snooze_ahead}
            title={
              selected ? (titleOf(titles, rowItem(selected)) ?? taskTitleOf(selected)) : undefined
            }
            onBack={single ? () => nav.filter({ row: null }) : undefined}
            onSettled={(label, answer, hold) => selected && settle(selected, label, answer, hold)}
          />
        )}
      </div>
    </div>
  );
}

/**
 * Where asks and mentions go where the company runs no Worklet, as far
 * as the engine has said which tracker it runs.
 */
export function elsewhereLine(tracker: OrgServices["tracker"] | null): string {
  if (tracker === "jira") return "Jira handles work items, so asks and mentions arrive there.";
  if (tracker === "none") {
    return "The company runs no work tracker, so no asks or mentions arrive here.";
  }
  return "Work items live outside Crewlet, so asks and mentions don't arrive here.";
}

/**
 * What the reading pane says with nothing open where the company runs no
 * Worklet: "All clear. New updates open here." promised updates that
 * cannot arrive. It says where asks and mentions go instead, and what still
 * comes here.
 */
export function paneElsewhere(tracker: OrgServices["tracker"] | null): {
  title: string;
  description: string;
} {
  const description = "Seats stopped on a budget and the engine's conditions still arrive here.";
  if (tracker === "jira") return { title: "Asks and mentions arrive in Jira", description };
  return { title: "No asks or mentions arrive here", description };
}

/**
 * "Mark all read", which reads through the NEWEST UPDATE LOADED and nothing
 * past it: an update that arrived after this page was drawn is one the person
 * has not seen.
 */
function MarkAllRead({ through }: { through: string | null }) {
  const write = useAct("mark_inbox");
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        leadingIcon={<CheckGlyph size="sm" />}
        showRefusal={false}
        blocked={through ? undefined : "Nothing on this page is unread."}
        onPress={() =>
          through && void write.run({ read_through: through }, { done: "Marked all read" })
        }
      >
        Mark all read
      </WriteButton>
      <RefusalNote write={write} />
    </>
  );
}
