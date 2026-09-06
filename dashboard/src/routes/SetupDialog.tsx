/**
 * Connecting an integration, from the requirement list the engine answers.
 *
 * NOTHING ABOUT ANY VENDOR IS IN THIS FILE. Every label, every help line,
 * every link to a vendor's own page arrives on the requirement, so adding a
 * vendor is a Go change and no screen work at all. That is the one structural
 * difference from the console, where each vendor's fields are inline in a
 * single very long component and the seventh vendor cost as much as the
 * first.
 *
 * # A secret is never rendered
 *
 * A field the engine already holds shows the `${VAR}` it points at and a
 * Replace link, never a masked value: there is no masked value to show,
 * because no route here returns one. A mintable field shows no input at all,
 * only a line saying the engine will generate it, because asking a person to
 * invent a shared token is asking them to invent a password.
 *
 * # It submits once
 *
 * Every value goes in one request. The engine seals the credentials, patches
 * the document and advances the epoch in that order, so there is no state
 * where this dialog holds a credential across two calls and no way for a
 * closed tab to leave the write half done.
 */

import { useMemo, useState } from "react";
import { Badge, Button } from "~/ui/primitives.tsx";
import { Dialog } from "~/ui/Dialog.tsx";
import { Field, type FieldKind } from "~/ui/Field.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useToast } from "~/ui/Toast.tsx";
import { rest, RestError } from "~/protocol/index.ts";
import type { SetupRequirement, SetupToolState } from "~/protocol/index.ts";

/** What the engine answers a submission with. */
interface Submitted {
  revision_id: string;
  wrote_secrets?: string[];
  reloaded?: boolean;
  state?: SetupToolState;
}

/**
 * Which requirements a dialog shows.
 *
 * `all` is the Connect and Manage case. A finding kind narrows it to the
 * fields that clear that finding, which is what makes the Fix button on a
 * degraded row open the two inputs that matter rather than the whole form.
 */
export function fieldsFor(reqs: SetupRequirement[], blocks?: string): SetupRequirement[] {
  if (!blocks) return reqs;
  const matching = reqs.filter((r) => r.blocks === blocks);
  // A finding no requirement clears is not a reason to show an empty
  // dialog: fall back to the whole list, where the answer is at least
  // somewhere.
  return matching.length ? matching : reqs;
}

/** A satisfied secret shows what it points at; nothing else needs a note. */
function pointerNote(r: SetupRequirement): string {
  if (r.kind !== "secret" || !r.present) return "";
  return r.secret_name ? "Stored as ${" + r.secret_name + "}" : "Already stored";
}

export function SetupDialog({
  tool,
  title,
  blocks,
  revision,
  onClose,
  onDone,
}: {
  tool: SetupToolState;
  /** The vendor's own name, which the catalogue has and the API does not. */
  title: string;
  /** Narrow to the fields clearing one finding. */
  blocks?: string;
  /** The revision the requirement list was read against. */
  revision?: string;
  onClose: () => void;
  onDone: () => void;
}) {
  const toast = useToast();
  const shown = useMemo(() => fieldsFor(tool.requirements, blocks), [tool.requirements, blocks]);

  // A field the engine already holds is left alone unless the operator asks
  // to replace it: sending it back would rewrite a working credential with
  // whatever a form rendered.
  const [replacing, setReplacing] = useState<Record<string, boolean>>({});
  const [values, setValues] = useState<Record<string, string>>(() => {
    const initial: Record<string, string> = {};
    for (const r of shown) {
      if (r.kind === "toggle") initial[r.field] = r.present ? "true" : "true";
    }
    return initial;
  });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  function editable(r: SetupRequirement): boolean {
    if (r.kind === "secret" && r.mintable) return false;
    if (r.kind === "secret" && r.present && !replacing[r.field]) return false;
    return true;
  }

  async function submit(): Promise<void> {
    setBusy(true);
    setError("");
    setFieldErrors({});
    // Only what the operator actually touched, plus the mints they left in
    // place. A field sent back unchanged is a field rewritten for no reason.
    const send: Record<string, string> = {};
    const generate: string[] = [];
    for (const r of shown) {
      if (r.kind === "secret" && r.mintable) {
        if (!r.present || replacing[r.field]) generate.push(r.field);
        continue;
      }
      const value = values[r.field];
      if (value === undefined) continue;
      if (r.kind === "secret" && r.present && !replacing[r.field]) continue;
      send[r.field] = value;
    }
    if (Object.keys(send).length === 0 && generate.length === 0) {
      setError("Nothing to submit: fill in a field, or choose to replace a stored one.");
      setBusy(false);
      return;
    }
    try {
      const body = (await rest.post(`/setup/integrations/${tool.key}/inputs`, {
        values: send,
        generate,
        ...(revision ? { if_match: revision } : {}),
      })) as Submitted;
      toast.ok(
        body.reloaded ? `${title} credentials rotated and republished` : `${title} connected`,
      );
      onDone();
      onClose();
    } catch (err) {
      if (!(err instanceof RestError)) {
        setError(String(err));
        return;
      }
      // The engine's refusals are specific, and the specific ones belong
      // beside the field rather than in a banner nobody connects to an
      // input.
      if (err.code === "literal_in_config") {
        const path = String(err.body.path ?? "");
        const target = shown.find((r) => r.config_path === path);
        if (target) {
          setFieldErrors({
            [target.field]:
              "The company configuration holds a value here rather than a ${VAR} " +
              "reference, so there is no variable to store the credential in. " +
              "Clear it in Configuration, then try again.",
          });
          return;
        }
      }
      if (err.code === "revision_advanced") {
        setError(
          "The configuration changed while this was open. Close and reopen to see " +
            "the current state, then submit again.",
        );
        return;
      }
      setError(err.detail || err.hint || err.code || "The engine refused that.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog
      title={`Connect ${title}`}
      icon="plug"
      onClose={onClose}
      dismissable={!busy}
      width={560}
      onSubmit={() => void submit()}
      footer={
        <>
          <span className="spacer" />
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button variant="primary" type="submit" disabled={busy}>
            {busy ? "Saving" : "Save"}
          </Button>
        </>
      }
    >
      {tool.public_url && (
        <div className="banner neutral">
          <Icon name="link" size="sm" />
          <span className="col" style={{ gap: 4 }}>
            <span>
              {title} delivers to <code className="inline">{tool.public_url}</code>
            </span>
            <span className="t-caption">
              Paste that into the vendor's own webhook settings. The engine does not register it for
              you.
            </span>
          </span>
        </div>
      )}

      {shown.map((r) => {
        const note = pointerNote(r);
        return (
          <div key={r.field} className="col gap-1">
            {r.kind === "secret" && r.mintable ? (
              <div className="field">
                <label>{r.label}</label>
                <span className="hint">
                  {r.present && !replacing[r.field]
                    ? note
                    : "Crewlet will generate this and seal it in the secret store."}
                </span>
                {/* A REFUSAL BELONGS BESIDE ITS FIELD EVEN WHEN THE FIELD HAS
                    NO INPUT. A mintable secret is exactly the case where the
                    engine can answer literal_in_config, because the operator
                    never sees the slot it refuses to overwrite. */}
                {fieldErrors[r.field] && (
                  <span className="hint field-error" role="alert">
                    {fieldErrors[r.field]}
                  </span>
                )}
                {r.present && !replacing[r.field] && (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => setReplacing((c) => ({ ...c, [r.field]: true }))}
                  >
                    Generate a new one
                  </Button>
                )}
              </div>
            ) : editable(r) ? (
              <Field
                label={r.label}
                kind={r.kind === "toggle" ? "choice" : (r.kind as FieldKind)}
                value={values[r.field] ?? ""}
                onChange={(v) => setValues((c) => ({ ...c, [r.field]: v }))}
                required={r.required}
                error={fieldErrors[r.field]}
                choices={
                  r.kind === "toggle"
                    ? [
                        { value: "true", label: "On" },
                        { value: "false", label: "Off" },
                      ]
                    : r.choices?.map((c) => ({
                        value: c.value,
                        label: c.label,
                        hint: c.hint,
                      }))
                }
                help={
                  <>
                    {r.help}
                    {r.where && <> {r.where}</>}
                    {r.vendor_url && (
                      <>
                        {" "}
                        <a href={r.vendor_url} target="_blank" rel="noreferrer">
                          Open at the vendor
                        </a>
                      </>
                    )}
                  </>
                }
              />
            ) : (
              <div className="field">
                <label>{r.label}</label>
                <span className="hint">{note}</span>
                {fieldErrors[r.field] && (
                  <span className="hint field-error" role="alert">
                    {fieldErrors[r.field]}
                  </span>
                )}
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => setReplacing((c) => ({ ...c, [r.field]: true }))}
                >
                  Replace
                </Button>
              </div>
            )}
          </div>
        );
      })}

      {error && (
        <div className="banner critical">
          <Icon name="alert" size="sm" />
          <span>{error}</span>
        </div>
      )}

      {/* WHAT THE SAVE ACTUALLY DOES, said before it is pressed. An operator
          about to hand a credential to a self-hosted process is owed the
          sentence that says where it goes. */}
      <span className="t-caption faint">
        Credentials are sealed in the fleet's secret store and the company configuration is given a{" "}
        <code className="inline">${"{VAR}"}</code> pointing at them. Nothing on this page is ever
        sent back to a browser.
      </span>
      {!tool.satisfied && (
        <Badge tone="caution" outline>
          setup incomplete
        </Badge>
      )}
    </Dialog>
  );
}
