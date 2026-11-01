/**
 * What Settings › Models reads, and the one view of it every part of the page
 * draws: each model's condition and uses as the engine reports them
 * (`llm_entries`), joined for an admin to the model as the company document
 * writes it (`config`), which an edit starts from.
 *
 * NOTHING HERE DECIDES A CONDITION OR A USE. Whether a model is ready, which
 * seat runs which job on it and what its alarms say are the engine's answer:
 * the page names them. The document is read only for what the engine does
 * not report and an edit must keep (where it runs executors, its limits, its
 * web tools, the fields the wizard does not hold).
 *
 * KEPT CURRENT three ways, because each misses what the others catch: every
 * 15 seconds while the page is open (a limit that lifted, a test that passed
 * elsewhere), when the fleet applies a new configuration (a model added or
 * removed by anyone), and when a seat's provider limit changes on the live
 * state (a model just started sitting out).
 */

import { useCallback, useEffect, useMemo, useRef } from "react";
import { EXECUTOR_HARNESSES } from "~/contract/models.ts";
import { indexOrg, type Seat } from "~/lib/seats.ts";
import { useAgents, useEngineHealth, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { usesOf, type Uses } from "~/lib/modelUses.ts";
import type { AgentRow, CompanyDocument } from "~/protocol/types.ts";
import type {
  MemberModelEntry,
  ModelAlarm,
  ModelEntriesAnswer,
  ModelEntry,
  ModelNode,
} from "~/protocol/models.ts";

/**
 * How often an open page asks again. A sit-out lifts and a test passes on
 * another node without any event this page hears; 15 seconds keeps a lifted
 * limit from reading as current for longer than a person glances away, and is
 * one small query per open page.
 */
export const MODELS_POLL_MS = 15_000;

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

/** One model as the page draws it: the engine's reading, and for an admin the document's entry. */
export interface ModelView {
  key: string;
  harness: string;
  model: string;
  /** Whether its harness can run a seat's coding (`EXECUTOR_HARNESSES`, held to the engine). */
  executor: boolean;
  /** The admin's reading, or the member's, which names no credential. */
  entry: ModelEntry | MemberModelEntry;
  /** The admin's reading alone; null for a member. */
  admin: ModelEntry | null;
  /** What runs on it now, from the engine's `used_by`. */
  uses: Uses;
  /** Where it runs executors, as written; null for the company default. */
  runIn: string | null;
  timeoutSeconds: number | null;
  maxConcurrent: number | null;
  webDenied: boolean;
  /** The document entry as written, for an edit. Null for a member, who reads no document. */
  raw: Record<string, unknown> | null;
}

/** The page's whole reading. */
export interface ModelsData {
  /** Whether the reader holds admin: the full reading, the document, and every write. */
  admin: boolean;
  /** The active document, for an admin; null for a member or while loading. */
  doc: CompanyDocument | null;
  models: ModelView[];
  /** Every agent seat, for names and pictures. */
  seats: Seat[];
  /** The five model alarms as this node last evaluated them; empty for a member. */
  alarms: ModelAlarm[];
  /** The live nodes, and whether each reported its models. */
  nodes: ModelNode[];
  /** The node that answered. */
  node: string;
  /** Whether the Dashboard Assistant is on, and the model it runs on. */
  assistant: { enabled: boolean; key: string | null };
  loading: boolean;
  error: string | null;
  /** Ask everything again now, after a write this page made. */
  reload: () => void;
}

/** The model the assistant runs on, from the document an admin reads or the uses the engine reports. */
function assistantOf(doc: CompanyDocument | null, models: readonly ModelView[]) {
  const assistant =
    doc && isRecord((doc as Record<string, unknown>).assistant)
      ? ((doc as Record<string, unknown>).assistant as Record<string, unknown>)
      : null;
  const fromUses = models.find((m) => m.uses.assistant)?.key ?? null;
  if (!assistant) return { enabled: fromUses !== null, key: fromUses };
  const llm = assistant.llm;
  const key =
    typeof llm === "string"
      ? llm
      : Array.isArray(llm) && typeof llm[0] === "string"
        ? llm[0]
        : fromUses;
  return { enabled: assistant.enabled === true, key: key || null };
}

/** The models, joined: the engine's reading in its order, each with its document entry. */
export function modelViews(
  answer: ModelEntriesAnswer | null,
  doc: CompanyDocument | null,
): ModelView[] {
  const llm =
    doc && isRecord(doc.providers) && isRecord(doc.providers.llm)
      ? (doc.providers.llm as Record<string, unknown>)
      : null;
  return (answer?.entries ?? []).map((entry) => {
    const raw =
      llm && isRecord(llm[entry.key]) ? (llm[entry.key] as Record<string, unknown>) : null;
    const overrides = raw && isRecord(raw.overrides) ? raw.overrides : {};
    return {
      key: entry.key,
      harness: entry.harness,
      model: entry.model,
      executor: (EXECUTOR_HARNESSES as readonly string[]).includes(entry.harness),
      entry,
      admin: answer?.reading === "admin" ? (entry as ModelEntry) : null,
      uses: usesOf(entry.used_by),
      runIn: typeof raw?.run_in === "string" ? raw.run_in : null,
      timeoutSeconds: typeof raw?.timeout_seconds === "number" ? raw.timeout_seconds : null,
      maxConcurrent: typeof raw?.max_concurrent === "number" ? raw.max_concurrent : null,
      webDenied: overrides.web_tools === "denied",
      raw,
    };
  });
}

/** A key that moves whenever any seat's provider limit does: the live state's signal that a model began or stopped sitting out. */
function limitsKey(agents: readonly AgentRow[]): string {
  return agents
    .map((a) =>
      a.provider_limit ? `${a.id}:${a.provider_limit.since}:${a.provider_limit.parked}` : "",
    )
    .filter(Boolean)
    .sort()
    .join("|");
}

export function useModelsData(): ModelsData {
  const viewer = useViewer();
  const admin = viewer.admin;
  const entries = useQuery("llm_entries", undefined, {
    pollMs: MODELS_POLL_MS,
    refetchOnFocus: true,
  });
  const config = useQuery("config", undefined, { enabled: admin });
  const org = useOrg();
  const health = useEngineHealth();
  const agents = useAgents();

  // A NEW CONFIGURATION APPLIED, OR A LIMIT THAT MOVED, is asked about at once.
  const epoch = health?.applied_epoch ?? 0;
  const limits = limitsKey(agents);
  const { refetch: refetchEntries } = entries;
  const { refetch: refetchConfig } = config;
  const seen = useRef({ epoch, limits });
  useEffect(() => {
    const was = seen.current;
    seen.current = { epoch, limits };
    if (was.epoch !== epoch) {
      refetchEntries();
      if (admin) refetchConfig();
    } else if (was.limits !== limits) {
      refetchEntries();
    }
  }, [epoch, limits, admin, refetchEntries, refetchConfig]);

  const seats = useMemo(() => indexOrg(org).seats.filter((s) => s.kind !== "human"), [org]);
  const doc = admin ? ((config.data as CompanyDocument | null) ?? null) : null;
  const answer = entries.data;
  const models = useMemo(() => modelViews(answer, doc), [answer, doc]);
  const assistant = useMemo(() => assistantOf(doc, models), [doc, models]);
  const reload = useCallback(() => {
    refetchEntries();
    if (admin) refetchConfig();
  }, [admin, refetchEntries, refetchConfig]);

  return {
    admin,
    doc,
    models,
    seats,
    alarms: answer?.reading === "admin" ? (answer.alarms ?? []) : [],
    nodes: answer?.nodes ?? [],
    node: answer?.node ?? "",
    assistant,
    loading: (entries.loading && !entries.data) || (admin && config.loading && !config.data),
    error: entries.error
      ? (entries.detail ?? entries.error)
      : config.error
        ? (config.detail ?? config.error)
        : null,
    reload,
  };
}
