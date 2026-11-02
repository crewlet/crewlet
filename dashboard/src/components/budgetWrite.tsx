/**
 * Raising a token ceiling where the reader is looking at it.
 *
 * TWO SHAPES OF ONE WRITE (`lib/useCeilingWrite.ts`):
 *
 *  - [CeilingEditor] — ONE window's ceiling, edited in place on the Budgets
 *    screen: the ceiling is the figure beside the bar, and the pencil beside
 *    it turns that figure into a field;
 *  - [RaiseBudgetDialog] — ONE SCOPE's three windows at once, opened from
 *    where a stopped seat is reported (Home, the Inbox) with the window it is
 *    stopped in focused, because the person answering "raise it" should not
 *    have to find the seat on another screen first.
 *
 * NEVER HIDDEN. Both are drawn for every reader and disabled with the reason
 * for one who cannot change that ceiling (`useOrgWriteAccess`), which is a
 * different gate from acting in it: a ceiling is a `/config` change, written
 * by an admin — or, for a seat inside a unit the reader leads, by that lead,
 * since a ceiling is no credential and the engine admits a lead's write of
 * their own seats (`lib/leadScope.ts`). The company's own ceilings are
 * Admin's alone.
 *
 * THERE IS NO RESET, and nothing here offers one. A window's `used` IS what
 * the window spent; the room comes back when it turns over on the company
 * clock, or now by raising the ceiling — which is what these write.
 */

import {
  createContext,
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
  type FormEvent,
} from "react";
import {
  Button,
  Callout,
  FormField,
  IconButton,
  Input,
  Modal,
  Popover,
  returnFocus,
  Text,
  useToast,
} from "@crewlethq/ui";
import { PencilGlyph, SlidersVerticalGlyph } from "@crewlethq/icons/glyphs";
import { BUDGET_WINDOWS } from "~/contract/config.ts";
import { PERIOD_ADJECTIVE, PERIOD_WORDS, readCeiling, waitedOn, windowOf } from "~/lib/budget.ts";
import {
  ceilingText,
  scopeName,
  whoseCeiling,
  type CeilingScope,
  type Period,
} from "~/lib/ceilings.ts";
import { companyDateLabel, fmtCount, fmtDateTime, fmtExact } from "~/lib/format.ts";
import { dayLabelIn } from "~/lib/range.ts";
import { useOrgBudget } from "~/lib/store-hooks.ts";
import { useCeilingWrite, type CeilingWrite, type TypedCeilings } from "~/lib/useCeilingWrite.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrgWriteAccess, type OrgTarget } from "~/lib/useWriteAccess.ts";
import { useWaiting } from "~/lib/waiting.ts";
import type { BudgetWindow } from "~/protocol/types.ts";
import { refuse, useAssistantDraft } from "~/app/assistant/draft.ts";
import { remedyOffer, scopeRemedy } from "~/lib/remedy.ts";

/**
 * A ceiling the assistant prepared, for the editor of its scope and window to
 * OPEN WITH: the field filled in, nothing saved. Each request carries a fresh
 * `nonce`, so the editor opens once per request however often it renders.
 */
export interface CeilingPrefill {
  /** `company`, or a seat's handle. */
  readonly scope: string;
  readonly period: Period;
  /** The ceiling as the field takes it: a number of tokens, `40M`, or empty for none. */
  readonly text: string;
  readonly nonce: number;
}

/** The screen's last prepared ceiling, read by every [CeilingEditor] under it. */
export const CeilingPrefillContext = createContext<CeilingPrefill | null>(null);

/** What changing a scope's ceilings is a change to: one seat, or the company. */
function targetOf(scope: CeilingScope): OrgTarget {
  return scope.kind === "seat" ? { seat: scope.handle } : "company";
}

/** "Daily", "Weekly", "Monthly" — the label a ceiling's field wears. */
function periodTitle(period: Period): string {
  const word = PERIOD_ADJECTIVE[period];
  return word.charAt(0).toUpperCase() + word.slice(1);
}

/** How a ceiling is typed, said under the field that takes one. */
const CEILING_TYPING = "Type 40M or 2.5M; empty means no limit.";

/**
 * When a window's allowance comes back, as the company's calendar says it:
 * the date for a week or a month ("Oct 1"), the time for a day, which always
 * turns over at the company's midnight.
 */
export function resetsWords(w: BudgetWindow, zone: string): string {
  const at = Date.parse(w.resets_at);
  if (!Number.isFinite(at)) return "";
  if (w.period === "day") return `resets at midnight (${zone || "UTC"})`;
  return `resets ${companyDateLabel(dayLabelIn(at, zone))}`;
}

// ---------------------------------------------------------------------------
// What a write came to
// ---------------------------------------------------------------------------

/**
 * The line under a ceiling being changed: checking, what the change would
 * introduce, why it was refused — each with the one control that answers it.
 *
 * A LIVE REGION, because each of these arrives after the press with nothing
 * else on screen moving: a reader who pressed Save and heard nothing would
 * press it again.
 */
function WriteStatus({ write, onReload }: { write: CeilingWrite; onReload: () => void }) {
  const { state } = write;
  return (
    <div className="ceiling-status" role="status" aria-live="polite">
      {state.kind === "checking" && <span className="t-caption">Checking with the engine…</span>}
      {state.kind === "saving" && <span className="t-caption">Saving…</span>}
      {state.kind === "unchanged" && <span className="t-caption">Already the limit.</span>}
      {state.kind === "warned" && (
        <Callout variant="warning">
          <ul className="ceiling-warnings">
            {state.warnings.map((w) => (
              <li key={`${w.path}:${w.message}`}>{w.message}</li>
            ))}
          </ul>
        </Callout>
      )}
      {state.kind === "refused" && (
        <Callout variant="danger">
          <span>{state.message}</span>
          {state.reload && (
            <div className="row gap-2">
              <Button size="small" variant="secondary" onClick={onReload}>
                Reload
              </Button>
            </div>
          )}
        </Callout>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// One window, in place
// ---------------------------------------------------------------------------

/**
 * One window's ceiling as a figure, and — for an admin — as a field.
 *
 * WHAT IT SHOWS AFTER A SAVE is the ceiling SAVED, marked as applying, until
 * this node has applied it and the answer the screen draws carries it. Going
 * back to the served figure at once drew the ceiling being replaced for a
 * beat after "Saved", which reads as a save that did not take.
 *
 * UNLESS THIS NODE REFUSED IT. Then the figure is the served one again — the
 * ceiling the gate goes on enforcing — marked as refused here, rather than
 * the saved ceiling marked as applying for as long as the page stayed open.
 * A save a later activation passed before this node applied it settles the
 * same way an apply does, on the answer re-read after it.
 */
export function CeilingEditor({
  scope,
  period,
  window: w,
  zone,
  onApplied,
}: {
  scope: CeilingScope;
  period: Period;
  /** The window as the budgets answer states it; `limit` absent is no ceiling. */
  window: BudgetWindow | undefined;
  /** The company clock, for when the window resets. */
  zone: string;
  /**
   * Re-read what the screen draws: once this node applies the saved ceiling,
   * or a later activation passes it (`useCeilingWrite`), and on the Reload a
   * conflict, or a request that may have landed, offers.
   */
  onApplied: () => void;
}) {
  const access = useOrgWriteAccess(targetOf(scope));
  const write = useCeilingWrite(scope, { onApplied });
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState("");
  const [error, setError] = useState<string | undefined>();
  // The value the last save wrote, so the figure says it while it applies.
  const [expected, setExpected] = useState<number | null | undefined>(undefined);
  const pencil = useRef<HTMLButtonElement>(null);
  const id = useId();

  // A CEILING THE ASSISTANT PREPARED FOR THIS EDITOR opens it with the value
  // in the field, as the pencil opens it with the current one: the warning,
  // the save and its step-up stay the person's.
  const prefill = useContext(CeilingPrefillContext);
  const scopeId = scope.kind === "seat" ? scope.handle : scope.kind;
  const prefilled = useRef(0);
  const { reset: resetWrite } = write;
  useEffect(() => {
    if (!prefill || prefill.nonce === prefilled.current) return;
    if (prefill.scope !== scopeId || prefill.period !== period) return;
    prefilled.current = prefill.nonce;
    resetWrite();
    setText(prefill.text);
    setError(undefined);
    setEditing(true);
  }, [prefill, scopeId, period, resetWrite]);

  const whose = whoseCeiling(scope);
  const adjective = PERIOD_ADJECTIVE[period];
  const limit = w?.limit;

  const saved = write.state.kind === "saved" ? write.state : null;
  // SETTLED once the answer re-read after the apply has arrived: the applying
  // mark goes, and the figure is the answer's own again. The served ceiling
  // matching the one saved settles it too; and an answer that arrived after
  // the apply and says otherwise (a colleague's newer ceiling) is the truth,
  // so it settles on that rather than drawing the saved value for ever. A
  // save a later activation passed was never this node's to apply: the
  // served figure is the truth at once, and the re-read brings it up to date.
  const seenAtApply = useRef<BudgetWindow | undefined | null>(null);
  const { reset } = write;
  const standing = saved?.standing;
  useEffect(() => {
    if (standing === "passed") {
      seenAtApply.current = null;
      reset();
      setExpected(undefined);
      return;
    }
    if (standing !== "applied") {
      seenAtApply.current = null;
      return;
    }
    if (seenAtApply.current === null) seenAtApply.current = w;
    if ((limit ?? null) === expected || w !== seenAtApply.current) {
      seenAtApply.current = null;
      reset();
      setExpected(undefined);
    }
  }, [standing, w, limit, expected, reset]);

  // A SAVE CLOSES THE FIELD: the figure beside the bar says the new ceiling
  // and that it is applying, which is the answer the person was waiting for.
  useEffect(() => {
    if (!editing || !saved) return;
    setEditing(false);
    returnFocus(pencil.current);
  }, [editing, saved]);

  function open() {
    write.reset();
    setText(ceilingText(limit, period));
    setError(undefined);
    setEditing(true);
  }

  function close() {
    write.reset();
    setEditing(false);
    // FOCUS GOES BACK TO WHAT OPENED IT, or a keyboard reader lands at the
    // top of the page after every edit. Handed back rather than focused, so a
    // pencil a pointer opened is not ringed for leaving the field.
    queueMicrotask(() => returnFocus(pencil.current));
  }

  function submit(event?: FormEvent) {
    event?.preventDefault();
    if (write.busy) return;
    // ENTER ON A WARNING IS THE SECOND PRESS the warning asked for, exactly
    // as the button under it is.
    if (write.state.kind === "warned") {
      write.confirm();
      return;
    }
    const read = readCeiling(period, text);
    if (!read.ok) {
      setError(read.error);
      return;
    }
    setError(undefined);
    setExpected(read.value);
    write.submit({ [period]: text } as TypedCeilings);
  }

  // THE SAVED CEILING while this node applies it, and the one it enforces
  // once it has refused it, or a later activation has passed it.
  const drawsSaved = saved && (saved.standing === "applying" || saved.standing === "applied");
  const shown = drawsSaved && expected !== undefined ? expected : (limit ?? null);
  const who = scopeName(scope);

  // WHAT IS OPEN, for the assistant, while the field is: the ceiling as
  // typed, set as typing sets it and refused in the field's own words; the
  // save, its warning and its step-up stay the person's.
  useAssistantDraft<string>(
    editing
      ? {
          kind: "client",
          id: scopeId,
          step: period,
          scope: `${who}, ${adjective} token ceiling (not saved)`,
          help:
            "One window's token ceiling is open for editing. You can propose a value; " +
            "saving it is the person's, and the engine may ask them to confirm it.",
          value: text,
          fields: [
            {
              path: "ceiling",
              label: `${who} · ${periodTitle(period)} token ceiling`,
              hint: CEILING_TYPING,
              read: (t) => t,
              write: (_, next) => {
                if (typeof next !== "string") return refuse("A ceiling is one value.");
                const read = readCeiling(period, next);
                return read.ok ? next : refuse(read.error);
              },
            },
          ],
          apply: (next) => {
            setText(next);
            setError(undefined);
            if (write.state.kind !== "idle" && !write.busy) write.reset();
          },
          blocked: () => (write.busy ? "The ceiling is being saved." : null),
        }
      : null,
  );
  const fieldId = `${id}-ceiling`;
  return (
    <span className="ceiling">
      <span className={shown === null ? "ceiling-value muted" : "ceiling-value"}>
        {shown === null ? "No ceiling" : `of ${fmtCount(shown)}`}
      </span>
      {saved && saved.standing !== "passed" && (
        <span className="ceiling-applying t-caption" role="status">
          {saved.standing === "applied"
            ? "applied"
            : saved.standing === "refused"
              ? "refused by this node"
              : "applying…"}
        </span>
      )}
      {/* A PANEL ANCHORED TO THE PENCIL, not a field in the cell: a check's
          warning is a paragraph of the engine's own words, and a table column
          is a quarter of a page wide — the panel is where it can be read. */}
      <Popover
        label={`${who} · ${adjective} token ceiling`}
        align="start"
        className="ceiling-pop"
        open={editing}
        onOpenChange={(next) => (next ? open() : close())}
        trigger={(isOpen, toggle) => (
          <IconButton
            ref={pencil}
            size="sm"
            variant="ghost"
            icon={<PencilGlyph />}
            label={`Change ${whose} ${adjective} token ceiling`}
            aria-expanded={isOpen}
            disabledReason={access.can ? undefined : access.reason}
            title={access.can ? undefined : access.reason}
            onClick={toggle}
          />
        )}
      >
        <form className="ceiling-edit" onSubmit={submit} noValidate>
          <FormField
            htmlFor={fieldId}
            label={`${who} · ${periodTitle(period)} token ceiling`}
            optional
            error={error}
            helper={
              w
                ? `${fmtCount(w.used)} spent ${PERIOD_WORDS[period]}. ${CEILING_TYPING}`
                : CEILING_TYPING
            }
          >
            <Input
              id={fieldId}
              value={text}
              autoFocus
              placeholder="No ceiling"
              autoComplete="off"
              spellCheck={false}
              error={Boolean(error)}
              disabled={write.busy}
              onChange={(event) => {
                setText(event.target.value);
                setError(undefined);
                if (write.state.kind !== "idle" && !write.busy) write.reset();
              }}
            />
          </FormField>
          <WriteStatus
            write={write}
            onReload={() => {
              onApplied();
              write.reset();
              setText(ceilingText(limit, period));
            }}
          />
          <div className="ceiling-actions">
            <Button type="button" size="small" variant="ghost" onClick={close}>
              Cancel
            </Button>
            <Button
              type={write.state.kind === "warned" ? "button" : "submit"}
              size="small"
              variant="primary"
              loading={write.busy}
              onClick={write.state.kind === "warned" ? write.confirm : undefined}
            >
              {write.state.kind === "warned" ? "Save anyway" : "Save"}
            </Button>
          </div>
        </form>
      </Popover>
    </span>
  );
}

// ---------------------------------------------------------------------------
// One scope, in a dialog
// ---------------------------------------------------------------------------

/**
 * A scope's three ceilings in one dialog, the stopped window focused.
 *
 * Each field says what the window has spent and when it turns over, because
 * that is what a raise is decided against: a day refusing at 11pm needs no
 * raise, and one refusing at 9am does.
 */
export function RaiseBudgetDialog({
  scope,
  focus,
  onClose,
}: {
  scope: CeilingScope;
  /** The window to put the cursor in: the one the scope is stopped in. */
  focus?: Period;
  onClose: () => void;
}) {
  const toast = useToast();
  const waiting = useWaiting();
  const access = useOrgWriteAccess(targetOf(scope));
  const budgets = useQuery("budgets", undefined);
  const write = useCeilingWrite(scope);
  const zone = budgets.data?.timezone ?? "";
  const windows =
    scope.kind === "company"
      ? budgets.data?.org?.windows
      : scope.kind === "assistant"
        ? budgets.data?.assistant?.windows
        : budgets.data?.seats?.find((s) => s.handle === scope.handle)?.windows;

  // PREFILLED ONCE, from the first answer: a poll landing while somebody types
  // would otherwise put the old ceiling back under their cursor.
  const [typed, setTyped] = useState<Partial<Record<Period, string>> | null>(null);
  if (typed === null && windows) {
    setTyped(
      Object.fromEntries(
        BUDGET_WINDOWS.map(({ period }) => [
          period,
          ceilingText(windowOf(windows, period)?.limit, period),
        ]),
      ),
    );
  }
  const values = typed ?? {};
  // THE CURSOR GOES TO THE STOPPED WINDOW once there is something in it: the
  // fields are disabled until the budgets answer prefills them, and a field
  // cannot take focus while it is disabled, so `autoFocus` alone left the
  // cursor on the dialog's close control.
  const focused = useRef<HTMLInputElement>(null);
  const ready = typed !== null;
  useEffect(() => {
    if (ready) focused.current?.focus();
  }, [ready]);
  const errors = Object.fromEntries(
    BUDGET_WINDOWS.map(({ period }) => {
      const read = readCeiling(period, values[period] ?? "");
      return [period, read.ok ? undefined : read.error];
    }),
  ) as Partial<Record<Period, string>>;
  const invalid = Object.values(errors).some(Boolean);
  const changed = BUDGET_WINDOWS.some(({ period }) => {
    const read = readCeiling(period, values[period] ?? "");
    return read.ok && read.value !== (windowOf(windows, period)?.limit ?? null);
  });

  // A SAVE ENDS THE DIALOG, and the toast carries the revision's own summary:
  // the sentence the history will show.
  const savedSummary = write.state.kind === "saved" ? write.state.summary : null;
  const announced = useRef(false);
  useEffect(() => {
    // ONCE: the parent's `onClose` is a new function on every render, and a
    // render between this save and the unmount would say "Saved" twice.
    if (savedSummary === null || announced.current) return;
    announced.current = true;
    toast.show({
      variant: "success",
      title: savedSummary,
      message: "Saved.",
    });
    onClose();
  }, [savedSummary, toast, onClose]);

  const blocked = !access.can
    ? access.reason
    : !windows
      ? "Waiting for the budgets to load."
      : invalid
        ? "Correct the ceiling first."
        : !changed
          ? "Change a ceiling first."
          : write.busy
            ? waiting.reason
            : undefined;

  const warned = write.state.kind === "warned";
  const save = () => {
    if (warned) write.confirm();
    else if (!blocked) write.submit(values);
  };
  const title = `Raise ${whoseCeiling(scope)} budget`;

  return (
    <Modal
      open
      size="sm"
      title={title}
      icon={<SlidersVerticalGlyph />}
      onClose={onClose}
      onSubmit={save}
      dismissable={!write.busy}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <Button
            variant="primary"
            onClick={save}
            loading={write.busy}
            disabledReason={warned ? undefined : blocked}
            title={warned ? undefined : blocked}
          >
            {warned ? "Save anyway" : "Save"}
          </Button>
        </>
      }
    >
      <div className="col gap-3">
        <p className="t-caption">Leave a field empty for no limit.</p>
        {BUDGET_WINDOWS.map(({ period }) => {
          const w = windowOf(windows, period);
          const fieldId = `raise-${period}`;
          return (
            <FormField
              key={period}
              htmlFor={fieldId}
              label={`${periodTitle(period)} ceiling`}
              optional
              error={errors[period]}
              helper={
                w
                  ? `${fmtExact(w.used)} tokens spent ${PERIOD_WORDS[period]} · ${resetsWords(w, zone)}${
                      // A STAMP ONLY ON A WINDOW STILL REFUSING. Under a
                      // ceiling raised since, the stamp stays until the next
                      // admitted charge clears it, and "last refused" would
                      // contradict the window's own state.
                      w.state === "refusing" && w.refused_at
                        ? ` · last refused ${fmtDateTime(w.refused_at)}`
                        : ""
                    }`
                  : undefined
              }
            >
              <Input
                id={fieldId}
                value={values[period] ?? ""}
                placeholder="No ceiling"
                autoComplete="off"
                spellCheck={false}
                ref={period === (focus ?? "month") ? focused : undefined}
                disabled={!typed || write.busy}
                error={Boolean(errors[period])}
                onChange={(event) => {
                  setTyped({ ...values, [period]: event.target.value });
                  if (!write.busy) write.reset();
                }}
              />
            </FormField>
          );
        })}
        <WriteStatus
          write={write}
          onReload={() => {
            budgets.refetch();
            setTyped(null);
            write.reset();
          }}
        />
      </div>
    </Modal>
  );
}

/**
 * "Raise budget" for a seat the engine stopped, opening the dialog on the
 * scope that is actually refusing it.
 *
 * WHOSE CEILING STOPPED IT decides what the button raises. A seat is refused
 * by its own ceiling or by the company's, and raising the seat's while the
 * company's is the one refusing changes nothing the gate reads — so the seat's
 * own refusing window wins, then the company's, and a seat whose windows name
 * neither (a report that has not caught up) opens on its own.
 */
export function RaiseBudgetButton({
  handle,
  name,
  window: own,
}: {
  handle: string;
  name: string;
  /** The seat's own window the gate is refusing in, where it names one. */
  window: BudgetWindow | undefined;
}) {
  const org = useOrgBudget();
  const [open, setOpen] = useState(false);
  const company = own ? undefined : waitedOn(org?.org.windows, "refusing");
  const scope: CeilingScope = company ? { kind: "company" } : { kind: "seat", handle, name };
  const access = useOrgWriteAccess(targetOf(scope));
  const focus = (own ?? company)?.period;
  // WHO MAY RAISE IT, by the rule every remedy is offered by: a reader the
  // gate refuses for who they are is told who can, never handed a button that
  // is locked for them. A reader held for a moment (offline, a hold) keeps
  // the button, disabled with the reason, since the moment passes.
  const offer = remedyOffer(scopeRemedy(scope), access);
  if (offer.kind === "pending") return null;
  if (offer.kind === "who") {
    return (
      <Text variant="caption" tone="secondary">
        {offer.sentence}
      </Text>
    );
  }
  return (
    <>
      <Button
        size="small"
        variant="secondary"
        disabledReason={access.can ? undefined : access.reason}
        title={access.can ? undefined : access.reason}
        onClick={() => setOpen(true)}
      >
        Raise budget
      </Button>
      {open && <RaiseBudgetDialog scope={scope} focus={focus} onClose={() => setOpen(false)} />}
    </>
  );
}
