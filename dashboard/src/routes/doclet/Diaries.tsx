/**
 * Agent diaries (`#/doclet/diaries` and `#/doclet/diaries/{handle}`):
 * what every agent chose to remember, and one agent's diary in full.
 *
 * # Every agent, from one answer
 *
 * The list is `memory_overview`: EVERY agent seat in the chart with its three
 * totals and its newest note, each counted by the node holding the seat, in
 * one read. The Doclet home this replaced drew a dozen diaries and stopped,
 * so the thirteenth agent's diary was reachable from nowhere — here the list
 * is as long as the chart and says how many there are.
 *
 * # A row answers, or says why it cannot
 *
 * A seat's memory is kept current only on the node holding it, so a row is
 * one of three things: counted by its holder ("node-2"), held by nobody (no
 * copy anywhere is current, so nothing is counted), or not answered — the
 * holder was silent, its answer could not be read, or it is still taking the
 * seat — with that reason where the counts would be. A row of zeros for the
 * last would read as an agent that remembers nothing. The answer's
 * `coverage` names every holder that did not answer, in the callout every
 * fleet answer draws.
 *
 * # One agent's page is its holder's answer
 *
 * `agent_memory`, the seat's own answer, drawn as the seat's Memory tab draws
 * its diary and episodes (`components/memory.tsx`) — and saying which node
 * answered, or that none holds the seat.
 */

import { useMemo, type ReactNode } from "react";
import {
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  InlineCode,
  TableNumberCell,
  TableDateCell,
  TableEntityCell,
  PageNote,
} from "@crewlethq/ui";
import { BrainGlyph, CircleAlertGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { seatKindKey } from "~/app/crumbs.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { plainSeat } from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import { CoverageNote } from "~/components/CoverageNote.tsx";
import { DiaryCard, EpisodesCard, heldWords } from "~/components/memory.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { handleLabel, indexOrg, type OrgIndex } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { plural, relTime, tsKey, fmtCount } from "~/lib/format.ts";
import type { MemoryOverviewSeat } from "~/contract/memory.ts";

/**
 * How often the list and a diary re-read: a minute, the Doclet workspace's
 * cadence. A diary moves when a turn ends, and a reader here is reading what
 * an agent concluded, not watching it conclude.
 */
const DIARY_POLL_MS = 60_000;

/** Why a row carries no count: nobody holds the seat, or its holder did not say. */
export function uncounted(row: MemoryOverviewSeat): string | null {
  if (row.held_by === "none") return "Not running on any node";
  if (row.unavailable) return `Not answered: ${row.unavailable}`;
  return null;
}

function nameOf(index: OrgIndex, handle: string): string {
  return index.byHandle.get(handle)?.name || handle;
}

export function Diaries() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const overview = useQuery("memory_overview", undefined, { pollMs: DIARY_POLL_MS });
  const rows = overview.data?.seats ?? [];

  const count = (row: MemoryOverviewSeat, value: number) => {
    const why = uncounted(row);
    return why ? <EmptyValue label={why} /> : <TableNumberCell value={value} format={fmtCount} />;
  };

  return (
    <>
      <PageNote>What each agent chose to remember.</PageNote>
      <CoverageNote
        coverage={[overview.data?.coverage]}
        what="Diaries"
        lost={{
          one: "Its agents are listed without counts",
          many: "Their agents are listed without counts",
        }}
      />
      <QueryState
        error={overview.error}
        refusal={overview.refusal}
        loading={overview.loading && !overview.data}
        empty={
          overview.data && rows.length === 0
            ? {
                title: "No agents",
                hint: "Add an agent in Edit org.",
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
            rowHref={(r) => href(["doclet", "diaries", r.handle])}
            columns={[
              {
                key: "agent",
                header: "Agent",
                floor: "170px",
                phoneLead: true,
                sortValue: (r) => nameOf(index, r.handle).toLowerCase(),
                cell: (r) => (
                  <TableEntityCell
                    {...plainSeat({
                      name: nameOf(index, r.handle),
                      kind: index.byHandle.get(r.handle)?.kind,
                    })}
                  />
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
                    <TableDateCell value={r.last_reflection_at} now={now} />
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
            ]}
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
  const now = useNow();
  const seat = index.byHandle.get(handle);
  const name = seat?.name || handle;
  usePageLabels(seat?.name ? { [handle]: seat.name, [seatKindKey(handle)]: seat.kind } : {});
  const memory = useQuery("agent_memory", { id: handle }, { pollMs: DIARY_POLL_MS });
  const data = memory.data;

  if (seat && seat.kind === "human") {
    return (
      <EmptyState
        icon={<BrainGlyph size="xl" />}
        title={`${name} is a person`}
        description="Only agents keep diaries."
      />
    );
  }
  return (
    <div className="col gap-4">
      {data && data.held_by !== "none" && (
        <PageNote>
          {heldWords(data.held_by)}
          {data.latest_reflection && (
            <> Last written {relTime(data.latest_reflection.created_at, now)}.</>
          )}
        </PageNote>
      )}
      {!seat && org && (
        <Callout variant="info" icon={<CircleAlertGlyph size="md" />}>
          No agent is called <InlineCode>{handleLabel(handle)}</InlineCode>. Showing memory left
          under that handle.
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
              <EpisodesCard memory={data} now={now} heading="h2" />
              <p className="t-caption">
                <a
                  className="t-link prose-link"
                  href={href(["company", "org", "seats", handle], { tab: "memory" })}
                >
                  {plural(data.skills_total, "learned skill")} and{" "}
                  {plural(data.counterparties_total, "colleague")} remembered
                </a>
              </p>
            </>
          )
        )}
      </QueryState>
    </div>
  );
}
