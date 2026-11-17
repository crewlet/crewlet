/**
 * One numeric setting of the company document, drawn and changed where it is
 * read: the engine's row for it (`queries.LimitRow`), its value in force,
 * where the value comes from, and its pencil and reset.
 *
 * THE ONE EDITOR of such a value, by the one setting editor (`SettingEditor`):
 * a number is stepped within its range (the kit's `NumberField`), a duration
 * is an amount and a unit (`DurationField`), each with its default as a chip
 * and common values beside it (`lib/limits.ts`).
 */

import { DurationField, NumberField, type DurationValue } from "@crewlethq/ui";
import { SettingEditor } from "~/components/SettingEditor.tsx";
import { fmtSetting, plural } from "~/lib/format.ts";
import {
  durationUnits,
  limitBounds,
  limitDraft,
  limitHint,
  limitPlan,
  limitPresets,
  limitRefusal,
  limitText,
  limitUnchanged,
  limitUnitWords,
  readLimit,
  type LimitDraft,
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
  words,
  unit,
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
  /** A count in the page's own words, where a bare number says it badly: "12 characters". */
  words?: (amount: number) => string;
  /** What the field's number counts, inside it, where the cap's unit does not say: "characters". */
  unit?: string;
}) {
  const amountWords = (row: LimitRow, amount: number, isOff: boolean): string =>
    words && !isOff ? words(amount) : limitAmountWords(row, amount, isOff, off);
  const defaultWords = amountWords(cap, cap.default, cap.default_off);
  const bounds = limitBounds(cap);
  const presets = limitPresets(cap);
  // A SHARE IS TYPED AS A PERCENTAGE: its default and chips in percent too.
  const shown = (n: number): number => (cap.unit === "fraction" ? Math.round(n * 10_000) / 100 : n);
  const chipWords = (n: number): string =>
    n === shown(cap.default)
      ? defaultWords
      : amountWords(cap, cap.unit === "fraction" ? n / 100 : n, false);
  return (
    <SettingEditor<LimitDraft, number | null>
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
        const row: LimitRow = saved ? { ...cap, value, off: value === 0 && zeroIsOff } : cap;
        const source = words
          ? row.value === row.default
            ? { changed: false, words: "Default" }
            : { changed: true, words: `Changed from ${defaultWords}` }
          : limitSource(row, off);
        return {
          value: amountWords(row, row.value, row.off),
          source: source.words,
          changed: source.changed,
        };
      }}
      edit={{
        open: () => limitDraft(cap),
        defaultState: limitDraft({ ...cap, value: cap.default }),
        defaultWords,
        read: (draft) => readLimit(cap, draft, zero),
        same: (value) => limitUnchanged(cap, value),
        sameWords: "This is the limit already.",
        unchangedWords: "Already the limit.",
        plan: (value) => limitPlan(cap, label, value, area),
        served: (value) => cap.value === (value ?? cap.default),
        hint: limitHint(cap, zero, unit),
        field: ({ id, describedBy, state, set, error, busy }) =>
          typeof state === "string" ? (
            <NumberField
              id={id}
              aria-describedby={describedBy}
              focusOnMount
              value={state}
              onValueChange={set}
              defaultValue={shown(cap.default)}
              {...bounds}
              unit={unit ?? limitUnitWords(cap)}
              presets={presets.map((p) => ({
                value: limitText(cap, p),
                label: chipWords(shown(p)),
              }))}
              formatValue={chipWords}
              error={error}
              disabled={busy}
            />
          ) : (
            <DurationField
              id={id}
              aria-describedby={describedBy}
              focusOnMount
              value={state}
              onValueChange={(next: DurationValue) => set(next)}
              defaultValue={cap.default}
              {...bounds}
              units={durationUnits(cap)}
              presets={presets}
              labels={{ defaultPreset: () => `Default (${defaultWords})` }}
              error={error}
              disabled={busy}
            />
          ),
      }}
    />
  );
}
