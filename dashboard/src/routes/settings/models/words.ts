/**
 * What every state on Settings › Models & Fleets is called, and the words for what a
 * model is for.
 *
 * EXHAUSTIVE over each engine enum (`contract/models.ts`, held to the engine
 * by Go gates), so a state the engine adds fails the typecheck here rather
 * than drawing as nothing. Vendor products are named as what a model RUNS
 * ("Claude Code", "Codex"); no feature of ours borrows a vendor's name.
 */

import {
  HARNESS_PRODUCTS,
  type RunnerCellName,
  type ModelAlarmKind,
  type ModelCheckName,
  type ModelHealth,
  type ModelDeviceStatus,
  type ModelLimitState,
  type ModelLimitWindow,
  type ModelSignInRoute,
} from "~/contract/models.ts";
import { listed } from "~/lib/format.ts";
import { categoryFields, type JobCategory } from "~/lib/modelUses.ts";

type Tone = "success" | "neutral" | "warning" | "danger" | "info";

/**
 * A model's one reading, as the list's dot and the page's header say it. A
 * CONDITION, never a use (design QA M14): `untested` is a model nothing has
 * called yet, so it reads "Not tested"; whether anything runs on it is Used
 * by's to say.
 */
export const HEALTH: Record<ModelHealth, { label: string; tone: Tone }> = {
  ready: { label: "Ready", tone: "success" },
  expiring: { label: "Expires soon", tone: "warning" },
  rejected: { label: "Sign-in refused", tone: "danger" },
  sitting_out: { label: "Sitting out", tone: "warning" },
  no_sign_in: { label: "Not signed in", tone: "danger" },
  untested: { label: "Not tested", tone: "neutral" },
};

/** How a model signs in on the answering node. */
export const ROUTE: Record<ModelSignInRoute, string> = {
  token: "Subscription token",
  credentials: "Harness login on the host",
  "api-key": "API key",
  env: "Cloud provider or gateway",
  "inherit-env": "The engine's own environment",
  environment: "The harness's environment",
  none: "Nothing signs it in",
};

/** Which limit a model sits out. A session or weekly limit holds every model on the plan. */
export const WINDOW: Record<ModelLimitWindow, string> = {
  session: "session limit",
  weekly: "weekly limit",
  model: "model limit",
  unknown: "usage limit",
};

/** A limit window's name when the vendor gives no length: "Session", "Weekly". */
export const LIMIT_WINDOW: Record<ModelLimitWindow, string> = {
  session: "Session",
  weekly: "Weekly",
  model: "Model",
  unknown: "Usage",
};

/** A window's state in a word, the figure of a window whose fraction is unknown. */
export const LIMIT_STATE: Record<ModelLimitState, string> = {
  ok: "OK",
  warning: "Near limit",
  reached: "Reached",
};

/** The meter's verdict for each state (`MeterState`). */
export const LIMIT_METER: Record<ModelLimitState, "ok" | "near" | "refusing"> = {
  ok: "ok",
  warning: "near",
  reached: "refusing",
};

/** What a model whose harness reports no window says. */
export const NO_LIMITS = "None reported";

/** One line of a model's test, by what it measured. */
export const CHECK: Record<ModelCheckName, string> = {
  binary: "Harness installed",
  version: "Harness version",
  "sign-in": "Signs in",
  usage: "Reports its usage",
  smoke: "Answers a call",
  shell: "Shell and files",
  web: "Web tools",
  executor: "Executor",
};

/** Where a device sign-in is, as its panel says it. */
export const DEVICE: Record<ModelDeviceStatus, { label: string; tone: Tone }> = {
  starting: { label: "Starting", tone: "neutral" },
  pending: { label: "Waiting for approval", tone: "info" },
  cancel_requested: { label: "Cancelling", tone: "neutral" },
  approved: { label: "Signed in", tone: "success" },
  failed: { label: "Sign-in failed", tone: "danger" },
  expired: { label: "Code expired", tone: "warning" },
  cancelled: { label: "Cancelled", tone: "neutral" },
};

/**
 * Each alarm's tone (design QA M15): a model that cannot sign in is the one
 * that stops work, so it is the loud one; an expiry and a limit are warnings,
 * and seats waiting on a limit are the limit's consequence, said in its tone.
 */
export const ALARM_TONE: Record<ModelAlarmKind, "danger" | "warning"> = {
  llm_sign_in_refused: "danger",
  llm_credential_expiring: "warning",
  llm_credential_expires_soon: "warning",
  llm_limit: "warning",
  llm_limit_waiting: "warning",
};

/** A category's words: its name, and its one-word name. */
interface CategoryWords {
  label: string;
  short: string;
}

/** The words with every field a non-empty literal: an empty one is `never`, which nothing satisfies. */
type Worded<T> = { [K in keyof T]: { [F in keyof T[K]]: T[K][F] extends "" ? never : T[K][F] } };

const WORDS = {
  coding: {
    label: "Coding",
    short: "Coding",
  },
  reviews: {
    label: "Reviewing and delegations",
    short: "Reviews",
  },
  routines: {
    label: "Routines and evaluations",
    short: "Routines",
  },
} as const satisfies Record<JobCategory, CategoryWords>;

/**
 * What a model is for, by category. Each label is a column's head on the seats' grid
 * and names it on a phone's card, so AN EMPTY WORD FAILS THE TYPECHECK here
 * (`Worded`) rather than drawing a column nothing names.
 */
export const CATEGORY_WORDS = WORDS satisfies Worded<typeof WORDS>;

/** A category's tooltip: the engine fields it writes. */
export function categoryTip(category: JobCategory): string {
  return `Sets ${listed(categoryFields(category))}`;
}

/** Ask Crewlet as a use of a model: company-wide, one key, no seats. */
export const ASSISTANT_USE = {
  label: "Ask Crewlet",
  tip: "Sets assistant.model. Ask Crewlet is the company's, so no seats are picked.",
} as const;

/** The Advisor: the model it explains its findings and writes its weekly summary with. */
export const ADVISOR_USE = {
  label: "Advisor",
  tip: "Sets advisor.model. The Advisor is the company's, so no seats are picked; its checks run without a model too, with no explanations.",
} as const;

/** Every seat job at once. */
export const EVERYTHING = {
  label: "Everything",
  hint: "Use this model for every seat job: coding, reviewing and delegations, and routines and evaluations.",
} as const;

/**
 * The category whose jobs have a fixed deadline, and the quiet note shown when
 * the model chosen is not a fast one. The deadlines are the engine's
 * (llm-backends.md, "Auxiliary work and latency"): the onboarding extension
 * judge has 30 seconds; the turn-start prefetch 30, condensing and the
 * reflection passes 60. A note, never a block: a slow model is cut off and
 * does less, it does not stall a turn.
 */
export const FAST_NOTE: Partial<Record<JobCategory, string>> = {
  routines: "These run on a deadline (30 to 60 s), so pick a fast model.",
};

/** Whether a model name reads as a vendor's fast tier. */
export function isFastModel(model: string): boolean {
  return /haiku|mini|nano|flash|lite/i.test(model);
}

/** A harness's product name, or its key for a custom one. */
export function product(harness: string): string {
  return HARNESS_PRODUCTS[harness] ?? harness;
}

/** "A or B", "A, B or C". */
export const either = (parts: readonly string[]) =>
  parts.length < 3 ? parts.join(" or ") : `${parts.slice(0, -1).join(", ")} or ${parts.at(-1)}`;

/** Where a model's executors run, as the wizard's choice and the model's page both name it. */
export const RUN_IN: Record<RunnerCellName, string> = {
  direct: "On this engine's host",
  container: "In a container",
};
