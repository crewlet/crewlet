/**
 * LLM fleets as the Models & Fleets page reads and writes them: the words a
 * fleet is said in, the `fleet:<key>` reference a seat job or helper names,
 * the model class a member runs (mirroring the engine's check), and the
 * `llm-fleets` entity writes.
 */

import { fleetLabel, fleetOfRef, fleetRef } from "~/lib/fleets.ts";
import { listed } from "~/lib/format.ts";
import { classifyConfigRefusal, type ConfigRefusal } from "~/protocol/configAnswer.ts";
import {
  answerOf,
  createEntity,
  getEntity,
  outcomeOf,
  putEntity,
  type ConfigWriteOutcome,
} from "~/protocol/configWrite.ts";
import { pathSegment, rest } from "~/protocol/rest.ts";
import type {
  Fleet,
  FleetAssignment,
  FleetMemberKind,
  FleetStrategy,
  FleetTarget,
  LLMFleet,
} from "~/protocol/fleets.ts";
import type { ModelView } from "../data.ts";
import { draftOf, methodOf } from "../wizard/draft.ts";

/** The fleet every new seat and helper starts on: engine-made, never deleted or renamed. */
export const DEFAULT_FLEET = "default-fleet";

/** A target as a picker's value: `fleet:<key>`, or the pinned model's key. */
export const valueOf = (t: FleetTarget): string => ("fleet" in t ? fleetRef(t.fleet) : t.pinned);

export const STRATEGY_WORDS: Readonly<Record<FleetStrategy, { name: string; line: string }>> = {
  balanced: { name: "Balanced", line: "Keep every member at a similar share." },
  sequential: { name: "Sequential", line: "Fill the first member, then the next." },
};

export const KIND_WORDS: Readonly<Record<FleetMemberKind, string>> = {
  plan: "Plan",
  api: "API key",
  cloud: "Cloud account",
  gateway: "Gateway",
};

/** A picker's value in words: "Fleet · Claude Opus", a model's key, or "Not set". */
export function wordOf(value: string, fleets: readonly Fleet[]): string {
  if (!value) return "Not set";
  const key = fleetOfRef(value);
  if (key === null) return value;
  return fleetLabel(fleets.find((f) => f.key === key) ?? { name: key });
}

/**
 * A model's class, as the engine names it for one-class fleets: the Claude
 * tiers and GPT-5 Codex by the model id, else the id itself.
 */
export function classOf(model: string): string {
  const id = model.toLowerCase();
  if (id.includes("opus")) return "Claude Opus";
  if (id.includes("sonnet")) return "Claude Sonnet";
  if (id.includes("haiku")) return "Claude Haiku";
  if (id.includes("codex")) return "GPT-5 Codex";
  return model;
}

/** How a model signs in, as a fleet member's kind: a plan is a subscription sign-in. */
export function kindOf(model: ModelView): FleetMemberKind {
  switch (methodOf(model)) {
    case "token":
    case "host-login":
    case "device":
      return "plan";
    case "cloud":
      return draftOf(model, []).cloud === "gateway" ? "gateway" : "cloud";
    default:
      return "api";
  }
}

/**
 * Why a model cannot join a fleet whose first member is `first`: another class,
 * or the same class on another harness. Null where it fits.
 */
export function misfitOf(model: Pick<ModelView, "harness" | "model">, first: ModelView | null) {
  if (!first) return null;
  const [runs, fleet] = classPair(
    { class: classOf(model.model), harness: model.harness },
    { class: classOf(first.model), harness: first.harness },
  );
  return runs === fleet ? null : `Runs ${runs}; this fleet runs ${fleet}.`;
}

/** Two classes in the engine's words, each with its harness where the harnesses differ. */
export function classPair(
  mine: { class: string; harness: string },
  theirs: { class: string; harness: string },
): [string, string] {
  return mine.harness === theirs.harness
    ? [mine.class, theirs.class]
    : [`${mine.class} on ${mine.harness}`, `${theirs.class} on ${theirs.harness}`];
}

/** The longest fleet name the engine takes. */
export const MAX_FLEET_NAME = 60;

/** Why the engine would refuse a fleet's name, in its words, or null. */
export function nameProblem(
  name: string,
  key: string | null,
  fleets: readonly Fleet[],
): string | null {
  const trimmed = name.trim();
  if (!trimmed) return "Give the fleet a name.";
  if ([...trimmed].length > MAX_FLEET_NAME) return `Use at most ${MAX_FLEET_NAME} characters.`;
  const other = fleets.find((f) => f.key !== key && f.name.toLowerCase() === trimmed.toLowerCase());
  return other ? `Another fleet is called ${other.name}.` : null;
}

/**
 * A new fleet's key, from its name: lowercase, each run of other characters a
 * hyphen, trimmed, at most 63 characters; `-2`, `-3` on a clash.
 */
export function keyFromName(name: string, taken: ReadonlySet<string>): string {
  const base =
    name
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 63)
      .replace(/-+$/g, "") || "fleet";
  if (!taken.has(base)) return base;
  for (let n = 2; ; n++) {
    const suffix = `-${n}`;
    const key = `${base.slice(0, 63 - suffix.length).replace(/-+$/g, "")}${suffix}`;
    if (!taken.has(key)) return key;
  }
}

/** The names a list of subjects is said by: a seat's name, or a helper's. */
export function namesOf(
  subjects: readonly string[],
  assignments: readonly FleetAssignment[],
): string[] {
  const byId = new Map(assignments.map((a) => [a.subject, a.name]));
  return subjects.map((s) => byId.get(s) ?? s);
}

/** "Product Lead and Ask Crewlet run", "Ask Crewlet runs". */
export const verbFor = (names: readonly string[], one: string, many: string) =>
  `${listed([...names])} ${names.length === 1 ? one : many}`;

/** Who is set to a fleet: every seat or helper with a job on it. */
export function onFleet(assignments: readonly FleetAssignment[], key: string): FleetAssignment[] {
  return assignments.filter((a) =>
    (a.jobs ?? [a]).some((j) => "fleet" in j.target && j.target.fleet === key),
  );
}

/** A new fleet, written as one revision: create-only (`If-None-Match: *`), so a taken key is a conflict. */
export function createFleet(
  key: string,
  fleet: LLMFleet,
  summary: string,
): Promise<ConfigWriteOutcome> {
  return createEntity("llm-fleets", key, { ...fleet }, summary, new AbortController().signal);
}

/**
 * A fleet the reading lists, replaced as one revision under its own tag
 * (`If-Match`); the engine serves the made `default-fleet` with one too.
 */
export async function saveFleet(
  key: string,
  fleet: LLMFleet,
  summary: string,
): Promise<ConfigWriteOutcome> {
  const signal = new AbortController().signal;
  const read = await getEntity("llm-fleets", key, signal);
  // DELETED SINCE THE PAGE READ IT: a conflict to reload, never a create behind the reader's back.
  if (read.kind === "missing")
    return { kind: "conflict", reason: "revision_advanced", currentRevisionId: null };
  if (read.kind !== "entity") return read;
  return putEntity("llm-fleets", key, { ...fleet }, read.etag, summary, signal);
}

/** A fleet as its entity, with `members` in their new order. */
export const entityOf = (f: Fleet, members: readonly string[]): LLMFleet => ({
  name: f.name,
  strategy: f.strategy,
  members: [...members],
  threshold_points: f.threshold_points,
});

/** `GET /config/llm-fleets/{key}/references`: every place that names the fleet. */
async function fleetReferences(key: string): Promise<{ at: unknown }[] | ConfigRefusal> {
  const answer = await answerOf(
    rest.request("GET", `/config/llm-fleets/${pathSegment(key)}/references`),
  );
  if (answer.status !== 200) return classifyConfigRefusal(answer);
  const refs = (answer.body as { references?: unknown }).references;
  return Array.isArray(refs) ? (refs as { at: unknown }[]) : [];
}

/**
 * `DELETE /config/llm-fleets/{key}`: the fleet deleted and every place that
 * names it moved to `to` (`fleet:<key>` or an entry key), as one revision.
 */
export async function deleteFleet(
  key: string,
  to: string,
  summary: string,
): Promise<ConfigWriteOutcome> {
  const signal = new AbortController().signal;
  const [read, refs] = await Promise.all([
    getEntity("llm-fleets", key, signal),
    fleetReferences(key),
  ]);
  // DELETED SINCE THE PAGE READ IT, or never written (the default fleet, which is never deleted).
  if (read.kind === "missing")
    return { kind: "conflict", reason: "revision_advanced", currentRevisionId: null };
  if (read.kind !== "entity") return read;
  if (!Array.isArray(refs)) return refs;
  return outcomeOf(
    await answerOf(
      rest.request("DELETE", `/config/llm-fleets/${pathSegment(key)}`, {
        body: { repoint: refs.map((r) => ({ at: r.at, to })) },
        headers: { "If-Match": read.etag, "X-Summary": summary },
      }),
    ),
    "partial",
  );
}
