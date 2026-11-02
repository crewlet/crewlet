/**
 * A server's Connection, read only: where it is and how it starts, how the
 * engine signs in to it, the bounds it is held to, and every header or
 * variable it is sent, by name.
 *
 * NAMES, NEVER VALUES. The address is the engine's, any credential in it
 * withheld (`url` on the row); the headers and variables are the entry as the
 * configuration writes it (`GET /config/mcp-servers/{name}`), whose values
 * the engine masks, and this card reads their names and the variable each one
 * reads, never what any of them holds. A member reads how the server connects
 * and signs in, and none of the launch line or its names.
 */

import {
  ButtonLink,
  Card,
  Copyable,
  DescriptionList,
  Skeleton,
  TableTwoLineCell,
  Text,
} from "@crewlethq/ui";
import { KeyGlyph, ServerGlyph } from "@crewlethq/icons/glyphs";
import type { McpMemberServerStatus } from "~/contract/mcp.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { href } from "~/app/router.tsx";
import { useRest } from "~/lib/useRest.ts";
import { getEntity } from "~/protocol/configWrite.ts";
import type { ServerDetail } from "../api.ts";
import type { McpData } from "../data.ts";
import { AUTH } from "../words.ts";

/** One header or variable the entry sends, by name, and the variable it reads, if any. */
interface Sent {
  name: string;
  /** The variable a whole `${VAR}` value reads; "" for a value written in the entry. */
  secret: string;
}

const REFERENCE = /^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$/;

/** A map of the entry's names to what each is written as, read for its names alone. */
function sentOf(value: unknown): Sent[] {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return [];
  return Object.entries(value as Record<string, unknown>)
    .map(([name, v]) => ({
      name,
      secret: typeof v === "string" ? (REFERENCE.exec(v)?.[1] ?? "") : "",
    }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

/** The entry as written, for an administrator: its headers or variables, and its tool prefix. */
function useEntry(name: string, enabled: boolean) {
  return useRest(
    enabled ? `mcp-entity:${name}` : null,
    async (signal) => {
      const read = await getEntity("mcp-servers", name, signal);
      return read.kind === "entity" ? read.entity : null;
    },
    { refetchOnFocus: true },
  );
}

const seconds = (n: number, byDefault: boolean) =>
  `${Number.isInteger(n) ? n : n.toFixed(1)} s${byDefault ? " (the default)" : ""}`;

export function ConnectionTab({
  server,
  detail,
  data,
}: {
  server: McpMemberServerStatus;
  detail: ServerDetail;
  data: McpData;
}) {
  const row = data.adminRow(server.name);
  const admin = detail.reading === "admin" && row !== null;
  const entry = useEntry(server.name, admin && server.configured);
  const written = entry.data;
  const http = server.transport === "http";
  const sentWord = http ? "Header" : "Variable";
  const sent = sentOf(http ? written?.headers : written?.env);
  const prefix = typeof written?.tool_prefix === "string" ? written.tool_prefix : "";
  const resolved = detail.resolved;
  const binding = resolved?.binding ?? null;
  const command = row?.command ? [row.command, ...row.args].join(" ") : "";
  return (
    <div className="mcp-stack">
      <Card padding="none">
        <Card.Header icon={<ServerGlyph size="sm" />}>
          {/* Named for what it leads with: a member is not shown the address
              or the command, and the card is then its connection's facts. */}
          <Card.Title>
            {!(admin && (row?.url || command)) ? "Connection" : http ? "Address" : "Command"}
          </Card.Title>
        </Card.Header>
        <Card.Body>
          <DescriptionList
            dense
            items={[
              ...(admin && row?.url
                ? ([["Address", <Copyable value={row.url} monospace variant="inline" />]] as const)
                : []),
              ...(admin && command
                ? ([["Command", <span className="mono">{command}</span>]] as const)
                : []),
              ["Transport", http ? "Streamable HTTP" : "Standard input and output"],
              ...(resolved?.runs_on === "engine_host"
                ? ([["Runs on", "The engine's host, on each node that runs an instance"]] as const)
                : []),
              ["Instances", server.shared ? "One for the company" : "One per agent"],
              ...(resolved
                ? ([
                    [
                      "Start timeout",
                      seconds(resolved.timeouts.startup_seconds, resolved.timeouts.startup_default),
                    ],
                    [
                      "Call timeout",
                      seconds(resolved.timeouts.request_seconds, resolved.timeouts.request_default),
                    ],
                  ] as const)
                : []),
              ...(prefix
                ? ([["Tool prefix", <span className="mono">{prefix}</span>]] as const)
                : []),
            ]}
          />
        </Card.Body>
      </Card>

      {resolved ? (
        <Card padding="none">
          <Card.Header
            icon={<KeyGlyph size="sm" />}
            actions={
              binding && data.admin ? (
                <ButtonLink
                  size="small"
                  variant="secondary"
                  href={href(["company", "integrations", binding.vendor], { open: "agents" })}
                >
                  Open {server.title}'s Agents tab
                </ButtonLink>
              ) : undefined
            }
          >
            <Card.Title>Sign-in</Card.Title>
          </Card.Header>
          <Card.Body>
            <div className="mcp-stack">
              <DescriptionList
                dense
                items={[
                  ["Method", AUTH[resolved.auth]],
                  ...(binding
                    ? ([
                        [
                          "Integration",
                          binding.by === "declared"
                            ? `${binding.vendor}, as the entry declares`
                            : `${binding.vendor}, recognised from its ${http ? "address" : "command"}`,
                        ],
                      ] as const)
                    : []),
                  ...(detail.reading === "admin" && detail.resolved?.binding?.keys.length
                    ? ([
                        [
                          "Signs in with",
                          <span className="mono">
                            {detail.resolved.binding.keys.map((k) => k.key).join(", ")}
                          </span>,
                        ],
                      ] as const)
                    : []),
                ]}
              />
              {binding ? (
                <Text as="p" variant="caption" tone="tertiary">
                  Which agents get {server.title}, and with what access, is set on the{" "}
                  {binding.vendor} integration's Agents tab. Turning an agent on there starts its
                  instance here.
                </Text>
              ) : null}
            </div>
          </Card.Body>
        </Card>
      ) : null}

      {admin && server.configured && resolved?.auth !== "integration" ? (
        <Card padding="none">
          <Card.Header icon={<KeyGlyph size="sm" />} count={sent.length || undefined}>
            <Card.Title>{http ? "Request headers" : "Environment"}</Card.Title>
          </Card.Header>
          {written === null && entry.loading ? (
            <Card.Body>
              <Skeleton variant="text" rows={2} label="Loading its configuration" />
            </Card.Body>
          ) : (
            <DataGrid<Sent>
              name="sent"
              rows={sent}
              rowKey={(v) => v.name}
              columns={[
                {
                  key: "name",
                  header: sentWord,
                  label: sentWord,
                  cell: (v) => (
                    <TableTwoLineCell
                      value={v.name}
                      mono={v.secret ? "both" : "value"}
                      detail={v.secret ? `Reads \${${v.secret}}` : "Set in the configuration"}
                    />
                  ),
                },
              ]}
              empty={{
                title: http ? "No header" : "No variable",
                hint: "The engine sends the server no value of its own.",
                icon: "key",
              }}
            />
          )}
        </Card>
      ) : null}
    </div>
  );
}
