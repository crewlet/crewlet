/**
 * The MCP alarms as the engine raised them, in one compact "Needs attention"
 * panel: a row for each server an alarm stands on, with its tone, the
 * server's mark, a one-line title over a shorter line, and the one fix the
 * engine names in the page's own quiet button. What the engine measured, and
 * the remedy at length, open under the row, so a server's own words never
 * lead. Past a few rows the rest fold to a summary.
 *
 * THE ENGINE DECIDES WHAT IS AN ALARM AND WHAT FIXES IT (`alarms[].entries[]`
 * and their `fix`); the page draws the kind in its words and carries the fix
 * out. An administrator's alone: a member's reading carries none.
 */

import type { ReactNode } from "react";
import {
  AttentionList,
  Button,
  ButtonLink,
  RelativeTime,
  Text,
  type AttentionItem,
} from "@crewlethq/ui";
import type { McpAlarm, McpAlarmEntry, McpServerNode, McpServerStatus } from "~/contract/mcp.ts";
import { href } from "~/app/router.tsx";
import { listed } from "~/lib/format.ts";
import type { McpData } from "../data.ts";
import { ServerMark, type OnFlow } from "../parts.tsx";
import { ALARM, failureWords, FIX } from "../words.ts";

/**
 * The node cell an entry's failure is read from: the named agent's, else the
 * one down longest. The engine dates and classes it on the heartbeat.
 */
function cellFor(row: McpServerStatus | null, entry: McpAlarmEntry): McpServerNode | null {
  if (!row) return null;
  const down = row.nodes.filter((n) => n.failed > 0);
  if (entry.seat) return down.find((n) => n.error_seat === entry.seat) ?? null;
  down.sort((a, b) => (a.down_since ?? "").localeCompare(b.down_since ?? ""));
  return down[0] ?? null;
}

/** The fix's button: carried out here, or a link to where it is. */
function Fix({
  entry,
  title,
  integration,
  onFlow,
}: {
  entry: McpAlarmEntry;
  title: string;
  /** The integration a server's page knows the server reads, for `integration`. */
  integration: string;
  onFlow: OnFlow;
}) {
  const server = entry.fix.server;
  switch (entry.fix.kind) {
    case "restart":
      return (
        <Button
          size="small"
          variant="secondary"
          onClick={() => onFlow("restart", server, entry.fix.seat ? { seat: entry.fix.seat } : {})}
        >
          {FIX.restart}
        </Button>
      );
    case "integration":
      return integration ? (
        <ButtonLink
          size="small"
          variant="secondary"
          href={href(["company", "integrations", integration], { open: "agents" })}
        >
          Open {title}'s Agents tab
        </ButtonLink>
      ) : (
        <ButtonLink
          size="small"
          variant="secondary"
          href={href(["settings", "mcp-tools", "servers", server], { tab: "connection" })}
        >
          {FIX.integration}
        </ButtonLink>
      );
    case "edit":
      return (
        <ButtonLink
          size="small"
          variant="secondary"
          href={href(["settings", "mcp-tools", "servers", server], {
            flow: "edit",
            step: "connection",
          })}
        >
          {FIX.edit}
        </ButtonLink>
      );
    case "sign_in":
      return (
        <Button size="small" variant="secondary" onClick={() => onFlow("signin", server)}>
          {FIX.sign_in}
        </Button>
      );
    case "approve":
      return (
        <ButtonLink size="small" variant="secondary" href={href(["inbox"])}>
          {FIX.approve}
        </ButtonLink>
      );
    case "review":
      return (
        <ButtonLink
          size="small"
          variant="secondary"
          href={href(["settings", "mcp-tools", "servers", server], { tab: "tools" })}
        >
          {FIX.review}
        </ButtonLink>
      );
  }
}

/** One alarm's row for one server. */
function itemFor(
  alarm: McpAlarm,
  entry: McpAlarmEntry,
  data: McpData,
  onServerPage: boolean,
  integration: string,
  onFlow: OnFlow,
): AttentionItem {
  const server = data.server(entry.key);
  const row = data.adminRow(entry.key);
  const title = server?.title ?? entry.key;
  const cell = cellFor(row, entry);
  const who = entry.seat ? data.seatName(entry.seat) : "";
  const refused = row?.refused_tools ?? [];
  const words = ALARM[alarm.kind];
  const failure = cell ? failureWords(cell.error_class, cell.error) : null;
  const since = entry.since ? (
    <>
      since <RelativeTime value={entry.since} />
    </>
  ) : null;

  let meta: ReactNode;
  let body: string;
  let details: string[] = [entry.detail];
  switch (alarm.kind) {
    case "mcp_server_failing":
    case "mcp_server_partial":
      meta = (
        <>
          {failure?.title ?? "No instance answers"}
          {since ? <>, {since}</> : null}
        </>
      );
      body = `${failure?.remedy ?? ""}${
        alarm.kind === "mcp_server_partial"
          ? entry.seat
            ? " The other agents are not affected."
            : " The other nodes still serve it."
          : ""
      }`.trim();
      break;
    case "mcp_server_waiting_long":
      meta = <>Its integration has not minted the credential{since ? <>, {since}</> : null}</>;
      body =
        "The instance starts once the integration seals the credential it mints for the agent. Its Agents tab says whether the agent is turned on there.";
      break;
    case "mcp_tools_changed":
      meta = <>Each changed tool asks first{since ? <>, {since}</> : null}</>;
      body =
        "The server changed tools since their levels were decided, or added some. Each asks a person before every call until somebody reviews the change and accepts it or keeps it blocked.";
      break;
    case "mcp_approval_waiting":
      meta = <>Its run is parked{since ? <>, {since}</> : null}</>;
      body =
        "It called a tool that asks a person before each call. Allow it once, always, or deny it; until then the run waits and its seat takes other work.";
      break;
    case "mcp_tools_refused": {
      const one = (row?.refused_count ?? refused.length) === 1;
      meta = listed(refused.map((t) => t.tool));
      body = `The engine cannot file ${one ? "it" : "them"} under ${one ? "its" : "their"} name, so no agent sees ${one ? "it" : "them"}. A tool prefix usually fixes it.`;
      details = refused.map((t) => t.message);
      break;
    }
    case "mcp_oauth_needs_sign_in":
      meta = <>No agent can call its tools{since ? <>, {since}</> : null}</>;
      body =
        "An administrator signs the company in to it, and the engine renews the sign-in from then on.";
      break;
    case "mcp_oauth_expiring":
      meta = <>Its vendor set when the sign-in runs out</>;
      body =
        "Sign in again before then. A new sign-in replaces the old one, and the agents keep the server throughout.";
      break;
  }
  return {
    key: `${alarm.kind}:${entry.key}:${entry.seat}`,
    tone: words.tone,
    // ON A SERVER'S OWN PAGE its mark is the page's header already.
    ...(server && !onServerPage ? { media: <ServerMark server={server} /> } : {}),
    title: words.title({
      server: title,
      who,
      node: cell?.node ?? "",
      count: row?.refused_count ?? refused.length,
    }),
    meta,
    action: <Fix entry={entry} title={title} integration={integration} onFlow={onFlow} />,
    details: (
      <div className="mcp-alarm">
        {body ? <span>{body}</span> : null}
        {details
          .filter((d) => d)
          .map((d) => (
            <Text key={d} as="span" variant="caption" tone="secondary" className="mono">
              {d}
            </Text>
          ))}
      </div>
    ),
  };
}

export function Alarms({
  data,
  onFlow,
  server,
  integration = "",
}: {
  data: McpData;
  onFlow: OnFlow;
  /** On a server's page: its own alarms alone. */
  server?: string;
  /** The integration the server reads its agents' credentials from, where the page knows it. */
  integration?: string;
}) {
  const items = data.alarms.flatMap((alarm) =>
    alarm.entries
      .filter((entry) => !server || entry.key === server)
      .map((entry) => itemFor(alarm, entry, data, Boolean(server), integration, onFlow)),
  );
  return <AttentionList items={items} collapseAfter={3} />;
}
