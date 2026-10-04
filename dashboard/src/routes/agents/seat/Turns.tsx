/**
 * A seat's turns: the settled list, the transcript of each, and where its
 * tokens go.
 *
 * # One seat's, by its handle
 *
 * The list is `turns {seat}`. It was `turns {role}` — a parameter the engine
 * does not read — so this tab listed EVERY seat's turns under one seat's
 * name. The engine resolves a handle to the id the seat's events carry
 * (`seatParam`), which is also what tells two unit seats stamped from one
 * template apart, where a role name cannot.
 *
 * # The list is paged, and its count is what is loaded
 *
 * A page is fifty turns over the whole thirty days the event store keeps,
 * and "Load older" follows the engine's cursor back past it. The header
 * counts the rows loaded — "50+" while an older page exists — because the
 * read counts nothing: it was `.length` of the first page under "the newest
 * 50 this seat took", which a seat with twelve turns contradicted and a seat
 * with four hundred could never get past. It also asked for the read's
 * default WEEK while saying nothing of one.
 *
 * # The running turn is pushed, the settled ones are asked
 *
 * The transcript cards merge the seat's stored phase history with the phases
 * the socket streams and the live call the projection pushes, and they split
 * into "Running now" and the settled list: a running turn changes every
 * couple of hundred milliseconds, and letting that churn sit inside the
 * settled history reflowed whatever the reader was working through. "Running
 * now" LEADS the tab, so a turn in flight is on screen when it opens, and the
 * settled cards follow the table under their own heading ("Transcripts"),
 * because untitled they read as the table's rows repeated.
 */

import { useMemo, useRef } from "react";
import { BarList, Button, Card, EmptyState, Skeleton, Tag } from "@crewlethq/ui";
import { BrainGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { peekHref } from "~/app/frame/DetailRail.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import {
  DateCell,
  NumberCell,
  TokenCell,
  TurnWhatCell,
  UnsettledCell,
} from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import { TurnCard } from "~/components/TurnCard.tsx";
import { fmtCount, plural, tsKey } from "~/lib/format.ts";
import {
  attempts,
  fromLiveCall,
  fromPhaseEvent,
  groupTurns,
  mergePhases,
  streamedPhases,
  type PhaseRecord,
} from "~/lib/phases.ts";
import { useSettled } from "~/lib/settled.ts";
import { phaseColor, spentOnly } from "~/lib/spend.ts";
import { useClient, useConnection, usePhaseEvents } from "~/lib/store-hooks.ts";
import { MAX_TURN_DAYS, openState, runningNow, UNSETTLED } from "~/lib/turns.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import type { Seat } from "~/lib/seats.ts";
import type { AgentRow, EventRecord, TurnRow, TurnsAnswer } from "~/protocol/index.ts";

/** How many turns one page of the list holds — the read's own default. */
export const TURN_PAGE = 50;

const pickTurns = (a: TurnsAnswer) => a.turns ?? [];
const turnRowId = (t: TurnRow) => t.turn_id;
/** The turn list's cursor is `next`, on the turn's START — see `usePaged`. */
const turnsBefore = (a: TurnsAnswer) => a.next ?? "";

/** The window "Where its tokens go" sums: the week the Overview counts. */
export const SPEND_DAYS = 7;

const turnKey = (g: { turnId: string }) => g.turnId;

export function Turns({
  seat,
  agent,
  now,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  now: number;
}) {
  const handle = seat.handle;
  const phaseEvents = usePhaseEvents();
  // THE SEAT'S OWN LIVE ROW, the one overlay a running turn on this list can
  // be on.
  const running = useMemo(() => (agent ? [agent] : []), [agent]);
  const { connected } = useConnection();
  // The seat's AGENT ID, which is what a phase record names its seat by —
  // never the role NAME, which two seats may share. Empty for a seat no roster
  // row is held for, and the stream filter below reads it as "match no phase"
  // rather than "match every phase that named no seat".
  const agentId = agent?.agent_id ?? "";
  const { socket } = useClient();
  // EVERY DAY THE STORE KEEPS: named, because the read's default is a week,
  // and a seat's record is the whole of what can still be read.
  const asked = { seat: handle, days: MAX_TURN_DAYS, limit: TURN_PAGE };
  const list = usePaged(
    useQuery(
      "turns",
      asked,
      // TWENTY SECONDS, the cadence `routes/live/Turns.tsx` gives the same
      // question — a store aggregate with no push behind it.
      { enabled: handle !== "", pollMs: 20_000 },
    ),
    asked,
    (before) => socket.query("turns", withFloor("turns", JSON.stringify({ ...asked, before }))),
    pickTurns,
    turnRowId,
    turnsBefore,
  );
  // The seat's own phase history. Its `live` half is NOT read: the
  // projection pushes it onto the roster, and one fact with two sources on
  // one screen is two facts.
  const history = useQuery("agent", { id: handle }, { enabled: handle !== "" });
  const spend = useQuery(
    "tokens",
    { seat: handle, days: SPEND_DAYS },
    { enabled: handle !== "", pollMs: 60_000 },
  );

  const phases = useMemo<PhaseRecord[]>(() => {
    const stored = (history.data?.llm_history ?? [])
      .map((ev) => fromPhaseEvent(ev as EventRecord))
      .filter((r): r is PhaseRecord => r !== null);
    // The query is answered ONCE, at mount. Every phase that finishes after
    // it — every phase of the turn a reader opened this tab to watch —
    // reaches the tab only here.
    const streamed = streamedPhases(phaseEvents, (r) => agentId !== "" && r.agentId === agentId);
    const live = agent?.live_call ? [fromLiveCall(agent.live_call, agent.role, agent.turn)] : [];
    // Streamed FIRST so the query's own copy of the same phase wins the key.
    return mergePhases([...streamed, ...stored], live);
  }, [history.data, phaseEvents, agent, agentId]);
  const turns = useMemo(() => groupTurns(phases), [phases]);
  // WHICH OF THESE ARE THE SAME WORK: a turn id names one run, so a trigger
  // that failed without acting and came back is several cards.
  const attempt = useMemo(() => attempts(turns), [turns]);
  // THE ENGINE'S OWN ROW FOR EACH CARD, so a card and the list above it read
  // one turn's start and length off one record.
  const rows = useMemo(() => new Map(list.items.map((t) => [t.turn_id, t])), [list.items]);
  const liveTurns = useMemo(() => turns.filter((g) => g.live), [turns]);
  const doneTurns = useMemo(() => turns.filter((g) => !g.live), [turns]);
  const liveKeys = useMemo(() => liveTurns.map(turnKey), [liveTurns]);
  // The turns this reader has watched RUN. A card is remounted when it
  // crosses from the live region into the settled list, and its open state
  // goes with it — so the transcript a reader had open collapsed the moment
  // its last phase landed. Written during render, as `useSettled` does:
  // adding to a set is idempotent, so a discarded render leaves the same set.
  const watched = useRef<Set<string>>(new Set());
  for (const key of liveKeys) watched.current.add(key);
  const settled = useSettled(doneTurns, turnKey, liveKeys);

  return (
    <div className="col gap-4">
      {/* RUNNING NOW LEADS THE TAB, so a turn in flight is on screen when
          the tab opens: under the fifty-row list it was 2,000px down, and a
          reader who came to watch it saw only the history. */}
      {liveTurns.length > 0 && (
        <section className="col gap-2 live-region" aria-labelledby="prof-turns-live">
          <div className="col">
            <h3 id="prof-turns-live" className="prof-section-title">
              Running now
            </h3>
            <span className="t-caption">Updates as each round is written</span>
          </div>
          <div className="col gap-2">
            {liveTurns.map((g) => (
              <TurnCard
                key={g.turnId}
                group={g}
                row={rows.get(g.turnId)}
                attempt={attempt.get(g.turnId)}
                defaultOpen
              />
            ))}
          </div>
        </section>
      )}
      <Card padding="none">
        <Card.Header
          // THE WINDOW AND ITS TOTAL UNDER THE TITLE, as a chart's key is:
          // beside the title and the link, a phone cut all three.
          className="card-head-stacked"
          subtitle={
            spend.data
              ? `${fmtCount(spend.data.totals.total_tokens)} tokens over ${SPEND_DAYS} company days, every node`
              : undefined
          }
          actions={
            <a className="t-link" href={href(["live"], { seat: handle })}>
              All its model activity
            </a>
          }
        >
          <Card.Title as="h3">Where its tokens go</Card.Title>
        </Card.Header>
        <QueryState
          error={spend.error}
          refusal={spend.refusal}
          loading={spend.loading && !spend.data}
        >
          <div className="prof-spend">
            <section aria-label="By phase">
              <h4 className="t-label">By phase</h4>
              <BarList
                data={spentOnly(spend.data?.by_phase ?? []).map((p) => ({
                  id: p.phase,
                  label: p.phase,
                  value: p.total_tokens,
                  display: fmtCount(p.total_tokens),
                  color: phaseColor(p.phase),
                  sub: plural(p.calls, "call"),
                }))}
                emptyLabel="No calls in the window."
              />
            </section>
            <section aria-label="By model">
              <h4 className="t-label">By model</h4>
              <BarList
                data={spentOnly(spend.data?.by_model ?? []).map((m) => ({
                  id: m.model,
                  label: m.model,
                  value: m.total_tokens,
                  display: fmtCount(m.total_tokens),
                  sub: plural(m.calls, "call"),
                }))}
                emptyLabel="No calls in the window."
              />
            </section>
          </div>
        </QueryState>
      </Card>

      {/* THE SETTLED RECORD, ABOVE THE TRANSCRIPT: the paged, sortable list a
          reader finds last Tuesday's failed turn in, each row opening it in
          the rail. */}
      <Card padding="none">
        <Card.Header
          // WHAT IS LOADED, and a floor while more is: nothing here counts
          // the seat's whole record, so nothing here claims to.
          count={
            list.answers.length > 0 ? `${list.items.length}${list.more ? "+" : ""}` : undefined
          }
          subtitle={`Newest first, from every node, over the ${MAX_TURN_DAYS} days the event store keeps`}
        >
          <Card.Title as="h3">Turns</Card.Title>
        </Card.Header>
        <QueryState error={list.error} loading={list.loading && list.answers.length === 0}>
          <DataGrid
            name="prof-turns"
            rows={list.items}
            rowKey={(t) => t.turn_id}
            rowHref={(t) => peekHref({ kind: "turn", id: t.turn_id })}
            empty={{
              title: "No turns in the record for this seat",
              hint: `A turn is recorded when it starts; the store keeps ${MAX_TURN_DAYS} days of them.`,
            }}
            columns={[
              {
                key: "started",
                header: "Started",
                shrink: true,
                sortValue: (t) => tsKey(t.started_at),
                cell: (t) => <DateCell at={t.started_at} now={now} />,
              },
              {
                key: "summary",
                header: "What it did",
                sortValue: (t) => t.summary ?? "",
                // A TURN STILL RUNNING says what it is doing and what it is
                // on, from the push (`runningNow`) — the store's row for it
                // has neither yet.
                cell: (t) => {
                  const live = runningNow(t, running);
                  return (
                    <TurnWhatCell
                      summary={t.summary}
                      item={t.work_item ?? live?.item}
                      doing={live?.words}
                    />
                  );
                },
              },
              {
                key: "iterations",
                // SELF-ITERATE ROUNDS, and the word says so: a phase's TOOL
                // rounds are on its own card, under their own word.
                header: (
                  <span title="self-iterate rounds — the tool rounds each phase used are on the phase row">
                    Iterations
                  </span>
                ),
                label: "Iterations",
                shrink: true,
                sortValue: (t) => (t.complete ? t.iterations : null),
                cell: (t) => (t.complete ? <NumberCell value={t.iterations} /> : <UnsettledCell />),
              },
              {
                key: "tokens",
                header: "Tokens",
                shrink: true,
                sortValue: (t) => (t.complete ? t.total_tokens : null),
                cell: (t) =>
                  t.complete ? <TokenCell value={t.total_tokens} /> : <UnsettledCell />,
              },
              {
                key: "state",
                // NAMED, like every other column: an empty head over the one
                // column whose tags say what went wrong read as a gap.
                header: "State",
                shrink: true,
                // NOTHING AT ALL for a turn that ended cleanly — not an empty
                // wrapper: a cell with no child nodes is what a phone's card
                // drops (`.grid-cell:empty`), where an empty span left a
                // "STATE" label alone on a line of every settled turn.
                cell: (t) => {
                  // NO COMPLETION RECORD is running or not settled, read off
                  // the seat's own overlay exactly as the turn's page reads
                  // it ([openState]) — "running" for both disagreed with the
                  // page one click away.
                  const open = openState(t, running, connected);
                  return t.parked || open === "running" || open === "unsettled" || t.failed ? (
                    <span className="row gap-1">
                      {/* A RUNNING TURN IS NOT A ZERO-LENGTH ONE: `complete`
                          tells a turn in flight from one that ended, and a
                          turn waiting on its coding run is `parked`,
                          neither. */}
                      {t.parked && (
                        <Tag appearance="outline" title="waiting on a coding run it launched">
                          parked
                        </Tag>
                      )}
                      {open === "running" && (
                        <Tag variant="info" title="this seat is running the turn now">
                          running
                        </Tag>
                      )}
                      {open === "unsettled" && (
                        <Tag variant="warning" title={UNSETTLED.title}>
                          {UNSETTLED.word}
                        </Tag>
                      )}
                      {t.failed && <Tag variant="danger">failed</Tag>}
                    </span>
                  ) : null;
                },
              },
            ]}
          />
        </QueryState>
        {/* OLDER TURNS ARE FETCHED, from the cursor the engine answered —
            never reached by a wider page. */}
        {(list.more || list.paging || list.pageError) && (
          <footer className="panel-foot">
            {list.pageError ? (
              <QueryState error={list.pageError} loading={false} />
            ) : (
              <Button size="small" variant="secondary" onClick={list.older} disabled={list.paging}>
                {list.paging ? "Loading…" : "Load older turns"}
              </Button>
            )}
          </footer>
        )}
      </Card>

      {/* THE SAME TURNS AGAIN, AS TRANSCRIPTS — titled, because without a
          heading the cards read as the table above repeating itself. */}
      <section className="col gap-2" aria-labelledby="prof-transcripts">
        <div className="col">
          <h3 id="prof-transcripts" className="prof-section-title">
            Transcripts · newest first
          </h3>
          <span className="t-caption">Each settled turn's phases, as the engine recorded them</span>
        </div>
        {history.loading && !turns.length && (
          <Skeleton variant="text" rows={4} rowHeight={44} label="Loading this seat's turns" />
        )}
        {/* THE QUERY'S OWN STATE, BESIDE THE TURNS RATHER THAN IN PLACE OF
            THEM: only the settled half comes from it, and the running half is
            pushed — so a failed or slow read must not hide the turn happening
            right now. */}
        {history.error && (
          <QueryState error={history.error} refusal={history.refusal} loading={history.loading} />
        )}
        {!history.loading && !history.error && !turns.length && (
          <EmptyState
            size="compact"
            icon={<BrainGlyph size={32} />}
            title="No phases in the record for this seat"
            description="A phase is recorded when it completes. A seat that has not taken a turn has nothing here."
          />
        )}
        {settled.pending > 0 && (
          <button className="new-rows" onClick={settled.flush}>
            {plural(settled.pending, "new turn")} finished while you were reading — show
          </button>
        )}
        <div className="col gap-2">
          {settled.items.map((g, i) => (
            <TurnCard
              key={g.turnId}
              group={g}
              row={rows.get(g.turnId)}
              attempt={attempt.get(g.turnId)}
              defaultOpen={(i === 0 && !liveTurns.length) || watched.current.has(g.turnId)}
            />
          ))}
        </div>
      </section>
    </div>
  );
}
