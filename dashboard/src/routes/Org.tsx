/**
 * The company as a structure.
 *
 * Three lenses over one tree, and the lens is in the URL so a chart someone is
 * looking at is a link they can send. The tree is drawn as nested units rather
 * than as a centred graph: the hierarchy nests to any depth by design, and a
 * centred layout at depth four is a horizontal scroll nobody reads.
 */

import { useMemo } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { plural } from "~/lib/format.ts";
import { href, useParam } from "~/app/router.tsx";
import { SeatChip, StateBadge, Section } from "~/components/common.tsx";
import { Avatar, Badge, Empty, Panel, Segmented } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useAgents, useOrg, useSandboxes } from "~/lib/store-hooks.ts";
import { indexOrg, statusLine, type Seat } from "~/lib/seats.ts";
import type { OrgUnit } from "~/protocol/index.ts";

type Lens = "chart" | "directory" | "charter";

export function OrgScreen() {
  const org = useOrg();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const [lens, setLens] = useParam("lens", "chart", "section");
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
                href={href(["seats", seat.handle])}
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
      <ScreenHead
        title={org?.name ? `${org.name} — org chart` : "Org chart"}
        sub="The hierarchy is the execution graph: knowledge, delegation and routing all follow it."
        actions={
          <Segmented<Lens>
            ariaLabel="Org view"
            value={lens as Lens}
            onChange={setLens}
            options={[
              { value: "chart", label: "Chart", icon: "sitemap" },
              { value: "directory", label: "Directory", icon: "users" },
              { value: "charter", label: "Charter", icon: "flag" },
            ]}
          />
        }
      />

      {lens === "chart" && (
        <>
          {rootSeats.length > 0 && (
            <Panel title="Org-wide" icon="crown" subtitle="seats above every unit">
              <div className="org-seats">
                {rootSeats.map((seat) => (
                  <a
                    key={seat.handle}
                    className={`org-node${seat.kind === "human" ? " human" : ""}`}
                    href={href(["seats", seat.handle])}
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

      {lens === "directory" && (
        <Panel padding="none">
          <DataTable<Seat>
            rows={index.seats}
            rowKey={(s) => s.handle}
            defaultSort={{ key: "name", dir: "asc" }}
            empty={{ title: "No seats", hint: "Roles come from the company configuration." }}
            columns={[
              {
                key: "name",
                header: "Seat",
                sortValue: (s) => s.name,
                cell: (s) => (
                  <SeatChip name={s.name} handle={s.handle} human={s.kind === "human"} />
                ),
              },
              {
                key: "handle",
                header: "Handle",
                sortValue: (s) => s.handle,
                shrink: true,
                cell: (s) => <code className="inline">@{s.handle}</code>,
              },
              {
                key: "unit",
                header: "Unit",
                sortValue: (s) => s.unit?.name ?? "",
                cell: (s) => s.unit?.name ?? <span className="faint">org-wide</span>,
              },
              {
                key: "manager",
                header: "Reports to",
                sortValue: (s) => index.managerOf.get(s.name)?.name ?? "",
                cell: (s) => {
                  const m = index.managerOf.get(s.name);
                  return m ? (
                    <SeatChip name={m.name} handle={m.handle} />
                  ) : (
                    <span className="faint">—</span>
                  );
                },
              },
              {
                key: "reports",
                header: "Manages",
                align: "right",
                sortValue: (s) => index.reportsOf.get(s.name)?.length ?? 0,
                cell: (s) =>
                  index.reportsOf.get(s.name)?.length || <span className="faint">—</span>,
              },
              {
                key: "state",
                header: "State",
                shrink: true,
                sortValue: (s) =>
                  s.kind === "human" ? "human" : (seatFor(s.name)?.state ?? "offline"),
                cell: (s) =>
                  s.kind === "human" ? (
                    <Badge outline>human</Badge>
                  ) : (
                    <StateBadge agent={seatFor(s.name)} sandboxes={sandboxes} />
                  ),
              },
              {
                key: "doing",
                header: "Doing",
                cell: (s) => (
                  <span className="truncate t-caption">
                    {statusLine(seatFor(s.name), {
                      seat: s,
                      sandbox: sandboxes.find((b) => b.role === s.name) ?? null,
                    })}
                  </span>
                ),
              },
            ]}
          />
        </Panel>
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
