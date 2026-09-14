/**
 * The pieces the node editor and the builder's dialogs share.
 *
 * Not primitives: each of these says something about THIS screen's subject
 * (a tool that is not connected, a schedule nothing could run, a seat with
 * work in flight, a unit's type), so they live beside the dialogs rather than
 * in `ui/`, and they are built only from `ui/` components and tokens.
 */

import { createContext, useContext, useId, useState, type MouseEvent, type ReactNode } from "react";
import type { HumanContactKey } from "~/protocol/index.ts";
import { href } from "~/app/router.tsx";
import { Field, type FieldChoice } from "~/ui/Field.tsx";
import { Banner } from "~/ui/primitives.tsx";
import { Problems } from "~/ui/Problems.tsx";
import type { Acknowledgement } from "./model/changes.ts";
import type { Segment } from "./model/document.ts";
import { CONTACT_IDENTITIES } from "./model/templates.ts";
import type { PlacedProblem, ProblemLink } from "./model/problems.ts";
import { strandedSentence, type StrandedSchedule } from "./preflight.ts";
import { TOOL_NAMES, workingNote, type Tool } from "./nodeFacts.ts";

/** How deep an [EditorSection] sits inside others; 0 for one directly in a drawer or dialog. */
const SectionDepth = createContext(0);

type SectionHeading = "h2" | "h3" | "h4" | "h5" | "h6";

/**
 * A titled group of fields or facts inside a drawer or dialog.
 *
 * ITS HEADING LEVEL IS ITS DEPTH. A drawer or dialog is named by its own
 * title, so a section directly inside one is a second-level heading, and a
 * section inside a section (one tool inside Integrations) is a level below
 * it: a reader moving by heading hears the parts of the form as they nest,
 * rather than every section at one level.
 */
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
  const depth = useContext(SectionDepth);
  const Heading = `h${Math.min(depth + 2, 6)}` as SectionHeading;
  return (
    <section className="builder-section col gap-3" aria-labelledby={id}>
      <div className="col gap-1">
        <Heading className="builder-section-title" id={id}>
          {title}
        </Heading>
        {hint && <p className="t-caption">{hint}</p>}
      </div>
      <SectionDepth.Provider value={depth + 1}>{children}</SectionDepth.Provider>
    </section>
  );
}

/**
 * What a form whose changes exist nowhere else does when a link inside it is
 * followed: `true` when it took the navigation over (to ask first), `false`
 * to let the link go. No guard lets every link go.
 */
const LeaveGuard = createContext<((target: string) => boolean) | null>(null);
export const LeaveGuardProvider = LeaveGuard.Provider;

/**
 * A link to another screen, in the caption register that keeps reading as a
 * link.
 *
 * INSIDE A CHANGED FORM IT ASKS FIRST. Following it replaces the screen, and
 * with it a form whose changes exist nowhere else, so a plain click goes
 * through the form's [LeaveGuard]. A click that opens another tab or window
 * leaves the form where it is, and is left alone.
 */
export function ScreenLink({ to, children }: { to: string[]; children: ReactNode }) {
  const guard = useContext(LeaveGuard);
  const target = href(to);
  const onClick = (event: MouseEvent<HTMLAnchorElement>) => {
    const elsewhere =
      event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
    if (guard === null || event.defaultPrevented || elsewhere) return;
    if (guard(target)) event.preventDefault();
  };
  return (
    <a className="t-link" href={target} onClick={onClick}>
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

/**
 * Why a name has to be free, said where one is typed.
 *
 * WHAT NAMES WHAT. A unit's lead and a `manages` entry name a seat, so a seat
 * name must pick out one seat; a `manages` entry and a root seat's `unit:`
 * reference name a unit, so a unit name must pick out one unit. A lead never
 * names a unit, so it is no reason for a unit's name to be unique.
 */
export const UNIQUE_NAME_HELP = {
  seat: "Seat names are unique: a lead or a manages entry names exactly one seat.",
  unit: "Unit names are unique: a manages entry or a unit reference names exactly one unit.",
} as const;

/**
 * The sentence each acknowledgement asks the operator to accept, wherever it
 * is asked: the review asks for every one, and the charter editor asks for
 * the company rename before it records one, so the two can never describe
 * one consequence two ways.
 *
 * EACH STATES WHAT THE ENGINE KEYS ON, because that is what is lost. An agent
 * seat's id is a UUIDv5 over the company name and the seat's handle
 * (`org.DeriveAgentID`): the diary and the onboarding marker are keyed by
 * that id, while the mailbox (`topics.AgentInbox`) and the episodes are keyed
 * by the handle. So a company rename orphans the first two and keeps the
 * rest, and a handle change loses all of them.
 */
export const ACKNOWLEDGEMENT_TEXT: Readonly<Record<Acknowledgement, string>> = {
  company_rename:
    "An agent seat's id is derived from the company name and its handle, so renaming the company gives every agent seat a new id: each seat's diary and onboarding progress stay under the old id and are no longer read, and every agent seat onboards again. Handles, mailboxes and episodes are unchanged.",
  handle_change:
    "A seat whose handle changes is a new identity to the engine: its memory, inbox and onboarding start over.",
  kind_change:
    "Changing a seat's kind removes the fields listed above, and a removed credential cannot be recovered from the dashboard.",
  credential_servers: "The tool credentials the seats listed above receive will change.",
  mass_removal: "This removes more than half of the company's seats.",
};

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
  disabled,
}: {
  value: string;
  onChange: (next: string) => void;
  error?: string;
  disabled?: boolean;
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
        disabled={disabled}
      />
      {custom && (
        <Field
          label="Custom type"
          kind="id"
          value={value}
          onChange={onChange}
          error={error}
          disabled={disabled}
        />
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

/** One seat's presence outside the chart, by name: what the vendors hold and what it references. */
export interface LeftBehind {
  readonly key: string;
  readonly name: string;
  /** What exists for it at the vendors (see `vendorIdentities`). */
  readonly made: readonly string[];
  /** The secret store entries its fields reference, by name. */
  readonly references: readonly string[];
}

/**
 * What stays at the vendors and in the secret store once a seat is deleted or
 * stops being an agent. Removing configuration tears nothing down at a
 * vendor, so each is named, never by value, with the screens that
 * decommission them.
 */
export function StaysUntilDecommissioned({ entries }: { entries: readonly LeftBehind[] }) {
  if (entries.length === 0) return null;
  return (
    <div className="col gap-2">
      <p className="t-body">
        These stay until you decommission them.{" "}
        <ScreenLink to={["integrations"]}>Open Integrations</ScreenLink>{" "}
        <ScreenLink to={["secrets"]}>Open Secrets</ScreenLink>
      </p>
      <ul className="builder-list">
        {entries.map(({ key, name, made, references }) => (
          <li key={key}>
            {name}
            {made.length > 0 && `: ${made.join(", ")}`}
            {references.length > 0 && (
              <>
                {made.length > 0 ? "; " : ": "}secret store entries{" "}
                {references.map((ref, i) => (
                  <span key={ref}>
                    {i > 0 && ", "}
                    <code className="inline">{ref}</code>
                  </span>
                ))}
              </>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

/** Why the reducer refused an operation, kept on screen beside what caused it. */
export function Refusal({ message }: { message: string | null }) {
  if (!message) return null;
  return <Banner tone="critical">{message}</Banner>;
}

/**
 * What a dialog says in a posture that records nothing.
 *
 * A DISABLED BUTTON IS NOT A REASON. Every dialog refuses to record while the
 * builder is guarded, read only, in a conflict or holding a kept draft, so
 * without this the operator fills the dialog in, finds its one button
 * unavailable, and has nothing on screen telling them the builder rather than
 * their answers is what is in the way.
 */
export function ReadOnlyNote() {
  return (
    <Banner tone="neutral">
      The organization cannot be changed right now, so this change cannot be applied.
    </Banner>
  );
}

/** A read-only list of names or sentences, and nothing at all when there are none. */
export function NameList({ names }: { names: readonly string[] }) {
  if (names.length === 0) return null;
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
 *
 * A WARNING IS NEVER A FIELD'S ERROR. The engine takes a draft that carries
 * warnings, and a field's error slot says the opposite: it is drawn in the
 * critical tone, marks the control invalid and is announced as an alert. So
 * a warning (a lead naming no seat, say) is listed at the top in its own
 * tone, with the path it names, rather than beside the field as a refusal.
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
    if (p.severity !== "problem" || p.field.length === 0) return undefined;
    const matches = fields.filter((path) => startsWith(p.field, path));
    matches.sort((a, b) => b.length - a.length);
    return matches[0] === undefined ? undefined : id(matches[0]);
  };
  const rest = placed.filter((p) => owner(p) === undefined);
  return {
    errorFor: (path) => {
      const mine = placed.filter((p) => owner(p) === id(path));
      return mine.length > 0 ? mine.map((p) => p.message).join("\n") : undefined;
    },
    rest,
  };
}

/** Where a problem the builder cannot fix is fixed instead. */
const PROBLEM_LINKS: Readonly<Record<ProblemLink, string>> = {
  integrations: "Open Integrations",
  schedules: "Open Schedules",
};

/**
 * The problems a form could not place on a field, at its top.
 *
 * WITH THE LINK EACH ONE CARRIES. A problem that names a field the form draws
 * sits beside that field, so what reaches the top is usually about something
 * the builder does not author at all: a schedule on a seat that may not have
 * one, a company integration block. The problem's own link is then the only
 * thing on screen that says where it is fixed. A check answers with
 * refusals or with warnings, never both, so the banner takes the tone of
 * what it holds.
 */
export function NodeProblems({ problems }: { problems: readonly PlacedProblem[] }) {
  if (problems.length === 0) return null;
  const critical = problems.some((p) => p.severity === "problem");
  const links = [...new Set(problems.map((p) => p.link))].filter(
    (link): link is ProblemLink => link !== null,
  );
  return (
    <Banner tone={critical ? "critical" : "caution"}>
      <Problems detail={problems.map((p) => p.message).join("\n")} />
      {links.length > 0 && (
        <p className="row wrap gap-3">
          {links.map((link) => (
            <ScreenLink key={link} to={[link]}>
              {PROBLEM_LINKS[link]}
            </ScreenLink>
          ))}
        </p>
      )}
    </Banner>
  );
}
