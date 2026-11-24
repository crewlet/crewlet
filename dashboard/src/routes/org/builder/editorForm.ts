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
 * text, an empty list and an empty token ceiling are all written as the field
 * removed, because a `goal: ""` the base never had would read as an edit
 * nobody made — and an absent window is the only way the engine lets a
 * ceiling say "none" (it refuses a 0 rather than reading it as unlimited).
 *
 * A BOX NOBODY TYPED IN IS NO CHANGE, whatever the form would write for it.
 * Single-line values are written trimmed, so a value the document stored
 * with spaces around it differs from what its own untouched box would write,
 * and asking only "does the written value differ" wrote the trimmed form the
 * moment anything ELSE in the form was applied: an email, a channel, a
 * contact identity, and the company's name, whose change gives every agent
 * seat a new id. So every text field first asks whether its box changed.
 */

import { lookOf, type AgentAvatar } from "~/lib/avatar.ts";
import { BUDGET_WINDOWS } from "~/contract/config.ts";
import { readCeiling, tokenBudgetError } from "~/lib/budget.ts";
import { ceilingText } from "~/lib/ceilings.ts";
import type {
  CompanyDocument,
  ConfigRole,
  ConfigUnit,
  HumanContactKey,
  PhaseLLM,
  ScheduleSpec,
  TokenBudget,
} from "~/protocol/index.ts";
import { fleetOfRef } from "~/lib/fleets.ts";
import type { NodeKey } from "./model/keys.ts";
import { getPath, isRecord, jsonEqual } from "./model/json.ts";
import type { EditPartIntent, FieldSet, Intent } from "./model/operations.ts";
import { CONTACT_IDENTITIES } from "./model/templates.ts";
import { PHASE_MODEL_FIELDS } from "./nodeFacts.ts";

// ---------------------------------------------------------------------------
// Shapes
// ---------------------------------------------------------------------------

/**
 * The company's form: its name, the one company field the builder writes
 * (`model/document.ts`'s `COMPANY_FIELDS`). Its principles are written on
 * Company › Principles.
 */
export interface CompanyForm {
  readonly name: string;
}

export interface UnitForm {
  readonly name: string;
  /** The unit's key: editable only on a unit this draft created. */
  readonly id: string;
  readonly type: string;
  readonly purpose: string;
  /** A seat's handle; "" for none. */
  readonly lead: string;
  readonly goals: readonly string[];
  readonly channel: string;
  readonly knowledge: readonly string[];
  /** The tracker project the unit owns (`project`): a vendor-neutral key, on the unit. */
  readonly project: string;
  /** The knowledge container the unit owns (`space`), on the unit. */
  readonly space: string;
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
  /** The explicit `manages` list: seat handles and unit keys. */
  readonly manages: readonly string[];
  readonly contact: Readonly<Record<HumanContactKey, string>>;
  readonly availability: string;
  /**
   * How an agent seat is drawn, each part as the document writes it or as the
   * running seat is served; null for a person, and for an agent nothing says yet.
   */
  readonly avatar: AgentAvatar | null;
  /**
   * The model chain, provider keys in order; `null` when the seat's `llm` is
   * a per-phase mapping, which the editor shows but does not edit.
   */
  readonly llm: readonly string[] | null;
  /** Each window's ceiling as typed: digits, or empty for no ceiling on that window. */
  readonly tokenBudget: BudgetForm;
  readonly schedules: Readonly<Record<string, boolean>>;
  /** The seat's `code.access`: its tier on Gitlet; "" for unset (read only). */
  readonly codeAccess: string;
  readonly githubRepos: readonly string[];
  readonly mattermostChannel: string;
  readonly mattermostUsername: string;
  /** The seat's own GitLab access level override; "" for the company default. */
  readonly accessLevel: string;
  /**
   * The tracker project the seat writes in (`project`): a vendor-neutral key,
   * on the seat — and the one it owns when the seat sits at the root.
   */
  readonly project: string;
  /** The knowledge container the seat writes in (`space`), owned likewise. */
  readonly space: string;
  /**
   * Each per-phase chain beside `llm` (`llm_review`, `llm_subagent`,
   * `llm_auxiliary`, `llm_judge`, `llm_triage`), as keys in order; `null` for one written
   * as a per-phase mapping, which the editor shows but does not edit.
   */
  readonly phaseModels: Readonly<Record<PhaseModelField, readonly string[] | null>>;
  /** `learning_enabled`: "" for the company's default, else "on" or "off". */
  readonly learning: Learning;
  /** The delegate templates the seat may use (`workers`); empty is every one. */
  readonly workers: readonly string[];
  /** The node the seat is pinned to (`placement.node`); "" for none. */
  readonly placementNode: string;
  /** The labels a node must carry (`placement.labels`), each as `key=value`. */
  readonly placementLabels: readonly string[];
  /** `sandbox.pause_ttl_seconds` as typed: digits, or empty for the default. */
  readonly pauseTTL: string;
  /** `sandbox.max_turns` as typed: digits, or empty for the default. */
  readonly maxTurns: string;
  /** `sandbox.max_concurrent_runs` as typed: digits, or empty for the default. */
  readonly maxRuns: string;
}

/** One of the per-phase chains a seat may carry beside `llm` ([PHASE_MODEL_FIELDS]). */
export type PhaseModelField = (typeof PHASE_MODEL_FIELDS)[number];

/** A seat's own learning setting: unset follows the company. */
export type Learning = "" | "on" | "off";

/** A calendar window a `token_budget:` caps: `day`, `week` or `month`. */
export type BudgetWindow = keyof TokenBudget;

/** A seat's `token_budget:`, one typed box per window. */
export type BudgetForm = Readonly<Record<BudgetWindow, string>>;

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
  };
}

export function unitForm(data: ConfigUnit): UnitForm {
  return {
    name: text(data.name),
    id: text(data.id),
    type: text(data.type),
    purpose: text(data.purpose),
    lead: text(data.lead),
    goals: strings(data.goals),
    channel: text(data.channel),
    knowledge: strings(data.knowledge),
    project: text(data.project),
    space: text(data.space),
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

/**
 * A `token_budget:` as the boxes show it: each window's ceiling as the digits
 * it holds, and empty where the document names none.
 *
 * WHATEVER NUMBER IS THERE, including one this form refuses (a ceiling above
 * 2^53-1, which the engine's int64 accepts from YAML or another writer):
 * showing a box empty over it would hide the value the operator has to see to
 * correct it.
 */
export function budgetForm(budget: unknown): BudgetForm {
  const windows = isRecord(budget) ? budget : {};
  return Object.fromEntries(
    BUDGET_WINDOWS.map(({ period }) => {
      const value = windows[period];
      // IN THE SPELLING EVERY BUDGET SCREEN DRAWS, exact: 400M, where 400000000
      // left a reader counting zeros to tell it from 4B.
      return [period, typeof value === "number" ? ceilingText(value, period) : ""];
    }),
  ) as Record<BudgetWindow, string>;
}

/** A seat's form; `served` is the running seat's look, for a part its author left out. */
export function seatForm(
  data: ConfigRole,
  accessLevel: string,
  served: AgentAvatar | null | undefined,
): SeatForm {
  const contact = isRecord(data.contact) ? data.contact : {};
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
    avatar: data.kind === "human" ? null : lookOf(data.avatar, served),
    llm: llmChain(data.llm),
    tokenBudget: budgetForm(data.token_budget),
    schedules: scheduleToggles(data),
    codeAccess: text(getPath(data, ["code", "access"])),
    githubRepos: strings(getPath(data, ["integrations", "github", "repos"])),
    mattermostChannel: text(getPath(data, ["integrations", "mattermost", "channel"])),
    mattermostUsername: text(getPath(data, ["integrations", "mattermost", "username"])),
    accessLevel,
    project: text(data.project),
    space: text(data.space),
    phaseModels: Object.fromEntries(
      PHASE_MODEL_FIELDS.map((field) => [field, llmChain(data[field] as PhaseLLM | undefined)]),
    ) as Record<PhaseModelField, string[] | null>,
    learning: data.learning_enabled === true ? "on" : data.learning_enabled === false ? "off" : "",
    workers: strings(data.workers),
    placementNode: text(getPath(data, ["placement", "node"])),
    placementLabels: labelEntries(getPath(data, ["placement", "labels"])),
    pauseTTL: numberText(getPath(data, ["sandbox", "pause_ttl_seconds"])),
    maxTurns: numberText(getPath(data, ["sandbox", "max_turns"])),
    maxRuns: numberText(getPath(data, ["sandbox", "max_concurrent_runs"])),
  };
}

/** A number the document holds, as its box shows it; empty where it holds none. */
function numberText(value: unknown): string {
  return typeof value === "number" && Number.isFinite(value) ? String(value) : "";
}

/** A label map as `key=value` entries, in the document's order. */
function labelEntries(value: unknown): string[] {
  if (!isRecord(value)) return [];
  return Object.entries(value).map(([key, v]) => `${key}=${String(v)}`);
}

/**
 * Why a typed placement label cannot be written, or undefined: each is
 * `key=value`, a key at most once, as `placement.labels` maps one to one.
 */
export function placementLabelsError(entries: readonly string[]): string | undefined {
  const seen = new Set<string>();
  for (const entry of entries) {
    const at = entry.indexOf("=");
    const key = at > 0 ? entry.slice(0, at).trim() : "";
    if (key === "") return `“${entry}” is not a label: write it as key=value.`;
    if (seen.has(key)) return `The label ${key} is named twice: a node carries one value for it.`;
    seen.add(key);
  }
  return undefined;
}

/**
 * Why a typed whole number of the executor box cannot be written, or
 * undefined: empty is the default, else digits, zero included (`0` means
 * never pause, or no round cap, which the engine reads as a setting).
 */
export function wholeNumberError(typed: string): string | undefined {
  const t = typed.trim();
  if (t === "" || /^\d+$/.test(t)) return undefined;
  return "Write a whole number, or leave it empty for the default.";
}

/**
 * Why a typed run cap cannot be written, or undefined: `max_concurrent_runs`
 * is a count of runs, so 0 is refused as the engine refuses it, never "none".
 */
export function runCapError(typed: string): string | undefined {
  const t = typed.trim();
  if (t === "" || /^0*[1-9]\d*$/.test(t)) return undefined;
  return "Write a whole number of at least 1, or leave it empty for two.";
}

// ---------------------------------------------------------------------------
// Checking what was typed
// ---------------------------------------------------------------------------

/** Every window's refusal, in window order; empty when all can be written. */
export function tokenBudgetErrors(form: BudgetForm): Partial<Record<BudgetWindow, string>> {
  const out: Partial<Record<BudgetWindow, string>> = {};
  for (const { period } of BUDGET_WINDOWS) {
    const error = tokenBudgetError(period, form[period]);
    if (error !== undefined) out[period] = error;
  }
  return out;
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

/**
 * A text field's part: nothing unless its box changed, then the value as the
 * document writes it (trimmed for a single-line value, removed when empty),
 * when that differs from the stored one.
 */
function textPart(
  path: readonly string[],
  initial: string,
  typed: string,
  { trim }: { trim: boolean },
): FieldSet[] {
  if (typed === initial) return [];
  return changed(path, textValue(initial), textValue(trim ? typed.trim() : typed));
}

const line = { trim: true };
const prose = { trim: false };

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
 * BOTH HALVES. A rename is not an ordinary field: it moves the onboarding
 * pages a unit's members are found by and makes every agent under it onboard
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
    // A rename, like a seat's or a unit's, comes only from the box.
    ...(renames(initial.name, form.name) ? textPart(["name"], initial.name, form.name, line) : []),
  ];
  return set.length > 0 ? [{ type: "updateCompany", set }] : [];
}

export function unitParts(
  key: NodeKey,
  initial: UnitForm,
  form: UnitForm,
  { editableKey }: { editableKey: boolean },
): EditPartIntent[] {
  const parts: EditPartIntent[] = [];
  if (renames(initial.name, form.name)) {
    parts.push({ type: "renameUnit", target: key, name: form.name });
  }
  const set = [
    ...(editableKey ? textPart(["id"], initial.id, form.id, line) : []),
    ...textPart(["type"], initial.type, form.type, line),
    ...textPart(["purpose"], initial.purpose, form.purpose, prose),
    ...changed(["goals"], listValue(initial.goals), listValue(form.goals)),
    ...textPart(["channel"], initial.channel, form.channel, line),
    ...changed(["knowledge"], listValue(initial.knowledge), listValue(form.knowledge)),
    ...textPart(["project"], initial.project, form.project, line),
    ...textPart(["space"], initial.space, form.space, line),
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
  // A FLEET IS WRITTEN ALONE, never as a list: the engine refuses `fleet:` inside one.
  if (chain.length === 1 && (!wasList || fleetOfRef(chain[0]!) !== null)) return chain[0]!;
  return [...chain];
}

/** A learning setting as the document writes it: removed for the company's default. */
const learningValue = (learning: Learning): boolean | undefined =>
  learning === "" ? undefined : learning === "on";

/** Placement labels as the document writes them: a map, removed when empty. */
function labelsValue(entries: readonly string[]): Record<string, string> | undefined {
  if (entries.length === 0) return undefined;
  return Object.fromEntries(
    entries.map((entry) => {
      const at = entry.indexOf("=");
      return [entry.slice(0, at).trim(), entry.slice(at + 1).trim()];
    }),
  );
}

/** A typed whole number as the document writes it: removed when empty. */
function wholeValue(typed: string): number | undefined {
  const t = typed.trim();
  return t === "" ? undefined : Number(t);
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
  // One part PER WINDOW, never the whole mapping: a colleague's new weekly
  // ceiling survives an update that only changed this seat's daily one, and
  // clearing the last window removes the block (see `setPath`).
  // READ AS THE BUDGETS SCREEN READS IT (`lib/budget.ts`): `40M` is forty
  // million here too. A box the reader refuses is one of two things: a typed
  // value, which the form refuses before Apply so it never reaches here, or a
  // STORED one this form refuses — a ceiling above 2^53-1 the engine's int64
  // accepted from YAML or another writer — which is taken as the number it
  // is: read as "no value" it would equal an emptied box, and emptying the
  // box could never remove it.
  const ceiling = (period: BudgetWindow, typed: string) => {
    const read = readCeiling(period, typed);
    if (read.ok) return read.value ?? undefined;
    const stored = Number(typed.trim());
    return Number.isFinite(stored) ? stored : undefined;
  };
  const set: FieldSet[] = [
    ...(editableHandle ? textPart(["handle"], initial.handle, form.handle, line) : []),
    ...textPart(["email"], initial.email, form.email, line),
    ...textPart(["goal"], initial.goal, form.goal, prose),
    ...textPart(["backstory"], initial.backstory, form.backstory, prose),
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
      textPart(["contact", identity], initial.contact[identity], form.contact[identity], line),
    ),
    ...textPart(["availability"], initial.availability, form.availability, prose),
    // ONE PART PER HALF, never the whole block: a half nobody touched is never
    // written, so a part the document leaves to the engine stays left to it.
    ...(form.avatar !== null
      ? [
          ...changed(
            ["avatar", "character"],
            initial.avatar?.character ?? "",
            form.avatar.character,
          ),
          ...changed(["avatar", "color"], initial.avatar?.color ?? "", form.avatar.color),
        ]
      : []),
    ...(form.llm !== null && initial.llm !== null
      ? changed(
          ["llm"],
          llmValue(initial.llm, Array.isArray(data.llm)),
          llmValue(form.llm, Array.isArray(data.llm)),
        )
      : []),
    ...BUDGET_WINDOWS.flatMap(({ period }) =>
      changed(
        ["token_budget", period],
        ceiling(period, initial.tokenBudget[period]),
        ceiling(period, form.tokenBudget[period]),
      ),
    ),
    ...textPart(["code", "access"], initial.codeAccess, form.codeAccess, line),
    ...changed(
      ["integrations", "github", "repos"],
      listValue(initial.githubRepos),
      listValue(form.githubRepos),
    ),
    ...textPart(
      ["integrations", "mattermost", "channel"],
      initial.mattermostChannel,
      form.mattermostChannel,
      line,
    ),
    ...textPart(
      ["integrations", "mattermost", "username"],
      initial.mattermostUsername,
      form.mattermostUsername,
      line,
    ),
    ...textPart(["project"], initial.project, form.project, line),
    ...textPart(["space"], initial.space, form.space, line),
    // ONE PART PER PHASE, as `llm` is written: a chain written as a mapping
    // is shown and left alone.
    ...PHASE_MODEL_FIELDS.flatMap((field) => {
      const before = initial.phaseModels[field];
      const after = form.phaseModels[field];
      if (before === null || after === null) return [];
      return changed(
        [field],
        llmValue(before, Array.isArray(data[field])),
        llmValue(after, Array.isArray(data[field])),
      );
    }),
    ...changed(["learning_enabled"], learningValue(initial.learning), learningValue(form.learning)),
    ...changed(["workers"], listValue(initial.workers), listValue(form.workers)),
    ...textPart(["placement", "node"], initial.placementNode, form.placementNode, line),
    ...changed(
      ["placement", "labels"],
      labelsValue(initial.placementLabels),
      labelsValue(form.placementLabels),
    ),
    ...changed(
      ["sandbox", "pause_ttl_seconds"],
      wholeValue(initial.pauseTTL),
      wholeValue(form.pauseTTL),
    ),
    ...changed(["sandbox", "max_turns"], wholeValue(initial.maxTurns), wholeValue(form.maxTurns)),
    ...changed(
      ["sandbox", "max_concurrent_runs"],
      wholeValue(initial.maxRuns),
      wholeValue(form.maxRuns),
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
