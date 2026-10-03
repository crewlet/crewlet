/**
 * The Inbox — the place a person acts on what reached them, built around the
 * decisions waiting on them.
 *
 * # One list, then one pane
 *
 * The left column is ONE list in a fixed order: "Needs a decision" first —
 * the asks put to this person and the coding runs parked on a question to
 * them (the engine's `decisions`), the seats stopped on a spent budget, and
 * the engine conditions a person decides (`lib/attention.ts`, `where: seat`)
 * — then the notices that reached them, by the company's day. Decisions come
 * first because they are what only this person can unblock; they are never
 * interleaved with the notices by time, which would eventually rank "a task
 * you watch moved" above the CEO asking whether to hold a release.
 *
 * The right column is the open row, in full, with the answer to it
 * (`DecisionPane.tsx`). Stepping down the list REPLACES history, so four rows
 * read through one open pane are one place the reader has been — the rule the
 * frame's own peek follows — and `j`/`k` step through it from the keyboard.
 *
 * # What is where
 *
 * The ENGINE's own conditions (no configuration, a draining node, a refused
 * token) are the sidebar's health card and Home's sentence, on every screen,
 * and a stalled round or the company's parked runs are Live's; this list
 * holds only what a person decides (`WHERE_OF`). The scope is a question to
 * the engine (Unread, All, Snoozed — `snoozed=exclude|include|only`), and the
 * Unread option carries the page's unread count inside it, as the approved
 * design draws it; the chips narrow the rows loaded, and where the page is not
 * every row each chip says its count is the page's.
 *
 * # Whose inbox
 *
 * THE READER'S OWN RECORD, under `owner` — their seat when the identity
 * directory binds them to one, their login when it does not — which is the
 * name every notice routed to them, and every mark their assistant made, is
 * kept under. An unbound reader is an ordinary state, not a fault: what they
 * follow and what names them reaches that record, and the screen draws it.
 * Nobody types whose inbox this is — no question takes a `viewer=`.
 *
 * PUSHED AS WELL AS POLLED: the frame watches the reader's own record, the
 * engine sends `inbox_changed` when a committed batch moves it, and the list
 * is asked again within half a second (`refetchOnInboxOf`). The poll stays,
 * because a frame lost to a reconnect is never re-sent.
 *
 * # Every mark is a gesture, made as you
 *
 * Done, Snooze and "Mark all read" each name exactly what they mark
 * (`mark_inbox{read}`, `{snooze}`, `{read_through}` at the newest notice
 * loaded) as the person signed in (ADR-0024). The engine changes that and
 * nothing else, and the list is re-read at the position the write answered
 * with, so a row moves only once the engine says it has.
 *
 * # The clock
 *
 * READ WHERE IT IS SHOWN. A row's "12m", the pane's "asked you 3h ago" and a
 * parked run's countdown each read the clock in their own cell; the screen
 * itself reads only the company's MIDNIGHT, which is what cuts Today from
 * Yesterday and moves once a day. Taking the second here drew every row of
 * both groups once a second to change the handful whose words moved.
 */

import { useCallback, useMemo } from "react";
import { Callout } from "@crewlethq/ui";
import { CheckGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator, useParam } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { useKeymap } from "~/app/keymap.ts";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import { decisionSubjects, seatConditionsOf } from "~/components/DecisionRow.tsx";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useClockReading } from "~/lib/clock.ts";
import { companyMidnight } from "~/lib/range.ts";
import { useMediaQuery } from "~/lib/media.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { conditionsToDecide, watchedIn } from "~/lib/attention.ts";
import { useAttention } from "~/lib/useAttention.ts";
import { INBOX_PAGE } from "~/lib/useInboxCounts.ts";
import { NoticeList } from "./NoticeList.tsx";
import { DecisionPane } from "./DecisionPane.tsx";
import {
  chipCounts,
  chipOf,
  dayGroups,
  inboxQuiet,
  inboxRows,
  readThrough,
  rowMatches,
  scopeOf,
  type InboxRow,
  type Scope,
} from "./model.ts";

/**
 * The notices and the decisions are asked again on this beat. The notices are
 * pushed as well (`refetchOnInboxOf`), so for them this is the backstop for a
 * push lost to a reconnect; the decisions are not, so for them it is the beat.
 */
const INBOX_POLL_MS = 30_000;

export function Inbox() {
  const viewer = useViewer();
  const org = useOrg();
  const agents = useAgents();
  const nav = useNavigator();
  const index = useMemo(() => indexOrg(org), [org]);
  const zone = org?.timezone;
  // THE COMPANY'S MIDNIGHT, the one reading of the clock the list itself
  // takes: it moves once a day, and with it a notice from Today to Yesterday.
  const midnight = useClockReading((now) => companyMidnight(now, zone));
  const phone = useMediaQuery(`(width < ${PHONE_BREAKPOINT}px)`);
  // THE LIST AND THE PANE EACH SCROLL ON THEIR OWN, so the screen takes the
  // window's height rather than growing the page — above a phone, where the
  // two are one column and the page scrolls as any other.
  useFillScreen(!phone);

  // THE STATE IS IN THE URL: the scope, the chip and the open row.
  const [scopeParam, setScope] = useParam("scope", "unread");
  const scope: Scope = scopeOf(scopeParam);
  const [chipParam, setChip] = useParam("reason", "");
  const chip = chipOf(chipParam);
  const [open] = useParam("row", "");

  // A RECORD OF THEIR OWN — anybody signed in, bound to a seat or not. Asked
  // by `handle`, an unbound reader had no inbox here at all while their
  // assistant wrote notices to their login.
  const owner = viewer.owner;
  const bound = owner !== "";
  const inbox = useQuery(
    "work_inbox",
    bound
      ? {
          handle: owner,
          limit: INBOX_PAGE,
          unread: scope === "unread",
          // `only` IS WHAT WAS PUT OFF, and nothing else.
          snoozed: scope === "snoozed" ? "only" : "exclude",
        }
      : undefined,
    { enabled: bound, pollMs: INBOX_POLL_MS, refetchOnInboxOf: owner },
  );
  usePageCoverage(inbox.data);
  // THE CALLER'S OWN: the question names nobody, and the engine answers for
  // the principal that asked.
  const decisions = useQuery("decisions", undefined, {
    enabled: bound,
    pollMs: INBOX_POLL_MS,
  });
  // THE SNOOZE BOUND, so a preset the engine would refuse is never offered.
  const person = useQuery("work_person", bound ? { handle: owner } : undefined, {
    enabled: bound,
  });
  const attention = useAttention();

  const rows = useMemo(
    () =>
      inboxRows({
        scope,
        // EVERY SEAT STOPPED ON ITS BUDGET, not only the ones this reader can
        // raise or hand on: Home counts the rest as conditions that need a
        // look and sends the reader here, and a list that dropped them would
        // be the one place that count is not drawn. The row's own controls
        // say why a reader who cannot act cannot.
        subjects: bound ? decisionSubjects(decisions.data, seatConditionsOf(agents)) : [],
        conditions: conditionsToDecide(attention),
        notices: bound ? (inbox.data?.notices ?? []) : [],
      }),
    [scope, bound, decisions.data, agents, attention, inbox.data],
  );
  const counts = useMemo(() => chipCounts(rows), [rows]);
  const shownDecisions = rows.decisions.filter((r) => rowMatches(r, chip));
  const shownNotices = rows.notices.filter((r) => rowMatches(r, chip));
  const groups = dayGroups(shownNotices, midnight, zone);
  const visible: InboxRow[] = [...shownDecisions, ...groups.flatMap((g) => g.rows)];

  // THE OPEN ROW: the one the URL names, or — beside the list, never on a
  // phone where the pane replaces it — the first one, so the pane is never an
  // empty box beside a list with something in it.
  const named = visible.find((r) => r.id === open) ?? null;
  const selected = named ?? (phone ? null : (visible[0] ?? null));
  const openRow = useCallback((id: string) => nav.filter({ row: id }), [nav]);

  // j AND k STEP THROUGH THE LIST, opening each row in the pane.
  const at = selected ? visible.findIndex((r) => r.id === selected.id) : -1;
  useKeymap({
    "list.next": {
      run: () => {
        const next = visible[Math.min(visible.length - 1, at + 1)];
        if (next) openRow(next.id);
      },
      when: visible.length > 0,
    },
    "list.previous": {
      run: () => {
        const prev = visible[Math.max(0, at - 1)];
        if (prev) openRow(prev.id);
      },
      when: visible.length > 0,
    },
  });

  const loaded = inbox.data?.notices ?? [];
  const unreadOnPage =
    scope === "unread" && inbox.data
      ? { count: loaded.length, floor: Boolean(inbox.data.next_cursor) }
      : null;
  const quiet = inboxQuiet({
    answer: inbox.data,
    bound,
    error: Boolean(inbox.error),
    scope,
    chip,
    shown: visible.length,
    watched: watchedIn("seat"),
  });
  const showList = !phone || !named;
  const showPane = !phone || named !== null;

  return (
    <div className="inbox-frame" data-phone-pane={phone && named ? "true" : undefined}>
      <PageActions>
        <MarkAllRead through={bound ? readThrough(loaded) : null} />
      </PageActions>

      {/* THREE VIEWER STATES, three sentences — and only one of them is
          anybody's fault. The conditions a person decides are listed for all
          three, which is why the screen is not simply locked. */}
      {viewer.anonymous ? (
        <Callout variant="warning" className="inbox-callout">
          Nobody is signed in and no API token is presented, so this browser is nobody. The
          conditions below are the engine&rsquo;s; your own notices and decisions appear once you
          sign in.
        </Callout>
      ) : viewer.unbound ? (
        <Callout variant="info" className="inbox-callout">
          You are <code className="inline">{viewer.login}</code> and the directory binds you to no
          seat, so the notices below are the ones kept under that login — what you follow and what
          names you. Work the org chart hands to a seat reaches you once{" "}
          <code className="inline">crewlet iam bind</code> binds you to one.
        </Callout>
      ) : null}

      <div className="inbox-panes">
        {showList && (
          <NoticeList
            scope={scope}
            onScope={(next) => setScope(next)}
            unread={unreadOnPage}
            chip={chip}
            onChip={(next) => setChip(next)}
            counts={counts}
            decisions={shownDecisions}
            groups={groups}
            selected={selected?.id ?? ""}
            onOpen={openRow}
            index={index}
            loading={bound && inbox.loading && !inbox.data}
            quiet={quiet}
            more={Boolean(inbox.data?.next_cursor)}
            // THE CHIPS COUNT ROWS LOADED FROM TWO READS, and either can be
            // the one that stopped: the notices at their page, the decisions
            // at theirs (fewer items than the total the engine counted).
            pageLocal={
              (bound && Boolean(inbox.data?.next_cursor)) ||
              // READ AS `decisionSubjects` READS IT, so an answer with no
              // items is an empty page rather than a crash.
              (decisions.data?.items ?? []).length < (decisions.data?.total ?? 0)
            }
            showDecisions={scope !== "snoozed"}
            showNotices={bound}
            refusal={
              // A READ THAT FAILED IS SAID, never drawn as a quiet list: each
              // read's own refusal, where the rows it would have given go.
              (inbox.error || decisions.error) && (
                <>
                  <QueryState error={inbox.error} refusal={inbox.refusal} loading={false}>
                    {null}
                  </QueryState>
                  <QueryState error={decisions.error} refusal={decisions.refusal} loading={false}>
                    {null}
                  </QueryState>
                </>
              )
            }
          />
        )}
        {showPane && (
          <DecisionPane
            row={selected}
            index={index}
            viewerHandle={viewer.handle}
            zone={zone}
            maxSnoozeAhead={person.data?.max_snooze_ahead}
            onBack={phone ? () => nav.filter({ row: null }) : undefined}
          />
        )}
      </div>
      {!bound && !viewer.loading && (
        <p className="t-caption inbox-nobody">
          Signed in, this screen also shows what reached you and what waits on your decision.
        </p>
      )}
    </div>
  );
}

/**
 * "Mark all read", which reads through the NEWEST NOTICE LOADED and nothing
 * past it: a notice that arrived after this page was drawn is one the person
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
