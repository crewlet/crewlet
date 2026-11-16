/**
 * Changing the company's delegated-decision rules (ADR-0125), as values: a
 * rule as its form holds it, the rule the document writes, and the merge
 * patch and revision summary of each change Settings › Delegated decisions
 * makes.
 *
 * PURE, so every rule a save turns on is pinned without a network:
 * `lib/useSettingWrite.ts` is the half that sends it.
 *
 * # A rule list is replaced whole
 *
 * A merge patch replaces a list rather than merging it (RFC 7396), so every
 * change to a rule writes the document's whole `rules` list back, built from
 * the list the document holds AT THE SAVE. The rule a form changes is found
 * there by its place and checked to be the one the form was filled from, so a
 * colleague's edit of it in between is a conflict to reload rather than a
 * change silently written over.
 *
 * # The engine decides what a rule may be
 *
 * A form is read here only as far as its SHAPE: a spot-check rate is a whole
 * number, a required-checks entry names a repository and a branch. Which
 * seats may delegate, who may be escalated to, a kind in two rules, a panel on
 * one model: each is the engine's rule, checked against the whole company by
 * the dry run every save makes first and said in the engine's own words.
 */

import type { DelegatedAct } from "~/contract/delegated.ts";
import type { CompanyDocument, ConfigProblem } from "~/protocol/types.ts";
import { isReleaseAct, kindWords } from "./delegation.ts";
import type { SettingChange, SettingPlan } from "./useSettingWrite.ts";
import { andList } from "./words.ts";

/** The kinds a rule may delegate, in the order a form offers them (`other` never is). */
export const DELEGABLE_KINDS: readonly DelegatedAct[] = ["ssh_operate", "pr_merge", "deploy"];

/** One `required_checks` entry as the document writes it. */
export interface DocumentChecks {
  repo: string;
  branch: string;
  checks: string[];
}

/** A rule as the company document writes it (`config.DelegatedRule`). */
export interface DocumentRule {
  kinds: string[];
  delegates: string[];
  escalate_to: string[];
  sample_one_in?: number;
  always_escalate_paths?: string[];
  required_checks?: DocumentChecks[];
}

/** A rule as its form holds it, every field as typed. */
export interface RuleDraft {
  kinds: string[];
  delegates: string[];
  escalateTo: string[];
  /** The spot-check rate as typed; empty is the engine's default. */
  sample: string;
  paths: string[];
  checks: DocumentChecks[];
}

/** The fields of a form an error can sit on. */
export type RuleField = "kinds" | "delegates" | "escalateTo" | "sample" | "paths" | "checks";

/** A panel is two agents; the engine refuses a third. */
export const MAX_DELEGATES = 2;

/** The spot-check rate's default and the range the engine holds it to (the read's `sample`). */
export interface SampleRange {
  default: number;
  min: number;
  max: number;
}

/** The spot-check rates a form offers by name; any other is typed. */
export const SAMPLE_PRESETS: readonly number[] = [1, 5, 10, 50];

/** What a spot-check rate means for the people a rule escalates to. */
export function sampleWords(rate: number): string {
  return rate <= 1
    ? "These people review every decision afterwards."
    : `These people review about 1 in ${rate} of the agent's decisions afterwards.`;
}

/** A form read as a rule, or what is wrong with its shape. */
export type ReadRule =
  | { readonly ok: true; readonly rule: DocumentRule }
  | { readonly ok: false; readonly errors: Partial<Record<RuleField, string>> };

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

const strings = (value: unknown): string[] =>
  Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : [];

/** The document's `delegated_decisions` block, or an empty one. */
function block(document: CompanyDocument): Record<string, unknown> {
  const raw = (document as Record<string, unknown>).delegated_decisions;
  return isRecord(raw) ? raw : {};
}

/** Whether the document turns delegation on. */
export function documentEnabled(document: CompanyDocument): boolean {
  return block(document).enabled === true;
}

/** The document's rules, in order, as it writes them. */
export function documentRules(document: CompanyDocument): DocumentRule[] {
  const rules = block(document).rules;
  if (!Array.isArray(rules)) return [];
  return rules.filter(isRecord).map((r) => {
    const rule: DocumentRule = {
      kinds: strings(r.kinds),
      delegates: strings(r.delegates),
      escalate_to: strings(r.escalate_to),
    };
    if (typeof r.sample_one_in === "number" && r.sample_one_in !== 0) {
      rule.sample_one_in = r.sample_one_in;
    }
    const paths = strings(r.always_escalate_paths);
    if (paths.length > 0) rule.always_escalate_paths = paths;
    const checks = Array.isArray(r.required_checks)
      ? r.required_checks.filter(isRecord).map((c) => ({
          repo: typeof c.repo === "string" ? c.repo : "",
          branch: typeof c.branch === "string" ? c.branch : "",
          checks: strings(c.checks),
        }))
      : [];
    if (checks.length > 0) rule.required_checks = checks;
    return rule;
  });
}

/** A form filled from a rule the document writes, or empty for a new one. */
export function draftOf(rule?: DocumentRule): RuleDraft {
  return {
    kinds: [...(rule?.kinds ?? [])],
    delegates: [...(rule?.delegates ?? [])],
    escalateTo: [...(rule?.escalate_to ?? [])],
    sample: rule?.sample_one_in ? String(rule.sample_one_in) : "",
    paths: [...(rule?.always_escalate_paths ?? [])],
    checks: (rule?.required_checks ?? []).map((c) => ({ ...c, checks: [...c.checks] })),
  };
}

/** Whether a form's kinds include a merge or a deploy, which alone take paths and checks. */
export function releasesIn(kinds: readonly string[]): boolean {
  return kinds.some(isReleaseAct);
}

/**
 * Reads a form as the rule it writes. Shape only (see the file's head): the
 * paths and checks of a rule with no merge or deploy are left out, since the
 * engine takes them only for one, and a checks entry left wholly empty is a
 * row nobody filled in. `range` is the engine's, where the read carried it.
 */
export function readDraft(draft: RuleDraft, range?: SampleRange): ReadRule {
  const errors: Partial<Record<RuleField, string>> = {};
  if (draft.kinds.length === 0) errors.kinds = "Pick at least one kind of request.";
  if (draft.delegates.length === 0) errors.delegates = "Pick the agent that decides.";
  else if (draft.delegates.length > MAX_DELEGATES) {
    errors.delegates = "Pick one agent, or two for a panel.";
  }
  if (draft.escalateTo.length === 0) errors.escalateTo = "Pick at least one person.";
  const sample = draft.sample.trim();
  if (sample !== "" && !/^\d+$/.test(sample)) {
    errors.sample = "Enter a whole number.";
  } else if (sample !== "" && range && (Number(sample) < range.min || Number(sample) > range.max)) {
    errors.sample = `From ${range.min} to ${range.max}.`;
  }
  const releases = releasesIn(draft.kinds);
  const checks = releases
    ? draft.checks
        .map((c) => ({ repo: c.repo.trim(), branch: c.branch.trim(), checks: c.checks }))
        .filter((c) => c.repo !== "" || c.branch !== "" || c.checks.length > 0)
    : [];
  if (checks.some((c) => c.repo === "" || c.branch === "")) {
    errors.checks = "Pick a repository and a branch for each entry.";
  } else if (checks.some((c) => c.checks.length === 0)) {
    errors.checks = "Add at least one check to each entry.";
  }
  if (Object.keys(errors).length > 0) return { ok: false, errors };
  const rule: DocumentRule = {
    kinds: [...draft.kinds],
    delegates: [...draft.delegates],
    escalate_to: [...draft.escalateTo],
  };
  if (sample !== "") rule.sample_one_in = Number(sample);
  if (releases && draft.paths.length > 0) rule.always_escalate_paths = [...draft.paths];
  if (checks.length > 0) rule.required_checks = checks;
  return { ok: true, rule };
}

/** Whether two rules write the same thing. */
export function sameRule(a: DocumentRule, b: DocumentRule): boolean {
  return JSON.stringify(canonical(a)) === JSON.stringify(canonical(b));
}

function canonical(rule: DocumentRule): unknown[] {
  return [
    rule.kinds,
    rule.delegates,
    rule.escalate_to,
    rule.sample_one_in ?? 0,
    rule.always_escalate_paths ?? [],
    (rule.required_checks ?? []).map((c) => [c.repo, c.branch, c.checks]),
  ];
}

/**
 * The merge patch that writes `rules` whole. No rule left removes the key, so
 * the document says nothing rather than an empty list.
 */
export function rulesPatch(rules: readonly DocumentRule[]): Record<string, unknown> {
  return { delegated_decisions: { rules: rules.length > 0 ? rules : null } };
}

/** A rule's kinds as a reader says them: "Merges and Deploys". */
function kindsWords(rule: Pick<DocumentRule, "kinds">): string {
  return andList(rule.kinds.map((k) => kindWords(k as DelegatedAct)));
}

const SUMMARY = "Delegated decisions";

/** Turning delegation on or off. Off is the default, so it removes the key. */
export function enabledPlan(on: boolean): SettingPlan {
  return (document): SettingChange | null => {
    if (documentEnabled(document) === on) return null;
    return {
      patch: { delegated_decisions: { enabled: on ? true : null } },
      summary: `${SUMMARY}: turned ${on ? "on" : "off"}`,
    };
  };
}

/** A new rule, after every rule the document holds. */
export function addRulePlan(rule: DocumentRule): SettingPlan {
  return (document): SettingChange => {
    const rules = documentRules(document);
    return {
      patch: rulesPatch([...rules, rule]),
      summary: `${SUMMARY}: add rule ${rules.length + 1} (${kindsWords(rule)})`,
    };
  };
}

/**
 * Rule `number` (1-based, as the engine numbers it) replaced by `rule`. `was`
 * is the rule the form was filled from: a document whose rule at that place
 * is another is a conflict.
 */
export function editRulePlan(number: number, was: DocumentRule, rule: DocumentRule): SettingPlan {
  return (document): SettingChange | null | "conflict" => {
    const rules = documentRules(document);
    const held = rules[number - 1];
    if (held === undefined || !sameRule(held, was)) return "conflict";
    if (sameRule(held, rule)) return null;
    const next = rules.map((r, i) => (i === number - 1 ? rule : r));
    return {
      patch: rulesPatch(next),
      summary: `${SUMMARY}: change rule ${number} (${kindsWords(rule)})`,
    };
  };
}

/**
 * Rule `number` removed. `kinds` are the kinds the page drew for it: a
 * document whose rule at that place delegates other kinds is a conflict, so
 * a press never removes a rule the reader did not see. Every later rule moves
 * up one.
 */
export function removeRulePlan(number: number, kinds: readonly string[]): SettingPlan {
  return (document): SettingChange | "conflict" => {
    const rules = documentRules(document);
    const held = rules[number - 1];
    if (held === undefined || JSON.stringify(held.kinds) !== JSON.stringify(kinds)) {
      return "conflict";
    }
    return {
      patch: rulesPatch(rules.filter((_, i) => i !== number - 1)),
      summary: `${SUMMARY}: remove rule ${number} (${kindsWords(held)})`,
    };
  };
}

/** Which form field each key of a rule is. */
const FIELD_OF_KEY: Readonly<Record<string, RuleField>> = {
  kinds: "kinds",
  delegates: "delegates",
  escalate_to: "escalateTo",
  sample_one_in: "sample",
  always_escalate_paths: "paths",
  required_checks: "checks",
};

/** `delegated_decisions.rules[2].escalate_to[0]`: the rule's index and its key. */
const RULE_PATH = /^delegated_decisions\.rules\[(\d+)\](?:\.([a-z_]+))?/;

/** A problem in a person's words: the engine's own, else its line without the path. */
export function problemWords(problem: Pick<ConfigProblem, "path" | "message" | "said">): string {
  if (problem.said) return problem.said;
  const prefix = `${problem.path}: `;
  return problem.message.startsWith(prefix)
    ? problem.message.slice(prefix.length)
    : problem.message;
}

/**
 * The engine's problems with rule `index` (0-based), each under the field it
 * names, and every other problem left over to be said whole.
 */
export function placeProblems(
  problems: readonly Pick<ConfigProblem, "path" | "message" | "said">[],
  index: number,
): { fields: Partial<Record<RuleField, string>>; rest: string[] } {
  const fields: Partial<Record<RuleField, string>> = {};
  const rest: string[] = [];
  for (const problem of problems) {
    const at = RULE_PATH.exec(problem.path);
    const field = at && Number(at[1]) === index && at[2] ? FIELD_OF_KEY[at[2]] : undefined;
    const words = problemWords(problem);
    if (field) fields[field] = fields[field] ? `${fields[field]} ${words}` : words;
    else rest.push(words);
  }
  return { fields, rest };
}
