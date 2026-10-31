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
 * # Where the company runs no Crewlet Works
 *
 * Updates, Snoozed and Earlier are the tracker's notices, and Mark all read
 * marks them, so where the company runs another tracker or none
 * (`lib/services.ts`) none of them is asked for or drawn: one line says where
 * asks and mentions go instead ([elsewhereLine]). Needs you stays, because
 * what fills it without the tracker still arrives: the seats stopped on a
 * spent budget and the engine's conditions (the agents and budget pushes) and
 * the coding runs parked on a question (`decisions`, which answers them
 * whatever runs the tracker).
 *
 * # What is done is shown, then the next item opens
 *
 * An answered question or run stays in its place in the list, struck through,
 * and its pane shows what was answered; a read or snoozed update shows its new
 * state. A moment later (`ADVANCE_MS`) the next item opens, unless the reader
 * has already moved on.
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
import { Callout, InlineCode, useMediaMatch } from "@crewlethq/ui";
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
import { InboxColumn, taskTitleOf, titleOf } from "./InboxColumn.tsx";
import { InboxPane } from "./InboxPane.tsx";
import { usePagedInbox } from "./pages.ts";
import {
  earlierOf,
  inboxQuiet,
  inboxRows,
  isNoticeRow,
  nextAfter,
  noticeRows,
  readThrough,
  rowItem,
  rowKey,
  type InboxRow,
  type ReadThrough,
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

/** A row answered here: how, and where it stood in Needs you. */
interface Settled {
  label: string;
  row: InboxRow;
  at: number;
}

/**
 * Needs you with the rows answered here put back where they stood. The engine
 * drops an answered ask from `decisions` at the next read, and a row that
 * vanished the moment it was answered left the reader no sign the answer had
 * landed.
 */
function keepSettled(
  needs: readonly InboxRow[],
  settled: ReadonlyMap<string, Settled>,
): InboxRow[] {
  const out = [...needs];
  const present = new Set(out.map((r) => r.key));
  const held = [...settled.values()]
    .filter((s) => !present.has(s.row.key))
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
  // THE LIST AND THE PANE EACH SCROLL ON THEIR OWN, so the screen takes the
  // window's height rather than growing the page, above a phone, where the
  // two are one column and the page scrolls as any other.
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
  // THE NOTICES ARE EVERY NATIVE DOMAIN'S, composed by the engine (`inbox`):
  // the tracker's where the company runs Crewlet Works and the reader holds
  // Tracker, the code host's where it hosts code — each under its own
  // permission, which the engine decides — so they are asked for every reader
  // with a record, and a company running neither answers an empty list.
  const runs = useServicesRun();
  const backends = useBackends();
  const notices = bound && (runs.tracker || runs.code);
  const updatesRead = useQuery(
    "inbox",
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
  // THE SNOOZE BOUND, so a preset the engine would refuse is never offered:
  // the tracker's person record states it, and the code host keeps the same
  // rule (internal/inbox's MaxSnoozeAhead).
  const person = useQuery("work_person", bound && runs.tracker ? { handle: owner } : undefined, {
    enabled: bound && runs.tracker,
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

  const [settled, setSettled] = useState<ReadonlyMap<string, Settled>>(new Map());
  const needs = useMemo(() => keepSettled(rows.needs, settled), [rows.needs, settled]);
  const snoozedRows = snoozedOpen && snoozed.notices ? noticeRows(snoozed.notices) : null;
  const earlierRows =
    earlierOpen && earlier.notices ? noticeRows(earlierOf(earlier.notices)) : null;
  const order: InboxRow[] = [
    ...needs,
    ...rows.updates,
    ...(snoozedRows ?? []),
    ...(earlierRows ?? []),
  ];

  // THE OPEN ROW: the one the URL names, or, beside the list (never on a phone,
  // where the pane replaces it), the first one not yet dealt with, so the pane
  // is never an empty box beside a list with something in it. A row the URL
  // names that has just LEFT the list (an update read a moment ago) stays open
  // until the next one opens: what happened to it is what the pane shows.
  const held = useRef<InboxRow | null>(null);
  const named =
    order.find((r) => r.key === open) ?? (open && held.current?.key === open ? held.current : null);
  const selected =
    named ?? (phone ? null : (order.find((r) => !settled.has(r.key)) ?? order[0] ?? null));
  useEffect(() => {
    if (selected) held.current = selected;
  }, [selected]);

  const openRow = useCallback((key: string) => nav.filter({ row: key }), [nav]);

  // THE NEXT ROW, once the open one is settled. Read from refs when the timer
  // fires rather than from this render: by then the list has been re-read.
  const live = useRef({ open, order, settled });
  live.current = { open, order, settled };
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);
  const settle = useCallback(
    (row: InboxRow, label: string | null) => {
      const before = live.current.order.map((r) => r.key);
      if (label) {
        const at = live.current.order
          .filter((r) => !isNoticeRow(r))
          .findIndex((r) => r.key === row.key);
        setSettled((was) => new Map(was).set(row.key, { label, row, at: Math.max(0, at) }));
      }
      // NAMED FIRST, so the row stays open while it leaves the list.
      if (live.current.open !== row.key) nav.filter({ row: row.key });
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => {
        const { open: now, order: after, settled: done } = live.current;
        // THE READER MOVED ON: the advance is theirs now, not this one's.
        if (now !== row.key) return;
        const next = nextAfter(
          row.key,
          before,
          after.map((r) => r.key),
          new Set(done.keys()),
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
    shown: needs.length + rows.updates.length,
    watched: watchedIn("seat"),
  });
  const showList = !phone || !named;
  const showPane = !phone || named !== null;
  const settledLabels = useMemo(
    () => new Map([...settled].map(([key, s]) => [key, s.label] as const)),
    [settled],
  );

  return (
    <div className="inbox-frame" data-phone-pane={phone && named ? "true" : undefined}>
      {/* MARK ALL READ IS EVERY SOURCE'S: hidden where the company runs no
          source of notices, and otherwise drawn for every reader, held with
          its reason where there is nothing of theirs to mark. */}
      {(runs.tracker || runs.code) && (
        <PageActions>
          <MarkAllRead through={notices ? readThrough(loaded) : NOTHING_TO_READ} />
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

      <div className="inbox-panes">
        {showList && (
          <InboxColumn
            needs={needs}
            updates={rows.updates}
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
            settled={settledLabels}
            selected={selected?.key ?? ""}
            onOpen={openRow}
            titles={titles}
            index={index}
            now={now}
            loading={notices && updatesRead.loading && !updatesRead.data}
            quiet={runs.tracker || runs.code ? quiet : null}
            more={Boolean(updatesRead.data?.next_cursor)}
            bound={notices}
            elsewhere={runs.tracker || runs.code ? null : elsewhereLine(backends?.tracker ?? null)}
            refusal={
              // A READ THAT FAILED IS SAID, never drawn as a quiet list: each
              // read's own refusal, where the rows it would have given go.
              (updatesRead.error || decisions.error || snoozed.error || earlier.error) && (
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
            empty={runs.tracker || runs.code ? undefined : paneElsewhere(backends?.tracker ?? null)}
            listEmpty={Boolean(quiet) && (runs.tracker || runs.code)}
            index={index}
            now={now}
            zone={zone}
            maxSnoozeAhead={person.data?.max_snooze_ahead}
            title={
              selected ? (titleOf(titles, rowItem(selected)) ?? taskTitleOf(selected)) : undefined
            }
            onBack={phone ? () => nav.filter({ row: null }) : undefined}
            onSettled={(label) => selected && settle(selected, label)}
          />
        )}
      </div>
    </div>
  );
}

/**
 * Where asks and mentions go where the company runs no Crewlet Works, as far
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
 * Crewlet Works: "All clear. New updates open here." promised updates that
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

/** A page with nothing unread on it, from either source. */
const NOTHING_TO_READ: ReadThrough = { tracker: null, code: null };

/**
 * "Mark all read", which reads EACH SOURCE through the NEWEST UPDATE LOADED of
 * its and nothing past it: an update that arrived after this page was drawn
 * is one the person has not seen. Each domain keeps its own marks on its own
 * log, so the gesture is one write to each source with anything unread here —
 * `mark_inbox` for the tracker's notices, `code_mark_inbox` for the code
 * host's — and the list is re-read past both.
 */
function MarkAllRead({ through }: { through: ReadThrough }) {
  const tracker = useAct("mark_inbox");
  const code = useAct("code_mark_inbox");
  const nothing = !through.tracker && !through.code;
  // THE BUTTON STANDS FOR WHICHEVER WRITE IT SENDS FIRST, so its access and
  // its busy state are that write's; the other's refusal is shown beside it.
  const lead = through.tracker || !through.code ? tracker : code;
  return (
    <>
      <WriteButton
        write={lead}
        size="small"
        variant="ghost"
        leadingIcon={<CheckGlyph size="sm" />}
        showRefusal={false}
        blocked={nothing ? "Nothing on this page is unread." : undefined}
        onPress={() => {
          if (through.tracker) {
            void tracker.run({ read_through: through.tracker }, { done: "Marked all read" });
          }
          if (through.code) {
            void code.run({ read_through: through.code }, { done: "Marked all read" });
          }
        }}
      >
        Mark all read
      </WriteButton>
      <RefusalNote write={tracker} />
      <RefusalNote write={code} />
    </>
  );
}
