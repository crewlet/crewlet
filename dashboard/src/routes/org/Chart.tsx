/**
 * The Chart lens: the company's units nested as they are, with the seats in
 * each.
 *
 * Drawn as nested units rather than as a centred graph: the hierarchy nests to
 * any depth by design, and a centred layout at depth four is a horizontal
 * scroll nobody reads.
 *
 * EVERY PLACEMENT HERE IS THE ENGINE'S. A root seat whose `unit:` reference
 * moved it sits inside that unit, marked as placed by reference; a unit with no
 * lead of its own shows the lead it inherits, marked as inherited. Both come
 * from the projection's derived block, and an engine that sends none gets a
 * chart of the document as written with a note saying what it cannot show.
 *
 * `unit=` and `seat=` in the URL name a selection. They are FILTERS in the
 * router's sense (a selection replaces the entry, it is not a screen the
 * reader called) and a link carrying one reveals and rings that unit or seat
 * on arrival, so the command palette's "Units" results and the seat screen's
 * way back into the chart land on what they named rather than at the top of
 * a long page.
 */

import { href, useParam, useRevealOnArrival } from "~/app/router.tsx";
import { StateBadge } from "~/components/common.tsx";
import { plural } from "~/lib/format.ts";
import { seatPath, type OrgIndex, type Seat, type Unit } from "~/lib/seats.ts";
import { useAgents, useSandboxes } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { AgentRow, SandboxEntry } from "~/protocol/index.ts";
import { Icon } from "~/ui/Icon.tsx";
import { Avatar, Badge, Banner, ButtonLink, Empty, Panel, cx } from "~/ui/primitives.tsx";

/** The DOM id a unit block carries, which is what a reveal scrolls to. */
export const unitElementId = (unit: Unit) => `org-unit-${unit.key}`;
/** The DOM id a seat node carries. */
export const seatElementId = (seat: Seat) => `org-seat-${seat.key}`;

interface Live {
  agents: AgentRow[];
  sandboxes: SandboxEntry[];
}

interface Selection {
  unit: Unit | null;
  seat: Seat | null;
  onUnit: (unit: Unit) => void;
}

export function Chart({ index }: { index: OrgIndex }) {
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const [unitName, setUnitName] = useParam("unit", "", "filter");
  const [seatHandle] = useParam("seat", "", "filter");
  // Asked only when there is nothing to draw, to decide which empty state is
  // true: "nothing is configured" and "the configuration has no seats" are
  // different problems with different ways out.
  const empty = !index.units.length && !index.seats.length;
  const engine = useQuery("stream", undefined, { enabled: empty });

  const selection: Selection = {
    unit: unitName ? (index.units.find((u) => u.name === unitName) ?? null) : null,
    seat: seatHandle ? (index.byHandle.get(seatHandle) ?? null) : null,
    // Selecting the selected unit clears it, so the name is a toggle rather
    // than a way in that has no way out.
    onUnit: (unit) => setUnitName(unit.name === unitName ? "" : unit.name),
  };
  useRevealOnArrival(
    selection.seat
      ? seatElementId(selection.seat)
      : selection.unit
        ? unitElementId(selection.unit)
        : null,
  );

  const live: Live = { agents, sandboxes };

  if (empty) {
    const unconfigured = engine.data?.configured === false;
    return (
      <Empty
        icon="sitemap"
        title="No organization is loaded"
        hint={
          unconfigured
            ? "No company configuration is active on this engine, so there is no organization to draw."
            : "The org tree comes from the active company configuration."
        }
        action={
          unconfigured ? (
            <ButtonLink variant="primary" href={href(["org"], { lens: "builder" })}>
              Create the company
            </ButtonLink>
          ) : undefined
        }
      />
    );
  }

  return (
    <>
      {!index.hierarchy && (
        <Banner tone="neutral" icon="info">
          This engine did not report the hierarchy it derived, so seats are drawn where the document
          places them. Inherited leads and seats moved into a unit by their{" "}
          <code className="inline">unit</code> reference are not shown.
        </Banner>
      )}
      {index.rootSeats.length > 0 && (
        <Panel title="Org-wide" icon="crown" subtitle="seats above every unit">
          <div className="org-seats">
            {index.rootSeats.map((seat) => (
              <SeatNode key={seat.key} seat={seat} live={live} selected={selection.seat === seat} />
            ))}
          </div>
        </Panel>
      )}
      <div className="org-tree">
        {index.topUnits.map((unit) => (
          <UnitBlock key={unit.key} unit={unit} live={live} selection={selection} />
        ))}
      </div>
    </>
  );
}

/**
 * One unit and everything beneath it.
 *
 * AT MODULE SCOPE. It was declared inside the screen's own render, which made
 * it a new component type on every render: React unmounted and rebuilt the
 * whole tree on each `agents` push, twice per tool-loop round, which is also
 * what would have thrown away focus and any selection a reader had made.
 */
function UnitBlock({ unit, live, selection }: { unit: Unit; live: Live; selection: Selection }) {
  const selected = selection.unit === unit;
  return (
    <div id={unitElementId(unit)} className={cx("org-unit", selected && "selected")}>
      <div className="org-unit-head">
        <Icon name="folder" size="sm" style={{ color: "var(--color-text-muted)" }} />
        <button
          type="button"
          className="org-unit-name t-body truncate"
          aria-pressed={selected}
          title={selected ? "Clear the selection" : "Select this unit"}
          onClick={() => selection.onUnit(unit)}
        >
          {unit.name}
        </button>
        <Badge outline>{unit.type || "unit"}</Badge>
        {/* A unit with no lead of its own inherits the nearest ancestor's, and
            the chart says which it is: an inherited lead behaves identically
            to an explicit one everywhere in the engine, and hiding the
            difference is how an operator comes to think a unit is unmanaged. */}
        {unit.lead && (
          <Badge
            tone="neutral"
            icon="crown"
            title={unit.leadInherited ? "Inherited from a parent unit" : "This unit's own lead"}
          >
            {unit.lead.name}
            {unit.leadInherited && <span className="faint"> (inherited)</span>}
          </Badge>
        )}
        <span className="spacer" />
        <span className="t-caption">{plural(unit.seats.length, "seat")}</span>
      </div>
      {unit.purpose && <div className="t-caption measure">{unit.purpose}</div>}
      {unit.seats.length > 0 && (
        <div className="org-seats" style={{ marginTop: "var(--spacing-2)" }}>
          {unit.seats.map((seat) => (
            <SeatNode key={seat.key} seat={seat} live={live} selected={selection.seat === seat} />
          ))}
        </div>
      )}
      {unit.children.length > 0 && (
        <div className="org-children">
          {unit.children.map((child) => (
            <UnitBlock key={child.key} unit={child} live={live} selection={selection} />
          ))}
        </div>
      )}
    </div>
  );
}

function SeatNode({ seat, live, selected }: { seat: Seat; live: Live; selected: boolean }) {
  const human = seat.kind === "human";
  return (
    <a
      id={seatElementId(seat)}
      className={cx("org-node", human && "human", selected && "selected")}
      href={href(seatPath(seat))}
      aria-current={selected ? "true" : undefined}
    >
      <Avatar name={seat.name} human={human} />
      <span className="col" style={{ gap: 0, minWidth: 0, flex: 1 }}>
        <span className="truncate t-cell">{seat.name}</span>
        {seat.handle && <span className="truncate t-caption mono">@{seat.handle}</span>}
        {seat.placedByRef && <span className="truncate t-caption">Placed by unit reference</span>}
      </span>
      {human ? (
        <Badge outline>human</Badge>
      ) : (
        <StateBadge
          agent={live.agents.find((a) => a.role === seat.name)}
          sandboxes={live.sandboxes}
        />
      )}
    </a>
  );
}
