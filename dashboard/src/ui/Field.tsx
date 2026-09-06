/**
 * A labelled input, with the help line and the error line beside it.
 *
 * The dashboard had `SearchInput` and `Select` and nothing else: a filter box
 * and a dropdown, both for narrowing a read. A form that collects a credential
 * needs a label bound to its control, a line saying where the value comes
 * from, and a line saying why the engine refused the last one, and every one
 * of those is a thing that gets forgotten when each screen writes its own
 * `<label>` next to its own `<input>`.
 *
 * A SECRET FIELD IS A PASSWORD FIELD, and that is not decoration: `type` is
 * what keeps the value out of an autofill store, out of a screenshot, and out
 * of the accessibility tree as text. The field also refuses to carry a
 * `defaultValue` for a secret, because a value the engine has is a value this
 * page must never have received.
 */

import { useId, type ReactNode } from "react";

export type FieldKind = "text" | "secret" | "url" | "id" | "choice" | "handle";

/** One option of a choice field. */
export interface FieldChoice {
  value: string;
  label: string;
  hint?: string;
}

export function Field({
  label,
  kind = "text",
  value,
  onChange,
  help,
  error,
  placeholder,
  choices,
  disabled,
  required,
  autoFocus,
}: {
  label: string;
  kind?: FieldKind;
  value: string;
  onChange: (v: string) => void;
  help?: ReactNode;
  error?: string;
  placeholder?: string;
  /** Required for `choice` and `handle`; ignored otherwise. */
  choices?: FieldChoice[];
  disabled?: boolean;
  required?: boolean;
  autoFocus?: boolean;
}) {
  const id = useId();
  const helpID = `${id}-help`;
  const errorID = `${id}-error`;
  const describedBy = [help ? helpID : "", error ? errorID : ""].filter(Boolean).join(" ");
  const picker = kind === "choice" || kind === "handle";

  return (
    <div className="field">
      <label htmlFor={id}>
        {label}
        {required === false && <span className="faint"> (optional)</span>}
      </label>
      {picker ? (
        <select
          id={id}
          className="input"
          value={value}
          disabled={disabled}
          aria-describedby={describedBy || undefined}
          aria-invalid={error ? true : undefined}
          onChange={(e) => onChange(e.target.value)}
        >
          <option value="">Choose one</option>
          {(choices ?? []).map((c) => (
            <option key={c.value} value={c.value}>
              {c.label}
            </option>
          ))}
        </select>
      ) : (
        <input
          id={id}
          className="input"
          // A secret is a password field, always. See the note above: the
          // type is what keeps it out of autofill and out of a screenshot.
          type={kind === "secret" ? "password" : "text"}
          inputMode={kind === "url" ? "url" : undefined}
          value={value}
          placeholder={placeholder}
          disabled={disabled}
          // Off for every field here, not only the secrets: a browser
          // offering a saved password for a webhook token is offering the
          // wrong credential to the wrong vendor.
          autoComplete="off"
          spellCheck={false}
          autoFocus={autoFocus}
          aria-describedby={describedBy || undefined}
          aria-invalid={error ? true : undefined}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
      {help && (
        <span className="hint" id={helpID}>
          {help}
        </span>
      )}
      {error && (
        <span className="hint field-error" id={errorID} role="alert">
          {error}
        </span>
      )}
    </div>
  );
}
