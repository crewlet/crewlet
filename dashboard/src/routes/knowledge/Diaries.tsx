/**
 * Agent diaries (`#/knowledge/diaries` and `#/knowledge/diaries/{handle}`):
 * what every agent chose to remember, and one agent's diary in full.
 *
 * # Every agent, from one answer
 *
 * The list is `memory_overview`: EVERY agent seat in the chart with its three
 * totals and its newest note, each counted by the node holding the seat, in
 * one read. The Knowledge home this replaced drew a dozen diaries and stopped,
 * so the thirteenth agent's diary was reachable from nowhere — here the list
 * is as long as the chart and says how many there are.
 *
 * # A row answers, or says why it cannot
 *
 * A seat's memory is kept current only on the node holding it, so a row is
 * one of three things: counted by its holder ("node-2"), held by nobody (no
 * copy anywhere is current, so nothing is counted), or not answered — the
 * holder was silent, runs an older build, or is still taking the seat — with
 * that reason where the counts would be. A row of zeros for the last would
 * read as an agent that remembers nothing. The answer's `coverage` names
 * every holder that did not answer, in the callout every fleet answer draws.
 *
 * # One agent's page is its holder's answer
 *
 * `agent_memory`, the seat's own answer, drawn as the seat's Memory tab draws
 * its diary and episodes (`components/memory.tsx`) — and saying which node
 * answered, or that none holds the seat.
 */

import { useMemo, type ReactNode } from "react";
import { Callout, Card, EmptyState, EmptyValue } from "@crewlethq/ui";
import { BrainGlyph, CircleAlertGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { seatKindKey } from "~/app/crumbs.ts";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { ClockText, DateCell, NumberCell, SeatLabel } from "~/app/frame/cells.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { QueryState } from "~/components/common.tsx";
import { CoverageNote } from "~/components/CoverageNote.tsx";
import { DiaryCard, EpisodesCard, heldWords } from "~/components/memory.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { handleLabel, indexOrg, seatByAddress, type OrgIndex } from "~/lib/seats.ts";
import { plural, relTime, tsKey } from "~/lib/format.ts";
import type { MemoryOverviewSeat } from "~/contract/memory.ts";

/**
 * How often the list and a diary re-read: a minute, the Knowledge workspace's
 * cadence. A diary moves when a turn ends, and a reader here is reading what
 * an agent concluded, not watching it conclude.
 */
const DIARY_POLL_MS = 60_000;

/** Why a row carries no count: nobody holds the seat, or its holder did not say. */
export function uncounted(row: MemoryOverviewSeat): string | null {
  if (row.held_by === "none")
    return "No node holds this agent, so no copy of its memory is current";
  if (row.unavailable) return `Not answered: ${row.unavailable}`;
  return null;
}

/** A seat's name by its address — a handle a rename retired still finds it. */
function nameOf(index: OrgIndex, handle: string): string {
  return seatByAddress(index, handle)?.name || handle;
}

/** A row's count, or why it carries none. */
function count(row: MemoryOverviewSeat, value: number) {
  const why = uncounted(row);
  return why ? <EmptyValue label={why} /> : <NumberCell value={value} />;
}

/**
 * The diary list's columns. They read the chart's names and kinds and nothing
 * else on the screen, so they are held on it (`useMemo` below): every row is
 * memoised on this list, and one built inline drew every agent on every poll.
 */
function diaryColumns(index: OrgIndex): GridColumn<MemoryOverviewSeat>[] {
  return [
    {
      key: "agent",
      header: "Agent",
      floor: "170px",
      phoneLead: true,
      sortValue: (r) => nameOf(index, r.handle).toLowerCase(),
      cell: (r) => (
        <SeatLabel name={nameOf(index, r.handle)} kind={seatByAddress(index, r.handle)?.kind} />
      ),
    },
    {
      key: "latest",
      header: "Latest entry",
      floor: "160px",
      drop: 3,
      cell: (r) => <LatestCell row={r} />,
    },
    {
      key: "written",
      header: "Written",
      shrink: true,
      align: "right",
      sortValue: (r) => (r.last_reflection_at ? tsKey(r.last_reflection_at) : null),
      cell: (r) =>
        uncounted(r) ? (
          <EmptyValue label={uncounted(r)!} />
        ) : (
          <DateCell at={r.last_reflection_at} />
        ),
    },
    {
      key: "diary",
      header: "Entries",
      shrink: true,
      align: "right",
      sortValue: (r) => (uncounted(r) ? null : r.diary_total),
      cell: (r) => count(r, r.diary_total),
    },
    {
      key: "episodes",
      header: "Episodes",
      shrink: true,
      align: "right",
      drop: 2,
      sortValue: (r) => (uncounted(r) ? null : r.episodes_total),
      cell: (r) => count(r, r.episodes_total),
    },
    {
      key: "skills",
      header: "Skills",
      shrink: true,
      align: "right",
      drop: 1,
      sortValue: (r) => (uncounted(r) ? null : r.skills_total),
      cell: (r) => count(r, r.skills_total),
    },
    {
      key: "held",
      header: "Held by",
      shrink: true,
      drop: 4,
      sortValue: (r) => r.held_by,
      cell: (r) =>
        r.held_by === "none" ? (
          <span className="muted">No node</span>
        ) : (
          <span className="mono t-cell">{r.held_by}</span>
        ),
    },
  ];
}

export function Diaries() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const overview = useQuery("memory_overview", undefined, { pollMs: DIARY_POLL_MS });
  const rows = overview.data?.seats ?? [];
  const columns = useMemo(() => diaryColumns(index), [index]);

  return (
    <>
      <PageNote>
        What each agent chose to remember, counted by the node holding the agent — the one copy of
        its memory kept current.
      </PageNote>
      <CoverageNote
        coverage={[overview.data?.coverage]}
        what="this list"
        lost="the agents each of them holds are listed without their counts"
      />
      <QueryState
        error={overview.error}
        refusal={overview.refusal}
        loading={overview.loading && !overview.data}
        empty={
          overview.data && rows.length === 0
            ? {
                title: "No agents",
                hint: "A diary belongs to an agent seat, and this company's chart has none yet.",
              }
            : undefined
        }
      >
        <Card padding="none">
          <Card.Header
            icon={<BrainGlyph size="sm" />}
            count={rows.length}
            subtitle="Every agent in the chart"
          >
            <Card.Title as="h2">Agent diaries</Card.Title>
          </Card.Header>
          <DataGrid<MemoryOverviewSeat>
            name="diaries"
            rows={rows}
            rowKey={(r) => r.agent_id}
            defaultSort="agent"
            rowHref={(r) => href(["knowledge", "diaries", r.handle])}
            columns={columns}
          />
        </Card>
      </QueryState>
    </>
  );
}

/** A row's newest note, or why there is none to show. */
function LatestCell({ row }: { row: MemoryOverviewSeat }): ReactNode {
  const why = uncounted(row);
  if (why) {
    return (
      <span className="muted truncate" title={why}>
        {row.held_by === "none" ? "Held by no node" : "Its holder did not answer"}
      </span>
    );
  }
  if (!row.latest_reflection) return <EmptyValue label="Nothing written yet" />;
  return (
    <span className="truncate" title={row.latest_reflection.content}>
      {row.latest_reflection.content}
    </span>
  );
}

export function Diary({ handle }: { handle: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const seat = seatByAddress(index, handle);
  const name = seat?.name || handle;
  usePageLabels(seat?.name ? { [handle]: seat.name, [seatKindKey(handle)]: seat.kind } : {});
  const memory = useQuery("agent_memory", { id: handle }, { pollMs: DIARY_POLL_MS });
  const data = memory.data;

  if (seat && seat.kind === "human") {
    return (
      <EmptyState
        icon={<BrainGlyph size="xl" />}
        title={`${name} is a person`}
        description="A diary is what an agent keeps; a person's memory is their own."
      />
    );
  }
  return (
    <div className="col gap-4">
      {data && data.held_by !== "none" && (
        <PageNote>
          {heldWords(data.held_by)}
          {data.latest_reflection && (
            <>
              {" "}
              Last written <LastWritten at={data.latest_reflection.created_at} />.
            </>
          )}
        </PageNote>
      )}
      {!seat && org && (
        <Callout variant="info" icon={<CircleAlertGlyph size="md" />}>
          No agent in the chart is called <code className="inline">{handleLabel(handle)}</code> —
          this is whatever memory the fleet still holds under that handle.
        </Callout>
      )}
      <QueryState error={memory.error} refusal={memory.refusal} loading={memory.loading && !data}>
        {data?.held_by === "none" ? (
          <EmptyState
            icon={<BrainGlyph size="xl" />}
            title="No node holds this agent"
            description={`${heldWords("none")} It is drawn again once a node takes ${name}.`}
          />
        ) : (
          data && (
            <>
              <DiaryCard memory={data} heading="h2" />
              <EpisodesCard memory={data} heading="h2" />
              <p className="t-caption">
                {plural(data.skills_total, "learned skill")} and{" "}
                {plural(data.counterparties_total, "colleague")} remembered —{" "}
                <a
                  className="t-link prose-link"
                  href={href(["agents", "seats", handle], { tab: "memory" })}
                >
                  {name}&rsquo;s whole memory
                </a>
              </p>
            </>
          )
        )}
      </QueryState>
    </div>
  );
}

/** When a diary was last written, read off the clock by these words alone. */
function LastWritten({ at }: { at: string }) {
  return <ClockText read={(now) => relTime(at, now)} />;
}
