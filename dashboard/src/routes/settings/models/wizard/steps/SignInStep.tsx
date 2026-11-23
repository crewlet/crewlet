/**
 * How the model proves who it is to the vendor: the methods its harness offers,
 * the one allowed on any engine first, and what each needs.
 *
 * A STORED CREDENTIAL IS SHOWN AS STORED, never read back. An edit keeps it
 * until the reader asks to replace it; a replacement is tested before it is
 * stored, and the stored one signs the model in until then. A deep link that
 * replaces a key opens the field empty and quiet (design QA M5): it says what
 * will happen, and says what is wrong only once Continue or Save is pressed.
 */

import { Callout, ChoiceList, Copyable, FormField, Select, Text, ValueField } from "@crewlethq/ui";
import type { ReactNode } from "react";
import { listed } from "~/lib/format.ts";
import { HostLogin } from "../../HostLogin.tsx";
import type { ModelView } from "../../data.ts";
import { product } from "../../words.ts";
import {
  CLOUD_FIELDS,
  cloudsOf,
  credentialWord,
  factsOf,
  methodsFor,
  pastesCredential,
  providersFor,
  type Cloud,
  type Draft,
  type Method,
} from "../draft.ts";
import { credentialExample, ENDPOINT_EXAMPLE } from "../examples.ts";
import { AgoTime } from "~/components/AgoTime.tsx";

export const METHOD_WORDS: Record<Method, { label: string; detail: string }> = {
  "api-key": {
    label: "API key",
    detail: "A key from the vendor's console, billed per token. Allowed on any engine.",
  },
  cloud: {
    label: "Cloud provider or gateway",
    detail:
      "Amazon Bedrock, Google Vertex AI, Microsoft Foundry, or your own LLM gateway, under that provider's agreement.",
  },
  "host-login": {
    label: "Harness login on the host",
    detail:
      "You sign the harness in yourself in a terminal on the engine host. This page shows whether it is signed in.",
  },
  token: {
    label: "Subscription token",
    detail: "A token you make with claude setup-token on your own computer.",
  },
  device: {
    label: "ChatGPT sign-in with a device code",
    detail:
      "You approve a one-time code on OpenAI's site. Uses your ChatGPT plan's included usage.",
  },
  unchanged: {
    label: "As Configuration sets it",
    detail: "This model's sign-in is written in Configuration, and the wizard keeps it as it is.",
  },
};

/** A method in the words of the draft's harness, where they differ from the general ones. */
export function methodWords(harness: string, method: Method): { label: string; detail: ReactNode } {
  const facts = factsOf(harness);
  if (method === "api-key" && facts.providerKeys)
    return {
      label: "A provider's API key",
      detail: `A key from the model provider ${product(harness)} calls, billed per token. Allowed on any engine.`,
    };
  if (method === "api-key" && harness === "copilot")
    return {
      label: "GitHub token",
      detail: "A GitHub token with Copilot access. Allowed on any engine.",
    };
  if (method === "api-key" && harness === "qwen-code")
    return {
      label: "API key",
      detail:
        "A key for an OpenAI-compatible endpoint, such as DashScope's. Allowed on any engine.",
    };
  if (method === "cloud" && harness === "gemini-cli")
    return {
      label: "Google Vertex AI",
      detail:
        "Your Google Cloud project's Vertex AI, on its API key, under Google Cloud's agreement.",
    };
  if (method === "host-login" && harness === "qwen-code")
    return {
      label: "Qwen subscription login",
      detail: (
        <>
          You sign in to your Qwen Plus or Pro subscription with <span className="mono">qwen</span>{" "}
          yourself on the engine host, and the model uses that login. Not measured against Qwen's
          service.
        </>
      ),
    };
  if (method === "host-login" && facts.login === "adopt")
    return {
      label: METHOD_WORDS["host-login"].label,
      detail: (
        <>
          You sign in with <span className="mono">{facts.binary}</span> yourself on the engine host,
          and the model uses that login. This page shows whether it is signed in.
        </>
      ),
    };
  return METHOD_WORDS[method];
}

/** What the credential field says, by whether it is new or replaces a stored one. */
function credentialHelp(d: Draft, replacing: boolean): string {
  const word = credentialWord(d);
  return replacing
    ? `Paste the new ${word}. It is tested before it replaces the stored one.`
    : `Tested before it is stored. Never shown again.`;
}

export function SignInStep({
  draft,
  editing,
  keptCredential,
  replacing,
  shared,
  storedCloud,
  errors,
  onChange,
  onVariable,
  onReplace,
  onChecked,
}: {
  draft: Draft;
  /** The model being edited; null while adding one. */
  editing: ModelView | null;
  /** Whether the stored key or token stands (an edit, the method and secret unchanged, not replacing). */
  keptCredential: boolean;
  replacing: boolean;
  /** The other models that read the same secret, which a replaced credential changes too. */
  shared: readonly string[];
  /** The cloud secrets stored now, by field. */
  storedCloud: Readonly<Record<string, string>>;
  errors: {
    credential?: string | undefined;
    variable?: string | undefined;
    env?: string | undefined;
  };
  onChange: (patch: Partial<Draft>) => void;
  onVariable: (variable: string) => void;
  onReplace: (replacing: boolean) => void;
  onChecked: () => void;
}) {
  const methods = methodsFor(draft);
  const word = credentialWord(draft);
  const providers = providersFor(draft.harness);
  // OPENCODE'S AND PI'S MODEL NAMES ITS PROVIDER, so a login's model needs one too.
  const namesItsProvider = draft.harness === "opencode" || draft.harness === "pi";
  const facts = factsOf(draft.harness);
  // A REVOKED KEY IS NOT ONE TO KEEP (design QA Minor 4): the option to go
  // back to it is offered only while the stored one still signs in.
  const canKeep = !!editing && editing.entry.health !== "rejected";
  const hostLogins = editing?.admin?.sign_in.host_logins ?? [];

  return (
    <>
      <ChoiceList
        label="Method"
        value={draft.method}
        onChange={(v) => onChange({ method: v as Method })}
        options={methods.map((m) => ({
          value: m,
          label: methodWords(draft.harness, m).label,
          detail: methodWords(draft.harness, m).detail,
        }))}
      />

      {providers.length > 0 && (draft.method === "api-key" || namesItsProvider) ? (
        <FormField
          label="Provider"
          helper={`The provider ${product(draft.harness)} sends the model's calls to.`}
        >
          {(field) => (
            <Select
              id={field.id}
              value={draft.provider}
              options={providers.map((p) => ({ value: p.value, label: p.label }))}
              onChange={(v) => onChange({ provider: String(v) })}
            />
          )}
        </FormField>
      ) : null}

      {draft.method === "token" ? (
        <>
          <Text as="p" variant="body">
            On your own computer, signed in to your own Claude subscription, run:
          </Text>
          <Copyable
            value="claude setup-token"
            variant="block"
            monospace
            ariaLabel="Copy the command"
          />
        </>
      ) : null}

      {pastesCredential(draft) ? (
        keptCredential ? (
          <div className="models-stored">
            <Text as="span" variant="body">
              <span className="mono">{draft.variable}</span>
              {editing?.admin?.sign_in.stored_at ? (
                <>
                  {" · stored "}
                  <AgoTime value={editing.admin.sign_in.stored_at} />
                </>
              ) : (
                " · stored"
              )}
            </Text>
            <button type="button" className="t-link" onClick={() => onReplace(true)}>
              Replace the {word}
            </button>
          </div>
        ) : (
          <div className="models-stack models-credential" data-focus="credential">
            {replacing && shared.length > 0 ? (
              <Text as="p" variant="caption" tone="secondary">
                {sharedWords(shared)}
              </Text>
            ) : null}
            <ValueField
              kind="secret"
              label={
                word === "token"
                  ? "Token"
                  : draft.harness === "copilot"
                    ? "GitHub token"
                    : "API key"
              }
              value={draft.credential}
              required
              onChange={(v) => onChange({ credential: v })}
              placeholder={credentialExample(draft)}
              help={credentialHelp(draft, replacing)}
              error={errors.credential}
            />
            {replacing && canKeep ? (
              <button
                type="button"
                className="t-link models-credential__keep"
                onClick={() => onReplace(false)}
              >
                Keep the stored {word}
              </button>
            ) : null}
            {replacing ? null : (
              <ValueField
                kind="id"
                label="Stored as"
                value={draft.variable}
                onChange={onVariable}
                help="The secret the model reads its credential from. A name of its own keeps this model's credential apart from the others."
                error={errors.variable}
              />
            )}
          </div>
        )
      ) : null}

      {draft.harness === "qwen-code" && draft.method === "api-key" ? (
        <ValueField
          kind="text"
          label="Endpoint"
          value={draft.endpoint}
          placeholder={ENDPOINT_EXAMPLE}
          help="The OpenAI-compatible address Qwen Code calls. Empty uses the harness's own."
          onChange={(v) => onChange({ endpoint: v })}
        />
      ) : null}

      {draft.method === "cloud" ? (
        <>
          <FormField label="Provider">
            {(field) => (
              <Select
                id={field.id}
                value={draft.cloud}
                options={cloudsOf(draft.harness).map((c) => ({
                  value: c,
                  label: CLOUD_FIELDS[c].label,
                }))}
                onChange={(v) => onChange({ cloud: v as Cloud, cloudFields: {} })}
              />
            )}
          </FormField>
          {CLOUD_FIELDS[draft.cloud].fields.map((f) => {
            const stored = f.secret ? storedCloud[f.name] : undefined;
            return (
              <ValueField
                key={f.name}
                kind={f.secret ? "secret" : "text"}
                label={f.label}
                value={draft.cloudFields[f.name] ?? ""}
                required={!stored}
                placeholder={stored ? "Stored. Leave it empty to keep it." : f.placeholder}
                {...(stored
                  ? { help: `Stored as ${stored}. A new value is tested before it replaces it.` }
                  : f.secret
                    ? { help: "Tested before it is stored. Never shown again." }
                    : {})}
                onChange={(v) => onChange({ cloudFields: { ...draft.cloudFields, [f.name]: v } })}
              />
            );
          })}
          {errors.env ? (
            <Text as="p" variant="caption" className="models-field-error" role="alert">
              {errors.env}
            </Text>
          ) : null}
        </>
      ) : null}

      {draft.method === "host-login" && editing ? (
        <div data-focus="host-login" tabIndex={-1} className="models-focus-target">
          <HostLogin modelKey={editing.key} logins={hostLogins} onChecked={onChecked} />
        </div>
      ) : draft.method === "host-login" ? (
        <Callout variant="info" title="You sign in, on the host">
          {facts.login === "adopt" ? (
            <>
              After you create the model, sign in with <span className="mono">{facts.binary}</span>{" "}
              in a terminal on the engine host, then run{" "}
              <span className="mono">crewlet llm login {draft.key || "<model>"} -from-host</span>.
            </>
          ) : (
            <>
              After you create the model, run{" "}
              <span className="mono">crewlet llm login {draft.key || "<model>"}</span> in a terminal
              on the engine host and finish the vendor's sign-in.
            </>
          )}{" "}
          The model's page then shows whether it is signed in.
        </Callout>
      ) : null}

      {draft.method === "device" ? (
        <Callout variant="info" title="Sign in right after you save">
          The next screen shows a code to approve on OpenAI's site. Nothing you type there passes
          through Crewlet.
        </Callout>
      ) : null}

      {draft.method === "unchanged" ? (
        <Text as="p" variant="body" tone="secondary">
          {product(draft.harness)} signs this model in as Configuration sets it.
        </Text>
      ) : null}
    </>
  );
}

/** The models besides `key` that read the same secret, which a replaced credential changes too. */
export function sharedSecret(
  models: readonly ModelView[],
  key: string,
  variable: string,
): string[] {
  if (!variable) return [];
  return models
    .filter((m) => m.key !== key && m.admin?.sign_in.variable === variable)
    .map((m) => m.key);
}

/** "The model aux reads the same secret, so it changes too." */
export function sharedWords(shared: readonly string[]): string {
  if (shared.length === 0) return "";
  return shared.length === 1
    ? `The model ${shared[0]} reads the same secret, so it changes too.`
    : `The models ${listed(shared)} read the same secret, so they change too.`;
}
