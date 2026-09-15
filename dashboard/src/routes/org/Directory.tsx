/**
 * The Directory lens: every seat as a sortable row, with the reporting line
 * the engine derived.
 *
 * "Reports to" and "Manages" are the engine's answers, and an engine that did
 * not report its hierarchy gets cells saying so rather than a zero: nobody
 * reporting to a seat is a measurement, and a count this screen could not
 * take is not one.
 */

import { useParam } from "~/app/router.tsx";
import { SeatChip, StateBadge } from "~/components/common.tsx";
import { statusLine, type OrgIndex, type Seat } from "~/lib/seats.ts";
import { useAgents, useSandboxes } from "~/lib/store-hooks.ts";
import { DataTable } from "~/ui/DataTable.tsx";
import { Badge } from "~/ui/primitives.tsx";
import { Card } from "@crewlethq/ui";

export function Directory({ index }: { index: OrgIndex }) {
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const [unitName] = useParam("unit", "", "filter");
  const [seatHandle] = useParam("seat", "", "filter");
  const seatFor = (seat: Seat) => agents.find((a) => a.role === seat.name);
  const unknown = <span className="muted">Not reported</span>;

  return (
    <Card padding="none">
      <DataTable<Seat>
        rows={index.seats}
        rowKey={(s) => s.key}
        defaultSort={{ key: "name", dir: "asc" }}
        // The selection a link carries marks its rows here too, so switching
        // lens does not lose what the reader came to look at.
        isSelected={(s) =>
          (seatHandle !== "" && s.handle === seatHandle) ||
          (unitName !== "" && s.unit?.name === unitName)
        }
        empty={{ title: "No seats", hint: "Roles come from the company configuration." }}
        columns={[
          {
            key: "name",
            header: "Seat",
            sortValue: (s) => s.name,
            cell: (s) => <SeatChip name={s.name} handle={s.handle} human={s.kind === "human"} />,
          },
          {
            key: "handle",
            header: "Handle",
            sortValue: (s) => s.handle,
            shrink: true,
            cell: (s) => (s.handle ? <code className="inline">@{s.handle}</code> : unknown),
          },
          {
            key: "unit",
            header: "Unit",
            sortValue: (s) => s.unit?.name ?? "",
            cell: (s) => s.unit?.name ?? <span className="muted">org-wide</span>,
          },
          {
            key: "manager",
            header: "Reports to",
            sortValue: (s) => s.manager?.name ?? "",
            cell: (s) =>
              s.manager ? (
                <SeatChip name={s.manager.name} handle={s.manager.handle} />
              ) : index.hierarchy ? (
                <span className="muted">Nobody</span>
              ) : (
                unknown
              ),
          },
          {
            key: "reports",
            header: "Manages",
            align: "right",
            sortValue: (s) => (index.hierarchy ? s.reports.length : -1),
            cell: (s) => (index.hierarchy ? s.reports.length : unknown),
          },
          {
            key: "state",
            header: "State",
            shrink: true,
            sortValue: (s) => (s.kind === "human" ? "human" : (seatFor(s)?.state ?? "offline")),
            cell: (s) =>
              s.kind === "human" ? (
                <Badge outline>human</Badge>
              ) : (
                <StateBadge agent={seatFor(s)} sandboxes={sandboxes} />
              ),
          },
          {
            key: "doing",
            header: "Doing",
            cell: (s) => (
              <span className="truncate t-caption">
                {statusLine(seatFor(s), {
                  seat: s,
                  sandbox: sandboxes.find((b) => b.role === s.name) ?? null,
                })}
              </span>
            ),
          },
        ]}
      />
    </Card>
  );
}
