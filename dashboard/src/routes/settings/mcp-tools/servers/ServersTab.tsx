/**
 * Settings › MCP & tools, the servers: what is wrong (the engine's alarms),
 * how the servers stand in four figures, then every server with how it
 * connects, its condition, its tools, who reaches it and what last went
 * wrong.
 *
 * EVERY SERVER LISTS ITS TOOLS, per-agent ones too: the engine merges each
 * node's listing, so a per-agent server's tools show although no shared
 * instance of it exists. Status is health alone; whether anybody uses a
 * server is the Used by column's question.
 */

import {
  Button,
  Callout,
  Card,
  IconButton,
  StatCard,
  StatGroup,
  TableTwoLineCell,
  Tooltip,
  VisuallyHidden,
} from "@crewlethq/ui";
import {
  ActivityGlyph,
  PencilGlyph,
  PlusGlyph,
  ServerGlyph,
  ShieldCheckGlyph,
  TriangleAlertGlyph,
  WrenchGlyph,
} from "@crewlethq/icons/glyphs";
import type { McpMemberServerStatus } from "~/contract/mcp.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { href } from "~/app/router.tsx";
import { listed, plural } from "~/lib/format.ts";
import type { McpData } from "../data.ts";
import {
  LastError,
  ReachCell,
  ServerMark,
  ServerMenu,
  StateLine,
  ToolsCell,
  whereWords,
  type OnFlow,
} from "../parts.tsx";
import { Alarms } from "./Alarms.tsx";

/** A remote server's host, for the line under its name; the address whole on hover. */
function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

/**
 * The server's mark, its name and where it is, each one line; the address
 * whole on hover. A local server's command line is a literal, so it is set in
 * the mono face, as Models sets a model id: in the sans face `--` and `=` read
 * as prose. Where it is, is an administrator's to read: a member reads what
 * the server is for.
 */
function ServerCell({ server, data }: { server: McpMemberServerStatus; data: McpData }) {
  const row = data.adminRow(server.name);
  const command = row?.command ? [row.command, ...row.args].join(" ") : "";
  const where = row ? (row.url ? hostOf(row.url) : command) : "";
  return (
    <Tooltip
      content={
        <span className="mono">{row ? row.url || command || server.name : server.title}</span>
      }
      placement="top"
    >
      <a
        className="mcp-row-reveal"
        href={href(["settings", "mcp-tools", "servers", server.name])}
        tabIndex={-1}
        aria-hidden="true"
      >
        <TableTwoLineCell
          mark={<ServerMark server={server} />}
          value={server.title}
          detail={where || server.description || undefined}
          titleWhenCut={false}
          {...(row && !row.url && command ? { mono: "detail" as const } : {})}
        />
      </a>
    </Tooltip>
  );
}

/** The page's primary action, for an admin. */
export function AddServerButton({ onFlow }: { onFlow: OnFlow }) {
  return (
    <Button
      leadingIcon={<PlusGlyph size="sm" />}
      variant="primary"
      size="small"
      onClick={() => onFlow("add")}
    >
      Add server
    </Button>
  );
}

/** The read-only note, the alarms and the four figures, over the servers table. */
function ServersSummary({ data, onFlow }: { data: McpData; onFlow: OnFlow }) {
  const { servers, admin } = data;
  const enabled = servers.filter((s) => s.enabled);
  const running = enabled.filter((s) => s.state === "running").length;
  const notRunning = enabled.length - running;
  const tools = servers.reduce((n, s) => n + s.tools, 0);
  const refused = servers.reduce((n, s) => n + s.refused_count, 0);
  // WHAT WAITS ON AN ADMINISTRATOR: the calls held for an answer, and the
  // servers whose changed tools wait for a review (the engine's alarm on each).
  const waiting =
    data.answer?.reading === "admin"
      ? data.answer.approvals.filter((a) => a.status === "waiting").length
      : 0;
  const changed = data.alarms
    .filter((a) => a.kind === "mcp_tools_changed")
    .reduce((n, a) => n + a.entries.length, 0);
  return (
    <>
      {/* One line: the page already shows a reader with no actions what
          they may do, so the note says only why. */}
      {!admin ? <Callout variant="info">Read-only. Changes take Admin access.</Callout> : null}

      {admin ? <Alarms data={data} onFlow={onFlow} /> : null}

      <Card padding="none">
        <StatGroup columns={admin ? 4 : 3}>
          <StatCard
            icon={<ServerGlyph size="xs" />}
            label="Servers"
            value={servers.length}
            sub={
              servers.length === enabled.length
                ? "every one enabled"
                : `${servers.length - enabled.length} disabled`
            }
          />
          <StatCard
            icon={<ActivityGlyph size="xs" />}
            label="Running"
            value={running}
            unit={`of ${enabled.length}`}
            sub={
              notRunning === 0
                ? "every enabled server answers"
                : `${plural(notRunning, "server")} not fully running`
            }
            subTone={notRunning > 0 ? "warning" : undefined}
          />
          <StatCard
            icon={<WrenchGlyph size="xs" />}
            label="MCP tools"
            value={tools}
            sub={refused > 0 ? `${refused} not offered` : "every one offered"}
            subTone={refused > 0 ? "warning" : undefined}
          />
          {/* Calls and changes wait on an administrator: a member answers
              neither, so the figure is not theirs to read. */}
          {admin ? (
            <StatCard
              icon={
                waiting + changed > 0 ? (
                  <TriangleAlertGlyph size="xs" />
                ) : (
                  <ShieldCheckGlyph size="xs" />
                )
              }
              label="Waiting on you"
              value={waiting + changed}
              sub={
                waiting + changed === 0
                  ? "no call or change to review"
                  : listed([
                      ...(waiting ? [`${plural(waiting, "call")} to approve`] : []),
                      ...(changed ? [`changed tools on ${plural(changed, "server")}`] : []),
                    ])
              }
              subTone={waiting + changed > 0 ? "warning" : undefined}
            />
          ) : null}
        </StatGroup>
      </Card>
    </>
  );
}

/** Every server, a row each. */
function ServersTable({ data, onFlow }: { data: McpData; onFlow: OnFlow }) {
  const { servers, admin } = data;
  return (
    <DataGrid<McpMemberServerStatus>
      name="servers"
      rows={[...servers]}
      rowKey={(s) => s.name}
      rowHref={(s) => href(["settings", "mcp-tools", "servers", s.name])}
      empty={{
        title: "No MCP server yet",
        hint: admin
          ? "Add one from the catalogue, by its address, or as a local command."
          : "An admin adds the servers the agents use.",
        icon: "server",
        ...(admin
          ? {
              action: (
                <Button variant="primary" size="small" onClick={() => onFlow("add")}>
                  Add server
                </Button>
              ),
            }
          : {}),
      }}
      columns={[
        {
          key: "server",
          header: "Server",
          // THE TABLE'S BUDGET AT A DESKTOP WIDTH: seven columns in the
          // settings page's 905px at 1440. The content-sized columns take
          // what their words need, and the three flexible ones share the rest
          // from their floors (11 + 9 + 10rem), so nothing gives way at 1280
          // and wider; a host past the name's width is whole on hover.
          floor: "11rem",
          sortValue: (s) => s.title,
          cell: (s) => <ServerCell server={s} data={data} />,
        },
        // CONTENT-SIZED where the words are a short, known set (Connection,
        // Status, Tools): each takes what its longest value and its head need,
        // so neither is cut, and the width left goes to the columns that carry
        // names and errors.
        {
          key: "where",
          header: "Connection",
          shrink: true,
          drop: 1,
          sortValue: (s) => whereWords(s).scope,
          cell: (s) => {
            const w = whereWords(s);
            return <TableTwoLineCell value={w.where} detail={w.scope} />;
          },
        },
        {
          key: "status",
          header: "Status",
          shrink: true,
          sortValue: (s) => s.state,
          cell: (s) => <StateLine server={s} />,
        },
        {
          key: "tools",
          header: "Tools",
          align: "right",
          shrink: true,
          sortValue: (s) => s.tools,
          cell: (s) => <ToolsCell server={s} />,
        },
        // WHO A SERVER REACHES, where the engine said: it pushes each agent
        // seat's grants to an administrator alone, and a column of "No agent"
        // for a member would be a claim nobody made.
        ...(data.reachKnown
          ? [
              {
                key: "reach",
                header: "Used by",
                floor: "9rem",
                drop: 3,
                cell: (s: McpMemberServerStatus) => <ReachCell server={s} data={data} />,
              },
            ]
          : []),
        {
          key: "error",
          header: "Last error",
          floor: "10rem",
          drop: 2,
          cell: (s) => <LastError server={s} data={data} />,
        },
        ...(admin
          ? [
              {
                key: "actions",
                header: <VisuallyHidden>Actions</VisuallyHidden>,
                action: true,
                shrink: true,
                cell: (s: McpMemberServerStatus) =>
                  s.configured ? (
                    <span className="mcp-row-actions">
                      <IconButton
                        className="mcp-row-edit"
                        size="sm"
                        variant="ghost"
                        label={`Edit ${s.title}`}
                        icon={<PencilGlyph size="sm" />}
                        onClick={() => onFlow("edit", s.name, { step: "connection" })}
                      />
                      <ServerMenu server={s} onFlow={onFlow} />
                    </span>
                  ) : null,
              },
            ]
          : []),
      ]}
    />
  );
}

/** The Servers tab: the summary over the servers table in its card. */
export function ServersTab({ data, onFlow }: { data: McpData; onFlow: OnFlow }) {
  return (
    <div className="mcp-page">
      <ServersSummary data={data} onFlow={onFlow} />
      <Card padding="none">
        <Card.Header icon={<ServerGlyph size="sm" />}>
          <Card.Title>Servers</Card.Title>
        </Card.Header>
        <ServersTable data={data} onFlow={onFlow} />
      </Card>
    </div>
  );
}
