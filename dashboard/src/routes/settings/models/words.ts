/**
 * What every state on Settings › Models is called, and the words for what a
 * model is for.
 *
 * EXHAUSTIVE over each engine enum (`contract/models.ts`, held to the engine
 * by Go gates), so a state the engine adds fails the typecheck here rather
 * than drawing as nothing. Vendor products are named as what a model RUNS
 * ("Claude Code", "Codex"); no feature of ours borrows a vendor's name.
 */

import {
  CLI_PRODUCTS,
  type RunnerCellName,
  type ModelAlarmKind,
  type ModelCheckName,
  type ModelHealth,
  type ModelLimitWindow,
  type ModelSignInRoute,
} from "~/contract/models.ts";
import type { ModelDeviceStatus } from "~/protocol/models.ts";
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
  credentials: "CLI login on the host",
  "api-key": "API key",
  env: "Cloud provider or gateway",
  "inherit-env": "The engine's own environment",
  environment: "The CLI's environment",
  none: "Nothing signs it in",
};

/** Which limit a model sits out. A session or weekly limit holds every model on the plan. */
export const WINDOW: Record<ModelLimitWindow, string> = {
  session: "session limit",
  weekly: "weekly limit",
  model: "model limit",
  unknown: "usage limit",
};

/** One line of a model's test, by what it measured. */
export const CHECK: Record<ModelCheckName, string> = {
  binary: "CLI installed",
  version: "CLI version",
  "sign-in": "Signs in",
  usage: "Reports its usage",
  smoke: "Answers a call",
  shell: "Shell and files",
  web: "Web tools",
  executor: "Coding runs",
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

/** What a model is for, by category: its name, what it covers, and the verb a summary uses. */
export const CATEGORY_WORDS: Record<JobCategory, { label: string; hint: string; verb: string }> = {
  coding: {
    label: "Coding",
    hint: "The seat's own work: writing code, running tools, and its onboarding pass.",
    verb: "code",
  },
  reviews: {
    label: "Reviewing and delegations",
    hint: "Checks a turn's result and runs the smaller jobs a seat hands off.",
    verb: "review work and run delegated tasks",
  },
  routines: {
    label: "Routines and evaluations",
    hint: "Background memory and summaries, and the checks that decide when onboarding is done.",
    verb: "run routines and evaluations",
  },
};

/** A category's tooltip: the engine fields it writes. */
export function categoryTip(category: JobCategory): string {
  return `Sets ${listed(categoryFields(category))}`;
}

/** The Dashboard Assistant as a use of a model: company-wide, one field, no seats. */
export const ASSISTANT_USE = {
  label: "Assistant",
  hint: "Answers you in the dashboard's assistant.",
  tip: "Sets assistant.llm. The assistant is the company's, so no seats are picked.",
  rule: "The assistant runs only on a model that denies the CLI's web tools, so a page it reads cannot send what it read anywhere.",
} as const;

/** Every seat job at once. */
export const EVERYTHING = {
  label: "Everything",
  hint: "Use this model for every seat job: coding, reviewing and delegations, and routines and evaluations.",
  tip: "Sets the seat's llm to this model and clears llm_review, llm_subagent, llm_auxiliary and llm_judge, so every seat job resolves to it. The Assistant is separate: choose it on its own.",
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
  routines:
    "Evaluations run under a 30 s limit and routines under 30 s and 60 s limits; a fast model is recommended.",
};

/** Whether a model name reads as a vendor's fast tier. */
export function isFastModel(model: string): boolean {
  return /haiku|mini|nano|flash|lite/i.test(model);
}

/** A CLI's product name, or its key for a custom one. */
export function product(agent: string): string {
  return CLI_PRODUCTS[agent] ?? agent;
}

/** Where a model's coding runs happen, as the wizard's choice and the model's page both name it. */
export const RUN_IN: Record<RunnerCellName, string> = {
  direct: "On this engine's host",
  container: "In a container",
  e2b: "In E2B",
};
