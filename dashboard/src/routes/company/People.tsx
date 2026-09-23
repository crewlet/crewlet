/**
 * Everyone in the company, and what each of them is doing.
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
 */

import { useMemo } from "react";
import { fmtMinutes } from "~/lib/work.ts";
import { plural } from "~/lib/format.ts";
import { href, useParam } from "~/app/router.tsx";
import { SeatCard, Section } from "~/components/common.tsx";
import { Card, cx, EmptyState, EmptyValue, Input, Tag } from "@crewlethq/ui";
import { GroupGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
// OURS, AND DELIBERATELY. `SegmentedControl` welds keyboard ACTIVATION to its
// `semantics`: `radio` commits the option the arrows land on. Both strips here
// drive a `useTab`, which pushes a history entry — and the view strip swaps the
// screen for a table that fires `work_workload`. See the report.
import { Segmented } from "~/ui/primitives.tsx";
import { useAgents, useOrg, useSandboxes } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { QueryState } from "~/components/common.tsx";
import { awaitingPerson, indexOrg, runState, unitDirectLabel, type Seat } from "~/lib/seats.ts";
import { loadFraction, loadRows, loadSentence, loadTone, type Load } from "~/lib/workload.ts";
import type { AgentRow } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { useTab } from "~/app/frame/tabs.ts";

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
function Workload({ seats }: { seats: Seat[] }) {
  const answer = useQuery("work_workload", undefined, { pollMs: 60_000 });
  const rows = useMemo(() => loadRows(answer.data?.rows ?? [], seats), [answer.data?.rows, seats]);
  // THE HEAVIEST QUEUE ON SCREEN is the bar's own scale — computed once here
  // rather than per row, so every track on the table is drawn against the
  // same number.
  const heaviest = useMemo(() => rows.reduce((n, load) => Math.max(n, load.open), 0), [rows]);
  return (
    <QueryState
      error={answer.error}
      refusal={answer.refusal}
      loading={answer.loading}
      empty={
        rows.length
          ? undefined
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
          `#/company/people/{handle}`, and `href` is what spells it — the one
          place the route and its encoding are written down. A plain click
          peeks instead, which on this table is the whole point: the question
          is who is carrying the most, and answering it should not cost the row
          the reader was comparing against. */}
      <a
        className="wl-who"
        href={href(["company", "people", load.handle])}
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
  { key: "offline", label: "Not running here" },
  // A SEAT THE ENGINE REMOVED is not a seat a healthy peer is running.
  // `terminated` had no branch and fell through to `offline`, so a removed
  // seat sat in "Not running here" beside seats another node runs perfectly
  // well — the one bucket whose whole meaning is "this is fine, look
  // elsewhere".
  { key: "terminated", label: "Removed from the company" },
  { key: "human", label: "Human teammates" },
] as const;

function bucketOf(
  seat: Seat,
  agent: AgentRow | undefined,
  sandboxes: ReturnType<typeof useSandboxes>,
): string {
  // A KIND IS NOT A STATE, and this short-circuit is why a human teammate
  // could never be reported as waiting on anybody: every human seat was
  // filed under one label before anything about what it is doing was read.
  // Grouping by state now answers the same question for both kinds, and
  // "which seats are people" is a filter rather than a bucket.
  const sandbox = sandboxes.find((s) => s.role === seat.name);
  if (awaitingPerson(sandbox?.status)) return "needs";
  if (agent?.last_error) return "broken";
  const state = runState(agent, sandboxes);
  if (state === "afk" || state === "failed") return "broken";
  if (state === "working" || state === "awaiting_sandbox") return "working";
  if (state === "terminated") return "terminated";
  if (state === "idle") return "idle";
  // A human seat is never run by a node, so "not running here" would be a
  // fault report about something that is working exactly as designed.
  return seat.kind === "human" ? "human" : "offline";
}

export function People() {
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const org = useOrg();
  const [view, setView] = useTab("view", VIEWS);
  // A GROUPING IS A FILTER, not a section, which is `tabs.ts`'s own
  // distinction: a section is the page you are on and takes the number keys,
  // a filter narrows what is on it and leaves them alone. Declared as a
  // section it bound the digits a SECOND time on this one screen —
  // `useKeyChords` installs one window listener per call and each returns
  // after its own first match, so there is no precedence and both fired:
  // `1` set the view AND regrouped, `2` set the view AND regrouped, and `3`,
  // which is past the end of the two views, moved the grouping alone. It also
  // put every regroup in the history, so Back walked groupings instead of
  // leaving the screen.
  const [group, setGroup] = useTab("group", GROUPINGS, "filter");
  const [q, setQ] = useParam("q", "");

  const index = useMemo(() => indexOrg(org), [org]);

  // HOISTED OUT OF THE MEMO, because two things need to know whether a filter
  // is on: the filter itself, and every count on the screen that is now over
  // what matched rather than over the company.
  const needle = q.trim().toLowerCase();

  const rows = useMemo(() => {
    return (
      index.seats
        .filter(
          (s) =>
            !needle ||
            s.name.toLowerCase().includes(needle) ||
            s.handle.toLowerCase().includes(needle) ||
            s.goal.toLowerCase().includes(needle) ||
            s.unit?.name.toLowerCase().includes(needle),
        )
        .map((seat) => ({ seat, agent: agents.find((a) => a.role === seat.name) }))
        // By NAME. Never by a field that a live push moves.
        .sort((a, b) => a.seat.name.localeCompare(b.seat.name))
    );
  }, [index.seats, agents, needle]);

  const groups = useMemo(() => {
    if (group === "flat") return [{ key: "all", label: "", rows }];
    if (group === "unit") {
      const byUnit = new Map<string, typeof rows>();
      for (const row of rows) {
        const key = row.seat.unit?.name ?? "No unit — org-wide";
        byUnit.set(key, [...(byUnit.get(key) ?? []), row]);
      }
      return [...byUnit.entries()]
        .sort((a, b) => a[0].localeCompare(b[0]))
        .map(([key, list]) => ({ key, label: key, rows: list }));
    }
    return STATE_ORDER.map((b) => ({
      key: b.key,
      label: b.label,
      rows: rows.filter((r) => bucketOf(r.seat, r.agent, sandboxes) === b.key),
    })).filter((g) => g.rows.length > 0);
  }, [rows, group, sandboxes]);

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

  const agentSeats = index.seats.filter((s) => s.kind === "agent").length;

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
              g.rows.map(({ seat }) => ({ kind: "seat" as const, id: seat.handle || seat.name })),
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
  const card = ({ seat, agent }: (typeof rows)[number]) => (
    <div
      key={seat.key}
      style={{ display: "contents" }}
      // BY HANDLE, OR BY NAME WHERE THE ENGINE REPORTED NONE. The seat screen
      // resolves both, which is what keeps a seat this engine derived no
      // handle for reachable at all; `seatPath` is the same rule for a link.
      onClick={rowPeekHandler(() => openPeek({ kind: "seat", id: seat.handle || seat.name }))}
    >
      <SeatCard seat={seat} agent={agent} sandboxes={sandboxes} />
    </div>
  );

  return (
    <>
      <PageActions>
        {
          <>
            <Tag appearance="outline">{plural(agentSeats, "agent seat")}</Tag>
            {index.seats.length - agentSeats > 0 && (
              <Tag appearance="outline">{plural(index.seats.length - agentSeats, "human")}</Tag>
            )}
          </>
        }
      </PageActions>
      <PageNote>
        Every seat in the company, the ones this node runs and the ones its peers do. A seat that is
        not held anywhere reads as “not running here”.
      </PageNote>

      <div className="toolbar">
        {view === "seats" && (
          <div style={{ maxWidth: 320, flex: 1 }}>
            {/* `SearchTrigger` is the one that OPENS a palette; this box filters
                the list under it, so the peer is `Input` with the glyph in its
                leading slot. `onClear` is theirs and ours had none — the X only
                appears once something is typed. */}
            <Input
              type="search"
              width="full"
              value={q}
              onChange={(e) => setQ(e.target.value)}
              onClear={() => setQ("")}
              clearLabel="Clear the seat filter"
              leading={<SearchGlyph size="sm" />}
              aria-label="Filter seats"
              placeholder="Filter by name, handle, goal or unit"
            />
          </div>
        )}
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

      {view === "workload" && <Workload seats={index.seats} />}

      {view === "seats" && !groups.length && (
        <EmptyState
          icon={<GroupGlyph size={32} />}
          title={q ? `No seat matches “${q}”` : "This company has no seats"}
          description={
            q
              ? "The filter matches a seat's name, handle, goal or unit."
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
