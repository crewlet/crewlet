/**
 * The Inbox — the place a person acts on what reached them, built around the
 * decisions waiting on them.
 *
 * # One list, then one pane
 *
 * The left column is ONE list in a fixed order: "Needs a decision" first —
 * the asks put to this person and the coding runs parked on a question to
 * them (the engine's `decisions`) — then the notices that reached them, by
 * the company's day. Decisions come
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
 * The ENGINE's conditions — a seat stopped on its budget or failed, a budget
 * refusing, a draining node, a stalled round — are the console's: its
 * Overview's "Needs an admin", its health card and Live. They are read from
 * what only an admin is served and answered in the console, and this screen
 * asks only what a member is (`app/shell.test.ts`), so it holds what was put
 * to this person and what reached them, and nothing about the machine. The
 * scope is a question to
 * the engine (Unread, All, Snoozed — `snoozed=exclude|include|only`), and the
 * Unread option carries the page's unread count inside it, as the approved
 * design draws it; the chips narrow the rows loaded, and where the page is not
 * every row each chip says its count is the page's.
 *
 * # Two figures, two names
 *
 * Unread lists every unread notice, so its count is every unread notice —
 * what waits on the person and what they were only told. The sidebar's badge
 * counts the first half alone (`lib/useInboxCounts.ts` says why), so this
 * screen used to open on "Unread 12" under a badge saying 3, with nothing to
 * say which was right. Both are: the badge's figure is drawn here too, beside
 * the scope, under the badge's own words and from the FRAME's reading — never
 * a second read of it, and never a count of this page's primary notices, which
 * is a different number whenever the page fills with notices that merely
 * informed.
 *
 * # Every mark is a gesture, made as you
 *
 * Done, Snooze and "Mark all read" each name exactly what they mark
 * (`mark_inbox{read}`, `{snooze}`, `{read_through}` at the newest notice
 * loaded) as the person the token is bound to (ADR-0024). The engine changes
 * that and nothing else, and the list is re-read at the position the write
 * answered with, so a row moves only once the engine says it has.
 */

import { useCallback, useMemo } from "react";
import { Callout } from "@crewlethq/ui";
import { CheckGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator, useParam } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePageCoverage } from "~/app/page.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { useKeymap } from "~/app/keymap.ts";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import { decisionSubjects } from "~/components/DecisionRow.tsx";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useNow } from "~/lib/clock.ts";
import { useMediaQuery } from "~/lib/media.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { INBOX_PAGE, useInboxCounts } from "~/lib/useInboxCounts.ts";
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

/** The notices and the decisions are asked again on this beat: nothing pushes either. */
const INBOX_POLL_MS = 30_000;

export function Inbox() {
  const viewer = useViewer();
  const now = useNow();
  const org = useOrg();
  const nav = useNavigator();
  const index = useMemo(() => indexOrg(org), [org]);
  const zone = org?.timezone;
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

  const bound = viewer.handle !== "";
  const inbox = useQuery(
    "work_inbox",
    bound
      ? {
          handle: viewer.handle,
          limit: INBOX_PAGE,
          unread: scope === "unread",
          // `only` IS WHAT WAS PUT OFF, and nothing else.
          snoozed: scope === "snoozed" ? "only" : "exclude",
        }
      : undefined,
    { enabled: bound, pollMs: INBOX_POLL_MS },
  );
  usePageCoverage(inbox.data);
  const decisions = useQuery("decisions", undefined, {
    enabled: bound,
    pollMs: INBOX_POLL_MS,
  });
  // THE SNOOZE BOUND, so a preset the engine would refuse is never offered.
  const person = useQuery("work_person", bound ? { handle: viewer.handle } : undefined, {
    enabled: bound,
  });
  // THE BADGE'S FIGURE, read where the badge reads it — the frame — and never
  // asked again here: see "Two figures, two names" above.
  const waiting = useInboxCounts();

  const rows = useMemo(
    () =>
      inboxRows({
        scope,
        subjects: bound ? decisionSubjects(decisions.data) : [],
        notices: bound ? (inbox.data?.notices ?? []) : [],
      }),
    [scope, bound, decisions.data, inbox.data],
  );
  const counts = useMemo(() => chipCounts(rows), [rows]);
  const shownDecisions = rows.decisions.filter((r) => rowMatches(r, chip));
  const shownNotices = rows.notices.filter((r) => rowMatches(r, chip));
  const groups = dayGroups(shownNotices, now, zone);
  const visible: InboxRow[] = [...shownDecisions, ...groups.flatMap((g) => g.rows)];

  // THE OPEN ROW: the one the URL names, or — beside the list, never on a
  // phone where the pane replaces it — the first one, so the pane is never an
  // empty box beside a list with something in it.
  const named = visible.find((r) => r.key === open) ?? null;
  const selected = named ?? (phone ? null : (visible[0] ?? null));
  const openRow = useCallback((key: string) => nav.filter({ row: key }), [nav]);

  // j AND k STEP THROUGH THE LIST, opening each row in the pane.
  const at = selected ? visible.findIndex((r) => r.key === selected.key) : -1;
  useKeymap({
    "list.next": {
      run: () => {
        const next = visible[Math.min(visible.length - 1, at + 1)];
        if (next) openRow(next.key);
      },
      when: visible.length > 0,
    },
    "list.previous": {
      run: () => {
        const prev = visible[Math.max(0, at - 1)];
        if (prev) openRow(prev.key);
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
  });
  const showList = !phone || !named;
  const showPane = !phone || named !== null;

  return (
    <div className="inbox-frame" data-phone-pane={phone && named ? "true" : undefined}>
      <PageActions>
        <MarkAllRead through={bound ? readThrough(loaded) : null} />
      </PageActions>

      {/* THREE VIEWER STATES, three sentences — and only one of them is
          anybody's fault. The controls stay drawn for all three, each saying
          what it would need, which is why the screen is not simply locked. */}
      {viewer.anonymous ? (
        <Callout variant="warning" className="inbox-callout">
          No API token is presented, so this browser is nobody: a person&rsquo;s notices and
          decisions need a credential bound to their seat.
        </Callout>
      ) : viewer.unbound ? (
        <Callout variant="warning" className="inbox-callout">
          This token is <code className="inline">{viewer.tokenID}</code> and no seat claims it. Give
          a human seat <code className="inline">contact.crewlet_operator_id</code> with that value
          and this becomes their inbox.
        </Callout>
      ) : null}

      <div className="inbox-panes">
        {showList && (
          <NoticeList
            scope={scope}
            onScope={(next) => setScope(next)}
            unread={unreadOnPage}
            waiting={waiting}
            chip={chip}
            onChip={(next) => setChip(next)}
            counts={counts}
            decisions={shownDecisions}
            groups={groups}
            selected={selected?.key ?? ""}
            onOpen={openRow}
            index={index}
            now={now}
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
                  <QueryState error={inbox.error} loading={false}>
                    {null}
                  </QueryState>
                  <QueryState error={decisions.error} loading={false}>
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
            now={now}
            zone={zone}
            maxSnoozeAhead={person.data?.max_snooze_ahead}
            onBack={phone ? () => nav.filter({ row: null }) : undefined}
          />
        )}
      </div>
      {!bound && !viewer.loading && (
        <p className="t-caption inbox-nobody">
          With a credential bound to a seat, this screen also shows what reached that person and
          what waits on their decision.
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
