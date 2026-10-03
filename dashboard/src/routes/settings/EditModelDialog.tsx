/**
 * Editing one model from Settings › Models & keys: its model id, the keys it
 * rotates through, its endpoint and its bench times.
 *
 * A NEW CONFIGURATION REVISION, not an act: a model is a `providers.llm`
 * entry, so the write is the entity PUT at `/config/llm-providers/{id}`
 * through `protocol/configWrite.ts`, guarded by the operator credential
 * (`useConfigWriteAccess`). It states the revision it was read from, so a
 * colleague's save in between is a conflict to re-read rather than an
 * overwrite.
 *
 * ONLY WHAT THE FORM SHOWS CHANGES. The entity is read as the engine serves it
 * and sent back whole with the edited fields replaced — the type, reasoning,
 * the timeout and a cli-agent's block ride through untouched.
 *
 * A KEY IS A POINTER. Every key field is a secret field: typing `$` offers the
 * company's sealed entries by name, a whole `${NAME}` reads as the name, and a
 * literal is masked. A key the document holds INLINE was served as the mask
 * and is drawn locked: its value never reached this page, so it can be kept
 * or replaced IN ITS PLACE, never shown or edited. And while one is in the
 * list the list neither grows nor shrinks — the engine puts an inline key's
 * value back by its position (`keysKeepInlinePlaces`), so Add and every
 * removal that would move one are unavailable, with the reason, rather than
 * offered and refused (or, at an unchanged length, silently answered with the
 * wrong key).
 *
 * CHECKED BEFORE IT IS STORED, against the company's own warnings, so only a
 * warning this edit introduces stops the save.
 */

import { useEffect, useMemo, useRef, useState } from "react";
import { Button, Callout, Disclosure, IconButton, Modal, Skeleton, useToast } from "@crewlethq/ui";
import { CpuGlyph, KeyGlyph, PlusGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import { ConfigField } from "~/components/ConfigField.tsx";
import { REDACTED } from "~/lib/format.ts";
import {
  editSummary,
  formChanged,
  formErrors,
  formOf,
  inlinePlaceWords,
  keysKeepInlinePlaces,
  MAX_BENCH_SECONDS,
  MIN_BENCH_SECONDS,
  modelEntity,
  newKeyField,
  problemsByField,
  type KeyField,
  type ModelField,
  type ModelForm,
} from "~/lib/models.ts";
import { useSecretNames } from "~/lib/useSecretNames.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { introducedWarnings, type ConfigRefusal } from "~/protocol/configAnswer.ts";
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
      return "The engine did not take this token as an operator's, so nothing was changed.";
    case "conflict":
      return refusal.reason === "no_active_revision"
        ? "No company is configured any more, so there is no model to edit."
        : `The configuration changed since ${id} was opened, so nothing was stored. Close this and edit it again from what is there now.`;
    case "draining":
      return `This node is draining and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`;
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

  const local = useMemo(
    () => (form && loaded.kind === "entity" ? formErrors(form, loaded.read) : {}),
    [form, loaded],
  );
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
      editSummary(id, loaded.read, form),
      signal,
    );
    if (outcome.kind === "saved") {
      toast.show({
        variant: "success",
        title: `Saved ${id}`,
        message: "Each node rebuilds its pools once it has applied the new configuration.",
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
        ? "Waiting for the engine to answer."
        : unchanged
          ? "Nothing has changed."
          : undefined;
  const press = () => {
    if (blocked) return;
    if (warned) confirm();
    else submit();
  };

  const cli = loaded.kind === "entity" && loaded.entity.type === "cli-agent";

  return (
    <Modal
      open
      size="md"
      title={`Edit ${id}`}
      subtitle="Which model this entry serves, the keys it rotates through, and how long a refused key is benched."
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
            help="The vendor's model id. A ${NAME} is resolved when the model is built."
            // FOCUS STARTS ON THE FIRST FIELD. The body is a skeleton when
            // the dialog opens, so the frame's own first-control rule found
            // only the footer and put the reader on Cancel.
            autoFocus
          />
          <KeysEditor
            keys={form.keys}
            read={loaded.read.keys}
            onChange={(keys) => edit({ keys })}
            secrets={secrets}
            onSecretsNeeded={refresh}
            disabled={busy}
            error={errorOf("keys")}
            cli={cli}
          />
          {!cli && (
            <ConfigField
              label="Endpoint"
              kind="url"
              required={false}
              value={form.baseUrl}
              onChange={(baseUrl) => edit({ baseUrl })}
              placeholder="gateway.example.com/v1"
              disabled={busy}
              error={errorOf("baseUrl")}
              help="Empty is the vendor's own. Required for an openai-compatible model."
            />
          )}
          <Disclosure title="Bench times">
            <div className="mcp-add-pair">
              <ConfigField
                label="After a rate limit"
                kind="text"
                required={false}
                value={form.rateLimit}
                onChange={(rateLimit) => edit({ rateLimit })}
                placeholder="3600"
                disabled={busy}
                error={errorOf("rateLimit")}
                help={`Seconds a key the vendor answered 429 is left out, ${MIN_BENCH_SECONDS}–${MAX_BENCH_SECONDS}; empty is an hour. A Retry-After the vendor sends wins.`}
              />
              <ConfigField
                label="After an auth failure"
                kind="text"
                required={false}
                value={form.auth}
                onChange={(auth) => edit({ auth })}
                placeholder="300"
                disabled={busy}
                error={errorOf("auth")}
                help={`Seconds a key answered 401 or 403 is left out, doubling on each repeat, ${MIN_BENCH_SECONDS}–${MAX_BENCH_SECONDS}; empty is five minutes.`}
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
 * The keys, in the order a call tries them. A key written into the document
 * is a locked row: kept, or replaced in its place, never drawn. Every gesture
 * whose result would move an inline key off the place it was read at is
 * unavailable and says why (see [keysKeepInlinePlaces]).
 */
function KeysEditor({
  keys,
  read,
  onChange,
  secrets,
  onSecretsNeeded,
  disabled,
  error,
  cli,
}: {
  keys: ModelForm["keys"];
  /** The keys as read, whose inline places every gesture must keep. */
  read: readonly KeyField[];
  onChange: (next: ModelForm["keys"]) => void;
  secrets: string[];
  onSecretsNeeded: () => void;
  disabled: boolean;
  error: string | undefined;
  cli: boolean;
}) {
  const set = (i: number, value: string) =>
    onChange(keys.map((k, j) => (j === i ? { ...k, value } : k)));
  const kept = (next: readonly KeyField[]) => keysKeepInlinePlaces(read, next);
  const without = (i: number) => keys.filter((_, j) => j !== i);
  const added = [...keys, newKeyField()];
  const held = inlinePlaceWords(keys);
  const lastLocked = keys.map((k) => k.value).lastIndexOf(REDACTED);
  return (
    <fieldset className="mcp-add-pairs">
      <legend className="t-label">{cli ? "Tokens" : "Keys"}</legend>
      {keys.length === 0 && (
        <p className="t-caption">
          {cli
            ? "None: the CLI uses the login in its state directory."
            : "None: the model reads its vendor's own variable (ANTHROPIC_API_KEY or OPENAI_API_KEY). Add one as ${NAME} — typing $ offers the company's sealed secrets."}
        </p>
      )}
      {keys.map((k, i) => {
        const removable = kept(without(i));
        return (
          <div className="model-key-slot" key={k.id}>
            <div className="model-key-row">
              {k.value === REDACTED ? (
                <span className="model-key-locked">
                  <KeyGlyph size="sm" />
                  <span className="model-key-locked-text">
                    Key {i + 1} is written inline — its value is never sent here.
                  </span>
                  <Button
                    variant="ghost"
                    size="small"
                    disabled={disabled}
                    aria-label={`Replace key ${i + 1} with a reference`}
                    // IN ITS PLACE: an empty field at the same position, so
                    // no other key moves and nothing is left to restore.
                    onClick={() => set(i, "")}
                  >
                    Replace
                  </Button>
                </span>
              ) : (
                <ConfigField
                  label={`Key ${i + 1}`}
                  kind="secret"
                  value={k.value}
                  onChange={(value) => set(i, value)}
                  placeholder="${ANTHROPIC_KEY}"
                  secrets={secrets}
                  onSecretsNeeded={onSecretsNeeded}
                  disabled={disabled}
                />
              )}
              <IconButton
                className="mcp-add-pair-remove"
                label={`Remove key ${i + 1}`}
                icon={<XGlyph size="sm" />}
                variant="ghost"
                disabled={disabled}
                disabledReason={removable ? undefined : held}
                title={removable ? undefined : held}
                onClick={() => onChange(without(i))}
              />
            </div>
            {/* SAID UNDER THE ROW IT IS ABOUT, once, after the last locked
                row — under Add key it read as a rule about the new key. */}
            {i === lastLocked && (
              <p className="t-caption">
                Kept by its place in the list, so the list cannot grow or shrink while it is here.
                Replace it with a <span className="mono">{"${NAME}"}</span> stored under Secrets to
                add or remove keys.
              </p>
            )}
          </div>
        );
      })}
      {error && (
        <p className="t-caption mcp-add-pairs-error" role="alert">
          {error}
        </p>
      )}
      <div>
        <Button
          variant="secondary"
          size="small"
          leadingIcon={<PlusGlyph size="sm" />}
          disabled={disabled}
          disabledReason={kept(added) ? undefined : held}
          title={kept(added) ? undefined : held}
          onClick={() => onChange(added)}
        >
          Add key
        </Button>
      </div>
    </fieldset>
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
