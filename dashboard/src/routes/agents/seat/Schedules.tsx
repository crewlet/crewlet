/**
 * The recurring work that wakes a seat (its own schedules and every unit
 * schedule whose fire reaches it) from the resolved rows every reader is
 * pushed (`profile.ts`'s `seatSchedules`), each opening its peek.
 *
 * THE COMPANY'S COLUMNS, for the same rows: a schedule reads the same here as
 * on the company's Schedules, with its last fire read from the same history.
 */

import { useMemo } from "react";
import { Card, EmptyState, peekClickHandler, type DataGridActivationEvent } from "@crewlethq/ui";
import { CalendarGlyph } from "@crewlethq/icons/glyphs";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { useSchedules } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { seatLookup, type OrgIndex, type Seat } from "~/lib/seats.ts";
import { lastFires, scheduleColumns, scheduleRef } from "../Schedules.tsx";
import { seatSchedules } from "./profile.ts";

export function Schedules({ seat, index, now }: { seat: Seat; index: OrgIndex; now: number }) {
  const pushed = useSchedules();
  const rows = useMemo(() => seatSchedules(pushed, seat.handle), [pushed, seat.handle]);
  const who = useMemo(() => seatLookup(index), [index]);
  // THE FIRES ARE NOT PUSHED: the company's history, polled as its own screen
  // polls it, and asked only while there is a schedule to read it for.
  const history = useQuery("schedules", undefined, { enabled: rows.length > 0, pollMs: 30_000 });
  const runs = history.data?.recent_runs;
  const lastFire = useMemo(() => (runs ? lastFires(runs) : undefined), [runs]);
  const { open: openPeek } = usePeekControls();
  if (rows.length === 0) {
    return (
      <EmptyState
        size="compact"
        icon={<CalendarGlyph size={28} />}
        title="Nothing recurring reaches them"
        description="No schedule includes this seat."
      />
    );
  }
  return (
    <Card padding="none">
      <Card.Header
        icon={<CalendarGlyph size="sm" />}
        count={rows.length}
        subtitle="Their own and their team’s"
      >
        <Card.Title as="h3">Recurring work</Card.Title>
      </Card.Header>
      <DataGrid
        name="prof-schedules"
        rows={rows}
        rowKey={(row) => [row.scope_type, row.scope_id, row.name].join("/")}
        rowHref={(row) => peekHref(scheduleRef(row.scope_type, row.scope_id, row.name))}
        // A PLAIN PRESS OPENS THE PEEK BESIDE THE PAGE, as the company's grid
        // does; a modifier press is the browser's, on the row's real link.
        onRowActivate={(row, e: DataGridActivationEvent) => {
          const go = () => openPeek(scheduleRef(row.scope_type, row.scope_id, row.name));
          if (!("button" in e)) {
            go();
            return;
          }
          peekClickHandler(go)?.(e);
        }}
        defaultSort="next"
        columns={scheduleColumns({ now, who, lastFire })}
      />
    </Card>
  );
}
