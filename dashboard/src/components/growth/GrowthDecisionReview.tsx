/**
 * A held growth call read whole and decided in place: the engine's decision
 * card for it (`GET /mcp-approvals/{id}`'s `card`), answered Allow once or
 * Deny with a note back to the agent.
 *
 * ALLOW ONCE NAMES WHAT WAS SHOWN. The answer carries the digest of the call
 * as it was read, so it is never given from a view that showed less. The
 * engine asks for a fresh sign-in before it takes the answer, and the
 * transport asks the person for it (`app/StepUp.tsx`). A call whose card
 * could not be read is decided on the call itself, as the engine allows.
 */

import { useEffect, useState } from "react";
import { Button, Callout, Inline, Stack, Text } from "@crewlethq/ui";
import { HeldCallWhole } from "~/components/HeldCallWhole.tsx";
import { Modal } from "~/components/Modal.tsx";
import type { GrowthAct } from "~/contract/growth.ts";
import type { McpHeldWhole } from "~/contract/mcp.ts";
import { answerHeldCall, heldAnswerWords, readHeldCall } from "~/lib/heldCall.ts";
import { refusalText } from "~/lib/refusal.ts";
import { GrowthDecisionCard } from "./GrowthDecisionCard.tsx";

/** The acts a held growth call asks a person to allow, none of them ever a delegate's. */
const GROWTH_ACTS: ReadonlySet<string> = new Set<GrowthAct>([
  "ad_spend",
  "public_post",
  "account_admin",
]);

/** Whether a held call's act is a growth decision, answered on its card. */
export function isGrowthAct(act: string | undefined): act is GrowthAct {
  return act !== undefined && GROWTH_ACTS.has(act);
}

interface GrowthDecisionProps {
  id: string;
  /** What the answer did, said, and whether it is settled. */
  onAnswered: (words: string, settled: boolean) => void;
  /** Whether an answer is on its way, for a host that holds its own close. */
  onBusy?: (busy: boolean) => void;
}

/**
 * A held growth call read whole and drawn as its card, decided in place: the
 * Inbox's reading pane holds it, and the review dialog of a decision row.
 */
export function GrowthDecision(props: GrowthDecisionProps) {
  // EACH CALL OPENS FRESH: a run's next held call never shows the last one's card or note.
  return <Decision key={props.id} {...props} />;
}

function Decision({ id, onAnswered, onBusy }: GrowthDecisionProps) {
  const [whole, setWhole] = useState<McpHeldWhole | null>(null);
  const [failed, setFailed] = useState("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState<"once" | "deny" | null>(null);
  const [refusal, setRefusal] = useState("");
  // Why the call could not be read again after a refusal: said beside it, the card kept.
  const [unread, setUnread] = useState("");
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
  const answer = async (how: "once" | "deny") => {
    if (!whole || busy) return;
    setBusy(how);
    onBusy?.(true);
    setRefusal("");
    setUnread("");
    try {
      const sent = await answerHeldCall(
        id,
        how,
        how === "once" ? whole.arguments_sha256 : undefined,
        note,
      );
      onAnswered(heldAnswerWords(how, sent), sent.outcome !== "unknown");
    } catch (err) {
      setRefusal(refusalText(err));
      // A REFUSED ANSWER may mean the call moved or lapsed: read it again, so the card says where it stands.
      readHeldCall(id).then(setWhole, (again: unknown) =>
        setUnread(`The call could not be read again: ${refusalText(again)}`),
      );
    } finally {
      setBusy(null);
      onBusy?.(false);
    }
  };
  if (failed) return <Text as="p">{failed}</Text>;
  if (!whole) return <Text as="p">Reading the call.</Text>;
  if (!whole.card)
    return (
      <WholeCall
        whole={whole}
        busy={busy}
        refusal={[refusal, unread].filter(Boolean).join(" ")}
        onAllow={() => void answer("once")}
        onDeny={() => void answer("deny")}
      />
    );
  return (
    <GrowthDecisionCard
      card={whole.card}
      question={whole.question}
      state={busy === "once" ? "applying" : "pending"}
      // A DENIAL ON ITS WAY holds Allow once with what is happening, never "Applying". A MOVED
      // TARGET runs nothing if allowed: Deny stays, and the agent asks again on what is there now.
      held={
        busy === "deny"
          ? "Sending your denial."
          : whole.card.moved
            ? "What this changes moved since the agent asked, so allowing it would run nothing. Deny it, and the agent asks again."
            : undefined
      }
      note={note}
      onNote={setNote}
      onAllow={() => void answer("once")}
      onDeny={() => void answer("deny")}
      refusal={refusal ? { sentence: refusal, hint: unread || undefined } : undefined}
    />
  );
}

/** A growth call whose card could not be read (its vendor did not answer): decided on the call itself. */
function WholeCall({
  whole,
  busy,
  refusal,
  onAllow,
  onDeny,
}: {
  whole: McpHeldWhole;
  busy: "once" | "deny" | null;
  refusal: string;
  onAllow: () => void;
  onDeny: () => void;
}) {
  return (
    <Stack gap={3}>
      <Callout variant="warning">
        The vendor could not be read just now, so this ask comes without its card. It can still be
        decided on the call as the agent made it, below.
      </Callout>
      <HeldCallWhole whole={whole} />
      {refusal ? <Callout variant="danger">{refusal}</Callout> : null}
      <Inline gap={2}>
        <Button
          size="small"
          variant="primary"
          loading={busy === "once"}
          disabled={busy !== null}
          onClick={onAllow}
        >
          Allow once
        </Button>
        <Button
          size="small"
          variant="secondary"
          loading={busy === "deny"}
          disabled={busy !== null}
          onClick={onDeny}
        >
          Deny
        </Button>
      </Inline>
    </Stack>
  );
}

/** A held growth call decided in a dialog, from a decision row that has no room for its card. */
export function GrowthDecisionReview({
  id,
  onClose,
  onAnswered,
}: {
  id: string;
  onClose: () => void;
  /** What the answer did, said, and whether it is settled. */
  onAnswered: (words: string, settled: boolean) => void;
}) {
  const [busy, setBusy] = useState(false);
  return (
    <Modal
      open
      title="An agent asks"
      size="lg"
      onClose={onClose}
      closeDisabledReason={busy ? "Sending your answer." : undefined}
    >
      <GrowthDecision
        id={id}
        onBusy={setBusy}
        onAnswered={(words, settled) => {
          onAnswered(words, settled);
          onClose();
        }}
      />
    </Modal>
  );
}
