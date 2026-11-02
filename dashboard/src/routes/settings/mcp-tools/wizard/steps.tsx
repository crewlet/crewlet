/**
 * The wizard's steps after the source, each a plain form over the draft: its
 * name, how it connects, how it signs in, the test and the review. Every
 * problem drawn on a field is the engine's (`problemAt`), or a field the
 * step needs before it can go on (`missing`).
 */

import type { ReactNode } from "react";
import {
  Callout,
  ChoiceCards,
  DescriptionList,
  Disclosure,
  FormField,
  Input,
  KeyValueRows,
  Select,
  Text,
  Textarea,
} from "@crewlethq/ui";
import { VENDORS, VendorMark, type Vendor } from "@crewlethq/icons";
import { CircleDashedGlyph, InfoGlyph, KeyGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import type { McpAuthMethod, McpCheckAnswer } from "~/contract/mcp.ts";
import type { Seat } from "~/lib/seats.ts";
import { listed, plural } from "~/lib/format.ts";
import type { ConfigProblem } from "~/protocol/types.ts";
import { Testing, TestResult } from "../flows/TestDialog.tsx";
import { ServerMark } from "../parts.tsx";
import { SIGN_IN } from "../words.ts";
import {
  argsOf,
  fixedValues,
  newRow,
  problemAt,
  rowsOf,
  valuesOf,
  type Draft,
  type ValueRow,
} from "./draft.ts";

type Set = (patch: Partial<Draft>) => void;

/** A field the step needs before it can go on, said on the field once Continue is pressed. */
export type Missing = Partial<Record<"name" | "url" | "command" | "rows", string>> & {
  /** Per row, by its id. */
  row?: Record<string, string>;
};

/** What each step needs before it goes on: present fields, never a judgement of them. */
export function missingIn(d: Draft, step: string): Missing {
  const out: Missing = {};
  if (step === "name" && !d.name.trim()) out.name = "Give the server a name.";
  if (step === "connection" && d.transport === "http" && !d.url.trim())
    out.url = "Give the server's address.";
  if (step === "connection" && d.transport === "stdio" && !d.command.trim())
    out.command = "Give the command that starts the server.";
  const rowsStep = d.transport === "stdio" ? "connection" : "signin";
  if (step === rowsStep && (d.transport === "stdio" || d.signIn === "headers")) {
    const row: Record<string, string> = {};
    for (const r of rowsOf(d)) {
      const word = d.transport === "http" ? "header" : "variable";
      if (!r.key.trim() && r.value.trim()) row[r.id] = `Name the ${word} this value is sent as.`;
      else if (r.key.trim() && !r.stored && !r.value.trim() && r.required)
        row[r.id] = `Give ${r.key.trim()} a value, or remove it.`;
    }
    if (Object.keys(row).length > 0) out.row = row;
  }
  return out;
}

/** Whether a step's needs are met. */
export function isComplete(m: Missing): boolean {
  return !m.name && !m.url && !m.command && !m.row;
}

// ---- name ----------------------------------------------------------------------

export function NameStep({
  draft,
  set,
  editing,
  problems,
  missing,
}: {
  draft: Draft;
  set: Set;
  editing: boolean;
  problems: readonly ConfigProblem[];
  missing: Missing;
}) {
  return (
    <>
      <FormField
        label="Name"
        helper={
          editing ? "A saved server keeps its name." : "Lowercase letters, digits and hyphens."
        }
        error={missing.name ?? problemAt(problems, "name")}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid || undefined}
            error={field.invalid}
            className="mono"
            value={draft.name}
            readOnly={editing}
            placeholder="linear"
            autoComplete="off"
            spellCheck={false}
            onChange={(e) => set({ name: e.target.value })}
          />
        )}
      </FormField>
      <FormField label="What it is for" error={problemAt(problems, "description")}>
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid || undefined}
            error={field.invalid}
            value={draft.description}
            onChange={(e) => set({ description: e.target.value })}
          />
        )}
      </FormField>
    </>
  );
}

// ---- the keys a server is sent -------------------------------------------------

/**
 * The headers or variables the server is sent, each value typed write-only:
 * a stored one says so and is kept unless a new one is typed.
 */
function ValueRows({
  draft,
  set,
  problems,
  missing,
}: {
  draft: Draft;
  set: Set;
  problems: readonly ConfigProblem[];
  missing: Missing;
}) {
  const http = draft.transport === "http";
  const word = http ? "Header" : "Variable";
  return (
    <KeyValueRows<ValueRow>
      label={http ? "Request headers" : "Environment"}
      // The column heads name the list; its legend is read, not drawn twice.
      hideLabel
      keyLabel={word}
      value={rowsOf(draft)}
      onChange={(rows) => set(http ? { headers: rows } : { env: rows })}
      // A key added here is one the server is sent: it takes a value, or is removed.
      createRow={() => newRow("", { required: true })}
      secret
      keyReadOnly={(r) => r.stored}
      valuePlaceholder={(r) =>
        r.stored
          ? "Stored. Type to replace it"
          : r.format || (r.required ? "Sealed when you save" : "Optional")
      }
      keyError={(r) => problemAt(problems, "rows", r.key.trim()) ?? missing.row?.[r.id]}
      helper="Each value is tested and sealed when you save, and never shown again."
    />
  );
}

// ---- connection ----------------------------------------------------------------

export function ConnectionStep({
  draft,
  set,
  problems,
  missing,
}: {
  draft: Draft;
  set: Set;
  problems: readonly ConfigProblem[];
  missing: Missing;
}) {
  // EVERY GROUP OF CARDS UNDER THE FORM'S LABEL, as the fields beside them are.
  return (
    <>
      <section className="mcp-field-group" aria-labelledby="mcp-transport-label">
        <Text as="p" variant="label" id="mcp-transport-label">
          How it connects
        </Text>
        <ChoiceCards
          label="How it connects"
          minCardWidth="sm"
          density="compact"
          value={draft.transport}
          onChange={(v) => set({ transport: v as Draft["transport"] })}
          options={[
            {
              value: "http",
              label: "Remote address",
              description: "A server the vendor runs, reached over HTTPS.",
            },
            {
              value: "stdio",
              label: "Local command",
              description: "A program the engine starts on its own host.",
            },
          ]}
        />
      </section>
      {draft.transport === "http" ? (
        <FormField
          label="Address"
          helper="https only. Credentials go in a header, never in the address."
          error={missing.url ?? problemAt(problems, "url")}
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid || undefined}
              error={field.invalid}
              className="mono"
              value={draft.url}
              placeholder="https://mcp.example.com/mcp"
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => set({ url: e.target.value })}
            />
          )}
        </FormField>
      ) : (
        <>
          <FormField label="Command" error={missing.command ?? problemAt(problems, "command")}>
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                aria-invalid={field.invalid || undefined}
                error={field.invalid}
                className="mono"
                value={draft.command}
                autoComplete="off"
                spellCheck={false}
                onChange={(e) => set({ command: e.target.value })}
              />
            )}
          </FormField>
          <FormField label="Arguments" helper="One per line." error={problemAt(problems, "args")}>
            {(field) => (
              <Textarea
                id={field.id}
                aria-describedby={field.describedBy}
                aria-invalid={field.invalid || undefined}
                rows={3}
                className="mono"
                value={draft.args}
                onChange={(e) => set({ args: e.target.value })}
              />
            )}
          </FormField>
          {draft.signIn !== "integration" || rowsOf(draft).length > 0 ? (
            <ValueRows draft={draft} set={set} problems={problems} missing={missing} />
          ) : null}
          <section className="mcp-field-group" aria-labelledby="mcp-runs-label">
            <Text as="p" variant="label" id="mcp-runs-label">
              Where it runs
            </Text>
            <ChoiceCards
              label="Where it runs"
              minCardWidth="sm"
              density="compact"
              value="host"
              onChange={() => undefined}
              options={[
                {
                  value: "host",
                  label: "The engine host",
                  description: "One process the engine supervises, on each node that runs it.",
                },
                {
                  value: "box",
                  label: "Each agent's coding box",
                  description: "Started inside the box beside the code, which calls it directly.",
                  disabled: true,
                  disabledReason: "Set for each agent in its sandbox settings.",
                },
              ]}
            />
          </section>
        </>
      )}
      <FormField
        label="Tool prefix"
        helper="Optional. Keeps two servers' tool names apart."
        error={problemAt(problems, "tool_prefix")}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid || undefined}
            error={field.invalid}
            className="mono"
            value={draft.toolPrefix}
            placeholder="sentry_"
            autoComplete="off"
            spellCheck={false}
            onChange={(e) => set({ toolPrefix: e.target.value })}
          />
        )}
      </FormField>
    </>
  );
}

// ---- sign-in ---------------------------------------------------------------------

const isVendor = (v: string): v is Vendor => (VENDORS as readonly string[]).includes(v);

/** The methods this step offers, each a card: what a person picks between. */
const METHOD: Record<"headers" | "none", { line: string; icon: ReactNode }> = {
  headers: { line: "A token or key", icon: <KeyGlyph size="lg" /> },
  none: { line: "No sign-in", icon: <CircleDashedGlyph size="lg" /> },
};

/** The line that says what the address check or the catalogue found. */
function detectedWords(draft: Draft): string | null {
  const d = draft.detected;
  if (!d) return null;
  const what =
    d.auth === "integration"
      ? `the ${draft.catalogue?.title ?? "vendor"} integration`
      : SIGN_IN[d.auth].noun;
  return `${d.from === "server" ? "Detected from the server" : "From the catalogue"}: ${what}`;
}

/** Where a server an integration signs in for is given to agents. */
function IntegrationNote({ draft }: { draft: Draft }) {
  const entry = draft.catalogue;
  if (!entry?.integration) return null;
  const keys = [...entry.headers, ...entry.env]
    .filter((i) => i.from_integration)
    .map((i) => i.name);
  return (
    <Callout variant="info" title={`${entry.title} decides which agents get it`}>
      Each agent turned on at the {entry.title} integration gets its own instance, signed in as
      itself{keys.length > 0 ? ` with the ${listed(keys)} the integration keeps for it` : ""}.
      Nothing is stored here. Change who has it on the{" "}
      <a
        className="t-link prose-link"
        href={href(["company", "integrations", entry.integration], { open: "agents" })}
      >
        {entry.title} integration's Agents tab
      </a>
      .
    </Callout>
  );
}

export function SignInStep({
  draft,
  set,
  problems,
  missing,
  editing,
}: {
  draft: Draft;
  set: Set;
  problems: readonly ConfigProblem[];
  missing: Missing;
  editing: boolean;
}) {
  const entry = draft.catalogue;
  if (draft.transport === "stdio") {
    return draft.signIn === "integration" ? (
      <IntegrationNote draft={draft} />
    ) : (
      <Text as="p" tone="secondary">
        A local server signs in with the variables it starts with, set on the previous step.
      </Text>
    );
  }
  const supported = draft.supported;
  const offers = (m: McpAuthMethod) => !supported || supported.includes(m);
  const vendor = draft.detected?.title ?? entry?.title ?? "The server";
  const choose = (m: McpAuthMethod) =>
    set({
      signIn: m,
      // A new server an integration signs in for runs one instance per agent; an edit keeps what it says.
      ...(editing ? {} : { shared: m !== "integration" }),
      ...(m === "headers" && draft.headers.length === 0
        ? { headers: [newRow("Authorization", { required: true })] }
        : {}),
    });
  const methods = [
    ...(entry?.integration
      ? [
          {
            value: "integration",
            label: entry.title,
            tag: "Recommended",
            media: isVendor(entry.vendor) ? (
              <VendorMark vendor={entry.vendor} size="lg" />
            ) : (
              <ServerMark server={entry} size="lg" />
            ),
            description: "Each agent signs in as itself",
          },
        ]
      : []),
    ...(["headers", "none"] as const).map((m) => ({
      value: m,
      label: SIGN_IN[m].label,
      media: METHOD[m].icon,
      description: METHOD[m].line,
      ...(offers(m)
        ? {}
        : {
            disabled: true,
            disabledReason:
              m === "none"
                ? `${vendor} asks for a sign-in.`
                : `${vendor} does not offer ${SIGN_IN[m].noun}.`,
          }),
    })),
  ];
  const detected = detectedWords(draft);
  const header = entry?.headers.find((h) => !h.from_integration && h.format);
  return (
    <>
      {detected ? (
        <p className="mcp-slim-note is-info">
          <InfoGlyph size="sm" />
          <span>{detected}</span>
        </p>
      ) : null}
      {draft.detected?.auth === "oauth" ? (
        <Callout variant="warning" title={`${vendor} asks for an OAuth sign-in`}>
          This build does not offer one. Give it a header it accepts, or its test stops at the
          sign-in.
        </Callout>
      ) : null}
      <ChoiceCards
        label="Sign-in"
        minCardWidth="sm"
        density="compact"
        value={draft.signIn === "oauth" ? null : draft.signIn}
        onChange={(v) => choose(v as McpAuthMethod)}
        options={methods}
      />
      {draft.signIn === "integration" ? <IntegrationNote draft={draft} /> : null}
      {draft.signIn === "headers" ? (
        <ValueRows draft={draft} set={set} problems={problems} missing={missing} />
      ) : null}
      <Disclosure title="How this works" size="compact" headingLevel="none" flush>
        <ul className="mcp-list mcp-howto">
          {entry?.integration ? (
            <li>
              <span className="mcp-lead">{entry.title}.</span> Each agent turned on at the{" "}
              {entry.title} integration gets its own instance, signed in with the credential the
              integration keeps for it. Nothing is stored here.
            </li>
          ) : null}
          <li>
            <span className="mcp-lead">Header.</span> A token or key sent with every request. It is
            tested before it is sealed and never shown again.
            {header ? ` ${vendor} reads it from ${header.name}: ${header.format}.` : ""}
          </li>
          {entry?.prerequisite ? (
            <li>
              <span className="mcp-lead">Before it works.</span> {entry.prerequisite}
            </li>
          ) : null}
        </ul>
      </Disclosure>
    </>
  );
}

// ---- test --------------------------------------------------------------------------

/** Where the wizard's test stands. */
export type TestState =
  | { kind: "idle" }
  | { kind: "running"; for: string }
  | { kind: "done"; for: string; answer: McpCheckAnswer }
  | { kind: "refused"; for: string; message: string };

export function TestStep({
  draft,
  test,
  current,
  agents,
  seat,
  onSeat,
}: {
  draft: Draft;
  test: TestState;
  /** The draft's signature now, so an answer for an older draft says so. */
  current: string;
  agents: readonly Seat[];
  seat: string;
  onSeat: (handle: string) => void;
}) {
  const stale = test.kind !== "idle" && test.kind !== "running" && test.for !== current;
  return (
    <div className="mcp-stack">
      {!draft.shared ? (
        <FormField
          label="Test as"
          helper="A per-agent server runs as one agent, with its own credentials."
        >
          {(field) => (
            <Select
              id={field.id}
              value={seat}
              disabled={test.kind === "running"}
              options={agents.map((s) => ({ value: s.handle, label: s.name }))}
              onChange={(v) => onSeat(String(v))}
            />
          )}
        </FormField>
      ) : null}
      {!draft.shared && agents.length === 0 ? (
        <Callout variant="info" title="No agent to test it as">
          A per-agent server runs as one agent, and the company has none yet. Add the server, and
          test it from its page once an agent is given it.
        </Callout>
      ) : null}
      {test.kind === "running" ? <Testing /> : null}
      {stale ? (
        <Text as="p" variant="caption" tone="secondary">
          The server has changed since this test. Test again to try it as it is now.
        </Text>
      ) : null}
      {test.kind === "refused" ? (
        <Callout variant="danger" title="The test did not run">
          {test.message}
        </Callout>
      ) : null}
      {test.kind === "done" ? <TestResult answer={test.answer} /> : null}
    </div>
  );
}

// ---- review ------------------------------------------------------------------------

/** Where the review's check of the draft stands. */
export type ReviewCheck =
  | { kind: "idle" }
  | { kind: "checking" }
  | { kind: "accepted" }
  | { kind: "problems" }
  | { kind: "blocked"; message: string };

export function ReviewStep({
  draft,
  problems,
  check,
  testRow,
  editing,
  partial,
}: {
  draft: Draft;
  problems: readonly ConfigProblem[];
  check: ReviewCheck;
  testRow: ReactNode;
  editing: boolean;
  /** A save that wrote the entry and stopped at a value, in a sentence; null otherwise. */
  partial: string | null;
}) {
  const rows = rowsOf(draft).filter((r) => r.key.trim());
  const typed = Object.keys(valuesOf(draft));
  const fixed = editing ? [] : Object.entries(fixedValues(draft.catalogue));
  const where =
    draft.transport === "http" ? draft.url : [draft.command, ...argsOf(draft.args)].join(" ");
  const signIn =
    draft.signIn === "integration"
      ? `${draft.catalogue?.title ?? "The integration"}, as each agent`
      : draft.transport === "stdio" || draft.signIn === "headers"
        ? rows.length > 0
          ? `${listed(rows.map((r) => r.key.trim()))}${
              typed.length === 0
                ? ", as stored"
                : typed.length === rows.length
                  ? ", sealed when you save"
                  : `, ${listed(typed)} sealed when you save`
            }`
          : "No credential"
        : SIGN_IN[draft.signIn].label;
  return (
    <div className="mcp-stack">
      {/* THE SERVER FIRST, as the Models review opens with the entry it
          saves: its mark, its name and where it is reached. The stepper above
          is the way back to a step, so a row carries no Change of its own. */}
      <div className="mcp-review-head">
        <ServerMark
          server={{ vendor: draft.catalogue?.vendor ?? "", transport: draft.transport }}
        />
        <span className="mcp-two">
          <Text as="span" variant="body" className="mono" truncate>
            {draft.name || "Not named"}
          </Text>
          <Text as="span" variant="caption" tone="secondary" className="mono" truncate>
            {where.trim() || "No address yet"}
          </Text>
        </span>
      </div>
      <DescriptionList
        dense
        items={[
          [draft.transport === "http" ? "Sign-in" : "Variables", signIn],
          [
            "Instances",
            draft.shared
              ? "One for the company"
              : draft.signIn === "integration"
                ? `One for each agent ${draft.catalogue?.title ?? "the integration"} is turned on for`
                : "One for each agent it is given to",
          ],
          ...(fixed.length > 0
            ? ([
                [
                  "Set by the catalogue",
                  <span className="mono">{fixed.map(([k, v]) => `${k}=${v}`).join(", ")}</span>,
                ],
              ] as const)
            : []),
          ["Test", testRow],
        ]}
      />
      {check.kind === "problems" && problems.length > 0 ? (
        <Callout variant="danger" title="The engine refused this">
          <ul className="mcp-list">
            {problems.map((p) => (
              <li key={`${p.path}:${p.message}`}>{p.message}</li>
            ))}
          </ul>
        </Callout>
      ) : null}
      {check.kind === "blocked" ? (
        <Callout variant="danger" title="Nothing can be saved yet">
          {check.message}
        </Callout>
      ) : null}
      {partial ? (
        <Callout variant="warning" title="Saved, with a value still to store">
          {partial} Change it on its step and save again: only what is left is sent.
        </Callout>
      ) : check.kind === "accepted" ? (
        <Callout
          variant="success"
          title={editing ? "The engine accepts this change" : "The engine accepts this server"}
        >
          {editing
            ? "Saving writes one revision, applied with no restart; a running turn finishes first."
            : "Adding it writes one revision. The engine starts it with no restart, and the agents it reaches see its tools on their next turn."}
          {typed.length > 0
            ? ` Then ${plural(typed.length, "value")} ${typed.length === 1 ? "is" : "are"} tested and sealed, each in a revision of its own.`
            : ""}
        </Callout>
      ) : null}
    </div>
  );
}
