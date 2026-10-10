/**
 * The pieces the node editor and the builder's dialogs share.
 *
 * Not primitives: each of these says something about THIS screen's subject
 * (a tool that is not connected, a schedule nothing could run, a seat with
 * work in flight, a unit's type), so they live beside the dialogs rather than
 * in `ui/`, and they are built only from `ui/` components and tokens.
 */

import {
  createContext,
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { cx } from "@crewlethq/ui";
import type { HumanContactKey } from "~/protocol/index.ts";
import { href } from "~/app/router.tsx";
import { ConfigField } from "~/components/ConfigField.tsx";

import { Problems } from "~/ui/Problems.tsx";
import type { Acknowledgement } from "./model/changes.ts";
import type { Segment } from "./model/document.ts";
import { CONTACT_IDENTITIES } from "./model/templates.ts";
import type { PlacedProblem, ProblemLink } from "./model/problems.ts";
import { strandedSentence, type StrandedSchedule } from "./preflight.ts";
import { TOOL_NAMES, workingNote, type Tool } from "./nodeFacts.ts";
import {
  Callout,
  CharacterPicker,
  FormField,
  HuePicker,
  InlineCode,
  Input,
  SegmentedControl,
} from "@crewlethq/ui";
import { AVATAR_CHARACTERS } from "~/contract/avatar.ts";
import type { AgentAvatar } from "~/lib/avatar.ts";

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
    /* THE DEPTH IS ON THE ELEMENT, because the stylesheet needs it too: a
       top-level section is separated from what is above it by a rule, and a
       section inside one is a sub-part that takes none. */
    <section className="builder-section col gap-3" data-depth={depth} aria-labelledby={id}>
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
 * Every screen the builder links out to, and where it is.
 *
 * ONE TABLE, AND A NAME AT THE CALL SITE. Twelve links in this builder named
 * their destination as a path literal — `to="integrations"` — which is
 * twelve copies of a fact that belongs to `app/nav.ts`, and the failure of a
 * wrong copy is a link that renders, clicks and lands on "there is no such
 * screen". That is not hypothetical here: every one of them was written
 * against a flat route table (`#/integrations`, `#/schedules`, `#/secrets`)
 * and this application puts those screens under a workspace, so the whole set
 * went dead at once and nothing in the build said so.
 *
 * A NAME CANNOT BE WRONG IN THAT WAY. `ScreenName` is a closed union, so a
 * destination this table does not have is a type error at the call site, and
 * a screen that MOVES is one line here. `builderLinks.test.ts` is the other
 * half: it holds each entry against `app/nav.ts`'s own destinations, which is
 * the only thing that can catch a table that is internally consistent and
 * points nowhere.
 */
export const SCREENS = {
  integrations: ["settings", "integrations"],
  schedules: ["agents", "schedules"],
  /** The company's credentials: Settings › Secrets, the engine's own word
   *  (`/secrets`). */
  secrets: ["settings", "secrets"],
  config: ["settings", "config"],
  nodes: ["settings", "nodes"],
} as const satisfies Record<string, readonly [string, ...string[]]>;

/** A screen the builder can send a reader to. */
export type ScreenName = keyof typeof SCREENS;

/** The route segments of a named screen, as `href` and `nav.to` take them. */
export function screenPath(name: ScreenName): string[] {
  return [...SCREENS[name]];
}

/**
 * A link to another screen, in the caption register that keeps reading as a
 * link.
 *
 * A PLAIN LINK, EVEN INSIDE A CHANGED FORM. Following it moves to another
 * entry, which the router puts to the form's leave guard first
 * (`useLeaveGuard`), the same as Back; a click that opens another tab or
 * window moves nothing here and is left alone by the browser itself.
 *
 * AND IT IS PROSE UNTIL TOLD OTHERWISE, because eleven of its fourteen call
 * sites are a remedy trailing the sentence that states the problem — "…so it
 * has no channel to set. Open Integrations" — which is an anchor that is a
 * word in a line and therefore takes `.prose-link` (base.css, WCAG 1.4.1).
 * They carried `.t-link` alone, so what told them apart from the sentence was
 * colour; the baseline's hover underline had been standing in for the mark,
 * which no keyboard or touch reader ever saw, and resetting it is what made
 * that visible. THE DEFAULT IS THE COMMON CASE so a new call site is right
 * without anybody deciding: `standalone` is the opt-out, for the three that
 * are a call to action on a line of their own with no sentence for an
 * underline to separate them from.
 */
export function ScreenLink({
  to,
  children,
  standalone,
}: {
  to: ScreenName;
  children: ReactNode;
  /** This link is chrome on a line of its own, not a word inside a sentence. */
  standalone?: boolean;
}) {
  return (
    <a className={cx("t-link", !standalone && "prose-link")} href={href(screenPath(to))}>
      {children}
    </a>
  );
}

/** What a field for a tool nobody connected says instead of the field. */
export function NotConnected({ tool }: { tool: Tool }) {
  return (
    <p className="builder-note muted">
      {TOOL_NAMES[tool]} is not connected.{" "}
      <ScreenLink to="integrations">Connect it from Integrations</ScreenLink>
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
  link?: { to: ScreenName; label: string };
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

/**
 * The value the Custom chip stands for in the row. Never a unit type, so a
 * type somebody types (even "custom") is never read as the chip itself.
 */
const CUSTOM_TYPE = "\u0000custom";

/** The engine's name for the type a unit that names none is run as. */
const DEFAULT_UNIT_TYPE = "team";

const typeLabel = (type: string) => type.charAt(0).toUpperCase() + type.slice(1);

/**
 * A unit's type: one of the engine's well-known names, or anything else.
 *
 * ONE ROW OF CHIPS, which is the console's own drawing of this field: a chip
 * per name the engine knows, the chosen one filled, and a Custom chip that
 * opens a box of the reader's own IN THE ROW, after it, rather than a second
 * labelled field stacked under the first. The box was a combobox that offered
 * the nine names behind a click and a prefix, so a reader had to know a type
 * existed to be offered it; a row of chips says every answer at once.
 *
 * EMPTY IS THE ENGINE'S DEFAULT, `team` (`org.propagateDownward`), so a unit
 * that names no type is drawn with Team chosen, which is what the engine runs
 * it as: the chosen chip says it, and no help line under the row repeats it.
 * Choosing Custom empties the value until something is typed, so what the
 * row shows and what is saved never differ.
 *
 * CHOSEN ON PURPOSE, NOT IN PASSING: the arrows move along the row and Enter
 * or Space chooses, because choosing Custom puts the reader in its box, and a
 * row that chose as the arrows moved would pull them out of the row each time
 * they passed it.
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
  const typed = value.trim().toLowerCase();
  const known = (UNIT_TYPES as readonly string[]).includes(typed);
  const [customOpen, setCustomOpen] = useState(!known && typed !== "");
  const custom = customOpen || (!known && typed !== "");
  const [focusBox, setFocusBox] = useState(false);
  const box = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (!focusBox) return;
    box.current?.focus();
    setFocusBox(false);
  }, [focusBox]);

  return (
    <FormField as="fieldset" label="Type" error={error}>
      {() => (
        <SegmentedControl
          label="Unit type"
          semantics="radio"
          activate="manual"
          activateHint={null}
          layout="chips"
          value={custom ? CUSTOM_TYPE : known ? typed : DEFAULT_UNIT_TYPE}
          options={[
            ...UNIT_TYPES.map((type) => ({ value: type, label: typeLabel(type), disabled })),
            { value: CUSTOM_TYPE, label: "Custom", disabled },
          ]}
          onValueChange={(next) => {
            if (next === CUSTOM_TYPE) {
              if (!custom) onChange("");
              setCustomOpen(true);
              setFocusBox(true);
              return;
            }
            setCustomOpen(false);
            onChange(next);
          }}
          trailing={
            custom ? (
              <Input
                ref={box}
                inputSize="sm"
                width="xs"
                aria-label="Custom type"
                placeholder="Custom type"
                value={known ? "" : value}
                disabled={disabled}
                onChange={(event) => onChange(event.target.value)}
              />
            ) : null
          }
        />
      )}
    </FormField>
  );
}

/**
 * How an agent seat is drawn: its Crewlet character and its colour, chosen
 * here when the seat is added and in its editor after.
 *
 * TWO FIELDS, ONE PICTURE. The character grid draws every option in the
 * colour chosen below it, so the grid is a preview of the agent it will be,
 * and the colour row names each colour in words rather than asking for a
 * swatch to be told apart. The characters offered are the contract's list,
 * which the engine admits and the design system draws, both held by gates.
 */
export function AvatarField({
  value,
  onChange,
  disabled = false,
  characterError,
  colorError,
}: {
  value: AgentAvatar;
  onChange: (next: AgentAvatar) => void;
  disabled?: boolean;
  /** What the last check said about the character or the block. */
  characterError?: string | undefined;
  /** What the last check said about the colour. */
  colorError?: string | undefined;
}) {
  return (
    <div className="col gap-3">
      <FormField label="Character" as="fieldset" error={characterError}>
        <CharacterPicker
          label="Character"
          characters={AVATAR_CHARACTERS}
          value={value.character}
          hue={value.color}
          disabled={disabled}
          onValueChange={(character) => onChange({ ...value, character })}
        />
      </FormField>
      <FormField label="Color" as="fieldset" error={colorError}>
        <HuePicker
          label="Color"
          value={value.color}
          disabled={disabled}
          onValueChange={(color) => onChange({ ...value, color })}
        />
      </FormField>
    </div>
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
      <ConfigField
        label="Contact"
        kind="choice"
        choices={CONTACT_IDENTITIES.map((c) => ({ value: c.key, label: c.label }))}
        value={identity}
        onChange={(next) => onIdentity(next as HumanContactKey)}
      />
      <ConfigField label={label} kind="id" value={value} onChange={onValue} error={error} />
    </div>
  );
}

/** The schedules an operation would leave with no runner, one sentence each. */
export function StrandedNotes({ stranded }: { stranded: readonly StrandedSchedule[] }) {
  if (stranded.length === 0) return null;
  return (
    <Callout variant="warning">
      <ul className="builder-list">
        {stranded.map((s) => (
          <li key={`${s.unit}/${s.schedule}`}>{strandedSentence(s)}</li>
        ))}
      </ul>
      <ScreenLink to="schedules" standalone>
        Open Schedules
      </ScreenLink>
    </Callout>
  );
}

/** The note for every seat in scope that has work in flight. */
export function WorkingNotes({ names }: { names: readonly string[] }) {
  if (names.length === 0) return null;
  return (
    <Callout variant="info">
      {names.length === 1 ? (
        workingNote(names[0]!)
      ) : (
        <ul className="builder-list">
          {names.map((name) => (
            <li key={name}>{workingNote(name)}</li>
          ))}
        </ul>
      )}
    </Callout>
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
        <ScreenLink to="integrations">Open Integrations</ScreenLink>{" "}
        <ScreenLink to="secrets">Open Secrets</ScreenLink>
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
                    <InlineCode>{ref}</InlineCode>
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

/**
 * Why the reducer refused an operation, kept on screen beside what caused it.
 *
 * ANNOUNCED, because nothing else says it. The dialog asks `recordIntent`
 * before it dispatches, so a refusal never reaches the reducer and the
 * builder's own polite region, which speaks for a recorded operation, never
 * hears about this one. Without the role a reader presses the dialog's one
 * button, the dialog stays open, and the only report is a paragraph they were
 * not looking at.
 */
export function Refusal({ message }: { message: string | null }) {
  if (!message) return null;
  return (
    <Callout variant="danger" role="alert">
      {message}
    </Callout>
  );
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
    <Callout variant="neutral">
      The organization cannot be changed right now, so this change cannot be applied.
    </Callout>
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
    // ANNOUNCED WHEN IT IS A REFUSAL, and only then. A Callout is silent by
    // default, because a screen reader arriving at a page would otherwise read
    // out every static banner on it; this one appears in ANSWER to a check the
    // operator just ran, and a refusal is what stops the save.
    <Callout variant={critical ? "danger" : "warning"} role={critical ? "alert" : undefined}>
      <Problems detail={problems.map((p) => p.message).join("\n")} />
      {links.length > 0 && (
        <p className="row wrap gap-3">
          {links.map((link) => (
            <ScreenLink key={link} to={link} standalone>
              {PROBLEM_LINKS[link]}
            </ScreenLink>
          ))}
        </p>
      )}
    </Callout>
  );
}
