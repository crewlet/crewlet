/**
 * A seat moved to a model from its picker on Settings › Models, in one
 * confirm. EVERY MODEL PICKER ON THE PAGE WORKS ALIKE: a pick asks once, then
 * saves in place, as Ask Crewlet's and the Advisor's do. A seat's pick opened
 * the model's whole wizard while a helper's saved at once, so one control did
 * two different things on one page.
 *
 * THE SAME CHANGE FINISH AND SAVE MAKES: the model's uses with the seat's
 * categories added, written job by job (`nextDocument`), read against the
 * revision it is written on, checked by the engine's dry run, then saved as one
 * revision. The confirm says each job that moves, from where to where, and any
 * fallback the write drops, before anything is written.
 */

import { useState } from "react";
import { Callout, NoticeList } from "@crewlethq/ui";
import { ConfirmModal } from "~/components/Modal.tsx";
import { listed } from "~/lib/format.ts";
import { intentOf, withCategory, type JobCategory, type Uses } from "~/lib/modelUses.ts";
import { configGuardedReason } from "~/lib/useWriteAccess.ts";
import { managedSentence, unsettledSentence } from "~/protocol/configAnswer.ts";
import { getConfig, type ConfigWriteOutcome } from "~/protocol/configWrite.ts";
import { writeDocument } from "../api.ts";
import type { ModelsData } from "../data.ts";
import { seatName } from "../places.ts";
import { CATEGORY_WORDS } from "../words.ts";
import { baseOf, draftOf, messageOf, nextDocument } from "../wizard/draft.ts";
import { droppedWords } from "../wizard/steps/Review.tsx";
import { seatChangesOf } from "../wizard/steps/UseItFor.tsx";

/** A pick to confirm: the model, the seat and the categories it takes on it. */
export interface SeatMoveAsk {
  key: string;
  seat: string;
  categories: readonly JobCategory[];
}

/** The model's uses with the seat's categories added. */
export function movedUses(uses: Uses, ask: SeatMoveAsk): Uses {
  return ask.categories.reduce((u, c) => withCategory(u, ask.seat, c, true), uses);
}

/** Why the move was not saved, in one sentence a person can act on. */
function notMoved(outcome: ConfigWriteOutcome): string {
  switch (outcome.kind) {
    case "valid":
    case "saved":
    case "unchanged":
      return "";
    case "problems":
      return outcome.problems.length > 0
        ? outcome.problems.map((p) => messageOf(p)).join(" ")
        : "The engine refused the change.";
    case "guarded":
      return configGuardedReason(outcome.code);
    case "managed":
      return managedSentence(outcome.managedBy);
    case "conflict":
      return "The configuration changed while this was open. Choose again.";
    case "unsettled":
      return `${unsettledSentence(outcome)} The seat may or may not have moved; reload the page to see.`;
    default:
      return `Nothing was saved${"detail" in outcome && outcome.detail ? `: ${outcome.detail}` : "."}`;
  }
}

export function SeatMoveDialog({
  ask,
  data,
  onClose,
}: {
  ask: SeatMoveAsk | null;
  data: ModelsData;
  onClose: () => void;
}) {
  const [refused, setRefused] = useState("");
  const model = ask ? data.models.find((m) => m.key === ask.key) : undefined;
  if (!ask || !model) return null;
  const next = movedUses(model.uses, ask);
  const name = seatName(data.seats, ask.seat);
  const moves = seatChangesOf(data, model.key, model.uses, next);
  const draft = { ...draftOf(model), uses: next, intent: intentOf(next) };
  const editing = { base: baseOf(model), was: model.uses };
  const dropped = data.doc ? nextDocument(data.doc, draft, editing).dropped : [];
  const what = listed(ask.categories.map((c) => CATEGORY_WORDS[c].short.toLowerCase()));
  const close = () => {
    setRefused("");
    onClose();
  };

  async function move() {
    setRefused("");
    const read = await getConfig();
    if (read.kind !== "document") {
      setRefused("No company is configured, so there is nothing to move.");
      return;
    }
    const { doc } = nextDocument(read.document, draft, editing);
    const checked = await writeDocument(doc, { etag: read.etag, summary: "", dryRun: true });
    if (checked.kind !== "valid") {
      setRefused(notMoved(checked));
      return;
    }
    const saved = await writeDocument(doc, {
      etag: read.etag,
      summary: `Move ${name} to ${model!.key} for ${what}`,
      dryRun: false,
    });
    if (saved.kind !== "saved") {
      setRefused(notMoved(saved));
      return;
    }
    data.reload();
    close();
  }

  return (
    <ConfirmModal
      open
      onClose={close}
      onConfirm={move}
      title={`Move ${name} to ${model.key}?`}
      confirmLabel="Move"
    >
      <NoticeList
        label="What moves"
        notices={[
          ...moves.map((line) => ({ key: line, title: line })),
          ...dropped.map((d) => ({
            key: `${d.seat}-${d.job}`,
            title: droppedWords(d, (h) => seatName(data.seats, h)),
          })),
        ]}
      />
      {refused ? (
        <Callout variant="danger" title="Not moved" live="polite">
          {refused}
        </Callout>
      ) : null}
    </ConfirmModal>
  );
}
