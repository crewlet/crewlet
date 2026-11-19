/**
 * Apply, as the person reads it: "Checking it still holds", then the engine's
 * answer (`apply.ts` says why every Apply is re-checked first):
 *
 * - holds: the facts as checked now, and the change it sends, Apply change;
 * - changed: "Since this was found", each moved value was beside now, and the
 *   updated change, Apply the updated change or Keep it open;
 * - resolved: "No longer needed", with what shows it, and nothing sent;
 * - not applicable: why, and nothing sent;
 * - a permission the reader lacks: who can apply it, the finding left open.
 *
 * After a send, the finding reads "Applied, checking its effect until …".
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Callout, ChoiceList, DiffList, Text, type DiffRow } from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import { fmtDateTime } from "~/lib/format.ts";
import { marked } from "~/ui/Problems.tsx";
import { applyFrom, failureWords, recheck, sendable, type ApplyState } from "./apply.ts";
import type {
  AdvisorKeptFinding,
  AdvisorMoved,
  AdvisorPreviewRow,
  AdvisorRecheck,
} from "~/protocol/advisorFindings.ts";

import { CLOSE_REASONS, factWords } from "./words.ts";

/** What the change sets, field by field, as the kit's diff draws it. */
export function previewRows(rows: readonly AdvisorPreviewRow[]): DiffRow[] {
  return rows.map((row) => ({
    kind: row.before === null ? "added" : row.after === null ? "removed" : "changed",
    path: row.label,
    ...(row.before === null ? {} : { from: row.before }),
    ...(row.after === null ? {} : { to: row.after }),
  }));
}

/** What moved since the finding was made, was beside now. */
export function movedRows(moved: readonly AdvisorMoved[]): DiffRow[] {
  return moved.map((m) => ({
    kind: "changed",
    path: m.label,
    from: String(m.was ?? "none"),
    to: String(m.now ?? "none"),
  }));
}

/** The dialog's title for each answer. */
function titleOf(state: ApplyState, finding: AdvisorKeptFinding): string {
  if (state.kind === "checking") return "Checking it still holds";
  if (state.kind === "applied") return "Applied";
  const check = state.check;
  if (check === null) return finding.title;
  switch (check.outcome) {
    case "holds":
      return "It still holds";
    case "changed":
      return "It changed since it was found";
    case "resolved":
      return "No longer needed";
    case "not_applicable":
      return "This no longer applies";
  }
}

export function ApplyDialog({
  finding,
  onClose,
  onSettled,
}: {
  finding: AdvisorKeptFinding;
  onClose: () => void;
  /** The finding moved (applied, resolved, retired, revised): the page reads it again. */
  onSettled: () => void;
}) {
  const [state, setState] = useState<ApplyState>({ kind: "checking" });
  // WHICH OF THE FIX'S OPTIONS IS RE-CHECKED: the one the person picks, where
  // the engine offers more than one, each re-checked as it is picked.
  const options = finding.fix.options;
  const [option, setOption] = useState(0);
  const live = useRef(new AbortController());
  const settled = useRef(false);

  // A STEP ANSWERS ONLY WHILE ITS OWN RE-CHECK STANDS: one started for an
  // option the person has since moved off (or a dialog since closed) is
  // dropped, however late it lands, and its cancelled request is no failure.
  const run = useCallback(async (signal: AbortSignal, step: () => Promise<ApplyState>) => {
    try {
      const next = await step();
      if (!signal.aborted) setState(next);
    } catch (err) {
      if (!signal.aborted) {
        setState((was) => ({
          kind: "failed",
          message: failureWords(err),
          check: was.kind === "checked" || was.kind === "sending" ? was.check : null,
        }));
      }
    }
  }, []);

  // THE RE-CHECK RUNS AS THE DIALOG OPENS, and again for each option picked:
  // nothing is offered from the finding as it was stored, however recently.
  useEffect(() => {
    const controller = new AbortController();
    live.current = controller;
    setState({ kind: "checking" });
    void run(controller.signal, async () => ({
      kind: "checked",
      check: await recheck(finding.id, option, controller.signal),
    }));
    return () => controller.abort();
  }, [finding.id, option, run]);

  // ANY ANSWER BUT "HOLDS" MOVED THE FINDING (revised, closed): the page is
  // told once, so its row says so when the dialog closes.
  useEffect(() => {
    const moved =
      state.kind === "applied" || (state.kind === "checked" && state.check.outcome !== "holds");
    if (moved && !settled.current) {
      settled.current = true;
      onSettled();
    }
  }, [state, onSettled]);

  const apply = (check: AdvisorRecheck) => {
    setState({ kind: "sending", check });
    const { signal } = live.current;
    void run(signal, async () => {
      const answer = await applyFrom(finding.id, option, check, Date.now, signal);
      return answer.kind === "applied"
        ? { kind: "applied", finding: answer.finding }
        : { kind: "checked", check: answer.check };
    });
  };

  const check = state.kind === "checked" || state.kind === "sending" ? state.check : null;
  const busy = state.kind === "checking" || state.kind === "sending";
  const offer = check !== null && sendable(check);

  return (
    <Modal
      open
      onClose={onClose}
      title={titleOf(state, finding)}
      subtitle={marked(finding.title)}
      closeDisabledReason={state.kind === "sending" ? "Sending the change" : undefined}
      footer={
        <>
          <Button
            variant="ghost"
            onClick={onClose}
            disabledReason={state.kind === "sending" ? "Sending the change" : undefined}
          >
            {offer ? (check.outcome === "changed" ? "Keep it open" : "Cancel") : "Close"}
          </Button>
          {offer ? (
            <Button
              variant="primary"
              disabledReason={state.kind === "sending" ? "Sending the change" : undefined}
              onClick={() => apply(check)}
            >
              {check.outcome === "changed" ? "Apply the updated change" : "Apply change"}
            </Button>
          ) : null}
        </>
      }
    >
      <div className="col gap-3" aria-busy={busy}>
        {options.length > 1 && state.kind !== "applied" ? (
          <ChoiceList
            label="Which change"
            value={String(option)}
            options={options.map((o, i) => ({ value: String(i), label: o.label }))}
            disabled={state.kind === "sending"}
            onChange={(value) => setOption(Number(value))}
          />
        ) : null}
        {state.kind === "checking" ? (
          <Text as="p" tone="secondary">
            Reading the latest records and building the change again, so nothing is applied on what
            was true when it was found.
          </Text>
        ) : null}
        {state.kind === "applied" ? <Applied finding={state.finding} /> : null}
        {state.kind === "failed" ? (
          <Callout variant="danger" title="Nothing was applied">
            {state.message}
          </Callout>
        ) : null}
        {check ? <Answer check={check} /> : null}
      </div>
    </Modal>
  );
}

/** What the engine answered, in the words of its outcome. */
function Answer({ check }: { check: AdvisorRecheck }) {
  const facts = check.finding.evidence.facts;
  return (
    <>
      {check.outcome === "holds" ? (
        <Text as="p">
          Checked {fmtDateTime(check.checked_at)} against the latest records: the condition and its
          change are the same as when it was found.
        </Text>
      ) : null}
      {check.outcome === "changed" ? (
        <Callout variant="warning" title="Since this was found">
          The condition still holds, but what it rests on moved, so the change was built again.
          Nothing is sent until you apply the updated one.
        </Callout>
      ) : null}
      {check.outcome === "resolved" ? (
        <Callout variant="success" title="Nothing to apply">
          {check.finding.close_reason
            ? `${CLOSE_REASONS[check.finding.close_reason]}. `
            : "The condition no longer holds. "}
          The finding is closed and moves to History.
        </Callout>
      ) : null}
      {check.outcome === "not_applicable" ? (
        <Callout variant="neutral" title="Nothing to apply">
          {check.finding.close_reason
            ? `${CLOSE_REASONS[check.finding.close_reason]}. `
            : "What it was about is gone, or its change can no longer be made. "}
          The finding is retired and moves to History.
        </Callout>
      ) : null}
      {check.refusal ? (
        <Callout variant="warning" title="You can't apply this one">
          {check.refusal.reason} {check.refusal.who_can}
        </Callout>
      ) : null}
      {check.moved.length > 0 ? (
        <section aria-label="What moved since it was found" className="col gap-1">
          <Text as="h3" variant="label">
            {check.outcome === "changed" ? "What moved" : "What shows it"}
          </Text>
          <DiffList rows={movedRows(check.moved)} layout="auto" labels={{ to: "now" }} />
        </section>
      ) : null}
      {(check.outcome === "holds" || check.outcome === "changed") && facts.length > 0 ? (
        <section aria-label="As checked now" className="col gap-1">
          <Text as="h3" variant="label">
            As checked now
          </Text>
          <ul className="advisor-facts">
            {facts.map((fact) => (
              <li key={fact.key}>
                <span className="advisor-facts__label">{fact.label}</span>{" "}
                <span className="advisor-facts__value">{factWords(fact)}</span>
              </li>
            ))}
          </ul>
        </section>
      ) : null}
      {check.preview && (check.outcome === "holds" || check.outcome === "changed") ? (
        <section aria-label="The change" className="col gap-1">
          <Text as="h3" variant="label">
            {check.outcome === "changed" ? "The updated change" : "The change"}
          </Text>
          <Text as="p">{marked(check.finding.fix.sentence)}</Text>
          <DiffList
            rows={previewRows(check.preview.rows)}
            layout="auto"
            emptyMessage="The change sets nothing that is not already so."
          />
          {check.preview.notes.map((note) => (
            <Text as="p" key={note} tone="secondary">
              {marked(note)}
            </Text>
          ))}
        </section>
      ) : null}
    </>
  );
}

/** A sent change, waiting on the engine's check of its effect. */
function Applied({ finding }: { finding: AdvisorKeptFinding }) {
  return (
    <Callout variant="success" title="The change was applied">
      {finding.effect_due_at
        ? `The Advisor checks its effect until ${fmtDateTime(finding.effect_due_at)}, and closes the finding once the condition clears.`
        : "The Advisor checks its effect, and closes the finding once the condition clears."}
    </Callout>
  );
}
