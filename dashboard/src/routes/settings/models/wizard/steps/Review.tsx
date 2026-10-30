/**
 * Everything the save makes, as one revision, in words first: the model, its
 * sign-in, where it runs, what it is used for, its limits and web tools, and
 * whether the test call ran. Then what the engine said of it, each problem
 * once with the step that fixes it, and every change in the revision behind a
 * disclosure, the one place an engine path is printed (design QA M8, M9).
 *
 * WHAT THE WRITE DROPS IS SAID (decision D3). A field the page writes holds
 * one model; a fallback written there by hand is replaced, and each one is
 * named here before it goes.
 */

import { useMemo, type ReactNode } from "react";
import {
  Callout,
  DescriptionList,
  DiffList,
  Disclosure,
  Link,
  Spinner,
  Text,
  type DiffRow,
} from "@crewlethq/ui";
import {
  DEFAULT_CLI_MAX_CONCURRENT,
  DEFAULT_CLI_TIMEOUT_SECONDS,
  SEAT_JOB_FIELDS,
} from "~/contract/models.ts";
import { href } from "~/app/router.tsx";
import { listed } from "~/lib/format.ts";
import { SEAT_JOBS, agentRoles, type Dropped, type Uses } from "~/lib/modelUses.ts";
import type { CompanyDocument, ConfigProblem, ConfigWarning } from "~/protocol/types.ts";
import { CliMark, MARK_SIZE } from "../../CliMark.tsx";
import type { ModelsData } from "../../data.ts";
import { JOB_WORDS, seatName } from "../../places.ts";
import { RUN_IN } from "../../words.ts";
import {
  CLOUD_FIELDS,
  STEP_LABELS,
  canExecute,
  credentialValues,
  fieldOf,
  messageOf,
  modelId,
  stepOf,
  type Draft,
} from "../draft.ts";
import type { WizardStep } from "../../parts.tsx";
import { webToolsWords } from "./LimitsStep.tsx";
import { seatChangesOf, summaryOf } from "./UseItFor.tsx";

/** Where the engine's check of the change stands. */
export type ReviewCheck =
  | { kind: "idle" }
  | { kind: "checking"; what: "credential" | "document" }
  | { kind: "ready"; warnings: readonly ConfigWarning[] }
  /** The engine refused the change on its problems, which the list shows. */
  | { kind: "problems" }
  /** The change cannot be made from here as it stands: a managed document, a conflict, no engine. */
  | { kind: "blocked"; message: string };

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

const show = (value: unknown) =>
  value === undefined
    ? undefined
    : typeof value === "object"
      ? JSON.stringify(value)
      : String(value);

/** One row of a diff, by whether the value appeared, went or changed. */
function row(path: string, from: string | undefined, to: string | undefined): DiffRow | null {
  if (from === to) return null;
  if (from === undefined) return { kind: "added", path, to: to ?? "" };
  if (to === undefined) return { kind: "removed", path, from };
  return { kind: "changed", path, from, to };
}

/** Every change the revision makes, with the paths the engine writes: shown only inside the disclosure. */
export function changesOf(
  before: CompanyDocument,
  after: CompanyDocument,
  key: string,
  names: (handle: string) => string,
  secrets: readonly string[],
  editing: boolean,
): DiffRow[] {
  const out: DiffRow[] = [];
  const llmOf = (doc: CompanyDocument) =>
    isRecord(doc.providers) && isRecord(doc.providers.llm) ? doc.providers.llm : {};
  const was = isRecord(llmOf(before)[key]) ? (llmOf(before)[key] as Record<string, unknown>) : {};
  const now = isRecord(llmOf(after)[key]) ? (llmOf(after)[key] as Record<string, unknown>) : {};
  for (const field of new Set([...Object.keys(was), ...Object.keys(now)])) {
    const r = row(`providers.llm.${key}.${field}`, show(was[field]), show(now[field]));
    if (r) out.push(r);
  }
  const order = (doc: CompanyDocument) =>
    show(isRecord(doc.providers) ? doc.providers.llm_order : undefined);
  const orderRow = row("providers.llm_order", order(before), order(after));
  if (orderRow) out.push(orderRow);
  const beforeRoles = agentRoles(before as unknown as Record<string, unknown>);
  for (const [handle, role] of agentRoles(after as unknown as Record<string, unknown>)) {
    const old = beforeRoles.get(handle) ?? {};
    for (const field of new Set(["llm", ...SEAT_JOBS.map((j) => SEAT_JOB_FIELDS[j])])) {
      const r = row(`${names(handle)}: ${field}`, show(old[field]), show(role[field]));
      if (r) out.push(r);
    }
  }
  const assistantLlm = (doc: CompanyDocument) =>
    show(isRecord(doc.assistant) ? doc.assistant.llm : undefined);
  const assistantRow = row("assistant.llm", assistantLlm(before), assistantLlm(after));
  if (assistantRow) out.push(assistantRow);
  for (const name of secrets) {
    out.push({
      kind: editing ? "changed" : "added",
      path: `secret ${name}`,
      to: "stored after its test passes",
    });
  }
  return out;
}

/** "QA's reviewing also named sonnet as a fallback; this removes it." */
export function droppedWords(dropped: Dropped, names: (handle: string) => string): string {
  const many = dropped.keys.length > 1;
  return `${names(dropped.seat)}'s ${JOB_WORDS[dropped.job]} also named ${listed(dropped.keys)} as ${
    many ? "fallbacks" : "a fallback"
  }; this removes ${many ? "them" : "it"}.`;
}

/** The other models a change leaves with no use at all. */
export function orphansOf(data: Pick<ModelsData, "models">, draft: Draft): string[] {
  return data.models
    .filter((m) => m.key !== draft.key)
    .filter((m) => {
      const jobs = SEAT_JOBS.flatMap((j) => m.uses.jobs[j].map((seat) => [j, seat] as const));
      if (jobs.length === 0 && !m.uses.assistant) return false;
      const seatsMoved = jobs.every(([j, seat]) => draft.uses.jobs[j].includes(seat));
      return seatsMoved && (!m.uses.assistant || draft.uses.assistant);
    })
    .map((m) => m.key);
}

export function Review({
  draft,
  data,
  before,
  after,
  dropped,
  editing,
  served,
  keptCredential,
  problems,
  check,
  testRow,
  onGo,
}: {
  draft: Draft;
  data: Pick<ModelsData, "models" | "seats" | "assistant">;
  /** The document the change is made on, and the one it makes; null before the document is read. */
  before: CompanyDocument | null;
  after: CompanyDocument | null;
  dropped: readonly Dropped[];
  editing: boolean;
  /** What the model is used for today: none for a model being added. */
  served: Uses;
  keptCredential: boolean;
  problems: readonly ConfigProblem[];
  check: ReviewCheck;
  /** The Test call row: its words and, where it has not passed, the way to run it. */
  testRow: ReactNode;
  onGo: (step: WizardStep, problem: ConfigProblem) => void;
}) {
  const names = (h: string) => seatName(data.seats, h);
  const secrets = Object.keys(credentialValues(draft));
  const rows = useMemo(
    () => (before && after ? changesOf(before, after, draft.key, names, secrets, editing) : []),
    // `names` and `secrets` follow the draft and the seats.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [before, after, draft, data.seats, editing],
  );
  const orphans = orphansOf(data, draft);
  const changes = seatChangesOf(data, draft.key, served, draft.uses);
  const stored = (what: string) =>
    keptCredential ? (
      `${what}, the stored one`
    ) : (
      <>
        {what}, stored as <span className="mono">{draft.variable}</span>
      </>
    );
  const method = {
    "api-key": stored("API key"),
    token: stored("Subscription token"),
    cloud: CLOUD_FIELDS[draft.cloud].label,
    "host-login": editing
      ? "CLI login on the host"
      : "CLI login on the host, made after it is saved",
    device: editing
      ? "ChatGPT sign-in with a device code"
      : "ChatGPT sign-in with a device code, right after it is saved",
    unchanged: "As Configuration sets it",
  }[draft.method];
  const id = modelId(draft);

  return (
    <div className="models-stack">
      {/* WHO CHANGES MODEL FIRST, above every other summary: an edit made for a
          sign-in moved two seats, and said so only inside a summary line. */}
      {changes.length > 0 ? (
        <Callout variant="warning" title="Who uses a model changes from today">
          <ul className="models-changes" aria-label="Seats that change model">
            {changes.map((line) => (
              <li key={line}>{line}</li>
            ))}
          </ul>
        </Callout>
      ) : editing ? (
        <Text as="p" variant="caption" tone="secondary">
          No seat changes model.
        </Text>
      ) : null}
      <div className="models-review-head">
        <CliMark agent={draft.agent} size={MARK_SIZE} named tooltip />
        <span className="col models-review-head__names">
          <Text as="span" variant="body" className="mono models-clamp" title={draft.key}>
            {draft.key || "New model"}
          </Text>
          <Text
            as="span"
            variant="caption"
            tone="secondary"
            className={id ? "mono models-clamp" : undefined}
            title={id || undefined}
          >
            {id || "No model chosen yet"}
          </Text>
        </span>
      </div>
      <DescriptionList
        dense
        items={[
          ["Method", method] as const,
          ...(canExecute(draft)
            ? [
                [
                  "Where it runs",
                  draft.runIn ? RUN_IN[draft.runIn] : "Where the company default says",
                ] as const,
              ]
            : []),
          [
            "Used for",
            <span key="uses" className="col">
              {summaryOf(data, draft).map((line) => (
                <span key={line}>{line}</span>
              ))}
            </span>,
          ] as const,
          [
            "Limits",
            `${draft.concurrent || DEFAULT_CLI_MAX_CONCURRENT} processes at once, ${draft.timeout || DEFAULT_CLI_TIMEOUT_SECONDS} s without progress`,
          ] as const,
          ["Web tools", webToolsWords(draft.webDenied)] as const,
          ["Test call", testRow] as const,
        ]}
      />
      {dropped.length > 0 ? (
        <ul className="models-dropped">
          {dropped.map((d) => (
            <li key={`${d.seat}-${d.job}`}>
              <Text as="span" variant="body">
                {droppedWords(d, names)}
              </Text>
            </li>
          ))}
        </ul>
      ) : null}

      {problems.length > 0 ? (
        <Callout
          variant="danger"
          title={`${problems.length} ${problems.length === 1 ? "problem" : "problems"} to fix first`}
        >
          <ul className="models-problems">
            {problems.map((p, i) => {
              const step = stepOf(p, draft.key);
              return (
                <li key={`${p.path}-${i}`} className="models-problems__line">
                  <span>
                    <strong>{fieldOf(p, draft.key)}:</strong> {messageOf(p)}
                  </span>
                  <button type="button" className="t-link" onClick={() => onGo(step, p)}>
                    Fix in {STEP_LABELS[step]}
                  </button>
                </li>
              );
            })}
          </ul>
        </Callout>
      ) : check.kind === "checking" ? (
        <div className="models-waiting" role="status">
          <Spinner size="sm" />
          <Text variant="body">
            {check.what === "credential"
              ? `Testing the ${draft.method === "token" ? "token" : "key"} with the CLI…`
              : "Checking the change with the engine…"}
          </Text>
        </div>
      ) : check.kind === "blocked" ? (
        <Callout variant="warning" title="This cannot be saved from here yet">
          {check.message}
        </Callout>
      ) : check.kind === "ready" ? (
        <Callout variant="success" title="The engine accepts this change">
          {editing ? "Saving" : "Creating"} it makes one revision, applied at once on every node
          with no restart.
          {check.warnings.length > 0 ? (
            <ul className="models-problems">
              {check.warnings.map((w, i) => (
                <li key={`${w.path}-${i}`}>{messageOf(w)}</li>
              ))}
            </ul>
          ) : null}
        </Callout>
      ) : null}

      {orphans.length > 0 ? (
        <Text as="p" variant="caption" tone="secondary" className="models-orphans">
          {orphans.map((key) => (
            <span key={key}>
              The model <span className="mono">{key}</span> will not be used by anything once this
              is saved.{" "}
              <Link href={href(["settings", "models", key], { flow: "remove" })} size="inherit">
                Remove {key}
              </Link>
            </span>
          ))}
        </Text>
      ) : null}

      <Disclosure title="Every change in the revision" count={rows.length}>
        <DiffList rows={rows} layout="stacked" emptyMessage="Nothing changes yet." />
      </Disclosure>
    </div>
  );
}
