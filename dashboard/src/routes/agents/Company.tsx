/**
 * The company's units: Agents › Teams, one unit's page and one unit's peek.
 *
 * # Teams is the tree of units; the chart is the tree of people
 *
 * `#/agents` (`OrgChart.tsx`) draws who reports to whom, one card per seat.
 * `#/agents/teams` draws the UNITS — what each is for, what it was told to do
 * (its goals, which moved here from the charter: a unit's goals are the
 * unit's, and filed under the company's settings they read as company
 * policy), who leads it, which project its work is filed under, and the seats
 * in it — nested as the document nests them, because a unit holds units to
 * any depth and a flat grid of cards lost which team a team was in.
 *
 * Both read the `state:read` org projection this node has APPLIED, which is why
 * [PreviousRevisionNote] is drawn on both: between a builder save and this
 * node applying it, neither has moved.
 *
 * # A unit is an object with a page
 *
 * `UnitScreen` is that page, at `#/agents/teams/{unit}`. It existed as a query
 * key on the org screen (`#/org?unit=`), which meant a team could not be
 * linked to, could not carry its own tabs, and was one filter away from being
 * lost.
 *
 * # Not reported is not nothing
 *
 * An engine that sends no derived hierarchy cannot say who inherits a lead,
 * so a unit declaring none has an UNKNOWN lead rather than none — every place
 * a lead is drawn says "not reported by this engine" in that case rather than
 * leaving a blank that reads as an unmanaged team.
 */

import { useEffect, useMemo, type CSSProperties } from "react";
import { href, useNavigator, useRoute } from "~/app/router.tsx";
import { usePeekControls } from "~/app/frame/DetailRail.tsx";
import { StateBadge } from "~/components/common.tsx";
import { Card, EmptyState, EmptyValue, Skeleton, Tag } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import {
  NetworkGlyph,
  ArrowRightGlyph,
  CrownGlyph,
  FlagGlyph,
  FolderGlyph,
  UsersGlyph,
  TargetGlyph,
} from "@crewlethq/icons/glyphs";
import { useAgents, useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { projectsByUnit } from "~/lib/orgchart.ts";
import { projectPath } from "~/lib/work.ts";
import {
  handleLabel,
  indexOrg,
  liveRowFor,
  seatPath,
  unitByKey,
  unitPath,
  unitSeatsLabel,
  unitTally,
  UNIT_TOTAL_HINT,
  type OrgIndex,
  type Seat,
  type Unit,
} from "~/lib/seats.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { PreviousRevisionNote } from "~/routes/org/builder/AfterSaveStrip.tsx";
import { AgentsHeader, useAgentsCounts } from "./header.tsx";

/** The project page the unit keys are read from: every live project. */
const PROJECT_PAGE = 200;

/** A lead this engine did not report: never drawn as "no lead". */
export const LEAD_NOT_REPORTED = "Lead not reported by this engine";

/**
 * Every live project filed to each unit of `index`, from the tracker — keyed
 * on the unit itself, resolved through every key it has answered to
 * ([projectsByUnit]), so a renamed team keeps the projects filed before its
 * rename and two teams sharing a name keep their own. A company on another
 * tracker answers nothing, and a unit then carries no key rather than a wrong
 * one.
 */
function useUnitProjects(index: OrgIndex): Map<Unit, string[]> {
  const projects = useQuery("work_projects", { limit: PROJECT_PAGE }, { pollMs: 120_000 });
  const rows = projects.error ? undefined : projects.data?.projects;
  return useMemo(() => projectsByUnit(rows ?? [], index), [rows, index]);
}

/** A unit's project keys, as the chips the chart's boxes carry. */
function ProjectKeys({ keys }: { keys: readonly string[] }) {
  return (
    <>
      {keys.map((key) => (
        <a key={key} className="oc-key" href={href(projectPath(key))} title={`The ${key} project`}>
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
 * page and the unit peek — the chart's units, the seats above every unit, the
 * page's roster and the rail's — and it renders a seat's STATE: whether a node
 * is running it, whether it is waiting on a person, whether it broke. Four
 * hand-written copies of that is four chances for one of them to keep drawing
 * a healthy badge over a seat nothing is running.
 *
 * It reads the agent and sandbox slices itself rather than taking them as
 * props: every caller was subscribing to both for this and for nothing else,
 * and a subscription per row-block is what the store's slice keys are for.
 */
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
            decorative
            title={seat.name}
          />
          <span className="col" style={{ gap: 0, minWidth: 0, flex: 1 }}>
            <span className="truncate t-cell">{seat.name}</span>
            {/* ONLY A HANDLE THE ENGINE REPORTED. An engine that sends no
                derived hierarchy leaves the handle of a seat that declares
                none unknown, and deriving one here is the second
                implementation `lib/seats.ts` exists to have removed. */}
            {seat.handle && (
              <span className="truncate t-caption mono">{handleLabel(seat.handle)}</span>
            )}
          </span>
          {seat.kind === "human" ? (
            // THE CIRCLE SAYS IT. A tag reading "human" beside a person's
            // circle was the outline said twice; the word stays for a
            // screen reader, which does not see the outline.
            <span className="sr-only">human</span>
          ) : (
            <StateBadge agent={liveRowFor(agents, seat)} />
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
 * list this object is not in — the row goes to the unit's own page instead,
 * where its seats and its own children are the point rather than a preview.
 */
function SubUnitLinks({ units }: { units: Unit[] }) {
  return (
    <div className="list">
      {units.map((child) => (
        <a key={child.key} className="thread-entry" href={href(unitPath(child))}>
          <FolderGlyph size="sm" />
          <span className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
            <strong className="t-cell truncate">{child.name}</strong>
            {child.purpose && <span className="t-caption truncate">{child.purpose}</span>}
          </span>
          <Tag appearance="outline">{child.type || "unit"}</Tag>
          <ArrowRightGlyph size="sm" />
        </a>
      ))}
    </div>
  );
}

/**
 * What a unit IS — the five facts, and the roster two of them are counted from.
 *
 * THE PAGE AND THE PEEK READ THE SAME FUNCTION, which is the only thing that
 * keeps them from drifting: "Seats" and "Directly in it" are two different
 * questions about the same tree — everything under the unit, against what sits
 * immediately in it — and two hand-written fact lists would eventually answer
 * them the other way round in one of the two frames.
 */
function unitView(
  index: OrgIndex,
  unit: Unit,
  projects: ReadonlyMap<Unit, readonly string[]>,
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
        label: "Lead",
        // NOT REPORTED IS NOT NOBODY. Without the engine's derived block an
        // inherited lead is a question this client cannot answer, and an
        // empty cell there would read as a unit nobody leads.
        value: lead ? (
          <>
            {lead.name}
            {unit.leadInherited && <span className="muted"> (inherited)</span>}
          </>
        ) : !index.hierarchy && !unit.lead ? (
          <EmptyValue label={LEAD_NOT_REPORTED} />
        ) : (
          ""
        ),
        path: lead ? seatPath(lead) : undefined,
      },
      // ALL THREE OFF ONE TALLY. They were three separate expressions over two
      // different trees — `seats.length` is a filter over every seat in the
      // company, `unit.seats.length` is the unit's own list — and the rail and
      // the org chart each computed a fourth and a fifth somewhere else. What
      // made that a defect rather than duplication is that they DISAGREED in
      // public: "Leadership 5" in the rail, "2 seats" on the chart block, a
      // third figure on the roster, none of them saying which question it was
      // the answer to.
      { label: "Seats", value: tally.total, note: UNIT_TOTAL_HINT },
      { label: "Directly in it", value: tally.direct },
      { label: "Sub-units", value: tally.subUnits },
      // WHERE ITS WORK IS FILED, from the tracker — absent rather than "none"
      // on a company whose tracker is not this engine's.
      ...(projects.get(unit)?.length
        ? [{ label: "Project", value: <ProjectKeys keys={projects.get(unit)!} /> }]
        : []),
    ],
  };
}

/** The hint under every "no such unit", on the page and in the rail alike. */
const NO_UNIT_HINT =
  "A unit is addressed by its key — its id, or its name where it declares none. One renamed since this link was made answers to its new key.";

/** And what an empty one costs, which is the part a reader acts on. */
const NO_SEATS_HINT =
  "A unit with no seats routes nothing: work filed to it reaches its lead, or nobody.";

/**
 * One unit on the Teams tree, and everything under it.
 *
 * A UNIT BLOCK IS ONE COMPONENT FOR ITS WHOLE LIFE. It was once declared
 * inside the screen's render, which makes it a NEW component type on every
 * render, so each `agents` push — twice per tool-loop round, for every seat in
 * the company — unmounted the whole tree and built it again: scroll position,
 * focus and every open disclosure in it went with it.
 *
 * Its members are [Unit.seats], which is what the ENGINE placed there: a root
 * seat its `unit:` reference moved into this unit is a member here, where the
 * document wrote it above every unit.
 */
export function UnitBlock({
  unit,
  hierarchy = true,
  projects = new Map(),
}: {
  unit: Unit;
  /** Whether the engine reported the derived hierarchy (`OrgIndex.hierarchy`). */
  hierarchy?: boolean;
  /** Project keys by unit, from [projectsByUnit] over the same index. */
  projects?: ReadonlyMap<Unit, readonly string[]>;
}) {
  const lead = unit.effectiveLead;
  const keys = projects.get(unit) ?? [];
  return (
    <section className="org-unit" aria-label={unit.name}>
      <div className="org-unit-head">
        {/* THE NAME IS THE ONE THING THIS HEAD MUST SAY, so it is the one part
            that never gives way: the head WRAPS instead, and the kind, the lead
            and the count move to a line of their own under it. Shrunk in one
            row with them, a phone drew "Developer Relations" as "D…" beside a
            full "department" tag and a full lead chip. */}
        <span className="org-unit-name">
          <FolderGlyph size="sm" style={{ color: "var(--color-text-muted)" }} />
          <a className="t-body truncate org-unit-link" href={href(unitPath(unit))}>
            {unit.name}
          </a>
        </span>
        <ProjectKeys keys={keys} />
        <Tag appearance="outline">{unit.type || "unit"}</Tag>
        {lead ? (
          <Tag
            variant="neutral"
            leadingIcon={<CrownGlyph size="xs" />}
            title={unit.leadInherited ? "inherited from the parent unit" : "explicit lead"}
          >
            {lead.name}
            {unit.leadInherited && <span className="muted"> (inherited)</span>}
          </Tag>
        ) : !hierarchy && !unit.lead ? (
          <Tag appearance="outline" leadingIcon={<CrownGlyph size="xs" />}>
            {LEAD_NOT_REPORTED}
          </Tag>
        ) : null}
        <span className="spacer" />
        {/* THE SUBTREE FIRST, and the direct count only where the two differ —
            which is what `unitSeatsLabel` decides, once, for every surface. */}
        <span className="t-caption org-unit-count" title={UNIT_TOTAL_HINT}>
          {unitSeatsLabel(unitTally(unit))}
        </span>
      </div>
      {unit.purpose && <p className="t-caption measure org-unit-purpose">{unit.purpose}</p>}
      {unit.goals.length > 0 && (
        <ul className="org-unit-goals" aria-label={`${unit.name} goals`}>
          {unit.goals.map((g, i) => (
            <li key={i} className="t-cell">
              <FlagGlyph size="xs" aria-hidden="true" />
              <span>{g}</span>
            </li>
          ))}
        </ul>
      )}
      {unit.seats.length > 0 && <SeatLinks seats={unit.seats} />}
      {unit.children.length > 0 && (
        <div className="org-children">
          {unit.children.map((child) => (
            <UnitBlock key={child.key} unit={child} hierarchy={hierarchy} projects={projects} />
          ))}
        </div>
      )}
    </section>
  );
}

/**
 * Agents › Teams: every unit, nested, with what it is for, what it was told to
 * do, who leads it, where its work is filed and who is in it.
 *
 * The seats above every unit come first, as their own block: they are part
 * of no team, and a Teams page that left them out would hide the founder.
 */
export function Teams() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const projects = useUnitProjects(index);
  useAgentsCounts(index);
  return (
    <>
      <AgentsHeader index={index} />
      <PreviousRevisionNote />
      {index.units.length === 0 ? (
        <EmptyState
          icon={<UsersGlyph size={32} />}
          title="This company declares no units"
          description="Every seat sits at the top level. A unit is declared in the company configuration — Edit org adds one."
        />
      ) : (
        <div className="org-tree">
          {index.rootSeats.length > 0 && (
            <section className="org-unit" aria-label="Above every unit">
              <div className="org-unit-head">
                <span className="org-unit-name">
                  <CrownGlyph size="sm" style={{ color: "var(--color-text-muted)" }} />
                  <strong className="t-body truncate">Above every unit</strong>
                </span>
                <span className="spacer" />
                <span className="t-caption org-unit-count">
                  {unitSeatsLabel({
                    total: index.rootSeats.length,
                    direct: index.rootSeats.length,
                    subUnits: 0,
                  })}
                </span>
              </div>
              <SeatLinks seats={index.rootSeats} />
            </section>
          )}
          {index.topUnits.map((unit) => (
            <UnitBlock key={unit.key} unit={unit} hierarchy={index.hierarchy} projects={projects} />
          ))}
        </div>
      )}
    </>
  );
}

/**
 * One unit's page.
 *
 * A UNIT IS AN OBJECT, and it had no address: it was `#/org?unit=`, a filter
 * on a lens, so a team could not be linked to and carried nothing of its own.
 * Here it is a page with the four facts a unit actually has — who leads it,
 * what it is for, who is in it, and what it has been told to do — and the
 * routing consequence that makes a unit more than a label: knowledge,
 * delegation and escalation all follow this tree.
 *
 * Addressed by its KEY ([Unit.id]) — `id` where a unit declares one and its
 * name where it does not, which is the engine's own `Unit.Key` carried on the
 * projection — so a link from the chart, a schedule's scope and a report's
 * finding reach the same page, and two units sharing a name reach their own.
 * A key a rename retired still resolves ([unitByKey]) and is replaced by the
 * key the unit holds now.
 */
export function UnitScreen({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const projects = useUnitProjects(index);
  useAgentsCounts(index);

  const unit = unitByKey(index, id);
  // A RETIRED KEY IS MOVED TO THE CURRENT ONE, by REPLACE — the same place
  // said the way the unit is known now — so every link this page builds keys
  // on the key the unit actually holds and Back skips the old spelling.
  const nav = useNavigator();
  const route = useRoute();
  useEffect(() => {
    if (unit?.id && unit.id !== id) nav.replace(unitPath(unit), route.query);
  }, [unit, id, nav, route.query]);
  usePageLabels(unit ? { [id]: unit.name } : {});

  if (!unit) {
    return (
      <EmptyState
        icon={<NetworkGlyph size={32} />}
        title={`No unit “${id}”`}
        description={NO_UNIT_HINT}
      />
    );
  }

  const { seats, facts } = unitView(index, unit, projects);

  return (
    <>
      <PageActions>
        <a className="t-link" href={href(["agents"])}>
          Chart →
        </a>
      </PageActions>
      <PreviousRevisionNote />

      <ObjectHeader kind="Unit" icon="network" title={unit.name} facts={facts} />

      {unit.purpose && (
        <Card>
          <Card.Header icon={<TargetGlyph size="sm" />}>
            <Card.Title>Purpose</Card.Title>
          </Card.Header>
          <p className="t-body measure">{unit.purpose}</p>
        </Card>
      )}

      {unit.goals?.length ? (
        <Card>
          <Card.Header icon={<FlagGlyph size="sm" />} count={unit.goals.length}>
            <Card.Title>Goals</Card.Title>
          </Card.Header>
          <ul className="col gap-1" style={{ paddingLeft: "var(--spacing-4)", margin: 0 }}>
            {unit.goals.map((g, i) => (
              <li key={i} className="t-cell">
                {g}
              </li>
            ))}
          </ul>
        </Card>
      ) : null}

      <Card padding="none">
        <Card.Header
          icon={<UsersGlyph size="sm" />}
          count={seats.length}
          subtitle="everyone in this unit and everything under it"
        >
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

      {unit.children.length > 0 && (
        <Card padding="none">
          <Card.Header icon={<NetworkGlyph size="sm" />} count={unit.children.length}>
            <Card.Title>Sub-units</Card.Title>
          </Card.Header>
          <SubUnitLinks units={unit.children} />
        </Card>
      )}
    </>
  );
}

/**
 * One unit, in the rail.
 *
 * # Why this reads the pushed tree and not a query of its own
 *
 * Every other peek asks the engine for its object, because a row's copy is
 * whatever its own list needed. A unit has no such answer to ask for: the org
 * tree arrives whole in the connect handshake and is REPLACED whole whenever
 * the company configuration activates, so it is already present before any
 * list is — which is the property the "fetch your own object" rule is actually
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
 * name is a tree that has not arrived and the rail says so with a skeleton
 * rather than claiming a unit does not exist.
 */
export function UnitPeek({ id }: { id: string }) {
  const org = useOrg();
  const { connected } = useConnection();
  const index = useMemo(() => indexOrg(org), [org]);
  const projects = useUnitProjects(index);
  const unit = unitByKey(index, id);
  // A retired key moves to the current one, by the rail's replacing move.
  const { move } = usePeekControls();
  useEffect(() => {
    if (unit?.id && unit.id !== id) move({ kind: "unit", id: unit.id });
  }, [unit, id, move]);

  if (!unit) {
    if (!connected) return <Skeleton variant="text" rows={6} label="Loading the org tree" />;
    return (
      <EmptyState
        size="compact"
        icon={<NetworkGlyph size={32} />}
        title={`No unit “${id}”`}
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
        {/* WHAT IT IS FOR, always drawn — including when nobody wrote one.
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
