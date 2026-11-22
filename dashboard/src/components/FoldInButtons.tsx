/**
 * The two answers to a fold-in: whether another person's request, waiting
 * behind a running executor, joins that executor's work on the apps of whom
 * the work is for (ADR-0109). Sent as `answer_executor{fold_in, choice}`, the
 * shape a held call's approval takes beside a parked run's question: ONE SET
 * OF BUTTONS for every place a fold-in is answered (a decision row, the
 * Inbox's pane), so the two cannot disagree about which answer is primary.
 *
 * The engine decides who may answer (the run's requester, a lead of its seat,
 * an administrator; never the asker) and records the first answer: a later
 * press is refused in its words, drawn under the buttons.
 */

import { WriteButton } from "./WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { possessive } from "~/lib/format.ts";
import type { FoldInItem } from "~/protocol/index.ts";

/** The engine's ids for the two answers (internal/sandbox's FoldInAnswers). */
export const FOLD_IN_APPROVE = "approve";
export const FOLD_IN_REFUSE = "refuse";

export function FoldInButtons({
  item,
  seat,
  primary = true,
  onAnswered,
}: {
  item: FoldInItem;
  /** The busy seat's name, as the toast says it. */
  seat: string;
  /** Whether approving is the row's primary answer: put to the reader. */
  primary?: boolean;
  /** What the answer came to, once the engine recorded it. */
  onAnswered?: (choice: string) => void;
}) {
  const write = useAct("answer_executor");
  const answer = async (choice: string, done: string) => {
    const result = await write.run(
      { turn_id: item.turn_id, launch_id: item.launch_id, fold_in: item.id, choice },
      { done },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onAnswered?.(choice);
  };
  const label = (id: string, fallback: string) =>
    item.decision.options.find((o) => o.id === id)?.label ?? fallback;
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant={primary ? "primary" : "secondary"}
        onPress={() =>
          void answer(FOLD_IN_APPROVE, `Added the request to ${possessive(seat)} running work`)
        }
      >
        {label(FOLD_IN_APPROVE, "Add it to the running work")}
      </WriteButton>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        showRefusal={false}
        onPress={() => void answer(FOLD_IN_REFUSE, "Left the request to run on its own")}
      >
        {label(FOLD_IN_REFUSE, "Run it on its own")}
      </WriteButton>
    </>
  );
}
