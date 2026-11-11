/**
 * One MCP server: its condition and what to do about it, how it connects,
 * its tools and their levels, who gets it, and how it was used. Five tabs
 * (`?tab=`), each read from the server's own answer (`GET /mcp-servers/{name}`),
 * its head from the list's row so the page draws at once. Access is the
 * administrator's: who a server reaches is on their reading alone.
 */

import { Button, Skeleton, Tabs } from "@crewlethq/ui";
import { PencilGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import type { McpMemberServerStatus } from "~/contract/mcp.ts";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { useParam } from "~/app/router.tsx";
import { usePageLabels, usePageMenu } from "~/app/Shell.tsx";
import { plural } from "~/lib/format.ts";
import { useServerDetail, type McpData } from "../data.ts";
import { ServerMark, ServerMenu, StateLine, whereWords, type OnFlow } from "../parts.tsx";
import { AccessTab } from "./AccessTab.tsx";
import { ActivityTab } from "./ActivityTab.tsx";
import { ConnectionTab } from "./ConnectionTab.tsx";
import { OverviewTab } from "./OverviewTab.tsx";
import { ToolsTab } from "./ToolsTab.tsx";

const TABS = ["overview", "connection", "tools", "access", "activity"] as const;
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
  const detail = useServerDetail(server.name);
  const answer = detail.data;
  // ACCESS IS THE ADMINISTRATOR'S: a member's reading carries no reach.
  const tabs: readonly Tab[] = data.admin ? TABS : TABS.filter((t) => t !== "access");
  const tab: Tab = (tabs as readonly string[]).includes(raw) ? (raw as Tab) : "overview";
  const w = whereWords(server);
  const reach = data.reaches(server.name);
  const toolCount = detail.data?.tools.length ?? server.tools;
  // The trail names the server as its head does ("GitHub"), not by its key.
  usePageLabels({ [server.name]: server.title });
  const gestures = data.admin && server.configured;
  usePageMenu(
    gestures
      ? [
          ...(server.enabled
            ? [
                {
                  key: "test",
                  label: `Test ${server.title}`,
                  onSelect: () => onFlow("test"),
                },
              ]
            : []),
          {
            key: "edit",
            label: `Edit ${server.title}`,
            onSelect: () => onFlow("edit", undefined, { step: "connection" }),
          },
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
    // ONE LINE, as a token is: "Remote, company-shared" broken at its hyphen
    // read as two facts.
    { label: "Connection", value: `${w.where}, ${w.scope.toLowerCase()}`, token: true },
    { label: "Tools", value: toolCount > 0 ? String(toolCount) : "None listed" },
    ...(reach
      ? [{ label: "Reaches", value: reach.length > 0 ? plural(reach.length, "agent") : "No agent" }]
      : []),
  ];
  const waiting =
    data.answer?.reading === "admin"
      ? data.answer.approvals.filter((a) => a.server === server.name && a.status === "waiting")
          .length
      : 0;
  const reached = answer?.reading === "admin" ? answer.reaches.length : (reach?.length ?? 0);
  return (
    <div className="mcp-page">
      {gestures ? (
        <PageActions>
          <Button
            size="small"
            variant="secondary"
            leadingIcon={<PencilGlyph size="sm" />}
            onClick={() => onFlow("edit", undefined, { step: "connection" })}
          >
            Edit
          </Button>
          {/* On a phone the menu folds into the bar's "More", which carries the same actions. */}
          <span className="page-action-folds">
            <ServerMenu server={server} onFlow={onFlow} />
          </span>
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
            {
              value: "overview",
              label: "Overview",
              ...(waiting > 0
                ? { marker: { tone: "warning" as const, label: `${waiting} waiting for approval` } }
                : {}),
            },
            { value: "connection", label: "Connection" },
            {
              value: "tools",
              label: "Tools",
              count: toolCount || null,
              ...(answer?.reading === "admin" && answer.changes.length > 0
                ? {
                    marker: {
                      tone: "changed" as const,
                      label: `${answer.changes.length} changed`,
                    },
                  }
                : server.refused_count > 0
                  ? {
                      marker: {
                        tone: "warning" as const,
                        label: `${server.refused_count} not offered`,
                      },
                    }
                  : {}),
            },
            ...(data.admin ? [{ value: "access", label: "Access", count: reached || null }] : []),
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
              <ConnectionTab server={server} detail={answer} data={data} onFlow={onFlow} />
            ) : null}
            {tab === "tools" ? (
              <ToolsTab
                server={server}
                detail={answer}
                onChanged={() => void detail.reload({ quiet: true })}
                onFlow={onFlow}
              />
            ) : null}
            {tab === "access" && answer.reading === "admin" ? (
              <AccessTab
                server={server}
                detail={answer}
                data={data}
                onSaved={() => {
                  void detail.reload({ quiet: true });
                  data.reload();
                }}
              />
            ) : null}
            {tab === "activity" ? <ActivityTab detail={answer} data={data} /> : null}
          </>
        )}
      </div>
    </div>
  );
}
