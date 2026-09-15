/**
 * Every tool a seat can call, grouped by where it came from.
 *
 * The origin grammar is two-valued — `builtin`, `mcp:<server>`, `a2a` — and it
 * is recorded at registration, which is the only frame that knows and the last
 * that can say.
 */

import { useMemo } from "react";
import { plural } from "~/lib/format.ts";
import { useParam } from "~/app/router.tsx";
import { Section } from "~/components/common.tsx";
import { Badge, Chip, Empty, Panel, SearchInput, Stat, StatRow } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { useTools } from "~/lib/store-hooks.ts";
import type { Capability } from "~/lib/tools.ts";
import {
  capabilityOf,
  capabilityTone,
  hintSentence,
  hintsAdvertised,
  schemaFields,
} from "~/lib/tools.ts";
import type { ToolRow } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

/** Worst first: what an operator auditing a new server reads down. */
const CAPABILITY_ORDER: Record<Capability, number> = {
  destroys: 0,
  writes: 1,
  unknown: 2,
  reads: 3,
};

function originOf(source: string): { kind: string; detail: string } {
  const idx = source.indexOf(":");
  if (idx < 0) return { kind: source || "unknown", detail: "" };
  return { kind: source.slice(0, idx), detail: source.slice(idx + 1) };
}

export function Tools({ server }: { server?: string }) {
  const tools = useTools();
  const [q, setQ] = useParam("q", "");
  const [origin, setOrigin] = useParam("origin", "");

  const origins = useMemo(() => {
    const map = new Map<string, number>();
    for (const t of tools) map.set(t.source, (map.get(t.source) ?? 0) + 1);
    return [...map.entries()].sort((a, b) => b[1] - a[1]);
  }, [tools]);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return tools
      .filter((t) => !origin || t.source === origin)
      .filter(
        (t) =>
          !needle ||
          t.name.toLowerCase().includes(needle) ||
          (t.description ?? "").toLowerCase().includes(needle),
      );
  }, [tools, q, origin]);

  const builtins = tools.filter((t) => t.source === "builtin").length;
  const mcp = tools.filter((t) => t.source.startsWith("mcp")).length;
  // HOW MANY OF THESE CAN WRITE, which is the number an operator opens this
  // screen for after adding a server — and which the catalogue could not
  // answer at all while it carried three strings per tool.
  const writes = tools.filter((t) => {
    const c = capabilityOf(t.annotations);
    return c === "writes" || c === "destroys";
  }).length;
  const unannotated = tools.filter((t) => hintsAdvertised(t.annotations) === 0).length;

  return (
    <>
      <PageActions>{<Badge outline>{plural(tools.length, "tool")} registered</Badge>}</PageActions>
      <PageNote>
        What the models can actually call. A planner sees only the server names; the tool names
        below are discovered and activated during a turn.
      </PageNote>

      <Panel padding="none">
        <StatRow cols={4}>
          <Stat icon="wrench" label="Total" value={tools.length} sub="across every origin" />
          <Stat icon="box" label="Built in" value={builtins} sub="shipped by the engine itself" />
          <Stat
            icon="plug"
            label="From MCP servers"
            value={mcp}
            sub={`${origins.filter(([s]) => s.startsWith("mcp")).length} server(s)`}
          />
          <Stat
            icon="alert"
            label="Can write"
            value={writes}
            sub={
              unannotated
                ? `${unannotated} more advertise nothing at all`
                : "every tool advertises what it does"
            }
          />
        </StatRow>
      </Panel>

      <div className="toolbar">
        <div style={{ maxWidth: 340, flex: 1 }}>
          <SearchInput
            value={q}
            onChange={setQ}
            ariaLabel="Search tools"
            placeholder="Search by name or description"
          />
        </div>
        <span className="spacer" />
        <Chip on={!origin} onClick={() => setOrigin("")}>
          All
        </Chip>
        {origins.map(([source, count]) => (
          <Chip
            key={source}
            on={origin === source}
            count={count}
            onClick={() => setOrigin(origin === source ? "" : source)}
          >
            {source}
          </Chip>
        ))}
      </div>

      {!tools.length ? (
        <Empty
          icon="wrench"
          title="No tools are registered"
          hint="Builtins register at boot; MCP tools are discovered from the servers in mcp_servers. An engine with no active configuration has neither."
        />
      ) : (
        <Panel padding="none">
          <DataTable<ToolRow>
            rows={rows}
            rowKey={(t) => `${t.source}:${t.name}`}
            defaultSort={{ key: "name", dir: "asc" }}
            empty={{ title: `No tool matches “${q}”` }}
            columns={[
              {
                key: "name",
                header: "Tool",
                // SIZED TO THE NAME. With the columns this table grew, the
                // name column took whatever was left and broke
                // `comment_on_work_item` across three lines mid-word — an
                // identifier a reader is matching against a config file, so
                // a break in the middle of one is worse than a narrower
                // description beside it.
                shrink: true,
                sortValue: (t) => t.name,
                cell: (t) => <code className="inline nowrap">{t.name}</code>,
              },
              {
                key: "origin",
                header: "Origin",
                shrink: true,
                sortValue: (t) => t.source,
                cell: (t) => {
                  const { kind, detail } = originOf(t.source);
                  return (
                    <span className="row gap-1">
                      <Badge outline>{kind}</Badge>
                      {detail && <span className="t-caption mono">{detail}</span>}
                    </span>
                  );
                },
              },
              {
                key: "desc",
                header: "What it does",
                cell: (t) => (
                  <span className="col" style={{ gap: 2 }}>
                    <span className="t-caption">
                      {t.description || <span className="faint">no description</span>}
                    </span>
                    <span className="t-caption faint">{hintSentence(t.annotations)}</span>
                  </span>
                ),
              },
              {
                key: "can",
                header: "Can",
                shrink: true,
                // Sorted worst-first, which is the order an operator
                // auditing a new server reads: the tools that can destroy
                // something are the ones they have to decide about.
                sortValue: (t) => CAPABILITY_ORDER[capabilityOf(t.annotations)],
                cell: (t) => {
                  const c = capabilityOf(t.annotations);
                  return (
                    <span className="row gap-1">
                      <Badge tone={capabilityTone(c)}>{c}</Badge>
                      {t.annotations?.open_world === "yes" && <Badge outline>outside</Badge>}
                    </span>
                  );
                },
              },
              {
                key: "delivers",
                header: "Delivers to",
                shrink: true,
                sortValue: (t) => t.delivers ?? "",
                cell: (t) =>
                  t.delivers ? (
                    <Badge outline>{t.delivers}</Badge>
                  ) : (
                    <span className="faint t-caption">nobody</span>
                  ),
              },
              {
                key: "args",
                header: "Arguments",
                shrink: true,
                align: "right",
                sortValue: (t) => schemaFields(t).length,
                cell: (t) => {
                  const fields = schemaFields(t);
                  if (!fields.length) {
                    return <span className="faint t-caption">none</span>;
                  }
                  const required = fields.filter((f) => f.required).length;
                  return (
                    <span
                      className="t-caption"
                      title={fields
                        .map((f) => `${f.name}${f.required ? "*" : ""}: ${f.type || "—"}`)
                        .join("\n")}
                    >
                      {fields.length}
                      {required > 0 && <span className="faint"> · {required} required</span>}
                    </span>
                  );
                },
              },
            ]}
          />
        </Panel>
      )}

      <Section title="How a model reaches these">
        <div className="banner neutral">
          <span className="col" style={{ gap: 4 }}>
            <span>
              An executor prompt lists <strong>server names</strong>, not tool names — a role with
              50–150 MCP tools would push 15–25 KB of catalogue into every call.
            </span>
            <span className="t-caption">
              The model calls <code className="inline">list_mcp_server_tools(server)</code> to see
              what a server offers, then <code className="inline">activate_tool(name)</code> to
              promote one into the schemas it can actually invoke. It activates what it needs the
              moment it needs it — there is no separate planning pass to name a tool in advance.
            </span>
          </span>
        </div>
      </Section>
    </>
  );
}
