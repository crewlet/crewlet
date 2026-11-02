/**
 * One tool in the dashboard's drawer (`?peek=tool:<name>`): what it does,
 * what it can do, who the engine lets call it or which server serves it, and
 * its arguments as the engine declares them.
 *
 * A BUILT-IN IS READ FROM THE ENGINE'S GROUPS, which carry the words its
 * group is filed under; any other tool from the pushed catalogue, which every
 * reader holds and which names the server a tool came from. Both rows carry
 * the engine's risk, so neither is read again from the hints.
 */

import { ButtonLink, DescriptionList, EmptyState, Skeleton, Tag, Text } from "@crewlethq/ui";
import { WrenchGlyph } from "@crewlethq/icons/glyphs";
import type { ToolRow } from "~/contract/tools.ts";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { href } from "~/app/router.tsx";
import { useTools } from "~/lib/store-hooks.ts";
import { useMcpData } from "../data.ts";
import { GATE } from "../words.ts";
import { argumentsOf } from "./arguments.ts";
import { builtinRows, CapabilityTag } from "./Catalogue.tsx";

/** The server an `mcp:<server>` origin names, or "". */
const serverOf = (source: string) => (source.startsWith("mcp:") ? source.slice(4) : "");

/** A tool's arguments, each a row of its own. */
function Arguments({ tool }: { tool: ToolRow }) {
  const args = argumentsOf(tool.input_schema);
  const required = (args ?? []).filter((a) => a.required).length;
  return (
    <section className="mcp-tool-detail__args" aria-label="Arguments">
      <div className="mcp-tool-detail__head">
        <span className="t-label">Arguments</span>
        <Text as="span" variant="caption" tone="tertiary">
          {args === null
            ? "Not sent"
            : args.length === 0
              ? "None"
              : `${args.length}${required ? `, ${required} required` : ""}`}
        </Text>
      </div>
      {args === null ? (
        <Text as="p" variant="body" tone="secondary">
          The engine sent no schema for it.
        </Text>
      ) : args.length === 0 ? (
        <Text as="p" variant="body" tone="secondary">
          It takes no arguments.
        </Text>
      ) : (
        <ul className="mcp-args">
          {args.map((a) => (
            <li key={a.name}>
              <span className="mcp-args__head">
                <span className="mono">{a.name}</span>
                <Text as="span" variant="caption" tone="tertiary" mono>
                  {a.type || "type not stated"}
                </Text>
                {a.required ? (
                  <Tag size="xs" variant="neutral" appearance="outline">
                    Required
                  </Tag>
                ) : null}
              </span>
              {a.description ? (
                <Text as="span" variant="caption" tone="secondary">
                  {a.description}
                </Text>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

export function ToolPeek({ name }: { name: string }) {
  const data = useMcpData();
  const pushed = useTools();
  if (data.loading && pushed.length === 0)
    return <Skeleton variant="text" rows={6} label="Loading" />;
  const builtin = builtinRows(data.builtin).find((r) => r.tool.name === name) ?? null;
  const tool = builtin?.tool ?? pushed.find((t) => t.name === name) ?? null;
  if (!tool) {
    return (
      <EmptyState
        size="compact"
        icon={<WrenchGlyph size="xl" />}
        title={`No tool called ${name}`}
        description="Its server may have stopped, or the tool was renamed."
      />
    );
  }
  const server = serverOf(tool.source);
  const gate = tool.gate ? GATE[tool.gate] : null;
  return (
    <>
      <ObjectHeader
        size="peek"
        kind={builtin ? "Built-in tool" : "MCP tool"}
        icon="wrench"
        title={<span className="mono">{tool.name}</span>}
        status={<CapabilityTag tool={tool} />}
      />
      <div className="mcp-tool-detail">
        <Text as="p" variant="body" tone={tool.description ? "primary" : "secondary"}>
          {tool.description || "No description from the server."}
        </Text>
        <DescriptionList
          dense
          fit
          items={[
            ...(builtin ? ([["Group", builtin.group.label]] as const) : []),
            ...(gate
              ? ([
                  [
                    "Who may call",
                    <span className="mcp-two">
                      <span>{gate.label}</span>
                      <Text as="span" variant="caption" tone="secondary">
                        {gate.detail}
                      </Text>
                    </span>,
                  ],
                ] as const)
              : []),
            ...(server
              ? ([
                  [
                    "Server",
                    <ButtonLink
                      size="small"
                      variant="ghost"
                      href={href(["settings", "mcp-tools", "servers", server], {
                        tab: "tools",
                        q: tool.name,
                      })}
                    >
                      {data.server(server)?.title ?? server}
                    </ButtonLink>,
                  ],
                ] as const)
              : []),
          ]}
        />
        <Arguments tool={tool} />
      </div>
    </>
  );
}
