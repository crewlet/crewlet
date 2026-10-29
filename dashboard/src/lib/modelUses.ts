/**
 * What a model is FOR, and the one write that says so: Settings › Models'
 * three categories of seat work and the Assistant, mapped onto the engine's
 * fields, and the next company document a change of uses becomes.
 *
 * THREE CATEGORIES, FIVE FIELDS. The engine resolves a seat's five jobs from
 * five fields (`SEAT_JOB_FIELDS`, held to the engine by a Go gate). A person
 * tells three kinds of work apart, so the page offers those, and a category
 * writes every field it holds in the same revision: Coding is `llm`;
 * Reviewing and delegations is `llm_review` and `llm_subagent`; Routines and
 * evaluations is `llm_auxiliary` and `llm_judge`. The pairs are paired by the
 * model each wants: the review and the delegated work take a strong model, and
 * the routines and the onboarding judge, each under a fixed deadline, a fast
 * one, so one choice of model serves both jobs it writes. The Assistant is
 * the company's, not a seat's: `assistant.llm`, with no seats.
 *
 * ONE MODEL PER FIELD. The page writes a field as one scalar key and never a
 * chain (decision D3): a chain the document held there is replaced, and every
 * member that write drops is returned so the review can name it. Taking a
 * model off a job removes it from the field, keeping whatever the field named
 * behind it, so the job resolves as the document then says.
 *
 * WHAT A JOB RUNS ON NOW is the engine's answer (`llm_entries`' `used_by`),
 * never resolved here: this module only writes.
 */

import { SEAT_JOB_FIELDS, type ModelJob } from "~/contract/models.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import type { ModelUse } from "~/protocol/models.ts";

/** A seat's job, as `SEAT_JOB_FIELDS` keys it. */
export type SeatJob = keyof typeof SEAT_JOB_FIELDS;

/** Every seat job, in the engine's order. */
export const SEAT_JOBS = Object.keys(SEAT_JOB_FIELDS) as SeatJob[];

/** The flat satellite fields, which Everything clears. */
const SATELLITES = SEAT_JOBS.filter((j) => j !== "executor");

/** What a person picks a model for: a category of seat work. */
export type JobCategory = "coding" | "reviews" | "routines";

/** The categories in the order every surface lists them. */
export const CATEGORY_ORDER: readonly JobCategory[] = ["coding", "reviews", "routines"];

/** The engine jobs each category holds, and so the fields it writes. */
export const CATEGORY_JOBS: Readonly<Record<JobCategory, readonly SeatJob[]>> = {
  coding: ["executor"],
  reviews: ["review", "subagent"],
  routines: ["auxiliary", "judge"],
};

/** The category each seat job belongs to. */
export const CATEGORY_OF: Readonly<Record<SeatJob, JobCategory>> = {
  executor: "coding",
  review: "reviews",
  subagent: "reviews",
  auxiliary: "routines",
  judge: "routines",
};

/** The fields a category writes, as its tooltip names them: "llm_review and llm_subagent". */
export function categoryFields(category: JobCategory): string[] {
  return CATEGORY_JOBS[category].map((j) => SEAT_JOB_FIELDS[j]);
}

/** A seat job's category, or null for the assistant's two. */
export function categoryOfJob(job: ModelJob): JobCategory | null {
  return job in CATEGORY_OF ? CATEGORY_OF[job as SeatJob] : null;
}

/**
 * A model's uses: the seats that run each seat job on it, and whether the
 * Dashboard Assistant does. The page edits it a category at a time; it holds
 * jobs, so a document that split a category by hand reads back exactly.
 */
export interface Uses {
  jobs: Record<SeatJob, string[]>;
  assistant: boolean;
}

/** No use at all. */
export function noUses(): Uses {
  return {
    jobs: Object.fromEntries(SEAT_JOBS.map((j) => [j, [] as string[]])) as Record<
      SeatJob,
      string[]
    >,
    assistant: false,
  };
}

/**
 * A model's uses as the engine reports them: each seat job a seat RUNS on it
 * (position 0), and the assistant's chat. A fallback position is not a use the
 * page edits; deep work stays in Configuration.
 */
export function usesOf(usedBy: readonly ModelUse[]): Uses {
  const out = noUses();
  for (const use of usedBy) {
    if (use.position !== 0) continue;
    if (use.assistant === "chat") out.assistant = true;
    if (use.seat && use.job && use.job in SEAT_JOB_FIELDS) {
      const job = use.job as SeatJob;
      if (!out.jobs[job].includes(use.seat)) out.jobs[job].push(use.seat);
    }
  }
  for (const job of SEAT_JOBS) out.jobs[job].sort();
  return out;
}

/** Whether a seat holds a category on a model: every job in it, some of them, or none. */
export function categoryState(
  uses: Pick<Uses, "jobs">,
  seat: string,
  category: JobCategory,
): "all" | "some" | "none" {
  const jobs = CATEGORY_JOBS[category];
  const on = jobs.filter((j) => uses.jobs[j].includes(seat)).length;
  return on === 0 ? "none" : on === jobs.length ? "all" : "some";
}

/** The uses with one seat's category switched on or off: every job in it at once. */
export function withCategory(uses: Uses, seat: string, category: JobCategory, on: boolean): Uses {
  const jobs = { ...uses.jobs };
  for (const job of CATEGORY_JOBS[category]) {
    const rest = jobs[job].filter((s) => s !== seat);
    jobs[job] = on ? [...rest, seat].sort() : rest;
  }
  return { ...uses, jobs };
}

/** The seats that run every seat job on the model: Everything, for them. */
export function everythingSeats(uses: Pick<Uses, "jobs">): string[] {
  return uses.jobs.executor.filter((seat) => SEAT_JOBS.every((j) => uses.jobs[j].includes(seat)));
}

/**
 * The one choice a model's uses read as: Everything when every seat job runs
 * on it for the same seats, one category when that category alone does, the
 * Assistant when no seat does, none at all, and `custom` for anything else.
 * The Assistant beside seat work does not change the choice: the page says it
 * on a line of its own.
 */
export type Intent = "all" | JobCategory | "assistant" | "none" | "custom";

export function intentOf(uses: Uses): Intent {
  const used = SEAT_JOBS.filter((j) => uses.jobs[j].length > 0);
  if (used.length === 0) return uses.assistant ? "assistant" : "none";
  const same = (jobs: readonly SeatJob[]) =>
    jobs.every((j) => uses.jobs[j].join(",") === uses.jobs[jobs[0]!].join(","));
  if (used.length === SEAT_JOBS.length && same(SEAT_JOBS)) return "all";
  const whole = CATEGORY_ORDER.find((c) => {
    const jobs = CATEGORY_JOBS[c];
    return used.length === jobs.length && jobs.every((j) => used.includes(j)) && same(jobs);
  });
  return whole ?? "custom";
}

/** The uses an intent sets for `seats`, every other seat job off; the assistant as given. */
export function usesForIntent(
  intent: Exclude<Intent, "custom">,
  seats: readonly string[],
  assistant: boolean,
): Uses {
  const out = noUses();
  const jobs: readonly SeatJob[] =
    intent === "all"
      ? SEAT_JOBS
      : intent === "none" || intent === "assistant"
        ? []
        : CATEGORY_JOBS[intent];
  for (const job of jobs) out.jobs[job] = [...seats].sort();
  out.assistant = intent === "assistant" || assistant;
  return out;
}

/**
 * The order the company's models are declared in, the order a seat that names
 * none falls back through, as the engine's `Providers.ProviderOrder` reads it.
 *
 * `llm_order` is how the document carries it, because a JSON object's keys
 * are sorted on the way out; it is reconciled against the map rather than
 * trusted, since the two travel separately: a key it names that the map lacks
 * is dropped, and a key the map has that it omits comes after, sorted.
 */
export function providerOrder(llm: Record<string, unknown>, order: unknown): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const key of Array.isArray(order) ? order : []) {
    if (typeof key !== "string" || !Object.hasOwn(llm, key) || seen.has(key)) continue;
    seen.add(key);
    out.push(key);
  }
  return [
    ...out,
    ...Object.keys(llm)
      .filter((key) => !seen.has(key))
      .sort(),
  ];
}

// ---------------------------------------------------------------------------
// The next document
// ---------------------------------------------------------------------------

type Path = (string | number)[];
type Doc = Record<string, unknown>;

const isRecord = (v: unknown): v is Doc => typeof v === "object" && v !== null && !Array.isArray(v);

/** The keys one written chain names: a scalar, a list, or nothing. */
export function keysOf(value: unknown): string[] {
  if (typeof value === "string") return value ? [value] : [];
  if (Array.isArray(value)) return value.filter((k): k is string => typeof k === "string" && !!k);
  return [];
}

/** A chain as the document writes it: one key as a scalar, more as a list, none as nothing. */
function chainValue(keys: readonly string[]): string | string[] | undefined {
  if (keys.length === 0) return undefined;
  return keys.length === 1 ? keys[0] : [...keys];
}

/** Every agent seat in the document, by its handle, with the role object to edit. */
export function agentRoles(doc: Doc): Map<string, Doc> {
  const out = new Map<string, Doc>();
  const take = (roles: unknown) => {
    if (!Array.isArray(roles)) return;
    for (const role of roles) {
      if (!isRecord(role) || role.kind === "human") continue;
      if (typeof role.handle === "string" && role.handle) out.set(role.handle, role);
    }
  };
  take(doc.roles);
  const walk = (units: unknown) => {
    if (!Array.isArray(units)) return;
    for (const unit of units) {
      if (!isRecord(unit)) continue;
      take(unit.roles);
      walk(unit.children);
    }
  };
  walk(doc.units);
  return out;
}

/**
 * Where a seat writes a job now: the field an edit finds it in. The executor
 * is `llm`, or the mapping's `default`; a satellite is its flat field, which
 * wins, else the mapping's own key.
 */
function writtenAt(role: Doc, job: SeatJob): Path | null {
  const llm = role.llm;
  if (job === "executor") {
    if (isRecord(llm)) return llm.default !== undefined ? ["llm", "default"] : null;
    return llm !== undefined ? ["llm"] : null;
  }
  const flat = SEAT_JOB_FIELDS[job];
  if (role[flat] !== undefined) return [flat];
  if (isRecord(llm) && llm[job] !== undefined) return ["llm", job];
  return null;
}

function read(role: Doc, path: Path): unknown {
  return path.reduce<unknown>((node, s) => (isRecord(node) ? node[s as string] : undefined), role);
}

function write(role: Doc, path: Path, value: unknown): void {
  let node = role;
  for (const s of path.slice(0, -1)) {
    const next = node[s as string];
    if (!isRecord(next)) return;
    node = next;
  }
  const last = path[path.length - 1] as string;
  if (value === undefined) delete node[last];
  else node[last] = value;
}

/** A mapping-shaped `llm` that holds only its default is stored as the scalar it means. */
function collapse(role: Doc): void {
  const llm = role.llm;
  if (!isRecord(llm)) return;
  const keys = Object.keys(llm).filter((k) => llm[k] !== undefined);
  if (keys.length === 0) delete role.llm;
  else if (keys.length === 1 && keys[0] === "default") role.llm = llm.default;
}

/** A fallback member a write takes out of a seat's field, for the review to name. */
export interface Dropped {
  seat: string;
  job: SeatJob;
  keys: string[];
}

/** What a change of uses writes: the next document, and the fallbacks it drops. */
export interface UsesWrite {
  doc: CompanyDocument;
  dropped: Dropped[];
}

/**
 * The document with `key`'s uses changed from `was` to `next`.
 *
 * - A seat job newly on the model writes `key` as the job's one model in its
 *   flat field (the mapping's `default` for the executor of a seat that writes
 *   a mapping), and clears the mapping's own key for it, which the flat field
 *   would shadow. Whatever the field named before is dropped and returned.
 * - A seat on EVERY job writes `llm: key` as a scalar and deletes every
 *   satellite, in the flat fields and the mapping alike, so every job resolves
 *   to `llm` (the shorthand the turn engine documents).
 * - A seat job taken off the model removes `key` from the field it is written
 *   in, keeping the rest; a field left empty is deleted, so the job resolves as
 *   an unset job does.
 * - The Assistant writes `assistant.llm: key`. Leaving it is not a write: the
 *   assistant runs on something, so it moves when another model is chosen.
 */
export function applyUses(doc: CompanyDocument, key: string, was: Uses, next: Uses): UsesWrite {
  const out = structuredClone(doc) as unknown as Doc;
  const roles = agentRoles(out);
  const dropped: Dropped[] = [];
  const everything = new Set(everythingSeats(next));
  const before = new Set(everythingSeats(was));

  for (const [seat, role] of roles) {
    if (everything.has(seat)) {
      // EVERYTHING ON A SEAT THAT ALREADY RAN IT is no write, and drops nothing.
      if (before.has(seat) && keysOf(read(role, writtenAt(role, "executor") ?? ["llm"]))[0] === key)
        continue;
      for (const job of SEAT_JOBS) {
        const at = writtenAt(role, job);
        const keys = at ? keysOf(read(role, at)).filter((k) => k !== key) : [];
        if (keys.length > 0) dropped.push({ seat, job, keys });
      }
      for (const job of SATELLITES) delete role[SEAT_JOB_FIELDS[job]];
      role.llm = key;
      continue;
    }
    for (const job of SEAT_JOBS) {
      const on = next.jobs[job].includes(seat);
      const had = was.jobs[job].includes(seat);
      if (on && !had) {
        const at = writtenAt(role, job);
        const keys = at ? keysOf(read(role, at)).filter((k) => k !== key) : [];
        if (keys.length > 0) dropped.push({ seat, job, keys });
        if (job === "executor") {
          if (isRecord(role.llm)) role.llm.default = key;
          else role.llm = key;
        } else {
          role[SEAT_JOB_FIELDS[job]] = key;
          if (isRecord(role.llm)) delete role.llm[job];
        }
      } else if (!on && had) {
        const at = writtenAt(role, job);
        if (!at) continue;
        write(role, at, chainValue(keysOf(read(role, at)).filter((k) => k !== key)));
      }
    }
    collapse(role);
  }

  if (next.assistant && !was.assistant) {
    const assistant = isRecord(out.assistant) ? out.assistant : (out.assistant = {});
    (assistant as Doc).llm = key;
  }
  return { doc: out as unknown as CompanyDocument, dropped };
}
