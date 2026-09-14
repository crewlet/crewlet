/**
 * Recurring work: what fires, when it next fires, and how it last went.
 *
 * THE NAMES HERE ARE THE SERVER'S. This screen used to read `scope`,
 * `scope_name`, `last_run` and `last_outcome` off a schedule and `name`,
 * `scope_name` and `detail` off a run; `schedule.Row` serialises `scope_type`
 * and `scope_id` and has no last-run field at all, and a ledger row carries
 * `schedule_name`, `fire_label`, `target_handle`, `scheduled_at` and
 * `trace_id`. Seven cells rendered `undefined` on every row, and the sort and
 * row keys they were built from collapsed every row onto one key.
 *
 * The last fire is JOINED from the ledger rather than read off the row, on the
 * identity the ledger keys on — (scope_type, scope_id, name) — because that is
 * where the fact actually lives.
 *
 * AND THE LEDGER HAS TWO OUTCOMES. `schedule.Outcome` is `fired` or
 * `skipped_catchup`: it is a DISPATCH ledger, not a turn-outcome one, so
 * "recent failures" counted a value the engine cannot emit and read 0 for ever
 * on every company. What it can honestly say is how many ticks were skipped,
 * which is the catchup cap doing its job or a node that was down too long.
 */

import { ScreenHead } from "~/app/Shell.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Badge, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, inTime, relTime, tsKey, plural } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { ScheduleRow, ScheduleRunRow } from "~/protocol/index.ts";

/** The ledger's two outcomes, and nothing else — see the note above. */
const OUTCOME_TONE: Record<string, "positive" | "caution" | "neutral"> = {
  fired: "positive",
  skipped_catchup: "caution",
};

/** The identity a ledger row and a schedule row share. */
function identity(scopeType: string, scopeID: string, name: string): string {
  return `${scopeType}:${scopeID}:${name}`;
}

const rowID = (s: ScheduleRow) => identity(s.scope_type, s.scope_id, s.name);
const runID = (r: ScheduleRunRow) => identity(r.scope_type, r.scope_id, r.schedule_name);

export function Schedules() {
  const now = useNow();
  // Schedules are pushed on a config apply, and the RUNS are not pushed at
  // all — so this polls, slowly, because a cron's next fire moves in minutes.
  const { data, loading, error } = useQuery("schedules", undefined, { pollMs: 30_000 });

  const schedules = data?.schedules ?? [];
  const runs = data?.recent_runs ?? [];
  const due = schedules.filter((s) => tsKey(s.next_run) > 0 && tsKey(s.next_run) - now < 3_600_000);
  // A schedule that cannot fire at all, which is a defect rather than a
  // choice: a cron nobody can parse, a timezone that no longer exists. The
  // row says so in its own `problem` field and a blank Next cell was the
  // only symptom.
  const broken = schedules.filter((s) => s.problem);

  // The last fire per schedule, from the ledger. `Recent` returns newest
  // first, so the first row per identity is the latest one.
  const lastFire = new Map<string, ScheduleRunRow>();
  for (const run of runs) if (!lastFire.has(runID(run))) lastFire.set(runID(run), run);

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
            icon="calendar"
            label="Schedules"
            value={schedules.length}
            sub="across every seat and unit"
          />
          <Stat
            icon="clock"
            label="Firing within the hour"
            value={due.length}
            sub={due.length ? due.map((s) => s.name).join(", ") : "nothing due soon"}
          />
          <Stat
            icon="alert"
            label="Cannot fire"
            tone={broken.length ? "critical" : undefined}
            value={broken.length}
            sub={
              broken.length
                ? broken.map((s) => s.name).join(", ")
                : "every expression parses and every timezone resolves"
            }
          />
        </StatRow>
      </Panel>

      {loading && !schedules.length && <Skeleton rows={4} />}
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
        <Panel title="Defined" icon="calendar" count={schedules.length} padding="none">
          <DataTable
            rows={schedules}
            rowKey={rowID}
            defaultSort={{ key: "next", dir: "asc" }}
            columns={[
              {
                key: "name",
                header: "Name",
                sortValue: (s) => s.name,
                cell: (s) => (
                  <span className="row gap-1">
                    <span className="truncate">{s.name}</span>
                    {!s.enabled && <Badge outline>disabled</Badge>}
                  </span>
                ),
              },
              {
                key: "scope",
                header: "Scope",
                shrink: true,
                sortValue: (s) => `${s.scope_type}:${s.scope_id}`,
                cell: (s) =>
                  s.scope_type === "role" ? (
                    <SeatChip name={s.scope_id} handle={s.scope_id} />
                  ) : (
                    <Badge outline>{s.scope_id}</Badge>
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
                // WHO A FIRE REACHES, resolved by the engine from the scope
                // and the target — a unit schedule targeting `each` wakes
                // every member, one targeting `lead` wakes one seat, and a
                // role schedule's target is meaningless rather than defaulted.
                key: "runners",
                header: "Wakes",
                shrink: true,
                cell: (s) =>
                  s.runners?.length ? (
                    <span className="row gap-1">
                      {s.runners.slice(0, 3).map((handle) => (
                        <SeatChip key={handle} name={handle} handle={handle} />
                      ))}
                      {s.runners.length > 3 && (
                        <span className="t-caption">+{s.runners.length - 3}</span>
                      )}
                    </span>
                  ) : (
                    <span className="faint">nobody</span>
                  ),
              },
              {
                key: "next",
                header: "Next",
                shrink: true,
                sortValue: (s) => tsKey(s.next_run) || Number.MAX_SAFE_INTEGER,
                cell: (s) =>
                  s.next_run ? (
                    <span className="t-caption" title={fmtDateTime(s.next_run)}>
                      {inTime(s.next_run, now)}
                    </span>
                  ) : s.problem ? (
                    // THE REASON, not a blank. A schedule whose timezone was
                    // renamed shows nothing under Next and looks merely idle.
                    <Badge tone="critical" title={s.problem}>
                      {s.problem}
                    </Badge>
                  ) : (
                    <span className="faint" title="disabled, or the calendar never reaches it">
                      —
                    </span>
                  ),
              },
              {
                key: "last",
                header: "Last",
                shrink: true,
                sortValue: (s) => tsKey(lastFire.get(rowID(s))?.fired_at ?? ""),
                cell: (s) => {
                  const run = lastFire.get(rowID(s));
                  if (!run) return <span className="faint">never</span>;
                  return (
                    <span className="row gap-1">
                      <span className="t-caption" title={fmtDateTime(run.fired_at)}>
                        {relTime(run.fired_at, now)}
                      </span>
                      {run.outcome && (
                        <Badge tone={OUTCOME_TONE[run.outcome] ?? "neutral"}>{run.outcome}</Badge>
                      )}
                    </span>
                  );
                },
              },
            ]}
          />
        </Panel>

        <Panel title="Recent runs" icon="clock" count={runs.length} padding="none">
          <DataTable
            rows={runs}
            rowKey={(r) => `${r.fired_at}:${runID(r)}:${r.fire_label}`}
            defaultSort={{ key: "fired", dir: "desc" }}
            empty={{
              title: "No runs recorded",
              hint: "A run is recorded when a schedule fires. Nothing has fired since this node started keeping the record.",
            }}
            columns={[
              {
                key: "fired",
                header: "Fired",
                shrink: true,
                sortValue: (r) => tsKey(r.fired_at),
                cell: (r) => (
                  <span className="t-caption" title={fmtDateTime(r.fired_at)}>
                    {relTime(r.fired_at, now)}
                  </span>
                ),
              },
              {
                key: "name",
                header: "Schedule",
                sortValue: (r) => r.schedule_name,
                cell: (r) => r.schedule_name,
              },
              {
                key: "scope",
                header: "Scope",
                shrink: true,
                sortValue: (r) => `${r.scope_type}:${r.scope_id}`,
                cell: (r) => <Badge outline>{r.scope_id}</Badge>,
              },
              {
                // WHICH SEAT this fire woke. A unit schedule targeting `each`
                // writes one ledger row per member, so without this column
                // three rows read as one fire repeated.
                key: "target",
                header: "Woke",
                shrink: true,
                cell: (r) =>
                  r.target_handle ? (
                    <SeatChip name={r.target_handle} handle={r.target_handle} />
                  ) : (
                    <span className="faint">—</span>
                  ),
              },
              {
                key: "outcome",
                header: "Outcome",
                shrink: true,
                sortValue: (r) => r.outcome,
                cell: (r) => (
                  <Badge
                    tone={OUTCOME_TONE[r.outcome] ?? "neutral"}
                    title={
                      r.outcome === "skipped_catchup"
                        ? "the tick was missed and fell outside the catchup window"
                        : undefined
                    }
                  >
                    {r.outcome || "—"}
                  </Badge>
                ),
              },
              {
                // THE TICK, which is not the instant it ran: a catchup run
                // fires now for a tick that was due earlier, and the pair is
                // the only way to see that.
                key: "due",
                header: "For the tick",
                shrink: true,
                sortValue: (r) => tsKey(r.scheduled_at),
                cell: (r) =>
                  r.scheduled_at ? (
                    <span className="t-caption" title={r.fire_label}>
                      {fmtDateTime(r.scheduled_at)}
                    </span>
                  ) : (
                    <span className="faint">—</span>
                  ),
              },
            ]}
          />
        </Panel>
      </QueryState>
    </>
  );
}
