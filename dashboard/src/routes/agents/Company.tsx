/**
 * The company's shape: the org chart, its teams, and where that shape is
 * edited — three sections of Agents.
 *
 * # Three sections, and the third one writes
 *
 * `#/agents` is the CHART, `#/agents/teams` the units with what each is for,
 * and `#/agents/edit` the BUILDER. They were three lenses of one company
 * screen (`lens=chart|charter|builder`) with the directory beside them as a
 * fourth; a lens is a way of looking at one object, and these are three
 * different places a reader goes, so each is a section with its own path. The
 * charter itself — mission, vision, the standing policies — is the company's
 * own settings and lives at Settings › General (`routes/settings/General.tsx`).
 *
 * The builder (`routes/org/OrgEdit.tsx`, a chunk of its own) is the odd
 * one: the two read sections draw the ANONYMOUS org projection this node has applied, and the
 * builder edits the GUARDED configuration document a revision behind it. That
 * is why [PreviousRevisionNote] is drawn on the chart and the teams and not on
 * the builder — between a save and this node applying it, the chart has not
 * moved and would otherwise read as a save that did nothing — and why the
 * builder is the one section that can hold work a move would lose. Its leave
 * guard is what holds such a move, and `builder/BuilderContext.keepsTheLens`
 * asks the route table for this section's address.
 *
 * The tree is drawn as nested units rather than as a centred graph: the
 * hierarchy nests to any depth by design, and a centred layout at depth four
 * is a horizontal scroll nobody reads.
 *
 * # A unit is an object with a page
 *
 * `UnitScreen` is that page, at `#/agents/teams/{unit}`. It existed as a query
 * key on the org screen (`#/org?unit=`), which meant a team could not be
 * linked to, could not carry its own tabs, and was one filter away from being
 * lost.
 */

import { useMemo, type CSSProperties } from "react";
import { href } from "~/app/router.tsx";
import { StateBadge } from "~/components/common.tsx";
import { ButtonLink, Callout, Card, EmptyState, EmptyValue, Skeleton, Tag } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import {
  NetworkGlyph,
  PencilGlyph,
  ArrowRightGlyph,
  CrownGlyph,
  FlagGlyph,
  FolderGlyph,
  UsersGlyph,
  TargetGlyph,
} from "@crewlethq/icons/glyphs";
import { useAgents, useConnection, useOrg } from "~/lib/store-hooks.ts";
import {
  handleLabel,
  indexOrg,
  seatPath,
  unitSeatsLabel,
  unitTally,
  UNIT_TOTAL_HINT,
  type OrgIndex,
  type Seat,
  type Unit,
} from "~/lib/seats.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { PreviousRevisionNote } from "~/routes/org/builder/AfterSaveStrip.tsx";

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
            <StateBadge agent={agents.find((a) => a.role === seat.name)} />
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
        <a key={child.name} className="thread-entry" href={href(["agents", "teams", child.name])}>
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
function unitView(index: OrgIndex, unit: Unit): { seats: Seat[]; facts: Fact[] } {
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
          <EmptyValue label="Not reported by this engine" />
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
    ],
  };
}

/** The hint under every "no such unit", on the page and in the rail alike. */
const NO_UNIT_HINT =
  "A unit is addressed by name here. Its stable id is part of the guarded configuration rather than the public org projection, so no link this screen can build carries one.";

/** And what an empty one costs, which is the part a reader acts on. */
const NO_SEATS_HINT =
  "A unit with no seats routes nothing: work filed to it reaches its lead, or nobody.";

/**
 * One unit in the chart, and everything under it.
 *
 * A UNIT BLOCK IS ONE COMPONENT FOR ITS WHOLE LIFE. This was declared inside
 * the screen's render, which makes it a NEW component type on every render, so
 * each `agents` push — twice per tool-loop round, for every seat in the
 * company — unmounted the whole chart and built it again. React cannot
 * reconcile two function identities as one type, so nothing about the tree
 * survived: scroll position, focus and every open disclosure in it.
 *
 * Its members are [Unit.seats], which is what the ENGINE placed there: a root
 * seat its `unit:` reference moved into this unit is a member here and is
 * marked as one, where the document wrote it above every unit.
 */
export function UnitBlock({ unit }: { unit: Unit }) {
  const lead = unit.effectiveLead;
  return (
    <div className="org-unit">
      <div className="org-unit-head">
        {/* THE NAME IS THE ONE THING THIS HEAD MUST SAY, so it is the one part
            that never gives way: the head WRAPS instead, and the kind, the lead
            and the count move to a line of their own under it. Shrunk in one
            row with them, a phone drew "Developer Relations" as "D…" beside a
            full "department" tag and a full lead chip. */}
        <span className="org-unit-name">
          <FolderGlyph size="sm" style={{ color: "var(--color-text-muted)" }} />
          <strong className="t-body truncate">{unit.name}</strong>
        </span>
        <Tag appearance="outline">{unit.type || "unit"}</Tag>
        {lead && (
          <Tag
            variant="neutral"
            leadingIcon={<CrownGlyph size="xs" />}
            title={unit.leadInherited ? "inherited from the parent unit" : "explicit lead"}
          >
            {lead.name}
            {unit.leadInherited && <span className="muted"> (inherited)</span>}
          </Tag>
        )}
        <span className="spacer" />
        {/* THE SUBTREE FIRST, and the direct count only where the two differ —
            which is what `unitSeatsLabel` decides, once, for every surface.
            This drew `unit.seats.length` bare while the workspace rail drew
            the SUBTREE bare under the same name, so Leadership was "2 seats"
            here and "5" three inches to the left. A unit with no sub-units has
            one honest number and still reads as one fact. */}
        <span className="t-caption org-unit-count">{unitSeatsLabel(unitTally(unit))}</span>
      </div>
      {unit.purpose && <div className="t-caption measure">{unit.purpose}</div>}
      {unit.seats.length > 0 && (
        <SeatLinks seats={unit.seats} style={{ marginTop: "var(--spacing-2)" }} />
      )}
      {unit.children.length > 0 && (
        <div className="org-children">
          {unit.children.map((child) => (
            <UnitBlock key={child.key} unit={child} />
          ))}
        </div>
      )}
    </div>
  );
}

/**
 * Agents › Org chart: the hierarchy every seat works inside.
 *
 * THE BUILDER IS A BUTTON HERE, NOT A TAB. It is the one section of Agents
 * that writes, and a reader arrives at the chart to look; the button is drawn
 * for every reader because the builder states its own posture — read-only
 * over an engine that refuses the credential, editable over one that does not
 * — and a control that vanished for a reader without a token would read as a
 * product with no way to change its own shape.
 */
export function OrgChart() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const rootSeats = index.rootSeats;

  return (
    <>
      <PageActions>
        <ButtonLink
          size="small"
          variant="secondary"
          href={href(["agents", "edit"])}
          leadingIcon={<PencilGlyph size="sm" />}
        >
          Edit org
        </ButtonLink>
      </PageActions>
      <PageNote>
        The hierarchy is the execution graph: knowledge, delegation and routing all follow it.
      </PageNote>
      {/* A SAVE IS NOT AN APPLY: until this node applies the revision the
          builder saved, the projection the chart draws is the previous one,
          and a chart that has not moved reads as a save that did nothing. */}
      <PreviousRevisionNote />

      {rootSeats.length > 0 && (
        <Card>
          <Card.Header icon={<CrownGlyph size="sm" />} subtitle="seats above every unit">
            <Card.Title>Org-wide</Card.Title>
          </Card.Header>
          <SeatLinks seats={rootSeats} />
        </Card>
      )}
      {/* A HIERARCHY NOBODY DERIVED IS NOT A HIERARCHY. Without the engine's
          `derived` block the tree is only what the document wrote: a root
          seat its `unit:` reference belongs in sits above every unit here,
          and an inherited lead is not shown at all. */}
      {!index.hierarchy && (org?.units ?? []).length > 0 && (
        <Callout variant="info">
          This engine did not report its derived hierarchy, so the chart is drawn as the document
          writes it: seats sit where they were written, and an inherited unit lead is not shown.
        </Callout>
      )}
      <div className="org-tree">
        {index.topUnits.map((unit) => (
          <UnitBlock key={unit.key} unit={unit} />
        ))}
        {!index.units.length && !rootSeats.length && (
          <EmptyState
            icon={<NetworkGlyph size={32} />}
            title="No organisation is loaded"
            description="The org tree comes from the active company configuration."
          />
        )}
      </div>
    </>
  );
}

/**
 * Agents › Teams: every unit, what it is for and what it was told to do.
 *
 * THE GOALS MOVED HERE FROM THE CHARTER. A unit's goals are the unit's, and
 * the charter is the company's: drawn under the mission they read as company
 * policy, and a reader looking for what a team is for had to know that the
 * answer was filed under the company's settings. Each card is the way to the
 * unit's own page, which is where its seats and sub-units are.
 */
export function Teams() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  return (
    <>
      <PageNote>
        A unit routes the work filed to it: knowledge, delegation and escalation all follow this
        tree.
      </PageNote>
      <PreviousRevisionNote />
      {index.units.length === 0 ? (
        <EmptyState
          icon={<UsersGlyph size={32} />}
          title="This company declares no units"
          description="Every seat sits at the top level. A unit is declared in the company configuration — Edit org adds one."
        />
      ) : (
        <div className="grid grid-auto">
          {index.units.map((u) => {
            const tally = unitTally(u);
            return (
              <Card key={u.key}>
                <Card.Header subtitle={u.type || "unit"}>
                  <Card.Title>
                    <a className="t-link" href={href(["agents", "teams", u.name])}>
                      {u.name}
                    </a>
                  </Card.Title>
                </Card.Header>
                {u.purpose && <p className="t-caption">{u.purpose}</p>}
                <p className="t-caption" title={UNIT_TOTAL_HINT}>
                  {unitSeatsLabel(tally)}
                  {u.effectiveLead ? ` · led by ${u.effectiveLead.name}` : ""}
                </p>
                {u.goals.length ? (
                  <ul
                    className="col gap-1"
                    style={{ paddingLeft: "var(--spacing-4)", margin: "var(--spacing-2) 0 0" }}
                  >
                    {u.goals.map((g, i) => (
                      <li key={i} className="t-cell">
                        {g}
                      </li>
                    ))}
                  </ul>
                ) : (
                  <span className="t-caption">No goals set.</span>
                )}
              </Card>
            );
          })}
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
 * ADDRESSED BY NAME, which is what every link writes — the chart, a seat's
 * unit, the palette, a unit schedule (whose scope the engine keys on the name,
 * `internal/schedule`) — and the one identifier the org tree carries. The
 * stable `id` a unit may declare is a GUARDED field of the configuration
 * (`ConfigUnit.id`), absent from the tree an anonymous reader is pushed, so a
 * page that resolved it would be a page only an operator could open.
 */
export function UnitScreen({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);

  const unit = index.units.find((u) => u.name === id);
  usePageLabels(unit ? { [id]: unit.name } : {});

  if (!unit) {
    return (
      <EmptyState
        icon={<NetworkGlyph size={32} />}
        title={`No unit called “${id}”`}
        description={NO_UNIT_HINT}
      />
    );
  }

  const { seats, facts } = unitView(index, unit);

  return (
    <>
      <PageActions>
        {
          <a className="t-link" href={href(["agents"])}>
            Chart →
          </a>
        }
      </PageActions>

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
  const unit = index.units.find((u) => u.name === id);

  if (!unit) {
    if (!connected) return <Skeleton variant="text" rows={6} label="Loading the org tree" />;
    return (
      <EmptyState
        size="compact"
        icon={<NetworkGlyph size={32} />}
        title={`No unit called “${id}”`}
        description={NO_UNIT_HINT}
      />
    );
  }

  const { seats, facts } = unitView(index, unit);
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
