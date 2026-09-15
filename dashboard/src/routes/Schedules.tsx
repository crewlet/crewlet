/**
 * Recurring work: what fires, when it next fires, and how it last went.
 */

import { ScreenHead } from "~/app/Shell.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Badge, Panel, Stat, StatRow } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, tsKey, plural } from "~/lib/format.ts";
import { EmptyValue, RelativeTime, Skeleton, useNow } from "@crewlethq/ui";
import { CalendarClockGlyph, ErrorGlyph, ScheduleGlyph } from "@crewlethq/icons/glyphs";

const OUTCOME_TONE: Record<string, "positive" | "caution" | "critical" | "neutral"> = {
  fired: "positive",
  ok: "positive",
  skipped: "caution",
  missed: "caution",
  failed: "critical",
  error: "critical",
};

export function Schedules() {
  const now = useNow();
  // Schedules are pushed on a config apply, and the RUNS are not pushed at
  // all — so this polls, slowly, because a cron's next fire moves in minutes.
  const { data, loading, error } = useQuery("schedules", undefined, { pollMs: 30_000 });

  const schedules = data?.schedules ?? [];
  const runs = data?.recent_runs ?? [];
  const due = schedules.filter((s) => tsKey(s.next_run) > 0 && tsKey(s.next_run) - now < 3_600_000);

  return (
    <>
      <ScreenHead
        title="Schedules"
        sub="Role- and unit-scoped recurring work. Delivery is at-most-once, a missed tick is caught up, and a run is capped on wall clock."
        badges={<Badge outline>{plural(schedules.length, "schedule")} defined</Badge>}
      />

      <Panel padding="none">
        <StatRow cols={3}>
          <Stat
            icon={CalendarClockGlyph}
            label="Schedules"
            value={schedules.length}
            sub="across every seat and unit"
          />
          <Stat
            icon={ScheduleGlyph}
            label="Firing within the hour"
            value={due.length}
            sub={due.length ? due.map((s) => s.name).join(", ") : "nothing due soon"}
          />
          <Stat
            icon={ErrorGlyph}
            label="Recent failures"
            value={runs.filter((r) => OUTCOME_TONE[r.outcome] === "critical").length}
            sub={`in the last ${runs.length} recorded runs`}
          />
        </StatRow>
      </Panel>

      {loading && !schedules.length && (
        <Skeleton label="Loading the schedules" variant="text" rows={4} />
      )}
      <QueryState
        error={error}
        loading={loading}
        empty={
          schedules.length
            ? undefined
            : {
                title: "No recurring work is defined",
                hint: "Add a schedules block to a role or a unit in the company configuration — a standup, a nightly audit, a weekly report.",
              }
        }
      >
        <Panel title="Defined" icon={CalendarClockGlyph} count={schedules.length} padding="none">
          <DataTable
            rows={schedules}
            rowKey={(s) => `${s.scope}:${s.scope_name}:${s.name}`}
            defaultSort={{ key: "next", dir: "asc" }}
            columns={[
              { key: "name", header: "Name", sortValue: (s) => s.name, cell: (s) => s.name },
              {
                key: "scope",
                header: "Scope",
                shrink: true,
                sortValue: (s) => `${s.scope}:${s.scope_name}`,
                cell: (s) =>
                  s.scope === "role" ? (
                    <SeatChip name={s.scope_name} handle={s.scope_name} />
                  ) : (
                    <Badge outline>{s.scope_name}</Badge>
                  ),
              },
              {
                key: "cron",
                header: "Cron",
                shrink: true,
                sortValue: (s) => s.cron,
                cell: (s) => (
                  <code
                    className="inline"
                    title={s.timezone ? `timezone: ${s.timezone}` : undefined}
                  >
                    {s.cron}
                  </code>
                ),
              },
              {
                key: "task",
                header: "Task",
                cell: (s) => <span className="truncate">{s.task}</span>,
              },
              {
                key: "next",
                header: "Next",
                shrink: true,
                sortValue: (s) => tsKey(s.next_run) || Number.MAX_SAFE_INTEGER,
                cell: (s) =>
                  s.next_run ? (
                    <RelativeTime className="t-caption" value={s.next_run} now={now} />
                  ) : (
                    <EmptyValue label="Not reported" />
                  ),
              },
              {
                key: "last",
                header: "Last",
                shrink: true,
                sortValue: (s) => tsKey(s.last_run),
                cell: (s) =>
                  s.last_run ? (
                    <span className="row gap-1">
                      <RelativeTime className="t-caption" value={s.last_run} now={now} />
                      {s.last_outcome && (
                        <Badge tone={OUTCOME_TONE[s.last_outcome] ?? "neutral"}>
                          {s.last_outcome}
                        </Badge>
                      )}
                    </span>
                  ) : (
                    <span className="muted">never</span>
                  ),
              },
            ]}
          />
        </Panel>

        <Panel title="Recent runs" icon={ScheduleGlyph} count={runs.length} padding="none">
          <DataTable
            rows={runs}
            rowKey={(r) => `${r.fired_at}:${r.name}:${r.scope_name}`}
            defaultSort={{ key: "fired", dir: "desc" }}
            empty={{
              title: "No runs recorded",
              hint: "A run is recorded when a schedule fires. Nothing has fired since this node started keeping the record.",
            }}
            isFailed={(r) => OUTCOME_TONE[r.outcome] === "critical"}
            columns={[
              {
                key: "fired",
                header: "Fired",
                shrink: true,
                sortValue: (r) => tsKey(r.fired_at),
                cell: (r) => <RelativeTime className="t-caption" value={r.fired_at} now={now} />,
              },
              { key: "name", header: "Schedule", sortValue: (r) => r.name, cell: (r) => r.name },
              {
                key: "scope",
                header: "Scope",
                shrink: true,
                cell: (r) => <Badge outline>{r.scope_name}</Badge>,
              },
              {
                key: "outcome",
                header: "Outcome",
                shrink: true,
                sortValue: (r) => r.outcome,
                cell: (r) => <Badge tone={OUTCOME_TONE[r.outcome] ?? "neutral"}>{r.outcome}</Badge>,
              },
              {
                key: "detail",
                header: "Detail",
                cell: (r) => <span className="truncate t-caption">{r.detail}</span>,
              },
            ]}
          />
        </Panel>
      </QueryState>
    </>
  );
}
