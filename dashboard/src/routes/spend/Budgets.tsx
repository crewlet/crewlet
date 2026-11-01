/**
 * Company › Budgets: every scope's day, week and month, what it has spent, the
 * ceiling it is held to, and whether the gate is refusing it — with each
 * ceiling editable in place by an admin.
 *
 * # Its own screen, because it answers a different question from spend
 *
 * Spend is "what has this cost". A budget is "what will be REFUSED", and the
 * two are read at different moments by different people: a founder reads the
 * first at the end of a month and the second the instant a seat stops
 * working.
 *
 * # The company first, then each person's assistant, then its seats
 *
 * Everything a seat spends is the company's spend too, so a company ceiling
 * binds every seat at once and is the one read first. The ceiling every
 * person's Dashboard Assistant is held to (`assistant.token_budget`) follows,
 * on the same tracks: it is what refuses a person's question, and the one
 * ceiling a person meets themselves. The seats follow as one table with a
 * column per window, because a column is how a reader finds the one seat near
 * its ceiling among seven.
 *
 * # Each ceiling is a section a refusal opens
 *
 * A refusal a ceiling caused names it (`lib/remedy.ts`), and its press opens
 * this screen at `section=company`, `section=assistant` or `section=seat:<handle>`:
 * the card, or the seat's row, is brought into view, focused, and marked as
 * the one the reader came for (`lib/arrival.ts`). A section this screen does
 * not draw for the reader, a seat it no longer lists, reveals nothing.
 *
 * ONE SET OF TRACKS FOR BOTH. The company's row is drawn on the seats' own
 * columns (`TRACKS`, handed to the table and to the row alike), so Today,
 * This week and This month stand over the same three columns in both cards:
 * as three tiles of their own they started at x positions no seat column
 * shared, and their grey ground drew a frame inside the card's frame. The
 * company row's first track, where a seat's name stands below, says once what
 * the windows are: every seat together, on the company clock, and when each
 * turns over.
 *
 * # The state is the engine's
 *
 * Each bar is handed the window's `state` (`ok`, `near`, `refusing`) and draws
 * that: the engine calls a window near at `near_fraction` of its ceiling and
 * refusing once a charge does not fit, and there is no threshold here to
 * disagree with it (`docs/reference/dashboard-design.md`, "A budget bar is
 * coloured by the engine").
 *
 * # Raising a ceiling, and why there is no reset
 *
 * A window's `used` IS what it spent — there is nothing to reset. Its room
 * comes back when it turns over on the company clock, or now by raising the
 * ceiling, which is a change to the company's configuration: checked against
 * the whole company first (a seat ceiling above the company's never refuses
 * a turn, and the check says so before it is saved), stored as a revision,
 * and applied by every node (`components/budgetWrite.tsx`). The editor is
 * drawn for every reader and disabled with the reason for one without
 * Admin.
 *
 * # The counter is the FLEET's, and unreadable is not zero
 *
 * `durable: false` means the coordination store could not be READ, which is
 * not the same as the counter being zero. Rendering it as zero would tell a
 * person their company has spent nothing when what happened is that nobody
 * could ask.
 */

import { useMemo, useRef, useState, type CSSProperties } from "react";
import {
  Callout,
  Card,
  EmptyValue,
  Meter,
  Skeleton,
  TableEntityCell,
  peekClickHandler,
} from "@crewlethq/ui";
import { BuildingComplexGlyph, DatabaseGlyph, KeyGlyph, UsersGlyph } from "@crewlethq/icons/glyphs";
import { ProductMark } from "@crewlethq/icons/product-mark";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { plainSeat } from "~/app/frame/cells.tsx";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import {
  CeilingEditor,
  CeilingPrefillContext,
  type CeilingPrefill,
} from "~/components/budgetWrite.tsx";
import { useAssistantContext } from "~/app/assistant/useAssistantContext.ts";
import type { PageActionSpec } from "~/app/assistant/context.ts";
import {
  ceilingPrefillArgs,
  parseCeilingPrefill,
  type CeilingProposal,
} from "./assistantActions.ts";
import { QueryState } from "~/components/common.tsx";
import { BUDGET_WINDOWS } from "~/contract/config.ts";
import { PERIOD_ADJECTIVE, windowOf } from "~/lib/budget.ts";
import { scopeName, whoseCeiling, type CeilingScope, type Period } from "~/lib/ceilings.ts";
import { useArrivedSection, useSectionArrival } from "~/lib/arrival.ts";
import { ASSISTANT_SECTION, COMPANY_SECTION, seatSection } from "~/lib/remedy.ts";
import { useNow } from "~/lib/clock.ts";
import { companyDateLabel, companyMonthLabel, fmtCount, fmtExact, relTime } from "~/lib/format.ts";
import { dayLabelIn } from "~/lib/range.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrgWriteAccess, useOrgWriteAccessFor } from "~/lib/useWriteAccess.ts";
import type { BudgetWindow, BudgetsAnswer } from "~/protocol/types.ts";

/** How often the counters are re-read: the gate's own report cadence is 15s,
 *  and a table read the instant a seat stops is worth one fresh answer a
 *  half-minute, not a poll that outruns what the fleet reports. */
const BUDGETS_POLL_MS = 30_000;

/** The column heading each window reads under. */
const HEADING: Readonly<Record<Period, string>> = {
  day: "Today",
  week: "This week",
  month: "This month",
};

/** A scope whose ceilings are a key of the company document, drawn as a row of tiles. */
type DocumentScope = Extract<CeilingScope, { kind: "company" | "assistant" }>;

const COMPANY: DocumentScope = { kind: "company" };
const ASSISTANT: DocumentScope = { kind: "assistant" };

/** The seat a `section=seat:<handle>` address names, or "" for none. */
function arrivedSeat(section: string): string {
  return section.startsWith(SEAT_SECTION_PREFIX) ? section.slice(SEAT_SECTION_PREFIX.length) : "";
}
const SEAT_SECTION_PREFIX = seatSection("");

/** The row a seats table marks as current, for an arrival at it. */
function currentRow(el: HTMLElement): HTMLElement | null {
  return el.querySelector<HTMLElement>('[aria-current="true"]');
}

/**
 * The four tracks the company's row and the seats' table share: the scope,
 * then one per window, equal. One value for both, so the company's windows
 * stand over the seats' columns by construction rather than by a number each
 * side measured.
 */
const TRACK = "minmax(0, 1fr)";
const TRACKS = [TRACK, TRACK, TRACK, TRACK].join(" ");

/** A window's state in words, for the reader who is told the bar rather than shown it. */
function stateWords(w: BudgetWindow): string {
  return w.state === "refusing"
    ? "refusing charges"
    : w.state === "near"
      ? "nearly spent"
      : "within the ceiling";
}

/**
 * The name a window's span carries: `Sep 29`, `Week 40`, `September`.
 *
 * ALL THREE FROM THE WINDOW'S LABEL, which the engine cut on the company's
 * clock: naming one reads no clock here, so a company zone this browser
 * cannot read names its windows as every other does (`companyMonthLabel`
 * says why the month's first instant would not).
 */
function windowName(w: BudgetWindow): string {
  if (w.period === "day") return companyDateLabel(w.window);
  if (w.period === "week") return `Week ${w.window.slice(-2).replace(/^0/, "")}`;
  return companyMonthLabel(w.window);
}

/**
 * "Last refused a call 12m ago" — the gate's own stamp, where it has one. LAST,
 * because the counter moves it to every refusal it records; "since" claimed
 * the window had been refusing for less time than it had.
 */
function RefusedLine({ w, now }: { w: BudgetWindow; now: number }) {
  if (w.state !== "refusing") return null;
  return (
    <span className="budget-refused t-caption">
      {w.refused_at
        ? `Last refused a call ${relTime(w.refused_at, now)}`
        : "No further charge fits"}
    </span>
  );
}

/**
 * One of a document-kept scope's three windows (the company's, or each
 * person's assistant): what it spent, the bar against its ceiling, the ceiling
 * (editable), and when it turns over.
 *
 * `counted` false draws NO FIGURE and no bar: the spend beside the ceiling is
 * a zero nobody measured (the assistant's, for a reader with no counter of
 * their own), and drawing it would tell them they had spent nothing.
 */
function ScopeWindow({
  scope,
  w,
  period,
  zone,
  now,
  onApplied,
  counted = true,
}: {
  scope: DocumentScope;
  w: BudgetWindow | undefined;
  period: Period;
  zone: string;
  now: number;
  onApplied: () => void;
  counted?: boolean;
}) {
  const capped = w?.limit !== undefined;
  const name = scopeName(scope);
  return (
    <section className="budget-tile" aria-label={`${name} ${HEADING[period].toLowerCase()}`}>
      <div className="budget-tile-head">
        <span className="budget-tile-title">{HEADING[period]}</span>
        {w && <span className="t-caption muted">{windowName(w)}</span>}
      </div>
      {w ? (
        <>
          {counted && (
            <div className="budget-tile-figure">
              <span className="budget-figure t-num">{fmtCount(w.used)}</span>
              <span className="t-caption muted">tokens</span>
            </div>
          )}
          {counted && capped ? (
            <Meter
              value={w.used}
              max={w.limit!}
              state={w.state}
              hideLabel
              label={`${upperFirst(whoseCeiling(scope))} ${PERIOD_ADJECTIVE[period]} token budget`}
              valueText={`${fmtExact(w.used)} of ${fmtExact(w.limit)} tokens, ${stateWords(w)}`}
            />
          ) : (
            // NO BAR WITHOUT A CEILING: a bar needs something to be a
            // fraction of, and an empty track reads as "nothing spent".
            <div className="budget-track-none" aria-hidden="true" />
          )}
          <div className="budget-tile-foot">
            <CeilingEditor
              scope={scope}
              period={period}
              window={w}
              zone={zone}
              onApplied={onApplied}
            />
          </div>
          {counted && <RefusedLine w={w} now={now} />}
        </>
      ) : (
        <EmptyValue label="Not stated" />
      )}
    </section>
  );
}

/** One seat's window in its column: spent, the ceiling beside it, the bar under. */
function SeatWindow({
  seat,
  name,
  w,
  period,
  zone,
  now,
  onApplied,
}: {
  seat: string;
  name: string;
  w: BudgetWindow | undefined;
  period: Period;
  zone: string;
  now: number;
  onApplied: () => void;
}) {
  if (!w) return <EmptyValue label="Not stated" />;
  const scope: CeilingScope = { kind: "seat", handle: seat, name };
  return (
    <div className="budget-cell">
      <div className="budget-cell-line">
        <span className="t-num budget-cell-used" title={`${fmtExact(w.used)} tokens`}>
          {fmtCount(w.used)}
        </span>
        <CeilingEditor scope={scope} period={period} window={w} zone={zone} onApplied={onApplied} />
      </div>
      {w.limit !== undefined && (
        <Meter
          size="compact"
          value={w.used}
          max={w.limit}
          state={w.state}
          hideLabel
          label={`${name}'s ${PERIOD_ADJECTIVE[period]} token budget`}
          valueText={`${fmtExact(w.used)} of ${fmtExact(w.limit)} tokens, ${stateWords(w)}`}
        />
      )}
      <RefusedLine w={w} now={now} />
    </div>
  );
}

/** When each window turns over, once for every scope: they share the company clock. */
function resetLine(answer: BudgetsAnswer): string {
  const zone = answer.timezone;
  const parts = BUDGET_WINDOWS.flatMap(({ period }) => {
    const w = windowOf(answer.org.windows, period);
    if (!w) return [];
    const at = Date.parse(w.resets_at);
    if (!Number.isFinite(at)) return [];
    return period === "day"
      ? ["today at midnight"]
      : [`the ${period} on ${companyDateLabel(dayLabelIn(at, zone))}`];
  });
  return parts.length ? `Resets ${parts.join(", ")}.` : "";
}

/** What the company row's first track says about every window, once. */
function companyLine(answer: BudgetsAnswer): string {
  const zone = answer.timezone || "UTC";
  return `Every seat together, on the company clock (${zone}). ${resetLine(answer)}`.trim();
}

/** What the assistant row's first track says: whose spend each ceiling holds, and whose is drawn. */
function assistantLine(answer: BudgetsAnswer): string {
  return answer.assistant.counted
    ? "Every person's questions to Ask Crewlet, each person on their own. The figures are yours."
    : "Every person's questions to Ask Crewlet, each person on their own.";
}

/** "the company's" as a sentence starts it. */
function upperFirst(text: string): string {
  return text.charAt(0).toUpperCase() + text.slice(1);
}

export function Budgets() {
  const seatBadge = useSeatBadgeOf();
  const budgets = useQuery("budgets", undefined, { pollMs: BUDGETS_POLL_MS });
  // ANYWHERE: a lead may raise the ceilings of the seats they lead, so the
  // one blanket reason below is only for a reader who can change none.
  const access = useOrgWriteAccess("anywhere");
  const now = useNow();
  const { open: openPeek } = usePeekControls();
  const answer = budgets.data;
  const zone = answer?.timezone ?? "";
  const refetch = budgets.refetch;

  // THE SECTION A REFUSAL'S REMEDY OPENED, marked and brought into view.
  const company = useSectionArrival(COMPANY_SECTION);
  const assistant = useSectionArrival(ASSISTANT_SECTION);
  const section = useArrivedSection();
  const seat = arrivedSeat(section);
  const seatRow = useSectionArrival(seat === "" ? "" : section, currentRow);

  // SORTED THE WAY THE TABLE OPENS — the grid applies `defaultSort` to whatever
  // it is handed, so seats published in the answer's own order would have `]`
  // walking a sequence nobody was shown.
  const seats = useMemo(
    () =>
      (answer?.seats ?? [])
        .slice()
        .sort(
          (a, b) =>
            (windowOf(b.windows, "month")?.used ?? 0) - (windowOf(a.windows, "month")?.used ?? 0),
        ),
    [answer],
  );
  usePeekNeighbours(
    useMemo(() => seats.map((s) => ({ kind: "seat" as const, id: s.handle || s.role })), [seats]),
  );

  // --- assistant page actions (WP12) ---
  // A CEILING PREPARED FOR ITS EDITOR: the field of that scope's window opens
  // with the value in it, and the warning, the save and its step-up are the
  // person's. The gate is the one each pencil is drawn by.
  const accessFor = useOrgWriteAccessFor();
  const [prefill, setPrefill] = useState<CeilingPrefill | null>(null);
  const nonce = useRef(0);
  const handles = useMemo(() => new Set(seats.map((s) => s.handle).filter(Boolean)), [seats]);
  const limitOf = (p: CeilingProposal) =>
    windowOf(
      p.scope === "company"
        ? answer?.org.windows
        : seats.find((s) => s.handle === p.scope)?.windows,
      p.period,
    )?.limit;
  const ceilingWords = (limit: number | null | undefined) =>
    limit === null || limit === undefined ? "No ceiling" : `${fmtExact(limit)} tokens`;
  useAssistantContext({
    actions: {
      "budgets.ceiling.prefill": {
        scope: "A ceiling on this screen, filled in for you to save (not saved)",
        args: ceilingPrefillArgs(handles),
        // NOTHING BEYOND THE SCREEN: the parser checks the seat and the window
        // against the screen as it is when the person approves, and opening an
        // editor is not a change to what another editor or a peek holds.
        tracks: [],
        parse: (raw) => parseCeilingPrefill(raw, handles),
        preview: (p) => [
          {
            field: `${p.scope === "company" ? "The company" : seatBadge(seats.find((s) => s.handle === p.scope)?.role ?? p.scope).name}, ${PERIOD_ADJECTIVE[p.period]} ceiling`,
            next: ceilingWords(p.value),
            current: ceilingWords(limitOf(p)),
          },
        ],
        blocked: (p) => {
          const a = accessFor(p.scope === "company" ? "company" : { seat: p.scope });
          return a.can ? null : a.reason;
        },
        apply: (p) => {
          nonce.current += 1;
          setPrefill({ scope: p.scope, period: p.period, text: p.text, nonce: nonce.current });
        },
      } satisfies PageActionSpec<CeilingProposal>,
    },
  });
  // --- end assistant page actions (WP12) ---

  const seatWindow = (s: BudgetsAnswer["seats"][number], period: Period) => (
    <SeatWindow
      seat={s.handle}
      name={seatBadge(s.role).name}
      w={windowOf(s.windows, period)}
      period={period}
      zone={zone}
      now={now}
      onApplied={refetch}
    />
  );

  return (
    <>
      {/* THE REASON, ONCE, where the pencils are all disabled for the same
          one: every control still says it on its own, but a reader scanning
          a table of dimmed pencils should not have to hover one to learn
          why. The notice every admin page draws for a reader it holds, not a
          loose caption under the page's own. Not while the viewer is still
          being asked. */}
      {!access.can && access.block !== "loading" && (
        <Callout variant="neutral" icon={<KeyGlyph size="md" />}>
          {access.reason}
        </Callout>
      )}

      {budgets.loading && !answer && (
        <Skeleton variant="text" rows={4} label="Loading the budget counters" />
      )}
      {!answer && (
        <QueryState error={budgets.error} refusal={budgets.refusal} loading={budgets.loading} />
      )}

      {answer && answer.durable === false && (
        <Callout variant="neutral" icon={<DatabaseGlyph size="md" />}>
          Couldn’t read current spend. Try again shortly.
        </Callout>
      )}

      {answer && answer.durable && (
        <CeilingPrefillContext.Provider value={prefill}>
          <div className="budgets">
            <div ref={company.ref} tabIndex={-1}>
              <Card as="section" padding="none" selected={company.arrived}>
                <Card.Header icon={<BuildingComplexGlyph size="sm" />}>
                  <Card.Title>The company</Card.Title>
                </Card.Header>
                <div className="budget-row" style={{ "--budget-tracks": TRACKS } as CSSProperties}>
                  <p className="budget-row-scope t-caption muted">{companyLine(answer)}</p>
                  {BUDGET_WINDOWS.map(({ period }) => (
                    <ScopeWindow
                      key={period}
                      scope={COMPANY}
                      period={period}
                      w={windowOf(answer.org.windows, period)}
                      zone={zone}
                      now={now}
                      onApplied={refetch}
                    />
                  ))}
                </div>
              </Card>
            </div>

            <div ref={assistant.ref} tabIndex={-1}>
              <Card as="section" padding="none" selected={assistant.arrived}>
                <Card.Header icon={<ProductMark product="assistant" size="sm" />}>
                  <Card.Title>Ask Crewlet, per person</Card.Title>
                </Card.Header>
                <div className="budget-row" style={{ "--budget-tracks": TRACKS } as CSSProperties}>
                  <p className="budget-row-scope t-caption muted">{assistantLine(answer)}</p>
                  {BUDGET_WINDOWS.map(({ period }) => (
                    <ScopeWindow
                      key={period}
                      scope={ASSISTANT}
                      period={period}
                      w={windowOf(answer.assistant.windows, period)}
                      zone={zone}
                      now={now}
                      onApplied={refetch}
                      counted={answer.assistant.counted}
                    />
                  ))}
                </div>
              </Card>
            </div>

            <div ref={seatRow.ref}>
              <Card as="section" padding="none">
                {/* NO SUBTITLE: when the windows turn over is the company row's,
                said once above the columns it is about. */}
                <Card.Header icon={<UsersGlyph size="sm" />}>
                  <Card.Title>Agent seats</Card.Title>
                </Card.Header>
                <DataGrid
                  rows={seats}
                  rowKey={(s) => s.agent_id || s.role}
                  // THE SEAT A REMEDY OPENED is the current row, and the one
                  // brought into view.
                  isSelected={(s) => seat !== "" && s.handle === seat}
                  defaultSort="-month"
                  // THE CARD IS THE FRAME: unflushed, the table drew a bordered
                  // box of its own inside the card's border.
                  flush
                  // THE SEAT BESIDE ITS BUDGET: the rail answers "what is it, what
                  // was it doing" without losing the row that raised it.
                  rowHref={(s) => peekHref({ kind: "seat", id: s.handle || s.role })}
                  onRowActivate={(s, e) => {
                    const go = () => openPeek({ kind: "seat", id: s.handle || s.role });
                    if (!("button" in e)) {
                      go();
                      return;
                    }
                    peekClickHandler(go)?.(e);
                  }}
                  empty={{ title: "No agent seats to count" }}
                  columns={[
                    {
                      key: "seat",
                      header: "Seat",
                      width: TRACK,
                      sortValue: (s) => s.role,
                      cell: (s) => <TableEntityCell {...plainSeat({ ...seatBadge(s.role) })} />,
                    },
                    // ONE COLUMN PER WINDOW, spelled out: each heading is a word
                    // the phone's card layout reads beside its value.
                    {
                      key: "day",
                      header: "Today",
                      width: TRACK,
                      sortValue: (s) => windowOf(s.windows, "day")?.used ?? 0,
                      cell: (s) => seatWindow(s, "day"),
                    },
                    {
                      key: "week",
                      header: "This week",
                      width: TRACK,
                      sortValue: (s) => windowOf(s.windows, "week")?.used ?? 0,
                      cell: (s) => seatWindow(s, "week"),
                    },
                    {
                      key: "month",
                      header: "This month",
                      width: TRACK,
                      sortValue: (s) => windowOf(s.windows, "month")?.used ?? 0,
                      cell: (s) => seatWindow(s, "month"),
                    },
                  ]}
                />
              </Card>
            </div>
          </div>
        </CeilingPrefillContext.Provider>
      )}
    </>
  );
}
