/**
 * "median tokens per finished task": the middle of the tasks finished inside
 * the window that spent anything, as the TRACKER takes it over the whole set
 * (`totals=spend_tokens:median` — nearest rank, so the figure is one task's
 * real spend rather than an average of two), with how many tasks it is over.
 *
 * ABSENT ON A COMPANY WHOSE TRACKER IS NOT THE ENGINE'S. A Jira or GitLab
 * tracker is not charged per task here, so `work_items` is not served at all
 * (`unknown_query`) — and a tile saying "no finished tasks" there would be a
 * claim about the company's work this node cannot see. Any other refusal is
 * said in the tile.
 */

import { Skeleton } from "@crewlethq/ui";
import { medianTaskParams } from "./model.ts";
import { fmtCount, fmtExact, plural } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";

export function TaskMedianTile({
  since,
  until,
  words,
}: {
  /** The window's two instants, from the spend answer; absent until it arrives. */
  since?: string;
  until?: string;
  words: string | null;
}) {
  const ready = Boolean(since && until);
  const answer = useQuery("work_items", ready ? medianTaskParams(since!, until!) : undefined, {
    enabled: ready,
  });
  if (answer.error === "unknown_query") return null;
  const totals = answer.data?.totals ?? [];
  const median = totals.find((t) => t.op === "median")?.value;
  const count = totals.find((t) => t.op === "count")?.value ?? 0;
  return (
    <div className="spend-tile">
      {answer.error ? (
        <span className="spend-tile-figure spend-muted">Unavailable</span>
      ) : !answer.data ? (
        <Skeleton width="3rem" height="1.25rem" />
      ) : median === undefined ? (
        <span className="spend-tile-figure spend-muted">No tasks</span>
      ) : (
        <span
          className="spend-tile-figure"
          title={`${fmtExact(median)} tokens — the middle of ${plural(count, "finished task")}${words ? `, ${words}` : ""}`}
        >
          {fmtCount(median)}
        </span>
      )}
      <span className="spend-label">
        median tokens per finished task
        {median !== undefined && count > 0 ? ` · ${plural(count, "task")}` : ""}
      </span>
    </div>
  );
}
