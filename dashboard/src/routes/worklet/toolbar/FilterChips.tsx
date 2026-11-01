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
 * EACH CHIP IS THE KIT'S FILTER PILL, the one drawing every filter in force
 * takes: the field and the relation in the secondary ink, the value in the
 * accent ink, separated by real spaces, so a chip reads "Status is In progress"
 * and its remove control is named by that same sentence.
 *
 * EACH CHIP IS ONE URL KEY. Taking it off clears that key and nothing else, so
 * there is no table of removers beside the table of chips — `filterChips` says
 * which key a chip stands for and this hands it straight back.
 */

import type { ReactNode } from "react";
import { Button, FilterPill } from "@crewlethq/ui";
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
        // THE REMOVE CONTROL IS NAMED BY THE WHOLE SENTENCE, the pill's own
        // default, because "remove" alone names nothing: a row of eight would
        // read as eight identical controls.
        <FilterPill
          key={chip.param}
          field={chip.label}
          operator={chip.verb}
          value={chip.value}
          onRemove={() => onRemove(chip.param)}
        />
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
