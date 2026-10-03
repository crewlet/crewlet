/**
 * The recurring work that wakes a seat — its own schedules and every unit
 * schedule whose fire reaches it — from the resolved rows every reader is
 * pushed (`profile.ts`'s `seatSchedules`), each opening its peek.
 */

import { useMemo } from "react";
import { Card, EmptyState, EmptyValue, Tag } from "@crewlethq/ui";
import { CalendarGlyph } from "@crewlethq/icons/glyphs";
import { peekHref } from "~/app/frame/DetailRail.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { TextCell } from "~/app/frame/cells.tsx";
import { describe as describeCron } from "~/lib/cron.ts";
import { fmtDateTime, inTime, tsKey } from "~/lib/format.ts";
import { useSchedules } from "~/lib/store-hooks.ts";
import type { Seat } from "~/lib/seats.ts";
import { seatSchedules } from "./profile.ts";

export function Schedules({ seat, now }: { seat: Seat; now: number }) {
  const pushed = useSchedules();
  const rows = useMemo(() => seatSchedules(pushed, seat.handle), [pushed, seat.handle]);
  if (rows.length === 0) {
    return (
      <EmptyState
        size="compact"
        icon={<CalendarGlyph size={28} />}
        title="Nothing recurring reaches them"
        description="A schedule wakes a seat on a cron, scoped to a role or to a unit. This seat declares none and is named by none."
      />
    );
  }
  return (
    <Card padding="none">
      <Card.Header
        icon={<CalendarGlyph size="sm" />}
        count={rows.length}
        subtitle="Their own, and every unit schedule a fire actually reaches them through."
      >
        <Card.Title as="h3">Recurring work</Card.Title>
      </Card.Header>
      <DataGrid
        name="prof-schedules"
        rows={rows}
        rowKey={(row) => [row.scope_type, row.scope_id, row.name].join("/")}
        rowHref={(row) =>
          peekHref({ kind: "schedule", id: [row.scope_type, row.scope_id, row.name].join("/") })
        }
        columns={[
          {
            key: "name",
            header: "Name",
            cell: (row) => <TextCell icon="calendar">{row.name}</TextCell>,
            sortValue: (row) => row.name,
          },
          {
            // A FIVE-FIELD SCHEDULE, not an identifier: the code face, with
            // the words it means beside it.
            key: "cron",
            header: "When",
            cell: (row) => (
              <span className="row gap-1">
                <code className="inline nowrap">{row.cron}</code>
                {describeCron(row.cron) && (
                  <span className="muted truncate">{describeCron(row.cron)}</span>
                )}
              </span>
            ),
          },
          {
            // WHOSE SCHEDULE IT IS: a unit schedule is not this seat's and
            // still lands in its day.
            key: "scope",
            header: "Scope",
            shrink: true,
            cell: (row) =>
              row.scope_type === "role" ? (
                <span className="muted">theirs</span>
              ) : (
                <TextCell icon="building-complex">{row.scope_id}</TextCell>
              ),
            sortValue: (row) => `${row.scope_type}/${row.scope_id}`,
          },
          {
            // THE ENGINE'S OWN ANSWER, and the REASON where it has none: a
            // disabled schedule and one whose zone was renamed both have no
            // `next_run`, and only the second is a defect.
            key: "next",
            header: "Next",
            shrink: true,
            sortValue: (row) => tsKey(row.next_run) || Number.MAX_SAFE_INTEGER,
            cell: (row) =>
              row.problem ? (
                <Tag variant="danger" title={row.problem}>
                  cannot fire
                </Tag>
              ) : !row.enabled ? (
                <Tag appearance="outline">disabled</Tag>
              ) : row.next_run ? (
                // FORWARD, not `DateCell`: `relTime` spells a future instant
                // as an elapsed one.
                <span className="t-caption" title={fmtDateTime(row.next_run)}>
                  {inTime(row.next_run, now)}
                </span>
              ) : (
                <EmptyValue label="The calendar never reaches it" />
              ),
          },
          {
            key: "task",
            header: "Task",
            cell: (row) => <TextCell>{row.task}</TextCell>,
          },
        ]}
      />
    </Card>
  );
}
