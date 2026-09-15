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

import { useMemo } from "react";
import { plural } from "~/lib/format.ts";
import { href, useParam } from "~/app/router.tsx";
import { StateBadge, Section } from "~/components/common.tsx";
import { Avatar, Badge, Empty, Panel, Segmented } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useAgents, useOrg, useSandboxes } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import type { OrgUnit } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { usePageLabels } from "~/app/Shell.tsx";

const LENSES = ["chart", "charter"] as const;
type Lens = (typeof LENSES)[number];

export function CompanyScreen() {
  const org = useOrg();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const [lens, setLens] = useTab("lens", LENSES);
  const index = useMemo(() => indexOrg(org), [org]);

  const seatFor = (name: string) => agents.find((a) => a.role === name);

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
        {seats.length > 0 && (
          <div className="org-seats" style={{ marginTop: "var(--space-2)" }}>
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
                  <StateBadge agent={seatFor(seat.name)} sandboxes={sandboxes} />
                )}
              </a>
            ))}
          </div>
        )}
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
              <div className="org-seats">
                {rootSeats.map((seat) => (
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
                      <StateBadge agent={seatFor(seat.name)} sandboxes={sandboxes} />
                    )}
                  </a>
                ))}
              </div>
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
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const index = useMemo(() => indexOrg(org), [org]);
  const seatFor = (name: string) => agents.find((a) => a.role === name);

  const unit = index.units.find((u) => (u.id || u.name) === id || u.name === id);
  usePageLabels(unit ? { [id]: unit.name } : {});

  if (!unit) {
    return (
      <Empty
        icon="sitemap"
        title={`No unit called “${id}”`}
        hint="A unit is addressed by its id where it declares one and by its name where it does not — the same key the engine files work, routing and pages under."
      />
    );
  }

  const seats = index.seats.filter((s) => s.unitChain.some((u) => u.name === unit.name));
  const direct = index.seats.filter((s) => s.unit?.name === unit.name);
  // THE EFFECTIVE LEAD, which is the nearest ancestor's where this unit
  // declares none. It behaves identically everywhere in the engine, and hiding
  // the difference is how somebody concludes a team is unmanaged.
  const lead = direct[0]?.unitLead ?? unit.lead ?? "";

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
        facts={[
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
        ]}
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
        <div className="org-seats" style={{ padding: "var(--space-3)" }}>
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
                <StateBadge agent={seatFor(seat.name)} sandboxes={sandboxes} />
              )}
            </a>
          ))}
          {seats.length === 0 && (
            <Empty
              inline
              icon="users"
              title="No seats in this unit"
              hint="A unit with no seats routes nothing: work filed to it reaches its lead, or nobody."
            />
          )}
        </div>
      </Panel>

      {(unit.children ?? []).length > 0 && (
        <Panel title="Sub-units" icon="sitemap" count={(unit.children ?? []).length} padding="none">
          <div className="list">
            {(unit.children ?? []).map((child) => (
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
        </Panel>
      )}
    </>
  );
}
