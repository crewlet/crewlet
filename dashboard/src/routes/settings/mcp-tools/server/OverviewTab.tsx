/**
 * A server's Overview: the calls its tools hold for a person's approval, each
 * answerable in place, what is wrong and its one fix, each instance's
 * condition (one per agent for a per-agent server, read from the nodes
 * holding them at query time), when it last answered, and the identity and
 * instructions the server reports in its handshake.
 *
 * THE INSTRUCTIONS ARE NOT THE AGENTS'. A seat's harness is handed the
 * entry's own `description` as the server's instructions; what the server
 * says of itself is shown here for the administrator to read, and the card
 * says so rather than claim a model reads it.
 */

import { useEffect, useState } from "react";
import {
  Button,
  Callout,
  Card,
  DescriptionList,
  RelativeTime,
  StatusDot,
  TableTextCell,
  Text,
  Tooltip,
} from "@crewlethq/ui";
import { ActivityGlyph, FileTextGlyph, ShieldCheckGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import type { McpApproval, McpMemberServerStatus } from "~/contract/mcp.ts";
import { listed, plural } from "~/lib/format.ts";
import type { ServerDetail } from "../api.ts";
import type { McpData } from "../data.ts";
import { ApprovalCard } from "../flows/ApprovalCard.tsx";
import type { OnFlow } from "../parts.tsx";
import { Alarms } from "../servers/Alarms.tsx";
import { failureWords, INSTANCE } from "../words.ts";

/** Each state, the one to look at first: a failed instance leads the default order. */
const STATE_ORDER: readonly ServerDetail["instances"][number]["state"][] = [
  "failed",
  "waiting",
  "running",
];

/**
 * The server's instances as a table: who and where, its state, what it
 * reports (its tools, or what went wrong in a few words with the sentence on
 * hover) and since when. Failed instances first, on the kit's failed edge.
 */
function InstanceGrid({
  detail,
  data,
  enabled,
}: {
  detail: ServerDetail;
  data: McpData;
  enabled: boolean;
}) {
  type Instance = ServerDetail["instances"][number];
  const who = (i: Instance) => (i.seat ? data.seatName(i.seat) : "The company");
  return (
    <DataGrid<Instance>
      name="instances"
      rows={[...detail.instances]}
      rowKey={(i) => `${i.seat}-${i.node}`}
      defaultSort="state"
      isFailed={(i) => i.state === "failed"}
      empty={{
        title: enabled ? "Nothing is running" : "Nothing runs while it is disabled",
        icon: "activity",
      }}
      columns={[
        {
          key: "agent",
          header: "Agent",
          sortValue: who,
          cell: (i) => <TableTextCell>{who(i)}</TableTextCell>,
        },
        {
          key: "node",
          header: "Node",
          shrink: true,
          drop: 2,
          sortValue: (i) => i.node,
          cell: (i) => <span className="mono">{i.node}</span>,
        },
        {
          key: "state",
          header: "State",
          shrink: true,
          sortValue: (i) => STATE_ORDER.indexOf(i.state),
          cell: (i) => (
            <StatusDot tone={INSTANCE[i.state].tone}>{INSTANCE[i.state].label}</StatusDot>
          ),
        },
        {
          key: "detail",
          header: "Detail",
          cell: (i) => <InstanceDetail instance={i} />,
        },
        {
          key: "since",
          header: "Since",
          align: "right",
          shrink: true,
          drop: 1,
          sortValue: (i) => i.since ?? "",
          cell: (i) => (i.since ? <RelativeTime value={i.since} /> : ""),
        },
      ]}
    />
  );
}

/** A running instance's tools, or what stops one in its class's few words, the sentence on hover. */
function InstanceDetail({ instance: i }: { instance: ServerDetail["instances"][number] }) {
  if (i.state === "running")
    return (
      <Text as="span" variant="cell" tone="secondary">
        {plural(i.tools, "tool")}
      </Text>
    );
  if (!i.failure) return "wait" in i && i.wait ? <TableTextCell>{i.wait}</TableTextCell> : null;
  const failure = failureWords(i.failure.class, "said" in i.failure ? i.failure.said : "");
  return (
    <Tooltip content={failure.title} placement="top">
      <Text as="span" variant="cell" tone="secondary" truncate titleWhenCut={false}>
        {failure.short}
      </Text>
    </Tooltip>
  );
}

/** The nodes said to hold an instance that did not answer, in a sentence; "" when every one did. */
function unanswered(detail: ServerDetail): string {
  const quiet = detail.instance_nodes.filter((n) => !n.answered);
  if (quiet.length === 0) return "";
  const held = quiet.reduce((n, q) => n + q.count, 0);
  return `${listed(quiet.map((q) => q.node))} did not answer, so ${
    held > 0
      ? `${plural(held, "instance")} there ${held === 1 ? "is" : "are"}`
      : "what runs there is"
  } not listed.`;
}

/**
 * The calls this server's tools hold for a person's answer, as the decision
 * rows the Inbox draws. AN ANSWERED CALL STAYS as the line saying what was
 * answered, until the page is drawn afresh: the engine drops a call from the
 * answer once its run carries on, which would take the line away mid-read.
 */
export function HeldCalls({
  server,
  detail,
  data,
}: {
  server: McpMemberServerStatus;
  detail: ServerDetail;
  data: McpData;
}) {
  const all =
    data.answer?.reading === "admin"
      ? data.answer.approvals.filter((a) => a.server === server.name)
      : [];
  const [kept, setKept] = useState<ReadonlyMap<string, McpApproval>>(
    () => new Map(all.filter((a) => a.status === "waiting").map((a) => [a.id, a])),
  );
  useEffect(() => {
    setKept((was) => {
      let next: Map<string, McpApproval> | null = null;
      for (const a of all) {
        if ((a.status === "waiting" || was.has(a.id)) && was.get(a.id) !== a) {
          next ??= new Map(was);
          next.set(a.id, a);
        }
      }
      return next ?? was;
    });
  }, [all]);
  return (
    <>
      {[...kept.values()].map((a) => (
        <ApprovalCard
          key={a.id}
          approval={a}
          server={server}
          tool={detail.tools.find((t) => t.name === a.tool) ?? null}
          seatName={data.seatName}
          onAnswered={() => data.reload()}
        />
      ))}
    </>
  );
}

export function OverviewTab({
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
  const admin = detail.reading === "admin" ? detail : null;
  const quiet = unanswered(detail);
  return (
    <div className="mcp-stack">
      {!server.enabled ? (
        <Callout
          variant="neutral"
          title="Disabled"
          action={
            data.admin ? (
              <Button size="small" variant="secondary" onClick={() => onFlow("disable")}>
                Enable
              </Button>
            ) : undefined
          }
        >
          No agent is offered its tools. Its settings, secrets, grants and levels are kept.
        </Callout>
      ) : null}
      {data.admin ? <HeldCalls server={server} detail={detail} data={data} /> : null}
      {data.admin ? (
        <Alarms
          data={data}
          onFlow={onFlow}
          server={server.name}
          integration={detail.resolved?.binding?.vendor ?? ""}
        />
      ) : null}
      <div className="mcp-grid">
        <Card padding="none">
          <Card.Header
            icon={<ActivityGlyph size="sm" />}
            count={detail.instances.length || undefined}
          >
            <Card.Title>{server.shared ? "Instance" : "One instance per agent"}</Card.Title>
          </Card.Header>
          <InstanceGrid detail={detail} data={data} enabled={server.enabled} />
          {quiet ? (
            <Text as="p" variant="caption" tone="tertiary" className="mcp-note">
              {quiet}
            </Text>
          ) : null}
        </Card>
        <Card padding="none">
          <Card.Header
            icon={<ShieldCheckGlyph size="sm" />}
            actions={
              admin && server.configured && server.enabled ? (
                <Button
                  size="small"
                  variant="secondary"
                  onClick={() => onFlow("test", server.name)}
                >
                  Test now
                </Button>
              ) : undefined
            }
          >
            <Card.Title>Answers</Card.Title>
          </Card.Header>
          <Card.Body>
            <div className="mcp-stack">
              <DescriptionList
                dense
                items={[
                  [
                    "Last answered",
                    detail.last_success ? <RelativeTime value={detail.last_success} /> : "Not yet",
                  ],
                  ...(admin
                    ? ([
                        [
                          "Last test",
                          admin.last_check ? (
                            <span>
                              {admin.last_check.ok ? "Passed" : "Failed"}
                              {admin.last_check.by ? ` by ${admin.last_check.by}` : ""},{" "}
                              <RelativeTime value={admin.last_check.at} />
                            </span>
                          ) : (
                            "Not tested yet"
                          ),
                        ],
                      ] as const)
                    : []),
                  [
                    "Last failure",
                    detail.last_failure ? (
                      <span>
                        {
                          failureWords(
                            detail.last_failure.class,
                            "said" in detail.last_failure ? detail.last_failure.said : "",
                          ).short
                        }
                        , <RelativeTime value={detail.last_failure.at} />
                      </span>
                    ) : (
                      "None"
                    ),
                  ],
                  ...(detail.server_info
                    ? ([
                        [
                          "Reported as",
                          <span className="mono">
                            {[detail.server_info.name, detail.server_info.version]
                              .filter(Boolean)
                              .join(" ")}
                          </span>,
                        ],
                        ...(detail.server_info.protocol
                          ? ([
                              [
                                "Protocol",
                                <span className="mono">{detail.server_info.protocol}</span>,
                              ],
                            ] as const)
                          : []),
                      ] as const)
                    : []),
                ]}
              />
              {admin ? (
                <Text as="p" variant="caption" tone="tertiary">
                  A test asks the server who it is and lists its tools. It never calls a tool.
                </Text>
              ) : null}
            </div>
          </Card.Body>
        </Card>
      </div>
      {admin?.instructions ? (
        <Card padding="none">
          <Card.Header icon={<FileTextGlyph size="sm" />}>
            <Card.Title>Instructions the server reports</Card.Title>
          </Card.Header>
          <Card.Body>
            <div className="mcp-stack">
              <Text as="p" variant="body" className="mcp-quote">
                {admin.instructions}
              </Text>
              <Text as="p" variant="caption" tone="tertiary">
                The server's own words from its handshake. The agents are not handed them: its
                description is what they read.
              </Text>
            </div>
          </Card.Body>
        </Card>
      ) : null}
    </div>
  );
}
