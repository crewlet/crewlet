/**
 * Apply, as the person reads it: "Checking it still holds", then the engine's
 * answer (`apply.ts` says why every Apply is re-checked first):
 *
 * - holds: the facts as checked now, and the change it sends, Apply change;
 * - changed: "Since this was found", each moved value was beside now, and the
 *   updated change, Apply the updated change or Keep it open;
 * - resolved: "No longer needed", with what shows it, and nothing sent;
 * - not applicable: why, and nothing sent;
 * - a permission the reader lacks: who can apply it, the finding left open;
 * - a finding no longer open (applied from another window, closed since the
 *   page read it): where it now stands, and nothing sent.
 *
 * After a send, the finding reads "Applied, checking its effect until …".
 *
 * THE FIX'S CHOICES BELONG TO AN ANSWER THAT CAN SEND. A fix offering several
 * changes (a grant to one of seven agents) is chosen from only once the
 * engine answered that the condition holds: while it checks, after it answered
 * that nothing is to be sent, and after a failure, there is nothing to choose,
 * and a live choice would invite one. NONE IS CHOSEN FOR THE PERSON: which
 * agent gets an access, or whether a schedule is slowed or removed, is theirs
 * to pick, and the change is shown and sent only once they have.
 *
 * ON A PHONE IT IS A SHEET (`phoneSheet`): its facts, choices and change fill
 * the screen and scroll between a still head and foot, where a centred card
 * scrolled them in a box a few rows tall.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Callout, ChangeList, ChoiceList, Text, type ChangeListRow } from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import { useClient } from "~/lib/store-hooks.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { marked } from "~/ui/Problems.tsx";
import {
  applyFrom,
  failureWords,
  recheck,
  sendable,
  SentUntold,
  type ApplyState,
  type ReadFinding,
} from "./apply.ts";
import type {
  AdvisorKeptFinding,
  AdvisorMoved,
  AdvisorPreviewRow,
  AdvisorRecheck,
} from "~/protocol/advisorFindings.ts";

import { CLOSE_REASONS, STATE_WORDS, factWords } from "./words.ts";

/** What the change sets, field by field: the engine's label, the value now and the value it would hold. */
export function previewRows(rows: readonly AdvisorPreviewRow[]): ChangeListRow[] {
  return rows.map((row): ChangeListRow => {
    if (row.before === null) return { kind: "added", label: row.label, to: row.after ?? "" };
    if (row.after === null) return { kind: "removed", label: row.label, from: row.before };
    return { kind: "changed", label: row.label, from: row.before, to: row.after };
  });
}

/** What moved since the finding was made, was beside now. */
export function movedRows(moved: readonly AdvisorMoved[]): ChangeListRow[] {
  return moved.map((m) => ({
    kind: "changed",
    label: m.label,
    from: String(m.was ?? "none"),
    to: String(m.now ?? "none"),
  }));
}

/** The dialog's title for each answer: always where it stands, never the finding, which is its subtitle. */
export function titleOf(state: ApplyState): string {
  switch (state.kind) {
    case "checking":
      return "Checking it still holds";
    case "applied":
      return "Applied";
    case "moved":
      return state.finding.state === "applied" ? "Already applied" : "Already closed";
    case "failed":
      if (state.saved) return "The change was saved";
      return state.check === null ? "Couldn't check it" : "The change wasn't applied";
  }
  switch (state.check.outcome) {
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
  const { socket } = useClient();
  const read: ReadFinding = useCallback((id) => socket.query("advisor_finding", { id }), [socket]);
  const [state, setState] = useState<ApplyState>({ kind: "checking" });
  // WHICH OF THE FIX'S OPTIONS IS RE-CHECKED: the one the person picks, where
  // the engine offers more than one, each re-checked as it is picked. Until
  // they pick, the first re-check reads the condition alone (on the first
  // option, as the engine builds one), and no change is shown or offered.
  const options = finding.fix.options;
  const choosing = options.length > 1;
  const [picked, setPicked] = useState<number | null>(null);
  const option = picked ?? 0;
  const ready = !choosing || picked !== null;
  // WHETHER AN ANSWER OFFERED A CHANGE TO SEND, after which the choices stay
  // in view while a newly picked one is re-checked.
  const [offered, setOffered] = useState(false);
  const live = useRef(new AbortController());
  const settled = useRef(false);

  // A STEP ANSWERS ONLY WHILE ITS OWN RE-CHECK STANDS: one started for an
  // option the person has since moved off (or a dialog since closed) is
  // dropped, however late it lands, and its cancelled request is no failure.
  const run = useCallback(async (signal: AbortSignal, step: () => Promise<ApplyState>) => {
    try {
      const next = await step();
      if (signal.aborted) return;
      if (next.kind === "checked" && sendable(next.check)) setOffered(true);
      setState(next);
    } catch (err) {
      if (!signal.aborted) {
        setState((was) => ({
          kind: "failed",
          message: failureWords(err),
          check: was.kind === "checked" || was.kind === "sending" ? was.check : null,
          saved: err instanceof SentUntold,
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
    void run(controller.signal, async () => {
      const answer = await recheck(finding.id, option, read, controller.signal);
      return answer.kind === "moved" ? answer : { kind: "checked", check: answer.check };
    });
    return () => controller.abort();
  }, [finding.id, option, read, run]);

  // ANY ANSWER BUT "HOLDS" MOVED THE FINDING (revised, closed, applied
  // elsewhere): the page is told once, so its row says so when the dialog closes.
  useEffect(() => {
    const moved =
      state.kind === "applied" ||
      state.kind === "moved" ||
      (state.kind === "checked" && state.check.outcome !== "holds");
    if (moved && !settled.current) {
      settled.current = true;
      onSettled();
    }
  }, [state, onSettled]);

  const apply = (check: AdvisorRecheck) => {
    setState({ kind: "sending", check });
    const { signal } = live.current;
    void run(signal, () => applyFrom(finding.id, option, check, Date.now, read, signal));
  };

  const check = state.kind === "checked" || state.kind === "sending" ? state.check : null;
  const busy = state.kind === "checking" || state.kind === "sending";
  const offer = check !== null && sendable(check);
  const showChoices =
    choosing && offered && (offer || state.kind === "checking" || state.kind === "sending");

  return (
    <Modal
      open
      phoneSheet
      onClose={onClose}
      title={titleOf(state)}
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
              disabledReason={
                state.kind === "sending"
                  ? "Sending the change"
                  : ready
                    ? undefined
                    : "Pick the change to make first"
              }
              onClick={() => apply(check)}
            >
              {check.outcome === "changed" ? "Apply the updated change" : "Apply change"}
            </Button>
          ) : null}
        </>
      }
    >
      <div className="col gap-3" aria-busy={busy}>
        {state.kind === "checking" && !showChoices ? (
          <Text as="p" tone="secondary">
            Reading the latest records and building the change again, so nothing is applied on what
            was true when it was found.
          </Text>
        ) : null}
        {state.kind === "applied" ? <Applied finding={state.finding} /> : null}
        {state.kind === "moved" ? <Standing finding={state.finding} /> : null}
        {state.kind === "failed" ? (
          <Callout
            variant={state.saved ? "warning" : "danger"}
            title={state.saved ? "The Advisor was not told" : "Nothing was applied"}
          >
            {state.message}
          </Callout>
        ) : null}
        {check ? <Answer check={check} change={ready} /> : null}
        {showChoices ? (
          <ChoiceList
            label="Which change"
            value={picked === null ? null : String(picked)}
            options={options.map((o, i) => ({ value: String(i), label: o.label }))}
            disabled={state.kind === "sending"}
            onChange={(value) => setPicked(Number(value))}
          />
        ) : null}
        {check && offer && ready && check.preview ? (
          <Change check={check} {...(options[option] ? { option: options[option].label } : {})} />
        ) : null}
      </div>
    </Modal>
  );
}

/** What the engine answered, in the words of its outcome, and the facts it read now. */
function Answer({ check, change }: { check: AdvisorRecheck; change: boolean }) {
  const facts = check.finding.evidence.facts;
  return (
    <>
      {check.outcome === "holds" ? (
        <Text as="p">
          Checked {fmtDateTime(check.checked_at)} against the latest records: the condition{" "}
          {change ? "and its change are" : "is"} the same as when it was found.
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
          <ChangeList
            heading={check.outcome === "changed" ? "What moved" : "What shows it"}
            rows={movedRows(check.moved)}
          />
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
    </>
  );
}

/**
 * The change the person would send, in words: the option they picked (which
 * names what it changes, "Let Agent SWE watch the engine's health") or the
 * fix's own sentence, then each field the engine labelled, as it is and as it
 * would be.
 */
function Change({ check, option }: { check: AdvisorRecheck; option?: string }) {
  const preview = check.preview!;
  return (
    <section aria-label="The change" className="col gap-1">
      <Text as="h3" variant="label">
        {check.outcome === "changed" ? "The updated change" : "The change"}
      </Text>
      <Text as="p">{option ?? marked(check.finding.fix.sentence)}</Text>
      {preview.rows.length > 0 ? (
        <ChangeList rows={previewRows(preview.rows)} />
      ) : (
        <Text as="p" tone="secondary">
          The change sets nothing that is not already so.
        </Text>
      )}
      {preview.notes.map((note) => (
        <Text as="p" key={note} tone="secondary">
          {marked(note)}
        </Text>
      ))}
    </section>
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

/** A finding that moved off the list since the page read it: where it stands, and that nothing was sent. */
function Standing({ finding }: { finding: AdvisorKeptFinding }) {
  if (finding.state === "applied") {
    return (
      <Callout variant="neutral" title="Nothing was sent from here">
        {finding.effect_due_at
          ? `It was applied from another window or by another person. The Advisor checks its effect until ${fmtDateTime(finding.effect_due_at)}.`
          : "It was applied from another window or by another person. The Advisor checks its effect."}
      </Callout>
    );
  }
  const reason = finding.close_reason
    ? CLOSE_REASONS[finding.close_reason]
    : STATE_WORDS[finding.state];
  return (
    <Callout variant="neutral" title="Nothing was sent from here">
      {reason}
      {finding.closed_at ? `, ${fmtDateTime(finding.closed_at)}.` : "."} It moves to History.
    </Callout>
  );
}
