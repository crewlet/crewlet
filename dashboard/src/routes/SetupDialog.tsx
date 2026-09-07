/**
 * Connecting an integration, from the requirement list the engine answers.
 *
 * NOTHING ABOUT ANY THIRD-PARTY APP IS IN THIS FILE. Every label, every help
 * line, every link to an app's own page arrives on the requirement, so adding
 * an integration is a Go change and no screen work at all. That is the one
 * structural difference from the console, where each app's fields are inline
 * in a single very long component and the seventh cost as much as the first.
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

import { Fragment, useEffect, useMemo, useState } from "react";
import { Badge, Button } from "~/ui/primitives.tsx";
import { Dialog } from "~/ui/Dialog.tsx";
import { Field, type FieldKind } from "~/ui/Field.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { Problems } from "~/ui/Problems.tsx";
import { useToast } from "~/ui/Toast.tsx";
import { rest, RestError } from "~/protocol/index.ts";
import type { SetupRequirement, SetupToolState } from "~/protocol/index.ts";
import { href } from "~/app/router.tsx";

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
 * A vendor link with its `{field}` placeholders filled in, or empty when one
 * of them has no answer yet.
 *
 * EMPTY MEANS NO LINK. Atlassian's API keys live at a per-organization
 * address, so until somebody has typed the organization id there is no page
 * to open — and a link to the console's front door sends them somewhere they
 * then have to navigate out of, which is worse than no link at all.
 */
export function vendorLink(
  url: string,
  values: Record<string, string>,
  key: (f: string) => string,
  read?: (f: string) => string,
): string {
  if (!url) return "";
  let resolved = url;
  for (const match of url.matchAll(/\{([a-z_]+)\}/g)) {
    const field = match[1] ?? "";
    const typed = (values[key(field)] ?? "").trim();
    // A REFERENCE IS NOT AN ADDRESS. A box holding `${ATLASSIAN_ORG_ID}`
    // names the organization rather than being it, so the path takes what
    // the engine currently reads for it. WHAT IS TYPED WINS otherwise, so
    // the link follows the box as somebody fills it in.
    const value = isReference(typed) ? (read?.(field) ?? "").trim() : typed;
    if (value === "") return "";
    resolved = resolved.replace(`{${field}}`, encodeURIComponent(value));
  }
  return resolved;
}

/** Whether a value is wholly a `${NAME}` reference. See ui/Field.tsx. */
function isReference(value: string): boolean {
  return /^\$\{[A-Za-z_][A-Za-z0-9_]*\}$/.test(value.trim());
}

/**
 * A field's own sentence, with `{other_field}` filled in from the form.
 *
 * A description that quotes a value is only true while that value is what
 * the form holds. Datadog's tag key is the case: its help gives the example
 * `"crewlet:<seat handle>"`, and somebody who changes the key to `owner` was
 * left reading an example for the key they had just replaced.
 *
 * SEPARATE FROM [vendorLink] although both read `{field}`, because they want
 * OPPOSITE things from an unanswered one: a link with a hole in its path
 * goes nowhere and is dropped entirely, while a sentence is still worth
 * reading. So `resolve` falls back to the field's DEFAULT, which is what
 * the engine uses for a blank optional field and makes the filled-in
 * sentence true rather than merely nonempty. Only a template naming a field
 * with neither is left visibly unresolved, which is an authoring mistake and
 * should look like one.
 */
export function fillTemplate(text: string, resolve: (field: string) => string): string {
  if (!text) return text;
  return text.replace(/\{([a-z_]+)\}/g, (whole, field: string) => {
    const value = resolve(field).trim();
    return value === "" ? whole : value;
  });
}

/**
 * Whether a requirement is a row on the form at all.
 *
 * A HIDDEN field is written without being asked for, and a MINTABLE
 * credential has nothing for a person to type. Neither is rendered, and
 * neither may be COUNTED: the fold said "More settings (5)" over two inputs,
 * because the count came from the requirement list and the rendering from
 * this rule. Both read it now.
 */
export function shownField(r: SetupRequirement): boolean {
  return !r.hidden && !(r.kind === "secret" && r.mintable);
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
  // WHAT IS RENDERED, on both sides of the fold. See [shownField].
  const shown = reqs.filter(shownField);
  const connect = shown.filter((r) => r.connect);
  if (connect.length === 0) return { connect: shown, more: [] };
  return { connect, more: shown.filter((r) => !r.connect) };
}

/**
 * What a stored credential shows in its input when there is nothing true to
 * put there.
 *
 * A credential that lives in the sealed store is REFERENCED from the config
 * as `${NAME}`, and the engine sends that reference back: it is a name, not a
 * secret, and showing it is how an operator can tell they are editing a
 * pointer rather than about to replace the credential behind it. Everything
 * this dashboard writes takes that shape, so this placeholder is for the one
 * case that does not: a company that hand-wrote a LITERAL into its config
 * document. There the engine sends nothing, correctly, and an empty box under
 * a required label would read as an unanswered question on a form that is
 * already complete.
 *
 * Left alone it is never submitted (see payloadFor): typing over it is what
 * replaces the credential.
 */
export const HELD = "••••••••••••••••";

/** Why a reference is worth preferring, for the note's hover. */
const WHY_SECRETS =
  "Recommendation: keep a credential in Secrets and put its reference here, written " +
  "${NAME}. The config then names the entry instead of carrying the value, so one " +
  "credential can serve several fields and rotating it is one edit in one place.";

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
   * The seat this section is for, on a third-party app whose credentials
   * live on the seat. Its requirements replace the tool's, and the
   * submission names it.
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
   * vendor block, which is what keeps every write atomic on the thing it
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
  // THE NAMES THIS COMPANY HOLDS, so typing `$` in any box offers them.
  //
  // NAMES ONLY, read once when the dialog opens. A refused read leaves the
  // list empty and the fields exactly as they were: the completion is a
  // convenience, and a form that could not be filled in because a second
  // request failed would be worse than one with no completion at all.
  const [secretNames, setSecretNames] = useState<string[]>([]);
  useEffect(() => {
    let live = true;
    void (async () => {
      try {
        const body = (await rest.get("/secrets")) as { secrets?: { name?: string }[] } | null;
        if (!live) return;
        setSecretNames((body?.secrets ?? []).map((s) => s.name ?? "").filter(Boolean));
      } catch {
        // Nothing to say and nothing to do: see the note above.
      }
    })();
    return () => {
      live = false;
    };
  }, []);
  // Keyed by SECTION rather than by vendor, because a per-seat vendor has
  // one section per seat and they all carry the same vendor key.
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
  // surfaces, so it is rendered by ONE section and dropped from the rest, and
  // it is SUBMITTED to all of them, which is what payloadFor reads ownBy for.
  //
  // THE LAST SECTION THAT DECLARES IT, not the first, which is what decides
  // where it lands on the form. Claimed by the first, Atlassian's account
  // email and API token rendered inside Jira and pushed the Confluence site
  // below them, so the two site addresses (one question, asked twice) were
  // split by two fields belonging to neither. A value several surfaces share
  // reads as the tool's own once it follows the surfaces rather than
  // interrupting the first of them.
  //
  // Atlassian asked for the account email, the API token, the cloud id and
  // the link address twice, under two headings, in one dialog. That is one
  // question with two inputs, and two inputs eventually hold two answers.
  const shownBy = useMemo(() => {
    const out = new Map<string, SetupRequirement[]>();
    const claimed = new Set<string>();
    // BACKWARDS to claim, forwards to render: the map keeps the sections'
    // own order, so only which section owns a shared field changes.
    for (const section of [...sections].reverse()) {
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
    return new Map(
      sections.map((section) => [sectionKey(section), out.get(sectionKey(section)) ?? []]),
    );
  }, [sections, ownBy]);
  const shown = useMemo(() => [...shownBy.values()].flat(), [shownBy]);

  // WHAT THE ONE FORM RENDERS.
  //
  // A per-seat app keeps a heading per section, because each is a different
  // agent's own credentials and the name is what says whose. Every other
  // tool is one thing to the person filling the form in, whatever the config
  // calls its blocks, so its surfaces run together with no headings and one
  // fold at the end.
  const perSeat = sections.some((section) => section.seat !== undefined);
  const grouped = useMemo(
    () =>
      sections
        .map((section) => {
          const { connect, more } = splitFields(shownBy.get(sectionKey(section)) ?? []);
          return { section, heading: perSeat ? section.name : "", connect, more };
        })
        .filter((g) => g.connect.length > 0 || g.more.length > 0),
    [sections, shownBy, perSeat],
  );
  const folded = useMemo(
    () => grouped.flatMap(({ section, more }) => more.map((r) => ({ section, r }))),
    [grouped],
  );
  // ONE INTRO PER DISTINCT SENTENCE. Two surfaces of one tool each carry
  // their own summary, and a per-seat app repeats one summary per agent.
  const intros = useMemo(() => {
    const seen = new Set<string>();
    return sections
      .map((section) => section.tool.summary ?? "")
      .filter((text) => text !== "" && !seen.has(text) && seen.add(text));
  }, [sections]);
  // The surfaces whose hooks a person has to register by hand.
  const manual = useMemo(
    () => sections.filter((section) => section.tool.public_url && !section.tool.can_provision),
    [sections],
  );

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
      // A CREDENTIAL SHOWS ITS REFERENCE, or that it is held.
      //
      // `value` on a secret is a `${NAME}` and never the credential: the
      // engine puts a whole reference on the wire and nothing else, so a
      // field pointing at the sealed store opens naming the entry it reads.
      // Only a hand-written LITERAL arrives with no value, and that is what
      // the dots are for. See [HELD].
      if (r.kind === "secret" && !r.value && r.present) {
        initial[key] = HELD;
        return;
      }
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

  // A MINTABLE SECRET IS NOT ON THE FORM AT ALL.
  //
  // There is nothing for a person to type — the engine generates the value,
  // and it has a shape a person would get wrong — so the row was a label
  // over a sentence saying the engine would handle it. That is the engine
  // narrating its own plumbing in the middle of a form somebody is filling
  // in. It is still generated on submit; payloadFor reads the requirement,
  // not the rendering.
  // Both are still SUBMITTED, because payloadFor reads the requirement list
  // rather than the rendering.
  const editable = shownField;

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
      // AN UNTOUCHED CREDENTIAL IS LEFT ALONE. Empty, or still showing the
      // dots that say one is held: either way there is nothing new to write,
      // and submitting it would rewrite a working key with placeholder text.
      // Typing over it is what replaces the credential.
      if (r.kind === "secret" && (value.trim() === "" || value === HELD)) continue;
      send[r.field] = value;
    }
    return { values: send, generate };
  }

  async function submit(): Promise<void> {
    setBusy(true);
    setError("");
    setFieldErrors({});

    // ONE REQUEST PER SURFACE, and only for the surfaces that have
    // something to send. Each is atomic on its own vendor block, so a
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
          const said =
            "The company configuration holds a value at " +
            path +
            " rather than a ${VAR} reference, so there is no variable to store the " +
            "credential in. Clear it in Configuration, then try again.";
          // A GENERATED CREDENTIAL HAS NO FIELD TO SIT UNDER, and it is
          // exactly the one the engine refuses this way, because the
          // operator never sees the slot it will not overwrite. Beside a
          // field that is not rendered, the refusal was invisible.
          if (!editable(target)) {
            setError(said);
            return;
          }
          setFieldErrors({ [valueKey(found.section, target)]: said });
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
      {/* ONE FORM OVER THE WHOLE TOOL.
          Atlassian is Jira and Confluence, and it rendered as two blocks
          under two headings, each with its own intro and its own "More
          settings" fold — which is the config's shape, not the product's. A
          person connecting Atlassian is connecting one thing, and this is
          one form: the surfaces are what the ENGINE talks to, and a reader
          filling in a site address does not need to know which of the two
          config blocks it lands in.

          A PER-SEAT app is the exception and keeps its headings, because
          there each section is a different agent's own credentials and the
          name is what says whose. */}
      <div className="int-form">
        {intros.map((intro, i) => (
          <p key={i} className="int-form-intro">
            {intro}
          </p>
        ))}
        {manual.map((section) => (
          <div key={sectionKey(section)} className="banner neutral">
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
        ))}

        {grouped.map(({ section, heading, connect, more }) => (
          <Fragment key={sectionKey(section) + ":group"}>
            {heading && <strong className="int-section">{heading}</strong>}
            {connect.map((r) => (
              <Fragment key={valueKey(section, r)}>{renderField(section, r)}</Fragment>
            ))}
          </Fragment>
        ))}

        {/* ONE FOLD FOR THE WHOLE TOOL, for the same reason: two "More
            settings" rows in one dialog is the config's shape showing
            through. CLOSED in both directions — a disclosure that sprang
            open because a field inside it was unset would be the settings
            form differing from the connect form. It opens when a submission
            is refused for something inside it, so an error is never hidden
            behind it. */}
        {folded.length > 0 && (
          <details className="int-form-more" open={moreOpen} onToggle={onMoreToggle}>
            <summary className="int-summary">
              More settings
              <span className="faint"> ({folded.length})</span>
            </summary>
            <div className="int-form-more-fields">
              {folded.map(({ section, r }) => (
                <Fragment key={valueKey(section, r)}>{renderField(section, r)}</Fragment>
              ))}
            </div>
          </details>
        )}

        {/* LAST, because it is an alternative to what the form just asked
            for rather than an instruction for filling it in. It is shown
            only where there is a credential to keep somewhere: a form with
            no secret on it has nothing this offers.

            ONE LINE, with the reasoning behind an icon. A paragraph at the
            foot of a form is read once and then never again, and it competes
            with the fields for the same attention; the sentence that says
            what to do is short enough to skim past, and why it is worth
            doing is there for whoever wants it. */}
        {shown.some((r) => r.kind === "secret") && (
          <p className="int-form-note">
            {/* THE MARK BEFORE THE SENTENCE, so the line reads as an aside
                from its first character rather than ending in an icon
                somebody has to go back for.

                A SPAN CARRIES THE TOOLTIP, not the icon: the svg is
                aria-hidden, and a `title` ATTRIBUTE on an svg is not the
                `<title>` CHILD that draws one, so the hover would be silently
                absent. The same words reach a screen reader as text. */}
            <span className="int-form-why" title={WHY_SECRETS}>
              <Icon name="info" size="sm" />
              <span className="sr-only">{WHY_SECRETS}</span>
            </span>
            <span>
              Tip: keep credentials in <a href={href(["secrets"])}>Secrets</a> and reference them
              here as {"${NAME}"}.
            </span>
          </p>
        )}
      </div>

      {error && (
        <div className="banner critical">
          <Icon name="alert" size="sm" />
          {/* THE PROBLEMS, not the paragraph. A validation refusal is several
              of them joined with newlines, which HTML collapses into one run
              where the second problem's config path lands inside the first
              one's sentence. See [Problems]. */}
          <Problems detail={error} />
        </div>
      )}
    </Dialog>
  );

  /**
   * The value key a `{field}` reference in one requirement's text resolves
   * through.
   *
   * IT FOLLOWS THE REFERENCED FIELD, not the referring one. This used to
   * build the key from the section plus the name with `shared` forced off,
   * which is right only while a template names a field in its own section:
   * Jira's site address cites `{org_id}`, and that value is asked once for
   * the whole of Atlassian and lives under the bare name, so the forced key
   * looked up `jira:org_id`, found nothing, and dropped the link every time.
   *
   * The section is still tried FIRST, because a field name is not unique
   * across a tool: Jira and Confluence both declare `url` and they are
   * different addresses.
   */
  function siblingOf(section: SetupSection, field: string): SetupRequirement | undefined {
    const here = (ownBy.get(sectionKey(section)) ?? []).find((r) => r.field === field);
    if (here) return here;
    for (const [, reqs] of ownBy) {
      const shared = reqs.find((r) => r.field === field && r.shared);
      if (shared) return shared;
    }
    return undefined;
  }

  function refKey(section: SetupSection, field: string): string {
    const here = (ownBy.get(sectionKey(section)) ?? []).find((r) => r.field === field);
    if (here) return valueKey(section, here);
    for (const [, reqs] of ownBy) {
      const shared = reqs.find((r) => r.field === field && r.shared);
      if (shared) return shared.field;
    }
    return `${sectionKey(section)}:${field}`;
  }

  /** What the engine reads for a field this company has left blank. */
  function defaultOf(section: SetupSection, field: string): string {
    for (const [key, reqs] of ownBy) {
      const found = reqs.find(
        (r) => r.field === field && (key === sectionKey(section) || r.shared),
      );
      if (found) return found.default ?? "";
    }
    return "";
  }

  // The app's NAME is passed in rather than read from a closure: this is
  // one function over every section's fields, and the link it draws has to
  // say which app it opens.
  function renderField(section: SetupSection, r: SetupRequirement) {
    const appName = section.name;
    const key = valueKey(section, r);
    // A LINK ONLY WHERE IT GOES SOMEWHERE. See [vendorLink].
    // A LINK IS BUILT OUT OF VALUES, so a box holding a reference hands over
    // what the reference READS AS rather than its name: `${ATLASSIAN_ORG_ID}`
    // in the path opened a console page for an organization of that name.
    // What is being TYPED still wins, so a link follows the box as somebody
    // fills it in.
    const link = vendorLink(
      r.vendor_url ?? "",
      values,
      (field) => refKey(section, field),
      (field) => siblingOf(section, field)?.resolved_value ?? "",
    );
    // WHAT THIS FORM CURRENTLY HOLDS, falling back to the field's own
    // default, which is what the engine reads when it is left blank. See
    // [fillTemplate].
    const fill = (text: string) =>
      fillTemplate(text, (field) => values[refKey(section, field)] || defaultOf(section, field));
    return (
      <div key={key} className="col gap-1">
        {editable(r) ? (
          <Field
            label={r.label}
            kind={r.kind === "toggle" ? "choice" : (r.kind as FieldKind)}
            value={values[key] ?? ""}
            onChange={(v) => setValues((c) => ({ ...c, [key]: v }))}
            secrets={secretNames}
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
                {fill(r.help ?? "")}
                {r.where && <> {fill(r.where)}</>}
                {/* THE LINK IS PART OF THE SENTENCE when the app says what
                    to call it: "Create one on your API keys page" sends
                    somebody to the page it names, where a trailing "Open
                    Datadog" makes them work out which of three pages the
                    form meant.

                    WHICH IS WHY THE WORDS OUTLIVE THE ADDRESS. A
                    per-organization page has no address until the
                    organization id is typed, and dropping the whole clause
                    then left "Find the Jira site value under" ending in
                    nothing. The name of the page is the useful half and is
                    true whether or not this form can open it yet, so it
                    stays and only the anchor waits. */}
                {(link || r.link_text) && (
                  <>
                    {" "}
                    {link ? (
                      <a href={link} target="_blank" rel="noreferrer">
                        {r.link_text || `Open ${appName}`}
                      </a>
                    ) : (
                      r.link_text
                    )}
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
