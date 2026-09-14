/**
 * The pieces the node editor and the builder's dialogs share.
 *
 * Not primitives: each of these says something about THIS screen's subject
 * (a tool that is not connected, a schedule nothing could run, a seat with
 * work in flight, a unit's type), so they live beside the dialogs rather than
 * in `ui/`, and they are built only from `ui/` components and tokens.
 */

import { useId, useState, type ReactNode } from "react";
import type { HumanContactKey } from "~/protocol/index.ts";
import { href } from "~/app/router.tsx";
import { Field, type FieldChoice } from "~/ui/Field.tsx";
import { Banner } from "~/ui/primitives.tsx";
import { Problems } from "~/ui/Problems.tsx";
import type { Segment } from "./model/document.ts";
import { CONTACT_IDENTITIES } from "./model/templates.ts";
import type { PlacedProblem } from "./model/problems.ts";
import { strandedSentence, type StrandedSchedule } from "./preflight.ts";
import { TOOL_NAMES, workingNote, type Tool } from "./nodeFacts.ts";

/** A titled group of fields or facts inside a drawer or dialog. */
export function EditorSection({
  title,
  hint,
  children,
}: {
  title: string;
  hint?: ReactNode;
  children: ReactNode;
}) {
  const id = useId();
  return (
    <section className="builder-section col gap-3" aria-labelledby={id}>
      <div className="col gap-1">
        <h3 className="builder-section-title" id={id}>
          {title}
        </h3>
        {hint && <p className="t-caption">{hint}</p>}
      </div>
      {children}
    </section>
  );
}

/** A link to another screen, in the caption register that keeps reading as a link. */
export function ScreenLink({ to, children }: { to: string[]; children: ReactNode }) {
  return (
    <a className="t-link" href={href(to)}>
      {children}
    </a>
  );
}

/** What a field for a tool nobody connected says instead of the field. */
export function NotConnected({ tool }: { tool: Tool }) {
  return (
    <p className="builder-note muted">
      {TOOL_NAMES[tool]} is not connected.{" "}
      <ScreenLink to={["integrations"]}>Connect it from Integrations</ScreenLink>
    </p>
  );
}

/**
 * A value the builder shows and does not edit, with why and where it is
 * edited instead.
 */
export function ReadOnlyFact({
  label,
  children,
  reason,
  link,
}: {
  label: string;
  children: ReactNode;
  reason: ReactNode;
  link?: { to: string[]; label: string };
}) {
  return (
    <div className="builder-fact col gap-1">
      <span className="t-label">{label}</span>
      <div className="t-body">{children}</div>
      <p className="t-caption">
        {reason}
        {link && (
          <>
            {" "}
            <ScreenLink to={link.to}>{link.label}</ScreenLink>
          </>
        )}
      </p>
    </div>
  );
}

/** The unit types the engine knows by name. Any other string is accepted as a custom type. */
export const UNIT_TYPES = [
  "division",
  "department",
  "group",
  "team",
  "squad",
  "pod",
  "guild",
  "chapter",
  "unit",
] as const;

const CUSTOM = "__custom__";

/**
 * A unit's type: one of the engine's well-known names, or a custom one.
 *
 * Empty is the engine's default, `team` (`org.propagateDownward`), and says
 * so rather than showing an unlabelled blank.
 */
export function UnitTypeField({
  value,
  onChange,
  error,
}: {
  value: string;
  onChange: (next: string) => void;
  error?: string;
}) {
  const known = value === "" || (UNIT_TYPES as readonly string[]).includes(value);
  // Chosen, not inferred: picking "Custom type" opens an empty box to type
  // into, and the value is whatever is typed there, even while it happens to
  // match nothing yet.
  const [custom, setCustom] = useState(!known);
  const choices: FieldChoice[] = [
    { value: "", label: "Team (the default)" },
    ...UNIT_TYPES.map((type) => ({
      value: type,
      label: type.charAt(0).toUpperCase() + type.slice(1),
    })),
    { value: CUSTOM, label: "Custom type" },
  ];
  return (
    <>
      <Field
        label="Type"
        kind="choice"
        choices={choices}
        value={custom ? CUSTOM : value}
        onChange={(next) => {
          if (next === CUSTOM) {
            setCustom(true);
            if (known) onChange("");
            return;
          }
          setCustom(false);
          onChange(next);
        }}
        help="Informational: the engine runs every unit type the same way."
        error={custom ? undefined : error}
      />
      {custom && (
        <Field label="Custom type" kind="id" value={value} onChange={onChange} error={error} />
      )}
    </>
  );
}

/** One contact identity for a human seat: which surface, and the identity there. */
export function ContactField({
  identity,
  value,
  onIdentity,
  onValue,
  error,
}: {
  identity: HumanContactKey;
  value: string;
  onIdentity: (next: HumanContactKey) => void;
  onValue: (next: string) => void;
  error?: string;
}) {
  const label = CONTACT_IDENTITIES.find((c) => c.key === identity)?.label ?? identity;
  return (
    <div className="builder-pair">
      <Field
        label="Contact"
        kind="choice"
        choices={CONTACT_IDENTITIES.map((c) => ({ value: c.key, label: c.label }))}
        value={identity}
        onChange={(next) => onIdentity(next as HumanContactKey)}
      />
      <Field label={label} kind="id" value={value} onChange={onValue} error={error} />
    </div>
  );
}

/** The schedules an operation would leave with no runner, one sentence each. */
export function StrandedNotes({ stranded }: { stranded: readonly StrandedSchedule[] }) {
  if (stranded.length === 0) return null;
  return (
    <Banner tone="caution">
      <ul className="builder-list">
        {stranded.map((s) => (
          <li key={`${s.unit}/${s.schedule}`}>{strandedSentence(s)}</li>
        ))}
      </ul>
      <ScreenLink to={["schedules"]}>Open Schedules</ScreenLink>
    </Banner>
  );
}

/** The note for every seat in scope that has work in flight. */
export function WorkingNotes({ names }: { names: readonly string[] }) {
  if (names.length === 0) return null;
  return (
    <Banner tone="info">
      {names.length === 1 ? (
        workingNote(names[0]!)
      ) : (
        <ul className="builder-list">
          {names.map((name) => (
            <li key={name}>{workingNote(name)}</li>
          ))}
        </ul>
      )}
    </Banner>
  );
}

/** Why the reducer refused an operation, kept on screen beside what caused it. */
export function Refusal({ message }: { message: string | null }) {
  if (!message) return null;
  return <Banner tone="critical">{message}</Banner>;
}

/** A read-only list of names, or a sentence saying there are none. */
export function NameList({ names, none }: { names: readonly string[]; none: string }) {
  if (names.length === 0) return <p className="t-body muted">{none}</p>;
  return (
    <ul className="builder-list">
      {names.map((name, i) => (
        <li key={`${name}-${i}`}>{name}</li>
      ))}
    </ul>
  );
}

// ---------------------------------------------------------------------------
// Problems beside fields
// ---------------------------------------------------------------------------

const startsWith = (field: readonly Segment[], path: readonly Segment[]) =>
  path.length <= field.length && path.every((segment, i) => field[i] === segment);

/**
 * Places a node's problems beside the fields a form renders.
 *
 * `fields` is every field path the form draws. A problem whose field starts
 * with one of them is that field's; the rest belong at the top of the form,
 * so nothing the engine said about the node is dropped because no field
 * matches it.
 */
export function placeOnFields(
  placed: readonly PlacedProblem[],
  fields: readonly (readonly Segment[])[],
): {
  errorFor: (path: readonly Segment[]) => string | undefined;
  rest: readonly PlacedProblem[];
} {
  const id = (path: readonly Segment[]) => JSON.stringify(path);
  // The LONGEST field path a problem starts with owns it, so a problem at
  // `integrations.github.tier` lands beside the tier rather than on a whole
  // GitHub section that also happens to be a field.
  const owner = (p: PlacedProblem): string | undefined => {
    const matches = fields.filter((path) => startsWith(p.field, path));
    matches.sort((a, b) => b.length - a.length);
    return matches[0] === undefined ? undefined : id(matches[0]);
  };
  const rest = placed.filter((p) => p.field.length === 0 || owner(p) === undefined);
  return {
    errorFor: (path) => {
      const mine = placed.filter((p) => p.field.length > 0 && owner(p) === id(path));
      return mine.length > 0 ? mine.map((p) => p.message).join("\n") : undefined;
    },
    rest,
  };
}

/** The problems a form could not place on a field, at its top. */
export function NodeProblems({ problems }: { problems: readonly PlacedProblem[] }) {
  if (problems.length === 0) return null;
  const critical = problems.some((p) => p.severity === "problem");
  return (
    <Banner tone={critical ? "critical" : "caution"}>
      <Problems detail={problems.map((p) => p.message).join("\n")} />
    </Banner>
  );
}
