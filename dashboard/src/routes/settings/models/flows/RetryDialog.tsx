/**
 * A limit, and the two ways to stop waiting for it: Retry now, which asks the
 * vendor once and keeps the model out if the limit still holds; or Clear
 * sit-out, for when the plan was raised and the next call should try it.
 *
 * FOR EVERY MODEL ON THE PLAN. A sit-out belongs to the sign-in, and models
 * that share one (`ModelEntry.plan`) sit out together, so the dialog acts on
 * this model first and then on each one sharing its plan that still sits out.
 * One that answers `not_sitting_out` by then was cleared with the plan.
 *
 * Retry now is the safe action and takes the focus (design QA Minor 10); Clear
 * sit-out sends calls to a vendor that may still refuse them.
 */

import { useRef, useState } from "react";
import { Button, Callout, Modal, RelativeTime, Text, useToast } from "@crewlethq/ui";
import { fmtClock, listed } from "~/lib/format.ts";
import type { CredentialLimited } from "~/protocol/models.ts";
import { retryModel } from "../api.ts";
import type { ModelsData, ModelView } from "../data.ts";
import { WINDOW } from "../words.ts";

/** Why nothing was cleared: the limit holds, the probe failed, or the engine refused. */
type Outcome =
  | { kind: "still"; key: string; still: CredentialLimited }
  | { kind: "failed"; key: string; failed: string }
  | { kind: "not_sitting_out"; sentence: string }
  | { kind: "refused"; sentence: string };

/** The model and every other model on its plan that still sits out. */
export function planModels(model: ModelView, data: Pick<ModelsData, "models">): ModelView[] {
  const plan = model.admin?.plan ?? "";
  const sharing = plan
    ? data.models.filter(
        (m) => m.key !== model.key && m.admin?.plan === plan && m.entry.sitting_out !== null,
      )
    : [];
  return [model, ...sharing];
}

const sentenceCase = (s: string) => s.replace(/^./, (c) => c.toUpperCase());

export function RetryDialog({
  model,
  data,
  onClose,
  onDone,
}: {
  model: ModelView;
  data: ModelsData;
  onClose: () => void;
  onDone: () => void;
}) {
  const toast = useToast();
  const [busy, setBusy] = useState<"probe" | "clear" | null>(null);
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const retryRef = useRef<HTMLButtonElement>(null);
  const bench = model.entry.sitting_out;
  const models = planModels(model, data);
  const keys = models.map((m) => m.key);

  async function act(mode: "probe" | "clear") {
    setBusy(mode);
    setOutcome(null);
    try {
      for (const [i, key] of keys.entries()) {
        const answer = await retryModel(key, mode);
        if (answer.kind === "refused") {
          // ANOTHER MODEL ON THE PLAN no longer sitting out was cleared with
          // it; this model no longer sitting out has nothing to retry.
          if (answer.code === "not_sitting_out" && i > 0) continue;
          setOutcome(
            answer.code === "not_sitting_out"
              ? { kind: "not_sitting_out", sentence: answer.sentence }
              : { kind: "refused", sentence: answer.sentence },
          );
          return;
        }
        if (answer.value.cleared) continue;
        if (answer.value.failed) {
          setOutcome({ kind: "failed", key, failed: answer.value.failed });
          return;
        }
        if (answer.value.still) {
          setOutcome({ kind: "still", key, still: answer.value.still });
          return;
        }
      }
      toast.ok(
        mode === "clear"
          ? `Cleared the sit-out on ${listed(keys)}.`
          : `${listed(keys)} answered again.`,
      );
      onClose();
    } catch {
      // Only an abort rejects, and nothing here aborts: the dialog stays.
    } finally {
      setBusy(null);
      // A RETRY MOVES THE READING whether it cleared or not: a probe refused
      // for a limit re-benches with the vendor's fresh word.
      onDone();
    }
  }

  return (
    <Modal
      open
      onClose={onClose}
      title={`Model ${model.key} is sitting out`}
      size="md"
      initialFocus={() => retryRef.current}
      footer={
        <>
          <Button
            variant="secondary"
            loading={busy === "clear"}
            disabled={busy !== null || !bench}
            onClick={() => void act("clear")}
          >
            Clear sit-out
          </Button>
          <Button
            ref={retryRef}
            variant="primary"
            loading={busy === "probe"}
            disabled={busy !== null || !bench}
            onClick={() => void act("probe")}
          >
            Retry now
          </Button>
        </>
      }
    >
      <div className="models-stack">
        {bench ? (
          <Text as="p" variant="body">
            {sentenceCase(WINDOW[bench.window])}, since <RelativeTime value={bench.since} />. The
            engine tries again at {fmtClock(bench.until, Date.now())} (
            <RelativeTime value={bench.until} />
            ).
          </Text>
        ) : (
          <Text as="p" variant="body">
            This model is not sitting out now. Nothing waits for a limit to lift.
          </Text>
        )}
        {keys.length > 1 ? (
          <Text as="p" variant="body" tone="secondary">
            The models {listed(keys)} share one plan, so a retry or a clear applies to all of them.
          </Text>
        ) : null}
        <dl className="models-choices">
          <dt>Retry now</dt>
          <dd>
            Asks the vendor once. If the limit still holds, the model keeps sitting out and nothing
            changes.
          </dd>
          <dt>Clear sit-out</dt>
          <dd>
            For when you raised the plan or the vendor lifted the limit: the next call tries the
            model straight away.
          </dd>
        </dl>
        {outcome ? <OutcomeCallout outcome={outcome} /> : null}
      </div>
    </Modal>
  );
}

function OutcomeCallout({ outcome }: { outcome: Outcome }) {
  switch (outcome.kind) {
    case "still":
      return (
        <Callout variant="warning" title="Still limited" live="polite">
          <span className="models-stack">
            <span>
              The vendor still limits {outcome.key}. It sits out until{" "}
              {fmtClock(outcome.still.until, Date.now())} (
              <RelativeTime value={outcome.still.until} />
              ).
            </span>
            {outcome.still.said ? (
              <Text as="span" variant="caption" tone="secondary" mono>
                {outcome.still.said}
              </Text>
            ) : null}
          </span>
        </Callout>
      );
    case "failed":
      return (
        <Callout variant="danger" title="Nothing was cleared" live="polite">
          The test call to {outcome.key} failed for a reason other than a limit: {outcome.failed}
        </Callout>
      );
    case "not_sitting_out":
      return (
        <Callout variant="info" title="Nothing to retry" live="polite">
          {outcome.sentence}
        </Callout>
      );
    case "refused":
      return (
        <Callout variant="danger" title="Not cleared" live="polite">
          {outcome.sentence}
        </Callout>
      );
  }
}
