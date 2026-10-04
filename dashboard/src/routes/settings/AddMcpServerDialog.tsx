/**
 * Adding an MCP server from Settings › Tools & MCP.
 *
 * A NEW CONFIGURATION REVISION, not an act: a server is a `mcp_servers` entry,
 * so the write goes through `protocol/configWrite.ts` like a seat or a budget
 * ceiling, guarded by the `config:write` grant (`useConfigWriteAccess`) rather
 * than a seat binding. The button is drawn for every reader and disabled with
 * the reason for one who cannot change the configuration.
 *
 * THE ADD IS SAID AS ONE. It is the entity PUT at the new server's own address
 * with `If-None-Match: *` — a plain PUT to a name nothing carries is a 404, the
 * engine reading it as a typo — so a name somebody else took a moment ago is
 * `entity_exists` rather than an overwrite of a launch command and credentials
 * this form never showed.
 *
 * CHECKED BEFORE IT IS STORED. The engine validates the whole company with the
 * server in it: a refusal is placed beside the field it is about, and a
 * WARNING the add would introduce (not one the company already carried) stops
 * the save and is said, so it is stored only when the person says so again.
 *
 * A CREDENTIAL IS A POINTER. Every environment value and header is a secret
 * field: typing `$` offers the company's sealed entries by name, and a literal
 * is masked. What the form never does is show a value it did not type.
 */

import { useEffect, useMemo, useRef, useState } from "react";
import { Button, Callout, Disclosure, IconButton, Modal, useToast } from "@crewlethq/ui";
import { PlugGlyph, PlusGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import { ConfigField } from "~/components/ConfigField.tsx";
import {
  addSummary,
  emptyServerForm,
  formErrors,
  nameExample,
  newPair,
  problemsByField,
  serverEntity,
  type Pair,
  type ServerField,
  type ServerForm,
} from "~/lib/mcpServers.ts";
import { useSecretNames } from "~/lib/useSecretNames.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { introducedWarnings, type ConfigRefusal } from "~/protocol/configAnswer.ts";
import { createEntity, dryRunCreate, dryRunPatch, getConfig } from "~/protocol/configWrite.ts";
import { isAbort } from "~/protocol/rest.ts";
import type { ConfigWarning } from "~/protocol/types.ts";

/** Where the write stands. */
type AddState =
  | { kind: "idle" }
  | { kind: "checking" }
  | { kind: "warned"; warnings: ConfigWarning[] }
  | { kind: "saving" }
  | {
      kind: "refused";
      message: string;
      /** The engine's problems, beside the fields they are about. */
      fields: Partial<Record<ServerField, string>>;
    };

/** A refusal in one sentence a person can act on. */
function addRefusalWords(refusal: ConfigRefusal, name: string): string {
  switch (refusal.kind) {
    case "guarded":
      return "The engine did not take this token as an operator's, so the server was not added.";
    case "conflict":
      switch (refusal.reason) {
        case "entity_exists":
          return `A server called ${name} was added since this form opened. Pick another name.`;
        case "no_active_revision":
          return "No company is configured to add a server to.";
        default:
          // THE COMPANY MOVED UNDER THE ADD: the create lands on the revision
          // active when it commits, compare-and-set, and a colleague's save
          // won that race. Nothing was stored; sending it again re-checks it
          // against their revision.
          return "The company's configuration changed while this was being added, so nothing was stored. Add it again.";
      }
    case "draining":
      return `This node is draining and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`;
    case "unreachable":
      return "The engine did not answer, so the server may or may not have been added. Look for it in the list before trying again.";
    case "problems":
      return "The engine refused the server. Correct what is marked and try again.";
  }
}

export function AddMcpServerDialog({
  taken,
  nodeOnly,
  onClose,
  onAdded,
}: {
  /** The names the configuration carries ([takenNames]): an add refuses them. */
  taken: ReadonlySet<string>;
  /**
   * Names only a node still runs, from a revision it has not left
   * ([nodeOnlyNames]): free to add, and said beside the field rather than
   * refused, since the engine's create accepts them.
   */
  nodeOnly: ReadonlySet<string>;
  onClose: () => void;
  /** Called with the new server's name once the revision is stored. */
  onAdded: (name: string) => void;
}) {
  const toast = useToast();
  const access = useConfigWriteAccess();
  const { names: secrets, refresh } = useSecretNames();
  const [form, setForm] = useState<ServerForm>(emptyServerForm);
  const [state, setState] = useState<AddState>({ kind: "idle" });
  // WHETHER A SEND WAS TRIED: a field is not red before anybody pressed Add.
  const [tried, setTried] = useState(false);
  const inflight = useRef<AbortController | null>(null);
  useEffect(() => () => inflight.current?.abort(), []);

  const local = useMemo(() => formErrors(form, taken), [form, taken]);
  const engine = state.kind === "refused" ? state.fields : {};
  const errorOf = (field: ServerField): string | undefined =>
    (tried ? local[field] : undefined) ?? engine[field];
  const busy = state.kind === "checking" || state.kind === "saving";

  // AN EDIT RETRACTS THE ENGINE'S ANSWER: a warning or a refusal is about
  // the form as it was sent, and a Save anyway under a changed form would
  // store what nobody checked.
  const edit = (patch: Partial<ServerForm>) => {
    setForm((prev) => ({ ...prev, ...patch }));
    if (!busy && state.kind !== "idle") setState({ kind: "idle" });
  };

  const begin = () => {
    inflight.current?.abort();
    const controller = new AbortController();
    inflight.current = controller;
    return controller.signal;
  };

  const store = async (signal: AbortSignal) => {
    setState({ kind: "saving" });
    const name = form.name.trim();
    const outcome = await createEntity(
      "mcp-servers",
      name,
      serverEntity(form),
      addSummary(form),
      signal,
    );
    if (outcome.kind === "saved") {
      toast.show({
        variant: "success",
        title: `Added ${name}`,
        message: "Each node starts it once it has applied the new configuration.",
      });
      onAdded(name);
      return;
    }
    if (outcome.kind === "valid") {
      setState({
        kind: "refused",
        message: "The engine checked the server but stored nothing. Try again.",
        fields: {},
      });
      return;
    }
    setState(refused(outcome, name));
  };

  const submit = () => {
    setTried(true);
    if (Object.keys(local).length > 0) return;
    const signal = begin();
    const name = form.name.trim();
    setState({ kind: "checking" });
    void (async () => {
      try {
        // THE COMPANY'S OWN WARNINGS FIRST, so only what THIS server
        // introduces is said: a dangling reference three seats away would
        // otherwise stop every add on the same unrelated sentence.
        const read = await getConfig(signal);
        const before = read.kind === "document" ? await dryRunPatch({}, read.etag, signal) : null;
        const after = await dryRunCreate("mcp-servers", name, serverEntity(form), signal);
        if (after.kind === "saved") {
          // No route answers a check with 201, but a 201 means a revision
          // exists, and saying so is the only answer that cannot become a
          // second add.
          onAdded(name);
          return;
        }
        if (after.kind !== "valid") {
          setState(refused(after, name));
          return;
        }
        const warnings = introducedWarnings(
          before?.kind === "valid" ? before.warnings : [],
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
  const blocked = !access.can
    ? access.reason
    : busy
      ? "Waiting for the engine to answer."
      : undefined;
  const press = () => {
    if (blocked) return;
    if (warned) confirm();
    else submit();
  };

  const stdio = form.transport === "stdio";

  return (
    <Modal
      open
      size="md"
      title="Add an MCP server"
      subtitle="Give agents any tool that speaks MCP — shared by the company, or one per seat with its own credentials."
      icon={<PlugGlyph />}
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
            {warned ? "Add anyway" : "Add server"}
          </Button>
        </>
      }
    >
      <div className="col gap-3">
        <ConfigField
          label="Name"
          kind="id"
          value={form.name}
          onChange={(name) => edit({ name })}
          // NEVER A TAKEN NAME: an example is something a reader copies.
          placeholder={nameExample(taken)}
          autoFocus
          disabled={busy}
          error={errorOf("name")}
          help={
            nodeOnly.has(form.name.trim()) ? (
              <>
                A node still runs a server called <span className="mono">{form.name.trim()}</span>{" "}
                from an earlier revision. The configuration carries none, so the name is free.
              </>
            ) : (
              "The key a seat declares credentials under, and how the server's tools are labelled in every prompt. It cannot be renamed here later."
            )
          }
        />
        <div className="mcp-add-pair">
          <ConfigField
            label="Transport"
            kind="choice"
            value={form.transport}
            onChange={(transport) => edit({ transport: transport as ServerForm["transport"] })}
            disabled={busy}
            choices={[
              {
                value: "stdio",
                label: "Launch a command",
                hint: "The engine runs it on each node (stdio).",
              },
              {
                value: "http",
                label: "Connect to an address",
                hint: "A server already running somewhere (http).",
              },
            ]}
          />
          <ConfigField
            label="Instances"
            kind="choice"
            value={form.sharing}
            onChange={(sharing) => edit({ sharing: sharing as ServerForm["sharing"] })}
            disabled={busy}
            choices={[
              {
                value: "shared",
                label: "One for the company",
                hint: "Every agent seat can call its tools.",
              },
              {
                value: "per_seat",
                label: "One per seat",
                hint: "Launched only for a seat that declares credentials for it under mcp_env — add those in Edit org.",
              },
            ]}
          />
        </div>
        {stdio ? (
          <>
            <ConfigField
              label="Command"
              kind="text"
              value={form.command}
              onChange={(command) => edit({ command })}
              placeholder="uvx"
              disabled={busy}
              error={errorOf("command")}
              help="The executable each node launches. It has to be on the engine host's PATH."
            />
            <ConfigField
              label="Arguments"
              kind="multiline"
              rows={2}
              required={false}
              value={form.args}
              onChange={(args) => edit({ args })}
              placeholder={"my-mcp-server\n--read-only"}
              disabled={busy}
              error={errorOf("args")}
              help="One per line, so an argument may hold a space."
            />
            <PairsEditor
              noun="variable"
              label="Environment"
              pairs={form.env}
              onChange={(env) => edit({ env })}
              secrets={secrets}
              onSecretsNeeded={refresh}
              disabled={busy}
              error={errorOf("env")}
            />
          </>
        ) : (
          <>
            <ConfigField
              label="Address"
              kind="url"
              value={form.url}
              onChange={(url) => edit({ url })}
              placeholder="mcp.example.com"
              disabled={busy}
              error={errorOf("url")}
            />
            <PairsEditor
              noun="header"
              label="Headers"
              pairs={form.headers}
              onChange={(headers) => edit({ headers })}
              secrets={secrets}
              onSecretsNeeded={refresh}
              disabled={busy}
              error={errorOf("headers")}
            />
          </>
        )}
        <Disclosure title="More options">
          <ConfigField
            label="Tool prefix"
            kind="id"
            required={false}
            value={form.toolPrefix}
            onChange={(toolPrefix) => edit({ toolPrefix })}
            disabled={busy}
            error={errorOf("toolPrefix")}
            help="Put before each of its tool names, for a server whose tools would collide with another's."
          />
        </Disclosure>
        <AddStatus state={state} />
      </div>
    </Modal>
  );
}

/**
 * The line under the form: checking, what the add would introduce, why it
 * was refused. A LIVE REGION, because each arrives after the press with
 * nothing else on screen moving.
 */
function AddStatus({ state }: { state: AddState }) {
  return (
    <div role="status" aria-live="polite">
      {state.kind === "checking" && (
        <span className="t-caption">Checking the company with the server in it…</span>
      )}
      {state.kind === "saving" && <span className="t-caption">Adding…</span>}
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

/**
 * A set of `NAME: value` rows — an environment or a header set.
 *
 * THE VALUE IS A SECRET FIELD, masked unless it is a `${NAME}` reference, and
 * `$` offers the sealed entries by name: the header an http server
 * authenticates with is the same token a stdio server reads from its
 * environment, and both belong in the sealed store rather than in the
 * company document.
 */
function PairsEditor({
  noun,
  label,
  pairs,
  onChange,
  secrets,
  onSecretsNeeded,
  disabled,
  error,
}: {
  noun: string;
  label: string;
  pairs: Pair[];
  onChange: (next: Pair[]) => void;
  secrets: string[];
  onSecretsNeeded: () => void;
  disabled: boolean;
  error: string | undefined;
}) {
  const set = (i: number, patch: Partial<Pair>) =>
    onChange(pairs.map((p, j) => (j === i ? { ...p, ...patch } : p)));
  return (
    <fieldset className="mcp-add-pairs">
      <legend className="t-label">{label}</legend>
      {pairs.length === 0 && (
        <p className="t-caption">
          None. Credentials go here as <span className="mono">{"${NAME}"}</span> — typing{" "}
          <span className="mono">$</span> offers the company&apos;s sealed secrets.
        </p>
      )}
      {pairs.map((pair, i) => (
        <div className="mcp-add-pair-row" key={pair.id}>
          <ConfigField
            label={`${capital(noun)} ${i + 1} name`}
            kind="id"
            value={pair.key}
            onChange={(key) => set(i, { key })}
            placeholder={noun === "header" ? "Authorization" : "API_TOKEN"}
            disabled={disabled}
          />
          <ConfigField
            label={`${capital(noun)} ${i + 1} value`}
            kind="secret"
            value={pair.value}
            onChange={(value) => set(i, { value })}
            placeholder="${SECRET_NAME}"
            secrets={secrets}
            onSecretsNeeded={onSecretsNeeded}
            disabled={disabled}
          />
          <IconButton
            className="mcp-add-pair-remove"
            label={`Remove ${noun} ${i + 1}`}
            icon={<XGlyph size="sm" />}
            variant="ghost"
            disabled={disabled}
            onClick={() => onChange(pairs.filter((_, j) => j !== i))}
          />
        </div>
      ))}
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
          onClick={() => onChange([...pairs, newPair()])}
        >
          Add {noun}
        </Button>
      </div>
    </fieldset>
  );
}

function capital(word: string): string {
  return word.charAt(0).toUpperCase() + word.slice(1);
}

function refused(refusal: ConfigRefusal, name: string): AddState {
  if (refusal.kind === "problems") {
    const { fields, rest } = problemsByField(refusal.problems);
    return {
      kind: "refused",
      message: rest.length > 0 ? rest.join(" ") : addRefusalWords(refusal, name),
      fields,
    };
  }
  return { kind: "refused", message: addRefusalWords(refusal, name), fields: {} };
}

/**
 * A failure no answer classifies — every refusal is a value, so this is a
 * fault in this page, said as one.
 */
function failed(err: unknown): AddState {
  return {
    kind: "refused",
    message: `The server could not be sent: ${err instanceof Error ? err.message : String(err)}.`,
    fields: {},
  };
}
