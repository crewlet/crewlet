/**
 * The node editor's form, as values: what it starts from, what it may be
 * changed to, and the one operation that applies it.
 *
 * FORM-LOCAL, THEN ONE OPERATION. The editor keeps every field in a local
 * form while the operator types, and Apply turns the whole form into one
 * `edit` intent (see `model/operations.ts`). Nothing reaches the draft while
 * a field is half written, so a check never runs on a sentence that is not
 * finished and Undo takes back what Apply did, all at once.
 *
 * ONLY WHAT CHANGED. Each part of the intent names a field whose value
 * differs from the form's INITIAL value, never the whole form. A field the
 * operator did not touch is therefore not in the operation at all, so a
 * colleague's change to it survives an update onto their revision, and an
 * unchanged form applies nothing.
 *
 * A FIELD THE DOCUMENT OMITS AND A FIELD LEFT EMPTY ARE THE SAME. An empty
 * text, an empty list and a token budget of 0 (the engine reads both 0 and
 * absent as unlimited) are all written as the field removed, because a
 * `goal: ""` or `token_budget: 0` the base never had would read as an edit
 * nobody made.
 */

import type {
  CompanyDocument,
  ConfigRole,
  ConfigUnit,
  HumanContactKey,
  PhaseLLM,
  ScheduleSpec,
} from "~/protocol/index.ts";
import type { NodeKey } from "./model/keys.ts";
import { getPath, isRecord, jsonEqual } from "./model/json.ts";
import { COMPANY_KEY } from "./model/keys.ts";
import type { EditPartIntent, FieldSet, Intent } from "./model/operations.ts";
import { CONTACT_IDENTITIES } from "./model/templates.ts";

// ---------------------------------------------------------------------------
// Shapes
// ---------------------------------------------------------------------------

export interface CompanyForm {
  readonly name: string;
  readonly mission: string;
  readonly vision: string;
  readonly policies: readonly string[];
}

export interface UnitForm {
  readonly name: string;
  readonly type: string;
  readonly purpose: string;
  /** A seat name; "" for none. */
  readonly lead: string;
  readonly goals: readonly string[];
  readonly channel: string;
  readonly knowledge: readonly string[];
  readonly jira: string;
  readonly confluence: string;
  /** Schedule name to whether it runs. */
  readonly schedules: Readonly<Record<string, boolean>>;
}

export interface SeatForm {
  readonly name: string;
  /** Editable only on a seat this draft created. */
  readonly handle: string;
  readonly email: string;
  readonly goal: string;
  readonly backstory: string;
  readonly responsibilities: readonly string[];
  readonly guidelines: readonly string[];
  /** The explicit `manages` list: seat and unit names. */
  readonly manages: readonly string[];
  readonly contact: Readonly<Record<HumanContactKey, string>>;
  readonly availability: string;
  /**
   * The model chain, provider keys in order; `null` when the seat's `llm` is
   * a per-phase mapping, which the editor shows but does not edit.
   */
  readonly llm: readonly string[] | null;
  /** As typed: digits, or empty for unlimited. */
  readonly tokenBudget: string;
  readonly schedules: Readonly<Record<string, boolean>>;
  readonly githubTier: string;
  readonly githubRepos: readonly string[];
  readonly slackChannel: string;
  readonly mattermostChannel: string;
  readonly mattermostUsername: string;
  /** The seat's own GitLab access level override; "" for the company default. */
  readonly accessLevel: string;
  readonly jira: string;
  readonly confluence: string;
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

const text = (value: unknown): string => (typeof value === "string" ? value : "");
const strings = (value: unknown): string[] =>
  Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : [];

/** Whether a schedule runs, as the engine reads `enabled` (unset and `null` run it). */
export const scheduleRuns = (schedule: ScheduleSpec): boolean => schedule.enabled !== false;

/** The schedules a unit or seat carries, those with a name only. */
export function schedulesOf(data: ConfigRole | ConfigUnit): ScheduleSpec[] {
  return (Array.isArray(data.schedules) ? data.schedules : []).filter(
    (s): s is ScheduleSpec => isRecord(s) && typeof s.name === "string" && s.name !== "",
  );
}

const scheduleToggles = (data: ConfigRole | ConfigUnit): Record<string, boolean> =>
  Object.fromEntries(schedulesOf(data).map((s) => [s.name, scheduleRuns(s)]));

export function companyForm(company: CompanyDocument): CompanyForm {
  return {
    name: text(company.name),
    mission: text(company.mission),
    vision: text(company.vision),
    policies: strings(company.policies),
  };
}

export function unitForm(data: ConfigUnit): UnitForm {
  return {
    name: text(data.name),
    type: text(data.type),
    purpose: text(data.purpose),
    lead: text(data.lead),
    goals: strings(data.goals),
    channel: text(data.channel),
    knowledge: strings(data.knowledge),
    jira: text(getPath(data, ["integrations", "jira", "project"])),
    confluence: text(getPath(data, ["integrations", "confluence", "space"])),
    schedules: scheduleToggles(data),
  };
}

/** A seat's `llm` as an editable chain, or `null` for the per-phase mapping form. */
export function llmChain(llm: PhaseLLM | undefined): string[] | null {
  if (llm === undefined) return [];
  if (typeof llm === "string") return llm.trim() === "" ? [] : [llm];
  if (Array.isArray(llm)) return strings(llm);
  return null;
}

export function seatForm(data: ConfigRole, accessLevel: string): SeatForm {
  const contact = isRecord(data.contact) ? data.contact : {};
  const budget = data.token_budget;
  return {
    name: text(data.name),
    handle: text(data.handle),
    email: text(data.email),
    goal: text(data.goal),
    backstory: text(data.backstory),
    responsibilities: strings(data.responsibilities),
    guidelines: strings(data.behavioral_guidelines),
    manages: strings(data.manages),
    contact: Object.fromEntries(
      CONTACT_IDENTITIES.map(({ key }) => [key, text(contact[key])]),
    ) as Record<HumanContactKey, string>,
    availability: text(data.availability),
    llm: llmChain(data.llm),
    tokenBudget: typeof budget === "number" && budget !== 0 ? String(budget) : "",
    schedules: scheduleToggles(data),
    githubTier: text(getPath(data, ["integrations", "github", "tier"])),
    githubRepos: strings(getPath(data, ["integrations", "github", "repos"])),
    slackChannel: text(getPath(data, ["integrations", "slack", "channel"])),
    mattermostChannel: text(getPath(data, ["integrations", "mattermost", "channel"])),
    mattermostUsername: text(getPath(data, ["integrations", "mattermost", "username"])),
    accessLevel,
    jira: text(getPath(data, ["integrations", "jira", "project"])),
    confluence: text(getPath(data, ["integrations", "confluence", "space"])),
  };
}

// ---------------------------------------------------------------------------
// Checking what was typed
// ---------------------------------------------------------------------------

/** The largest token budget a seat may be given: the engine reads it as a Go `int64`. */
const MAX_TOKEN_BUDGET = Number.MAX_SAFE_INTEGER;

/**
 * Why the typed token budget cannot be written, or `undefined` when it can.
 * A shape the form can see is caught here so Apply never records a value the
 * field could not hold; what the value MEANS is the engine's to judge.
 */
export function tokenBudgetError(typed: string): string | undefined {
  const value = typed.trim();
  if (value === "") return undefined;
  if (!/^\d+$/.test(value))
    return "Give a whole number of tokens, or leave it empty for unlimited.";
  if (Number(value) > MAX_TOKEN_BUDGET) return "That budget is larger than the engine can hold.";
  return undefined;
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

/** A text field's value as the document writes it: removed when empty. */
const textValue = (value: string): string | undefined => (value.trim() === "" ? undefined : value);
/** A list's value as the document writes it: removed when empty. */
const listValue = (value: readonly string[]): string[] | undefined =>
  value.length === 0 ? undefined : [...value];

/** A `FieldSet` when the written value differs, nothing otherwise. */
function changed(path: readonly string[], before: unknown, after: unknown): FieldSet[] {
  if (jsonEqual(before, after)) return [];
  return [{ path, ...(after !== undefined ? { value: after } : {}) }];
}

const toggleParts = (
  target: NodeKey,
  before: Readonly<Record<string, boolean>>,
  after: Readonly<Record<string, boolean>>,
): EditPartIntent[] =>
  Object.keys(after)
    .filter((name) => before[name] !== after[name])
    .map((name) => ({ type: "setScheduleEnabled", target, schedule: name, enabled: after[name]! }));

/** Wraps an editor's parts as the one intent Apply records. */
export function editIntent(target: NodeKey, parts: readonly EditPartIntent[]): Intent {
  return { type: "edit", target, intents: parts };
}

/**
 * Whether a name box holds a rename: the operator changed it, AND what the
 * model would write (the trimmed value) differs from the name the document
 * has.
 *
 * BOTH HALVES. A rename is not an ordinary field: it re-keys a unit's
 * schedules and onboarding pages and makes every agent under it onboard
 * again, so it may only ever come from somebody typing in the box. A stored
 * name that carries surrounding spaces differs from its own trimmed form, so
 * asking the trimmed question alone renamed such a node the moment anything
 * ELSE in its form was applied. And the model compares trimmed, so a box that
 * gained nothing but a trailing space is no rename either.
 */
export function renames(initial: string, typed: string): boolean {
  return typed !== initial && typed.trim() !== initial;
}

export function companyParts(initial: CompanyForm, form: CompanyForm): EditPartIntent[] {
  const set = [
    ...changed(["name"], textValue(initial.name), textValue(form.name.trim())),
    ...changed(["mission"], textValue(initial.mission), textValue(form.mission)),
    ...changed(["vision"], textValue(initial.vision), textValue(form.vision)),
    ...changed(["policies"], listValue(initial.policies), listValue(form.policies)),
  ];
  return set.length > 0 ? [{ type: "updateCompany", set }] : [];
}

export function unitParts(key: NodeKey, initial: UnitForm, form: UnitForm): EditPartIntent[] {
  const parts: EditPartIntent[] = [];
  if (renames(initial.name, form.name)) {
    parts.push({ type: "renameUnit", target: key, name: form.name });
  }
  const set = [
    ...changed(["type"], textValue(initial.type), textValue(form.type.trim())),
    ...changed(["purpose"], textValue(initial.purpose), textValue(form.purpose)),
    ...changed(["goals"], listValue(initial.goals), listValue(form.goals)),
    ...changed(["channel"], textValue(initial.channel), textValue(form.channel.trim())),
    ...changed(["knowledge"], listValue(initial.knowledge), listValue(form.knowledge)),
    ...changed(
      ["integrations", "jira", "project"],
      textValue(initial.jira),
      textValue(form.jira.trim()),
    ),
    ...changed(
      ["integrations", "confluence", "space"],
      textValue(initial.confluence),
      textValue(form.confluence.trim()),
    ),
  ];
  if (set.length > 0) parts.push({ type: "updateUnit", target: key, set });
  if (form.lead !== initial.lead) {
    parts.push({ type: "setLead", target: key, ...(form.lead !== "" ? { lead: form.lead } : {}) });
  }
  parts.push(...toggleParts(key, initial.schedules, form.schedules));
  return parts;
}

/** How a chain is written: one key as a string, unless the seat already wrote a list. */
function llmValue(chain: readonly string[], wasList: boolean): PhaseLLM | undefined {
  if (chain.length === 0) return undefined;
  if (chain.length === 1 && !wasList) return chain[0]!;
  return [...chain];
}

export function seatParts(
  key: NodeKey,
  data: ConfigRole,
  initial: SeatForm,
  form: SeatForm,
  { editableHandle }: { editableHandle: boolean },
): EditPartIntent[] {
  const parts: EditPartIntent[] = [];
  if (renames(initial.name, form.name)) {
    parts.push({ type: "renameSeat", target: key, name: form.name });
  }
  const budget = (typed: string) => {
    const value = typed.trim();
    return value === "" || Number(value) === 0 ? undefined : Number(value);
  };
  const set: FieldSet[] = [
    ...(editableHandle
      ? changed(["handle"], textValue(initial.handle), textValue(form.handle.trim()))
      : []),
    ...changed(["email"], textValue(initial.email), textValue(form.email.trim())),
    ...changed(["goal"], textValue(initial.goal), textValue(form.goal)),
    ...changed(["backstory"], textValue(initial.backstory), textValue(form.backstory)),
    ...changed(
      ["responsibilities"],
      listValue(initial.responsibilities),
      listValue(form.responsibilities),
    ),
    ...changed(
      ["behavioral_guidelines"],
      listValue(initial.guidelines),
      listValue(form.guidelines),
    ),
    ...CONTACT_IDENTITIES.flatMap(({ key: identity }) =>
      changed(
        ["contact", identity],
        textValue(initial.contact[identity]),
        textValue(form.contact[identity].trim()),
      ),
    ),
    ...changed(["availability"], textValue(initial.availability), textValue(form.availability)),
    ...(form.llm !== null && initial.llm !== null
      ? changed(
          ["llm"],
          llmValue(initial.llm, Array.isArray(data.llm)),
          llmValue(form.llm, Array.isArray(data.llm)),
        )
      : []),
    ...changed(["token_budget"], budget(initial.tokenBudget), budget(form.tokenBudget)),
    ...changed(
      ["integrations", "github", "tier"],
      textValue(initial.githubTier),
      textValue(form.githubTier),
    ),
    ...changed(
      ["integrations", "github", "repos"],
      listValue(initial.githubRepos),
      listValue(form.githubRepos),
    ),
    ...changed(
      ["integrations", "slack", "channel"],
      textValue(initial.slackChannel),
      textValue(form.slackChannel.trim()),
    ),
    ...changed(
      ["integrations", "mattermost", "channel"],
      textValue(initial.mattermostChannel),
      textValue(form.mattermostChannel.trim()),
    ),
    ...changed(
      ["integrations", "mattermost", "username"],
      textValue(initial.mattermostUsername),
      textValue(form.mattermostUsername.trim()),
    ),
    ...changed(
      ["integrations", "jira", "project"],
      textValue(initial.jira),
      textValue(form.jira.trim()),
    ),
    ...changed(
      ["integrations", "confluence", "space"],
      textValue(initial.confluence),
      textValue(form.confluence.trim()),
    ),
  ];
  const levelChanged = form.accessLevel !== initial.accessLevel;
  if (set.length > 0 || levelChanged) {
    parts.push({
      type: "updateSeat",
      target: key,
      set,
      ...(levelChanged ? { accessLevel: form.accessLevel === "" ? null : form.accessLevel } : {}),
    });
  }
  if (!jsonEqual(initial.manages, form.manages)) {
    parts.push({ type: "setManages", target: key, manages: [...form.manages] });
  }
  parts.push(...toggleParts(key, initial.schedules, form.schedules));
  return parts;
}

/** The intent Apply records for the company's form. */
export const companyIntent = (initial: CompanyForm, form: CompanyForm): Intent =>
  editIntent(COMPANY_KEY, companyParts(initial, form));
