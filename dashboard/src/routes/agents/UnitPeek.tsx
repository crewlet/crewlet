/**
 * One unit's panel, over Structure's Table: what the unit is for, who leads
 * it, its seats and the units under it. A unit has no page of its own; the
 * Table is where its seats are watched and controlled, and this panel is what
 * a link to the unit opens there (`lib/seats.ts`'s `unitPlace`).
 *
 * It reads the ANONYMOUS org projection this node has APPLIED, so between a
 * builder save and this node applying it, it has not moved.
 */

import { useMemo, type CSSProperties } from "react";
import { href } from "~/app/router.tsx";
import { StateBadge } from "~/components/common.tsx";
import { Card, EmptyState, Skeleton } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import {
  NetworkGlyph,
  ArrowRightGlyph,
  FolderGlyph,
  UsersGlyph,
  TargetGlyph,
} from "@crewlethq/icons/glyphs";
import { useAgents, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { AgentRow } from "~/protocol/types.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { projectsByUnit } from "~/lib/orgchart.ts";
import { PROJECT_PAGE } from "~/contract/work.ts";
import {
  activityOf,
  handleLabel,
  indexOrg,
  liveRowFor,
  seatPath,
  unitByKey,
  unitTally,
  UNIT_TOTAL_HINT,
  type OrgIndex,
  type Seat,
  type Unit,
} from "~/lib/seats.ts";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { peekHref } from "~/app/frame/peekAddress.ts";

/**
 * Every live project filed to each unit, by unit key, from Worklet. A
 * company on another tracker answers nothing, and a unit then carries no key
 * rather than a wrong one, as it does for a reader without Worklet, who is
 * not asked.
 */
function useUnitProjects(): Map<string, string[]> {
  const { worklet } = useViewer();
  const projects = useQuery(
    "work_projects",
    { limit: PROJECT_PAGE },
    { pollMs: 120_000, enabled: worklet },
  );
  const rows = projects.error ? undefined : projects.data?.projects;
  return useMemo(() => projectsByUnit(rows ?? []), [rows]);
}

/** A unit's project keys, as chips that open each project's board. */
function ProjectKeys({ keys }: { keys: readonly string[] }) {
  return (
    <>
      {keys.map((key) => (
        <a
          key={key}
          className="unit-key"
          href={href(["worklet", key])}
          title={`The ${key} project`}
        >
          {key}
        </a>
      ))}
    </>
  );
}

/**
 * A unit's seats, as rows.
 *
 * ONE COPY. This block is drawn in four places between this screen, the unit
 * page and the unit peek, the chart's units, the seats above every unit, the
 * page's roster and the rail's, and it renders a seat's STATE: whether a node
 * is running it, whether it is waiting on a person, whether it broke. Four
 * hand-written copies of that is four chances for one of them to keep drawing
 * a healthy badge over a seat nothing is running.
 *
 * It reads the agent and sandbox slices itself rather than taking them as
 * props: every caller was subscribing to both for this and for nothing else,
 * and a subscription per row-block is what the store's slice keys are for.
 */
/**
 * A seat chip's state, only where it is news. Idle is where a seat rests, and
 * a pill reading it on every chip of a team was a column a reader learned to
 * skip; working, waiting on a person and stopped are what a reader scans a
 * team for. The word stays for a screen reader either way.
 */
function SeatState({ agent }: { agent: AgentRow | undefined }) {
  return activityOf(agent) === "idle" ? (
    <span className="sr-only">Idle</span>
  ) : (
    <StateBadge agent={agent} />
  );
}

function SeatLinks({ seats, style }: { seats: Seat[]; style?: CSSProperties }) {
  const agents = useAgents();
  return (
    <div className="org-seats" style={style}>
      {seats.map((seat) => (
        <a key={seat.key} className="org-node" href={href(seatPath(seat))}>
          {/* THE KIND IS THE OUTLINE: a person is the kit's circle and an
              agent its squircle, the one cue that tells them apart. It is
              structure, not status, so it is drawn rather than tinted. The
              kit's `md` is 32px where ours was 26, so the step moves down
              one. */}
          <SeatAvatar
            name={seat.name}
            size="sm"
            kind={seat.kind === "human" ? "human" : "agent"}
            avatar={seat.avatar}
            decorative
            title={seat.name}
          />
          <span className="col" style={{ gap: 0, minWidth: 0, flex: 1 }}>
            <span className="truncate t-cell">{seat.name}</span>
            {/* THE HANDLE THE ENGINE DERIVED, never one worked out here: that
                would be the second implementation `lib/seats.ts` exists to
                have removed. */}
            <span className="truncate t-caption mono">{handleLabel(seat.handle)}</span>
          </span>
          {seat.kind === "human" ? (
            // THE CIRCLE SAYS IT. A tag reading "human" beside a person's
            // circle was the outline said twice; the word stays for a
            // screen reader, which does not see the outline.
            <span className="sr-only">human</span>
          ) : (
            <SeatState agent={liveRowFor(agents, seat)} />
          )}
        </a>
      ))}
    </div>
  );
}

/**
 * The units under one unit, as rows.
 *
 * A LINK OUT, on the page and in the rail alike. The rail holds one object at
 * a time and `[` and `]` step the list it was opened FROM, so a sub-unit
 * swapped into it in place of its parent would leave the stepper walking a
 * list this object is not in, the row goes to the unit's own page instead,
 * where its seats and its own children are the point rather than a preview.
 */
function SubUnitLinks({ units }: { units: Unit[] }) {
  return (
    <div className="list">
      {units.map((child) => (
        <a key={child.key} className="thread-entry" href={peekHref({ kind: "unit", id: child.id })}>
          <FolderGlyph size="sm" />
          <span className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
            <strong className="t-cell truncate">{child.name}</strong>
            {child.purpose && <span className="t-caption truncate">{child.purpose}</span>}
          </span>
          {/* The kind is a fact, quiet, as on the Teams tree. */}
          <span className="t-caption muted">{child.type || "unit"}</span>
          <ArrowRightGlyph size="sm" />
        </a>
      ))}
    </div>
  );
}

/**
 * What a unit IS, the five facts, and the roster two of them are counted from.
 *
 * THE PAGE AND THE PEEK READ THE SAME FUNCTION, which is the only thing that
 * keeps them from drifting: "Seats" and "Directly in it" are two different
 * questions about the same tree, everything under the unit, against what sits
 * immediately in it, and two hand-written fact lists would eventually answer
 * them the other way round in one of the two frames.
 */
function unitView(
  index: OrgIndex,
  unit: Unit,
  projects: ReadonlyMap<string, readonly string[]>,
): { seats: Seat[]; facts: Fact[] } {
  const seats = index.seats.filter((s) => s.unitChain.some((u) => u === unit));
  const tally = unitTally(unit);
  // THE EFFECTIVE LEAD, which is the nearest ancestor's where this unit
  // declares none. It behaves identically everywhere in the engine, and hiding
  // the difference is how somebody concludes a team is unmanaged. The ENGINE
  // resolves it: inheritance cascades to any depth and a client that walked
  // the chain itself would be the second implementation of that rule.
  const lead = unit.effectiveLead;
  return {
    seats,
    facts: [
      { label: "Type", value: unit.type || "unit" },
      {
        label: "Reports to",
        value: lead ? (
          <>
            {lead.name}
            {unit.leadInherited && <span className="muted"> (inherited)</span>}
          </>
        ) : (
          ""
        ),
        path: lead ? seatPath(lead) : undefined,
      },
      // ALL THREE OFF ONE TALLY. They were three separate expressions over two
      // different trees, `seats.length` is a filter over every seat in the
      // company, `unit.seats.length` is the unit's own list, and the rail and
      // the org chart each computed a fourth and a fifth somewhere else. What
      // made that a defect rather than duplication is that they DISAGREED in
      // public: "Leadership 5" in the rail, "2 seats" on the chart block, a
      // third figure on the roster, none of them saying which question it was
      // the answer to.
      { label: "Seats", value: tally.total, note: UNIT_TOTAL_HINT },
      { label: "Directly in it", value: tally.direct },
      { label: "Sub-units", value: tally.subUnits },
      // WHERE ITS WORK IS FILED, from the tracker, absent rather than "none"
      // on a company whose tracker is not this engine's.
      ...(projects.get(unit.id)?.length
        ? [{ label: "Project", value: <ProjectKeys keys={projects.get(unit.id)!} /> }]
        : []),
    ],
  };
}

/** The hint under every "no such unit", on the page and in the rail alike. */
const NO_UNIT_HINT =
  "Units are linked by key, which a rename keeps. It may have been removed, or the link used its name.";

/** And what an empty one costs, which is the part a reader acts on. */
const NO_SEATS_HINT = "Work filed to it goes to its lead, if it has one.";

/**
 * One unit, in the rail.
 *
 * # Why this reads the pushed tree and not a query of its own
 *
 * Every other peek asks the engine for its object, because a row's copy is
 * whatever its own list needed. A unit has no such answer to ask for: the org
 * tree arrives whole in the connect handshake and is REPLACED whole whenever
 * the company configuration activates, so it is already present before any
 * list is, which is the property the "fetch your own object" rule is actually
 * buying, and a peek opened from a pasted URL therefore renders with nothing
 * else on screen. The `config` query is the same document pulled instead of
 * pushed, and reading it here would give the page and the rail two sources for
 * one set of facts, which is exactly the drift [unitView] exists to prevent.
 *
 * # Two absences, and only one of them is honest as "no such unit"
 *
 * The store starts with an EMPTY tree rather than a null one, so "the
 * handshake has not landed" and "this company has no units" are the same
 * value. The connection is what tells them apart: until it is up, an unknown
 * key is a tree that has not arrived and the rail says so with a skeleton
 * rather than claiming a unit does not exist.
 */
export function UnitPeek({ id }: { id: string }) {
  const org = useOrg();
  const projects = useUnitProjects();
  const { connected } = useConnection();
  const index = useMemo(() => indexOrg(org), [org]);
  const unit = unitByKey(index, id);

  if (!unit) {
    if (!connected) return <Skeleton variant="text" rows={6} label="Loading the org tree" />;
    return (
      <EmptyState
        size="compact"
        icon={<NetworkGlyph size={32} />}
        title={`No unit has the key “${id}”`}
        description={NO_UNIT_HINT}
      />
    );
  }

  const { seats, facts } = unitView(index, unit, projects);
  const children = unit.children;

  return (
    <>
      <ObjectHeader size="peek" kind="Unit" icon="network" title={unit.name} facts={facts} />
      <div className="col gap-3">
        {/* WHAT IT IS FOR, always drawn, including when nobody wrote one.
            "Is this the one I meant" is the question the rail answers, and a
            purpose that is simply missing from the panel reads as a unit
            whose purpose the reader failed to scroll to. */}
        <Card>
          <Card.Header icon={<TargetGlyph size="sm" />}>
            <Card.Title>Purpose</Card.Title>
          </Card.Header>
          {unit.purpose ? (
            <p className="t-body measure">{unit.purpose}</p>
          ) : (
            <span className="muted">No purpose is written for this unit.</span>
          )}
        </Card>

        {/* THE SEATS THEMSELVES, not just the count in the facts above: a
            reader recognises a team by who is in it long before they
            recognise it by its name, and the badge on each row is the other
            half of "what is it doing". */}
        <Card padding="none">
          <Card.Header icon={<UsersGlyph size="sm" />} count={seats.length}>
            <Card.Title>Seats</Card.Title>
          </Card.Header>
          {seats.length > 0 ? (
            <SeatLinks seats={seats} style={{ padding: "var(--spacing-3)" }} />
          ) : (
            <EmptyState
              size="compact"
              icon={<UsersGlyph size={32} />}
              title="No seats in this unit"
              description={NO_SEATS_HINT}
            />
          )}
        </Card>

        {children.length > 0 && (
          <Card padding="none">
            <Card.Header icon={<NetworkGlyph size="sm" />} count={children.length}>
              <Card.Title>Sub-units</Card.Title>
            </Card.Header>
            <SubUnitLinks units={children} />
          </Card>
        )}
      </div>
    </>
  );
}
