/**
 * Agents › Roster: everyone in the company, and what each of them is doing.
 *
 * The list is grouped by RUN STATE by default rather than by unit, because the
 * question this screen is opened with is "who is working / who stopped", and a
 * unit grouping buries a broken seat among healthy colleagues. Grouping by
 * unit is one control away for when the question is the org instead.
 *
 * Sorting is by NAME within a group, never by a live timestamp. The screen this
 * replaces sorted on `sandbox.updated_at || agent.updated_at` — a field that
 * moves on every push — so rows physically re-ordered under the reader's
 * cursor several times a second while a turn ran.
 *
 * A SEAT THE ENGINE STILL REPORTS AND THE CHART NO LONGER HOLDS is its own
 * group, "Removed from the company": a revision that drops a seat does not
 * stop the turn it was on, and a roster that hid that seat would hide a turn
 * still spending tokens. It has no page to link to, so its card is not a link.
 */

import { useMemo } from "react";
import { useNow } from "~/lib/clock.ts";
import { fmtMinutes } from "~/lib/work.ts";
import { plural } from "~/lib/format.ts";
import { href, useParam } from "~/app/router.tsx";
import { SeatCard, Section } from "~/components/common.tsx";
import { Card, cx, EmptyState, EmptyValue, Tag } from "@crewlethq/ui";
import { UsersGlyph } from "@crewlethq/icons/glyphs";
// OURS, AND DELIBERATELY. `SegmentedControl` welds keyboard ACTIVATION to its
// `semantics`: `radio` commits the option the arrows land on. Both strips here
// drive a `useTab`, which pushes a history entry — and the view strip swaps the
// screen for a table that fires `work_workload`. See the report.
import { Segmented } from "~/ui/primitives.tsx";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { QueryState } from "~/components/common.tsx";
import {
  activityOf,
  handleLabel,
  indexOrg,
  nameOfIn,
  stateLine,
  unitDirectLabel,
  type NameOf,
  type Seat,
} from "~/lib/seats.ts";
import { loadFraction, loadRows, loadSentence, loadTone, type Load } from "~/lib/workload.ts";
import type { AgentRow } from "~/protocol/index.ts";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { AgentsHeader, useAgentsCounts } from "./header.tsx";

const VIEWS = ["seats", "workload"] as const;

const GROUPINGS = ["state", "unit", "flat"] as const;
type Grouping = (typeof GROUPINGS)[number];

/**
 * Who is carrying how much.
 *
 * ONE ENGINE READ. Grouped per project a screen paid one round trip each and
 * rewrote the arithmetic every time. See `work_workload`.
 *
 * The JUDGEMENT is `lib/workload.ts` and is pure: how heavy a queue is against
 * the rest of the company, and which seats are holding work they can move none
 * of. The bar is RELATIVE to the heaviest queue on screen, because there is no
 * absolute number that is "full".
 */
function Workload({ seats, needle }: { seats: Seat[]; needle: string }) {
  const answer = useQuery("work_workload", undefined, { pollMs: 60_000 });
  // THE BAR'S "FIND A SEAT" NARROWS THIS VIEW TOO, by the fields a load row
  // has: who it is and where they sit. The bar stays the same field whichever
  // view is open, so it has to mean something on both.
  const rows = useMemo(
    () =>
      loadRows(answer.data?.rows ?? [], seats).filter(
        (load) =>
          !needle ||
          [load.seat?.name, load.handle, load.seat?.unit?.name].some((f) =>
            f?.toLowerCase().includes(needle),
          ),
      ),
    [answer.data?.rows, seats, needle],
  );
  // THE HEAVIEST QUEUE ON SCREEN is the bar's own scale — computed once here
  // rather than per row, so every track on the table is drawn against the
  // same number.
  const heaviest = useMemo(() => rows.reduce((n, load) => Math.max(n, load.open), 0), [rows]);
  return (
    <QueryState
      error={answer.error}
      loading={answer.loading}
      empty={
        rows.length
          ? undefined
          : needle
            ? {
                title: "Nobody matching holds any work",
                hint: "Find a seat matches a person's name, handle or unit.",
              }
            : {
                title: "Nobody holds any work",
                hint: "No open task on this node's copy of the tracker is assigned to anybody.",
              }
      }
    >
      <Card padding="none">
        <div className="wl">
          <div className="wl-head">
            <span>Seat</span>
            <span>Open</span>
            <span>Load</span>
            <span className="wl-num">Points</span>
            <span className="wl-num">Estimate</span>
          </div>
          {rows.map((load) => (
            <LoadRow key={load.handle} load={load} heaviest={heaviest} />
          ))}
        </div>
      </Card>
      {answer.data?.truncated && (
        <PageNote>
          More people hold work than this answer names. It stops at a cap rather than scanning a
          company without one.
        </PageNote>
      )}
    </QueryState>
  );
}

/** One person's row. */
function LoadRow({ load, heaviest }: { load: Load; heaviest: number }) {
  const { open: openPeek } = usePeekControls();
  const tone = loadTone(load);
  // THE BAR IS RELATIVE TO THE HEAVIEST QUEUE ON SCREEN, so the table reads as
  // a comparison between people rather than against a ceiling nobody set.
  const fraction = loadFraction(load, heaviest);
  const width = fraction === null ? 0 : Math.round(fraction * 100);
  return (
    <div className="wl-row" title={loadSentence(load)}>
      {/* A DEAD LINK UNTIL NOW: this hand-built `#/company/seats/…` named a
          segment Company routes nothing under, so every name in this table
          landed on "there is no such screen". A seat's page is
          `#/agents/seats/{handle}`, and `href` is what spells it — the one
          place the route and its encoding are written down. A plain click
          peeks instead, which on this table is the whole point: the question
          is who is carrying the most, and answering it should not cost the row
          the reader was comparing against. */}
      <a
        className="wl-who"
        href={href(["agents", "seats", load.handle])}
        onClick={rowPeekHandler(() => openPeek({ kind: "seat", id: load.handle }))}
      >
        <span className="truncate">{load.seat?.name ?? load.handle}</span>
        <span className="wl-handle mono">{load.handle}</span>
      </a>
      <span className="wl-counts">
        <span className="wl-open">{load.open}</span>
        {load.blocked > 0 && (
          <Tag variant="danger" appearance="outline">
            {load.blocked} blocked
          </Tag>
        )}
        {load.overdue > 0 && (
          <Tag variant="warning" appearance="outline">
            {load.overdue} overdue
          </Tag>
        )}
        {load.unscheduled > 0 && <span className="wl-soft">{load.unscheduled} undated</span>}
      </span>
      <span className="wl-track">
        {/* NOTHING AT ALL WHEN NOBODY HOLDS ANYTHING. The track is a
            comparison against the heaviest queue on screen, and with no queue
            anywhere there is nothing to compare — a bar of some arbitrary
            length would be a proportion of a number that does not exist. */}
        {fraction !== null && (
          <span className={cx("wl-bar", `is-${tone}`)} style={{ width: `${width}%` }} />
        )}
      </span>
      <span className={cx("wl-num", load.points === 0 && "wl-soft")}>
        {load.points === 0 ? <EmptyValue label="Unsized" /> : load.points}
      </span>
      <span className={cx("wl-num", load.estimateMin === 0 && "wl-soft")}>
        {load.estimateMin === 0 ? <EmptyValue label="Unsized" /> : fmtMinutes(load.estimateMin)}
      </span>
    </div>
  );
}

const STATE_ORDER = [
  { key: "needs", label: "Waiting on a person" },
  { key: "broken", label: "Stopped" },
  { key: "working", label: "Working" },
  { key: "idle", label: "Idle" },
  // NO ROW FROM THE ENGINE YET — never "not running here": whether a seat is
  // held anywhere in the fleet is the engine's `stopped`/`unplaced` to say,
  // and a seat a peer runs is idle or working like any other.
  { key: "offline", label: "No state from the engine yet" },
  { key: "human", label: "Human teammates" },
  // THE ENGINE STILL REPORTS IT and the chart no longer holds it — see the
  // file's doc.
  { key: "removed", label: "Removed from the company" },
] as const;

/** One row of the roster: a seat of the chart, or a seat the chart dropped. */
interface Row {
  key: string;
  name: string;
  /** Null for a seat the engine still reports and the chart no longer holds. */
  seat: Seat | null;
  agent: AgentRow | undefined;
}

function bucketOf(seat: Seat | null, agent: AgentRow | undefined): string {
  if (!seat) return "removed";
  // A KIND IS NOT A STATE, and this short-circuit is why a human teammate
  // could never be reported as waiting on anybody: every human seat was
  // filed under one label before anything about what it is doing was read.
  // Grouping by state now answers the same question for both kinds, and
  // "which seats are people" is a filter rather than a bucket.
  //
  // THE ENGINE'S WORD, bucketed. A failed last turn is not a stop, so a seat
  // whose last turn failed sits with the idle ones; its card says why.
  switch (activityOf(agent)) {
    case "needs":
      return "needs";
    case "stopped":
      return "broken";
    case "working":
      return "working";
    case "idle":
      return "idle";
  }
  // A human seat is never run by a node, so "not running here" would be a
  // fault report about something that is working exactly as designed.
  return seat.kind === "human" ? "human" : "offline";
}

export function People() {
  const agents = useAgents();
  const org = useOrg();
  const [view, setView] = useTab("view", VIEWS);
  // A GROUPING IS A FILTER, not a section, which is `tabs.ts`'s own
  // distinction: a section is the page you are on and takes the number keys,
  // a filter narrows what is on it and leaves them alone. Declared as a
  // section it bound the digits a SECOND time on this one screen —
  // each `useKeymap` installs one window listener and each returns
  // after its own first match, so there is no precedence and both fired:
  // `1` set the view AND regrouped, `2` set the view AND regrouped, and `3`,
  // which is past the end of the two views, moved the grouping alone. It also
  // put every regroup in the history, so Back walked groupings instead of
  // leaving the screen.
  const [group, setGroup] = useTab("group", GROUPINGS, "filter");
  const [q, setQ] = useParam("q", "");

  const index = useMemo(() => indexOrg(org), [org]);
  const nameOf = useMemo(() => nameOfIn(index), [index]);
  useAgentsCounts(index);

  // HOISTED OUT OF THE MEMO, because two things need to know whether a filter
  // is on: the filter itself, and every count on the screen that is now over
  // what matched rather than over the company.
  const needle = q.trim().toLowerCase();

  const rows = useMemo<Row[]>(() => {
    const matches = (...fields: (string | undefined)[]) =>
      !needle || fields.some((f) => f?.toLowerCase().includes(needle));
    const inChart = new Set(index.seats.map((s) => s.name));
    const byRole = new Map(agents.map((a) => [a.role, a]));
    const out: Row[] = [
      ...index.seats
        .filter((s) => matches(s.name, s.handle, s.goal, s.unit?.name))
        .map((seat) => ({ key: seat.key, name: seat.name, seat, agent: byRole.get(seat.name) })),
      ...agents
        .filter((a) => !inChart.has(a.role) && matches(a.role, a.handle))
        .map((agent) => ({ key: `removed:${agent.role}`, name: agent.role, seat: null, agent })),
    ];
    // By NAME. Never by a field that a live push moves.
    return out.sort((a, b) => a.name.localeCompare(b.name));
  }, [index.seats, agents, needle]);

  const groups = useMemo(() => {
    if (group === "flat") return [{ key: "all", label: "", rows }];
    if (group === "unit") {
      const byUnit = new Map<string, typeof rows>();
      for (const row of rows) {
        const key = row.seat
          ? (row.seat.unit?.name ?? "No unit — org-wide")
          : "Removed from the company";
        byUnit.set(key, [...(byUnit.get(key) ?? []), row]);
      }
      return [...byUnit.entries()]
        .sort((a, b) => a[0].localeCompare(b[0]))
        .map(([key, list]) => ({ key, label: key, rows: list }));
    }
    return STATE_ORDER.map((b) => ({
      key: b.key,
      label: b.label,
      rows: rows.filter((r) => bucketOf(r.seat, r.agent) === b.key),
    })).filter((g) => g.rows.length > 0);
  }, [rows, group]);

  /**
   * WHAT THE NUMBER BESIDE A GROUP HEAD COUNTS.
   *
   * It was a bare `g.rows.length` — the third unqualified figure the same unit
   * carried across this product, beside "Leadership 5" in the workspace rail
   * (the whole subtree) and "2 seats" on the org chart's block (its own
   * members). None of them said which, so a reader comparing two screens
   * concluded their company had changed shape.
   *
   * A UNIT GROUP HERE IS ITS DIRECT MEMBERS and can never be anything else: a
   * seat sits in exactly one group, so nothing under a unit is in it. That is
   * `unitDirectLabel`'s whole subject, and it lives in `lib/seats.ts` so
   * "directly" is one word across the product.
   *
   * AND A FILTER OUTRANKS BOTH, because with one on, every count on the screen
   * is over what matched — saying "3 seats directly in it" about a unit of
   * eleven while showing three is the same lie in the other direction.
   */
  const groupHint = (n: number) =>
    needle
      ? `${plural(n, "seat")} matching`
      : group === "unit"
        ? unitDirectLabel(n)
        : plural(n, "seat");

  // WHAT `[` AND `]` WALK: the cards in the order they are on screen, which is
  // the GROUPS' order rather than `rows`' — a reader stepping from a stopped
  // seat expects the next stopped seat, not whoever follows it alphabetically
  // across every bucket. Empty on the workload view, whose table is a
  // different list in a different order; a rail opened from there gets no
  // stepper, which is what `PeekHost` does with a list that publishes nothing.
  const { open: openPeek } = usePeekControls();
  usePeekNeighbours(
    useMemo(
      () =>
        view === "seats"
          ? groups.flatMap((g) =>
              g.rows.flatMap(({ seat }) =>
                seat ? [{ kind: "seat" as const, id: seat.handle || seat.name }] : [],
              ),
            )
          : [],
      [groups, view],
    ),
  );

  /**
   * One card, peekable.
   *
   * THE WRAPPER IS `display: contents`, so the CARD stays the grid item: a box
   * of its own would become the cell and leave every card at its natural
   * height, ragged against the taller ones beside it in the row. What it adds
   * is the click, caught on its way out of the anchor — so the card is still a
   * real link and ⌘-click, middle-click and the status bar all still name the
   * seat's own page, exactly as `rowPeekHandler` has it everywhere else.
   */
  const card = ({ key, seat, agent }: Row) =>
    seat ? (
      <div
        key={key}
        style={{ display: "contents" }}
        // BY HANDLE, OR BY NAME WHERE THE ENGINE REPORTED NONE. The seat screen
        // resolves both, which is what keeps a seat this engine derived no
        // handle for reachable at all; `seatPath` is the same rule for a link.
        onClick={rowPeekHandler(() => openPeek({ kind: "seat", id: seat.handle || seat.name }))}
      >
        <SeatCard seat={seat} agent={agent} nameOf={nameOf} />
      </div>
    ) : (
      <RemovedCard key={key} agent={agent!} nameOf={nameOf} />
    );

  return (
    <>
      {/* THE BAR'S "FIND A SEAT" IS THIS LIST'S FILTER (`q=`), the one field
          every Agents section carries in the same place — see `header.tsx`.
          It narrows both views: the seats, and who is carrying how much. */}
      <AgentsHeader index={index} filter={{ value: q, onChange: setQ }} />

      <div className="toolbar">
        <span className="spacer" />
        {view === "seats" && (
          <Segmented<Grouping>
            ariaLabel="Grouping"
            value={group}
            onChange={setGroup}
            options={[
              { value: "state", label: "By state" },
              { value: "unit", label: "By unit" },
              { value: "flat", label: "Flat" },
            ]}
          />
        )}
        {/* THE VIEW IS A PLACE, so it pushes history: somebody who opened the
            workload and pressed back expects the roster, not the screen
            before this one. */}
        <Segmented<(typeof VIEWS)[number]>
          ariaLabel="View"
          value={view}
          onChange={setView}
          options={[
            { value: "seats", label: "Seats" },
            { value: "workload", label: "Workload" },
          ]}
        />
      </div>

      {view === "workload" && <Workload seats={index.seats} needle={needle} />}

      {view === "seats" && !groups.length && (
        <EmptyState
          icon={<UsersGlyph size={32} />}
          title={q ? `No seat matches “${q}”` : "This company has no seats"}
          description={
            q
              ? "Find a seat matches a seat's name, handle, goal or unit."
              : "Roles are defined in the company configuration. Import one to spawn seats."
          }
        />
      )}

      {view === "seats" &&
        groups.map((g) =>
          g.label ? (
            <Section key={g.key} title={g.label} hint={groupHint(g.rows.length)}>
              <div className="seat-grid">{g.rows.map(card)}</div>
            </Section>
          ) : (
            <div className="seat-grid" key={g.key}>
              {g.rows.map(card)}
            </div>
          ),
        )}
    </>
  );
}

/**
 * A seat the engine still reports and the chart no longer holds: its name,
 * its handle and what it is doing. Not a link — there is no page for a seat
 * the company does not have.
 */
function RemovedCard({ agent, nameOf }: { agent: AgentRow; nameOf: NameOf }) {
  const now = useNow();
  return (
    <div className="seat-card" data-removed="">
      <div className="row">
        <div className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
          <strong className="truncate t-body">{agent.role}</strong>
          {agent.handle && (
            <span className="truncate t-caption mono">{handleLabel(agent.handle)}</span>
          )}
        </div>
        <Tag appearance="outline">removed</Tag>
      </div>
      <div className="seat-line truncate">{stateLine(agent, { now, nameOf })}</div>
    </div>
  );
}
