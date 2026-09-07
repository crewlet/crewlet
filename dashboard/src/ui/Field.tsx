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
 *
 * A URL FIELD WEARS ITS SCHEME. Every config field of this kind is refused
 * without one, so a person typing their site the way they say it out loud
 * ("acme.atlassian.net") had a form that took the value, a Save that failed
 * validation, and nothing on screen to say which of the two was wrong. The
 * affix makes the requirement visible and satisfies it. See [schemeOf] for
 * what happens to a value that brings its own.
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
  const affixID = `${id}-affix`;
  const picker = kind === "choice" || kind === "handle";

  // THE AFFIX IS A DEFAULT, NOT A CAGE. It stands in for the scheme this
  // field would otherwise be refused without, and it steps aside for a value
  // that carries its own: a Data Center instance reachable only over http is
  // a thing the config accepts, and an affix that silently rewrote it to
  // https would point the engine at a port nothing answers on.
  const own = kind === "url" ? schemeOf(value) : "";
  const affix = kind === "url" && own !== "http://" ? "https://" : "";
  const shown = affix ? value.slice(own.length) : value;
  const describedBy = [affix ? affixID : "", help ? helpID : "", error ? errorID : ""]
    .filter(Boolean)
    .join(" ");

  // What the box holds becomes the whole value again, with a scheme somebody
  // typed or pasted taken as said rather than doubled onto the affix.
  function changed(typed: string) {
    if (!affix) return onChange(typed);
    const carried = schemeOf(typed);
    onChange(carried === "http://" ? typed : affix + typed.slice(carried.length));
  }

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
      ) : affix ? (
        <div className="input-affixed">
          <span className="input-affix" id={affixID} aria-hidden="true">
            {affix}
          </span>
          <input
            id={id}
            className="input"
            inputMode="url"
            value={shown}
            placeholder={placeholder}
            disabled={disabled}
            autoComplete="off"
            spellCheck={false}
            autoFocus={autoFocus}
            aria-describedby={describedBy || undefined}
            aria-invalid={error ? true : undefined}
            onChange={(e) => changed(e.target.value)}
          />
        </div>
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
          onChange={(e) => changed(e.target.value)}
        />
      )}
      {affix && (
        // The affix is decoration to a sighted reader and part of the value
        // to everybody else, so it is said once rather than read out as
        // punctuation before the box.
        <span className="sr-only" id={affixID}>
          Begins with {affix}
        </span>
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

/**
 * The scheme a value already carries, or "".
 *
 * Only the two the config accepts, and matched case-insensitively because a
 * pasted address is whatever the address bar had.
 */
function schemeOf(value: string): string {
  const lower = value.toLowerCase();
  if (lower.startsWith("https://")) return "https://";
  if (lower.startsWith("http://")) return "http://";
  return "";
}
