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

import { Fragment, useMemo, useState } from "react";
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
 * Which requirements a dialog shows, and in what order.
 *
 * ONE LIST, whether somebody is connecting the app or changing it afterwards.
 * It used to be two — the connect fields alone while connecting, everything
 * once configured — and the two forms behind one dialog were the bug: the
 * settings form of an app was a screen its operator had never seen, with
 * fields that appeared from nowhere and a title still reading Connect.
 *
 * CONNECT FIELDS COME FIRST. That is what the split was really protecting: a
 * form that opens by asking which seat a Datadog alert wakes asks the second
 * question before the first is answered. Ordering answers it without hiding
 * anything, so the person pasting an API key still finds it at the top and
 * the person changing the fallback seat can reach it at all.
 *
 * A FINDING still narrows to the fields that clear it, which is what makes
 * Fix on a failing row open the two inputs that matter rather than the whole
 * form. Somebody who pressed Fix asked about one thing.
 */
export function fieldsFor(reqs: SetupRequirement[], blocks?: string): SetupRequirement[] {
  if (blocks) {
    const matching = reqs.filter((r) => r.blocks === blocks);
    // A finding no requirement clears is not a reason to show an empty
    // dialog: fall back to the whole list, where the answer is at least
    // somewhere.
    return matching.length ? matching : reqs;
  }
  // A STABLE PARTITION, so within each group the app's own declared order
  // survives. An app that declares no connect fields is one whose every
  // field is part of connecting, and this is then the list unchanged.
  return [...reqs.filter((r) => r.connect), ...reqs.filter((r) => !r.connect)];
}

/**
 * The two halves of a form: what connects the app, and what configures what
 * happens over the connection.
 *
 * THE SECOND HALF IS FOLDED AWAY, and that is the difference between this
 * dialog and the console's. The console asks three things to connect
 * Datadog. This engine also receives Datadog's deliveries and routes its
 * alerts, so it has four more fields the console has no equivalent for — and
 * putting all seven in one column made connecting an app look like filling
 * in a configuration file, whichever button opened it.
 *
 * An app that declares no connect fields has one group and no disclosure:
 * every field is part of connecting, so there is nothing to fold.
 */
export function splitFields(reqs: SetupRequirement[]): {
  connect: SetupRequirement[];
  more: SetupRequirement[];
} {
  const connect = reqs.filter((r) => r.connect);
  if (connect.length === 0) return { connect: reqs, more: [] };
  return { connect, more: reqs.filter((r) => !r.connect) };
}

/**
 * How a field's value is addressed in this form.
 *
 * SCOPED TO ITS SECTION, because a field NAME is not unique across a tool:
 * Jira and Confluence both declare `url`, and they are different addresses.
 * Keyed on the name alone, the second section's value overwrote the first's,
 * so the Jira site input showed the Confluence address and saving would have
 * written it into Jira's block.
 *
 * A shared value is the exception and keeps the bare name, which is exactly
 * what makes one input feed every section that declares it.
 */
function valueKey(section: SetupSection, r: SetupRequirement): string {
  return r.shared ? r.field : `${sectionKey(section)}:${r.field}`;
}

/** A section's identity: the vendor, plus the seat when there is one. */
function sectionKey(section: SetupSection): string {
  return section.seat ? `${section.tool.key}/${section.seat}` : section.tool.key;
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
  // CONNECTING is "no section here is configured yet". It no longer decides
  // which fields the form shows — that is one list either way — only what
  // the dialog is CALLED and what its button says, which have to agree: a
  // title reading Connect over a Save button is the wrong promise, and was
  // what this dialog did.
  //
  // `sections.length > 0` because EVERY() IS TRUE OF NOTHING: a tool whose
  // setup read was refused, or that this build has no requirements for,
  // arrives with no sections at all, and an empty list called itself
  // unconfigured. The dialog then said "Connect X" with a Connect button
  // over a connected integration.
  const connecting = sections.length > 0 && sections.every((section) => !section.tool.configured);

  // What each section OWNS, before a shared value is folded into the first
  // section that asks for it.
  const ownBy = useMemo(() => {
    const out = new Map<string, SetupRequirement[]>();
    for (const section of sections) {
      const reqs =
        section.seat === undefined
          ? section.tool.requirements
          : ((section.tool.seats ?? []).find((s) => s.handle === section.seat)?.requirements ?? []);
      out.set(sectionKey(section), fieldsFor(reqs, blocks));
    }
    return out;
  }, [sections, blocks]);

  // ASKED ONCE. A shared value belongs to the tool rather than to one of its
  // surfaces, so it is rendered by the first section that declares it and
  // dropped from the rest — and it is SUBMITTED to all of them, which is what
  // payloadFor reads ownBy for.
  //
  // Atlassian asked for the account email, the API token, the cloud id and
  // the link address twice, under two headings, in one dialog. That is one
  // question with two inputs, and two inputs eventually hold two answers.
  const shownBy = useMemo(() => {
    const out = new Map<string, SetupRequirement[]>();
    const claimed = new Set<string>();
    for (const section of sections) {
      const key = sectionKey(section);
      out.set(
        key,
        (ownBy.get(key) ?? []).filter((r) => {
          if (!r.shared) return true;
          if (claimed.has(r.field)) return false;
          claimed.add(r.field);
          return true;
        }),
      );
    }
    return out;
  }, [sections, ownBy]);
  const shown = useMemo(() => [...shownBy.values()].flat(), [shownBy]);

  const [values, setValues] = useState<Record<string, string>>(() => {
    const initial: Record<string, string> = {};
    for (const section of sections) {
      for (const r of ownBy.get(sectionKey(section)) ?? []) {
        seed(initial, section, r, section.tool.configured);
      }
    }
    return initial;
  });

  function seed(
    initial: Record<string, string>,
    section: SetupSection,
    r: SetupRequirement,
    configured: boolean,
  ): void {
    {
      const key = valueKey(section, r);
      if (r.kind === "toggle") {
        // ON FOR AN APP NOBODY HAS CONFIGURED, because connecting something
        // and leaving it switched off is not what anybody means by
        // connecting it. A configured one opens on ITS OWN STATE, so a
        // paused integration no longer opens showing On and re-enables
        // itself on the next Save.
        initial[key] = configured && r.value === "false" ? "false" : "true";
        return;
      }
      // WHAT THIS COMPANY ALREADY ANSWERED, so the form is an edit of a
      // configuration rather than a blank one over it. Without this the
      // dialog offered "Choose one" over a region and a fallback seat the
      // document held, and saving it blanked both.
      //
      // Never a credential: the engine does not put one on this wire, so
      // `value` is absent on every secret and those open empty, which is
      // what "leave it blank to keep it" means below.
      if (r.value) {
        initial[key] = r.value;
        return;
      }
      // A DEFAULT ONLY WHERE THERE IS NOTHING. A field this company has
      // already answered keeps its answer; offering the default over it
      // would quietly propose changing a working setting to the common
      // one. And it is seeded into the form rather than assumed on the
      // far side, so what is submitted is what was on screen.
      if (r.default && !r.present) {
        initial[key] = r.default;
      }
    }
  }
  const [busy, setBusy] = useState(false);
  // The disclosure is closed until somebody opens it, or until a submission
  // is refused for a field inside it.
  const [moreOpen, setMoreOpen] = useState(false);
  const onMoreToggle = (e: React.SyntheticEvent<HTMLDetailsElement>) => {
    setMoreOpen(e.currentTarget.open);
  };
  const [error, setError] = useState("");
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  // A MINTABLE SECRET IS THE ONLY FIELD WITHOUT AN INPUT, because there is
  // nothing for a person to type: the engine generates the value and it has
  // a shape a person would get wrong.
  //
  // A stored ordinary credential used to be hidden behind a Replace button
  // too, which is what made the settings dialog a different form from the
  // connect one: three inputs became three lines of text. It renders as its
  // own input either way now, empty, and an empty one is left alone.
  function editable(r: SetupRequirement): boolean {
    return !(r.kind === "secret" && r.mintable);
  }

  /** What one section would send: only what was touched, plus its mints. */
  function payloadFor(
    section: SetupSection,
    reqs: SetupRequirement[],
  ): {
    values: Record<string, string>;
    generate: string[];
  } {
    const send: Record<string, string> = {};
    const generate: string[] = [];
    for (const r of reqs) {
      if (r.kind === "secret" && r.mintable) {
        // ONLY WHERE THERE IS NOTHING YET. A mintable credential this
        // company already holds is left alone: regenerating one is a
        // rotation, it revokes what every running seat is authenticating
        // with, and it is not something a form does as a side effect of
        // saving an unrelated field. That is the Secrets screen's job.
        if (!r.present) generate.push(r.field);
        continue;
      }
      const value = values[valueKey(section, r)];
      if (value === undefined) continue;
      // AN EMPTY CREDENTIAL FIELD MEANS KEEP THE ONE YOU HAVE. The input
      // opens empty because the engine never sends a credential back, so
      // submitting an empty one would rewrite a working key with nothing.
      // That is also what lets the connect form and the settings form be
      // the same form: no Replace step, and no way to blank a secret by
      // not retyping it.
      if (r.kind === "secret" && value.trim() === "") continue;
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
      // FROM WHAT THE SECTION OWNS, not from what it shows: a shared value is
      // rendered by one section and belongs to every section that declares
      // it, so writing only what is on screen would leave the second block
      // without the token the first one collected.
      .map((section) => ({
        section,
        body: payloadFor(section, ownBy.get(sectionKey(section)) ?? []),
      }))
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
        const found = sections
          .flatMap((section) => (ownBy.get(sectionKey(section)) ?? []).map((r) => ({ section, r })))
          .find(({ r }) => r.config_path === path);
        const target = found?.r;
        if (found && target) {
          setFieldErrors({
            [valueKey(found.section, target)]:
              "The company configuration holds a value here rather than a ${VAR} " +
              "reference, so there is no variable to store the credential in. " +
              "Clear it in Configuration, then try again.",
          });
          // An error inside the disclosure is an error nobody can see.
          if (!target.connect) setMoreOpen(true);
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
      title={connecting ? `Connect ${title}` : `${title} settings`}
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
        const { connect, more } = splitFields(reqs);
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
            {connect.map((r) => (
              <Fragment key={`${sectionKey(section)}:${r.field}`}>
                {renderField(section, r)}
              </Fragment>
            ))}
            {/* CLOSED, and closed in both directions: a disclosure that
                sprang open when a field inside it was unset would be the
                settings form differing from the connect form again, which
                is the one thing this dialog may not do. It opens when a
                submission is refused for something inside it, so an error
                is never hidden behind it. */}
            {more.length > 0 && (
              <details className="int-form-more" open={moreOpen} onToggle={onMoreToggle}>
                <summary className="int-summary">
                  More settings
                  <span className="faint"> ({more.length})</span>
                </summary>
                <div className="int-form-more-fields">
                  {more.map((r) => (
                    <Fragment key={`${sectionKey(section)}:${r.field}`}>
                      {renderField(section, r)}
                    </Fragment>
                  ))}
                </div>
              </details>
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
  function renderField(section: SetupSection, r: SetupRequirement) {
    const appName = section.name;
    const key = valueKey(section, r);
    return (
      <div key={key} className="col gap-1">
        {r.kind === "secret" && r.mintable ? (
          <div className="field">
            <label>{r.label}</label>
            <span className="hint">Crewlet generates this and seals it in the secret store.</span>
            {/* A REFUSAL BELONGS BESIDE ITS FIELD EVEN WHEN THE FIELD HAS
                    NO INPUT. A mintable secret is exactly the case where the
                    engine can answer literal_in_config, because the operator
                    never sees the slot it refuses to overwrite. */}
            {fieldErrors[key] && (
              <span className="hint field-error" role="alert">
                {fieldErrors[key]}
              </span>
            )}
          </div>
        ) : editable(r) ? (
          <Field
            label={r.label}
            kind={r.kind === "toggle" ? "choice" : (r.kind as FieldKind)}
            value={values[key] ?? ""}
            onChange={(v) => setValues((c) => ({ ...c, [key]: v }))}
            // WHAT THE APP SAYS, and nothing about which button opened the
            // form. Marking a connect field required while connecting and
            // optional afterwards made one field wear two labels in two
            // renderings of one form, and the app's own answer is the true
            // one: Datadog routes alerts with no keys at all, so its keys
            // are optional whoever is looking.
            // WHAT THE APP SAYS, and nothing about the state of this
            // company. A stored required secret read "(optional)" here and
            // required on the connect form, which is one field wearing two
            // labels in two renderings of what is meant to be one form.
            //
            // It does not force a re-entry: nothing here is an HTML
            // required attribute, and an empty credential field means keep
            // the stored one (see payloadFor).
            required={r.required}
            error={fieldErrors[key]}
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
                {/* THE APP'S OWN SENTENCE FIRST, always, so a field's
                    description opens the same way whether or not this
                    company has answered it. */}
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
        ) : null}
      </div>
    );
  }
}
