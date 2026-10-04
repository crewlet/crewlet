/**
 * The MCP servers on Settings › Tools & MCP: each one's launch, who it reaches,
 * and what every live node did with it.
 *
 * # Everything here is the engine's
 *
 * The STATE and the per-node counts are `mcp_servers_status`, which the engine
 * reads off every node's presence heartbeat — one listing, every node's answer,
 * so there is no node that "did not answer in time", only one whose build does
 * not report, drawn as unknown rather than as zero. WHO a server reaches is
 * each agent seat's `tool_sources` on the pushed org (`lib/mcpServers.ts`
 * `grantedSeats`). This component derives neither; it draws both.
 *
 * # Operator-only, and the rest of the screen is not
 *
 * The answer names nodes, launch commands and the first line of each failure,
 * so the engine refuses it to a reader without `config:read` — and that
 * refusal is drawn HERE, in this section's place, while the tool catalogue
 * around it (a push every reader gets) stays exactly as it is.
 */

import { useMemo } from "react";
import { Card, EmptyState, Tag } from "@crewlethq/ui";
import { PlugGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { KeyCell, NumberCell } from "~/app/frame/cells.tsx";
import { href } from "~/app/router.tsx";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { reachOf, SERVER_STATE_WORDS } from "~/lib/mcpServers.ts";
import type { Seat } from "~/lib/seats.ts";
import type { LogRefusal, QueryRefusal } from "~/protocol/index.ts";
import type {
  McpServerState,
  McpServerNode,
  McpServerStatus,
  McpServersStatusAnswer,
  McpStatusNode,
} from "~/contract/mcp.ts";

/**
 * One node's cell for one server, in words and a tone.
 *
 * UNREPORTED IS NOT ZERO: a node on a build older than the report says
 * nothing, and "none" under its name would be a claim it never made.
 */
export function nodeCellWords(cell: McpServerNode): {
  text: string;
  tone: "success" | "warning" | "danger" | "neutral";
} {
  if (!cell.reported) return { text: "not reported", tone: "neutral" };
  const launched = cell.started + cell.failed;
  if (launched === 0) return { text: "none here", tone: "neutral" };
  if (cell.failed === 0) return { text: `${cell.started} running`, tone: "success" };
  if (cell.started === 0) return { text: `${cell.failed} failed`, tone: "danger" };
  return { text: `${cell.failed} of ${launched} failed`, tone: "warning" };
}

/** How many tools a started instance serves, or null where none started. */
export function toolsOf(server: McpServerStatus): number | null {
  return server.started > 0 ? server.tools : null;
}

/** The launch as one line: the command and its arguments, or the address. */
export function launchLine(server: McpServerStatus): string {
  if (server.transport === "http") return server.url;
  return [server.command, ...server.args].filter(Boolean).join(" ");
}

export function McpServers({
  data,
  loading,
  error,
  refusal,
  seats,
}: {
  data: McpServersStatusAnswer | null;
  loading: boolean;
  error: string | null;
  /** What the refusal said beyond its code — the grant that would admit. */
  refusal: QueryRefusal | LogRefusal | null;
  seats: Seat[];
}) {
  const servers = useMemo(() => data?.servers ?? [], [data]);

  if (error || (loading && !data)) {
    return <QueryState error={error} refusal={refusal} loading={loading && !data} />;
  }
  if (servers.length === 0) {
    return (
      <EmptyState
        size="compact"
        icon={<PlugGlyph size="xl" />}
        title="No MCP servers yet"
        description="Add one to give agents a tool the engine does not ship: a tracker, a database, a vendor's API."
      />
    );
  }

  const silent = (data?.nodes ?? []).filter((n) => !n.reported);
  return (
    <div className="col gap-2">
      {silent.length > 0 && <SilentNodes nodes={silent} />}
      <Card padding="none">
        <DataGrid<McpServerStatus>
          rows={servers}
          rowKey={(s) => s.name}
          // A ROW NAMES ITS SERVER'S TOOLS: the catalogue below, filtered to
          // this origin, which is what `servers/{name}` is.
          rowHref={(s) => href(["settings", "tools", "servers", s.name])}
          defaultSort="name"
          columns={[
            {
              key: "name",
              header: "Server",
              shrink: true,
              sortValue: (s) => s.name,
              cell: (s) => (
                <span className="col" style={{ gap: 2 }}>
                  <KeyCell value={s.name} />
                  {!s.configured && <span className="t-caption">not in this configuration</span>}
                </span>
              ),
            },
            {
              key: "state",
              header: "State",
              shrink: true,
              sortValue: (s) => s.state,
              cell: (s) => <ServerStateTag state={s.state} />,
            },
            {
              key: "launch",
              header: "Launch",
              floor: "8rem",
              drop: 2,
              // ONE LINE, CUT AT ITS END. A launch is an identifier a reader
              // matches against a config file, so a break inside
              // `--read-only` or a URL (which `.clamp`'s `overflow-wrap:
              // anywhere` made at 1280) is worse than an ellipsis; the whole
              // launch is in the title and on the server's own page, which
              // the row opens.
              cell: (s) => (
                <span className="col" style={{ gap: 2, minWidth: 0 }}>
                  <span className="t-caption mono truncate" title={launchLine(s)}>
                    {launchLine(s) || <span className="muted">not configured here</span>}
                  </span>
                  <span className="t-caption">
                    {s.transport || "unknown transport"} · {s.shared ? "shared" : "one per seat"}
                  </span>
                </span>
              ),
            },
            {
              key: "reach",
              header: "Reaches",
              shrink: true,
              drop: 1,
              cell: (s) => <Reach server={s} seats={seats} />,
            },
            {
              key: "tools",
              header: "Tools",
              shrink: true,
              align: "right",
              drop: 3,
              // A COUNT ONLY WHERE AN INSTANCE STARTED: with none running, how
              // many tools the server serves is unknown, not zero — a 0 beside
              // a failing server reads as a server that offers nothing.
              sortValue: (s) => toolsOf(s),
              cell: (s) => <NumberCell value={toolsOf(s)} />,
            },
            {
              key: "nodes",
              header: "On each node",
              floor: "10rem",
              cell: (s) => <NodeCells server={s} />,
            },
          ]}
        />
      </Card>
    </div>
  );
}

/**
 * The nodes whose cells are unknown, named once above the grid rather than
 * guessed at in every row: a build older than the report says nothing about
 * its MCP servers, and each of its cells reads "not reported" for that reason.
 */
function SilentNodes({ nodes }: { nodes: McpStatusNode[] }) {
  return (
    <p className="t-caption">
      {nodes.length === 1 ? "Node " : "Nodes "}
      {nodes.map((n, i) => (
        <span key={n.id}>
          {i > 0 ? ", " : ""}
          <span className="mono">{n.id}</span>
        </span>
      ))}{" "}
      {nodes.length === 1 ? "runs" : "run"} a build that does not report its MCP servers, so whether
      they started there is unknown.
    </p>
  );
}

/** A server's state as the engine decided it, in words, with what it means. */
export function ServerStateTag({ state }: { state: McpServerState }) {
  const words = SERVER_STATE_WORDS[state] ?? {
    // A STATE A NEWER NODE SENDS draws as itself, not as nothing.
    label: state,
    tone: "neutral" as const,
    hint: "",
  };
  return (
    <Tag variant={words.tone} title={words.hint || undefined}>
      {words.label}
    </Tag>
  );
}

/**
 * Who a server reaches, and an unknown reach said AS unknown — in the muted
 * ink, so it never reads as the finding "no seat" does.
 */
function Reach({ server, seats }: { server: McpServerStatus; seats: Seat[] }) {
  const reach = reachOf(server, seats);
  return <span className={reach.known ? "t-caption" : "t-caption muted"}>{reach.text}</span>;
}

/**
 * What a failing instance said, without restating the node its chip already
 * names: the seat it was launched for (a per-seat server's failure is usually
 * that seat's credentials), then the reason.
 */
export function failureLine(cell: McpServerNode): string {
  return cell.error_seat ? `for ${cell.error_seat}: ${cell.error}` : cell.error;
}

/**
 * One block per live node: its chip, and under THAT chip the first failure it
 * reported — so with several nodes every reason sits under the node it came
 * from and none has to repeat the node's name.
 *
 * THE CHIP CARRIES THE STATE, THE REASON IS PROSE. Two lines of danger-ink
 * running text under a danger chip said the same thing twice at a volume
 * nothing else on the screen uses; the reason is secondary text, clamped, and
 * unclamped on the server's own page ([ServerHeader]), which the row opens — a
 * `title` alone reaches neither a keyboard nor a touch reader.
 */
function NodeCells({ server }: { server: McpServerStatus }) {
  return (
    <span className="col" style={{ gap: 6 }}>
      {server.nodes.map((cell) => {
        const words = nodeCellWords(cell);
        return (
          <span key={cell.node} className="col" style={{ gap: 2, alignItems: "flex-start" }}>
            <Tag variant={words.tone} appearance="outline">
              <span className="mono">{cell.node}</span> · {words.text}
            </Tag>
            {cell.error && (
              <span className="t-caption clamp mcp-failure" title={failureLine(cell)}>
                {failureLine(cell)}
              </span>
            )}
          </span>
        );
      })}
    </span>
  );
}

/**
 * A server's own page head: the server as the engine reports it, above the
 * catalogue filtered to its tools.
 *
 * `#/settings/tools/servers/{name}` used to be ONLY a filter, so a row's click
 * on a server that registered no tools changed nothing a reader could see.
 * This is what the click opens: the state, the launch, the reach and —
 * unclamped, as the heartbeat carried it (bounded at the node, with the whole
 * text in its `mcp_server_failed` log line) — every node's failure, the one
 * place a keyboard or touch reader can read a reason the grid had to cut.
 */
export function ServerHeader({
  name,
  server,
  seats,
  loading,
  unavailable,
}: {
  name: string;
  /** The server's row in the status answer; null where it has none. */
  server: McpServerStatus | null;
  seats: Seat[];
  /** The status answer has not arrived, which is not a server that is absent. */
  loading: boolean;
  /**
   * Why there is no status answer — refused (it is operator-only) or failed —
   * or null when there is one. A reader who could not be told is not a reader
   * who was told "no such server".
   */
  unavailable: string | null;
}) {
  if (!server) {
    return (
      <>
        <ObjectHeader kind="MCP server" icon="plug" title={name} />
        {/* THREE ANSWERS, NOT TWO: not yet asked, could not be told, and told
            there is none. Only the last may say the server does not exist —
            the refusal itself is drawn by the servers section below. */}
        {unavailable ? (
          <p className="t-body muted">
            <span className="mono">{name}</span>&apos;s status could not be read here, so whether it
            is configured or running is not known. The servers section below says why.
          </p>
        ) : (
          !loading && (
            <p className="t-body muted">
              No configuration this engine can see carries a server called{" "}
              <span className="mono">{name}</span>, and no live node reports one.
            </p>
          )
        )}
      </>
    );
  }
  const reach = reachOf(server, seats);
  const failures = server.nodes.filter((n) => n.error);
  return (
    <>
      <ObjectHeader
        kind="MCP server"
        icon="plug"
        title={name}
        status={<ServerStateTag state={server.state} />}
        facts={[
          { label: "Transport", value: server.transport || "unknown" },
          { label: "Instances", value: server.shared ? "one for the company" : "one per seat" },
          {
            label: "Reaches",
            value: reach.known ? reach.text : <span className="muted">{reach.text}</span>,
          },
          { label: "Tools", value: <NumberCell value={toolsOf(server)} /> },
        ]}
      />
      {/* THE LAUNCH WHOLE, as its own block rather than a fact: a fact track
          is a few words wide and a launch is a command line, which the grid
          above had to cut at its end. */}
      <section className="col gap-1">
        <div className="t-label">{server.transport === "http" ? "Address" : "Launched as"}</div>
        {launchLine(server) ? (
          <p className="t-body mono mcp-launch-whole">{launchLine(server)}</p>
        ) : (
          <p className="t-body muted">Not in this configuration: only a node reports it.</p>
        )}
      </section>
      {failures.length > 0 && (
        <section className="col gap-2">
          <div className="t-label">Why it failed</div>
          <ul className="mcp-failures">
            {failures.map((cell) => (
              <li key={cell.node} className="col" style={{ gap: 2 }}>
                <span className="t-caption">
                  On <span className="mono">{cell.node}</span>
                  {cell.error_seat ? `, launched for ${cell.error_seat}` : ""}
                </span>
                <span className="t-body mono mcp-failure-whole">{cell.error}</span>
              </li>
            ))}
          </ul>
        </section>
      )}
    </>
  );
}
