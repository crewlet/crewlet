/**
 * A seat moved to a fleet or a model from its picker under Who runs on which
 * model, in one confirm: the seat once, each job that moves from where to
 * where, any fallback the write drops. Move writes the document as one
 * revision (`applySeatTarget`), checked by the engine's dry run first.
 */

import { useState } from "react";
import { Callout } from "@crewlethq/ui";
import { ConfirmModal } from "~/components/Modal.tsx";
import { applySeatTarget, type SeatJob } from "~/lib/modelUses.ts";
import { SEAT_JOB_WORDS } from "~/lib/seatJobs.ts";
import { configGuardedReason } from "~/lib/useWriteAccess.ts";
import { managedSentence, unsettledSentence } from "~/protocol/configAnswer.ts";
import { getConfig, type ConfigWriteOutcome } from "~/protocol/configWrite.ts";
import { writeDocument } from "../api.ts";
import type { ModelsData } from "../data.ts";
import { seatName } from "../places.ts";
import { messageOf } from "../wizard/draft.ts";
import { droppedWords } from "../wizard/steps/Review.tsx";

/** A pick to confirm: the seat, the jobs it moves (every one, or some) and where to. */
export interface SeatMoveAsk {
  seat: string;
  jobs: readonly SeatJob[] | "all";
  /** An entry key, or `fleet:<key>`. */
  value: string;
  /** What each moving job runs on now, in words, by job ("all" for every job). */
  from: Readonly<Partial<Record<SeatJob | "all", string>>>;
  /** Where it moves, in words: "Fleet · Claude Opus", or the model's key. */
  to: string;
}

/** Why a write was not saved, in one sentence a person can act on; `unsure` where it may have been. */
export function notSaved(outcome: ConfigWriteOutcome, unsure: string): string {
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
      return `${unsettledSentence(outcome)} ${unsure}; reload the page to see.`;
    default:
      return `Nothing was saved${"detail" in outcome && outcome.detail ? `: ${outcome.detail}` : "."}`;
  }
}

const MAYBE = "The seat may or may not have moved";

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
  if (!ask) return null;
  const name = seatName(data.seats, ask.seat);
  const dropped = data.doc ? applySeatTarget(data.doc, ask.seat, ask.jobs, ask.value).dropped : [];
  const close = () => {
    setRefused("");
    onClose();
  };

  async function move(pick: SeatMoveAsk) {
    setRefused("");
    const read = await getConfig();
    if (read.kind !== "document") {
      setRefused("No company is configured, so there is nothing to move.");
      return;
    }
    const { doc } = applySeatTarget(read.document, pick.seat, pick.jobs, pick.value);
    const checked = await writeDocument(doc, { etag: read.etag, summary: "", dryRun: true });
    if (checked.kind !== "valid") {
      setRefused(notSaved(checked, MAYBE));
      return;
    }
    const jobs =
      pick.jobs === "all" ? "every job" : pick.jobs.map((j) => SEAT_JOB_WORDS[j].label).join(", ");
    const saved = await writeDocument(doc, {
      etag: read.etag,
      summary: `Move ${name} to ${pick.to} for ${jobs}`,
      dryRun: false,
    });
    if (saved.kind !== "saved") {
      setRefused(notSaved(saved, MAYBE));
      return;
    }
    data.reload();
    close();
  }

  const rows = (ask.jobs === "all" ? (["all"] as const) : ask.jobs).map((job) => ({
    kind: "changed" as const,
    label: job === "all" ? "All jobs" : SEAT_JOB_WORDS[job].label,
    from: ask.from[job] ?? "Not set",
    to: ask.to,
  }));
  return (
    <ConfirmModal
      open
      onClose={close}
      onConfirm={() => move(ask)}
      title={`Move ${name} to ${ask.to}?`}
      message="It moves at its next turn; a running turn finishes where it is."
      confirmLabel="Move"
      changes={[{ key: ask.seat, subject: name, subjectText: name, rows }]}
      consequencesHeading={dropped.length > 0 ? "What else changes" : undefined}
      consequences={dropped.map((d) => ({
        kind: "fact" as const,
        key: `${d.seat}-${d.job}`,
        tone: "warning" as const,
        text: droppedWords(d, (h) => seatName(data.seats, h)),
      }))}
    >
      {refused ? (
        <Callout variant="danger" title="Not moved" live="polite">
          {refused}
        </Callout>
      ) : null}
    </ConfirmModal>
  );
}
