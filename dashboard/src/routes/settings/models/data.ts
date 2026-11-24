/**
 * What Settings › Models & Fleets reads, and the one view of it every part of the page
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
 * elsewhere), when this node applies a new configuration (every question is
 * asked again then, `Store.applyHealth`), and when a seat's provider limit
 * changes on the live state (a model just started sitting out).
 */

import { useCallback, useEffect, useMemo, useRef } from "react";
import { indexOrg, type Seat } from "~/lib/seats.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { usesOf, type Uses } from "~/lib/modelUses.ts";
import { nameKey, useModelNames } from "./modelNames.ts";
import type { AgentRow, CompanyDocument, LogRefusal, QueryRefusal } from "~/protocol/types.ts";
import type { FleetsAnswer } from "~/protocol/fleets.ts";
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

/** No name for any model: each is said by its id. */
const NO_NAMES: ReadonlyMap<string, string> = new Map();
const NO_ENTRIES: readonly ModelEntriesAnswer["entries"][number][] = [];
const NO_EXECUTORS: readonly string[] = [];

/** One model as the page draws it: the engine's reading, and for an admin the document's entry. */
export interface ModelView {
  key: string;
  harness: string;
  model: string;
  /** Its model as a person reads it ("Claude Opus 5.5"), or its id where nothing names it (`modelNames.ts`). */
  modelName: string;
  /** Whether its harness can run a seat's coding: one of the engine's `executor_harnesses`. */
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
  /** The reasoning effort the entry sets, or null for the harness's default. */
  effort: string | null;
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
  /** The LLM fleets and what each seat and helper runs on; null until the engine answers. */
  fleets: FleetsAnswer | null;
  /** The live nodes, and whether each reported its models. */
  nodes: ModelNode[];
  /** The node that answered. */
  node: string;
  /** The model Ask Crewlet runs on, or null where none is assigned and Ask Crewlet is off. */
  assistant: { key: string | null };
  /** The model the Advisor explains its findings with, or null where it has none and runs its checks alone. */
  advisor: { key: string | null };
  /**
   * The harnesses the engine runs a seat's executor on, in the profiles' order
   * (`executor_harnesses`): served rather than compiled in, so a runner a build
   * adds needs no dashboard rebuild. Empty until the engine answers.
   */
  executors: readonly string[];
  loading: boolean;
  error: string | null;
  /**
   * The read that failed, whole, for the page's shared failed-read state
   * (`QueryState`): its code, its refusal and the engine's own sentence,
   * never the bare code drawn as words.
   */
  failed: {
    error: string;
    refusal: QueryRefusal | LogRefusal | null;
    detail?: string;
  } | null;
  /** Ask everything again now, after a write this page made. */
  reload: () => void;
}

/**
 * Ask Crewlet's model, from the document an admin reads or, for a member who
 * reads none, the place the engine reports its chat at (`used_by`).
 */
export function assistantOf(
  doc: CompanyDocument | null,
  models: readonly Pick<ModelView, "key" | "entry">[],
): ModelsData["assistant"] {
  if (doc) {
    const assistant = isRecord((doc as Record<string, unknown>).assistant)
      ? ((doc as Record<string, unknown>).assistant as Record<string, unknown>)
      : null;
    return { key: typeof assistant?.model === "string" ? assistant.model : null };
  }
  const reported = models.find((m) => m.entry.used_by.some((u) => u.assistant === "chat"));
  return { key: reported?.key ?? null };
}

/**
 * The Advisor's model, from the document an admin reads or, for a member who
 * reads none, the place the engine reports its findings at (`used_by`).
 */
export function advisorOf(
  doc: CompanyDocument | null,
  models: readonly Pick<ModelView, "key" | "entry">[],
): ModelsData["advisor"] {
  if (doc) {
    const advisor = isRecord((doc as Record<string, unknown>).advisor)
      ? ((doc as Record<string, unknown>).advisor as Record<string, unknown>)
      : null;
    return {
      key: typeof advisor?.model === "string" && advisor.model !== "" ? advisor.model : null,
    };
  }
  const reported = models.find((m) => m.entry.used_by.some((u) => u.advisor === "findings"));
  return { key: reported?.key ?? null };
}

/** The models, joined: the engine's reading in its order, each with its document entry. */
export function modelViews(
  answer: ModelEntriesAnswer | null,
  doc: CompanyDocument | null,
  names: ReadonlyMap<string, string> = NO_NAMES,
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
      modelName: names.get(nameKey(entry.harness, entry.model)) ?? entry.model,
      executor: answer?.executor_harnesses.includes(entry.harness) ?? false,
      entry,
      admin: answer?.reading === "admin" ? (entry as ModelEntry) : null,
      uses: usesOf(entry.used_by),
      runIn: typeof raw?.run_in === "string" ? raw.run_in : null,
      timeoutSeconds: typeof raw?.timeout_seconds === "number" ? raw.timeout_seconds : null,
      maxConcurrent: typeof raw?.max_concurrent === "number" ? raw.max_concurrent : null,
      effort: typeof raw?.effort === "string" ? raw.effort : null,
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
  const fleets = useQuery("llm_fleets", undefined, {
    pollMs: MODELS_POLL_MS,
    refetchOnFocus: true,
  });
  const org = useOrg();
  const agents = useAgents();

  // A LIMIT THAT MOVED is asked about at once; a configuration applied re-reads
  // every question (`Store.applyHealth`).
  const limits = limitsKey(agents);
  const { refetch: refetchEntries } = entries;
  const { refetch: refetchConfig } = config;
  const { refetch: refetchFleets } = fleets;
  const seen = useRef(limits);
  useEffect(() => {
    if (seen.current === limits) return;
    seen.current = limits;
    refetchEntries();
  }, [limits, refetchEntries]);

  const seats = useMemo(() => indexOrg(org).seats.filter((s) => s.kind !== "human"), [org]);
  const doc = admin ? ((config.data as CompanyDocument | null) ?? null) : null;
  const answer = entries.data;
  const names = useModelNames(answer?.entries ?? NO_ENTRIES, admin);
  const models = useMemo(() => modelViews(answer, doc, names), [answer, doc, names]);
  const assistant = useMemo(() => assistantOf(doc, models), [doc, models]);
  const advisor = useMemo(() => advisorOf(doc, models), [doc, models]);
  const reload = useCallback(() => {
    refetchEntries();
    refetchFleets();
    if (admin) refetchConfig();
  }, [admin, refetchEntries, refetchFleets, refetchConfig]);

  const failed = failedOf(entries, fleets, config);
  return {
    admin,
    doc,
    models,
    seats,
    alarms: answer?.reading === "admin" ? (answer.alarms ?? []) : [],
    fleets: fleets.data ?? null,
    nodes: answer?.nodes ?? [],
    node: answer?.node ?? "",
    assistant,
    advisor,
    executors: answer?.executor_harnesses ?? NO_EXECUTORS,
    loading:
      (entries.loading && !entries.data) ||
      (fleets.loading && !fleets.data) ||
      (admin && config.loading && !config.data),
    error: failed ? (failed.detail ?? failed.error) : null,
    failed,
    reload,
  };
}

/** The first of the page's reads that failed, whole, or null where none did. */
function failedOf(
  ...reads: {
    error: string | null;
    refusal: QueryRefusal | LogRefusal | null;
    detail?: string | undefined;
  }[]
): ModelsData["failed"] {
  const read = reads.find((r) => r.error);
  if (!read?.error) return null;
  return {
    error: read.error,
    refusal: read.refusal,
    ...(read.detail ? { detail: read.detail } : {}),
  };
}
