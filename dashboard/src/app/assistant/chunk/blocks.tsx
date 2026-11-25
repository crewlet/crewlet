/**
 * The wire's closed sets as the design system's components draw them, and
 * the cards an answer carries.
 *
 * ONE MODULE MAPS EVERY WIRE STATE TO EXACTLY ONE COMPONENT STATE: a step's
 * state to `StepList`'s, a turn's phase to `RunChip`'s, a proposal's state to
 * `ProposalCard`'s, a change's kind to `ChangeList`'s row and a word's op to
 * its mark. Each map is a `Record` over the wire's union, so a member the
 * engine adds is a compile error here until it is drawn, and `blocks.test.ts`
 * holds each value to the component's own list.
 *
 * TWO RULES THE MAPPING KEEPS: an `unknown` proposal offers Retry under the
 * same operation key and no other failed one does; and a step-up the engine
 * asks of an approval is confirmed by the dashboard's one step-up dialog
 * (`protocol/rest.ts` replays the approval), never drawn as a failure.
 *
 * A READ IS NEVER A CARD: a table or a record a read built is drawn by its
 * answer (`reads.tsx`), as a chip where the prose names it and on one "Also
 * read" line where it does not, because the answer is the model's prose. A
 * proposal, a refusal and a receipt are the point of the turn and are drawn
 * whole, here.
 *
 * EVERY CARD IS THE ENGINE'S: built from tool results and its own route
 * table, never from the model's words. Only prose is the model's, and no
 * address it writes is opened (`ASSISTANT_WORDS` in `lib/markdown.ts`): a link
 * is live only where the dashboard built it, and no picture is fetched. A card
 * this build cannot draw is a sentence saying so.
 */

import { type ReactNode } from "react";
import {
  BriefCard,
  Button,
  Callout,
  CopyButton,
  OpenInAppCard,
  ProposalCard,
  Prose,
  ReceiptStrip,
  RefusalCard,
  type ChangeListRow,
  type ProposalState,
  type ReceiptStripTone,
  type RunState,
  type StepListStep,
  type StepState,
} from "@crewlethq/ui";
import { LayoutDashboardGlyph } from "@crewlethq/icons/glyphs";
import { DiffView } from "~/components/DiffView.tsx";
import {
  type AssistantBriefAction,
  type AssistantBriefPoint,
  type AssistantBriefCard,
  type AssistantCardEvent,
  type AssistantErrorCard,
  type AssistantProposalCard,
  type AssistantStepEvent,
  type AssistantTableCard,
} from "~/contract/assistant.ts";
import { narrowCard } from "~/protocol/assistantWire.ts";
import { statusLabel } from "~/lib/work.ts";
import { ASSISTANT_WORDS, renderMarkdown } from "~/lib/markdown.ts";
import { fmtCount, fmtDateTime, fmtDuration, fmtElapsed, fmtExact, fmtTime } from "~/lib/format.ts";
import { accessNames, youNeedWords } from "~/lib/access.ts";
import { remedyFor } from "~/lib/authorityRemedy.ts";
import { crumbsFor } from "../../crumbs.ts";
import { href as hrefOf, useIsShowing } from "../../router.tsx";
import { openInApp, type InAppTarget } from "./openInApp.ts";
import { pageActionPreview, usePageSnapshot, type PageActionPreview } from "../context.ts";
import { changedNothing, type ActionLocal, type ActionLocalState } from "./pageActions.ts";
import { DraftFile } from "./DraftFile.tsx";
import { drawPhrase, drawValue } from "./changeValue.tsx";
import { phraseText } from "./phrase.ts";
import { Phrase, Phrases } from "./refs.tsx";
import type { ProposalLocal, TurnPhase, TurnView } from "./turn.ts";
import { cautionOf } from "./taint.ts";
import { recordWord } from "../brief.ts";

type WireStepState = AssistantStepEvent["state"];
type WireProposalState = AssistantProposalCard["state"];
type WireChangeKind = AssistantProposalCard["changes"][number]["kind"];
type WireWordOp = NonNullable<AssistantProposalCard["changes"][number]["words"]>[number]["op"];
type WireColumnType = AssistantTableCard["columns"][number]["type"];

/** A step's state as `StepList` draws it. */
export const STEP_STATE: Record<WireStepState, StepState> = {
  running: "running",
  done: "done",
  failed: "failed",
  refused: "refused",
  stopped: "stopped",
  waiting: "waiting",
};

/** A turn's phase as `RunChip` draws it. */
export const RUN_STATE: Record<TurnPhase, RunState> = {
  sending: "thinking",
  thinking: "thinking",
  working: "working",
  awaiting_approval: "waiting",
  applying: "applying",
  unconfirmed: "failed",
  refused: "refused",
  completed: "done",
  stopped: "stopped",
  failed: "failed",
  interrupted: "failed",
  expired: "stopped",
  budget_exhausted: "failed",
};

/** A proposal's wire state as `ProposalCard` draws it. */
export const PROPOSAL_STATE: Record<WireProposalState, ProposalState> = {
  proposed: "pending",
  approved: "applying",
  executing: "applying",
  applying: "applying",
  applied: "applied",
  failed: "failed",
  // NOBODY CAN SAY WHETHER IT LANDED: drawn failed, and the one failed
  // proposal that offers Retry, under the same operation key.
  unknown: "failed",
  declined: "declined",
  expired: "expired",
  stale: "stale",
};

/** A change's wire kind as `ChangeList`'s row kind. */
export const CHANGE_KIND: Record<WireChangeKind, ChangeListRow["kind"]> = {
  fact: "fact",
  changed: "changed",
  added: "added",
  removed: "removed",
  text: "text",
};

/** A word's op as `ChangeList`'s word mark. */
export const WORD_OP: Record<WireWordOp, "=" | "+" | "-"> = {
  same: "=",
  added: "+",
  removed: "-",
};

/**
 * How a table's cell, or an entity's fact, is written, by its type: with the
 * formatters the dashboard's own screens use for the same fields, so the
 * drawer and the screens never word one value two ways.
 */
const CELL: Record<WireColumnType, (value: string | number | boolean | null) => string> = {
  text: (v) => (v === null ? "" : String(v)),
  number: (v) => (typeof v === "number" ? fmtExact(v) : v === null ? "" : String(v)),
  tokens: (v) => (typeof v === "number" ? fmtCount(v) : v === null ? "" : String(v)),
  duration: (v) => (typeof v === "number" ? fmtDuration(v) : v === null ? "" : String(v)),
  datetime: (v) => (typeof v === "string" ? fmtDateTime(v) : v === null ? "" : String(v)),
  // A closed set's slug, in the words the work screens name it by (a
  // project's own status words aside, which a card does not carry).
  label: (v) => (typeof v === "string" && v !== "" ? statusLabel(v) : v === null ? "" : String(v)),
};

/** A table's cell as it is drawn, by its column's type. */
export function cellText(type: WireColumnType, value: string | number | boolean | null): string {
  return CELL[type](value);
}

/** The types whose wire value is a number, which a fact carries as a string. */
const NUMERIC = new Set<WireColumnType>(["number", "tokens", "duration"]);

/** A fact's value as it is drawn, by its type. */
export function factText(fact: { value: string; type?: WireColumnType }): string {
  const type = fact.type ?? "text";
  const n = Number(fact.value);
  return CELL[type](NUMERIC.has(type) && fact.value !== "" && Number.isFinite(n) ? n : fact.value);
}

/**
 * Whether a refusal refused the person access, which the drawer draws as "Not
 * permitted": the API's permission codes, or a refusal naming grants, which
 * only the authority table names. Every other refusal (a value not accepted, a
 * record not found, a request the assistant wrote wrong) is not a permission
 * problem and is never drawn as one.
 *
 * NOT WHETHER ACCESS IS THE WAY PAST IT, which the rule that refused says
 * ([refusalWords]): a stale proof names the grants that admitted the person,
 * and a verb the assistant never takes stays with them whatever they hold.
 */
export function isAccessRefusal(refusal: { code: string; grants?: string[] } | undefined): boolean {
  if (!refusal) return false;
  if ((refusal.grants ?? []).length > 0) return true;
  return ACCESS_CODES.has(refusal.code);
}

/** The refusal codes that refuse the person access. */
const ACCESS_CODES: ReadonlySet<string> = new Set([
  "unauthorized",
  "forbidden",
  "access_withdrawn",
  "assistant_not_permitted",
]);

const steps = (n: number) => `${n} ${n === 1 ? "step" : "steps"}`;

/** A run's step count as the chip's meta: none for a turn that took no step. */
const stepMeta = (n: number) => (n > 0 ? steps(n) : undefined);

/** The run chip for a turn: its state, the words it says, and what it says beside them. */
export function runChipOf(
  turn: TurnView,
  now: number,
): { state: RunState; title: string; meta?: string } {
  const elapsed = turn.done?.duration_ms ?? Math.max(0, now - Date.parse(turn.askedAt));
  const taken = turn.done?.steps ?? turn.steps.length;
  const state = RUN_STATE[turn.phase];
  switch (turn.phase) {
    case "sending":
    case "thinking":
      // A LATER ROUND IS WORK: once a step has run, the model thinking
      // again is the turn still working, not a turn that has not started.
      if (turn.steps.length > 0) {
        return { state: RUN_STATE.working, title: "Working", meta: fmtElapsed(elapsed) };
      }
      return { state, title: "Thinking" };
    case "working":
      return { state, title: "Working", meta: fmtElapsed(elapsed) };
    case "applying":
      return { state, title: "Applying", meta: stepMeta(taken) };
    case "awaiting_approval":
      return { state, title: "Waiting for your approval", meta: stepMeta(taken) };
    case "completed": {
      // A TURN READ BACK FROM ITS TRANSCRIPT carries no duration; the time
      // since it was asked is not how long it ran, so it says Done alone.
      const done = turn.done ? `Done in ${fmtDuration(turn.done.duration_ms)}` : "Done";
      // A REFUSED STEP IS SAID ON THE CHIP, never only behind it.
      const refused = turn.steps.filter((s) => s.state === "refused").length;
      if (refused > 0) {
        return {
          state: RUN_STATE.refused,
          title: `${done}, ${refused === 1 ? "1 step" : `${refused} steps`} refused`,
          meta: stepMeta(taken),
        };
      }
      return { state, title: done, meta: stepMeta(taken) };
    }
    case "stopped":
    case "expired": {
      const done = turn.steps.filter((s) => s.state === "done").length;
      return {
        state,
        title: `Stopped after ${fmtDuration(elapsed)}`,
        meta: taken > 0 ? `${done} of ${steps(taken)}` : undefined,
      };
    }
    case "refused": {
      // NOT PERMITTED ONLY FOR AN ACCESS REFUSAL: a value the engine did not
      // accept is not a permission problem.
      const at = turn.steps.find((s) => s.state === "refused");
      if (at && !isAccessRefusal(at.refusal)) {
        return { state, title: "Could not finish", meta: stepMeta(taken) };
      }
      return { state, title: "Not permitted", meta: stepMeta(taken) };
    }
    case "unconfirmed":
      return { state, title: "Could not confirm the change" };
    case "failed":
    case "interrupted":
    case "budget_exhausted": {
      const at = turn.steps.find((s) => s.state === "failed" || s.state === "refused");
      const refused = at?.state === "refused";
      if (refused && isAccessRefusal(at.refusal)) {
        return { state: "refused", title: "Not permitted", meta: stepMeta(taken) };
      }
      if (refused) return { state, title: `Could not finish step ${at.index}` };
      return { state, title: at ? `Failed at step ${at.index}` : "Failed" };
    }
  }
}

/** The phases in which the assistant itself is busy on a turn: not waiting on the person, not applying a change. */
const BUSY: ReadonlySet<TurnPhase> = new Set(["sending", "thinking", "working"]);

/**
 * What the thinking indicator says under a turn, or null where it is not
 * drawn: only while the assistant itself is busy on the turn, never while it
 * waits on the person or applies an approved change, which their cards say.
 *
 * WHAT IS HAPPENING NOW, AND FOR HOW LONG: "Stopping", the running step's
 * sentence (the run chip counts its time), or "Thinking" then "Answering" with
 * the time since the question, so a long answer never reads as stalled.
 */
export function thinkingOf(turn: TurnView, now: number): { status: string } | null {
  if (!BUSY.has(turn.phase)) return null;
  if (turn.stopping) return { status: "Stopping" };
  const running = turn.steps.filter((step) => step.state === "running").at(-1);
  if (running) return { status: running.label };
  const writing = (turn.replies.at(-1)?.text.trim() ?? "") !== "";
  const elapsed = fmtElapsed(Math.max(0, now - Date.parse(turn.askedAt)));
  return { status: `${writing ? "Answering" : "Thinking"}, ${elapsed}` };
}

/**
 * The shortest step whose time is said. Under it a step said "0.0s", which
 * reads as a step that did nothing rather than one that was quick, so a step
 * that quick says no time at all.
 */
export const STEP_DURATION_FLOOR_MS = 100;

/** A step's time as its line says it, or nothing for one under the floor or not yet ended. */
export function stepDuration(ms: number | undefined): string | undefined {
  return ms === undefined || ms < STEP_DURATION_FLOOR_MS ? undefined : fmtDuration(ms);
}

/** A turn's chip steps as `StepList` draws them; `technical` for an administrator. */
export function stepsOf(turn: TurnView, technical: boolean): StepListStep[] {
  return stepListOf(turn.steps, technical);
}

/**
 * Steps as `StepList` draws them: the engine's sentence for each, its time,
 * why it failed or was refused as prose ([stepNote]), and the request under it.
 */
export function stepListOf(
  steps: readonly AssistantStepEvent[],
  technical: boolean,
): StepListStep[] {
  return steps.map((step) => {
    const duration = stepDuration(step.duration_ms);
    const note = stepNote(step);
    const detail = stepDetail(step, technical);
    return {
      id: step.step_id,
      label: step.label,
      state: STEP_STATE[step.state],
      ...(duration === undefined ? {} : { duration }),
      ...(note === "" ? {} : { note }),
      ...(detail === "" ? {} : { detail }),
    };
  });
}

/** Why a step failed or was refused, in the engine's sentence for a person; empty for any other. */
export function stepNote(step: TurnView["steps"][number]): string {
  if (step.state !== "failed" && step.state !== "refused") return "";
  return step.refusal
    ? [step.refusal.message, step.refusal.hint].filter(Boolean).join(" ")
    : (step.summary ?? "");
}

/**
 * The request a step made, in mono under its sentence, for an administrator.
 *
 * NO TOOL NAME, EVER: "Listing tasks" is what it did; "list_tasks · GET
 * /worklet" is how the engine spells it, which a person does not read.
 */
export function stepDetail(step: TurnView["steps"][number], technical: boolean): string {
  return technical && step.request ? `${step.request.method} ${step.request.route}` : "";
}

/**
 * A model's prose, drawn inert; `dimmed` for an abandoned attempt.
 *
 * NO CARET WHILE IT STREAMS: the turn's `ThinkingIndicator` stands after the
 * answer instead. A caret drawn by the block landed after its last paragraph
 * or list, on a line of its own, and on an empty reply alone above its cards.
 */
export function ReplyProse({ text, dimmed = false }: { text: string; dimmed?: boolean }) {
  // A SILENT ROUND IS STORED as an empty message its cards hang from, and
  // draws nothing of its own, streaming or not.
  if (text.trim() === "") return null;
  return <ProseBlock dimmed={dimmed}>{renderMarkdown(text, ASSISTANT_WORDS)}</ProseBlock>;
}

/**
 * Rendered Markdown as a reply draws it. A DIV, NOT PROSE'S DEFAULT PARAGRAPH:
 * rendered Markdown is blocks (paragraphs, lists), which a <p> may not hold.
 * `prose md` is the class pair the dashboard's Markdown rhythm is written for,
 * as every other rendered body here carries it, so the reply's paragraphs are
 * spaced. An answer whose mentions are chips (`reads.tsx`) is drawn by it too.
 *
 * ITS WORDS READ AT A MEASURE, ITS TABLES AND CODE AT THE THREAD'S WIDTH
 * (`ask-answer`): a measure on the whole block cut a table to a column.
 */
export function ProseBlock({
  children,
  dimmed = false,
}: {
  children: ReactNode;
  dimmed?: boolean;
}) {
  return (
    <Prose as="div" measure="none" dimmed={dimmed} className="prose md ask-answer">
      {children}
    </Prose>
  );
}

/**
 * A proposal's change rows in `ChangeList`'s grammar (D10): the facts first,
 * then WHO WILL SEE THE RESULT where its visibility changes (a share, a
 * comment others are told of, D09), as one more fact, then the changes. A
 * record a row names is drawn as itself, opened with `open`.
 */
export function changeRowsOf(
  proposal: AssistantProposalCard,
  open: (target: InAppTarget) => void,
): ChangeListRow[] {
  const rows = proposal.changes.map((change): ChangeListRow => {
    const note = change.note;
    const base = note === undefined ? { label: change.label } : { label: change.label, note };
    const to = drawPhrase(change.to, change.items, open);
    const from = drawPhrase(change.from, change.from_items, open);
    switch (CHANGE_KIND[change.kind]) {
      case "fact": {
        const value = change.to === undefined && change.from !== undefined ? from : to;
        return { ...base, kind: "fact", value: value.node, block: value.block };
      }
      case "changed":
        return {
          ...base,
          kind: "changed",
          from: from.node,
          to: to.node,
          block: from.block || to.block,
        };
      case "added":
        return { ...base, kind: "added", to: to.node, isNew: change.new === true, block: to.block };
      case "removed":
        return { ...base, kind: "removed", from: from.node, block: from.block };
      case "text": {
        // A TEXT ACROSS LINES (a page's body) IS DRAWN AS THE DOCUMENT IT IS,
        // a line diff the way the page's own History draws it: a word diff
        // ran its headings and bullets into one paragraph and read a line
        // break as a word removed and added again.
        const before = phraseText(change.from);
        const after = phraseText(change.to);
        if (before.includes("\n") || after.includes("\n")) {
          return {
            ...base,
            kind: "fact",
            value: <DiffView aria-label={change.label} before={before} after={after} />,
            block: true,
          };
        }
        return {
          ...base,
          kind: "text",
          words: (change.words ?? []).map((w) => ({ op: WORD_OP[w.op], text: w.text })),
        };
      }
    }
  });
  // THE EXACT REQUEST, which the engine sends to an administrator alone, as
  // the last rows until the card has a "Technical details" disclosure of its
  // own (uilet's ProposalCard `details`).
  const technical = proposal.technical;
  if (technical) {
    rows.push({
      kind: "fact",
      label: "Request (technical details)",
      value: <code>{`${technical.method} ${technical.path}`}</code>,
    });
    if (technical.body) {
      const body = drawValue(technical.body);
      rows.push({
        kind: "fact",
        label: "Request body (technical details)",
        value: body.node,
        block: body.block,
      });
    }
  }
  const audience = proposal.audience ?? [];
  if (audience.length === 0) return rows;
  const facts = rows.findIndex((row) => row.kind !== "fact");
  const at = facts < 0 ? rows.length : facts;
  return [
    ...rows.slice(0, at),
    {
      kind: "fact",
      label: "Who will see it",
      value: <Phrases phrases={audience} sep=", " open={open} />,
    },
    ...rows.slice(at),
  ];
}

/** What the person does with a proposal: the drawer's session answers each. */
export interface ProposalHandlers {
  approve: (proposal: AssistantProposalCard) => void;
  decline: (proposal: AssistantProposalCard) => void;
  /**
   * Ask for an expired or stale change again: a question in the same
   * conversation, so a fresh proposal comes through the normal flow against
   * what is there now, never a replay of the old request.
   */
  prepareAgain: (proposal: AssistantProposalCard) => void;
}

/** The question "Prepare it again" asks, naming the change it was. */
export function prepareAgainQuestion(proposal: AssistantProposalCard): string {
  return `Please prepare that change again (${proposal.title}), from how things stand now.`;
}

/** What the person does with a card that leads somewhere. */
export interface CardHandlers extends ProposalHandlers {
  /**
   * Go to an address the engine built, already checked to be this dashboard's
   * own; `cardId` is the open in app card it was pressed on.
   */
  openHere: (target: InAppTarget, cardId?: string) => void;
  /** Ask a question in this conversation: a brief's follow-up, or its ask action. */
  ask: (question: string) => void;
  /** Undo a page change from its receipt. */
  undo: (actionId: string) => void;
}

/** A page action's outcome as its receipt's tone. */
export const ACTION_TONE: Record<ActionLocalState, ReceiptStripTone> = {
  applied: "success",
  undone: "neutral",
  elsewhere: "info",
  stale: "warning",
  blocked: "danger",
  invalid: "danger",
  no_handler: "danger",
  unconfirmed: "danger",
};

/**
 * What a page change the person approved nothing for (a pure view change,
 * D11) came to: a receipt in the thread, with Undo while the page still shows
 * what it made of it. A change that came with a card is drawn on the card.
 */
export function ActionReceipt({
  actionId,
  local,
  on,
}: {
  actionId: string;
  local: ActionLocal;
  on: CardHandlers;
}) {
  return (
    <ReceiptStrip
      tone={ACTION_TONE[local.state]}
      actions={
        local.undoable ? (
          <Button size="small" variant="ghost" onClick={() => on.undo(actionId)}>
            Undo
          </Button>
        ) : undefined
      }
    >
      {changedNothing(local.state) && local.state !== "undone"
        ? `${local.words} Nothing changed.`
        : local.words}
    </ReceiptStrip>
  );
}

/** A screen's preview row in `ChangeList`'s grammar: what it sets, from what. */
function previewRow(row: PageActionPreview["rows"][number]): ChangeListRow {
  // A PROPOSED FILE IS DRAWN, by its name, its type and a thumbnail.
  const next = row.file
    ? { node: <DraftFile file={row.file} ground={row.fileGround} />, block: false }
    : drawValue(row.next, row.items);
  if (row.current === null || (row.file && row.current === "(empty)")) {
    return { kind: "added", label: row.field, to: next.node, isNew: false, block: next.block };
  }
  if (!row.file && row.current === row.next) {
    return { kind: "fact", label: row.field, value: next.node, block: next.block };
  }
  const current = drawValue(row.current, row.currentItems);
  return {
    kind: "changed",
    label: row.field,
    from: current.node,
    to: next.node,
    block: current.block || next.block,
  };
}

/**
 * The screen's own account of a page proposal still waiting on the person,
 * or null where the card keeps the engine's rows: an action no screen here
 * declares, a payload its parser refuses, or a card already decided.
 */
export function screenPreviewOf(proposal: AssistantProposalCard): PageActionPreview | null {
  if (proposal.scope !== "page" || proposal.state !== "proposed" || !proposal.page_action) {
    return null;
  }
  return pageActionPreview(proposal.page_action.name, proposal.page_action.payload);
}

/** A proposal card, at its state now. */
function Proposal({
  proposal,
  local,
  on,
}: {
  proposal: AssistantProposalCard;
  local: ProposalLocal | undefined;
  on: CardHandlers;
}) {
  const open = on.openHere;
  // THE PAGE UNDER THE DRAWER IS READ AGAIN AS IT MOVES, so the card's rows
  // and its gate are the screen's as it is now.
  usePageSnapshot();
  const screen = screenPreviewOf(proposal);
  const unknown = proposal.state === "unknown";
  const stale = local?.pageMoved === true && proposal.state === "proposed";
  const state: ProposalState = stale ? "stale" : PROPOSAL_STATE[proposal.state];
  // A FAILED CARD SAYS WHY in the page's words where the page refused it
  // here first, else the engine's; one nobody could confirm says so in the
  // card's own words (failureOutcome) and never "Nothing changed.".
  const failure = local?.failure ?? proposal.failure?.refusal.message;
  const refusal = local?.refusal;
  return (
    <ProposalCard
      title={proposal.title}
      subtitle={scopeLine(proposal.scope, proposal.own)}
      scope={proposal.scope}
      state={state}
      // A CHANGE PREPARED AFTER OUTSIDE TEXT SAYS SO FIRST (D25), naming the
      // surface the turn read it in; the person weighs whether that text
      // asked for it.
      caution={proposal.taint ? cautionOf(proposal.taint) : undefined}
      // WHAT IT DOES LEADS THE CARD, above every row, in the engine's words:
      // what a destructive change takes away (ADR-0055), never only a
      // struck-through row among the others.
      summary={proposal.summary ? <Phrase phrase={proposal.summary} open={open} /> : undefined}
      changes={screen ? screen.rows.map(previewRow) : changeRowsOf(proposal, open)}
      note={proposal.note ? <Phrase phrase={proposal.note} open={open} /> : undefined}
      // THE SCREEN'S GATE HOLDS APPROVE, with its sentence, before the person
      // presses anything; the page checks it again when it applies the change.
      approveBlocked={screen?.blocked ?? undefined}
      approveLabel={proposal.approve_label}
      // WHAT APPROVING INVOLVES, said before the press: the engine's own
      // step-up flag (the identity check the approval will ask for) and the
      // proposal's expiry, after which the engine refuses the approval.
      stepUp={proposal.step_up === true}
      expiresAt={proposal.expires_at}
      onApprove={() => on.approve(proposal)}
      onDecline={() => on.decline(proposal)}
      onPrepareAgain={
        state === "expired" || state === "stale" ? () => on.prepareAgain(proposal) : undefined
      }
      onRetry={unknown || proposal.failure?.retryable ? () => on.approve(proposal) : undefined}
      receipt={
        proposal.receipt ? <Phrase phrase={proposal.receipt.summary} open={open} /> : undefined
      }
      failure={failure}
      failureOutcome={unknown ? "unknown" : undefined}
      // THE ROUTE'S NEXT STEP, on its own line in the failure strip, where the
      // engine's own refusal is shown (never beside the drawer's own words).
      failureHint={local?.failure ? undefined : proposal.failure?.refusal.hint}
      // A DECISION THE ENGINE REFUSED, or nobody answered, while the card
      // still waits: said above the decision, until the next press clears it.
      decisionRefusal={
        refusal ? { sentence: refusal.sentence, hint: refusal.hint ?? undefined } : undefined
      }
    />
  );
}

/**
 * Where a change lands, as the card's subtitle. Only where: the card's tag
 * says its state ("Needs your approval", "Saved"), so the line does not.
 */
export function scopeLine(scope: AssistantProposalCard["scope"], own = false): string {
  if (scope === "page") return "This page only";
  return own ? "Only for you" : "For the company";
}

/** A crumb that is a record's raw id (a UUID), which a person does not read. */
const RAW_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * Where an address goes, as a trail of the dashboard's own crumbs, without a
 * raw id: "Live › Turns", never "Live › Turns › 01a175e0-6ced-...". The card's
 * title names the record.
 */
export function placeOf(target: InAppTarget): string {
  return crumbsFor(target.path)
    .map((c) => c.label)
    .filter((label) => !RAW_ID.test(label.trim()))
    .join(" › ");
}

/**
 * What a refusal card says, and whether a permission is the way past it.
 *
 * THE RULE'S REASON DECIDES, through the dashboard's remedy table
 * ([remedyFor]), never the grants alone: a stale proof names the grants that
 * admitted the person, and an artifact names the admin override beside its
 * owner's share. Deciding by the code and the grants, the card said "You do
 * not have permission", offered a request to an admin and said the assistant
 * acts with the person's access for every refusal on authority, a proof to
 * confirm and a verb the assistant never takes among them, and told an admin
 * whose proof had gone stale that they needed Admin.
 *
 * WHERE ACCESS IS NOT THE WAY PAST, THE ENGINE'S OWN SENTENCE (`message`),
 * which follows the rule. Not `REFUSAL_SENTENCES`, which word a screen's own
 * requests (an `unnamed` there is a bug in the screen): this request was the
 * assistant's.
 */
function refusalWords(refusal: AssistantErrorCard["refusal"]): { access: boolean; why: string } {
  const grants = refusal.grants ?? [];
  const access = remedyFor(refusal.reason ?? "", grants).namesAccess;
  return { access, why: access ? youNeedWords(grants) : refusal.message };
}

/**
 * A brief row's action as its one press: an open the engine built, followed
 * only where it is one of this dashboard's own addresses, or a question asked
 * in the conversation. Anything else draws no button.
 */
function briefPress(
  action: AssistantBriefAction,
  on: CardHandlers,
): { label: string; onSelect: () => void } | undefined {
  if (action.kind === "ask") {
    const question = action.question;
    return question ? { label: action.label, onSelect: () => on.ask(question) } : undefined;
  }
  const target = action.link ? openInApp(action.link.href) : null;
  return target ? { label: action.label, onSelect: () => on.openHere(target) } : undefined;
}

/**
 * A room brief's point's press: the room, opened at the message it came from,
 * which the room draws flashed (a reply in its thread). The address is the
 * room screen's own, built from the source the engine read back, never from
 * the model's words.
 */
function pointPress(
  point: AssistantBriefPoint,
  on: CardHandlers,
): { label: string; onSelect: () => void } | undefined {
  const { channel_id, message_id, thread_root } = point.source;
  const target = openInApp(hrefOf(["chatlet", channel_id], { message: message_id }));
  if (!target) return undefined;
  return {
    label: thread_root ? "Open the thread" : "Open the message",
    onSelect: () => on.openHere(target),
  };
}

/**
 * A brief, drawn by the design system's `BriefCard`: what it covers (the page,
 * or the one record it was limited to) and when it was read, what the page is
 * about, the ranked rows with their presses, the context line, and the
 * follow-ups, each asked in this conversation.
 *
 * THE VERDICT IS THE CARD'S, FROM THE ROWS: whether anything needs the reader
 * is the number of rows the engine kept (ADR-0062), which `BriefCard` counts
 * into its head, so nothing here passes a verdict of its own.
 */
function Brief({ brief, on }: { brief: AssistantBriefCard; on: CardHandlers }) {
  const record = brief.scope.record;
  const word = record ? recordWord(record) : null;
  const scope =
    record && word
      ? `${word.charAt(0).toUpperCase()}${word.slice(1)} ${record.id}`
      : brief.scope.title;
  return (
    <BriefCard
      scope={scope}
      asOf={`As of ${fmtTime(brief.as_of)}`}
      about={brief.about}
      needs={brief.needs.map((need) => ({
        key: `${need.ref.kind}:${need.ref.id}`,
        text: need.text,
        ...(need.detail ? { detail: need.detail } : {}),
        tone: need.tone,
        action: briefPress(need.action, on),
      }))}
      points={(brief.points ?? []).map((point) => ({
        key: point.source.message_id,
        text: point.text,
        source: pointPress(point, on),
      }))}
      context={brief.context}
      followUps={brief.follow_ups.map((question) => ({
        key: question,
        question,
        onSelect: () => on.ask(question),
      }))}
    />
  );
}

/**
 * An open in app card, opened while its page is the one showing: "Opened" once
 * pressed, and said to be open already where the person was there before, so
 * Open here is never a press that changes nothing.
 */
function OpenTarget({
  title,
  target,
  opened,
  onOpenHere,
}: {
  title: string;
  target: InAppTarget;
  opened: boolean;
  onOpenHere: () => void;
}) {
  // A PAGE THAT TAKES ITS ARRIVAL OFF THE ADDRESS (a form's `open=`) is still
  // the one opened, so an opened card asks for its path alone.
  const showing = useIsShowing(target.path, opened ? {} : target.query);
  return (
    <OpenInAppCard
      icon={<LayoutDashboardGlyph size="md" />}
      title={title}
      path={placeOf(target)}
      href={hrefOf(target.path, target.query)}
      onOpenHere={onOpenHere}
      state={showing ? "opened" : "ready"}
      labels={showing && !opened ? ALREADY_OPEN : undefined}
    />
  );
}

/** The card's words for a page that was showing before anything was pressed. */
const ALREADY_OPEN = { opened: "Open", openedNotice: "Already open beside this conversation." };

/** One card of an answer, drawn as the design system's component for its kind. */
export function CardBlock({
  event,
  local,
  on,
  opened,
}: {
  event: AssistantCardEvent;
  local: ProposalLocal | undefined;
  on: CardHandlers;
  /** Whether this card's place was opened beside the conversation. */
  opened: boolean;
}): ReactNode {
  const card = narrowCard(event.card);
  if (card === null) {
    return (
      <Callout variant="neutral">
        This answer carries a card this version of the dashboard cannot draw. Reload to see it.
      </Callout>
    );
  }
  switch (card.type) {
    case "table":
    case "entity":
    case "oversize":
      // A READ IS NEVER A CARD: its answer draws it, as a chip where the
      // answer names it and on the Also looked up line where it does not
      // (`reads.tsx`). Reaching here is a caller drawing a read as a card.
      throw new Error(`a ${card.type} read is drawn by its answer, never as a card`);
    case "proposal":
      return <Proposal proposal={card.proposal} local={local} on={on} />;
    case "open_in_app": {
      const { link, permitted } = card.open_in_app;
      const target = permitted ? openInApp(link.href) : null;
      if (!target) {
        return (
          <RefusalCard title={link.label} heading="You cannot open this from here.">
            {permitted
              ? "The address is not one of this dashboard's pages."
              : "Your access does not include it."}
          </RefusalCard>
        );
      }
      return (
        <OpenTarget
          title={link.label}
          target={target}
          opened={opened}
          onOpenHere={() => on.openHere(target, event.card_id)}
        />
      );
    }
    case "error": {
      const { refusal, phrase } = card.error;
      // THE CARD'S OWN NEXT STEP, or else the refusal's: the engine's words,
      // never the route's `hint`, which only the model reads.
      const hint = card.error.hint ?? refusal.hint;
      const asked = phrase ? `${phrase.charAt(0).toUpperCase()}${phrase.slice(1)}` : undefined;
      // A REFUSAL THAT REFUSED NO ACCESS (a value not accepted, a record not
      // found) is a warning, never a refusal card.
      if (!isAccessRefusal(refusal)) {
        return (
          <Callout variant="warning" title={asked ?? "This could not be done"}>
            {refusal.message}
            {hint ? ` ${hint}` : ""}
          </Callout>
        );
      }
      const { access, why } = refusalWords(refusal);
      return (
        <RefusalCard
          // THE HEAD NAMES WHAT WAS ASKED FOR ("Read the fleet"), and the
          // card's own tag says it was not permitted; the sentence says why.
          title={asked ?? "Not permitted"}
          heading={access && phrase ? `You do not have permission to ${phrase}.` : undefined}
          actions={
            // A REQUEST FOR ACCESS ONLY WHERE ACCESS CLEARS IT: anywhere else
            // an admin could grant nothing that changes the answer.
            access ? (
              <CopyButton
                size="small"
                text={`Please give me ${accessNames(refusal.grants ?? [])} access${phrase ? ` to ${phrase}` : ""}.`}
                label="Copy a request to an admin"
              />
            ) : undefined
          }
        >
          {why}
          {hint ? ` ${hint}` : ""}
          {/* A REFUSED READ IS NOT A CHANGE: the sentence fits a read and a write. */}
          {access ? " Ask Crewlet acts with your access only, so it cannot do this for you." : ""}
        </RefusalCard>
      );
    }
    case "notice":
      return (
        <Callout variant={card.notice.tone === "warning" ? "warning" : "info"}>
          {card.notice.text}
        </Callout>
      );
    case "brief":
      return <Brief brief={card.brief} on={on} />;
  }
}

/** A Try again for a turn that failed for a reason the engine says may pass. */
export function RetryButton({ onRetry }: { onRetry: () => void }) {
  return (
    <Button size="small" variant="secondary" onClick={onRetry}>
      Try again
    </Button>
  );
}
