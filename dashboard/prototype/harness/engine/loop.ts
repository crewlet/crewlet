/**
 * The integrations reconcile loop's record of each surface
 * (`integration.State`), and the passes that move it.
 *
 * THE DEMO'S LOOP RUNS WHEN THE ENGINE'S WOULD START ONE, AND AT ONCE. The
 * engine runs a pass for every surface when the company document changes
 * (`Worker.MarkStale` brings the tick forward), on its own cadence after that,
 * and when a person asks (Check now). The demo runs the first and the last,
 * immediately, and never the cadence: there is no clock behind it.
 *
 * WHAT A PASS FINDS is what the tool answers it with. Where a scenario
 * scripted that answer (`integrationStatus`), every pass hears it again, as a
 * tool that refuses a key refuses it on every pass, until a person writes the
 * surface's inputs: the engine's next pass then asks afresh. Where nothing is
 * scripted, the pass finds what the demo can derive: GitHub's from the
 * agents' own apps in the document (`githubPass.Run`), and nothing at all on
 * every other surface, there being no tool behind the demo to report a fault.
 * The findings fold into the record as the engine's `Observe` folds them.
 *
 * Where a surface's deliveries are registered is recorded too, because the
 * engine compares it with the address in force: a pass the engine runs
 * stamps the current address on the surfaces it registers itself, and a Slack
 * app's address is recorded when its form is submitted, since a person pastes
 * it into Slack.
 */

import type { ReconcileFinding } from "~/contract/integrations.ts";
import type { CompanyDocument, ConfigRole } from "~/protocol/types.ts";
import { documentOf, type DemoCompany } from "./company.ts";
import { agentRoles } from "./derive.ts";
import { classify, promote } from "./findings.ts";
import { usable } from "./refs.ts";
import {
  githubSeatAppFindings,
  githubSeatsWithNoApp,
  githubWebBase,
  ingressOf,
  PASS_KINDS,
  type GitHubSeatApp,
} from "./vendors.ts";

/** One surface's record, as the engine stores it. An empty phase is a surface no pass has reported on. */
export interface DemoSurfaceState {
  phase: string;
  actor?: string;
  detail?: string;
  action_url?: string;
  findings: ReconcileFinding[];
  attempts: number;
  last_attempt_at: string | null;
  settled_at: string | null;
  next_attempt_at: string | null;
  last_error?: string;
  /** A disconnect asked for and not finished, and whether it removes the agents' accounts too. */
  disconnecting?: boolean;
  remove_seats?: boolean;
  /** The public address this surface's registration at the tool points at. */
  endpoint?: string;
}

/** What a pass hears from a surface's tool: its report, its findings and the fault it ended in, if any. */
export interface PassAnswer {
  phase: string;
  actor: string;
  detail: string;
  action_url: string;
  findings: ReconcileFinding[];
  last_error: string;
}

/** The error a pass ends in when its surface is not configured (`integration.ErrNotConfigured`). */
export const NOT_CONFIGURED = "integration: this surface is not configured";

/** A record no pass has written yet. */
function blank(): DemoSurfaceState {
  return {
    phase: "",
    findings: [],
    attempts: 0,
    last_attempt_at: null,
    settled_at: null,
    next_attempt_at: null,
  };
}

type Block = Record<string, unknown> | undefined;

/** A surface's company block in `doc`. */
export function blockOf(doc: CompanyDocument, kind: string): Block {
  const integrations = doc.integrations as Record<string, unknown> | undefined;
  const block = integrations?.[kind];
  return block && typeof block === "object" && !Array.isArray(block)
    ? (block as Record<string, unknown>)
    : undefined;
}

/**
 * The address third-party tools reach this engine on (`config.API.WebhookBase`):
 * the node's Tier A `api.external_url`, never a key of the company document.
 */
export function publicBase(c: DemoCompany): string {
  return c.engine.external_url.trim().replace(/\/+$/, "");
}

/** The Tier A key that address is read from (`config.API.WebhookBaseKey`). */
export const PUBLIC_BASE_KEY = "api.external_url";

/** Whether a surface's pass runs at all, or ends `NOT_CONFIGURED` (each pass's own first check). */
export function passRuns(doc: CompanyDocument, kind: string): boolean {
  const block = blockOf(doc, kind);
  if (!block) return false;
  if (["datadog", "gitlab", "mattermost", "github"].includes(kind)) return block.enabled === true;
  return true;
}

/** How the loop treats a report (`Report.Outcome`). */
export function outcomeOf(phase: string, actor: string | undefined): string {
  if (phase === "ready") return "settled";
  if (phase === "disconnecting") return "waiting";
  if (actor === "admin" || actor === "operator") return "blocked";
  return "waiting";
}

/** Which of the loop's four waits a report is on (`integration.CadenceOf`). */
function cadenceOf(phase: string, actor: string | undefined): string {
  if (phase === "ready") return "settled";
  if (actor === "admin" || actor === "operator") return actor;
  return "waiting";
}

/** When the loop asks again after a report (`Schedule.Next` with the default schedule). */
function nextAfter(phase: string, actor: string | undefined, attempts: number): number {
  const doubling = (base: number, ceiling: number) => {
    let delay = base;
    for (let i = 1; i < attempts && delay < ceiling; i++) delay *= 2;
    return Math.min(delay, ceiling);
  };
  if (phase === "ready") return 10 * 60_000;
  if (actor === "admin") return doubling(15_000, 10 * 60_000);
  if (actor === "operator") return 60 * 60_000;
  return doubling(30_000, 5 * 60_000);
}

/**
 * What GitHub's pass finds about the agents' own apps (`githubPass.Run`): one
 * finding for the agents with no app recorded (`seatsWithoutApps`), then each
 * agent carrying an app block, apps or not, as `ReconcileSeatApps` walks them.
 */
function githubFindings(c: DemoCompany): ReconcileFinding[] {
  const doc = documentOf(c);
  const block = blockOf(doc, "github");
  const provisioning = block?.provisioning as Record<string, unknown> | undefined;
  const org = typeof provisioning?.org === "string" ? provisioning.org.trim() : "";
  const appOf = (role: ConfigRole) =>
    (role as { integrations?: Record<string, unknown> }).integrations?.github as
      Record<string, unknown> | undefined;
  const agents = agentRoles(doc);
  const missing = agents.filter(({ role }) => !Number(appOf(role)?.app_id)).map((a) => a.handle);
  const seats: GitHubSeatApp[] = agents.flatMap(({ handle, role }) => {
    const app = appOf(role);
    if (!app) return [];
    return [
      {
        handle,
        appId: Number(app.app_id) || 0,
        slug: typeof app.app_slug === "string" ? app.app_slug : "",
        installationId: Number(app.installation_id) || 0,
        keyResolves: usable(c, app.private_key),
      },
    ];
  });
  const none = githubSeatsWithNoApp(missing);
  return [
    ...(none ? [none] : []),
    ...githubSeatAppFindings(seats, githubWebBase(block?.url as string | undefined), org),
  ];
}

/** What a pass finds where nothing scripted the tool's answer. */
function derivedAnswer(c: DemoCompany, kind: string): PassAnswer {
  const findings = kind === "github" ? githubFindings(c) : [];
  return { ...classify(findings), findings: promote(findings), last_error: "" };
}

/**
 * Fold what a pass heard into a surface's record (`integration.Observe`): the
 * report and findings replaced, the attempt counted (a settled one resetting
 * the count), the count restarted when the wait itself changes, and the
 * address stamped where the engine registers deliveries itself.
 */
function observe(
  c: DemoCompany,
  kind: string,
  prior: DemoSurfaceState,
  told: PassAnswer,
  at: string,
): DemoSurfaceState {
  const was = cadenceOf(prior.phase, prior.actor);
  const next: DemoSurfaceState = {
    ...prior,
    phase: told.phase,
    actor: told.actor,
    detail: told.detail,
    action_url: told.action_url,
    findings: structuredClone(told.findings),
    last_error: told.last_error,
    last_attempt_at: at,
  };
  if (next.phase === "ready") {
    next.attempts = 0;
    next.settled_at = at;
  } else {
    next.attempts = prior.attempts + 1;
  }
  if (cadenceOf(next.phase, next.actor) !== was) next.attempts = Math.min(next.attempts, 1);
  if (ingressOf(kind) === "engine") next.endpoint = publicBase(c);
  next.next_attempt_at = new Date(
    Date.parse(at) + nextAfter(next.phase, next.actor, next.attempts),
  ).toISOString();
  return next;
}

/**
 * Run one pass for `kind` at `at`: the tool's scripted answer if a scenario
 * set one, else what the demo derives, folded into the record. Answers
 * `NOT_CONFIGURED` and forgets the surface when its pass does not run.
 */
export function runPass(c: DemoCompany, kind: string, at: string): "done" | typeof NOT_CONFIGURED {
  if (!passRuns(documentOf(c), kind)) {
    delete c.integrations.states[kind];
    delete c.integrations.scripted[kind];
    return NOT_CONFIGURED;
  }
  const prior = c.integrations.states[kind] ?? blank();
  const told = c.integrations.scripted[kind] ?? derivedAnswer(c, kind);
  c.integrations.states[kind] = observe(c, kind, prior, told, at);
  return "done";
}

/**
 * What a new revision sets off: a pass for every surface the loop serves,
 * which observes the configured ones and forgets the rest. A surface being
 * torn down is left to its teardown.
 */
export function afterRevision(c: DemoCompany, at: string): void {
  for (const kind of PASS_KINDS) {
    if (c.integrations.states[kind]?.disconnecting) continue;
    runPass(c, kind, at);
  }
}

/** Record the address an operator-registered surface was set up for (`Service.recordEndpoint`). */
export function recordEndpoint(c: DemoCompany, kind: string): void {
  const base = publicBase(c);
  if (ingressOf(kind) !== "operator" || !base) return;
  c.integrations.states[kind] = { ...(c.integrations.states[kind] ?? blank()), endpoint: base };
}

/** A disconnect asked for: the surface is torn down by the loop's next pass (`AskTeardown`). */
export function askTeardown(c: DemoCompany, kind: string, removeSeats: boolean): void {
  c.integrations.states[kind] = {
    ...(c.integrations.states[kind] ?? blank()),
    disconnecting: true,
    remove_seats: removeSeats,
  };
}

/**
 * Script what a surface's tool answers every pass with, and record it as if a
 * pass had just heard it (a scenario's `integrationStatus`). The bookkeeping
 * a scenario sets (attempts, times, the address) is written once; the answer
 * stands until `forgetScript`.
 */
export function reportFor(
  c: DemoCompany,
  kind: string,
  report: Partial<DemoSurfaceState>,
  at: string,
): void {
  c.integrations.scripted[kind] = {
    phase: report.phase || "ready",
    actor: report.actor ?? "",
    detail: report.detail ?? "",
    action_url: report.action_url ?? "",
    findings: structuredClone(report.findings ?? []),
    last_error: report.last_error ?? "",
  };
  const prior = c.integrations.states[kind] ?? blank();
  const next: DemoSurfaceState = { ...prior, ...report, findings: report.findings ?? [] };
  next.phase = next.phase || "ready";
  next.last_attempt_at = report.last_attempt_at ?? at;
  if (next.phase === "ready") {
    next.attempts = report.attempts ?? 0;
    next.settled_at = report.settled_at ?? at;
  } else {
    next.attempts = report.attempts ?? Math.max(1, prior.attempts);
  }
  next.next_attempt_at =
    report.next_attempt_at ??
    new Date(
      Date.parse(next.last_attempt_at) + nextAfter(next.phase, next.actor, next.attempts),
    ).toISOString();
  if (ingressOf(kind) === "engine" && next.endpoint === undefined) next.endpoint = publicBase(c);
  c.integrations.states[kind] = next;
}

/**
 * Drop a surface's scripted answer: a person wrote its inputs, or it was
 * disconnected, so the next pass asks the tool afresh.
 */
export function forgetScript(c: DemoCompany, kind: string): void {
  delete c.integrations.scripted[kind];
}
