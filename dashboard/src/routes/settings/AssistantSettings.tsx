/**
 * Settings › Ask Crewlet: every setting of the company document's `assistant`
 * block, each changed where it is read through the shared setting editor.
 */

import {
  ArrangeList,
  Callout,
  Card,
  Checkbox,
  EmptyState,
  PageNote,
  Select,
  SettingsTable,
  Switch,
} from "@crewlethq/ui";
import {
  BrainGlyph,
  GaugeGlyph,
  WandSparklesGlyph,
  SquareTerminalGlyph,
  WrenchGlyph,
} from "@crewlethq/icons/glyphs";
import type { ReactNode } from "react";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { LimitSetting } from "~/components/LimitSetting.tsx";
import { SettingEditor, type Drawn, type Typed } from "~/components/SettingEditor.tsx";
import { listed } from "~/lib/format.ts";
import { documentLimit } from "~/lib/limits.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import { useModelsData, type ModelView } from "./models/data.ts";

const AREA = "Ask Crewlet";
const REREAD = "Reload to see Ask Crewlet's settings as they are now.";

/** The tool families `assistant.tools.deny` takes, in the engine's order, with what each turns off. */
export const TOOL_FAMILIES: readonly { key: string; label: string; description: string }[] = [
  { key: "writes", label: "Every change", description: "Ask Crewlet only reads and answers." },
  {
    key: "generic_changes",
    label: "Changes to any API route",
    description: "The change it prepares where no tool of its own covers the record.",
  },
  {
    key: "generic_reads",
    label: "Reads of any API route",
    description: "The read it makes where no tool of its own covers the record.",
  },
  {
    key: "chat_writes",
    label: "Chat messages and rooms",
    description: "Posting, replying, editing and room changes.",
  },
  {
    key: "atlas_writes",
    label: "Atlas entries",
    description: "Adding, correcting and retiring entries.",
  },
  {
    key: "agent_controls",
    label: "Agents' runs",
    description: "Pausing, resuming, steering and answering an agent's run.",
  },
  {
    key: "page_changes",
    label: "Changes to the open screen",
    description: "Filters and drafts on the page.",
  },
  {
    key: "deep_work",
    label: "Deep work",
    description: "Handing a large task to a run with its own tools.",
  },
];

/** The merge patch that writes `value` at a dotted `key`, or removes it for null. */
export function patchAt(key: string, value: unknown): Record<string, unknown> {
  let patch: unknown = value;
  for (const segment of key.split(".").reverse()) patch = { [segment]: patch };
  return patch as Record<string, unknown>;
}

/** A chain as the document writes it, read as keys. */
export function chainOf(value: unknown): string[] {
  if (typeof value === "string") return value ? [value] : [];
  return Array.isArray(value) ? value.filter((k): k is string => typeof k === "string") : [];
}

/** The chain after `key` is turned on (last) or off, the rest in order. */
export function toggled(chain: readonly string[], key: string, on: boolean): string[] {
  const rest = chain.filter((k) => k !== key);
  return on ? [...rest, key] : rest;
}

/** One `assistant` setting read off the document and changed with the shared editor. */
function DocumentSetting<V>({
  doc,
  settingKey,
  label,
  current,
  fallback,
  words,
  helper,
  check,
  write,
  field,
  onApplied,
}: {
  doc: CompanyDocument;
  settingKey: string;
  label: string;
  /** The value in force, read off the document. */
  current: V;
  /** The value an unwritten key means. */
  fallback: V;
  words: (value: V) => string;
  helper: ReactNode;
  /** Why a value cannot be saved, or null. */
  check?: (value: V) => string | null;
  /** What to write at the key: the value, or null to remove it. */
  write: (value: V) => unknown;
  field: (props: { id: string; value: V; set: (next: V) => void; busy: boolean }) => ReactNode;
  onApplied: () => void;
}) {
  const same = (a: V, b: V) => JSON.stringify(a) === JSON.stringify(b);
  return (
    <SettingEditor<V, V>
      label={label}
      settingKey={settingKey}
      answer={doc}
      settable
      reread={REREAD}
      onApplied={onApplied}
      draw={(saved): Drawn => {
        const value = saved ? saved.value : current;
        return {
          value: words(value),
          source: same(value, fallback) ? "Default" : `Changed from ${words(fallback)}`,
          changed: !same(value, fallback),
        };
      }}
      edit={{
        open: () => current,
        read: (value): Typed<V> => {
          const error = check?.(value) ?? null;
          return error === null ? { ok: true, value } : { ok: false, error };
        },
        same: (value) => same(value, current),
        sameWords: "This is the setting already.",
        unchangedWords: "Already the setting.",
        plan: (value) => (document) => {
          const held = documentLimit(document, settingKey);
          const next = write(value);
          if (JSON.stringify(held ?? null) === JSON.stringify(next ?? null)) return null;
          return {
            patch: patchAt(settingKey, next),
            summary: `${AREA}: ${label} set to ${words(value)} (${settingKey})`,
          };
        },
        served: (value) => same(value, current),
        helper,
        field: ({ id, state, set, busy }) => field({ id, value: state, set, busy }),
      }}
    />
  );
}

/** The models a chain can take, its members first in order, as ArrangeList items. */
function chainItems(chain: readonly string[], eligible: readonly ModelView[]) {
  const byKey = new Map(eligible.map((m) => [m.key, m]));
  const keys = [
    ...chain.filter((k) => byKey.has(k)),
    ...eligible.map((m) => m.key).filter((k) => !chain.includes(k)),
  ];
  return keys.map((key) => ({
    key,
    label: key,
    description: <span className="mono">{byKey.get(key)!.model}</span>,
    shown: chain.includes(key),
  }));
}

/** A model chain edited as an ordered list: on is in the chain, the order is the chain's. */
function ChainField({
  id,
  label,
  value,
  set,
  eligible,
}: {
  id: string;
  label: string;
  value: string[];
  set: (next: string[]) => void;
  eligible: readonly ModelView[];
}) {
  if (eligible.length === 0) {
    return <p id={id}>No model denies web tools yet. Add one under Settings, Models.</p>;
  }
  return (
    <div id={id}>
      <ArrangeList
        label={label}
        toggle="checkbox"
        items={chainItems(value, eligible)}
        onReorder={(keys) => set(keys.filter((k) => value.includes(k)))}
        onShownChange={(key, on) => set(toggled(value, key, on))}
      />
    </div>
  );
}

const onOff = (on: boolean) => (on ? "On" : "Off");
const chainWords = (chain: readonly string[]) =>
  chain.length === 0
    ? "None"
    : chain.length === 1
      ? chain[0]!
      : `${chain[0]}, then ${listed(chain.slice(1))}`;

export function AssistantSettings() {
  const data = useModelsData();
  const limits = useQuery("limits");
  const doc = data.doc;
  const refetch = () => {
    data.reload();
    limits.refetch();
  };
  const bounds = (limits.data?.limits ?? []).filter((r) => r.group === "assistant");
  const turnBounds = bounds.filter((r) => !r.key.startsWith("assistant.deep_work."));
  const deepBounds = bounds.filter((r) => r.key.startsWith("assistant.deep_work."));
  const read = (key: string) => (doc ? documentLimit(doc, key) : undefined);
  // ONLY A MODEL THAT DENIES WEB TOOLS can answer: the engine refuses any other.
  const eligible = data.models.filter((m) => m.webDenied);
  const deepModels = data.models.filter((m) => m.harness === "claude-code");
  const enabled = read("assistant.enabled") === true;
  const chain = chainOf(read("assistant.llm"));
  const strong = chainOf(read("assistant.deliberate.llm"));
  const deny = chainOf(read("assistant.tools.deny"));
  const deep =
    typeof read("assistant.deep_work.llm") === "string"
      ? (read("assistant.deep_work.llm") as string)
      : "";

  return (
    <div className="col gap-4">
      <QueryState
        error={data.failed?.error ?? limits.error}
        refusal={data.failed?.refusal ?? limits.refusal}
        loading={(data.loading && !doc) || (limits.loading && !limits.data)}
      >
        {doc === null ? (
          <EmptyState
            title="Only an admin reads Ask Crewlet's settings"
            description="They are read off the company document."
          />
        ) : (
          <>
            <Card padding="none">
              <Card.Header
                icon={<WandSparklesGlyph size="sm" />}
                subtitle="Who answers, and on which models"
              >
                <Card.Title as="h3" id="assistant-models">
                  Ask Crewlet
                </Card.Title>
              </Card.Header>
              <SettingsTable aria-labelledby="assistant-models">
                <DocumentSetting<boolean>
                  doc={doc}
                  settingKey="assistant.enabled"
                  label="Turned on"
                  current={enabled}
                  fallback={false}
                  words={onOff}
                  helper="Off hides Ask for everybody. Conversations are kept on their usual retention."
                  write={(on) => on}
                  onApplied={refetch}
                  field={({ id, value, set, busy }) => (
                    <Switch
                      id={id}
                      label="Ask Crewlet is on"
                      checked={value}
                      disabled={busy}
                      onCheckedChange={set}
                    />
                  )}
                />
                <DocumentSetting<string[]>
                  doc={doc}
                  settingKey="assistant.llm"
                  label="Models"
                  current={chain}
                  fallback={[]}
                  words={chainWords}
                  helper="The first answers; each next one takes over when the one before is spent or cannot sign in."
                  check={(next) =>
                    enabled && next.length === 0 ? "Ask Crewlet is on, so it needs a model." : null
                  }
                  write={(next) => (next.length === 0 ? null : next)}
                  onApplied={refetch}
                  field={({ id, value, set }) => (
                    <ChainField
                      id={id}
                      label="Models Ask Crewlet answers with"
                      value={value}
                      set={set}
                      eligible={eligible}
                    />
                  )}
                />
              </SettingsTable>
              <Card.Body padding="md">
                <PageNote>
                  Each person's token budget for Ask Crewlet is set on{" "}
                  <a className="t-link prose-link" href={href(["company", "budgets"])}>
                    Company › Budgets
                  </a>
                  .
                </PageNote>
              </Card.Body>
            </Card>

            <Card padding="none">
              <Card.Header
                icon={<BrainGlyph size="sm" />}
                subtitle="Think harder, one question at a time"
              >
                <Card.Title as="h3" id="assistant-deliberate">
                  A stronger model for hard questions
                </Card.Title>
              </Card.Header>
              <SettingsTable aria-labelledby="assistant-deliberate">
                <DocumentSetting<string[]>
                  doc={doc}
                  settingKey="assistant.deliberate.llm"
                  label="Stronger models"
                  current={strong}
                  fallback={[]}
                  words={chainWords}
                  helper="Offered as Think harder in the drawer. None turns Think harder off."
                  write={(next) => (next.length === 0 ? null : next)}
                  onApplied={refetch}
                  field={({ id, value, set }) => (
                    <ChainField
                      id={id}
                      label="Stronger models"
                      value={value}
                      set={set}
                      eligible={eligible}
                    />
                  )}
                />
                {strong.length > 0 ? (
                  <DocumentSetting<boolean>
                    doc={doc}
                    settingKey="assistant.deliberate.decision_briefs"
                    label="Decision briefs"
                    current={read("assistant.deliberate.decision_briefs") === true}
                    fallback={false}
                    words={onOff}
                    helper="Brief me on this decision runs on the stronger models too."
                    write={(on) => (on ? true : null)}
                    onApplied={refetch}
                    field={({ id, value, set, busy }) => (
                      <Switch
                        id={id}
                        label="Briefs of a decision think harder"
                        checked={value}
                        disabled={busy}
                        onCheckedChange={set}
                      />
                    )}
                  />
                ) : null}
              </SettingsTable>
            </Card>

            <Card padding="none">
              <Card.Header icon={<GaugeGlyph size="sm" />} subtitle="What one turn may take">
                <Card.Title as="h3" id="assistant-turns">
                  Turns
                </Card.Title>
              </Card.Header>
              <SettingsTable aria-labelledby="assistant-turns">
                {turnBounds.map((cap) => (
                  <LimitSetting
                    key={cap.key}
                    cap={cap}
                    label={BOUND_LABELS[cap.key] ?? cap.key}
                    area={AREA}
                    reread={REREAD}
                    onApplied={refetch}
                  />
                ))}
              </SettingsTable>
            </Card>

            <Card padding="none">
              <Card.Header
                icon={<WrenchGlyph size="sm" />}
                subtitle="It only narrows what each person's permissions offer"
              >
                <Card.Title as="h3" id="assistant-tools">
                  Tools turned off
                </Card.Title>
              </Card.Header>
              <SettingsTable aria-labelledby="assistant-tools">
                <DocumentSetting<string[]>
                  doc={doc}
                  settingKey="assistant.tools.deny"
                  label="Turned off for everybody"
                  current={deny}
                  fallback={[]}
                  words={(families) =>
                    families.length === 0
                      ? "None"
                      : listed(
                          families.map((f) => TOOL_FAMILIES.find((t) => t.key === f)?.label ?? f),
                        )
                  }
                  helper="A family turned off is never offered to the model, in any turn."
                  write={(families) => (families.length === 0 ? null : families)}
                  onApplied={refetch}
                  field={({ id, value, set, busy }) => (
                    <div id={id} role="group" className="col gap-2">
                      {TOOL_FAMILIES.map((family) => (
                        <Checkbox
                          key={family.key}
                          label={family.label}
                          description={family.description}
                          checked={value.includes(family.key)}
                          disabled={busy}
                          onCheckedChange={(on) =>
                            set(
                              TOOL_FAMILIES.map((f) => f.key).filter((k) =>
                                k === family.key ? on : value.includes(k),
                              ),
                            )
                          }
                        />
                      ))}
                    </div>
                  )}
                />
              </SettingsTable>
            </Card>

            <Card padding="none">
              <Card.Header
                icon={<SquareTerminalGlyph size="sm" />}
                subtitle="A large task, handed to a run with its own tools"
              >
                <Card.Title as="h3" id="assistant-deep">
                  Deep work
                </Card.Title>
              </Card.Header>
              <Card.Body padding="md">
                <Callout variant="warning">
                  A deep-work run's harness keeps its own web tools, so it can reach any web host.
                  Quick answers reach none.
                </Callout>
              </Card.Body>
              <SettingsTable aria-labelledby="assistant-deep">
                <DocumentSetting<string>
                  doc={doc}
                  settingKey="assistant.deep_work.llm"
                  label="Model"
                  current={deep}
                  fallback=""
                  words={(key) => key || "Off"}
                  helper="A Claude Code model. Off answers in quick mode only."
                  write={(key) => (key ? key : null)}
                  onApplied={refetch}
                  field={({ id, value, set, busy }) => (
                    <Select
                      id={id}
                      ariaLabel="Model deep work runs on"
                      value={value}
                      disabled={busy}
                      options={[
                        { value: "", label: "Off" },
                        ...deepModels.map((m) => ({
                          value: m.key,
                          label: m.key,
                          description: m.model,
                        })),
                      ]}
                      onChange={(v) => set(String(v))}
                    />
                  )}
                />
                {deep
                  ? deepBounds.map((cap) => (
                      <LimitSetting
                        key={cap.key}
                        cap={cap}
                        label={BOUND_LABELS[cap.key] ?? cap.key}
                        area={AREA}
                        reread={REREAD}
                        onApplied={refetch}
                      />
                    ))
                  : null}
              </SettingsTable>
            </Card>
          </>
        )}
      </QueryState>
    </div>
  );
}

/** What each bound is called on this page. */
const BOUND_LABELS: Readonly<Record<string, string>> = {
  "assistant.max_steps": "Model rounds per turn",
  "assistant.turn_timeout_seconds": "Longest turn",
  "assistant.proposal_ttl_seconds": "How long a change waits for approval",
  "assistant.retention_days": "Conversations kept",
  "assistant.deep_work.timeout_seconds": "Longest run",
  "assistant.deep_work.max_steps": "Rounds and calls per task",
  "assistant.deep_work.max_tokens": "Tokens per task",
};
