/**
 * Model activity — a FLEET MONITOR, not a reader.
 *
 * This screen showed every running phase as an expanded transcript. With one
 * agent that was pleasant; with seven it was a race — seven seats each
 * republishing streamed prose five times a second, in cards that grew as they
 * wrote, so nothing on the page held still long enough to read. Scale is the
 * whole design constraint here and a transcript does not have it: reading what
 * a model actually said is a ONE-AGENT activity, and it belongs on that
 * agent's own page, where exactly one turn is in focus.
 *
 * So this is rows. One fixed-height row per phase, the same shape running or
 * finished, sorted on a key that does not move — a live row updates its cells
 * (rounds, tokens, elapsed) and NOTHING reflows, because a number changing
 * inside a fixed row cannot change the layout around it. That property is why
 * the table survives fifty seats when a list of cards did not survive seven.
 *
 * A row is a phase, and a phase is one leg of a TURN — so a plain click opens
 * that turn in the rail beside the table, where the comparison the reader came
 * for survives reading one of them. The turn's own page is what `Open ↗` and a
 * ⌘-click reach, and the full transcript is still a seat's page, which is
 * where a row the engine recorded no turn id for goes directly.
 */

import { useCallback, useMemo } from "react";
import { useParam } from "~/app/router.tsx";
import { PhasesDroppedNote, QueryState } from "~/components/common.tsx";
import {
  Button,
  EmptyState,
  EmptyValue,
  FilterChip,
  FilterChipGroup,
  Select,
  Skeleton,
  Tag,
} from "@crewlethq/ui";
import { CloseGlyph, NeurologyGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import type { GridColumn } from "~/app/frame/DataGrid.tsx";
import {
  useAgents,
  useClient,
  useEngineHealth,
  usePhaseEvents,
  usePhasesDroppedSince,
} from "~/lib/store-hooks.ts";
import { useOlderPages } from "~/lib/paging.ts";
import { useSettled } from "~/lib/settled.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { eventHistoryLabel, fmtElapsed, plural, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { href, useNavigator } from "~/app/router.tsx";
import {
  decisionLabel,
  decisionTone,
  fromLiveCall,
  fromPhaseEvent,
  mergePhases,
  streamedPhases,
  type PhaseRecord,
} from "~/lib/phases.ts";
import type { EventRecord } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { DateCell, NumberCell, TokenCell } from "~/app/frame/cells.tsx";
import { rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
// THE ONE PHASE MARK. This file carried a private copy of the variant table
// until `PhaseTag` was rebuilt on uilet's `Tag`; two tables spelling one
// vocabulary is how a phase renders the neutral pill on one screen and its
// own hue on the next, with nothing in the build to say so.
import { PhaseTag, uiletTone } from "~/ui/primitives.tsx";

/**
 * One page of phase records, the first and every older one alike.
 *
 * THE ENGINE'S OWN CEILING (`store.MaxPhasePage`), which it sizes on row weight:
 * a phase carries its prompts, its response and every tool result. Asking for
 * less would only spend more round trips on the same history.
 */
const PAGE = 60;

// Stable identity for the settled list. Named rather than inline so the
// hook's dependencies do not change identity on every render.
const phaseRecordKey = (r: PhaseRecord) => r.key;

/** Where the next page of phase records starts, as the engine echoes it. */
type PhaseCursor = { before_time?: string; before_id?: string };

/** A stored phase record, named by its event id — the walk's row key. */
const phaseKey = (row: EventRecord) => row.id;

/**
 * The cursor an answer offers, or null where it was the last page.
 *
 * BOTH OF THE ENGINE'S SIGNALS are read: `exhausted`, and a `next` with no
 * `before_id` — the engine sends an empty `next` on the page that ends the
 * record, and a half cursor is one it refuses.
 */
function cursorOf(
  page: { next?: PhaseCursor; exhausted?: boolean } | null | undefined,
): PhaseCursor | null {
  return page && !page.exhausted && page.next?.before_id ? page.next : null;
}

// Every phase the engine emits, the turn's own two first. A phase left off
// this row is one nobody can filter to, which on this screen means its cost
// is only ever visible inside the "all phases" total.
const PHASES = ["execute", "review", "onboarding", "subagent", "auxiliary", "judge"] as const;

export function ModelActivity() {
  const { socket } = useClient();
  const agents = useAgents();
  const { data: engine } = useEngineHealth();
  const [phase, setPhase] = useParam("phase", "");
  const [role, setRole] = useParam("role", "");
  const [onlyFailed, setOnlyFailed] = useParam("failed", "");

  // `phases`, not `events?type=…`. The event listing deliberately never
  // selects the payload — a page of ordinary events with every payload
  // attached is the query that makes an activity screen slow — and a phase
  // record without its payload has no prompts, no response, no tool calls and
  // no decision, which is everything this screen is for.
  const question = useMemo(() => ({ limit: PAGE, ...(role ? { role } : {}) }), [role]);

  // THE PAGES PAST THE FIRST, from the cursor each answer carries, on the
  // shared walk in `lib/paging.ts`. The question is the walk's identity, so a
  // new seat starts a new walk and a page still in flight for the old one
  // lands nowhere. Older pages kept across a change of seat put another seat's
  // phases under this one's name, and the next page resumed from the old
  // seat's cursor.
  const pager = useOlderPages<EventRecord, PhaseCursor>(phaseKey, question);
  const { data, loading, error, refetch } = useQuery("phases", question);
  // A NEW ARRAY PER ANSWER, an empty one included, because the drop mark
  // below is keyed on this array's identity.
  const firstPage = useMemo(() => data?.phases ?? [], [data]);
  const firstNext = cursorOf(data);
  const more = pager.more(firstNext);

  const phaseEvents = usePhaseEvents();
  // THE STREAMED HALF CAN LOSE A PHASE — see `usePhasesDroppedSince`. What is
  // gone is the stretch between the first page ON SCREEN and the oldest phase
  // the tab still holds, so the mark is keyed on that page: the frozen one
  // while older pages are held, which a reconnect's re-read does not replace.
  const phasesDropped = usePhasesDroppedSince(pager.head(firstPage));
  const backToNewest = useCallback(() => {
    pager.backToNewest();
    refetch();
  }, [pager.backToNewest, refetch]);

  const answered = useMemo(() => pager.rows(firstPage), [pager.rows, firstPage]);
  const stored = useMemo<PhaseRecord[]>(() => {
    const records = answered
      .map((row) => fromPhaseEvent(row))
      .filter((r): r is PhaseRecord => r !== null);
    // The query above is answered once. Without the phases that finish after
    // it, a row here leaves "Running now" when its phase completes and never
    // appears among the recent ones — it just goes.
    const streamed = streamedPhases(phaseEvents, (r) => !role || r.role === role);
    return [...streamed, ...records];
  }, [answered, phaseEvents, role]);

  const live = useMemo<PhaseRecord[]>(
    () =>
      agents
        .filter((a) => a.live_call)
        .map((a) => fromLiveCall(a.live_call!, a.role))
        .filter((r) => !role || r.role === role),
    [agents, role],
  );

  const merged = useMemo(() => mergePhases(stored, live), [stored, live]);

  const filtered = useMemo(
    () => merged.filter((r) => !phase || r.phase === phase).filter((r) => !onlyFailed || r.failed),
    [merged, phase, onlyFailed],
  );

  // RUNNING AND SETTLED ARE DIFFERENT LISTS, and that split is the whole
  // point of this layout. A live phase changes every couple of hundred
  // milliseconds; a finished one never changes again. As one list, the churn
  // of the first reflowed the second — so a reader working through a
  // completed transcript had it shoved down the page every time any seat
  // anywhere published a round.
  const running = useMemo(() => filtered.filter((r) => r.live), [filtered]);
  const done = useMemo(() => filtered.filter((r) => !r.live), [filtered]);
  const runningKeys = useMemo(() => running.map(phaseRecordKey), [running]);
  // The settled list does not splice new rows in under a reader — see
  // lib/settled.ts. A phase that was in the running table above is exempt: the
  // reader has been watching it, and it lands here the moment it completes.
  const settled = useSettled(done, phaseRecordKey, runningKeys);

  // THE SAME QUESTION THE FIRST PAGE ASKED, resumed from the cursor its own
  // answer carries. The older pages were an `events` listing followed by one
  // `event` read per row, because a feed row has no payload — sixty-one round
  // trips a page for what `phases` answers in one, payloads included.
  const loadOlder = () =>
    void pager.loadOlder(firstPage, firstNext, async (cursor) => {
      const page = await socket.query("phases", { ...question, ...cursor });
      return { rows: page.phases ?? [], next: cursorOf(page) };
    });

  const roles = useMemo(
    () => [...new Set(agents.map((a) => a.role).filter(Boolean))].sort(),
    [agents],
  );

  const nav = useNavigator();
  const now = useNow();

  const { open: openPeek } = usePeekControls();

  // A row goes to the seat, because that is where a transcript is readable:
  // one turn in focus instead of seven competing for the page. That is still
  // where a row with no turn on it goes — see [openRow].
  const openSeat = useCallback(
    (r: PhaseRecord) => nav.to(["company", "people", r.role], { tab: "turns" }),
    [nav],
  );

  /**
   * Where a row points, which is the TURN the phase ran in.
   *
   * A phase is not an object a reader can address — it is one leg of a turn,
   * and "which turn was this" is the question a row on this table raises. So a
   * plain click opens the turn beside the table rather than replacing it: this
   * screen is a monitor, and a reader comparing seven running phases loses the
   * comparison the moment the page navigates.
   *
   * A row with no turn id keeps the old destination. That is a live call the
   * engine published before the turn was recorded, and there is no turn to
   * peek — the seat's Turns tab is the honest second-best, not an empty rail.
   */
  const openRow = useCallback(
    (r: PhaseRecord, e: React.MouseEvent | React.KeyboardEvent) => {
      const go = () => (r.turnId ? openPeek({ kind: "turn", id: r.turnId }) : openSeat(r));
      // THE GRID HANDS THIS BOTH EVENTS. `rowPeekHandler` is the frame's one
      // copy of "which clicks mean elsewhere" and reads a mouse event — ⌘,
      // ctrl, shift, alt and the middle button belong to the browser, which
      // is what keeps the row a real link to what `rowLink` names. The
      // `enter` chord carries no button at all and is never "open elsewhere".
      if (!("button" in e)) {
        go();
        return;
      }
      rowPeekHandler(go)?.(e);
    },
    [openPeek, openSeat],
  );

  /** And the page that same row is a link TO, for every click that is not a
   *  plain one. The two must name the same object or ⌘-click lands somewhere
   *  the click would not have. */
  const rowLink = useCallback(
    (r: PhaseRecord) =>
      r.turnId
        ? href(["activity", "turns", r.turnId])
        : href(["company", "people", r.role], { tab: "turns" }),
    [],
  );

  // Defined here rather than at module scope because two cells need `now` to
  // render an elapsed time, and memoised so the table's own sort does not see
  // a new column set on every push.
  const columns = useMemo<GridColumn<PhaseRecord>[]>(
    () => [
      {
        key: "seat",
        header: "Seat",
        // NOT `SeatCell`: it is a link, and every row here is already one —
        // an anchor inside an anchor is markup no browser agrees about. The
        // live dot is the other half, and it is state the cell has no idea
        // about.
        cell: (r) => (
          <span className="row gap-2">
            {r.live && <span className="dot info" />}
            <span className="truncate">{r.role || <EmptyValue label="No seat" />}</span>
          </span>
        ),
        sortValue: (r) => r.role,
      },
      {
        key: "phase",
        header: "Phase",
        shrink: true,
        cell: (r) => <PhaseTag phase={r.phase} />,
        sortValue: (r) => r.phase,
      },
      {
        key: "outcome",
        header: "Outcome",
        cell: (r) =>
          r.failed ? (
            <Tag variant="danger">{r.errorKind || "failed"}</Tag>
          ) : r.live ? (
            <span className="t-caption">running</span>
          ) : r.decision ? (
            // A DECISION IS A STATE, and this column already draws one of them
            // as a toned pill one branch up. Rendered as caption text beside it,
            // `blocked` and `delivered` were the same grey — the phase card's
            // defect, one screen over.
            //
            // THE WORD, WITH THE SENTENCE ON IT. A pill has a fixed line box and
            // this column does not shrink, so "never said what it did — the
            // engine marked it incomplete" inside one would set the column's
            // width for every other row. The word is what the header promises
            // and what the seat's episodes grid already shows; the gloss stays a
            // hover away.
            <Tag
              variant={uiletTone(decisionTone(r.phase, r.decision))}
              title={decisionLabel(r.phase, r.decision)}
            >
              {r.decision}
            </Tag>
          ) : (
            // NOT "done". A settled phase with no decision is an onboarding pass
            // that did NOT mark itself onboarded — the runner writes an empty
            // decision and the note "did not mark (will retry next turn)" — so
            // the word this cell invented was the opposite of what the record
            // says, in the one column a reader trusts it.
            <EmptyValue label="This phase recorded no decision" />
          ),
        sortValue: (r) => (r.failed ? 0 : r.live ? 1 : 2),
      },
      {
        key: "model",
        header: "Model",
        // A MARKED ABSENCE, not a punctuation mark. A bare dash is read as
        // "dash" or skipped entirely, so the cell with the least to say said
        // nothing at all.
        cell: (r) =>
          r.model ? (
            <span className="mono t-caption truncate">{r.model}</span>
          ) : (
            <EmptyValue label="No model reported for this phase" />
          ),
        sortValue: (r) => r.model,
      },
      {
        key: "rounds",
        header: "Rounds",
        align: "right",
        shrink: true,
        // ONE FIELD. The two this compared were the same quantity in two bases
        // on a settled phase — `roundNum` was `rounds_used` there — so
        // `roundNum + 1` won every comparison and every finished row in this
        // column claimed one round more than the phase ran.
        cell: (r) => <NumberCell value={r.roundsUsed} />,
        sortValue: (r) => r.roundsUsed,
      },
      {
        key: "tokens",
        header: "Tokens",
        align: "right",
        shrink: true,
        cell: (r) => <TokenCell value={r.totalTokens} />,
        sortValue: (r) => r.totalTokens,
      },
      {
        key: "when",
        // NOT RIGHT-ALIGNED, though the two columns before it are. A number is
        // set right so a reader can compare magnitudes digit by digit down the
        // column; "2 m ago" and "14:07" are a phrase and a clock, and neither
        // is read from its last character. Set right the values also sat away
        // from their own header, which is set left like every other header in
        // this table.
        header: "When",
        shrink: true,
        // Elapsed while it runs, and when it landed once it has. Two
        // different questions, and a running phase has no "when" yet.
        //
        // Measured from startedAt, which never moves. Against `at` — which
        // advances on every published round, several times a second while a
        // round streams — the answer was always about zero, so a phase nine
        // rounds deep read "0 ms".
        //
        // The settled half is `DateCell`, which is the same relative time
        // this column already wrote plus the exact instant in its title — the
        // fact a reader wants once they have found the row. The running half
        // stays a stopwatch: `DurationCell` spells a MEASURED duration, and
        // running these two through one cell would make a phase still in
        // flight look like one that took that long.
        cell: (r) =>
          r.live ? (
            <span className="t-num">{fmtElapsed(now - tsKey(r.startedAt))}</span>
          ) : (
            <DateCell at={r.at} now={now} />
          ),
        sortValue: (r) => Date.parse(r.at) || 0,
      },
    ],
    [now],
  );

  const liveCount = live.filter((r) => r.live).length;
  const failedCount = merged.filter((r) => r.failed).length;
  const filtering = !!(role || phase || onlyFailed);

  return (
    <>
      <PageActions>
        {
          <>
            {liveCount > 0 && (
              <Tag variant="info" dot>
                {liveCount} running
              </Tag>
            )}
            {/* The count IS the control. It used to be a stat tile that said
                "4 failed" and did nothing, next to a separate chip that did
                the filtering — so a reader who saw the number had to go find
                the unrelated pill that acted on it. */}
            {failedCount > 0 && (
              <Tag
                variant="danger"
                onClick={() => setOnlyFailed(onlyFailed ? "" : "1")}
                pressed={!!onlyFailed}
                title={onlyFailed ? "show every phase" : "show only failed phases"}
              >
                {failedCount} failed
              </Tag>
            )}
            <Tag appearance="outline">{plural(filtered.length, "phase")} loaded</Tag>
          </>
        }
      </PageActions>
      <PageNote>
        Every phase the models ran, one row each. Open a row for the turn it ran in, beside the
        table: reading what a model said is a one-agent job, and this page has to stay readable with
        fifty of them running.
      </PageNote>

      {/* ONE row of controls. This screen had eighteen: a segmented control, a
          free-text box, a chip per seat, a chip per phase and a failures chip
          — fifteen of them near-identical pills in two different active
          idioms. The seat filter is a picker rather than a text box because
          the match is exact on both sides of the wire, so a typed prefix
          silently returned nothing while looking like a search that missed. */}
      <div className="toolbar">
        {/* THE EMPTY OPTION IS A REAL ROW rather than their `placeholder`:
            "any seat" is a value this filter has, and a placeholder is only the
            label over an unset one, with nothing to select back to. */}
        <Select
          // A PICKER IN A TOOLBAR, not a field on a form. uilet's Select is
          // `width: 100%` unless told otherwise, and its own doc says why that
          // is wrong here: "a filter row of full-width selects is one question
          // per line, which is not what a filter bar is".
          width="auto"
          value={role}
          onChange={(value) => setRole(String(value))}
          options={[
            { value: "", label: "Any seat" },
            ...roles.map((r) => ({ value: r, label: r })),
          ]}
          ariaLabel="Filter by seat"
          placeholder="Any seat"
          active={role !== ""}
        />
        <span className="spacer" />
        {/* THEIRS, AND IT IS BETTER THAN THE ROW OF TOGGLES IT REPLACES. These
            chips were always mutually exclusive — clicking one replaced the
            other — and they announced themselves as six independent pressed
            buttons across six tab stops. `semantics="radio"` says what they
            are, `allowNone` keeps the second click that clears the filter, and
            the group is ONE stop with arrows inside it.

            Arrow keys COMMIT here, unlike the segmented controls elsewhere in
            this port, and that is the right trade for this control rather than
            an inconsistency: the phase filter is applied in memory over rows
            already loaded and `useParam`'s `filter` kind REPLACES the history
            entry, so arrowing across all six costs no query and leaves nothing
            to press Back through. */}
        <FilterChipGroup
          label="Phase"
          hideLabel
          semantics="radio"
          allowNone
          value={phase}
          onValueChange={(next) => setPhase(next ?? "")}
        >
          {PHASES.map((p) => (
            <FilterChip key={p} value={p}>
              {p}
            </FilterChip>
          ))}
        </FilterChipGroup>
        {filtering && (
          <Button
            size="small"
            variant="secondary"
            leadingIcon={<CloseGlyph size="xs" />}
            onClick={() => {
              setRole("");
              setPhase("");
              setOnlyFailed("");
            }}
          >
            Clear
          </Button>
        )}
      </div>

      {loading && !merged.length && (
        <Skeleton variant="text" rows={5} rowHeight={44} label="Loading model activity" />
      )}
      {error && <QueryState error={error} loading={loading} />}
      {phasesDropped && <PhasesDroppedNote reread={backToNewest} />}

      {!loading && !filtered.length && !error && (
        <EmptyState
          icon={<NeurologyGlyph size={32} />}
          title={filtering ? "Nothing matches these filters" : "No model activity in the record"}
          description={
            filtering
              ? "Clear them to see every phase the engine has kept."
              : "A phase is recorded when it completes. If seats are idle and no schedule has fired, there is nothing here yet."
          }
        />
      )}

      {/* RUNNING. Fixed-height rows, sorted on the seat handle — a key that
          does not move — so a live row updates its cells and nothing around
          it reflows. Seven seats republishing five times a second turned the
          card list this replaces into a race; a number changing inside a row
          of settled height cannot move the page at all. */}
      {running.length > 0 && (
        <section className="col gap-1">
          <div className="t-label">
            Running now
            <span className="muted"> · {plural(running.length, "phase")} mid-flight</span>
          </div>
          <DataGrid
            rows={running}
            columns={columns}
            rowKey={phaseRecordKey}
            onRowActivate={openRow}
            rowHref={rowLink}
            isFailed={(r) => r.failed}
            defaultSort="seat"
          />
        </section>
      )}

      {/* SETTLED. A finished phase never changes again, so this list only
          moves when the reader asks it to. */}
      {settled.pending > 0 && (
        <button className="new-rows" onClick={settled.flush}>
          {plural(settled.pending, "new phase")} finished while you were reading — show
        </button>
      )}

      {settled.items.length > 0 && (
        <section className="col gap-1">
          <div className="t-label">
            Recent phases
            <span className="muted"> · newest first · open a row for its turn</span>
          </div>
          <DataGrid
            name="settled"
            rows={settled.items}
            columns={columns}
            rowKey={phaseRecordKey}
            onRowActivate={openRow}
            rowHref={rowLink}
            isFailed={(r) => r.failed}
          />
        </section>
      )}

      {/* A bare row, not a Panel: one button did not need card chrome. The
          spend rollup that used to sit BELOW this is gone — it was Spend's
          panel on Spend's data, and every "load older" click pushed it
          another sixty cards down a single scroller, so nobody ever reached
          it. A link goes where the screen does. */}
      <div className="row gap-2 wrap">
        {pager.error && <QueryState error={pager.error} loading={false} />}
        {more ? (
          <>
            <Button
              size="small"
              variant="secondary"
              onClick={loadOlder}
              disabled={pager.paging}
              loading={pager.paging}
            >
              {pager.paging ? "Loading…" : `Load ${PAGE} older phases`}
            </Button>
            <span className="t-caption">{eventHistoryLabel(engine?.event_history_seconds)}</span>
          </>
        ) : (
          data && <span className="t-caption">That is the beginning of the retained record.</span>
        )}
        {/* THE SNAPSHOT, said — see `lib/paging.ts`. Phases that finish now
            still arrive above it; the stored first page is what holds still. */}
        {pager.frozen && (
          <>
            <span className="t-caption">
              The first page is kept as it was read while older phases are shown.
            </span>
            <Button size="small" variant="secondary" onClick={backToNewest}>
              Back to the newest
            </Button>
          </>
        )}
        <span className="spacer" />
        <a className="t-link" href={href(["cost"])}>
          where the tokens go →
        </a>
      </div>
    </>
  );
}
