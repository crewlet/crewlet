/**
 * A checkbox with its sentence.
 *
 * WHY IT EXISTS. Two dialogs hand-rolled the same `label > input` with a
 * screen stylesheet's class, and the builder needs a dozen more (an
 * acknowledgement before a rename, "Clear lead" on a move, a schedule's enabled
 * toggle). Hand-rolled, the description beside a box ended up in the
 * decoration-only ink, which is under the contrast floor for a sentence that
 * says what ticking the box deletes.
 *
 * THE RULES IT KEEPS.
 *
 * - THE WHOLE ROW IS ONE CLICK TARGET: the box, the label and the description
 *   are one `<label>`.
 * - THE NAME IS THE LABEL; THE DESCRIPTION DESCRIBES. A screen reader hears
 *   "Also remove the accounts Crewlet created, checkbox" and then the
 *   consequence, rather than the whole paragraph as a name.
 * - A DESTRUCTIVE CHOICE SAYS SO ON THE BOX: `tone="critical"` tints the box
 *   itself, and the description stays in the ink every other fact uses.
 * - `framed` draws the bordered row a dialog uses for a decision that stands on
 *   its own; a checkbox among form fields is unframed.
 */

import { useId, type ReactNode } from "react";
import { cx } from "@crewlethq/ui";

export function Checkbox({
  label,
  description,
  checked,
  onChange,
  disabled,
  tone = "neutral",
  framed = false,
}: {
  label: ReactNode;
  /** What ticking it does, when the label alone does not say. */
  description?: ReactNode;
  checked: boolean;
  onChange: (checked: boolean) => void;
  disabled?: boolean;
  /** "critical" when ticking it destroys something. */
  tone?: "neutral" | "critical";
  /** The bordered row a dialog uses for a decision that stands on its own. */
  framed?: boolean;
}) {
  const id = useId();
  const labelID = `${id}-label`;
  const descriptionID = `${id}-description`;
  return (
    <label
      className={cx(
        "checkbox",
        framed && "framed",
        tone === "critical" && "critical",
        disabled && "disabled",
      )}
    >
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        aria-labelledby={labelID}
        aria-describedby={description ? descriptionID : undefined}
        onChange={(e) => onChange(e.target.checked)}
      />
      <span className="checkbox-text">
        <span className="checkbox-label" id={labelID}>
          {label}
        </span>
        {description && (
          <span className="checkbox-description" id={descriptionID}>
            {description}
          </span>
        )}
      </span>
    </label>
  );
}
