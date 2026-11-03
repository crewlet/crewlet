/**
 * The wizard's steps after the source, each a plain form over the draft: its
 * name, how it connects, how it signs in, who gets it, its tools' levels, the
 * test and the review. Every
 * problem drawn on a field is the engine's (`problemAt`), or a field the
 * step needs before it can go on (`missing`).
 */

import type { ReactNode } from "react";
import {
  Button,
  Callout,
  ChoiceCards,
  ChoiceList,
  Copyable,
  DescriptionList,
  Disclosure,
  FormField,
  Input,
  KeyValueRows,
  Select,
  TableTwoLineCell,
  Text,
  Textarea,
} from "@crewlethq/ui";
import { VENDORS, VendorMark, type Vendor } from "@crewlethq/icons";
import {
  BuildingComplexGlyph,
  CircleDashedGlyph,
  InfoGlyph,
  KeyGlyph,
  LogInGlyph,
  ShieldUserGlyph,
  UserGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import type {
  McpCheckAnswer,
  McpCheckedTool,
  McpOAuthClient,
  McpToolLevel,
} from "~/contract/mcp.ts";
import type { Seat } from "~/lib/seats.ts";
import { listed, plural } from "~/lib/format.ts";
import type { ConfigProblem } from "~/protocol/types.ts";
import { Testing, TestResult } from "../flows/TestDialog.tsx";
import { CLIENT_SECRET } from "../api.ts";
import { GrantPicker } from "../GrantPicker.tsx";
import { BaselineSelect, LevelSelect, RiskChips, ServerMark } from "../parts.tsx";
import { BASELINE, LEVEL, OAUTH_CLIENT, SIGN_IN, type SignInChoice } from "../words.ts";
import {
  argsOf,
  fixedValues,
  newRow,
  problemAt,
  rowsOf,
  signsInWithOAuth,
  valuesOf,
  type Draft,
  type ValueRow,
} from "./draft.ts";

type Set = (patch: Partial<Draft>) => void;

/** A field the step needs before it can go on, said on the field once Continue is pressed. */
export type Missing = Partial<
  Record<"name" | "url" | "command" | "rows" | "clientId" | "clientSecret", string>
> & {
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
  if (step === "signin" && d.transport === "http") {
    const own = d.signIn === "oauth" && d.oauthClient === "own";
    const service = d.signIn === "client_credentials";
    if ((own || service) && !d.clientId.trim())
      out.clientId = "Give the id the client was registered under at the vendor.";
    if (service && !d.secretStored && !d.clientSecret.trim())
      out.clientSecret = "Give the client's secret: a service identity signs in with it.";
  }
  return out;
}

/** Whether a step's needs are met. */
export function isComplete(m: Missing): boolean {
  return !m.name && !m.url && !m.command && !m.row && !m.clientId && !m.clientSecret;
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
                  description:
                    "One process the engine supervises, on each node that runs it. Every call goes through the tools' levels.",
                },
                {
                  value: "box",
                  label: "Each agent's coding box",
                  description:
                    "Started inside the box beside the code, which calls it directly, so a level cannot hold its calls.",
                  disabled: true,
                  disabledReason:
                    "Set for each agent in its sandbox settings, for a server whose every tool is at Always allow.",
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
const METHOD: Record<Exclude<SignInChoice, "integration">, { line: string; icon: ReactNode }> = {
  oauth: { line: "Sign in once at the vendor", icon: <LogInGlyph size="lg" /> },
  headers: { line: "A token or key", icon: <KeyGlyph size="lg" /> },
  client_credentials: { line: "A service identity", icon: <ShieldUserGlyph size="lg" /> },
  none: { line: "No sign-in", icon: <CircleDashedGlyph size="lg" /> },
};

/** The line that says what the address check or the catalogue found. */
function detectedWords(draft: Draft): string | null {
  const d = draft.detected;
  if (!d) return null;
  const what =
    d.auth === "integration"
      ? `the ${draft.catalogue?.title ?? "vendor"} integration`
      : d.auth === "oauth" && d.client
        ? `OAuth, ${OAUTH_CLIENT[d.client].noun}`
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

/**
 * The company's own client: its id, and its secret, write-only. A secret the
 * entry reads already is kept unless a new one is typed.
 */
function ClientFields({
  draft,
  set,
  problems,
  missing,
  secretHelper,
}: {
  draft: Draft;
  set: Set;
  problems: readonly ConfigProblem[];
  missing: Missing;
  secretHelper: string;
}) {
  return (
    <div className="mcp-field-group">
      <FormField
        label="Client ID"
        error={missing.clientId ?? problemAt(problems, "oauth", "client_id")}
      >
        {(f) => (
          <Input
            id={f.id}
            aria-describedby={f.describedBy}
            aria-invalid={f.invalid || undefined}
            error={f.invalid}
            className="mono"
            spellCheck={false}
            value={draft.clientId}
            onChange={(e) => set({ clientId: e.target.value })}
          />
        )}
      </FormField>
      <FormField
        label="Client secret"
        helper={secretHelper}
        error={
          missing.clientSecret ??
          problemAt(problems, "values", CLIENT_SECRET) ??
          problemAt(problems, "oauth", "client_secret")
        }
      >
        {(f) => (
          <Input
            id={f.id}
            aria-describedby={f.describedBy}
            aria-invalid={f.invalid || undefined}
            error={f.invalid}
            type="password"
            autoComplete="new-password"
            spellCheck={false}
            placeholder={draft.secretStored ? "Stored. Type a new secret to replace it" : undefined}
            value={draft.clientSecret}
            onChange={(e) => set({ clientSecret: e.target.value })}
          />
        )}
      </FormField>
    </div>
  );
}

/** The scopes asked for, space separated. */
function ScopesField({
  draft,
  set,
  problems,
}: {
  draft: Draft;
  set: Set;
  problems: readonly ConfigProblem[];
}) {
  return (
    <FormField
      label="Scopes"
      helper="Space separated. Empty asks for the server's default."
      error={problemAt(problems, "oauth", "scopes")}
    >
      {(f) => (
        <Input
          id={f.id}
          aria-describedby={f.describedBy}
          className="mono"
          spellCheck={false}
          value={draft.scopes}
          onChange={(e) => set({ scopes: e.target.value })}
        />
      )}
    </FormField>
  );
}

export function SignInStep({
  draft,
  set,
  problems,
  missing,
  editing,
  callbackUrl,
}: {
  draft: Draft;
  set: Set;
  problems: readonly ConfigProblem[];
  missing: Missing;
  editing: boolean;
  /** Where a vendor sends a person's browser back after a sign-in. */
  callbackUrl: string;
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
  // CLIENT CREDENTIALS ARE AN OAUTH GRANT, offered wherever OAuth is.
  const offers = (m: SignInChoice) =>
    !supported || supported.includes(m === "client_credentials" ? "oauth" : m);
  const vendor = draft.detected?.title ?? entry?.title ?? "The server";
  const choose = (m: SignInChoice) =>
    set({
      signIn: m,
      // AN OAUTH SIGN-IN IS THE COMPANY'S, so it runs one instance; a new
      // server an integration signs in for runs one per agent; an edit
      // otherwise keeps what its entry says.
      ...(m === "oauth" || m === "client_credentials"
        ? { shared: true }
        : editing
          ? {}
          : { shared: m !== "integration" }),
      ...(m === "headers" && draft.headers.length === 0
        ? { headers: [newRow("Authorization", { required: true })] }
        : {}),
    });
  // HOW THE ENGINE REGISTERS, as the vendor's metadata or the catalogue
  // says: a published identity only where it reads one, a registration
  // wherever it offers one, the company's own client always.
  const found = draft.detected?.auth === "oauth" ? draft.detected.client : "";
  const clientOption = (c: McpOAuthClient) => {
    const refused =
      c === "cimd" && found && found !== "cimd"
        ? `${vendor} does not read a published identity.`
        : c === "dcr" && found === "own"
          ? `${vendor} does not register clients automatically.`
          : "";
    return {
      value: c,
      label: OAUTH_CLIENT[c].label,
      detail: OAUTH_CLIENT[c].detail,
      ...(c === (found || "cimd") ? { tag: "Recommended" } : {}),
      ...(refused ? { disabled: true, disabledReason: refused } : {}),
      ...(c === "own"
        ? {
            panel: (
              <ClientFields
                draft={draft}
                set={set}
                problems={problems}
                missing={missing}
                secretHelper="Only where the client has one. Sent when the sign-in renews, never shown again."
              />
            ),
          }
        : {}),
    };
  };
  const oauthDetail = (
    <div className="mcp-stack">
      <ChoiceList
        label="OAuth client"
        value={draft.oauthClient}
        onChange={(v) => set({ oauthClient: v as McpOAuthClient })}
        options={(["cimd", "dcr", "own"] as const).map(clientOption)}
      />
      <ScopesField draft={draft} set={set} problems={problems} />
      <FormField
        as="fieldset"
        label="Callback address"
        helper={
          entry?.prerequisite
            ? `${vendor}'s administrator allowlists it first.`
            : "For a vendor that allowlists redirects."
        }
      >
        <Copyable value={callbackUrl} variant="block" monospace />
      </FormField>
    </div>
  );
  const serviceDetail = (
    <div className="mcp-stack">
      <ClientFields
        draft={draft}
        set={set}
        problems={problems}
        missing={missing}
        secretHelper="Tested by signing in with it, then sealed. Never shown again."
      />
      <ScopesField draft={draft} set={set} problems={problems} />
    </div>
  );
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
    ...(["oauth", "headers", "client_credentials", "none"] as const).map((m) => ({
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
      ...(m === "oauth" ? { detail: oauthDetail } : {}),
      ...(m === "client_credentials" ? { detail: serviceDetail } : {}),
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
      <ChoiceCards
        label="Sign-in"
        minCardWidth="sm"
        density="compact"
        value={draft.signIn}
        onChange={(v) => choose(v as SignInChoice)}
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
            <span className="mcp-lead">OAuth.</span> An administrator signs the company in once at
            the vendor, from the server's page once it is saved, and approves the access it asks
            for. The engine keeps the grant sealed and renews it, and the agents use it for the
            company. A published identity needs no registration at the vendor; registering
            automatically creates a client there at the first sign-in; your own client is one you
            registered yourself.
          </li>
          <li>
            <span className="mcp-lead">Header.</span> A token or key sent with every request. It is
            tested before it is sealed and never shown again.
            {header ? ` ${vendor} reads it from ${header.name}: ${header.format}.` : ""}
          </li>
          <li>
            <span className="mcp-lead">Client credentials.</span> A service identity with no person
            behind it, for a vendor that offers one: the engine signs in with the client's id and
            secret whenever the server needs a token.
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
  ofDraft,
}: {
  draft: Draft;
  test: TestState;
  /** The draft's signature now, so an answer for an older draft says so. */
  current: string;
  agents: readonly Seat[];
  seat: string;
  onSeat: (handle: string) => void;
  /** Whether the test is of the draft rather than of the saved server as it runs. */
  ofDraft: boolean;
}) {
  const stale = test.kind !== "idle" && test.kind !== "running" && test.for !== current;
  return (
    <div className="mcp-stack">
      {ofDraft && draft.signIn === "oauth" ? (
        <Callout variant="info" title="Tested before anyone signs in">
          A draft is never sent the company's sign-in, so its test stops where the server asks for
          one. Sign in on the server's page once it is saved.
        </Callout>
      ) : null}
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

// ---- who gets it ---------------------------------------------------------------

export function AccessStep({ draft, set }: { draft: Draft; set: Set }) {
  const entry = draft.catalogue;
  if (draft.signIn === "integration") {
    const title = entry?.title ?? "The integration";
    return (
      <Callout variant="info" title={`${title} decides which agents get it`}>
        Each agent turned on at the {title} integration, with the access it has there, gets its own
        instance of this server.{" "}
        {entry?.integration ? (
          <>
            Change who has it on the{" "}
            <a
              className="t-link prose-link"
              href={href(["company", "integrations", entry.integration], { open: "agents" })}
            >
              integration's Agents tab
            </a>
            .
          </>
        ) : (
          "Change who has it on the integration's Agents tab."
        )}
      </Callout>
    );
  }
  return (
    <>
      <section className="mcp-field-group" aria-labelledby="mcp-scope-label">
        <Text as="p" variant="label" id="mcp-scope-label">
          Connection
        </Text>
        <ChoiceCards
          label="Connection"
          minCardWidth="sm"
          density="compact"
          value={draft.shared ? "company" : "per_agent"}
          onChange={(v) => set({ shared: v === "company" })}
          options={[
            {
              value: "company",
              label: "Company-shared",
              media: <BuildingComplexGlyph size="lg" />,
              description: "One connection, signed in once",
            },
            {
              value: "per_agent",
              label: "Per agent",
              media: <UserGlyph size="lg" />,
              description: "Each agent its own instance",
              ...(signsInWithOAuth(draft)
                ? { disabled: true, disabledReason: "An OAuth sign-in is the company's." }
                : {}),
            },
          ]}
        />
      </section>
      <section className="mcp-field-group" aria-labelledby="mcp-agents-label">
        <Text as="p" variant="label" id="mcp-agents-label">
          Agents
        </Text>
        <GrantPicker
          label="Who gets it"
          value={draft.access}
          onChange={(access) => set({ access })}
        />
      </section>
    </>
  );
}

// ---- tool approvals ------------------------------------------------------------

/**
 * Each tool's level before the server is saved: the tools the draft's test
 * listed, each at the level the engine starts it at (`starting_level`) or the
 * draft's default, and the draft's own choices over them. Only a level the
 * person changed is written; every other tool is left to the default.
 */
export function ApprovalsStep({
  draft,
  set,
  listing,
  onList,
}: {
  draft: Draft;
  set: Set;
  /** The draft's test is running to list the tools. */
  listing: boolean;
  onList: () => void;
}) {
  const startOf = (t: McpCheckedTool): McpToolLevel => draft.baseline ?? t.starting_level;
  const levelOf = (t: McpCheckedTool): McpToolLevel => draft.levels[t.raw] ?? startOf(t);
  return (
    <>
      <div className="mcp-baseline">
        <BaselineSelect
          label="Default level"
          value={draft.baseline}
          onChange={(baseline) => set({ baseline })}
        />
        <Text as="p" variant="caption" tone="secondary">
          For tools without a level of their own, and tools added later.
        </Text>
      </div>
      {draft.listed.length === 0 ? (
        listing ? (
          <Testing />
        ) : (
          <div>
            <Button variant="secondary" onClick={onList}>
              List its tools
            </Button>
          </div>
        )
      ) : (
        <ul className="mcp-approvals">
          {draft.listed.map((t) => (
            <li key={t.raw}>
              <TableTwoLineCell
                value={<span className="mono">{t.name}</span>}
                detail={t.description || t.title || undefined}
              />
              <RiskChips tool={t} compact />
              <LevelSelect
                label={`Level for ${t.name}`}
                value={levelOf(t)}
                onChange={(l) => {
                  const levels = { ...draft.levels };
                  if (l === startOf(t)) delete levels[t.raw];
                  else levels[t.raw] = l;
                  set({ levels });
                }}
              />
            </li>
          ))}
        </ul>
      )}
    </>
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
  const { units, seats, except } = draft.access;
  const who =
    draft.signIn === "integration"
      ? `The agents turned on at ${draft.catalogue?.title ?? "the integration"}`
      : draft.access.every
        ? "Every agent, and any that joins"
        : units.length + seats.length === 0
          ? "No agent yet"
          : `${listed([
              ...(units.length ? [plural(units.length, "team")] : []),
              ...(seats.length ? [plural(seats.length, "agent")] : []),
            ])}${except.length ? `, ${except.length} left out` : ""}`;
  const levelOf = (t: McpCheckedTool): McpToolLevel =>
    draft.levels[t.raw] ?? draft.baseline ?? t.starting_level;
  const counts: Record<McpToolLevel, number> = { allow: 0, approve: 0, blocked: 0 };
  for (const t of draft.listed) counts[levelOf(t)] += 1;
  const tools =
    draft.listed.length > 0
      ? listed([
          ...(counts.allow ? [`${counts.allow} ${LEVEL.allow.label.toLowerCase()}`] : []),
          ...(counts.approve ? [`${counts.approve} ${LEVEL.approve.label.toLowerCase()}`] : []),
          ...(counts.blocked ? [`${counts.blocked} blocked`] : []),
        ])
      : draft.baseline
        ? `Listed once it runs, each at ${LEVEL[draft.baseline].label}`
        : `Listed once it runs: ${BASELINE.hints.detail.toLowerCase()}`;
  const unasked = draft.listed.filter((t) => t.risk === "destructive" && levelOf(t) === "allow");
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
          ["Who gets it", who],
          ["Tools", tools],
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
      {unasked.length > 0 ? (
        <Callout
          variant="warning"
          title={
            unasked.length === 1
              ? "A destructive tool runs without asking"
              : `${unasked.length} destructive tools run without asking`
          }
        >
          {listed(unasked.map((t) => t.name))} {unasked.length === 1 ? "is" : "are"} at Always
          allow. Check {unasked.length === 1 ? "its level" : "their levels"} on the approvals step
          before you save.
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
