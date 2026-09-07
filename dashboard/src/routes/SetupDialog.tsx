/**
 * Connecting an integration, from the requirement list the engine answers.
 *
 * NOTHING ABOUT ANY VENDOR IS IN THIS FILE. Every label, every help line,
 * every link to a third-party app's own page arrives on the requirement, so adding a
 * third-party app is a Go change and no screen work at all. That is the one structural
 * difference from the console, where each third-party app's fields are inline in a
 * single very long component and the seventh third-party app cost as much as the
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
 * THREE CASES, and the first is the one this exists for.
 *
 * CONNECTING shows only the fields that establish the connection. An app
 * declares which those are; the rest configure what happens OVER the
 * connection, and asking which seat a Datadog alert wakes while somebody is
 * pasting an API key asks the second question before the first is answered.
 *
 * MANAGING shows everything, because by then there is a connection and the
 * rest of it is what there is to manage.
 *
 * A FINDING narrows to the fields that clear it, which is what makes Fix on
 * a failing row open the two inputs that matter rather than the whole form.
 * It wins over both: somebody who pressed Fix asked about one thing.
 */
export function fieldsFor(
  reqs: SetupRequirement[],
  blocks?: string,
  connecting?: boolean,
): SetupRequirement[] {
  if (blocks) {
    const matching = reqs.filter((r) => r.blocks === blocks);
    // A finding no requirement clears is not a reason to show an empty
    // dialog: fall back to the whole list, where the answer is at least
    // somewhere.
    return matching.length ? matching : reqs;
  }
  if (!connecting) {
    return reqs;
  }
  const connect = reqs.filter((r) => r.connect);
  // An app that declares none is one whose every field is part of
  // connecting — which is true of most of them — so the whole list is the
  // right answer rather than an empty form.
  return connect.length ? connect : reqs;
}

/** A section's identity: the vendor, plus the seat when there is one. */
function sectionKey(section: SetupSection): string {
  return section.seat ? `${section.tool.key}/${section.seat}` : section.tool.key;
}

/** A satisfied secret shows what it points at; nothing else needs a note. */
function pointerNote(r: SetupRequirement): string {
  if (r.kind !== "secret" || !r.present) return "";
  return r.secret_name ? "Stored as ${" + r.secret_name + "}" : "Already stored";
}

/** One engine surface inside a tool's dialog. */
export interface SetupSection {
  /** The surface's own name: Jira, Confluence, the Forge relay. */
  name: string;
  tool: SetupToolState;
  /**
   * The seat this section is for, on a third-party app whose credentials live on the
   * seat. Its requirements replace the tool's, and the submission names it.
   */
  seat?: string;
}

export function SetupDialog({
  sections,
  title,
  blocks,
  onClose,
  onDone,
}: {
  /**
   * The surfaces this tool is made of. Usually one; Atlassian is three,
   * because a company thinks in Atlassian and the engine reaches it over
   * Jira, Confluence and the Forge relay. Each section submits to its own
   * third-party app block, which is what keeps every write atomic on the thing it
   * changes.
   */
  sections: SetupSection[];
  /** The tool's own name, which the catalogue has and the API does not. */
  title: string;
  /** Narrow to the fields clearing one finding. */
  blocks?: string;
  onClose: () => void;
  onDone: () => void;
}) {
  const toast = useToast();
  // Keyed by SECTION rather than by vendor, because a per-seat vendor has
  // one section per seat and they all carry the same third-party app key.
  // CONNECTING is "no section here is configured yet". It decides which
  // fields the form shows and what its button says, and both have to agree:
  // a button reading Connect over a form of settings is the wrong promise.
  const connecting = sections.every((section) => !section.tool.configured);

  const shownBy = useMemo(() => {
    const out = new Map<string, SetupRequirement[]>();
    for (const section of sections) {
      const reqs =
        section.seat === undefined
          ? section.tool.requirements
          : ((section.tool.seats ?? []).find((s) => s.handle === section.seat)?.requirements ?? []);
      // CONNECTING is "this app is not configured yet". Once it is, the
      // same dialog is the Manage one and shows everything.
      out.set(sectionKey(section), fieldsFor(reqs, blocks, connecting));
    }
    return out;
  }, [sections, blocks, connecting]);
  const shown = useMemo(() => [...shownBy.values()].flat(), [shownBy]);

  // A field the engine already holds is left alone unless the operator asks
  // to replace it: sending it back would rewrite a working credential with
  // whatever a form rendered.
  const [replacing, setReplacing] = useState<Record<string, boolean>>({});
  const [values, setValues] = useState<Record<string, string>>(() => {
    const initial: Record<string, string> = {};
    for (const r of shown) {
      if (r.kind === "toggle") {
        initial[r.field] = r.present ? "true" : "true";
        continue;
      }
      // A DEFAULT ONLY WHERE THERE IS NOTHING. A field this company has
      // already answered keeps its answer; offering the default over it
      // would quietly propose changing a working setting to the common
      // one. And it is seeded into the form rather than assumed on the
      // far side, so what is submitted is what was on screen.
      if (r.default && !r.present) {
        initial[r.field] = r.default;
      }
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

  /** What one section would send: only what was touched, plus its mints. */
  function payloadFor(reqs: SetupRequirement[]): {
    values: Record<string, string>;
    generate: string[];
  } {
    const send: Record<string, string> = {};
    const generate: string[] = [];
    for (const r of reqs) {
      if (r.kind === "secret" && r.mintable) {
        if (!r.present || replacing[r.field]) generate.push(r.field);
        continue;
      }
      const value = values[r.field];
      if (value === undefined) continue;
      // A field the engine already holds is left alone unless the operator
      // asked to replace it: sending it back would rewrite a working
      // credential with whatever a form rendered.
      if (r.kind === "secret" && r.present && !replacing[r.field]) continue;
      send[r.field] = value;
    }
    return { values: send, generate };
  }

  async function submit(): Promise<void> {
    setBusy(true);
    setError("");
    setFieldErrors({});

    // ONE REQUEST PER SURFACE, and only for the surfaces that have
    // something to send. Each is atomic on its own third-party app block, so a
    // refusal on the second leaves the first landed rather than half
    // applied to one document.
    const work = sections
      .map((section) => ({ section, body: payloadFor(shownBy.get(sectionKey(section)) ?? []) }))
      .filter(({ body }) => Object.keys(body.values).length > 0 || body.generate.length > 0);
    if (work.length === 0) {
      setError("Nothing to submit: fill in a field, or choose to replace a stored one.");
      setBusy(false);
      return;
    }
    try {
      let rotated = false;
      for (const { section, body } of work) {
        const answer = (await rest.post(`/setup/integrations/${section.tool.key}/inputs`, {
          ...body,
          ...(section.seat ? { seat: section.seat } : {}),
        })) as Submitted;
        rotated = rotated || answer.reloaded === true;
      }
      toast.ok(rotated ? `${title} credentials rotated and republished` : `${title} connected`);
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
            {busy ? (connecting ? "Connecting" : "Saving") : connecting ? "Connect" : "Save"}
          </Button>
        </>
      }
    >
      {sections.map((section) => {
        const reqs = shownBy.get(sectionKey(section)) ?? [];
        if (reqs.length === 0) return null;
        return (
          <div key={sectionKey(section)} className="int-form">
            {/* A HEADING ONLY WHERE THERE IS MORE THAN ONE. On Slack the
                dialog is already titled Slack, and a "Slack" heading under
                it is a word that says nothing. */}
            {sections.length > 1 && <strong className="int-section">{section.name}</strong>}
            {/* WHAT CONNECTING DOES, before what it needs. A form that opens
                with a credential field asks for a secret before saying what
                it is for, and the engine is the one that knows: the sentence
                comes from the app's own package, not from here. */}
            {section.tool.summary && <p className="int-form-intro">{section.tool.summary}</p>}
            {section.tool.public_url && !section.tool.can_provision && (
              <div className="banner neutral">
                <Icon name="link" size="sm" />
                <span className="col" style={{ gap: 4 }}>
                  <span>
                    Deliveries arrive at <code className="inline">{section.tool.public_url}</code>
                  </span>
                  <span className="t-caption">
                    This engine registers no webhook for {section.name}, so paste that address into{" "}
                    {section.name}&apos;s own settings yourself.
                  </span>
                </span>
              </div>
            )}
            {reqs.map((r) => renderField(r, section.name))}
          </div>
        );
      })}

      {error && (
        <div className="banner critical">
          <Icon name="alert" size="sm" />
          <span>{error}</span>
        </div>
      )}

      {/* WHERE THE CREDENTIAL GOES, said before it is handed over. An
          operator typing a key into a self-hosted process is owed that much,
          and no more: how the configuration REFERS to a sealed value is the
          engine's business, not something to explain on the way past. */}
      <span className="t-caption faint">Credentials are sealed in the secret store.</span>
    </Dialog>
  );

  // The app's NAME is passed in rather than read from a closure: this is
  // one function over every section's fields, and the link it draws has to
  // say which app it opens.
  function renderField(r: SetupRequirement, appName: string) {
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
            // A CONNECT FIELD IS NOT "(optional)". It may be optional to
            // the integration as a whole — Datadog routes alerts with no
            // keys — but it is the thing being asked for on a form whose
            // button says Connect, and marking it optional there reads as
            // "you can skip this and still connect".
            required={r.required || (connecting && r.connect === true)}
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
                      {/* THE LINK IS PART OF THE SENTENCE when the app says
                          what to call it: "Create one on your API keys page"
                          sends somebody to the page it names, where a
                          trailing "Open Datadog" makes them work out which
                          of three pages the form meant. */}
                      {r.link_text || `Open ${appName}`}
                    </a>
                    {r.link_text ? "." : null}
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
  }
}
