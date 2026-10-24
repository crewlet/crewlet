/**
 * The company's charter as values: what each part holds, when two readings of
 * a part are the same, what a draft of one may not be, and the sentence its
 * revision is recorded with.
 *
 * PURE, with no React, network or clock, so every rule the editor keeps is
 * pinned by a test over values rather than through a rendered card.
 *
 * # Three parts, each written on its own
 *
 * The mission, the vision and the standing policies are three statements a
 * person changes for different reasons, and each save is one configuration
 * revision every agent reads from its next turn. So a save changes ONE part
 * and leaves whatever a colleague wrote to the other two in place
 * (`save.ts`), and its revision's summary says which statement moved.
 *
 * # Compared and judged as the engine does
 *
 * The engine trims every value before it judges or stores it, so two readings
 * of a part are compared TRIMMED, and a policy list item by item in order: a
 * stored mission with a trailing space is not a change somebody made, and a
 * list reordered is. Its admission rules (`internal/config`, the bounds in
 * `contract/config.ts`) are checked here on the same trimmed values, counted
 * in characters as the engine counts them, so a draft is marked where it is
 * typed rather than refused after Save.
 */

import { countCharacters } from "@crewlethq/ui";
import {
  MISSION_MAX_CHARS,
  POLICIES_MAX,
  POLICY_MAX_CHARS,
  VISION_MAX_CHARS,
  type Charter,
} from "~/contract/config.ts";
import type { CharterPart } from "../PrincipleCard.tsx";

export type { Charter, CharterPart };

/**
 * The white space the engine trims (`strings.TrimSpace`, which is Go's
 * `unicode.IsSpace`), written as escapes. NOT `String.prototype.trim`: that
 * keeps U+0085 (NEL), which the engine trims, and trims U+FEFF (BOM), which
 * the engine keeps, so a value at its bound with either at an end would be
 * counted one way here and judged the other way there.
 */
const LEADING_SPACE =
  /^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+/;
const TRAILING_SPACE =
  /[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/;

/** `text` trimmed exactly as the engine trims it before it judges or stores it. */
export function trimLikeEngine(text: string): string {
  return text.replace(LEADING_SPACE, "").replace(TRAILING_SPACE, "");
}

/** Every part, in reading order. */
export const CHARTER_PARTS: readonly CharterPart[] = ["mission", "vision", "policies"];

/** What one part holds: a statement, or the ordered policies. */
export type PartValue = string | readonly string[];

/**
 * The charter a source holds (the org projection, or the charter route's
 * answer), absent or malformed parts empty.
 */
export function charterOf(source: {
  readonly mission?: unknown;
  readonly vision?: unknown;
  readonly policies?: unknown;
}): Charter {
  return {
    mission: typeof source.mission === "string" ? source.mission : "",
    vision: typeof source.vision === "string" ? source.vision : "",
    policies: Array.isArray(source.policies)
      ? source.policies.filter((p): p is string => typeof p === "string")
      : [],
  };
}

/** One part of a charter. */
export function partOf(charter: Charter, part: CharterPart): PartValue {
  return charter[part];
}

/**
 * A part as it will be saved: trimmed, and a list of trimmed policies with
 * the BLANK ONES LEFT OUT. A row left empty is no rule anybody wrote, so it is
 * dropped rather than refused, and never counts as a change.
 */
export function normalized(value: PartValue): PartValue {
  return typeof value === "string"
    ? trimLikeEngine(value)
    : value.map((item) => trimLikeEngine(item)).filter((item) => item !== "");
}

/** [normalized] for a policy list, typed as one. */
export function normalizedPolicies(value: readonly string[]): string[] {
  return value.map((item) => trimLikeEngine(item)).filter((item) => item !== "");
}

/** `charter` with `part` replaced by `value` as it will be saved, the other two untouched. */
export function withPart(charter: Charter, part: CharterPart, value: PartValue): Charter {
  return typeof value === "string"
    ? { ...charter, [part]: trimLikeEngine(value) }
    : { ...charter, policies: normalizedPolicies(value) };
}

/** Whether a part holds nothing at all. */
export function isEmptyPart(value: PartValue): boolean {
  const clean = normalized(value);
  return clean.length === 0;
}

/** Whether two readings of one part say the same thing to every agent. */
export function samePart(a: PartValue, b: PartValue): boolean {
  const x = normalized(a);
  const y = normalized(b);
  if (typeof x === "string" || typeof y === "string") return x === y;
  return x.length === y.length && x.every((item, i) => item === y[i]);
}

/** The most characters a statement or one policy holds, and the most policies. */
export const CHARTER_LIMITS = {
  mission: MISSION_MAX_CHARS,
  vision: VISION_MAX_CHARS,
  policy: POLICY_MAX_CHARS,
  policies: POLICIES_MAX,
} as const;

/** What is wrong with a draft, said where it is wrong. */
export interface PartProblems {
  /** About the part as a whole: a statement too long, a list too long. */
  readonly field?: string;
  /** About each policy, aligned with the draft's items. */
  readonly items?: readonly (string | undefined)[];
}

/** Whether a draft may be saved as it stands. */
export function hasProblems(problems: PartProblems): boolean {
  return problems.field !== undefined || (problems.items?.some((p) => p !== undefined) ?? false);
}

/**
 * How many characters a value holds as it will be saved: trimmed, and counted
 * by code point (the kit's count), which is what the engine counts
 * (`utf8.RuneCountInString`). `.length` would count an emoji twice.
 */
export function lengthOf(text: string): number {
  return countCharacters(trimLikeEngine(text));
}

/** The most characters `part` holds, or one policy of it. */
export function limitOf(part: CharterPart): number {
  return part === "mission"
    ? CHARTER_LIMITS.mission
    : part === "vision"
      ? CHARTER_LIMITS.vision
      : CHARTER_LIMITS.policy;
}

/** A line break anywhere in a value, as the engine's `lineBreak` set has it. */
const LINE_BREAK = /[\n\r\v\f\u0085\u2028\u2029]/;

/**
 * Everything a draft of `part` breaks IN WHAT IT CHANGES from `was`, worded
 * for the person writing it: the engine's admission rules
 * (`config.ValidateCharterChange`), on the trimmed values. Without `was`,
 * everything is judged.
 *
 * JUDGED ON THE CHANGE, as the engine judges it, because the rules arrived
 * after companies were written: a stored policy that runs long must not stop
 * somebody saving a new one beside it. So a statement is judged when it
 * changed, a policy when its text is not one `was` held (as many times), the
 * list's length when it is past the bound AND longer than `was`, and a repeat
 * when the draft says a text more times than `was` did. What the change does
 * not touch is [warningsOf]'s to say, without holding Save.
 *
 * ONE LINE for the mission and for each policy, because each is rendered
 * inline in every seat's prompt and a policy is one item of a list there: a
 * line break would break out of it. The vision is paragraphs and may hold them.
 *
 * `lengths: false` leaves out a statement or a policy that runs past its
 * limit, for a field whose own count already says so beside it (the kit's
 * soft limit marks it invalid and says by how much); the rule still holds,
 * and a save still refuses it, through the default.
 */
export function problemsOf(
  part: CharterPart,
  value: PartValue,
  { lengths = true, was }: { readonly lengths?: boolean; readonly was?: PartValue } = {},
): PartProblems {
  if (typeof value === "string") {
    const text = trimLikeEngine(value);
    if (typeof was === "string" && text === trimLikeEngine(was)) return {};
    const problems: string[] = [];
    const length = lengthOf(text);
    const limit = limitOf(part);
    if (lengths && length > limit) {
      problems.push(`Keep the ${part} to ${limit} characters. It is ${length} now.`);
    }
    if (part === "mission" && LINE_BREAK.test(text)) problems.push("Keep the mission to one line.");
    return problems.length > 0 ? { field: problems.join(" ") } : {};
  }
  // How many times each text stood in `was`: an unchanged policy is one
  // `was` held, as many times, and a repeat is judged against it.
  const before = new Map<string, number>();
  if (was !== undefined && typeof was !== "string") {
    for (const text of normalizedPolicies(was)) before.set(text, (before.get(text) ?? 0) + 1);
  }
  const first = new Map<string, number>();
  const seen = new Map<string, number>();
  const items = value.map((raw, i): string | undefined => {
    const text = trimLikeEngine(raw);
    // A BLANK ROW is dropped on Save ([normalized]), so nothing to say.
    if (text === "") return undefined;
    const times = (seen.get(text) ?? 0) + 1;
    seen.set(text, times);
    if (times <= (before.get(text) ?? 0)) {
      if (!first.has(text)) first.set(text, i);
      return undefined;
    }
    if (LINE_BREAK.test(text)) return "Keep each policy to one line, or split it into two.";
    const length = lengthOf(text);
    if (length > CHARTER_LIMITS.policy) {
      return lengths
        ? `Keep each policy to ${CHARTER_LIMITS.policy} characters. This one is ${length}.`
        : undefined;
    }
    // THE SAME RULE TWICE is read twice by every agent and weighs double;
    // it is said on the later copy, which is the one to remove. Word for
    // word, as the engine judges it: two rules that differ by a capital are
    // two sentences somebody wrote.
    const earlier = first.get(text);
    if (earlier !== undefined) return `This repeats policy ${earlier + 1}.`;
    first.set(text, i);
    return undefined;
  });
  const count = normalizedPolicies(value).length;
  const grew =
    was === undefined || typeof was === "string" || count > normalizedPolicies(was).length;
  const field =
    count > CHARTER_LIMITS.policies && grew
      ? `Keep to ${CHARTER_LIMITS.policies} policies. There are ${count} now.`
      : undefined;
  return {
    ...(field === undefined ? {} : { field }),
    ...(items.some((p) => p !== undefined) ? { items } : {}),
  };
}

/**
 * What a draft breaks in what it does NOT change from `was`: a fault the
 * company's stored charter already had, written before the rules. Said, so
 * the person can fix it while they are here, and never holding Save, since
 * the engine admits a write that leaves it as it was.
 */
export function warningsOf(part: CharterPart, value: PartValue, was: PartValue): string[] {
  const all = problemsOf(part, value);
  const changed = problemsOf(part, value, { was });
  const out: string[] = [];
  if (all.field !== undefined && changed.field === undefined) out.push(all.field);
  all.items?.forEach((problem, i) => {
    if (problem !== undefined && changed.items?.[i] === undefined) {
      out.push(`Policy ${i + 1}: ${problem}`);
    }
  });
  return out;
}

/** The part's name in a sentence. */
const NAMES: Readonly<Record<CharterPart, string>> = {
  mission: "the mission",
  vision: "the vision",
  policies: "the standing policies",
};

/** The part's name at the start of a sentence: "Mission", "Standing policies". */
export const PART_TITLES: Readonly<Record<CharterPart, string>> = {
  mission: "Mission",
  vision: "Vision",
  policies: "Standing policies",
};

/** What each part is for, said in its card while it is unwritten. */
export const PART_PURPOSES: Readonly<Record<CharterPart, string>> = {
  mission: "One sentence on what the company is for tells every agent why its work matters.",
  vision: "Where the company is heading helps agents choose between two good options.",
  policies: "The rules every agent follows, such as how to handle money or when to ask first.",
};

/** What each part is for, said under its field while it is written. */
export const PART_HELP: Readonly<Record<CharterPart, string>> = {
  mission: "One sentence on what the company is for.",
  vision: "Where the company is heading.",
  policies: "Every agent follows these in order. Alt+Up and Alt+Down move the one you're in.",
};

/** The part's name inside a sentence: "the mission". */
export function partName(part: CharterPart): string {
  return NAMES[part];
}

/**
 * The sentence the revision is recorded with (`_summary`), naming the part
 * and what happened to it, so the company's history reads "Changed the
 * mission" rather than one undifferentiated configuration change.
 */
export function summaryOf(part: CharterPart, before: PartValue, after: PartValue): string {
  const was = normalized(before);
  const now = normalized(after);
  if (typeof was === "string" || typeof now === "string") {
    const name = NAMES[part];
    if (isEmptyPart(was)) return `Wrote ${name}`;
    if (isEmptyPart(now)) return `Cleared ${name}`;
    return `Changed ${name}`;
  }
  return `Standing policies: ${policyChanges(was, now)}`;
}

/**
 * What moved between two policy lists, in words: "added 1, removed 2,
 * reworded 1, reordered".
 *
 * Counted by TEXT, the only identity a policy has: a policy kept word for word
 * is the same policy wherever it moved, one present only after was added and
 * one only before was removed, and a policy that went beside one that came is
 * counted once, as reworded, since that is what an edit in place looks like.
 * Reordered is said only when the policies kept are no longer in the order
 * they were.
 */
export function policyChanges(before: readonly string[], after: readonly string[]): string {
  const left = countOf(before);
  const right = countOf(after);
  let added = 0;
  let removed = 0;
  for (const [text, n] of right) added += Math.max(0, n - (left.get(text) ?? 0));
  for (const [text, n] of left) removed += Math.max(0, n - (right.get(text) ?? 0));
  const reworded = Math.min(added, removed);
  added -= reworded;
  removed -= reworded;
  const kept = (list: readonly string[], other: Map<string, number>) => {
    const budget = new Map(other);
    return list.filter((text) => {
      const n = budget.get(text) ?? 0;
      if (n === 0) return false;
      budget.set(text, n - 1);
      return true;
    });
  };
  const keptBefore = kept(before, right);
  const keptAfter = kept(after, left);
  const reordered = keptBefore.some((text, i) => text !== keptAfter[i]);
  const parts = [
    added > 0 ? `added ${added}` : "",
    removed > 0 ? `removed ${removed}` : "",
    reworded > 0 ? `reworded ${reworded}` : "",
    reordered ? "reordered" : "",
  ].filter(Boolean);
  return parts.length > 0 ? parts.join(", ") : "unchanged";
}

function countOf(list: readonly string[]): Map<string, number> {
  const out = new Map<string, number>();
  for (const text of list) out.set(text, (out.get(text) ?? 0) + 1);
  return out;
}
