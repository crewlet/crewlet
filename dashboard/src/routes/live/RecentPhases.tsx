/**
 * Live › Now running › Recent phases — every model call the company finished,
 * newest first, one fixed-height row each.
 *
 * THE ONLY READER OF `phases`. It was a screen of its own, reached as a lens
 * on the turn list, where it drew a second "running now" table of the same
 * calls the running-turn rows above already draw — two answers to "what is
 * running", in two shapes, a hundred pixels apart. The running half is the
 * turn rows now, and this is the settled half: a finished phase never changes
 * again, so the list moves only when the reader asks it to (`useSettled`),
 * and a phase the reader watched running above lands here the moment it
 * completes rather than behind a "new rows" button.
 *
 * A row is a phase, and a phase is one leg of a TURN — so a plain click opens
 * that turn in the rail beside the list, and ⌘-click its page.
 *
 * # One seat, by its id
 *
 * The page is asked for with the seat's HANDLE (`seat=`), which the engine
 * resolves to the id every node derives for it, and the phases that finish
 * while the tab is open are matched on that same id. It was a role name, which
 * two unit seats stamped from one template share — so "this seat's phases"
 * was every such seat's.
 *
 * # Older is the engine's cursor
 *
 * "Load 60 older" asks `phases` again with the `next` the last page carried.
 * It used to page the event LIST and then read every row back one `event` at
 * a time, because the listing carries no payload: sixty-one round trips for
 * sixty rows, filtered by a role name the listing matched against the actor.
 */

import { useCallback, useMemo, useState } from "react";
import { Button, Card, EmptyState, EmptyValue, Skeleton, Tag } from "@crewlethq/ui";
import { BrainGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { CoverageNote } from "~/components/CoverageNote.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import type { GridColumn } from "~/app/frame/DataGrid.tsx";
import { DateCell, NumberCell, SeatLabel, TokenCell } from "~/app/frame/cells.tsx";
import { rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { href, useNavigator } from "~/app/router.tsx";
import { useClient, useEngineHealth, usePhaseEvents } from "~/lib/store-hooks.ts";
import { useSettled } from "~/lib/settled.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { eventHistoryLabel, plural } from "~/lib/format.ts";
import {
  decisionLabel,
  decisionTone,
  fromPhaseEvent,
  mergePhases,
  streamedPhases,
  type PhaseRecord,
} from "~/lib/phases.ts";
import { PhaseTag, uiletTone } from "~/ui/primitives.tsx";
import { queryFailure, type QueryFailure } from "~/protocol/index.ts";
import type { EventRecord, PhasesPage } from "~/protocol/index.ts";

/**
 * How many phases one page carries — the first and every "older" one.
 *
 * SIXTY: a phase row carries its whole payload (prompts, response, tool
 * calls), which is what makes it the heaviest page this screen asks for, and
 * sixty is two busy turns' worth on a large seat — enough to see what the
 * company just did without paying for an hour of prompts on every visit.
 */
export const PHASE_PAGE = 60;

const recordKey = (r: PhaseRecord) => r.key;

export function RecentPhases({
  seat,
  agentId,
  phase,
  failed,
  runningKeys,
  nameOf,
}: {
  /** The seat's handle, or "" for the company. */
  seat: string;
  /** That seat's derived id, which a streamed record is matched on. */
  agentId: string;
  /** One phase, or "" for every phase. */
  phase: string;
  /** Only the phases that failed. */
  failed: boolean;
  /** The phase keys the running-turn rows above are drawing right now. */
  runningKeys: readonly string[];
  /** A seat's name, from whatever a record names it by. */
  nameOf: (r: PhaseRecord) => string;
}) {
  const { socket } = useClient();
  const engine = useEngineHealth();
  const { open: openPeek } = usePeekControls();
  const nav = useNavigator();
  // `phases`, not `events?type=…`: the event listing deliberately never
  // selects the payload, and a phase record without one has no prompts, no
  // response, no tool calls and no decision.
  const first = useQuery("phases", { limit: PHASE_PAGE, ...(seat ? { seat } : {}) });
  // THE OLDER PAGES, keyed on the question they continue: a different seat
  // is a different list, and its pages must not be appended to this one.
  const [older, setOlder] = useState<{ for: string; pages: PhasesPage[] }>({ for: "", pages: [] });
  const [paging, setPaging] = useState(false);
  // THE WHOLE FAILURE, its refusal included — see [queryFailure].
  const [pageFailure, setPageFailure] = useState<QueryFailure | null>(null);
  const pages = older.for === seat ? older.pages : [];
  const last = pages.at(-1) ?? first.data;

  const phaseEvents = usePhaseEvents();
  const { records, answeredKeys } = useMemo(() => {
    const answered: EventRecord[] = [
      ...(first.data?.phases ?? []),
      ...pages.flatMap((p) => p.phases),
    ];
    const stored = answered
      .map((row) => fromPhaseEvent(row))
      .filter((r): r is PhaseRecord => r !== null);
    // THE PHASES THAT FINISH AFTER THE PAGE WAS ANSWERED, off the push —
    // matched on the seat's id, the same key the engine narrowed the page by.
    const streamed = streamedPhases(phaseEvents, (r) => !seat || r.agentId === agentId);
    return {
      records: mergePhases([...streamed, ...stored], []),
      answeredKeys: stored.map(recordKey),
    };
  }, [first.data, pages, phaseEvents, seat, agentId]);

  const shown = useMemo(
    () => records.filter((r) => (!phase || r.phase === phase) && (!failed || r.failed)),
    [records, phase, failed],
  );
  // THE SETTLED LIST DOES NOT SPLICE NEW ROWS IN UNDER A READER — see
  // lib/settled.ts. What IS let in at once is what the reader asked for or
  // was already watching: every row of a page the engine answered (the
  // first, which may land after a restored scroll position, and each older
  // one somebody pressed for), and the phases the running rows above were
  // drawing, which land here the moment they complete. Only a phase another
  // turn finished while the reader was down the list waits behind the button.
  const admitted = useMemo(() => [...runningKeys, ...answeredKeys], [runningKeys, answeredKeys]);
  const settled = useSettled(shown, recordKey, admitted);

  const loadOlder = useCallback(async () => {
    const next = last?.next;
    if (!next?.before_id) return;
    setPaging(true);
    setPageFailure(null);
    try {
      const page = await socket.query("phases", {
        limit: PHASE_PAGE,
        ...(seat ? { seat } : {}),
        before_time: next.before_time,
        before_id: next.before_id,
      });
      setOlder((prev) => ({ for: seat, pages: [...(prev.for === seat ? prev.pages : []), page] }));
    } catch (err) {
      setPageFailure(queryFailure(err));
    } finally {
      setPaging(false);
    }
  }, [socket, seat, last]);

  const openRow = useCallback(
    (r: PhaseRecord, e: React.MouseEvent | React.KeyboardEvent) => {
      // NO TURN TO PEEK: the row's own link is the way in, so the click is
      // left to the browser rather than opening an empty rail.
      if (!r.turnId) {
        if (!("button" in e)) nav.to(["live", "events", r.eventId]);
        return;
      }
      const go = () => openPeek({ kind: "turn", id: r.turnId });
      // THE GRID HANDS THIS BOTH EVENTS: the `enter` chord carries no button
      // and is never "open elsewhere"; a mouse click goes through the frame's
      // one copy of which clicks belong to the browser.
      if (!("button" in e)) {
        go();
        return;
      }
      rowPeekHandler(go)?.(e);
    },
    [openPeek, nav],
  );

  const columns = useMemo<GridColumn<PhaseRecord>[]>(
    () => [
      {
        key: "seat",
        header: "Seat",
        // THE SEAT'S BADGE, as the turn rows above and the Turns grid draw a
        // seat: a phase is always an agent's model call, so the kind is not a
        // question here.
        cell: (r) => {
          const name = nameOf(r);
          return name ? <SeatLabel name={name} kind="agent" /> : <EmptyValue label="No seat" />;
        },
        sortValue: (r) => nameOf(r),
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
          ) : r.decision ? (
            // THE WORD, WITH THE SENTENCE ON IT: a pill has a fixed line box,
            // and the gloss inside one would set this column's width for
            // every other row.
            <Tag
              variant={uiletTone(decisionTone(r.phase, r.decision))}
              title={decisionLabel(r.phase, r.decision)}
            >
              {r.decision}
            </Tag>
          ) : (
            // NOT "done": a settled phase with no decision is an onboarding
            // pass that did not mark itself onboarded.
            <EmptyValue label="This phase recorded no decision" />
          ),
        sortValue: (r) => (r.failed ? 0 : 1),
      },
      {
        key: "model",
        header: "Model",
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
        // TOOL ROUNDS, the phase's own `rounds_used` — one field, the same
        // quantity from the push and from the record.
        header: "Rounds",
        align: "right",
        shrink: true,
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
        header: "When",
        shrink: true,
        // THE CELL READS THE CLOCK ITSELF, so the columns hold still across a
        // tick and only the cells whose words moved are drawn again.
        cell: (r) => <DateCell at={r.at} />,
        sortValue: (r) => Date.parse(r.at) || 0,
      },
    ],
    [nameOf],
  );

  const exhausted = !!last && (last.exhausted || !last.next?.before_id);
  const filtering = !!(phase || failed);

  return (
    <Card padding="none" className="live-card">
      <Card.Header
        icon={<BrainGlyph size="sm" />}
        count={settled.items.length}
        subtitle="newest first · open a row for its turn"
      >
        <Card.Title as="h3">Recent phases</Card.Title>
      </Card.Header>
      <CoverageNote
        coverage={[first.data?.coverage, ...pages.map((p) => p.coverage)]}
        what="this list"
      />
      {settled.pending > 0 && (
        <button type="button" className="new-rows" onClick={settled.flush}>
          {plural(settled.pending, "new phase")} finished while you were reading — show
        </button>
      )}
      {first.loading && !first.data ? (
        <Skeleton variant="text" rows={5} rowHeight={44} label="Loading recent phases" />
      ) : first.error ? (
        <QueryState error={first.error} refusal={first.refusal} loading={false} />
      ) : settled.items.length === 0 ? (
        <EmptyState
          size="compact"
          icon={<BrainGlyph size={32} />}
          title={filtering ? "No phase matches these filters" : "No phase has finished yet"}
          description={
            filtering
              ? "Clear the phase and failure filters to see every phase this list holds."
              : "A phase is recorded when it completes. If every seat is idle and no schedule has fired, there is nothing here yet."
          }
        />
      ) : (
        <DataGrid
          name="phases"
          rows={settled.items}
          columns={columns}
          rowKey={recordKey}
          onRowActivate={openRow}
          // THE TURN, or — for a record the engine named no turn on — the
          // phase's own event, which is still a real page to land on.
          rowHref={(r) =>
            r.turnId ? href(["live", "turns", r.turnId]) : href(["live", "events", r.eventId])
          }
          isFailed={(r) => r.failed}
        />
      )}
      <div className="live-card-foot">
        {pageFailure ? (
          <QueryState error={pageFailure.error} refusal={pageFailure.refusal} loading={false} />
        ) : exhausted ? (
          <span className="t-caption">That is the beginning of the retained record.</span>
        ) : (
          <>
            <Button
              size="small"
              variant="secondary"
              onClick={() => void loadOlder()}
              disabled={paging || !last?.next?.before_id}
              loading={paging}
            >
              {`Load ${PHASE_PAGE} older`}
            </Button>
            <span className="t-caption">{eventHistoryLabel(engine?.event_history_seconds)}</span>
          </>
        )}
      </div>
    </Card>
  );
}
