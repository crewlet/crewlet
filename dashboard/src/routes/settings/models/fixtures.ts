/**
 * Settings › Models' readings as the engine sends them, for every suite on
 * the page: a model's `llm_entries` row, built from the fields a case names,
 * and the page's whole reading (`ModelsData`) joined from those rows by the
 * page's own joiner (`modelViews`), so a suite draws exactly what a reader
 * would.
 *
 * ONE COMPANY: three agent seats whose display names differ from their
 * handles (a surface that printed a handle where a name belongs fails on
 * sight), at one instant, `MODELS_NOW`, which a suite pins its clock to.
 */

import { indexOrg, type Seat } from "~/lib/seats.ts";
import type { ModelJob } from "~/contract/models.ts";
import type {
  MemberModelEntry,
  ModelAlarm,
  ModelEntriesAnswer,
  ModelEntry,
  ModelSignIn,
  ModelUse,
} from "~/protocol/models.ts";
import type { CompanyDocument, OrgProjection } from "~/protocol/types.ts";
import { modelViews, type ModelsData } from "~/routes/settings/models/data.ts";
import { withDerived } from "~/test/org.ts";

/** The instant every model reading here is taken at. */
export const MODELS_NOW = Date.parse("2026-10-29T12:00:00Z");

/** An instant `minutes` from `MODELS_NOW` (negative for the past), as the wire writes it. */
export const at = (minutes: number) => new Date(MODELS_NOW + minutes * 60_000).toISOString();

/** The company: three agents, named apart from their handles. */
export const MODELS_ORG = withDerived({
  name: "Acme",
  roles: [
    { name: "Backend Engineer", handle: "backend-engineer" },
    { name: "Product Manager", handle: "pm" },
    { name: "Quality Lead", handle: "qa" },
  ],
} as OrgProjection);

/** The company's agent seats, as the page reads them. */
export const MODELS_SEATS: Seat[] = indexOrg(MODELS_ORG).seats.filter((s) => s.kind !== "human");

/** Every seat job, in the engine's order. */
export const EVERY_JOB: ModelJob[] = ["executor", "review", "subagent", "auxiliary", "judge"];

/** `seat` runs `jobs` on the model: position 0, written by the seat. */
export function runs(seat: string, ...jobs: ModelJob[]): ModelUse[] {
  return jobs.map((job) => ({ seat, job, position: 0, written: true, source: "seat" as const }));
}

/** The Dashboard Assistant chats on the model. */
export const ASSISTANT_CHAT: ModelUse = { assistant: "chat", position: 0 };

/** A sign-in on a stored subscription token, unless the case says otherwise. */
export function signIn(over: Partial<ModelSignIn> = {}): ModelSignIn {
  return {
    route: "token",
    mode: "subscription",
    variable: "CLAUDE_CODE_OAUTH_TOKEN",
    source: "store",
    resolves: true,
    hint: "sk-ant-oat01-...Qm4x",
    stored_at: at(-60 * 24 * 30),
    stored_by: "jane",
    expires_at: null,
    expiry_basis: null,
    host_logins: [],
    ...over,
  };
}

/** A model's admin reading: a ready Claude Code model on a token, nothing on it, unless the case says otherwise. */
export function modelEntry(over: Partial<ModelEntry> & { key: string }): ModelEntry {
  return {
    harness: "claude-code",
    model: "claude-sonnet-5",
    health: "ready",
    plan: "",
    sign_in: signIn(),
    sitting_out: null,
    last_success: at(-5),
    last_failure: null,
    waiting: null,
    usage: { days: 7, calls: 0, total_tokens: 0 },
    used_by: [],
    device_sign_in: null,
    ...over,
  };
}

/** What a member reads of the same model: nothing that names a credential, a plan or a cost. */
export function memberEntry(e: ModelEntry): MemberModelEntry {
  return {
    key: e.key,
    harness: e.harness,
    model: e.model,
    health: e.health,
    sign_in: { route: e.sign_in.route, resolves: e.sign_in.resolves },
    sitting_out: e.sitting_out
      ? {
          until: e.sitting_out.until,
          resets_at: e.sitting_out.resets_at,
          window: e.sitting_out.window,
          since: e.sitting_out.since,
          nodes: e.sitting_out.nodes,
        }
      : null,
    last_success: e.last_success,
    waiting: e.waiting,
    used_by: e.used_by,
  };
}

/**
 * A model signed in with a stored API key, named for its key
 * (`ANTHROPIC_API_KEY_<KEY>`) and stored on a known date by a known person,
 * for the suites that read a credential's name and age.
 */
export function apiKeyEntry(over: Partial<ModelEntry> & { key: string }): ModelEntry {
  return modelEntry({
    model: "sonnet",
    sign_in: signIn({
      route: "api-key",
      mode: "api-key",
      variable: `ANTHROPIC_API_KEY_${over.key.toUpperCase()}`,
      hint: "sk-ant-...wxyz",
      stored_at: "2026-10-01T10:00:00Z",
      stored_by: "founder",
    }),
    last_success: "2026-10-29T09:00:00Z",
    ...over,
  });
}

/** A seat as the org index draws it, for a suite that names its own seats. */
export function seat(handle: string, name: string): Seat {
  return { key: handle, handle, name, kind: "agent", avatar: null } as unknown as Seat;
}

/** A company document holding `llm`'s entries as written, for the reading an admin joins. */
export function modelsDoc(llm: Record<string, Record<string, unknown>>): CompanyDocument {
  return { name: "Acme", providers: { llm } } as CompanyDocument;
}

/**
 * The page's whole reading, as `useModelsData` hands it to every part: an
 * admin's (each row joined to `doc`'s entry) or a member's.
 */
export function modelsData({
  entries,
  alarms = [],
  admin = true,
  doc = null,
  assistant,
  seats = MODELS_SEATS,
  reload = () => {},
}: {
  entries: ModelEntry[];
  alarms?: ModelAlarm[];
  admin?: boolean;
  doc?: CompanyDocument | null;
  assistant?: { enabled: boolean; key: string | null };
  seats?: Seat[];
  reload?: () => void;
}): ModelsData {
  const answer: ModelEntriesAnswer = admin
    ? {
        reading: "admin",
        node: "node-1",
        nodes: [{ id: "node-1", reported: true }],
        entries,
        alarms,
      }
    : {
        reading: "member",
        node: "node-1",
        nodes: [{ id: "node-1", reported: true }],
        entries: entries.map(memberEntry),
        alarms: null,
      };
  const models = modelViews(answer, admin ? doc : null);
  const runsAssistant = models.find((m) => m.uses.assistant)?.key ?? null;
  return {
    admin,
    doc: admin ? doc : null,
    models,
    seats,
    alarms: admin ? alarms : [],
    nodes: answer.nodes,
    node: answer.node,
    assistant: assistant ?? { enabled: runsAssistant !== null, key: runsAssistant },
    loading: false,
    error: null,
    reload,
  };
}
