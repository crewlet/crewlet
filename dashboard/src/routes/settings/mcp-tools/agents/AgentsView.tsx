/**
 * Agent access: every agent by every server, how many tools it may call on
 * each and why it reaches the server at all, and its tools against the
 * budget the engine flags.
 *
 * THE ENGINE COUNTS. Each row is the answer's `agents` (the administrator's
 * reading): the servers whose grant reaches the agent, each with its reason,
 * the tools it offers there and the server's state, the agent's total (a name
 * two servers offer counted once) and whether it is over `tool_budget`. The
 * page adds nothing up and compares nothing with the budget.
 *
 * A SERVER AN INTEGRATION SIGNS IN FOR IS READ, NOT WRITTEN, here: its agents
 * are the ones turned on at the integration, decided on that integration's
 * Agents tab. Every other server's grant is edited on its own Access tab, one
 * press away.
 */

import { useMemo } from "react";
import {
  ButtonLink,
  Callout,
  Card,
  EmptyValue,
  EntityList,
  Meter,
  Skeleton,
  StatusDot,
  Tag,
  Text,
  Tooltip,
  type DataGridColumn,
} from "@crewlethq/ui";
import { UsersGlyph } from "@crewlethq/icons/glyphs";
import type {
  McpAgentServer,
  McpAgentTools,
  McpMemberServerStatus,
  McpServersStatusAnswer,
  McpToolLevel,
} from "~/contract/mcp.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { href, useParam } from "~/app/router.tsx";
import { listed, plural } from "~/lib/format.ts";
import { useRest } from "~/lib/useRest.ts";
import { readServer } from "../api.ts";
import type { McpData } from "../data.ts";
import { ServerMark } from "../parts.tsx";
import { LEVEL, REASON, STATE } from "../words.ts";

const accessHref = (server: string) =>
  href(["settings", "mcp-tools", "servers", server], { tab: "access" });

/**
 * ONE SHORT VALUE A CELL, at the grid's one size: the tools the agent may
 * call there, the server's state where it is not running, or the kit's
 * marked absence where it does not reach the server at all.
 */
function Cell({ agent, server }: { agent: McpAgentTools; server: McpMemberServerStatus }) {
  const s = agent.servers.find((x) => x.server === server.name);
  if (!s) return <EmptyValue label={`${agent.name} does not reach it`} />;
  const state = s.state === "running" ? null : STATE[s.state];
  return (
    <Tooltip
      content={`${agent.name} reaches ${server.title}: ${REASON[s.reason]}.${state ? ` ${state.label}.` : ""}`}
      placement="top"
    >
      <a className="mcp-row-reveal" href={accessHref(server.name)} tabIndex={-1} aria-hidden="true">
        {state ? (
          <StatusDot tone={state.tone}>{state.label}</StatusDot>
        ) : (
          <span className="mcp-num">{s.tools}</span>
        )}
      </a>
    </Tooltip>
  );
}

/** Who is over the tool budget, and where an integration's agents are chosen. */
function AgentsNotes({
  agents,
  budget,
  servers,
}: {
  agents: readonly McpAgentTools[];
  budget: number;
  servers: readonly McpMemberServerStatus[];
}) {
  const over = agents.filter((a) => a.over_budget);
  const managed = servers.filter((s) =>
    agents.some((a) => a.servers.some((x) => x.server === s.name && x.reason === "integration")),
  );
  return (
    <>
      {over.length > 0 ? (
        <Callout
          variant="warning"
          title={`${listed(over.map((a) => a.name))} ${over.length === 1 ? "has" : "have"} more than ${budget} MCP tools`}
        >
          Models choose less reliably from a long tool list. Block tools{" "}
          {over.length === 1 ? "it does" : "they do"} not need on the servers below, or narrow who
          reaches the busiest servers. Built-in tools are not counted.
        </Callout>
      ) : null}
      {managed.length > 0 ? (
        <Text as="p" variant="caption" tone="secondary">
          The agents of{" "}
          {managed.map((s, i) => (
            <span key={s.name}>
              {i > 0 ? (i === managed.length - 1 ? " and " : ", ") : ""}
              <a className="t-link" href={accessHref(s.name)}>
                {s.title}
              </a>
            </span>
          ))}{" "}
          are the ones turned on at{" "}
          {managed.length === 1 ? "its integration" : "their integrations"}. Every other server's
          grant is on its own Access tab.
        </Text>
      ) : null}
    </>
  );
}

/** Every agent by every server. */
function AgentsGrid({
  agents,
  servers,
  budget,
}: {
  agents: readonly McpAgentTools[];
  servers: readonly McpMemberServerStatus[];
  /** The engine's budget, for the over-budget mark; whether an agent is over it is the engine's flag. */
  budget: number;
}) {
  const [chosen] = useParam("agent", "");
  const columns: DataGridColumn<McpAgentTools>[] = [
    {
      key: "agent",
      header: "Agent",
      floor: "9rem",
      phoneLead: true,
      sortValue: (a) => a.name,
      cell: (a) => (
        <Text as="span" variant="inherit" truncate>
          {a.name}
        </Text>
      ),
    },
    ...servers.map((s): DataGridColumn<McpAgentTools> => ({
      key: s.name,
      // A MARK FOR A HEAD, so six servers fit beside the agents: the column
      // is named by `label` (the phone's card and assistive technology) and
      // the tip.
      header: (
        <Tooltip content={s.title} placement="top">
          <span className="mcp-col-head">
            <ServerMark server={s} size="md" />
          </span>
        </Tooltip>
      ),
      label: s.title,
      align: "right",
      // AS WIDE AS WHAT IT HOLDS, never cut: the grid gives way as a whole
      // (the kit stacks it) before it shortens a value or a head.
      shrink: true,
      phoneOmit: true,
      sortValue: (a) => a.servers.find((x) => x.server === s.name)?.tools ?? -1,
      cell: (a) => <Cell agent={a} server={s} />,
    })),
    {
      key: "total",
      header: "Total",
      align: "right",
      shrink: true,
      sortValue: (a) => a.mcp_tools,
      cell: (a) => (
        <span className="mcp-tools-cell">
          <span className="mcp-num">
            {a.mcp_tools}
            <span className="mcp-phone-unit"> MCP tools</span>
          </span>
          {a.over_budget ? (
            <Tag size="xs" variant="warning" appearance="soft">
              Over {budget}
            </Tag>
          ) : null}
        </span>
      ),
    },
  ];
  return (
    <DataGrid<McpAgentTools>
      name="agents"
      rows={[...agents]}
      rowKey={(a) => a.seat}
      rowHref={(a) => href(["settings", "mcp-tools"], { view: "agents", agent: a.seat })}
      isSelected={(a) => a.seat === chosen}
      // ON A PHONE AN AGENT IS TWO LINES, its name and its total: a card of
      // one line a server was mostly "does not reach it". The row opens the
      // agent's own tools, which name every server it reaches.
      phoneRows="compact"
      columns={columns}
    />
  );
}

/** The view whole: the notes, the grid in its card, and the chosen agent's tools. */
export function AgentsView({ data }: { data: McpData }) {
  const answer = data.answer?.reading === "admin" ? data.answer : null;
  const [chosen] = useParam("agent", "");
  if (!answer) return null;
  if (answer.agents === null) {
    return <Skeleton variant="text" rows={6} label="Counting each agent's tools" />;
  }
  const agent = answer.agents.find((a) => a.seat === chosen) ?? null;
  return (
    <div className="mcp-page">
      <AgentsNotes agents={answer.agents} budget={answer.tool_budget} servers={data.servers} />
      <Card padding="none">
        <Card.Header icon={<UsersGlyph size="sm" />} count={answer.agents.length || undefined}>
          <Card.Title>Agents by server</Card.Title>
        </Card.Header>
        {answer.agents.length > 0 ? (
          <AgentsGrid agents={answer.agents} servers={data.servers} budget={answer.tool_budget} />
        ) : (
          <Card.Body>
            <Text as="p" variant="body" tone="secondary">
              The company has no agent yet.
            </Text>
          </Card.Body>
        )}
      </Card>
      {agent ? <EffectiveTools agent={agent} answer={answer} data={data} /> : null}
    </div>
  );
}

/**
 * How many of one server's tools sit at each level, from its detail: the
 * engine's level for every tool it lists. Read for the servers the chosen
 * agent reaches, when one is chosen.
 */
function useLevelsOf(names: readonly string[]): ReadonlyMap<string, Record<McpToolLevel, number>> {
  const key = names.join(",");
  const read = useRest(`mcp-agent-levels:${key}`, async (signal) => {
    const details = await Promise.all(names.map((n) => readServer(n, signal)));
    return new Map(
      details.map((d) => {
        const counts: Record<McpToolLevel, number> = { allow: 0, approve: 0, blocked: 0 };
        for (const t of d.tools) counts[t.level] += 1;
        return [d.server.name, counts] as const;
      }),
    );
  });
  return read.data ?? new Map();
}

/** One reached server's fact line: why the agent reaches it, then its levels or its state. */
function factOf(s: McpAgentServer, levels: Record<McpToolLevel, number> | undefined): string {
  const why = REASON[s.reason];
  if (s.state !== "running") return `${why} · ${STATE[s.state].label}`;
  if (!levels) return `${why} · ${plural(s.tools, "tool")}`;
  return `${why} · ${listed([
    ...(levels.allow ? [`${levels.allow} ${LEVEL.allow.label.toLowerCase()}`] : []),
    ...(levels.approve ? [`${levels.approve} ${LEVEL.approve.label.toLowerCase()}`] : []),
    ...(levels.blocked ? [`${levels.blocked} blocked`] : []),
  ])}`;
}

function EffectiveTools({
  agent,
  answer,
  data,
}: {
  agent: McpAgentTools;
  answer: McpServersStatusAnswer;
  data: McpData;
}) {
  const budget = answer.tool_budget;
  const names = useMemo(
    () => agent.servers.filter((s) => s.state === "running").map((s) => s.server),
    [agent.servers],
  );
  const levels = useLevelsOf(names);
  return (
    <Card padding="none">
      <Card.Header icon={<UsersGlyph size="sm" />}>
        <Card.Title>{agent.name}'s tools</Card.Title>
      </Card.Header>
      <Card.Body>
        <div className="mcp-stack">
          <Meter
            label="MCP tools it may call"
            value={agent.mcp_tools}
            max={Math.max(budget, agent.mcp_tools)}
            valueText={`${agent.mcp_tools} of the ${budget} the engine flags above`}
            tone={agent.over_budget ? "warning" : "neutral"}
          />
          <EntityList
            label={`Servers ${agent.name} reaches`}
            items={agent.servers.map((s) => {
              const server = data.server(s.server);
              const title = server?.title ?? s.server;
              const state = s.state === "running" ? null : STATE[s.state];
              const fact = factOf(s, levels.get(s.server));
              return {
                id: s.server,
                name: title,
                ...(server ? { avatar: <ServerMark server={server} /> } : {}),
                fact: state ? <StatusDot tone={state.tone}>{fact}</StatusDot> : fact,
                action: (
                  <ButtonLink
                    size="small"
                    variant="ghost"
                    href={href(["settings", "mcp-tools", "servers", s.server], { tab: "tools" })}
                    aria-label={`${title}'s tools`}
                  >
                    Tools
                  </ButtonLink>
                ),
              };
            })}
          />
          <Text as="p" variant="caption" tone="tertiary">
            Plus {plural(agent.builtin_tools, "built-in tool")} it is offered, which the budget does
            not count.
          </Text>
        </div>
      </Card.Body>
    </Card>
  );
}
