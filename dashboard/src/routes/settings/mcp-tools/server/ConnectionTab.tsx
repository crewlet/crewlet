/**
 * A server's Connection: where it is and how it starts, how the engine signs
 * in to it, the bounds it is held to, and every header or variable it is
 * sent, by name.
 *
 * NAMES, NEVER VALUES. The address is the engine's, any credential in it
 * withheld (`url` on the row). The headers and variables are the server's
 * answer (`values`): each key, the variable it reads, whether the store holds
 * it and who stored it last, never what it holds. A value stored from here is
 * write-only (`PUT /mcp-servers/{name}/values/{key}`): Replace takes a new
 * one, tested before it is sealed, and nothing shows the old one. A key an
 * integration hands each agent is that integration's, and has no Replace.
 * The tool prefix is the entry as the configuration writes it
 * (`GET /config/mcp-servers/{name}`), which no answer carries.
 *
 * AN OAUTH SIGN-IN IS ITS OWN CARD (the row's `oauth`): where it is, who
 * connected it and when, when access ends, the authorization server, the
 * client and the scopes, never a token. A person's sign-in is made, made
 * again and given up from it; a service identity signs itself in, so it
 * offers neither. The client's secret (`oauth.client_secret`) is listed there,
 * write-only like any value, rather than among the request headers.
 *
 * A member reads how the server connects and signs in, and none of the
 * launch line or its names.
 */

import {
  Button,
  ButtonLink,
  Card,
  Copyable,
  DescriptionList,
  RelativeTime,
  TableTwoLineCell,
  Tag,
  Text,
  VisuallyHidden,
} from "@crewlethq/ui";
import { KeyGlyph, LinkGlyph, ServerGlyph } from "@crewlethq/icons/glyphs";
import type { McpMemberServerStatus, McpOAuthView, McpValueState } from "~/contract/mcp.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { href } from "~/app/router.tsx";
import { fmtDate } from "~/lib/format.ts";
import { useRest } from "~/lib/useRest.ts";
import { getEntity } from "~/protocol/configWrite.ts";
import { CLIENT_SECRET, type ServerDetail } from "../api.ts";
import type { McpData } from "../data.ts";
import type { OnFlow } from "../parts.tsx";
import { AUTH, OAUTH_CLIENT, OAUTH_STATUS } from "../words.ts";

/** The entry as written, for an administrator: its tool prefix. */
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
  onFlow,
}: {
  server: McpMemberServerStatus;
  detail: ServerDetail;
  data: McpData;
  onFlow: OnFlow;
}) {
  const row = data.adminRow(server.name);
  const admin = detail.reading === "admin" && row !== null;
  const entry = useEntry(server.name, admin && server.configured);
  const written = entry.data;
  const http = server.transport === "http";
  const all = detail.reading === "admin" ? detail.values : [];
  // THE CLIENT'S SECRET IS THE SIGN-IN'S, listed on its card.
  const values = all.filter((v) => v.as !== "oauth");
  const secret = all.find((v) => v.key === CLIENT_SECRET) ?? null;
  const oauth = admin ? (row?.oauth ?? null) : null;
  const prefix = typeof written?.tool_prefix === "string" ? written.tool_prefix : "";
  const edit = (step: string) => onFlow("edit", server.name, { step });
  const resolved = detail.resolved;
  const binding = resolved?.binding ?? null;
  const command = row?.command ? [row.command, ...row.args].join(" ") : "";
  return (
    <div className="mcp-stack">
      <Card padding="none">
        <Card.Header
          icon={<ServerGlyph size="sm" />}
          actions={
            admin && server.configured ? (
              <Button size="small" variant="secondary" onClick={() => edit("connection")}>
                Edit
              </Button>
            ) : undefined
          }
        >
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

      {oauth ? (
        <OAuthCard
          server={server}
          oauth={oauth}
          secret={secret}
          callbackUrl={data.answer && "callback_url" in data.answer ? data.answer.callback_url : ""}
          onEdit={() => edit("signin")}
          onFlow={onFlow}
        />
      ) : resolved ? (
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

      {admin && server.configured && (values.length > 0 || resolved?.auth !== "integration") ? (
        <Card padding="none">
          <Card.Header
            icon={<KeyGlyph size="sm" />}
            count={values.length || undefined}
            actions={
              <Button
                size="small"
                variant="secondary"
                onClick={() => edit(http ? "signin" : "connection")}
              >
                Edit
              </Button>
            }
          >
            <Card.Title>{http ? "Request headers" : "Environment"}</Card.Title>
          </Card.Header>
          <DataGrid<McpValueState>
            name="values"
            rows={values}
            rowKey={(v) => v.key}
            columns={valueColumns(server, http, values, onFlow)}
            empty={{
              title: http ? "No header" : "No variable",
              hint: "The engine sends the server no value of its own.",
              icon: "key",
            }}
          />
        </Card>
      ) : null}
    </div>
  );
}

/**
 * A server's OAuth sign-in, as an administrator reads it. A person's sign-in
 * is made (Sign in), made again and given up (Revoke) here; a service
 * identity, which nobody signs in to, names its client and scopes alone.
 */
function OAuthCard({
  server,
  oauth,
  secret,
  callbackUrl,
  onEdit,
  onFlow,
}: {
  server: McpMemberServerStatus;
  oauth: McpOAuthView;
  /** The client's secret as the server's values list it; null where it has none to store. */
  secret: McpValueState | null;
  callbackUrl: string;
  onEdit: () => void;
  onFlow: OnFlow;
}) {
  const service = oauth.grant === "client_credentials";
  const connected = oauth.status === "connected";
  const state = OAUTH_STATUS[oauth.status];
  const client =
    oauth.client in OAUTH_CLIENT ? OAUTH_CLIENT[oauth.client as keyof typeof OAUTH_CLIENT] : null;
  return (
    <Card padding="none">
      <Card.Header
        icon={<LinkGlyph size="sm" />}
        actions={
          <>
            <Button size="small" variant="secondary" onClick={onEdit}>
              Edit
            </Button>
            {!service && connected ? (
              <Button
                size="small"
                variant="secondary"
                onClick={() => onFlow("revoke", server.name)}
              >
                Revoke
              </Button>
            ) : null}
            {service ? null : (
              <Button
                size="small"
                variant={connected ? "secondary" : "primary"}
                onClick={() => onFlow("signin", server.name)}
              >
                {connected ? "Sign in again" : "Sign in"}
              </Button>
            )}
          </>
        }
      >
        <Card.Title>Sign-in</Card.Title>
      </Card.Header>
      <Card.Body>
        <div className="mcp-stack">
          <DescriptionList
            dense
            items={[
              [
                "Method",
                service ? "OAuth client credentials, the company's service identity" : AUTH.oauth,
              ],
              ...(service
                ? []
                : ([
                    [
                      "State",
                      <Tag size="xs" variant={state.tone} appearance="soft">
                        {state.label}
                      </Tag>,
                    ],
                  ] as const)),
              ...(oauth.connected_by && oauth.connected_at
                ? ([
                    [
                      "Signed in",
                      <span>
                        by {oauth.connected_by} on {fmtDate(oauth.connected_at)}
                      </span>,
                    ],
                  ] as const)
                : []),
              ...(oauth.expires_at
                ? ([["Access ends", <RelativeTime value={oauth.expires_at} />]] as const)
                : []),
              ...(oauth.failure
                ? ([
                    [
                      "Renewal refused",
                      <span>
                        {fmtDate(oauth.failure.at)}: {oauth.failure.said}
                      </span>,
                    ],
                  ] as const)
                : []),
              ...(oauth.issuer
                ? ([
                    ["Authorization server", <span className="mono">{oauth.issuer}</span>],
                  ] as const)
                : []),
              [
                "Client",
                client && oauth.client !== "own" ? (
                  client.label
                ) : (
                  <span className="mono">{oauth.client_id}</span>
                ),
              ],
              ...(secret
                ? ([
                    [
                      "Client secret",
                      <span className="mcp-secret">
                        <Tag
                          size="xs"
                          variant={secret.set ? "success" : "danger"}
                          appearance="soft"
                        >
                          {secret.set ? "Set" : "Not set"}
                        </Tag>
                        <Button
                          size="small"
                          variant="ghost"
                          aria-label={`${secret.set ? "Replace" : "Set"} the client secret`}
                          onClick={() => onFlow("value", server.name, { key: secret.key })}
                        >
                          {secret.set ? "Replace" : "Set"}
                        </Button>
                      </span>,
                    ],
                  ] as const)
                : []),
              [
                "Scopes",
                oauth.scopes.length ? (
                  <span className="mono">{oauth.scopes.join(" ")}</span>
                ) : (
                  "The server's default"
                ),
              ],
              ...(service
                ? []
                : ([
                    [
                      "Callback address",
                      <Copyable value={callbackUrl} monospace variant="inline" />,
                    ],
                  ] as const)),
            ]}
          />
          <Text as="p" variant="caption" tone="tertiary">
            {service
              ? "Nobody signs in: the engine signs in as this client whenever the server needs a token, and holds each token in memory alone."
              : "The agents use this sign-in for the company. The engine keeps its refresh token sealed in its own store, which nobody can read back, and mints each access token as it is used."}
          </Text>
        </div>
      </Card.Body>
    </Card>
  );
}

/** The headers' or variables' columns: the key and what it reads, its state, who stored it, and Replace. */
function valueColumns(
  server: McpMemberServerStatus,
  http: boolean,
  values: readonly McpValueState[],
  onFlow: OnFlow,
) {
  const word = http ? "Header" : "Variable";
  return [
    {
      key: "key",
      header: word,
      label: word,
      cell: (v: McpValueState) => (
        <TableTwoLineCell
          value={v.key}
          mono={v.variable ? "both" : "value"}
          detail={
            v.bound_by
              ? `Each agent's own, from ${v.bound_by}`
              : v.variable
                ? `Reads \${${v.variable}}`
                : "Set in the configuration"
          }
        />
      ),
    },
    {
      key: "state",
      header: "Value",
      shrink: true,
      cell: (v: McpValueState) =>
        v.bound_by ? (
          <Text as="span" variant="caption" tone="secondary">
            Issued for each agent
          </Text>
        ) : v.set ? (
          <Tag size="xs" variant="success" appearance="soft">
            Set
          </Tag>
        ) : (
          <Tag size="xs" variant={v.optional ? "neutral" : "danger"} appearance="soft">
            {v.optional ? "Not set, optional" : "Not set"}
          </Tag>
        ),
    },
    {
      key: "stored",
      header: "Stored",
      shrink: true,
      // WHEN OVER WHO, two lines, so neither is cut against the row's action.
      cell: (v: McpValueState) =>
        v.stored_at ? (
          <TableTwoLineCell
            value={fmtDate(v.stored_at)}
            {...(v.stored_by ? { detail: `by ${v.stored_by}` } : {})}
          />
        ) : (
          <Text as="span" variant="caption" tone="secondary">
            {v.bound_by ? "When the agent starts" : "Never"}
          </Text>
        ),
    },
    // AN ACTIONS COLUMN ONLY WHERE A ROW HAS AN ACTION: a value an
    // integration binds for each agent is replaced nowhere here, and an empty
    // column of no buttons held a track at the row's end.
    ...(values.some((v) => !v.bound_by)
      ? [
          {
            key: "actions",
            header: <VisuallyHidden>Actions</VisuallyHidden>,
            action: true,
            shrink: true,
            cell: (v: McpValueState) =>
              v.bound_by ? null : (
                <Button
                  size="small"
                  variant="ghost"
                  aria-label={`${v.set ? "Replace" : "Set"} ${v.key}`}
                  onClick={() => onFlow("value", server.name, { key: v.key })}
                >
                  {v.set ? "Replace" : "Set"}
                </Button>
              ),
          },
        ]
      : []),
  ];
}
