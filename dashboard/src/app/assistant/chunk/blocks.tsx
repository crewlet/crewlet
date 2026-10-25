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
 * EVERY CARD IS THE ENGINE'S: built from tool results and its own route
 * table, never from the model's words. Only prose is the model's, and it is
 * drawn inert (`ASSISTANT_WORDS` in `lib/markdown.ts`): no live link and no
 * picture. A card this build cannot draw is a sentence saying so.
 */

import { type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  CopyButton,
  EntityList,
  OpenInAppCard,
  PropertyList,
  ProposalCard,
  Prose,
  ReceiptStrip,
  RefusalCard,
  ResultTableCard,
  StatusCard,
  type ChangeListRow,
  type ProposalState,
  type ReceiptStripTone,
  type RunState,
  type StepListStep,
  type StepState,
} from "@crewlethq/ui";
import { ArrowUpRightGlyph, LayoutDashboardGlyph } from "@crewlethq/icons/glyphs";
import type {
  AssistantCardEvent,
  AssistantProposalCard,
  AssistantStepEvent,
  AssistantTableCard,
} from "~/contract/assistant.ts";
import { narrowCard } from "~/protocol/assistantWire.ts";
import { exportFile } from "~/protocol/assistant.ts";
import { statusLabel } from "~/lib/work.ts";
import { ASSISTANT_WORDS, renderMarkdown } from "~/lib/markdown.ts";
import { fmtCount, fmtDateTime, fmtDuration, fmtExact } from "~/lib/format.ts";
import { youNeedWords } from "~/lib/access.ts";
import { crumbsFor } from "../../crumbs.ts";
import { href as hrefOf } from "../../router.tsx";
import { openInApp, type InAppTarget } from "./openInApp.ts";
import { pageActionPreview, usePageSnapshot, type PageActionPreview } from "../context.ts";
import { changedNothing, type ActionLocal, type ActionLocalState } from "./pageActions.ts";
import type { ProposalLocal, TurnPhase, TurnView } from "./turn.ts";

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

/** The types whose wire value is a number, which a fact carries as a string. */
const NUMERIC = new Set<WireColumnType>(["number", "tokens", "duration"]);

/** A fact's value as it is drawn, by its type. */
export function factText(fact: { value: string; type?: WireColumnType }): string {
  const type = fact.type ?? "text";
  const n = Number(fact.value);
  return CELL[type](NUMERIC.has(type) && fact.value !== "" && Number.isFinite(n) ? n : fact.value);
}

/** A number of seconds, as the run chip says it. */
function seconds(ms: number): string {
  return ms < 10_000 ? `${(ms / 1000).toFixed(ms < 1000 ? 1 : 0)}s` : `${Math.round(ms / 1000)}s`;
}

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
      return { state, title: "Thinking" };
    case "working":
      return { state, title: "Working", meta: seconds(elapsed) };
    case "applying":
      return { state, title: "Applying", meta: stepMeta(taken) };
    case "awaiting_approval":
      return { state, title: "Waiting for your approval", meta: stepMeta(taken) };
    case "completed":
      // A TURN READ BACK FROM ITS TRANSCRIPT carries no duration; the time
      // since it was asked is not how long it ran, so it says Done alone.
      return {
        state,
        title: turn.done ? `Done in ${seconds(turn.done.duration_ms)}` : "Done",
        meta: stepMeta(taken),
      };
    case "stopped":
    case "expired": {
      const done = turn.steps.filter((s) => s.state === "done").length;
      return {
        state,
        title: `Stopped after ${seconds(elapsed)}`,
        meta: taken > 0 ? `${done} of ${steps(taken)}` : undefined,
      };
    }
    case "refused":
      return { state, title: "Not permitted", meta: stepMeta(taken) };
    case "unconfirmed":
      return { state, title: "Could not confirm the change" };
    case "failed":
    case "interrupted":
    case "budget_exhausted": {
      const at = turn.steps.find((s) => s.state === "failed" || s.state === "refused");
      const refused = at?.state === "refused";
      if (refused) return { state: "refused", title: "Not permitted", meta: stepMeta(taken) };
      return { state, title: at ? `Failed at step ${at.index}` : "Failed" };
    }
  }
}

/** A turn's steps as `StepList` draws them: the label, the request it made in mono, its time. */
export function stepsOf(turn: TurnView): StepListStep[] {
  return turn.steps.map((step) => ({
    id: step.step_id,
    label: step.label,
    state: STEP_STATE[step.state],
    ...(step.duration_ms === undefined ? {} : { duration: seconds(step.duration_ms) }),
    detail: stepDetail(step),
  }));
}

/**
 * What a step's line says under its sentence: the request it made, and for a
 * step that failed or was refused, why, in the engine's own words, with what
 * to do about it where the route said.
 */
function stepDetail(step: TurnView["steps"][number]): string {
  const request = step.request
    ? `${step.tool} · ${step.request.method} ${step.request.route}`
    : step.tool;
  if (step.state !== "failed" && step.state !== "refused") return request;
  const why = step.refusal
    ? [step.refusal.message, step.refusal.hint].filter(Boolean).join(" ")
    : (step.summary ?? "");
  return why ? `${why} (${request})` : request;
}

/** A model's prose, drawn inert. `streaming` while deltas arrive; `dimmed` for an abandoned attempt. */
export function ReplyProse({
  text,
  streaming = false,
  dimmed = false,
}: {
  text: string;
  streaming?: boolean;
  dimmed?: boolean;
}) {
  return (
    <Prose measure="narrow" streaming={streaming} dimmed={dimmed} className="md">
      {renderMarkdown(text, ASSISTANT_WORDS)}
    </Prose>
  );
}

/**
 * A proposal's change rows in `ChangeList`'s grammar (D10): the facts first,
 * then WHO WILL SEE THE RESULT where its visibility changes (a share, a
 * comment others are told of, D09), as one more fact, then the changes.
 */
export function changeRowsOf(proposal: AssistantProposalCard): ChangeListRow[] {
  const rows = proposal.changes.map((change): ChangeListRow => {
    const note = change.note;
    const base = note === undefined ? { label: change.label } : { label: change.label, note };
    switch (CHANGE_KIND[change.kind]) {
      case "fact":
        return { ...base, kind: "fact", value: change.to ?? change.from ?? "" };
      case "changed":
        return { ...base, kind: "changed", from: change.from ?? "", to: change.to ?? "" };
      case "added":
        return { ...base, kind: "added", to: change.to ?? "", isNew: change.new === true };
      case "removed":
        return { ...base, kind: "removed", from: change.from ?? "" };
      case "text":
        return {
          ...base,
          kind: "text",
          words: (change.words ?? []).map((w) => ({ op: WORD_OP[w.op], text: w.text })),
        };
    }
  });
  const audience = proposal.audience ?? [];
  if (audience.length === 0) return rows;
  const facts = rows.findIndex((row) => row.kind !== "fact");
  const at = facts < 0 ? rows.length : facts;
  return [
    ...rows.slice(0, at),
    { kind: "fact", label: "Who will see it", value: audience.join(", ") },
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
  /** Go to an address the engine built, already checked to be this dashboard's own. */
  openHere: (target: InAppTarget) => void;
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
  if (row.current === null) return { kind: "added", label: row.field, to: row.next, isNew: false };
  if (row.current === row.next) return { kind: "fact", label: row.field, value: row.next };
  return { kind: "changed", label: row.field, from: row.current, to: row.next };
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
  on: ProposalHandlers;
}) {
  // THE PAGE UNDER THE DRAWER IS READ AGAIN AS IT MOVES, so the card's rows
  // and its gate are the screen's as it is now.
  usePageSnapshot();
  const screen = screenPreviewOf(proposal);
  const unknown = proposal.state === "unknown";
  const stale = local?.pageMoved === true && proposal.state === "proposed";
  const state: ProposalState = stale ? "stale" : PROPOSAL_STATE[proposal.state];
  const failure =
    local?.failure ??
    (proposal.failure
      ? proposal.failure.refusal.message
      : unknown
        ? "Could not confirm. It may have landed."
        : undefined);
  return (
    <ProposalCard
      title={proposal.title}
      subtitle={proposal.scope === "page" ? "This page only" : "Saved for the company"}
      scope={proposal.scope}
      state={state}
      changes={screen ? screen.rows.map(previewRow) : changeRowsOf(proposal)}
      note={proposal.note}
      // THE SCREEN'S GATE HOLDS APPROVE, with its sentence, before the person
      // presses anything; the page checks it again when it applies the change.
      approveBlocked={screen?.blocked ?? undefined}
      approveLabel={proposal.approve_label}
      onApprove={() => on.approve(proposal)}
      onDecline={() => on.decline(proposal)}
      onPrepareAgain={
        state === "expired" || state === "stale" ? () => on.prepareAgain(proposal) : undefined
      }
      onRetry={unknown || proposal.failure?.retryable ? () => on.approve(proposal) : undefined}
      receipt={proposal.receipt?.summary}
      failure={failure}
      // THE ROUTE'S NEXT STEP, on its own line in the failure strip, where the
      // engine's own refusal is shown (never beside the drawer's own words).
      failureHint={local?.failure ? undefined : proposal.failure?.refusal.hint}
    />
  );
}

/** A table card with its downloads from the stored snapshot. */
function Table({
  table,
  cardId,
  on,
}: {
  table: AssistantTableCard;
  cardId: string;
  on: CardHandlers;
}) {
  // THE DRAWER DRAWS THE COLUMNS THE ENGINE CHOSE FOR IT, name first; the
  // rest stay in the card's downloads.
  const drawn = table.columns.flatMap((c, i) => (c.detail ? [] : [i]));
  const align = drawn.map((i) => {
    const c = table.columns[i]!;
    return c.align ?? (c.type === "text" || c.type === "label" ? "start" : "end");
  });
  const cells = (row: (string | number | boolean | null)[]) =>
    drawn.map((i) => CELL[table.columns[i]!.type](row[i] ?? null));
  const base =
    table.title
      .replace(/[^a-z0-9]+/gi, "-")
      .replace(/^-|-$/g, "")
      .toLowerCase() || cardId;
  const downloads = table.exports
    ? [
        { label: "CSV", filename: `${base}.csv`, file: () => exportFile(table.exports!.csv) },
        { label: "XLSX", filename: `${base}.xlsx`, file: () => exportFile(table.exports!.xlsx) },
      ]
    : undefined;
  return (
    <ResultTableCard
      title={table.title}
      window={table.window}
      headers={drawn.map((i) => table.columns[i]!.label)}
      rows={table.rows.map(cells)}
      align={align}
      total={table.total ? cells(table.total) : undefined}
      downloads={downloads}
      link={table.open ? <EngineLink link={table.open} on={on} /> : undefined}
      emptyMessage="No rows match."
    />
  );
}

/** A link the engine built, followed only where it is one of this dashboard's own routes. */
function EngineLink({ link, on }: { link: { label: string; href: string }; on: CardHandlers }) {
  const target = openInApp(link.href);
  if (!target) return <span>{link.label}</span>;
  return (
    <ButtonLink
      size="small"
      variant="ghost"
      href={hrefOf(target.path, target.query)}
      trailingIcon={<ArrowUpRightGlyph size="sm" />}
      onClick={(event) => {
        if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey) return;
        event.preventDefault();
        on.openHere(target);
      }}
    >
      {link.label}
    </ButtonLink>
  );
}

/** Where an address goes, as a trail of the dashboard's own crumbs. */
function placeOf(target: InAppTarget): string {
  return crumbsFor(target.path)
    .map((c) => c.label)
    .join(" › ");
}

/** The words a refusal says about the person's own access. */
function refusalBody(grants: readonly string[] | undefined, message: string): string {
  return grants && grants.length > 0 ? youNeedWords(grants) : message;
}

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
      return <Table table={card.table} cardId={event.card_id} on={on} />;
    case "proposal":
      return <Proposal proposal={card.proposal} local={local} on={on} />;
    case "entity": {
      const entity = card.entity;
      if (entity.items && entity.items.length > 0) {
        return (
          <EntityList
            title={entity.title}
            items={entity.items.map((item) => ({
              id: `${item.kind}:${item.id}`,
              name: item.label,
              ...(item.fact === undefined ? {} : { fact: item.fact }),
              ...(item.open ? { action: <EngineLink link={item.open} on={on} /> } : {}),
            }))}
          />
        );
      }
      return (
        <StatusCard
          title={entity.title}
          subtitle={entity.subtitle}
          actions={entity.open ? <EngineLink link={entity.open} on={on} /> : undefined}
          actionsAlign="start"
        >
          {entity.facts && entity.facts.length > 0 ? (
            <PropertyList
              groups={[
                {
                  items: entity.facts
                    .filter((fact) => !fact.detail)
                    .map((fact) => ({
                      label: fact.label,
                      value: fact.mono ? <code>{factText(fact)}</code> : factText(fact),
                    })),
                },
              ]}
            />
          ) : null}
          {entity.text ? <ReplyProse text={entity.text} /> : null}
        </StatusCard>
      );
    }
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
        <OpenInAppCard
          icon={<LayoutDashboardGlyph size="md" />}
          title={link.label}
          path={placeOf(target)}
          href={hrefOf(target.path, target.query)}
          onOpenHere={() => on.openHere(target)}
          state={opened ? "opened" : "ready"}
        />
      );
    }
    case "error": {
      const { refusal, phrase } = card.error;
      // THE CARD'S OWN NEXT STEP, or else the route's (the envelope's `hint`).
      const hint = card.error.hint ?? refusal.hint;
      return (
        <RefusalCard
          // THE HEAD NAMES WHAT WAS ASKED FOR ("Read the fleet"), and the
          // card's own tag says it was not permitted; the sentence says why.
          title={phrase ? `${phrase.charAt(0).toUpperCase()}${phrase.slice(1)}` : "Not permitted"}
          heading={phrase ? `You do not have permission to ${phrase}.` : undefined}
          actions={
            <CopyButton
              size="small"
              text={`Please grant me the access I need${phrase ? ` to ${phrase}` : ""}. ${refusalBody(refusal.grants, refusal.message)}`}
              label="Copy a request to an admin"
            />
          }
        >
          {refusalBody(refusal.grants, refusal.message)}
          {/* A REFUSED READ IS NOT A CHANGE: the sentence fits a read and a write. */}
          {hint ? ` ${hint}` : ""} The assistant acts with your access only, so it cannot do this
          for you.
        </RefusalCard>
      );
    }
    case "notice":
      return (
        <Callout variant={card.notice.tone === "warning" ? "warning" : "info"}>
          {card.notice.text}
        </Callout>
      );
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
