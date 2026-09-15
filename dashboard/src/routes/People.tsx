/**
 * Everyone in the company, and what each of them is doing.
 *
 * The list is grouped by RUN STATE by default rather than by unit, because the
 * question this screen is opened with is "who is working / who stopped", and a
 * unit grouping buries a broken seat among healthy colleagues. Grouping by
 * unit is one control away for when the question is the org instead.
 *
 * Sorting is by NAME within a group, never by a live timestamp. The screen this
 * replaces sorted on `sandbox.updated_at || agent.updated_at` — a field that
 * moves on every push — so rows physically re-ordered under the reader's
 * cursor several times a second while a turn ran.
 */

import { useId, useMemo } from "react";
import { plural } from "~/lib/format.ts";
import { useParam } from "~/app/router.tsx";
import { SeatCard } from "~/components/common.tsx";
import { useAgents, useOrg, useSandboxes } from "~/lib/store-hooks.ts";
import { indexOrg, runState, type Seat } from "~/lib/seats.ts";
import type { AgentRow } from "~/protocol/index.ts";
import { GroupGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
import {
  AutoGrid,
  EmptyState,
  Input,
  PageHeader,
  Section,
  SegmentedControl,
  TabPanel,
  Tag,
  Toolbar,
} from "@crewlethq/ui";

type Grouping = "state" | "unit" | "flat";

const STATE_ORDER = [
  { key: "needs", label: "Waiting on a person" },
  { key: "broken", label: "Stopped" },
  { key: "working", label: "Working" },
  { key: "idle", label: "Idle" },
  { key: "offline", label: "Not running here" },
  { key: "human", label: "Human teammates" },
] as const;

function bucketOf(
  seat: Seat,
  agent: AgentRow | undefined,
  sandboxes: ReturnType<typeof useSandboxes>,
): string {
  if (seat.kind === "human") return "human";
  const sandbox = sandboxes.find((s) => s.role === seat.name);
  if (sandbox?.status === "awaiting_input") return "needs";
  if (agent?.last_error) return "broken";
  const state = runState(agent, sandboxes);
  if (state === "afk" || state === "failed") return "broken";
  if (state === "working" || state === "awaiting_sandbox") return "working";
  if (state === "idle") return "idle";
  return "offline";
}

export function People() {
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const org = useOrg();
  const [group, setGroup] = useParam("group", "state", "section");
  const panel = useId();
  const [q, setQ] = useParam("q", "");

  const index = useMemo(() => indexOrg(org), [org]);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return (
      index.seats
        .filter(
          (s) =>
            !needle ||
            s.name.toLowerCase().includes(needle) ||
            s.handle.toLowerCase().includes(needle) ||
            s.goal.toLowerCase().includes(needle) ||
            s.unit?.name.toLowerCase().includes(needle),
        )
        .map((seat) => ({ seat, agent: agents.find((a) => a.role === seat.name) }))
        // By NAME. Never by a field that a live push moves.
        .sort((a, b) => a.seat.name.localeCompare(b.seat.name))
    );
  }, [index.seats, agents, q]);

  const groups = useMemo(() => {
    if (group === "flat") return [{ key: "all", label: "", rows }];
    if (group === "unit") {
      const byUnit = new Map<string, typeof rows>();
      for (const row of rows) {
        const key = row.seat.unit?.name ?? "No unit — org-wide";
        byUnit.set(key, [...(byUnit.get(key) ?? []), row]);
      }
      return [...byUnit.entries()]
        .sort((a, b) => a[0].localeCompare(b[0]))
        .map(([key, list]) => ({ key, label: key, rows: list }));
    }
    return STATE_ORDER.map((b) => ({
      key: b.key,
      label: b.label,
      rows: rows.filter((r) => bucketOf(r.seat, r.agent, sandboxes) === b.key),
    })).filter((g) => g.rows.length > 0);
  }, [rows, group, sandboxes]);

  const agentSeats = index.seats.filter((s) => s.kind === "agent").length;

  return (
    <>
      <PageHeader
        title="People"
        description="Every seat in the company, the ones this node runs and the ones its peers do. A seat that is not held anywhere reads as “not running here”."
        badges={
          <>
            <Tag appearance="outline">{plural(agentSeats, "agent seat")}</Tag>
            {index.seats.length - agentSeats > 0 && (
              <Tag appearance="outline">{plural(index.seats.length - agentSeats, "human")}</Tag>
            )}
          </>
        }
      />

      <Toolbar label="Seat filters" mode="group" sticky>
        <Input
          type="search"
          width="md"
          value={q}
          onChange={(event) => setQ(event.target.value)}
          onClear={() => setQ("")}
          aria-label="Filter seats"
          placeholder="Filter by name, handle, goal or unit"
          leading={<SearchGlyph size="sm" />}
        />
        <span className="spacer" />
        <SegmentedControl<Grouping>
          label="Grouping"
          semantics="tabs"
          panelId={panel}
          value={group as Grouping}
          onValueChange={setGroup}
          options={[
            { value: "state", label: "By state" },
            { value: "unit", label: "By unit" },
            { value: "flat", label: "Flat" },
          ]}
        />
      </Toolbar>

      <TabPanel id={panel} value={group}>
        {!groups.length && (
          <EmptyState
            icon={<GroupGlyph />}
            title={q ? `No seat matches “${q}”` : "This company has no seats"}
            description={
              q
                ? "The filter matches a seat's name, handle, goal or unit."
                : "Roles are defined in the company configuration. Import one to spawn seats."
            }
          />
        )}

        {groups.map((g) =>
          g.label ? (
            <Section key={g.key} title={g.label} description={`${g.rows.length}`}>
              <AutoGrid min="lg" gap={3}>
                {g.rows.map(({ seat, agent }) => (
                  <SeatCard key={seat.key} seat={seat} agent={agent} sandboxes={sandboxes} />
                ))}
              </AutoGrid>
            </Section>
          ) : (
            <AutoGrid min="lg" gap={3} key={g.key}>
              {g.rows.map(({ seat, agent }) => (
                <SeatCard key={seat.key} seat={seat} agent={agent} sandboxes={sandboxes} />
              ))}
            </AutoGrid>
          ),
        )}
      </TabPanel>
    </>
  );
}
