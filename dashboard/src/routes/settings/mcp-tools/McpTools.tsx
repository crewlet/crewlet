/**
 * Settings › MCP & tools: every MCP server the company runs, what each is
 * doing, which agents reach it, the tools it serves and how they were used,
 * and the tools the engine runs itself.
 *
 * PAGE TABS. The page names itself (a header with one plain line and the
 * primary action), and its views are underline tabs under it (`?view=`),
 * drawn as a server's own page draws its tabs, so the list and the pages it
 * opens move the same way. A server's page is `servers/{name}`; a flow on a
 * server is the address (`?flow=`), so an alarm's fix and a menu item open
 * the same dialog.
 *
 * EVERY MEMBER'S. A member reads each server's condition, its tools and how
 * often they were called, in the reading the engine answers them, and is
 * offered no gesture; an administrator reads the launch lines, the alarms and
 * the recent calls, and every gesture is theirs.
 */

import { useEffect, useMemo } from "react";
import { EmptyState, PageHeader, Skeleton, Tabs } from "@crewlethq/ui";
import { ServerGlyph } from "@crewlethq/icons/glyphs";
import { refToken } from "~/app/frame/objects.ts";
import { QueryState } from "~/components/common.tsx";
import { usePeekControls } from "~/app/frame/peekAddress.ts";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { useTools } from "~/lib/store-hooks.ts";
import { AddMcpServerDialog } from "../AddMcpServerDialog.tsx";
import { BuiltinCatalogue, builtinRows } from "./builtin/Catalogue.tsx";
import { useMcpData, type McpData } from "./data.ts";
import { Flows } from "./flows/Flows.tsx";
import { useFlow, type OnFlow } from "./parts.tsx";
import { ServerPage } from "./server/ServerPage.tsx";
import { AddServerButton, ServersTab } from "./servers/ServersTab.tsx";
import "~/styles/mcp.css";

/** The views of the list page, as `?view=` names them (servers is no key at all). */
type View = "servers" | "builtin";
const VIEWS: readonly View[] = ["servers", "builtin"];

/** The view the address names, and the way to another; a press on one pushes. */
function useView(): [View, (next: View) => void] {
  const [raw, set] = useParam("view", "", "section");
  const view = (VIEWS as readonly string[]).includes(raw) ? (raw as View) : "servers";
  return [view, (next) => set(next === "servers" ? "" : next)];
}

/**
 * A tool's own address (`#/settings/mcp-tools/<name>`, which `objects.ts`
 * builds for a tool) said the way this page shows a tool: a built-in is the
 * catalogue with the tool open in the drawer, a server's tool is its server's
 * Tools tab searched for it. Replaces, so Back does not land on the spelling
 * nobody chose. Waits for the pushed catalogue, which says which it is.
 */
function useCanonicalTool(tool: string, data: McpData): "waiting" | "unknown" | "moved" {
  const nav = useNavigator();
  const pushed = useTools();
  const builtin = useMemo(
    () => builtinRows(data.builtin).some((r) => r.tool.name === tool),
    [data.builtin, tool],
  );
  const row = pushed.find((t) => t.name === tool) ?? null;
  const server = row?.source.startsWith("mcp:") ? row.source.slice(4) : "";
  const known = builtin || server !== "";
  const cold = data.loading || pushed.length === 0;
  useEffect(() => {
    if (!tool || cold || !known) return;
    if (builtin) {
      nav.replace(["settings", "mcp-tools"], {
        view: "builtin",
        peek: refToken({ kind: "tool", id: tool }),
      });
    } else {
      nav.replace(["settings", "mcp-tools", "servers", server], { tab: "tools", q: tool });
    }
    // The address and what the catalogue says of it are the only inputs.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tool, cold, known, builtin, server]);
  if (!tool) return "moved";
  return cold ? "waiting" : known ? "moved" : "unknown";
}

/** The list page: the header, the tabs and the view the address names. */
function ListPage({ data, onFlow }: { data: McpData; onFlow: OnFlow }) {
  const [view, setView] = useView();
  const peek = usePeekControls();
  const alarms = data.alarms.reduce((n, a) => n + a.entries.length, 0);
  const builtin = data.builtin.reduce((n, g) => n + g.tools.length, 0);
  return (
    <div className="mcp-page">
      <PageHeader
        title="MCP & tools"
        description="The MCP servers your agents reach, and the tools the engine runs itself."
        actions={data.admin ? <AddServerButton onFlow={onFlow} /> : undefined}
      />
      <div className="tab-detail">
        <Tabs
          variant="underline"
          ariaLabel="MCP & tools sections"
          value={view}
          onValueChange={(v) => setView(v as View)}
          items={[
            {
              value: "servers",
              label: "Servers",
              count: data.servers.length || null,
              ...(alarms > 0
                ? { marker: { tone: "warning" as const, label: `${alarms} need attention` } }
                : {}),
            },
            { value: "builtin", label: "Built-in tools", count: builtin || null },
          ]}
        />
        {view === "servers" ? <ServersTab data={data} onFlow={onFlow} /> : null}
        {view === "builtin" ? (
          <BuiltinCatalogue
            groups={data.builtin}
            onOpen={(name) => peek.open({ kind: "tool", id: name })}
          />
        ) : null}
      </div>
    </div>
  );
}

export function McpTools({
  server,
  tool,
}: {
  server?: string | undefined;
  tool?: string | undefined;
}) {
  const data = useMcpData();
  const nav = useNavigator();
  const flow = useFlow();
  const canonical = useCanonicalTool(tool ?? "", data);
  const target = server ?? flow.server;

  const open: OnFlow = (next, name, args = {}) => {
    if (name && server && name !== server) {
      nav.to(["settings", "mcp-tools", "servers", name], { flow: next, ...args });
      return;
    }
    flow.open(next, { ...(name && !server ? { server: name } : {}), ...args });
  };

  if (data.loading || canonical === "waiting") {
    return <Skeleton variant="text" rows={8} label="Loading MCP servers" />;
  }
  if (data.failure && !data.answer) {
    return <QueryState {...data.failure} loading={false} />;
  }
  if (tool && canonical === "unknown") {
    return (
      <EmptyState
        icon={<ServerGlyph size="xl" />}
        title={`No tool called ${tool}`}
        description="Its server may have stopped, or the tool was renamed."
        action={
          <a className="t-link" href={href(["settings", "mcp-tools"], { view: "builtin" })}>
            Every built-in tool
          </a>
        }
      />
    );
  }

  const page = server ? data.server(server) : null;
  return (
    <>
      {server && page ? <ServerPage server={page} data={data} onFlow={open} /> : null}
      {server && !page ? (
        <EmptyState
          icon={<ServerGlyph size="xl" />}
          title={`No MCP server called ${server}`}
          description="It may have been removed."
          action={
            <a className="t-link" href={href(["settings", "mcp-tools"])}>
              Every server
            </a>
          }
        />
      ) : null}
      {!server ? <ListPage data={data} onFlow={open} /> : null}

      {/* THE SHIPPED ADD DIALOG, until the wizard replaces it. */}
      {data.admin && flow.flow === "add" ? (
        <AddMcpServerDialog
          taken={new Set(data.servers.filter((s) => s.configured).map((s) => s.name))}
          nodeOnly={new Set(data.servers.filter((s) => !s.configured).map((s) => s.name))}
          onClose={flow.close}
          onAdded={(name) => {
            nav.to(["settings", "mcp-tools", "servers", name]);
            data.reload();
          }}
        />
      ) : null}
      {data.admin && target && flow.flow && flow.flow !== "add" ? (
        <Flows
          data={data}
          name={target}
          flow={flow}
          onRemoved={() => nav.to(["settings", "mcp-tools"])}
        />
      ) : null}
    </>
  );
}
