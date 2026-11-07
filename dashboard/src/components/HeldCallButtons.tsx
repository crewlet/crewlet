/**
 * The three answers to a call a run holds for approval (ADR-0077): Allow
 * once, Always allow and Deny, each sent through `POST /mcp-approvals/{id}`.
 *
 * ONE SET OF BUTTONS for every place a parked run is answered (a decision
 * row, the live run's banner, a seat's page), because the run's own answer
 * (`answer_executor`) is refused for a held call: a surface that drew a
 * held call's run with the run's Answer offered a button the engine refuses.
 */

import { useState } from "react";
import { Button, useToast } from "@crewlethq/ui";
import { answerHeldCall, heldAnswerWords, type HeldAnswer } from "~/lib/heldCall.ts";
import { refusalText } from "~/lib/refusal.ts";
import { useAnswerHold } from "~/lib/useWriteAccess.ts";

/*
 * HELD FOR A READER THE ENGINE DOES NOT ADMIT, through the rule the run's own
 * Answer is held by (`useAnswerHold`): drawn, and disabled with who may answer.
 */
export function HeldCallButtons({
  id,
  seat,
  onAnswered,
}: {
  /** The held call, as its row names it (`held_call.id`). */
  id: string;
  /** The seat whose run holds the call, by handle: who may answer is its rule. */
  seat: string;
  /**
   * What the answer did, said, and whether it is settled (false where the
   * engine could not confirm the delivery, and answering again is safe).
   * Left out, the words are said in a toast.
   */
  onAnswered?: (words: string, settled: boolean) => void;
}) {
  const toast = useToast();
  const held = useAnswerHold(seat);
  const [busy, setBusy] = useState<HeldAnswer | null>(null);
  const answer = async (how: HeldAnswer) => {
    setBusy(how);
    try {
      const sent = await answerHeldCall(id, how);
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
        onClick={() => void answer("once")}
      >
        Allow once
      </Button>
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
    </>
  );
}
