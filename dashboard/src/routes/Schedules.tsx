/**
 * Recurring work: what fires, when it next fires, and how it last went.
 */

import { QueryState, recordTable, SeatChip } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, tsKey, plural } from "~/lib/format.ts";
import {
  Card,
  DataTable,
  EmptyState,
  EmptyValue,
  PageHeader,
  RelativeTime,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
  useNow,
} from "@crewlethq/ui";
import type { Tone } from "@crewlethq/ui";
import { CalendarClockGlyph, ErrorGlyph, ScheduleGlyph } from "@crewlethq/icons/glyphs";

const OUTCOME_TONE: Record<string, Tone> = {
  fired: "success",
  ok: "success",
  skipped: "warning",
  missed: "warning",
  failed: "danger",
  error: "danger",
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
      <PageHeader
        title="Schedules"
        description="Role- and unit-scoped recurring work. Delivery is at-most-once, a missed tick is caught up, and a run is capped on wall clock."
        badges={<Tag appearance="outline">{plural(schedules.length, "schedule")} defined</Tag>}
      />

      <StatGroup columns={3}>
        <StatCard
          icon={<CalendarClockGlyph />}
          label="Schedules"
          value={schedules.length}
          sub="across every seat and unit"
        />
        <StatCard
          icon={<ScheduleGlyph />}
          label="Firing within the hour"
          value={due.length}
          sub={due.length ? due.map((s) => s.name).join(", ") : "nothing due soon"}
        />
        <StatCard
          icon={<ErrorGlyph />}
          label="Recent failures"
          value={runs.filter((r) => OUTCOME_TONE[r.outcome] === "danger").length}
          sub={`in the last ${runs.length} recorded runs`}
        />
      </StatGroup>

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
        <Card as="section" padding="none">
          <Card.Header
            divided
            style={{ paddingInline: "var(--spacing-4)", paddingTop: "var(--spacing-3)" }}
            icon={<CalendarClockGlyph size="sm" />}
            count={schedules.length}
          >
            <Card.Title>Defined</Card.Title>
          </Card.Header>
          <DataTable
            getRowKey={(s) => `${s.scope}:${s.scope_name}:${s.name}`}
            defaultSort={{ key: "next", direction: "asc" }}
            {...recordTable(schedules, [
              {
                key: "name",
                header: "Name",
                sortable: true,
                sortValue: (s) => s.name,
                render: (s) => s.name,
              },
              {
                key: "scope",
                header: "Scope",
                shrink: true,
                sortable: true,
                sortValue: (s) => `${s.scope}:${s.scope_name}`,
                render: (s) =>
                  s.scope === "role" ? (
                    <SeatChip name={s.scope_name} handle={s.scope_name} />
                  ) : (
                    <Tag appearance="outline">{s.scope_name}</Tag>
                  ),
              },
              {
                key: "cron",
                header: "Cron",
                shrink: true,
                sortable: true,
                sortValue: (s) => s.cron,
                render: (s) => (
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
                render: (s) => <span className="truncate">{s.task}</span>,
              },
              {
                key: "next",
                header: "Next",
                shrink: true,
                sortable: true,
                sortValue: (s) => tsKey(s.next_run) || Number.MAX_SAFE_INTEGER,
                render: (s) =>
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
                sortable: true,
                sortValue: (s) => tsKey(s.last_run),
                render: (s) =>
                  s.last_run ? (
                    <span className="row gap-1">
                      <RelativeTime className="t-caption" value={s.last_run} now={now} />
                      {s.last_outcome && (
                        <Tag variant={OUTCOME_TONE[s.last_outcome] ?? "neutral"}>
                          {s.last_outcome}
                        </Tag>
                      )}
                    </span>
                  ) : (
                    <span className="muted">never</span>
                  ),
              },
            ])}
          />
        </Card>

        <Card as="section" padding="none">
          <Card.Header
            divided
            style={{ paddingInline: "var(--spacing-4)", paddingTop: "var(--spacing-3)" }}
            icon={<ScheduleGlyph size="sm" />}
            count={runs.length}
          >
            <Card.Title>Recent runs</Card.Title>
          </Card.Header>
          <DataTable
            getRowKey={(r) => `${r.fired_at}:${r.name}:${r.scope_name}`}
            defaultSort={{ key: "fired", direction: "desc" }}
            emptyMessage={
              <EmptyState
                size="compact"
                title="No runs recorded"
                description="A run is recorded when a schedule fires. Nothing has fired since this node started keeping the record."
              />
            }
            rowTone={(r) => (OUTCOME_TONE[r.outcome] === "danger" ? "danger" : null)}
            {...recordTable(runs, [
              {
                key: "fired",
                header: "Fired",
                shrink: true,
                sortable: true,
                firstDirection: "desc",
                sortValue: (r) => tsKey(r.fired_at),
                render: (r) => <RelativeTime className="t-caption" value={r.fired_at} now={now} />,
              },
              {
                key: "name",
                header: "Schedule",
                sortable: true,
                sortValue: (r) => r.name,
                render: (r) => r.name,
              },
              {
                key: "scope",
                header: "Scope",
                shrink: true,
                render: (r) => <Tag appearance="outline">{r.scope_name}</Tag>,
              },
              {
                key: "outcome",
                header: "Outcome",
                shrink: true,
                sortable: true,
                sortValue: (r) => r.outcome,
                render: (r) => (
                  <Tag variant={OUTCOME_TONE[r.outcome] ?? "neutral"}>{r.outcome}</Tag>
                ),
              },
              {
                key: "detail",
                header: "Detail",
                render: (r) => <span className="truncate t-caption">{r.detail}</span>,
              },
            ])}
          />
        </Card>
      </QueryState>
    </>
  );
}
