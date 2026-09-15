/**
 * The company: what it is for, and the shape it has.
 *
 * # Two tabs, not three
 *
 * The chart and the charter. The DIRECTORY that used to be a third lens here
 * is now its own destination — `#/company/people` — because a list of every
 * seat is a place a reader goes rather than a way of looking at the tree, and
 * a row in the rail beats a segmented control nobody finds.
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
import { plural } from "~/lib/format.ts";
import { href, useParam } from "~/app/router.tsx";
import { StateBadge, Section } from "~/components/common.tsx";
import { Avatar, Badge, Empty, Panel, Segmented, Skeleton } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useAgents, useConnection, useOrg, useSandboxes } from "~/lib/store-hooks.ts";
import { indexOrg, type OrgIndex, type Seat } from "~/lib/seats.ts";
import type { OrgUnit } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { usePageLabels } from "~/app/Shell.tsx";

const LENSES = ["chart", "charter"] as const;
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
  const sandboxes = useSandboxes();
  return (
    <div className="org-seats" style={style}>
      {seats.map((seat) => (
        <a
          key={seat.handle}
          className={`org-node${seat.kind === "human" ? " human" : ""}`}
          href={href(["company", "people", seat.handle])}
        >
          <Avatar name={seat.name} human={seat.kind === "human"} />
          <span className="col" style={{ gap: 0, minWidth: 0, flex: 1 }}>
            <span className="truncate t-cell">{seat.name}</span>
            <span className="truncate t-caption mono">@{seat.handle}</span>
          </span>
          {seat.kind === "human" ? (
            <Badge outline>human</Badge>
          ) : (
            <StateBadge agent={agents.find((a) => a.role === seat.name)} sandboxes={sandboxes} />
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
function SubUnitLinks({ units }: { units: OrgUnit[] }) {
  return (
    <div className="list">
      {units.map((child) => (
        <a
          key={child.name}
          className="thread-entry"
          href={href(["company", "units", child.id || child.name])}
        >
          <Icon name="folder" size="sm" />
          <span className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
            <strong className="t-cell truncate">{child.name}</strong>
            {child.purpose && <span className="t-caption truncate">{child.purpose}</span>}
          </span>
          <Badge outline>{child.type || "unit"}</Badge>
          <Icon name="arrowRight" size="sm" />
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
function unitView(index: OrgIndex, unit: OrgUnit): { seats: Seat[]; facts: Fact[] } {
  const seats = index.seats.filter((s) => s.unitChain.some((u) => u.name === unit.name));
  const direct = index.seats.filter((s) => s.unit?.name === unit.name);
  // THE EFFECTIVE LEAD, which is the nearest ancestor's where this unit
  // declares none. It behaves identically everywhere in the engine, and hiding
  // the difference is how somebody concludes a team is unmanaged.
  const lead = direct[0]?.unitLead ?? unit.lead ?? "";
  return {
    seats,
    facts: [
      { label: "Type", value: unit.type || "unit" },
      {
        label: "Lead",
        value: lead ? (
          <>
            {index.byName.get(lead)?.name ?? lead}
            {!unit.lead && <span className="faint"> (inherited)</span>}
          </>
        ) : (
          ""
        ),
        path: lead ? ["company", "people", index.byName.get(lead)?.handle ?? lead] : undefined,
      },
      { label: "Seats", value: seats.length },
      { label: "Directly in it", value: direct.length },
      { label: "Sub-units", value: (unit.children ?? []).length },
    ],
  };
}

/** The hint under every "no such unit", on the page and in the rail alike. */
const NO_UNIT_HINT =
  "A unit is addressed by its id where it declares one and by its name where it does not — the same key the engine files work, routing and pages under.";

/** And what an empty one costs, which is the part a reader acts on. */
const NO_SEATS_HINT =
  "A unit with no seats routes nothing: work filed to it reaches its lead, or nobody.";

export function CompanyScreen() {
  const org = useOrg();
  const [lens, setLens] = useTab("lens", LENSES);
  const index = useMemo(() => indexOrg(org), [org]);

  function UnitBlock({ unit, depth }: { unit: OrgUnit; depth: number }) {
    const seats = index.seats.filter((s) => s.unit === unit);
    // A unit with no lead of its own inherits the nearest ancestor's, and the
    // chart says which it is: an inherited lead behaves identically to an
    // explicit one everywhere in the engine, and hiding the difference is how
    // an operator comes to think a unit is unmanaged.
    const explicitLead = unit.lead;
    const effectiveLead = seats[0]?.unitLead ?? explicitLead ?? "";
    return (
      <div className="org-unit">
        <div className="org-unit-head">
          <Icon name="folder" size="sm" style={{ color: "var(--text-faint)" }} />
          <strong className="t-body truncate">{unit.name}</strong>
          <Badge outline>{unit.type || "unit"}</Badge>
          {effectiveLead && (
            <Badge
              tone="neutral"
              icon="crown"
              title={explicitLead ? "explicit lead" : "inherited from the parent unit"}
            >
              {effectiveLead}
              {!explicitLead && <span className="faint"> (inherited)</span>}
            </Badge>
          )}
          <span className="spacer" />
          <span className="t-caption">{plural(seats.length, "seat")}</span>
        </div>
        {unit.purpose && <div className="t-caption measure">{unit.purpose}</div>}
        {seats.length > 0 && <SeatLinks seats={seats} style={{ marginTop: "var(--space-2)" }} />}
        {(unit.children?.length ?? 0) > 0 && (
          <div className="org-children">
            {unit.children!.map((child) => (
              <UnitBlock key={child.name} unit={child} depth={depth + 1} />
            ))}
          </div>
        )}
      </div>
    );
  }

  const rootSeats = index.seats.filter((s) => !s.unit);

  return (
    <>
      <PageActions>
        {
          <Segmented<Lens>
            ariaLabel="Org view"
            value={lens}
            onChange={setLens}
            options={[
              { value: "chart", label: "Chart", icon: "sitemap" },
              { value: "charter", label: "Charter", icon: "flag" },
            ]}
          />
        }
      </PageActions>
      <PageNote>
        The hierarchy is the execution graph: knowledge, delegation and routing all follow it.
      </PageNote>

      {lens === "chart" && (
        <>
          {rootSeats.length > 0 && (
            <Panel title="Org-wide" icon="crown" subtitle="seats above every unit">
              <SeatLinks seats={rootSeats} />
            </Panel>
          )}
          <div className="org-tree">
            {(org?.units ?? []).map((unit) => (
              <UnitBlock key={unit.name} unit={unit} depth={0} />
            ))}
            {!(org?.units ?? []).length && !rootSeats.length && (
              <Empty
                icon="sitemap"
                title="No organisation is loaded"
                hint="The org tree comes from the active company configuration."
              />
            )}
          </div>
        </>
      )}

      {lens === "charter" && (
        <div className="col gap-4">
          <Panel title="Mission" icon="target">
            <p className="t-body measure">
              {org?.mission || <span className="faint">No mission is set.</span>}
            </p>
          </Panel>
          {org?.vision && (
            <Panel title="Vision" icon="compass">
              <p className="t-body measure">{org.vision}</p>
            </Panel>
          )}
          <Panel title="Policies" icon="shield" count={org?.policies?.length ?? 0}>
            {org?.policies?.length ? (
              <ol className="col gap-2" style={{ paddingLeft: "var(--space-4)", margin: 0 }}>
                {org.policies.map((p, i) => (
                  <li key={i} className="t-body measure">
                    {p}
                  </li>
                ))}
              </ol>
            ) : (
              <Empty
                inline
                icon="shield"
                title="No policies are set"
                hint="Policies render into every planner's prompt in full. They are the company's standing instructions."
              />
            )}
          </Panel>
          <Section title="Unit goals" hint="what each team is for">
            <div className="grid grid-auto">
              {index.units.map((u) => (
                <Panel key={u.name} title={u.name} subtitle={u.type}>
                  {u.purpose && <p className="t-caption">{u.purpose}</p>}
                  {u.goals?.length ? (
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
                    <span className="t-caption faint">No goals set.</span>
                  )}
                </Panel>
              ))}
            </div>
          </Section>
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
 * Addressed by `id` where a unit declares one and by NAME where it does not,
 * which is the same rule the engine's own `Unit.Key` follows — so a link from
 * a project's owning unit and a link from the chart reach the same page.
 */
export function UnitScreen({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);

  const unit = index.units.find((u) => (u.id || u.name) === id || u.name === id);
  usePageLabels(unit ? { [id]: unit.name } : {});

  if (!unit) {
    return <Empty icon="sitemap" title={`No unit called “${id}”`} hint={NO_UNIT_HINT} />;
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

      <ObjectHeader
        kind="Unit"
        icon="sitemap"
        identifier={unit.id || undefined}
        title={unit.name}
        facts={facts}
      />

      {unit.purpose && (
        <Panel title="Purpose" icon="target">
          <p className="t-body measure">{unit.purpose}</p>
        </Panel>
      )}

      {unit.goals?.length ? (
        <Panel title="Goals" icon="flag" count={unit.goals.length}>
          <ul className="col gap-1" style={{ paddingLeft: "var(--space-4)", margin: 0 }}>
            {unit.goals.map((g, i) => (
              <li key={i} className="t-cell">
                {g}
              </li>
            ))}
          </ul>
        </Panel>
      ) : null}

      <Panel
        title="Seats"
        icon="users"
        count={seats.length}
        subtitle="everyone in this unit and everything under it"
        padding="none"
      >
        {seats.length > 0 ? (
          <SeatLinks seats={seats} style={{ padding: "var(--space-3)" }} />
        ) : (
          <Empty inline icon="users" title="No seats in this unit" hint={NO_SEATS_HINT} />
        )}
      </Panel>

      {(unit.children ?? []).length > 0 && (
        <Panel title="Sub-units" icon="sitemap" count={(unit.children ?? []).length} padding="none">
          <SubUnitLinks units={unit.children ?? []} />
        </Panel>
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
  const unit = index.units.find((u) => (u.id || u.name) === id || u.name === id);

  if (!unit) {
    if (!connected) return <Skeleton rows={6} />;
    return <Empty inline icon="sitemap" title={`No unit called “${id}”`} hint={NO_UNIT_HINT} />;
  }

  const { seats, facts } = unitView(index, unit);
  const children = unit.children ?? [];

  return (
    <>
      <ObjectHeader
        size="peek"
        kind="Unit"
        icon="sitemap"
        identifier={unit.id || undefined}
        title={unit.name}
        facts={facts}
      />
      <div className="col gap-3">
        {/* WHAT IT IS FOR, always drawn — including when nobody wrote one.
            "Is this the one I meant" is the question the rail answers, and a
            purpose that is simply missing from the panel reads as a unit
            whose purpose the reader failed to scroll to. */}
        <Panel title="Purpose" icon="target">
          {unit.purpose ? (
            <p className="t-body measure">{unit.purpose}</p>
          ) : (
            <span className="muted">No purpose is written for this unit.</span>
          )}
        </Panel>

        {/* THE SEATS THEMSELVES, not just the count in the facts above: a
            reader recognises a team by who is in it long before they
            recognise it by its name, and the badge on each row is the other
            half of "what is it doing". */}
        <Panel title="Seats" icon="users" count={seats.length} padding="none">
          {seats.length > 0 ? (
            <SeatLinks seats={seats} style={{ padding: "var(--space-3)" }} />
          ) : (
            <Empty inline icon="users" title="No seats in this unit" hint={NO_SEATS_HINT} />
          )}
        </Panel>

        {children.length > 0 && (
          <Panel title="Sub-units" icon="sitemap" count={children.length} padding="none">
            <SubUnitLinks units={children} />
          </Panel>
        )}
      </div>
    </>
  );
}
