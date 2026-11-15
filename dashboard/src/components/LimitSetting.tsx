/**
 * One numeric setting of the company document, drawn and changed where it is
 * read: the engine's row for it (`queries.LimitRow`), its value in force with
 * the pencil that changes it, and where the value comes from.
 *
 * THE ONE EDITOR of such a value. Settings › Limits drew it first; a page
 * that shows another setting the engine answers as a row of the same shape
 * (the delegated-decision limits, a retention, an interval) draws this, so
 * every one is typed, held to its range, checked and saved the same way
 * (`lib/limits.ts`), by the one setting editor (`SettingEditor`).
 */

import { Input } from "@crewlethq/ui";
import { SettingEditor } from "~/components/SettingEditor.tsx";
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

/**
 * A number of a cap's unit, in words: the value in force, or the default it
 * replaced. A value that switches the limit off says Off, and so does a
 * default of zero for a cap whose zero is off.
 */
export function limitAmountWords(
  row: LimitRow,
  amount: number,
  off: boolean,
  offWords = "Off",
): string {
  if (off) return offWords;
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
export function limitSource(row: LimitRow, offWords?: string): { changed: boolean; words: string } {
  if (row.value === row.default) return { changed: false, words: "Default" };
  return {
    changed: true,
    words: `Changed from ${limitAmountWords(row, row.default, row.default_off, offWords)}`,
  };
}

/**
 * One cap: its name and key, the value in force with the pencil that changes
 * it, and where the value comes from, drawn and saved by the one setting
 * editor (`SettingEditor`).
 *
 * A ZERO THAT SWITCHES THE CAP OFF reads Off, or what the page calls it
 * (`off`: "For ever" for a message horizon), and a range that admits 0 beside
 * its scale says what 0 does there (`zero`).
 */
export function LimitSetting({
  cap,
  label,
  area,
  reread,
  onApplied,
  off,
  zero,
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
  /** What a value that switches the cap off reads as, where Off says it wrong: "For ever". */
  off?: string;
  /** What typing 0 does, for a range that admits it beside its scale: "0 to keep messages for ever". */
  zero?: string;
}) {
  const defaultWords = limitAmountWords(cap, cap.default, cap.default_off, off);
  return (
    <SettingEditor<string, number | null>
      label={label}
      settingKey={cap.key}
      answer={cap}
      settable={cap.settable}
      reread={reread}
      onApplied={onApplied}
      problemWords={(problems) => limitRefusal(cap, problems, zero)}
      draw={(saved) => {
        // Which zeros switch a cap off is the engine's to say, and it says it
        // of the value in force, of the default, and of a range that admits
        // 0 apart from its scale: a saved zero reads Off while it applies only
        // where one of those already told this page that zero is off.
        const value = saved ? (saved.value ?? cap.default) : cap.value;
        const zeroIsOff = cap.off || cap.default_off || cap.range?.or_zero === true;
        const shown: LimitRow = saved ? { ...cap, value, off: value === 0 && zeroIsOff } : cap;
        const source = limitSource(shown, off);
        return {
          value: limitAmountWords(shown, shown.value, shown.off, off),
          source: source.words,
          changed: source.changed,
        };
      }}
      edit={{
        open: () => (cap.value === cap.default ? "" : limitText(cap, cap.value)),
        read: (text) => readLimit(cap, text, zero),
        same: (value) => limitUnchanged(cap, value),
        sameWords: "This is the limit already.",
        unchangedWords: "Already the limit.",
        plan: (value) => limitPlan(cap, label, value, area),
        served: (value) => cap.value === (value ?? cap.default),
        helper: `${limitTyping(cap)}${zero ? ` ${zero[0]!.toUpperCase()}${zero.slice(1)}.` : ""} Empty is the default, ${defaultWords}.`,
        field: ({ id, state, set, error, busy }) => (
          <Input
            id={id}
            value={state}
            autoFocus
            inputMode="decimal"
            placeholder={`Default: ${defaultWords}`}
            autoComplete="off"
            spellCheck={false}
            error={error}
            disabled={busy}
            onChange={(event) => set(event.target.value)}
          />
        ),
      }}
    />
  );
}
