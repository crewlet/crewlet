/**
 * The three answers to a call a run holds for approval (ADR-0077): Allow
 * once, Always allow and Deny, each sent through `POST /mcp-approvals/{id}`.
 *
 * ONE SET OF BUTTONS for every place a parked run is answered (a decision
 * row, the live run's banner, a seat's page), because the run's own answer
 * (`answer_executor`) is refused for a held call: a surface that drew a
 * held call's run with the run's Answer offered a button the engine refuses.
 */

import { useEffect, useState } from "react";
import { Button, CodeBlock, Modal, Text, useToast } from "@crewlethq/ui";
import type { McpHeldWhole } from "~/contract/mcp.ts";
import { answerHeldCall, heldAnswerWords, readHeldCall, type HeldAnswer } from "~/lib/heldCall.ts";
import { refusalText } from "~/lib/refusal.ts";
import { useAnswerHold } from "~/lib/useWriteAccess.ts";

/*
 * HELD FOR A READER THE ENGINE DOES NOT ADMIT, through the rule the run's own
 * Answer is held by (`useAnswerHold`): drawn, and disabled with who may answer.
 */
export function HeldCallButtons({
  id,
  seat,
  fixed = false,
  approvers = "",
  onAnswered,
}: {
  /** The held call, as its row names it (`held_call.id`). */
  id: string;
  /** The seat whose run holds the call, by handle: who may answer is its rule. */
  seat: string;
  /**
   * A call its tool asks about every time (an SSH operate command): answered
   * Allow once or Deny alone, Allow once after reading the whole call.
   */
  fixed?: boolean;
  /** Who answers it where not the seat's lead (`ssh`): the engine decides, so nothing is held here. */
  approvers?: string;
  /**
   * What the answer did, said, and whether it is settled (false where the
   * engine could not confirm the delivery, and answering again is safe).
   * Left out, the words are said in a toast.
   */
  onAnswered?: (words: string, settled: boolean) => void;
}) {
  const toast = useToast();
  const leadsHold = useAnswerHold(seat);
  // A CALL WITH ITS OWN APPROVERS is decided by the engine, which names them.
  const held = approvers ? undefined : leadsHold;
  const [busy, setBusy] = useState<HeldAnswer | null>(null);
  const [reviewing, setReviewing] = useState(false);
  // ONLY A REVIEWED FIXED CALL NAMES THE DIGEST: any other answer is sent as
  // the id and the answer alone, as an MCP tool's held call always was.
  const answer = async (how: HeldAnswer, ...reviewed: [] | [argumentsSha256: string]) => {
    setBusy(how);
    try {
      const sent = await answerHeldCall(id, how, ...reviewed);
      const words = heldAnswerWords(how, sent);
      const settled = sent.outcome !== "unknown";
      if (onAnswered) onAnswered(words, settled);
      else toast.show({ variant: settled ? "success" : "warning", title: words });
    } catch (err) {
      toast.show({
        variant: "danger",
        title: "The answer was not taken",
        message: refusalText(err),
      });
    } finally {
      setBusy(null);
    }
  };
  return (
    <>
      <Button
        size="small"
        variant="primary"
        tooltip
        loading={busy === "once"}
        disabled={busy !== null}
        disabledReason={held}
        onClick={() => (fixed ? setReviewing(true) : void answer("once"))}
      >
        {fixed ? "Review and allow once" : "Allow once"}
      </Button>
      {fixed ? null : (
        <Button
          size="small"
          variant="secondary"
          tooltip
          loading={busy === "always"}
          disabled={busy !== null}
          disabledReason={held}
          onClick={() => void answer("always")}
        >
          Always allow
        </Button>
      )}
      {/* OUTLINED LIKE ALWAYS ALLOW: a ghost Deny beside two framed answers read as held. */}
      <Button
        size="small"
        variant="secondary"
        tooltip
        loading={busy === "deny"}
        disabled={busy !== null}
        disabledReason={held}
        onClick={() => void answer("deny")}
      >
        Deny
      </Button>
      {reviewing ? (
        <HeldCallReview
          id={id}
          onClose={() => setReviewing(false)}
          onAllow={(digest) => {
            setReviewing(false);
            void answer("once", digest);
          }}
        />
      ) : null}
    </>
  );
}

/*
 * THE WHOLE CALL, READ BEFORE ALLOW ONCE: every argument, its standard input
 * whole, as the engine opens it for whoever may answer. The answer names the
 * digest of what was shown, so it is never given from a view that showed less.
 */
function HeldCallReview({
  id,
  onClose,
  onAllow,
}: {
  id: string;
  onClose: () => void;
  onAllow: (argumentsSha256: string) => void;
}) {
  const [whole, setWhole] = useState<McpHeldWhole | null>(null);
  const [failed, setFailed] = useState<string>("");
  useEffect(() => {
    let live = true;
    readHeldCall(id).then(
      (read) => live && setWhole(read),
      (err: unknown) => live && setFailed(refusalText(err)),
    );
    return () => {
      live = false;
    };
  }, [id]);
  return (
    <Modal
      open
      title="Review the call"
      size="lg"
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!whole}
            onClick={() => whole && onAllow(whole.arguments_sha256)}
          >
            Allow once
          </Button>
        </>
      }
    >
      {failed ? (
        <Text as="p" variant="body">
          {failed}
        </Text>
      ) : whole ? (
        <>
          <Text as="p" variant="body">
            {whole.question}
          </Text>
          <CodeBlock language="json" code={JSON.stringify(whole.arguments, null, 2)} />
        </>
      ) : (
        <Text as="p" variant="body">
          Reading the call.
        </Text>
      )}
    </Modal>
  );
}
