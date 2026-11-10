/**
 * A call held for a person's answer, on its server's page: the agent, the
 * tool with its risk, and what it would send, as the one decision row the
 * Inbox and Home answer it in (`HeldCallRow`), so a parked tool call reads
 * like every other thing waiting on a person and is answered the same way.
 *
 * THE CARD'S TITLE FOLLOWS THE ROW: "Approval answered" once it is, while
 * the row says what the answer did.
 */

import { useState } from "react";
import { Card } from "@crewlethq/ui";
import { HandGlyph } from "@crewlethq/icons/glyphs";
import { HeldCallRow, type HeldCall } from "~/components/DecisionRow.tsx";
import type { McpApproval, McpMemberServerStatus, McpToolView } from "~/contract/mcp.ts";
import { useNow } from "~/lib/clock.ts";
import { answeredWords } from "~/lib/heldCall.ts";
import { RiskChips } from "../parts.tsx";

/** A held call from the MCP answer's `approvals`, as the decision row draws it. */
export function heldCallFrom(
  approval: McpApproval,
  server: Pick<McpMemberServerStatus, "title">,
  tool: McpToolView | null,
  seatName: (handle: string) => string,
): HeldCall {
  return {
    id: approval.id,
    seat: approval.seat,
    server: server.title,
    tool: approval.tool,
    at: approval.at,
    item: approval.item ? { id: approval.item, key: approval.item } : null,
    args: approval.args,
    acting: approval.acting,
    ...(tool ? { extra: <RiskChips tool={tool} /> } : {}),
    ...(approval.status === "answered"
      ? { answered: answeredWords(approval.answer, seatName(approval.answered_by)) }
      : {}),
  };
}

export function ApprovalCard({
  approval,
  server,
  tool,
  seatName,
  onAnswered,
}: {
  approval: McpApproval;
  server: Pick<McpMemberServerStatus, "title">;
  /** The tool's row on the server's detail, for its risk; null where it is not listed now. */
  tool: McpToolView | null;
  /** A seat's name by its handle, for who answered. */
  seatName: (handle: string) => string;
  onAnswered: () => void;
}) {
  const now = useNow();
  const [answered, setAnswered] = useState(approval.status === "answered");
  return (
    <Card padding="none">
      <Card.Header icon={<HandGlyph size="sm" />}>
        <Card.Title>{answered ? "Approval answered" : "Waiting for your approval"}</Card.Title>
      </Card.Header>
      <ul className="decision-list">
        <HeldCallRow
          call={heldCallFrom(approval, server, tool, seatName)}
          now={now}
          onAnswered={() => {
            setAnswered(true);
            onAnswered();
          }}
        />
      </ul>
    </Card>
  );
}
