/**
 * The company: what it is for, the shape it has, and where that shape is
 * edited.
 *
 * # Three lenses, and the third one writes
 *
 * The chart, the charter and the BUILDER. The DIRECTORY that used to be a
 * lens here is now its own destination — `#/company/people` — because a list
 * of every seat is a place a reader goes rather than a way of looking at the
 * tree, and a row in the rail beats a segmented control nobody finds.
 *
 * The builder (`routes/org/builder/Builder.tsx`) is the odd one: the two read
 * lenses draw the ANONYMOUS org projection this node has applied, and the
 * builder edits the GUARDED configuration document a revision behind it. That
 * is why [PreviousRevisionNote] is drawn on the read lenses and not on the
 * builder — between a save and this node applying it, the chart has not moved
 * and would otherwise read as a save that did nothing — and why the builder
 * is the one lens that can hold work a move would lose. Its leave guard is
 * what holds such a move, and `builder/BuilderContext.keepsTheLens` is where
 * this screen's own address is written down for it.
 *
 * The tree is drawn as nested units rather than as a centred graph: the
 * hierarchy nests to any depth by design, and a centred layout at depth four
 * is a horizontal scroll nobody reads.
 *
 * # A unit is an object with a page
 *
 * `UnitScreen` is that page. It existed as a query key on the org screen
 * (`#/org?unit=`), which meant a team could not be linked to, could not carry
 * its own tabs, and was one filter away from being lost.
 */

import { useMemo, type CSSProperties } from "react";
import { href } from "~/app/router.tsx";
import { StateBadge, Section } from "~/components/common.tsx";
import { Avatar, Callout, Card, EmptyState, EmptyValue, Skeleton, Tag } from "@crewlethq/ui";
import {
  AccountTreeGlyph,
  ArrowForwardGlyph,
  CrownGlyph,
  ExploreGlyph,
  FlagGlyph,
  FolderGlyph,
  GroupGlyph,
  ShieldGlyph,
  TargetGlyph,
} from "@crewlethq/icons/glyphs";
// OURS, AND DELIBERATELY. `SegmentedControl` welds keyboard ACTIVATION to its
// `semantics`: `radio` commits the option the arrows land on, and this lens
// drives a `useTab`, which pushes a history entry and swaps the whole screen.
// Arrowing between chart and charter under that control is history a reader
// then has to press Back through. See the report.
import { Segmented } from "~/ui/primitives.tsx";
import { useAgents, useConnection, useOrg } from "~/lib/store-hooks.ts";
import {
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
import { useTab } from "~/app/frame/tabs.ts";
import { usePageLabels } from "~/app/Shell.tsx";
import { PreviousRevisionNote } from "~/routes/org/builder/AfterSaveStrip.tsx";
import { Builder } from "~/routes/org/builder/Builder.tsx";
import { builderSurfaces } from "~/routes/org/builder/surfaces.ts";

// THE ORDER IS THE STRIP'S, AND THE FIRST IS THE DEFAULT: `useTab` drops the
// parameter when it equals the first entry, so landing on the company writes
// no `lens=` and only a reader who moved carries one. The builder goes last
// because it is the only lens that WRITES — a reader arrives to look.
const LENSES = ["chart", "charter", "builder"] as const;
type Lens = (typeof LENSES)[number];

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
        <a
          key={seat.key}
          className={`org-node${seat.kind === "human" ? " human" : ""}`}
          href={href(seatPath(seat))}
        >
          {/* THEIR `variant="dashed"` IS OUR `human`, and it means the same
              thing: a human seat is DRAWN rather than tinted, because the
              engine does not run it and that is structure, not status. Their
              `md` is 32px where ours was 26, so the step moves down one. */}
          <Avatar
            name={seat.name}
            size="sm"
            variant={seat.kind === "human" ? "dashed" : "solid"}
            decorative
            title={seat.name}
          />
          <span className="col" style={{ gap: 0, minWidth: 0, flex: 1 }}>
            <span className="truncate t-cell">{seat.name}</span>
            {/* ONLY A HANDLE THE ENGINE REPORTED. An engine that sends no
                derived hierarchy leaves the handle of a seat that declares
                none unknown, and deriving one here is the second
                implementation `lib/seats.ts` exists to have removed. */}
            {seat.handle && <span className="truncate t-caption mono">@{seat.handle}</span>}
          </span>
          {seat.kind === "human" ? (
            <Tag appearance="outline">human</Tag>
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
        <a key={child.name} className="thread-entry" href={href(["company", "units", child.name])}>
          <FolderGlyph size="sm" />
          <span className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
            <strong className="t-cell truncate">{child.name}</strong>
            {child.purpose && <span className="t-caption truncate">{child.purpose}</span>}
          </span>
          <Tag appearance="outline">{child.type || "unit"}</Tag>
          <ArrowForwardGlyph size="sm" />
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
        <FolderGlyph size="sm" style={{ color: "var(--text-faint)" }} />
        <strong className="t-body truncate">{unit.name}</strong>
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
        <span className="t-caption">{unitSeatsLabel(unitTally(unit))}</span>
      </div>
      {unit.purpose && <div className="t-caption measure">{unit.purpose}</div>}
      {unit.seats.length > 0 && (
        <SeatLinks seats={unit.seats} style={{ marginTop: "var(--space-2)" }} />
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

export function CompanyScreen() {
  const org = useOrg();
  const [lens, setLens] = useTab("lens", LENSES);
  const index = useMemo(() => indexOrg(org), [org]);

  const rootSeats = index.rootSeats;

  return (
    <>
      <PageActions>
        {
          <Segmented<Lens>
            ariaLabel="Org view"
            value={lens}
            onChange={setLens}
            options={[
              { value: "chart", label: "Chart", icon: "account_tree" },
              { value: "charter", label: "Charter", icon: "flag" },
              { value: "builder", label: "Builder", icon: "edit" },
            ]}
          />
        }
      </PageActions>
      <PageNote>
        The hierarchy is the execution graph: knowledge, delegation and routing all follow it.
      </PageNote>
      {/* A SAVE IS NOT AN APPLY: until this node applies the revision the
          builder saved, the projection both read lenses draw is the previous
          one, and a chart that has not moved reads as a save that did
          nothing. Not on the builder, which draws the draft itself. */}
      {lens !== "builder" && <PreviousRevisionNote />}

      {lens === "chart" && (
        <>
          {rootSeats.length > 0 && (
            <Card>
              <Card.Header icon={<CrownGlyph size="sm" />} subtitle="seats above every unit">
                <Card.Title>Org-wide</Card.Title>
              </Card.Header>
              <SeatLinks seats={rootSeats} />
            </Card>
          )}
          {/* A HIERARCHY NOBODY DERIVED IS NOT A HIERARCHY. Without the
              engine's `derived` block the tree is only what the document
              wrote: a root seat its `unit:` reference belongs in sits above
              every unit here, and an inherited lead is not shown at all. */}
          {!index.hierarchy && (org?.units ?? []).length > 0 && (
            <Callout variant="info">
              This engine did not report its derived hierarchy, so the chart is drawn as the
              document writes it: seats sit where they were written, and an inherited unit lead is
              not shown.
            </Callout>
          )}
          <div className="org-tree">
            {index.topUnits.map((unit) => (
              <UnitBlock key={unit.key} unit={unit} />
            ))}
            {!index.units.length && !rootSeats.length && (
              <EmptyState
                icon={<AccountTreeGlyph size={32} />}
                title="No organisation is loaded"
                description="The org tree comes from the active company configuration."
              />
            )}
          </div>
        </>
      )}

      {lens === "charter" && (
        <div className="col gap-4">
          <Card>
            <Card.Header icon={<TargetGlyph size="sm" />}>
              <Card.Title>Mission</Card.Title>
            </Card.Header>
            <p className="t-body measure">
              {org?.mission || <span className="muted">No mission is set.</span>}
            </p>
          </Card>
          {org?.vision && (
            <Card>
              <Card.Header icon={<ExploreGlyph size="sm" />}>
                <Card.Title>Vision</Card.Title>
              </Card.Header>
              <p className="t-body measure">{org.vision}</p>
            </Card>
          )}
          <Card>
            <Card.Header icon={<ShieldGlyph size="sm" />} count={org?.policies?.length ?? 0}>
              <Card.Title>Policies</Card.Title>
            </Card.Header>
            {org?.policies?.length ? (
              <ol className="col gap-2" style={{ paddingLeft: "var(--space-4)", margin: 0 }}>
                {org.policies.map((p, i) => (
                  <li key={i} className="t-body measure">
                    {p}
                  </li>
                ))}
              </ol>
            ) : (
              <EmptyState
                size="compact"
                icon={<ShieldGlyph size={32} />}
                title="No policies are set"
                description="Policies render into every executor's prompt in full. They are the company's standing instructions."
              />
            )}
          </Card>
          <Section title="Unit goals" hint="what each team is for">
            <div className="grid grid-auto">
              {index.units.map((u) => (
                <Card key={u.key}>
                  <Card.Header subtitle={u.type}>
                    <Card.Title>{u.name}</Card.Title>
                  </Card.Header>
                  {u.purpose && <p className="t-caption">{u.purpose}</p>}
                  {u.goals.length ? (
                    <ul
                      className="col gap-1"
                      style={{ paddingLeft: "var(--space-4)", margin: "var(--space-2) 0 0" }}
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
              ))}
            </div>
          </Section>
        </div>
      )}

      {/* THE SURFACES ARE PASSED, NOT IMPORTED BY THE BUILDER: every dialog and
          both views are injected so a suite can drive the lens with fakes.
          `builderSurfaces` is the real set. */}
      {lens === "builder" && <Builder surfaces={builderSurfaces} />}
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
 * Addressed by `id` where a unit declares one and by NAME where it does not,
 * which is the same rule the engine's own `Unit.Key` follows — so a link from
 * a project's owning unit and a link from the chart reach the same page.
 */
export function UnitScreen({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);

  const unit = index.units.find((u) => u.name === id);
  usePageLabels(unit ? { [id]: unit.name } : {});

  if (!unit) {
    return (
      <EmptyState
        icon={<AccountTreeGlyph size={32} />}
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
          <a className="t-link" href={href(["company"], { lens: "chart" })}>
            Chart →
          </a>
        }
      </PageActions>

      <ObjectHeader kind="Unit" icon="account_tree" title={unit.name} facts={facts} />

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
          <ul className="col gap-1" style={{ paddingLeft: "var(--space-4)", margin: 0 }}>
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
          icon={<GroupGlyph size="sm" />}
          count={seats.length}
          subtitle="everyone in this unit and everything under it"
        >
          <Card.Title>Seats</Card.Title>
        </Card.Header>
        {seats.length > 0 ? (
          <SeatLinks seats={seats} style={{ padding: "var(--space-3)" }} />
        ) : (
          <EmptyState
            size="compact"
            icon={<GroupGlyph size={32} />}
            title="No seats in this unit"
            description={NO_SEATS_HINT}
          />
        )}
      </Card>

      {unit.children.length > 0 && (
        <Card padding="none">
          <Card.Header icon={<AccountTreeGlyph size="sm" />} count={unit.children.length}>
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
 * id is a tree that has not arrived and the rail says so with a skeleton
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
        icon={<AccountTreeGlyph size={32} />}
        title={`No unit called “${id}”`}
        description={NO_UNIT_HINT}
      />
    );
  }

  const { seats, facts } = unitView(index, unit);
  const children = unit.children;

  return (
    <>
      <ObjectHeader size="peek" kind="Unit" icon="account_tree" title={unit.name} facts={facts} />
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
          <Card.Header icon={<GroupGlyph size="sm" />} count={seats.length}>
            <Card.Title>Seats</Card.Title>
          </Card.Header>
          {seats.length > 0 ? (
            <SeatLinks seats={seats} style={{ padding: "var(--space-3)" }} />
          ) : (
            <EmptyState
              size="compact"
              icon={<GroupGlyph size={32} />}
              title="No seats in this unit"
              description={NO_SEATS_HINT}
            />
          )}
        </Card>

        {children.length > 0 && (
          <Card padding="none">
            <Card.Header icon={<AccountTreeGlyph size="sm" />} count={children.length}>
              <Card.Title>Sub-units</Card.Title>
            </Card.Header>
            <SubUnitLinks units={children} />
          </Card>
        )}
      </div>
    </>
  );
}
