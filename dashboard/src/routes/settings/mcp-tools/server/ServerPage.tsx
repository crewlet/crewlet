/**
 * One MCP server: its condition and what to do about it, how it connects,
 * its tools, and how they were used. Four tabs (`?tab=`), each read from the
 * server's own answer (`GET /mcp-servers/{name}`), its head from the list's
 * row so the page draws at once.
 */

import { Skeleton, Tabs } from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import type { McpMemberServerStatus } from "~/contract/mcp.ts";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { useParam } from "~/app/router.tsx";
import { usePageLabels, usePageMenu } from "~/app/Shell.tsx";
import { plural } from "~/lib/format.ts";
import { useServerDetail, type McpData } from "../data.ts";
import { ServerMark, ServerMenu, StateLine, whereWords, type OnFlow } from "../parts.tsx";
import { ActivityTab } from "./ActivityTab.tsx";
import { ConnectionTab } from "./ConnectionTab.tsx";
import { OverviewTab } from "./OverviewTab.tsx";
import { ToolsTab } from "./ToolsTab.tsx";

const TABS = ["overview", "connection", "tools", "activity"] as const;
type Tab = (typeof TABS)[number];

export function ServerPage({
  server,
  data,
  onFlow,
}: {
  server: McpMemberServerStatus;
  data: McpData;
  onFlow: OnFlow;
}) {
  const [raw, setTab] = useParam("tab", "", "section");
  const tab: Tab = (TABS as readonly string[]).includes(raw) ? (raw as Tab) : "overview";
  const detail = useServerDetail(server.name);
  const w = whereWords(server);
  const reach = data.reaches(server.name);
  const toolCount = detail.data?.tool_count ?? server.tools;
  // The trail names the server as its head does ("GitHub"), not by its key.
  usePageLabels({ [server.name]: server.title });
  const gestures = data.admin && server.configured;
  usePageMenu(
    gestures
      ? [
          ...(server.enabled
            ? [
                {
                  key: "restart",
                  label: `Restart ${server.title}`,
                  onSelect: () => onFlow("restart"),
                },
              ]
            : []),
          {
            key: "disable",
            label: `${server.enabled ? "Disable" : "Enable"} ${server.title}`,
            onSelect: () => onFlow("disable"),
          },
          {
            key: "remove",
            label: `Remove ${server.title}`,
            onSelect: () => onFlow("remove"),
          },
        ]
      : [],
  );
  const facts: Fact[] = [
    { label: "Status", value: <StateLine server={server} /> },
    { label: "Connection", value: `${w.where}, ${w.scope.toLowerCase()}` },
    { label: "Tools", value: toolCount > 0 ? String(toolCount) : "None listed" },
    ...(reach
      ? [{ label: "Reaches", value: reach.length > 0 ? plural(reach.length, "agent") : "No agent" }]
      : []),
  ];
  const answer = detail.data;
  return (
    <div className="mcp-page">
      {gestures ? (
        <PageActions>
          <ServerMenu server={server} onFlow={onFlow} />
        </PageActions>
      ) : null}
      <ObjectHeader
        kind={server.transport === "http" ? "Remote MCP server" : "Local MCP server"}
        icon="server"
        mark={<ServerMark server={server} />}
        title={server.title}
        identifier={server.name}
        facts={facts}
        {...(server.description ? { description: server.description } : {})}
      />
      <div className="tab-detail">
        {/* What does not fit a phone's width folds into "More", the kit's
            answer to a row that scrolls with its bar hidden and cuts its last
            tab off with nothing to say it is there. */}
        <Tabs
          variant="underline"
          overflow="menu"
          ariaLabel={`${server.title} sections`}
          value={tab}
          onValueChange={(v) => setTab(v === "overview" ? "" : v)}
          items={[
            { value: "overview", label: "Overview" },
            { value: "connection", label: "Connection" },
            {
              value: "tools",
              label: "Tools",
              count: toolCount || null,
              ...(server.refused_count > 0
                ? {
                    marker: {
                      tone: "warning" as const,
                      label: `${server.refused_count} not offered`,
                    },
                  }
                : {}),
            },
            { value: "activity", label: "Activity" },
          ]}
        />
        {answer === null ? (
          detail.code && !detail.loading ? (
            <QueryState
              error={detail.code}
              refusal={detail.refusal}
              detail={detail.error?.sentence || detail.error?.detail || ""}
              loading={false}
            />
          ) : (
            <Skeleton variant="text" rows={8} label={`Loading ${server.title}`} />
          )
        ) : (
          <>
            {tab === "overview" ? (
              <OverviewTab server={server} detail={answer} data={data} onFlow={onFlow} />
            ) : null}
            {tab === "connection" ? (
              <ConnectionTab server={server} detail={answer} data={data} />
            ) : null}
            {tab === "tools" ? <ToolsTab server={server} detail={answer} /> : null}
            {tab === "activity" ? <ActivityTab detail={answer} data={data} /> : null}
          </>
        )}
      </div>
    </div>
  );
}
