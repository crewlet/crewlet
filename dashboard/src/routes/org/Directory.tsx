/**
 * The Directory lens: every seat as a row, with the reporting line the engine
 * derived.
 *
 * It is a LIST SCREEN, so it is drawn as one: a toolbar with the search box
 * and the Filter and Sort menus, a chip for each axis a reader narrowed, the
 * rows, and a footer saying how many of how many are on screen. The head is
 * the Org screen's own, which names the lens.
 *
 * "Reports to" and "Manages" are the engine's answers, and an engine that did
 * not report its hierarchy gets cells saying so rather than a zero: nobody
 * reporting to a seat is a measurement, and a count this screen could not
 * take is not one.
 */

import { useCallback, useMemo } from "react";
import { useParam } from "~/app/router.tsx";
import {
  MATCH_FOOTER_LABELS,
  SeatChip,
  StateBadge,
  useTableChoices,
} from "~/components/common.tsx";
import { statusLine, type OrgIndex, type Seat } from "~/lib/seats.ts";
import { useAgents, useSandboxes } from "~/lib/store-hooks.ts";
import {
  DataView,
  EmptyState,
  Tag,
  type DataViewColumn,
  type FilterDef,
  type FilterValues,
} from "@crewlethq/ui";

export function Directory({ index }: { index: OrgIndex }) {
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const [unitName] = useParam("unit", "", "filter");
  const [seatHandle] = useParam("seat", "", "filter");
  const [q, setQ] = useParam("q", "");
  const [kind, setKind] = useParam("kind", "");
  const seatFor = useCallback((seat: Seat) => agents.find((a) => a.role === seat.name), [agents]);

  /*
   * THE LENS OWNS THE NARROWING, so both axes are URL parameters and a
   * narrowed directory is a link. `unit` and `seat` are not axes: they are the
   * SELECTION a link carries between lenses, and they mark rows rather than
   * hiding them.
   */
  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return index.seats
      .filter((s) => !kind || s.kind === kind)
      .filter(
        (s) =>
          !needle ||
          s.name.toLowerCase().includes(needle) ||
          (s.handle ?? "").toLowerCase().includes(needle) ||
          (s.unit?.name ?? "").toLowerCase().includes(needle),
      );
  }, [index.seats, q, kind]);

  const filters = useMemo<FilterDef<Seat>[]>(
    () => [
      { name: "q", label: "Search seats", role: "search", placeholder: "Name, handle or unit" },
      {
        name: "kind",
        label: "Kind",
        kind: "select",
        options: [
          { value: "", label: "Any kind" },
          { value: "agent", label: "Agent" },
          { value: "human", label: "Human" },
        ],
      },
    ],
    [],
  );

  const values: FilterValues = { q, kind };

  const onValuesChange = useCallback(
    (next: FilterValues) => {
      setQ(String(next.q ?? ""));
      setKind(String(next.kind ?? ""));
    },
    [setQ, setKind],
  );

  /*
   * The words, not a dash. "Not reported" is a fact about the ENGINE, not
   * about the seat, and a reader scanning the column has to be able to see the
   * difference between "nobody reports to this seat" and "this engine did not
   * say" without hovering anything.
   */
  const unknown = <span className="muted">Not reported</span>;

  const columns = useMemo<DataViewColumn<Seat>[]>(
    () => [
      {
        key: "name",
        header: "Seat",
        sortable: true,
        // WHO. A unit, a manager and a status belonging to nobody named is a
        // directory of nobody.
        hideable: false,
        sortValue: (s) => s.name,
        render: (s) => <SeatChip name={s.name} handle={s.handle} human={s.kind === "human"} />,
      },
      {
        key: "handle",
        header: "Handle",
        sortable: true,
        shrink: true,
        mono: true,
        copyable: true,
        sortValue: (s) => s.handle,
        render: (s) => (s.handle ? `@${s.handle}` : unknown),
      },
      {
        key: "unit",
        header: "Unit",
        sortable: true,
        sortValue: (s) => s.unit?.name ?? "",
        render: (s) => s.unit?.name ?? <span className="muted">org-wide</span>,
      },
      {
        key: "manager",
        header: "Reports to",
        sortable: true,
        sortValue: (s) => s.manager?.name ?? "",
        render: (s) =>
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
        sortable: true,
        firstDirection: "desc",
        // A count this engine could not take is ABSENT rather than zero, and
        // the table's own comparator puts an absent value last whichever way
        // the reader sorts.
        sortValue: (s) => (index.hierarchy ? s.reports.length : null),
        render: (s) => (index.hierarchy ? s.reports.length : unknown),
      },
      {
        key: "state",
        header: "State",
        shrink: true,
        sortable: true,
        sortValue: (s) => (s.kind === "human" ? "human" : (seatFor(s)?.state ?? "offline")),
        render: (s) =>
          s.kind === "human" ? (
            <Tag appearance="outline">human</Tag>
          ) : (
            <StateBadge agent={seatFor(s)} sandboxes={sandboxes} />
          ),
      },
      {
        key: "doing",
        header: "Doing",
        render: (s) => (
          <span className="truncate t-caption">
            {statusLine(seatFor(s), {
              seat: s,
              sandbox: sandboxes.find((b) => b.role === s.name) ?? null,
            })}
          </span>
        ),
      },
    ],
    [index.hierarchy, seatFor, sandboxes, unknown],
  );

  /*
   * The directory pages, and its choices are named after it rather than after
   * the screen: the lens shares its URL with the chart and the builder, so a
   * bare `page` on the org screen would read as the chart's.
   */
  const choices = useTableChoices({
    screen: "org",
    table: "directory",
    columns,
    defaultSort: { key: "name", direction: "asc" },
    filterKey: `${q}|${kind}`,
  });

  return (
    <DataView<Seat>
      framed
      {...choices}
      columns={columns}
      rows={shown}
      totalCount={index.seats.length}
      labels={{ footer: MATCH_FOOTER_LABELS }}
      getRowKey={(s) => s.key}
      filters={filters}
      filterValues={values}
      onFilterValuesChange={onValuesChange}
      // The selection a link carries marks its rows here too, so switching
      // lens does not lose what the reader came to look at.
      isSelected={(s) =>
        (seatHandle !== "" && s.handle === seatHandle) ||
        (unitName !== "" && s.unit?.name === unitName)
      }
      emptyMessage={
        <EmptyState
          size="compact"
          title={index.seats.length ? "No seat matches these filters" : "No seats"}
          description={
            index.seats.length
              ? "Clear them to see every seat the company declares."
              : "Roles come from the company configuration, so a company with none declared has no seats to draw."
          }
        />
      }
    />
  );
}
