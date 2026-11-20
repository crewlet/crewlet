/**
 * Apply, in place: the panel a finding's fix opens under its own row (or
 * under its fix on the finding's page), never a dialog over the list. It
 * re-checks first (`apply.ts` says why), then shows the change in a line or
 * two and its effect, and the person confirms where they pressed.
 *
 * WHAT IT SAYS, by the engine's answer, one line each:
 *
 * - checking: that it is re-checking, and nothing else;
 * - holds: the change (each field as it is and as it would be) and what it
 *   clears, Confirm;
 * - changed: what moved since it was found and the updated change, Confirm
 *   the updated change;
 * - resolved, not applicable, applied elsewhere or closed: where it stands,
 *   and that nothing was sent;
 * - a permission the reader lacks: who can apply it;
 * - applied: until when the Advisor checks its effect.
 *
 * THE CHOICE IS MADE BEFORE IT OPENS: a fix with several changes offers each
 * as its own press, labelled with what it does (the row's button and its
 * menu), so the panel re-checks and sends exactly the option pressed and
 * asks for no second choice.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { Button, ChangeList, Text, type ChangeListRow } from "@crewlethq/ui";
import { useClient } from "~/lib/store-hooks.ts";
import { ago } from "~/lib/ago.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtAt } from "~/lib/format.ts";
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
import { CLOSE_REASONS, STATE_WORDS, impactWords } from "./words.ts";

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

/** Where an Apply stands, in a few words: always the state, never the finding, which the row names. */
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

export function ApplyPanel({
  finding,
  option,
  onClose,
  onSettled,
}: {
  finding: AdvisorKeptFinding;
  /** The fix's option the press named, re-checked and sent. */
  option: number;
  onClose: () => void;
  /** The finding moved (applied, resolved, retired, revised): the page reads it again. */
  onSettled: () => void;
}) {
  const { socket } = useClient();
  const read: ReadFinding = useCallback((id) => socket.query("advisor_finding", { id }), [socket]);
  const [state, setState] = useState<ApplyState>({ kind: "checking" });
  const live = useRef(new AbortController());
  const settled = useRef(false);
  const self = useRef<HTMLDivElement>(null);
  const now = useNow();

  // THE PANEL TAKES FOCUS AS IT OPENS, so a keyboard reader is where it
  // speaks, and is handed the change before the Confirm that sends it.
  useEffect(() => self.current?.focus(), []);

  // A STEP ANSWERS ONLY WHILE ITS OWN RE-CHECK STANDS: one started before the
  // panel closed is dropped, however late it lands, and its cancelled request
  // is no failure.
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
          saved: err instanceof SentUntold,
        }));
      }
    }
  }, []);

  // THE RE-CHECK RUNS AS THE PANEL OPENS: nothing is offered from the
  // finding as it was stored, however recently.
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

  // ANY ANSWER BUT "HOLDS" MOVED THE FINDING: the page is told once.
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
  const sending = state.kind === "sending" ? "Sending the change" : undefined;

  return (
    <div className="advisor-apply" ref={self} tabIndex={-1} aria-busy={busy}>
      <Text as="p" className="advisor-apply__state" aria-live="polite">
        <strong>{titleOf(state)}</strong> {line(state, check, now)}
      </Text>
      {check && check.moved.length > 0 ? (
        <ChangeList
          layout="inline"
          heading={check.outcome === "changed" ? "What moved" : "What shows it"}
          rows={movedRows(check.moved)}
        />
      ) : null}
      {check && offer && check.preview ? (
        check.preview.rows.length > 0 ? (
          <ChangeList layout="inline" heading="The change" rows={previewRows(check.preview.rows)} />
        ) : (
          <Text as="p" tone="secondary">
            The change sets nothing that is not already so.
          </Text>
        )
      ) : null}
      {check && offer ? (
        <Text as="p" tone="secondary" className="advisor-apply__effect">
          {effect(finding)}
          {check.preview?.notes.map((n) => ` ${n}`)}
        </Text>
      ) : null}
      <div className="advisor-apply__actions">
        {offer ? (
          <Button
            size="small"
            variant="primary"
            disabledReason={sending}
            onClick={() => apply(check)}
          >
            {check.outcome === "changed" ? "Confirm the updated change" : "Confirm"}
          </Button>
        ) : null}
        <Button size="small" variant="ghost" disabledReason={sending} onClick={onClose}>
          {offer ? "Cancel" : "Close"}
        </Button>
      </div>
    </div>
  );
}

/** The state's one line after its title. */
function line(state: ApplyState, check: AdvisorRecheck | null, now: number): string {
  switch (state.kind) {
    case "checking":
      return "against the latest records, so nothing is applied on what was true when it was found.";
    case "applied":
      return state.finding.effect_due_at
        ? `The Advisor checks its effect until ${fmtAt(state.finding.effect_due_at, now)}.`
        : "The Advisor checks its effect.";
    case "moved":
      return standing(state.finding, now);
    case "failed":
      return state.message;
  }
  if (check?.refusal) return `${check.refusal.reason} ${check.refusal.who_can}`;
  switch (check?.outcome) {
    case "holds":
      return `Checked ${ago(check.checked_at, now)}: the condition and its change are as found.`;
    case "changed":
      return "What it rests on moved, so the change was built again. Nothing is sent until you confirm.";
    case "resolved":
      return `${check.finding.close_reason ? CLOSE_REASONS[check.finding.close_reason] : "The condition no longer holds"}. Nothing was sent.`;
    case "not_applicable":
      return `${check.finding.close_reason ? CLOSE_REASONS[check.finding.close_reason] : "What it was about is gone"}. Nothing was sent.`;
  }
  return "";
}

/** A finding that left the list since the page read it: where it stands. */
function standing(f: AdvisorKeptFinding, now: number): string {
  if (f.state === "applied") {
    return f.effect_due_at
      ? `It was applied from another window or by another person. The Advisor checks its effect until ${fmtAt(f.effect_due_at, now)}.`
      : "It was applied from another window or by another person.";
  }
  const reason = f.close_reason ? CLOSE_REASONS[f.close_reason] : STATE_WORDS[f.state];
  return f.closed_at
    ? `${reason}. It closed ${ago(f.closed_at, now)}, and nothing was sent.`
    : `${reason}. Nothing was sent.`;
}

/** What sending it is for: the finding it clears, and what that is worth where the engine measured it. */
function effect(f: AdvisorKeptFinding): string {
  const worth = impactWords(f);
  return `Sending it should clear this finding${worth ? `, worth ${worth}` : ""}; the Advisor then checks that it did.`;
}
