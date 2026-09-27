/**
 * What is narrowing the rows, as pills that take themselves off.
 *
 * A CHIP IS DRAWN ONLY WHERE SOMETHING IS ON. The bar this replaces held a
 * control per field whether or not it was set — a search box and five selects
 * — so the two that were narrowing looked exactly like the four that were not,
 * and a reader hunting an empty board had to open each one to find out. A chip
 * exists only when its filter does, so an unfiltered list's row is the "+
 * Filter" that adds one and a filtered one states the whole narrowing in a
 * line, ending in the same control.
 *
 * EACH CHIP IS ONE URL KEY. Taking it off clears that key and nothing else, so
 * there is no table of removers beside the table of chips — `filterChips` says
 * which key a chip stands for and this hands it straight back.
 */

import type { ReactNode } from "react";
import { Button, Tag } from "@crewlethq/ui";
import { XGlyph } from "@crewlethq/icons/glyphs";
import type { FilterChipSpec } from "~/lib/work.ts";

export function FilterChips({
  chips,
  onRemove,
  onClear,
  add,
}: {
  chips: FilterChipSpec[];
  /** Clears one URL key — the one the chip names. */
  onRemove: (param: string) => void;
  onClear: () => void;
  /**
   * The control that adds a narrowing — the Filter menu — drawn after the
   * chips as the approved board draws "+ Filter": the row is where a reader
   * reads what narrows the list, so it is where they add to it.
   */
  add?: ReactNode;
}) {
  if (chips.length === 0 && !add) return null;
  return (
    <div className="work-chips" role="group" aria-label="Filters">
      {chips.map((chip) => (
        <Tag
          key={chip.param}
          appearance="outline"
          size="sm"
          onRemove={() => onRemove(chip.param)}
          // THE WHOLE SENTENCE, because the remove control is announced by its
          // own name and "remove" alone names nothing. A row of eight of them
          // would otherwise read as eight identical controls.
          removeAriaLabel={`Remove the ${chipSentence(chip)} filter`}
        >
          <span className="work-chip-field">{chip.label}</span>
          {chip.verb && <span className="work-chip-verb">{chip.verb}</span>}
          {chip.value && <span className="work-chip-value">{chip.value}</span>}
        </Tag>
      ))}
      {add}
      {/* CLEAR IS NOT A CHIP. It removes every one of them, so drawing it as a
          pill in the same row would put a control that empties the row beside
          the controls that do not — and with nothing on, there is nothing for
          it to clear. */}
      {chips.length > 0 && (
        <Button size="small" variant="ghost" leadingIcon={<XGlyph size="sm" />} onClick={onClear}>
          Clear
        </Button>
      )}
    </div>
  );
}

/** One chip as a sentence, for the control that takes it off. */
function chipSentence(chip: FilterChipSpec): string {
  return [chip.label, chip.verb, chip.value].filter(Boolean).join(" ");
}
