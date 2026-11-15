/**
 * One numeric setting of the company document, drawn and changed where it is
 * read: the engine's row for it (`queries.LimitRow`), its value in force with
 * the pencil that changes it, and where the value comes from.
 *
 * THE ONE EDITOR of such a value. Settings › Limits drew it first; a page
 * that shows another setting the engine answers as a row of the same shape
 * (the delegated-decision limits, a retention, an interval) draws this, so
 * every one is typed, held to its range, checked and saved the same way
 * (`lib/limits.ts`, `lib/useSettingWrite.ts`).
 */

import {
  Button,
  FormField,
  IconButton,
  Input,
  Popover,
  returnFocus,
  SettingsTable,
} from "@crewlethq/ui";
import { PencilGlyph } from "@crewlethq/icons/glyphs";
import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { SettingWriteStatus, standingWords } from "~/components/SettingWriteStatus.tsx";
import { fmtSetting, plural } from "~/lib/format.ts";
import {
  limitPlan,
  limitRefusal,
  limitText,
  limitTyping,
  limitUnchanged,
  readLimit,
  type LimitRow,
} from "~/lib/limits.ts";
import { useSettingWrite } from "~/lib/useSettingWrite.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";

/**
 * A number of a cap's unit, in words: the value in force, or the default it
 * replaced. A value that switches the limit off says Off, and so does a
 * default of zero for a cap whose zero is off.
 */
export function limitAmountWords(row: LimitRow, amount: number, off: boolean): string {
  if (off) return "Off";
  switch (row.unit) {
    case "seconds":
      return fmtSetting(amount);
    case "fraction":
      return `${Math.round(amount * 100)}%`;
    case "per_second":
      return `${amount.toLocaleString()} per second`;
    case "count":
      return amount.toLocaleString();
    case "days":
      return plural(amount, "day");
  }
}

/**
 * Where a cap's value comes from: the engine's default, or a value the
 * company document sets, with the default it replaced.
 */
export function limitSource(row: LimitRow): { changed: boolean; words: string } {
  if (row.value === row.default) return { changed: false, words: "Default" };
  return {
    changed: true,
    words: `Changed from ${limitAmountWords(row, row.default, row.default_off)}`,
  };
}

/**
 * One cap: its name and key, the value in force with the pencil that changes
 * it, and where the value comes from.
 *
 * WHAT IT SHOWS AFTER A SAVE is the value SAVED, marked as applying, until
 * this node has applied it and the answer the page draws carries it: going
 * back to the served value at once drew the cap being replaced for a beat
 * after the save, which reads as a save that did not take. A node that
 * refused the revision goes on enforcing the value before, so that is what
 * is drawn, marked as refused here.
 */
export function LimitSetting({
  cap,
  label,
  area,
  reread,
  onApplied,
}: {
  cap: LimitRow;
  /** What the setting is called on its page; its key is drawn beside it. */
  label: string;
  /** The page it is changed on, which opens the revision's summary: "Limits". */
  area: string;
  /** How a conflict ends, in the page's words: "Reload to see the limits as they are now." */
  reread: string;
  /** Called once this node applies the saved revision, to read the page's answer again. */
  onApplied: () => void;
}) {
  const access = useConfigWriteAccess();
  const write = useSettingWrite({
    reread,
    onApplied,
    problemWords: (problems) => limitRefusal(cap, problems),
  });
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState("");
  // WHAT IS TYPED, READ AS IT IS TYPED: a value outside the cap's range is
  // said under the field before Save, not sent for the engine to refuse.
  const read = readLimit(cap, text);
  const error = read.ok ? undefined : read.error;
  // SAVE IS HELD while there is nothing it could save: a value the cap
  // cannot take, or the one in force already.
  const held =
    write.state.kind === "warned"
      ? undefined
      : !read.ok
        ? read.error
        : limitUnchanged(cap, read.value)
          ? "This is the limit already."
          : undefined;
  // The value the last save wrote (null: the default), drawn while it applies.
  const [expected, setExpected] = useState<number | null | undefined>(undefined);
  const pencil = useRef<HTMLButtonElement>(null);
  const fieldId = `${useId()}-limit`;

  const saved = write.state.kind === "saved" ? write.state : null;
  const standing = saved?.standing;
  const { reset } = write;
  // SETTLED once the answer re-read after the apply has arrived (the row is a
  // new object, or already says the saved value), and at once where a later
  // activation passed the save: the served value is the truth again.
  const seenAtApply = useRef<LimitRow | null>(null);
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
    if (seenAtApply.current === null) seenAtApply.current = cap;
    const wanted = expected === null ? cap.default : expected;
    if (cap.value === wanted || cap !== seenAtApply.current) {
      seenAtApply.current = null;
      reset();
      setExpected(undefined);
    }
  }, [standing, cap, expected, reset]);

  // A SAVE CLOSES THE FIELD: the value beside the pencil says the new cap and
  // that it is applying, which is the answer the person was waiting for.
  useEffect(() => {
    if (!editing || !saved) return;
    setEditing(false);
    returnFocus(pencil.current);
  }, [editing, saved]);

  function open() {
    write.reset();
    setText(cap.value === cap.default ? "" : limitText(cap, cap.value));
    setEditing(true);
  }

  function close() {
    write.reset();
    setEditing(false);
    // FOCUS GOES BACK TO WHAT OPENED IT, handed back rather than focused, so
    // a pencil a pointer opened is not ringed for leaving the field.
    queueMicrotask(() => returnFocus(pencil.current));
  }

  function submit(event?: FormEvent) {
    event?.preventDefault();
    if (write.busy) return;
    // ENTER ON A WARNING IS THE SECOND PRESS the warning asked for.
    if (write.state.kind === "warned") {
      write.confirm();
      return;
    }
    // ENTER SUBMITS THE FORM WITHOUT THE BUTTON, so the hold is checked here
    // too.
    if (held !== undefined || !read.ok) return;
    setExpected(read.value);
    write.submit(limitPlan(cap, label, read.value, area));
  }

  // THE SAVED VALUE while this node applies it; the served one otherwise.
  const drawsSaved =
    saved &&
    (saved.standing === "applying" || saved.standing === "applied") &&
    expected !== undefined;
  const shownValue = drawsSaved ? (expected ?? cap.default) : cap.value;
  // Which zeros switch a cap off is the engine's to say, and it says it of the
  // value in force and of the default: a saved zero reads Off while it applies
  // only where one of those already told this page that zero is off.
  const shown: LimitRow = drawsSaved
    ? { ...cap, value: shownValue, off: shownValue === 0 && (cap.off || cap.default_off) }
    : cap;
  const source = limitSource(shown);
  const mark = saved ? standingWords(saved.standing) : "";
  return (
    <SettingsTable.Row
      label={label}
      settingKey={cap.key}
      source={source.words}
      changed={source.changed}
    >
      <span className="ceiling">
        <span>{limitAmountWords(shown, shown.value, shown.off)}</span>
        {mark && (
          <span className="ceiling-applying t-caption" role="status">
            {mark}
          </span>
        )}
        {cap.settable && (
          // A PANEL ANCHORED TO THE PENCIL, not a field in the cell: the
          // engine's refusal of a value is a sentence, and the value column
          // is a quarter of the card wide.
          <Popover
            label={label}
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
                label={`Change ${label}`}
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
                label={label}
                optional
                error={error}
                helper={`${limitTyping(cap)} Empty is the default, ${limitAmountWords(cap, cap.default, cap.default_off)}.`}
              >
                <Input
                  id={fieldId}
                  value={text}
                  autoFocus
                  inputMode="decimal"
                  placeholder={`Default: ${limitAmountWords(cap, cap.default, cap.default_off)}`}
                  autoComplete="off"
                  spellCheck={false}
                  error={Boolean(error)}
                  disabled={write.busy}
                  onChange={(event) => {
                    setText(event.target.value);
                    if (write.state.kind !== "idle" && !write.busy) write.reset();
                  }}
                />
              </FormField>
              <SettingWriteStatus
                write={write}
                unchanged="Already the limit."
                onReload={() => {
                  onApplied();
                  close();
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
                  disabledReason={held}
                  title={held}
                  onClick={write.state.kind === "warned" ? write.confirm : undefined}
                >
                  {write.state.kind === "warned" ? "Save anyway" : "Save"}
                </Button>
              </div>
            </form>
          </Popover>
        )}
      </span>
    </SettingsTable.Row>
  );
}
