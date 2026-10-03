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
 * for one who cannot change the company's configuration
 * (`useConfigWriteAccess`), which is a different gate from acting in it: the
 * company's ceiling is a `/config` change and a seat's is its org chart
 * runtime half, and both are the company's grant (`config:write`).
 *
 * THERE IS NO RESET, and nothing here offers one. A window's `used` IS what
 * the window spent; the room comes back when it turns over on the company
 * clock, or now by raising the ceiling — which is what these write.
 */

import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import {
  Button,
  Callout,
  FormField,
  IconButton,
  Input,
  Modal,
  Popover,
  useToast,
} from "@crewlethq/ui";
import { PencilGlyph, SlidersVerticalGlyph } from "@crewlethq/icons/glyphs";
import { BUDGET_WINDOWS } from "~/contract/config.ts";
import { PERIOD_ADJECTIVE, PERIOD_WORDS, readCeiling, waitedOn, windowOf } from "~/lib/budget.ts";
import { ceilingText, whoseCeiling, type CeilingScope, type Period } from "~/lib/ceilings.ts";
import { companyDateLabel, fmtCount, fmtDateTime, fmtExact } from "~/lib/format.ts";
import { dayLabelIn } from "~/lib/range.ts";
import { useOrgBudget } from "~/lib/store-hooks.ts";
import { useCeilingWrite, type CeilingWrite, type TypedCeilings } from "~/lib/useCeilingWrite.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import type { BudgetWindow } from "~/protocol/types.ts";

/** "Daily", "Weekly", "Monthly" — the label a ceiling's field wears. */
function periodTitle(period: Period): string {
  const word = PERIOD_ADJECTIVE[period];
  return word.charAt(0).toUpperCase() + word.slice(1);
}

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
      {state.kind === "unchanged" && (
        <span className="t-caption">That is already the ceiling — nothing to save.</span>
      )}
      {state.kind === "warned" && (
        <Callout variant="warning">
          <ul className="ceiling-warnings">
            {state.warnings.map((w) => (
              <li key={`${w.path}:${w.message}`}>{w.message}</li>
            ))}
          </ul>
        </Callout>
      )}
      {state.kind === "unknown" && (
        // NOT A REFUSAL AND NOT A SAVE: the change may have landed. The form's
        // own button turns into "Send again" ([pressLabel]), which resends the
        // SAME operation — a fresh one would be a second write.
        <Callout variant="warning">{state.message}</Callout>
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

/**
 * Whether the form's own button sends what the last press held rather than
 * the typed values: a write a check WARNED about, confirmed as checked, and
 * one whose outcome is UNKNOWN, resent under its own operation — the typed
 * values pressed again would be a second write beside a first that may have
 * landed.
 */
function resends(write: CeilingWrite): boolean {
  return write.state.kind === "warned" || write.state.kind === "unknown";
}

/** The form's own button, in the words of what it will do. */
function pressLabel(write: CeilingWrite): string {
  return write.state.kind === "warned"
    ? "Save anyway"
    : write.state.kind === "unknown"
      ? "Send again"
      : "Save";
}

// ---------------------------------------------------------------------------
// One window, in place
// ---------------------------------------------------------------------------

/**
 * One window's ceiling as a figure, and — for a reader holding `config:write` — as a field.
 *
 * WHAT IT SHOWS AFTER A SAVE is the ceiling SAVED, marked as applying, until
 * this node has applied it and the answer the screen draws carries it. Going
 * back to the served figure at once drew the ceiling being replaced for a
 * beat after "Saved", which reads as a save that did not take.
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
  /** Re-read what the screen draws once this node applies a saved ceiling. */
  onApplied: () => void;
}) {
  const access = useConfigWriteAccess();
  const write = useCeilingWrite(scope, { onApplied });
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState("");
  const [error, setError] = useState<string | undefined>();
  // The value the last save wrote, so the figure says it while it applies.
  const [expected, setExpected] = useState<number | null | undefined>(undefined);
  const pencil = useRef<HTMLButtonElement>(null);
  const id = useId();

  const whose = whoseCeiling(scope);
  const adjective = PERIOD_ADJECTIVE[period];
  const limit = w?.limit;

  const saved = write.state.kind === "saved" ? write.state : null;
  // SETTLED once the answer re-read after the apply has arrived: the applying
  // mark goes, and the figure is the answer's own again. The served ceiling
  // matching the one saved settles it too; and an answer that arrived after
  // the apply and says otherwise (a colleague's newer ceiling) is the truth,
  // so it settles on that rather than drawing the saved value for ever.
  const seenAtApply = useRef<BudgetWindow | undefined | null>(null);
  const { reset } = write;
  useEffect(() => {
    if (!saved?.applied) {
      seenAtApply.current = null;
      return;
    }
    if (seenAtApply.current === null) seenAtApply.current = w;
    if ((limit ?? null) === expected || w !== seenAtApply.current) {
      seenAtApply.current = null;
      reset();
      setExpected(undefined);
    }
  }, [saved?.applied, w, limit, expected, reset]);

  // A SAVE CLOSES THE FIELD: the figure beside the bar says the new ceiling
  // and that it is applying, which is the answer the person was waiting for.
  useEffect(() => {
    if (!editing || !saved) return;
    setEditing(false);
    pencil.current?.focus();
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
    // top of the page after every edit.
    queueMicrotask(() => pencil.current?.focus());
  }

  function submit(event?: FormEvent) {
    event?.preventDefault();
    if (write.busy) return;
    // ENTER ON A WARNING IS THE SECOND PRESS the warning asked for, exactly
    // as the button under it is — and on an unknown, the resend.
    if (resends(write)) {
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

  const shown = saved && expected !== undefined ? expected : (limit ?? null);
  const who = scope.kind === "company" ? "The company" : scope.name;
  const fieldId = `${id}-ceiling`;
  return (
    <span className="ceiling">
      <span className={shown === null ? "ceiling-value muted" : "ceiling-value"}>
        {shown === null ? "No ceiling" : `of ${fmtCount(shown)}`}
      </span>
      {saved && (
        <span className="ceiling-applying t-caption" role="status">
          {saved.applied ? "applied" : "applying…"}
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
                ? `${fmtCount(w.used)} spent ${PERIOD_WORDS[period]}, ${resetsWords(w, zone)}. Type 40M, 2.5M or digits; empty is no ceiling.`
                : "Type 40M, 2.5M or digits; empty is no ceiling."
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
              type={resends(write) ? "button" : "submit"}
              size="small"
              variant="primary"
              loading={write.busy}
              onClick={resends(write) ? write.confirm : undefined}
            >
              {pressLabel(write)}
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
  const access = useConfigWriteAccess();
  const budgets = useQuery("budgets", undefined);
  const write = useCeilingWrite(scope);
  const zone = budgets.data?.timezone ?? "";
  const windows =
    scope.kind === "company"
      ? budgets.data?.org?.windows
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

  // A SAVE ENDS THE DIALOG, and the toast carries the revision's own summary
  // — the sentence the history will show — and that it is applying, since
  // the gate enforces the ceiling it replaced until each node has it.
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
      message: "Saved. Each node enforces it once it has applied the new configuration.",
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
            ? "Waiting for the engine to answer."
            : undefined;

  const warned = resends(write);
  const save = () => {
    if (warned) write.confirm();
    else if (!blocked) write.submit(values);
  };
  const title =
    scope.kind === "company" ? "Raise the company's budget" : `Raise ${scope.name}'s budget`;

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
            {pressLabel(write)}
          </Button>
        </>
      }
    >
      <div className="col gap-3">
        <p className="t-caption">
          A window resets on its own at the turn of its day, week or month on the company clock
          {zone ? ` (${zone})` : ""}; raise the ceiling to make room now. Leave a field empty for no
          ceiling on that window.
        </p>
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
                      w.refused_at ? ` · refusing since ${fmtDateTime(w.refused_at)}` : ""
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
  const access = useConfigWriteAccess();
  const org = useOrgBudget();
  const [open, setOpen] = useState(false);
  const company = own ? undefined : waitedOn(org?.org.windows, "refusing");
  const scope: CeilingScope = company ? { kind: "company" } : { kind: "seat", handle, name };
  const focus = (own ?? company)?.period;
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
