/**
 * "By agent": every seat that spent in the window — its ended turns, its
 * tokens, its share of the window, today's budget and its tokens per turn.
 *
 * THE BUDGET COLUMN IS TODAY'S, and says so in its heading: a seat's ceiling
 * is a calendar window of its own (ADR-0019), read from the live meter the
 * seat's overlay carries, and a thirty-day spend is never divided into one
 * day's allowance. `exhausted` is the engine's `refusing`, never a fill this
 * screen judged full; a seat with no daily ceiling says "No ceiling" rather
 * than drawing an empty bar or a bare dash. And where NO seat has one the
 * column is not drawn at all — a column of seven "No ceiling"s took the width
 * the names were cut for — and the caption says it once instead.
 *
 * A ROW OPENS THE SEAT BESIDE THE TABLE (the peek), and ⌘-click goes to its
 * page — the ranking stays on screen while one seat is read, which is the
 * comparison the table exists for.
 *
 * ON A PHONE A SEAT IS TWO LINES (`phoneRows="compact"`): its name, tokens and
 * share, then turns · today's budget · per turn, each figure carrying its own
 * unit word (`.spend-phone-unit`) since the compact row draws no labels. The
 * labelled card it replaced stood six lines per seat, and seven seats were a
 * thousand pixels of scrolling before the next card.
 */

import { Card, EmptyValue, Meter } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { NumberCell, TokenCell } from "~/app/frame/cells.tsx";
import { peekHref, peekRow, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { fmtCount, fmtExact, plural } from "~/lib/format.ts";
import { activityOf, ringOf, useSeatBadgeOf } from "~/lib/seats.ts";
import { useAgents } from "~/lib/store-hooks.ts";
import type { Window } from "~/lib/range.ts";
import { isRange, windowParam } from "~/lib/range.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { useMemo } from "react";
import type { AgentLine } from "./model.ts";

/**
 * Where "Recent turns by tokens" goes: the turn list, most tokens first, over
 * the same window where the turn list has it. The turn list reads the event
 * log, which holds thirty days, so it offers nothing longer — a quarter's
 * spend links to the last thirty days of turns, and a caption beside the link
 * says so. BESIDE, not in it: a link reading "last 30 days" under a card of
 * "last 90 days" looked like a mistake rather than a limit.
 */
export function turnsLink(w: Window): { path: string; clamped: string | null } {
  const clamp = isRange(w) && w === "90d";
  const window = isRange(w) ? (clamp ? "30d" : w) : windowParam(w);
  return {
    path: href(["live", "turns"], { sort: "-tokens", window }),
    clamped: clamp ? "the last 30 days — the turn list keeps no older" : null,
  };
}

export function ByAgent({
  lines,
  loading,
  error,
  words,
  range,
}: {
  lines: readonly AgentLine[];
  loading: boolean;
  error: QueryErrorCode | null;
  words: string | null;
  range: Window;
}) {
  const badgeOf = useSeatBadgeOf();
  const agents = useAgents();
  const { open } = usePeekControls();
  const rows = useMemo(() => [...lines], [lines]);
  // EACH SEAT'S LIVE ROW BY ITS AGENT ID, the key the spend row carries too:
  // a rename moves a handle, and the usage domain's row named the seat by the
  // one it had then.
  const byAgent = useMemo(() => new Map(agents.map((a) => [a.agent_id, a])), [agents]);
  const top = Math.max(0, ...lines.map((l) => l.share));
  const turns = turnsLink(range);
  // WHETHER ANY SEAT'S DAY IS CAPPED — the column exists only if so.
  const capped = lines.some((l) => l.day?.limit !== undefined);
  // `[` AND `]` WALK THE SEATS IN THE ORDER THE TABLE OPENS IN — most tokens
  // first, the order `agentLines` returns.
  usePeekNeighbours(
    useMemo(
      () => rows.map((l) => ({ kind: "seat" as const, id: l.row.handle || l.row.role })),
      [rows],
    ),
  );

  return (
    <Card className="spend-card spend-agents" padding="none">
      <div className="spend-card-head">
        <h2 className="spend-title">By agent</h2>
        <span className="spend-caption">
          {words ? `${words} · ` : ""}
          {capped
            ? "budget is today\u2019s per-seat allowance"
            : "no seat has a daily token ceiling"}
        </span>
      </div>
      <DataGrid
        name="agents"
        rows={loading && rows.length === 0 ? undefined : rows}
        rowKey={(l) => l.row.agent_id || l.row.role}
        defaultSort="-tokens"
        phoneRows="compact"
        flush
        rowHref={(l) => peekHref({ kind: "seat", id: l.row.handle || l.row.role })}
        onRowActivate={peekRow<AgentLine>((l) =>
          open({ kind: "seat", id: l.row.handle || l.row.role }),
        )}
        empty={
          error
            ? { title: "This window did not answer", hint: "The refusal above says why." }
            : {
                title: "No agent spent tokens in this window",
                hint: "A seat appears here once one of its turns makes a model call.",
              }
        }
        columns={[
          {
            key: "agent",
            header: "Agent",
            // THE ONE FLEXIBLE COLUMN, so every spare pixel goes to the name
            // (see Share). The floor is what the row keeps before Share gives
            // way — at 1280 every column still fits beside it.
            floor: "8rem",
            phoneLead: true,
            sortValue: (l) => badgeOf(l.row.handle || l.row.role).name,
            cell: (l) => {
              const badge = badgeOf(l.row.handle || l.row.role);
              const ring = ringOf(activityOf(byAgent.get(l.row.agent_id)));
              return (
                <span className="cell-seat" title={badge.name}>
                  <SeatAvatar
                    name={badge.name}
                    size="xs"
                    kind={badge.kind === "human" ? "human" : "agent"}
                    ring={ring}
                    decorative
                  />
                  <span className="truncate">{badge.name}</span>
                </span>
              );
            },
          },
          {
            key: "turns",
            header: "Turns",
            align: "right",
            // WIDTHS RATHER THAN `shrink`: a shrunk column is sized to its
            // cells, and at 1280 beside the list column the sortable heads
            // ("Tokens ⌄", "Per turn") were cut to "To…" over figures that fit.
            width: "4.5rem",
            sortValue: (l) => l.row.turns ?? -1,
            cell: (l) => (
              <>
                <NumberCell value={l.row.turns} />
                <PhoneUnit words={l.row.turns === 1 ? "turn" : "turns"} />
              </>
            ),
          },
          {
            key: "tokens",
            header: "Tokens",
            align: "right",
            width: "5.5rem",
            phoneLead: true,
            sortValue: (l) => l.row.total_tokens,
            cell: (l) => <TokenCell value={l.row.total_tokens} />,
          },
          {
            key: "share",
            header: "Share",
            // A FIXED TRACK, so the one flexible column is the name: a second
            // flexible column split the spare width with it, and at 1440 the
            // share bar stood in empty space while names were cut.
            width: "6rem",
            phoneLead: true,
            // THE SHARE GIVES WAY FIRST: it restates the tokens beside it as
            // a fraction, where today's budget is a fact nothing else on the
            // row says.
            drop: 1,
            sortValue: (l) => l.share,
            cell: (l) => (
              <span className="spend-share" title={`${fmtExact(l.row.total_tokens)} tokens`}>
                <span className="spend-share-track" aria-hidden="true">
                  <span
                    className="spend-share-bar"
                    style={{ width: `${top > 0 ? (l.share / top) * 100 : 0}%` }}
                  />
                </span>
                <span className="spend-share-pct">{Math.round(l.share * 100)}%</span>
              </span>
            ),
          },
          ...(capped
            ? [
                {
                  key: "today",
                  header: "Budget today",
                  width: "8rem",
                  drop: 2,
                  sortValue: (l: AgentLine) => (l.day?.limit ? l.day.used / l.day.limit : -1),
                  cell: (l: AgentLine) => (
                    <DayBudget line={l} name={badgeOf(l.row.handle || l.row.role).name} />
                  ),
                },
              ]
            : []),
          {
            key: "perturn",
            header: "Per turn",
            align: "right",
            width: "5.5rem",
            sortValue: (l) => l.perTurn ?? -1,
            cell: (l) =>
              l.perTurn === null ? (
                <EmptyValue label="No turn ended in this window" />
              ) : (
                <span
                  className="spend-per-turn"
                  title={`${fmtExact(l.perTurn)} tokens per ended turn`}
                >
                  {fmtCount(l.perTurn)}
                  <PhoneUnit words="per turn" />
                </span>
              ),
          },
        ]}
      />
      {/* PER-TURN SPEND IS THE TURN LIST'S. A company day holds no turn, so
          this screen has none to rank; the list does, over the event log. */}
      <div className="spend-card-foot">
        <a className="t-link" href={turns.path}>
          Recent turns by tokens →
        </a>
        {turns.clamped && <span className="spend-caption"> {turns.clamped}</span>}
      </div>
    </Card>
  );
}

/**
 * Today's budget for one seat: a meter in the engine's state, and its word.
 *
 * `name` is the seat's name AS THE ROW DRAWS IT, so a screen reader hears the
 * meter named for the same seat a sighted reader sees beside it — it was the
 * handle, "swe's daily token budget" beside "Agent SWE".
 */
function DayBudget({ line, name }: { line: AgentLine; name: string }) {
  const day = line.day;
  if (!day || day.limit === undefined) {
    // WORDS, NOT A DASH, at every width: beside seats whose day IS capped a
    // dash reads as a figure that failed to load, and the compact phone row
    // has no head to say what a dash would be the absence of.
    return (
      <span className="spend-muted" title="Nothing caps this seat's tokens today">
        No ceiling
      </span>
    );
  }
  const pct = day.limit > 0 ? Math.round((day.used / day.limit) * 100) : 0;
  return (
    <span className="spend-day">
      <Meter
        className="spend-day-meter"
        value={day.used}
        max={day.limit}
        state={day.state}
        size="compact"
        hideLabel
        label={`${name}'s daily token budget`}
        valueText={`${fmtExact(day.used)} of ${fmtExact(day.limit)} tokens today${
          day.state === "refusing" ? ", exhausted" : ""
        }`}
      />
      {day.state === "refusing" ? (
        <span className="spend-exhausted">
          exhausted
          <PhoneUnit words="today" />
        </span>
      ) : (
        <span
          className="spend-day-pct"
          title={`${plural(day.used, "token")} of ${fmtExact(day.limit)}`}
        >
          {pct}%
          <PhoneUnit words="of today's budget" />
        </span>
      )}
    </span>
  );
}

/**
 * A figure's unit, drawn only on a phone's compact row — where no column head
 * says what "12" is. Hidden at every width with columns, whose head says it.
 */
function PhoneUnit({ words }: { words: string }) {
  return <span className="spend-phone-unit"> {words}</span>;
}
