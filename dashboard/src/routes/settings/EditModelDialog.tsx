/**
 * Editing one model from Settings › Models & keys: the model it runs, the
 * credential its sign-in reads, and its two caps — the wall clock on one CLI
 * invocation and how many run at once.
 *
 * A NEW CONFIGURATION REVISION, not an act: a model is a `providers.llm`
 * entry, so the write is the entity PUT at `/config/llm-providers/{id}`
 * through `protocol/configWrite.ts`, guarded by Admin
 * (`useConfigWriteAccess`). It states the revision it was read from, so a
 * colleague's save in between is a conflict to re-read rather than an
 * overwrite.
 *
 * ONLY WHAT THE FORM SHOWS CHANGES. The entity is read as the engine serves it
 * and sent back whole with the edited fields replaced — the CLI, the mode,
 * where it runs, its state directory, its environment, its overrides and the
 * login bundle ride through untouched. Those are the entry's identity rather
 * than its tuning (a different CLI reads the model id in another grammar; a
 * different sign-in bills another account), and are changed in Configuration
 * with the whole entry in view.
 *
 * ONE CREDENTIAL, THE ONE ITS SIGN-IN READS: the token under `subscription`,
 * the API key under `api-key`, none under `inherit-env` (see
 * [credentialField]). The engine refuses a credential under a mode that does
 * not read it, so the form never offers one.
 *
 * A CREDENTIAL IS A POINTER. It is a secret field: typing `$` offers the
 * company's sealed entries by name, a whole `${NAME}` reads as the name, and a
 * literal is masked. One the document holds INLINE was served as the mask and
 * is drawn locked: its value never reached this page, so it can be kept or
 * replaced, never shown or edited.
 *
 * CHECKED BEFORE IT IS STORED, against the company's own warnings, so only a
 * warning this edit introduces stops the save.
 */

import { useEffect, useMemo, useRef, useState } from "react";
import { Button, Callout, Disclosure, Modal, Skeleton, useToast } from "@crewlethq/ui";
import { CpuGlyph, KeyGlyph } from "@crewlethq/icons/glyphs";
import { ConfigField } from "~/components/ConfigField.tsx";
import { REDACTED, fmtSetting } from "~/lib/format.ts";
import {
  authModeOf,
  authWords,
  credentialField,
  editSummary,
  formChanged,
  formErrors,
  formOf,
  modelEntity,
  problemsByField,
  type ModelField,
  type ModelForm,
} from "~/lib/models.ts";
import { useSecretNames } from "~/lib/useSecretNames.ts";
import { configGuardedReason, useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { useWaiting } from "~/lib/waiting.ts";
import { DEFAULT_CLI_MAX_CONCURRENT, DEFAULT_CLI_TIMEOUT_SECONDS } from "~/contract/models.ts";
import {
  introducedWarnings,
  managedSentence,
  type ConfigRefusal,
} from "~/protocol/configAnswer.ts";
import { dryRunEntity, getEntity, putEntity } from "~/protocol/configWrite.ts";
import { isAbort } from "~/protocol/rest.ts";
import type { ConfigWarning } from "~/protocol/types.ts";

/** What the entity read came to. */
type Loaded =
  | { kind: "loading" }
  | {
      kind: "entity";
      entity: Record<string, unknown>;
      etag: string;
      /** The form as read, which the summary and the unchanged check compare against. */
      read: ModelForm;
    }
  | { kind: "failed"; message: string };

/** Where the write stands. */
type SaveState =
  | { kind: "idle" }
  | { kind: "checking" }
  | { kind: "warned"; warnings: ConfigWarning[] }
  | { kind: "saving" }
  | { kind: "refused"; message: string; fields: Partial<Record<ModelField, string>> };

/** A refusal in one sentence a person can act on. */
function refusalWords(refusal: ConfigRefusal, id: string): string {
  switch (refusal.kind) {
    case "guarded":
      return configGuardedReason(refusal.code);
    case "managed":
      return managedSentence(refusal.managedBy);
    case "conflict":
      return refusal.reason === "no_active_revision"
        ? "No company is configured any more, so there is no model to edit."
        : `The configuration changed since ${id} was opened, so nothing was stored. Close this and edit it again from what is there now.`;
    case "draining":
      return `This node is draining and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`;
    case "starting":
      return `This node is still starting and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`;
    case "unavailable":
      return refusal.detail;
    case "unreachable":
      return "The engine did not answer, so the change may or may not have been stored. Reopen the model before trying again.";
    case "problems":
      return "The engine refused the change. Correct what is marked and try again.";
  }
}

export function EditModelDialog({
  id,
  onClose,
  onSaved,
}: {
  id: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const toast = useToast();
  const waiting = useWaiting();
  const access = useConfigWriteAccess();
  const { names: secrets, refresh } = useSecretNames();
  const [loaded, setLoaded] = useState<Loaded>({ kind: "loading" });
  const [form, setForm] = useState<ModelForm | null>(null);
  const [state, setState] = useState<SaveState>({ kind: "idle" });
  const [tried, setTried] = useState(false);
  const inflight = useRef<AbortController | null>(null);

  // THE ENTITY AS THE ENGINE SERVES IT, once per open: the form edits a copy,
  // and the tag it carries is the revision every write here states.
  useEffect(() => {
    const controller = new AbortController();
    void (async () => {
      try {
        const read = await getEntity("llm-providers", id, controller.signal);
        if (read.kind === "entity") {
          const initial = formOf(read.entity);
          setLoaded({ kind: "entity", entity: read.entity, etag: read.etag, read: initial });
          setForm(initial);
          return;
        }
        setLoaded({
          kind: "failed",
          message:
            read.kind === "missing"
              ? `The active configuration has no model called ${id}.`
              : refusalWords(read, id),
        });
      } catch (err) {
        if (!isAbort(err)) {
          setLoaded({
            kind: "failed",
            message: `The model could not be read: ${err instanceof Error ? err.message : String(err)}.`,
          });
        }
      }
    })();
    return () => controller.abort();
  }, [id]);
  useEffect(() => () => inflight.current?.abort(), []);

  const local = useMemo(() => (form ? formErrors(form) : {}), [form]);
  const engine = state.kind === "refused" ? state.fields : {};
  const errorOf = (field: ModelField): string | undefined =>
    (tried ? local[field] : undefined) ?? engine[field];
  const busy = state.kind === "checking" || state.kind === "saving";

  const edit = (patch: Partial<ModelForm>) => {
    setForm((prev) => (prev ? { ...prev, ...patch } : prev));
    // AN EDIT RETRACTS THE ENGINE'S ANSWER: it was about the form as sent.
    if (!busy && state.kind !== "idle") setState({ kind: "idle" });
  };

  const begin = () => {
    inflight.current?.abort();
    const controller = new AbortController();
    inflight.current = controller;
    return controller.signal;
  };

  const store = async (signal: AbortSignal) => {
    if (loaded.kind !== "entity" || !form) return;
    setState({ kind: "saving" });
    const sent = modelEntity(loaded.entity, form);
    const outcome = await putEntity(
      "llm-providers",
      id,
      sent,
      loaded.etag,
      editSummary(id, loaded.entity, loaded.read, form),
      signal,
    );
    if (outcome.kind === "saved") {
      toast.show({
        variant: "success",
        title: `Saved ${id}`,
      });
      onSaved();
      return;
    }
    if (outcome.kind === "valid") {
      setState({
        kind: "refused",
        message: "The engine checked the change but stored nothing. Try again.",
        fields: {},
      });
      return;
    }
    setState(refused(outcome, id, sent));
  };

  const submit = () => {
    if (loaded.kind !== "entity" || !form) return;
    setTried(true);
    if (Object.keys(local).length > 0) return;
    const signal = begin();
    setState({ kind: "checking" });
    void (async () => {
      try {
        // THE ENTITY AS IT STANDS, checked beside the edit, so only a warning
        // THIS change introduces is said.
        const sent = modelEntity(loaded.entity, form);
        const [before, after] = await Promise.all([
          dryRunEntity("llm-providers", id, loaded.entity, loaded.etag, signal),
          dryRunEntity("llm-providers", id, sent, loaded.etag, signal),
        ]);
        if (after.kind === "saved") {
          onSaved();
          return;
        }
        if (after.kind !== "valid") {
          setState(refused(after, id, sent));
          return;
        }
        const warnings = introducedWarnings(
          before.kind === "valid" ? before.warnings : [],
          after.warnings,
        );
        if (warnings.length > 0) {
          setState({ kind: "warned", warnings });
          return;
        }
        await store(signal);
      } catch (err) {
        if (!isAbort(err)) setState(failed(err));
      }
    })();
  };

  const confirm = () => {
    const signal = begin();
    void store(signal).catch((err: unknown) => {
      if (!isAbort(err)) setState(failed(err));
    });
  };

  const warned = state.kind === "warned";
  const unchanged = loaded.kind === "entity" && form !== null && !formChanged(loaded.read, form);
  const blocked = !access.can
    ? access.reason
    : loaded.kind !== "entity"
      ? "The model has not been read."
      : busy
        ? waiting.reason
        : unchanged
          ? "Nothing has changed."
          : undefined;
  const press = () => {
    if (blocked) return;
    if (warned) confirm();
    else submit();
  };

  return (
    <Modal
      open
      size="md"
      title={`Edit ${id}`}
      icon={<CpuGlyph />}
      onClose={onClose}
      onSubmit={press}
      dismissable={!busy}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            variant="primary"
            onClick={press}
            loading={busy}
            disabledReason={blocked}
            title={blocked}
          >
            {warned ? "Save anyway" : "Save"}
          </Button>
        </>
      }
    >
      {loaded.kind === "loading" && <Skeleton variant="text" rows={5} label="Reading the model" />}
      {loaded.kind === "failed" && <Callout variant="danger">{loaded.message}</Callout>}
      {loaded.kind === "entity" && form && (
        <div className="col gap-3">
          <ConfigField
            label="Model"
            kind="text"
            value={form.model}
            onChange={(model) => edit({ model })}
            disabled={busy}
            error={errorOf("model")}
            help="As the CLI names it: an alias or id for Claude Code, <provider>/<model> for OpenCode."
            // FOCUS STARTS ON THE FIRST FIELD. The body is a skeleton when
            // the dialog opens, so the frame's own first-control rule found
            // only the footer and put the reader on Cancel.
            autoFocus
          />
          <CredentialEditor
            entity={loaded.entity}
            value={form.credential}
            onChange={(credential) => edit({ credential })}
            secrets={secrets}
            onSecretsNeeded={refresh}
            disabled={busy}
            error={errorOf("credential")}
          />
          <Disclosure title="Limits">
            <div className="mcp-add-pair">
              <ConfigField
                label="Timeout"
                kind="text"
                required={false}
                value={form.timeout}
                onChange={(timeout) => edit({ timeout })}
                placeholder={String(DEFAULT_CLI_TIMEOUT_SECONDS)}
                disabled={busy}
                error={errorOf("timeout")}
                help={`Seconds the CLI may work without calling a tool. Empty is ${fmtSetting(DEFAULT_CLI_TIMEOUT_SECONDS)}.`}
              />
              <ConfigField
                label="At once"
                kind="text"
                required={false}
                value={form.concurrency}
                onChange={(concurrency) => edit({ concurrency })}
                placeholder={String(DEFAULT_CLI_MAX_CONCURRENT)}
                disabled={busy}
                error={errorOf("concurrency")}
                help={`CLI processes running together, each a few hundred MB. Empty is ${DEFAULT_CLI_MAX_CONCURRENT}.`}
              />
            </div>
          </Disclosure>
          <SaveStatus state={state} />
        </div>
      )}
    </Modal>
  );
}

/**
 * The credential the entry's sign-in reads, or the sentence saying it reads
 * none from here. One written into the document is a locked row: kept, or
 * replaced, never drawn.
 */
function CredentialEditor({
  entity,
  value,
  onChange,
  secrets,
  onSecretsNeeded,
  disabled,
  error,
}: {
  entity: Record<string, unknown>;
  value: string;
  onChange: (next: string) => void;
  secrets: string[];
  onSecretsNeeded: () => void;
  disabled: boolean;
  error: string | undefined;
}) {
  const field = credentialField(entity);
  const sign = authWords(authModeOf(entity));
  if (field === null) {
    return (
      <p className="t-caption">
        Signs in with {sign.label.toLowerCase()}: {sign.hint} Nothing to set here.
      </p>
    );
  }
  const label = field === "token" ? "Token" : "API key";
  const help =
    field === "token"
      ? "A long-lived subscription token, for a CLI that mints one. Empty uses the CLI’s own token variable, or its login."
      : "Billed to the metered account. Empty reads the CLI’s own key variable.";
  if (value === REDACTED) {
    return (
      <div className="col gap-1">
        <span className="t-label">{label}</span>
        <span className="model-credential-locked">
          <KeyGlyph size="sm" />
          <span className="model-credential-locked-text">The {label.toLowerCase()} is inline.</span>
          <Button
            variant="ghost"
            size="small"
            disabled={disabled}
            aria-label={`Replace the ${label.toLowerCase()} with a reference`}
            // AN EMPTY FIELD, so nothing is left to restore and the reader
            // types the reference that replaces it.
            onClick={() => onChange("")}
          >
            Replace
          </Button>
        </span>
        {error && (
          <p className="t-caption mcp-add-pairs-error" role="alert">
            {error}
          </p>
        )}
      </div>
    );
  }
  return (
    <ConfigField
      label={label}
      kind="secret"
      required={false}
      value={value}
      onChange={onChange}
      placeholder={field === "token" ? "${CLI_TOKEN}" : "${CLI_API_KEY}"}
      secrets={secrets}
      onSecretsNeeded={onSecretsNeeded}
      disabled={disabled}
      error={error}
      help={help}
    />
  );
}

/** The line under the form: checking, what the edit would introduce, why it was refused. */
function SaveStatus({ state }: { state: SaveState }) {
  return (
    <div role="status" aria-live="polite">
      {state.kind === "checking" && (
        <span className="t-caption">Checking the company with the change in it…</span>
      )}
      {state.kind === "saving" && <span className="t-caption">Saving…</span>}
      {state.kind === "warned" && (
        <Callout variant="warning" title="The engine accepts it, with a warning">
          <ul className="ceiling-warnings">
            {state.warnings.map((w) => (
              <li key={`${w.path}:${w.message}`}>{w.message}</li>
            ))}
          </ul>
        </Callout>
      )}
      {state.kind === "refused" && <Callout variant="danger">{state.message}</Callout>}
    </div>
  );
}

function refused(refusal: ConfigRefusal, id: string, sent: Record<string, unknown>): SaveState {
  if (refusal.kind === "problems") {
    const { fields, rest } = problemsByField(refusal.problems, id, sent);
    return {
      kind: "refused",
      message: rest.length > 0 ? rest.join(" ") : refusalWords(refusal, id),
      fields,
    };
  }
  return { kind: "refused", message: refusalWords(refusal, id), fields: {} };
}

function failed(err: unknown): SaveState {
  return {
    kind: "refused",
    message: `The change could not be sent: ${err instanceof Error ? err.message : String(err)}.`,
    fields: {},
  };
}
