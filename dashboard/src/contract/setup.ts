/**
 * The `/setup` wire: what an integration needs, the words its form says around
 * its fields, and what a Test of a typed credential found.
 *
 * Held against the Go types the engine marshals by
 * `internal/api/setupapi.TestTheSetupFormReadsWhatTheSetupWireSends`, which
 * reads these declarations by name: a field on either side the other does not
 * know fails there rather than as a form that reads `undefined`.
 */

/**
 * What sort of value a requirement holds (`setup.Kinds`). `toggle` is a JSON
 * boolean in the document, `handle` names a seat this company has, and
 * `email` is an address an account is identified by.
 */
export type SetupRequirementKind =
  "secret" | "url" | "id" | "choice" | "text" | "handle" | "email" | "toggle";

/**
 * One thing to do at the app before connecting, in order.
 *
 * `{link}` in `text` is the vendor link of the field `link` names, resolved as
 * that field's own is. `grants` are the permissions, roles or scopes to give
 * there, each in the app's own words; `yields` names the field the step
 * produces a value for.
 */
export interface SetupStep {
  text: string;
  link?: string;
  grants?: string[];
  yields?: string;
}

/** The one risk an app's credential carries, said once per form. */
export interface SetupCaution {
  title: string;
  body: string;
}

/**
 * One input an integration cannot work without, as `/setup` answers it.
 *
 * The screen renders a form from these and knows nothing about any
 * third-party app: every word a person reads is carried here, so adding one
 * adds no branch to a component. See internal/setup for what each field
 * means.
 */
export interface SetupRequirement {
  field: string;
  label: string;
  kind: SetupRequirementKind;
  config_path: string;
  secret_name?: string;
  required: boolean;
  /**
   * Shown and required only when another field holds a given value.
   *
   * A FIELD REQUIRED BY AN ANSWER rather than in general. GitHub's
   * organization token is the pair it exists for: it is the one thing
   * standing between "every repository in the organization" and that being
   * true, and a credential the other answer never uses. `required` alone
   * blocked a connect that needed nothing; optional let a choice be stored
   * that could not be carried out.
   *
   * Evaluated against what the FORM holds, not what is stored: the gating
   * field is being answered in the same dialog, so the stored value is the
   * one being replaced. The API checks the same condition against the
   * submission, so a caller that skips this form is refused too.
   */
  required_when?: { field: string; equals: string };
  /**
   * One of the few fields that ESTABLISH the connection.
   *
   * An ORDER, not a filter: these lead the form, with a rule under them and
   * everything that configures what happens OVER the connection below. Both
   * halves are one form, so an app's settings are the screen its operator
   * already saw when they connected it.
   */
  connect?: boolean;
  /**
   * One value across every surface of this tool, asked once and written to
   * all of them.
   *
   * Atlassian's account email, API token, cloud id and link address: two
   * config blocks, one Atlassian account. Not "same field name": Jira's url
   * and Confluence's url are both `url` and are different addresses.
   */
  shared?: boolean;
  /**
   * The words `{link}` stands for in `hint`, `more` or a step naming this
   * field, linked to `vendor_url` once every `{field}` in that address has a
   * value. Set wherever `vendor_url` is.
   */
  link_text?: string;
  /**
   * What the form offers when this company has no value.
   *
   * A SUGGESTION, not a stored value: nothing is written until submit, so a
   * default that is wrong is one somebody changes rather than one they have
   * to discover.
   */
  default?: string;
  /**
   * What a `${VAR}` in this field currently reads as, for a field that is
   * NOT a credential.
   *
   * A reference is a name, and the links a form draws are built out of
   * VALUES: the Atlassian API keys page is per-organization, so once that id
   * lived in the sealed store the link was built out of the literal text
   * `${ATLASSIAN_ORG_ID}`. Absent on a credential, on a literal (which is
   * already the value) and on a reference naming nothing.
   */
  resolved_value?: string;
  /**
   * Whether the engine can check a typed value for this field at the app
   * without storing it (`POST /setup/integrations/{kind}/verify`): the form
   * offers Test key only where this is set.
   */
  checkable?: boolean;
  /**
   * For a credential the engine cannot check ahead without acting on it,
   * the engine's sentence on when the app proves it ("Slack proves it with
   * the first event it signs."). The form says it rather than offering a
   * test it would have to fake.
   */
  check_note?: string;
  /**
   * The other fields of this list a check of this credential is made with,
   * for an app that proves several values in one check (Datadog's API key
   * and application key, against their region). The form offers one Test
   * control after this field, enabled once this field and every one named
   * here holds a value, and a result about all of them. Absent, the field
   * is checked on its own.
   */
  check_with?: string[];
  mintable?: boolean;
  /**
   * What a person may see of a value the engine minted, rendered by the
   * engine: a setup block holding a minted key's public half, for a person
   * to run at the app. Never the secret itself, which no route returns.
   */
  public?: { text: string; language: string };
  /**
   * Written without being asked for.
   *
   * The `enabled` toggle on every inbound app: connecting an integration and
   * leaving it switched off is not a thing anybody means, so the question
   * was a control whose only sensible answer was the one it already had. It
   * is still submitted, from its default.
   */
  hidden?: boolean;
  /**
   * The one line under the field: what the value is and where it is found,
   * under fifteen words. `{link}` places the field's vendor link, and
   * `{field}` reads another field of the form.
   */
  hint?: string;
  /** What a person may want once about the field, behind its disclosure. */
  more?: string;
  /**
   * The page at the app the value comes from, possibly with `{field}`
   * placeholders filled from the form. No link until each has a value.
   */
  vendor_url?: string;
  choices?: { value: string; label: string; hint?: string }[];
  format?: string;
  /**
   * The one form a minted or pasted value has to take at the app, which the
   * engine holds a value to before it stores or tests it. Absent is a token.
   */
  shape?: string;
  /**
   * The reconcile finding this input being absent produces.
   *
   * ON THE WIRE AND NOT READ BY THIS SCREEN, deliberately. It is the server's
   * join (setupapi's own suite checks that every vendor has a credential
   * field claiming `credential_missing`, so a row asking for a credential
   * cannot offer every field except the credential), and the dialog used to
   * narrow to it when a Fix control existed. It does not any more: a tile's
   * one action (Continue, Rotate token) opens the same settings rather than a
   * narrowed copy (see `actionFor` in Integrations.tsx and `rotate` on the
   * setup dialog).
   *
   * Declared because it is part of the shape the API sends, not because
   * anything here consumes it.
   */
  blocks?: string;
  /**
   * What the document holds here right now, for everything that is NOT a
   * credential: the region, the URL, the handle, the group.
   *
   * It is what the form opens on, so a settings dialog is an edit of a
   * configuration rather than a blank form over one. Absent on a secret, and
   * deliberately: a credential's value has no path onto this wire at all
   * (internal/setup: Requirement.MarshalJSON).
   */
  value?: string;
  /** The document names something here: a literal or a ${VAR}. */
  present: boolean;
  /** Null for a field the document leaves empty: there is nothing to resolve. */
  resolved?: boolean | null;
  seat?: string;
}

export interface SetupToolState {
  key: string;
  configured: boolean;
  enabled: boolean;
  requirements: SetupRequirement[];
  /**
   * One sentence on what connecting this app DOES, from the engine.
   *
   * The connect form opens with it. It lives in Go with the app whose
   * requirements it introduces, for the reason every other word on that form
   * does: this screen knows nothing about any app. Atlassian's organization
   * says it for Jira and Confluence, whose own answers carry none.
   */
  summary?: string;
  /** What to do at the app, in order, before or while the form is filled in. */
  prepare?: SetupStep[];
  /** The one risk this app's credential carries, absent where there is none. */
  caution?: SetupCaution;
  /** Nothing REQUIRED is outstanding. Not a health claim. */
  satisfied: boolean;
  /**
   * Whether every box this app's dialog draws has been answered: the company
   * block's requirements, and every seat's.
   *
   * SEPARATE FROM `satisfied`, and the gap is the whole of what a button can
   * usefully offer. A GitHub company whose agent has no app yet is not
   * satisfied and has nothing left to type: both acts that produce an app
   * happen at GitHub, from that agent's own row.
   */
  form_complete: boolean;
  inbound_path?: string;
  public_url?: string;
  /** This build runs a provisioning pass for this vendor. */
  can_provision?: boolean;
  /**
   * Whether a seat without its own credential makes this app unfinished.
   *
   * Slack and GitHub: an agent with no Slack app cannot post and an agent
   * with no GitHub App acts as nobody, so the roster IS the integration on
   * both. Everywhere else the roster is informational, and reading it as work
   * outstanding puts a Continue button on a working card.
   */
  seats_required?: boolean;
  /**
   * What a person clicks at the third-party app to delete ONE agent's app,
   * once a seat's `manage_url` has opened it. The same for every seat, so it
   * is stated once. Empty where the engine removes what it made on its own.
   */
  manage_path?: string;
  /** The transient third-party app credential its pass asks for, never stored. */
  needs_operator?: SetupRequirement | null;
  /**
   * Per-seat setup, for a third-party app whose credentials live on the
   * seat rather than on the company. Slack is the one: each agent has its
   * own app.
   */
  seats?: SetupSeatState[];
}

export interface SetupSeatState {
  handle: string;
  name?: string;
  requirements: SetupRequirement[];
  /** Whether anything is written down for this seat, working or not. */
  present?: boolean;
  /**
   * Whether this seat is MEANT to be covered, whether or not it holds
   * anything yet.
   *
   * Separate from `present`, and the gap between them is a real state: a
   * GitHub seat that is on and has no app yet has opted in with both of its
   * two acts still to do.
   */
  enrolled?: boolean;
  satisfied: boolean;
  inbound_path?: string;
  public_url?: string;
  /**
   * The variables this seat's own credentials for the app live in, sorted:
   * names, never values. What a disconnect leaves behind.
   */
  secrets?: string[];
  /**
   * The third-party app definition this agent's own app is created from,
   * ready to paste, for an app whose seats a person has to build by hand.
   *
   * The engine's own, byte for byte the one its provisioning command pushes:
   * the scopes, the events and this seat's request URL are what make an app
   * the engine can use. Absent where the company has no public address yet,
   * or where the build has no manifest for the app.
   */
  manifest?: string;
  /**
   * Why there is no manifest, when there could have been one: a company with
   * no public address, or a role name past the app's own name cap. A silent
   * absence is the worst answer, because the field help tells an operator to
   * paste one.
   */
  manifest_note?: string;
  /**
   * The one line under this agent's name: where its own credential for this
   * app is kept, or what is missing. Only Slack has a route per seat, so
   * every other app's roster has this and no path.
   */
  detail?: string;
  /**
   * What is outstanding for this seat, empty when nothing is. A CLOSED SET,
   * so a screen renders the control from the value rather than from the
   * sentence in `detail`: an app this agent has none of, and an app it has
   * that nothing has installed, are two acts by a person minutes or days
   * apart. Widened with `string & {}` because a newer node may name a step
   * this build has no control for, and drawing nothing is the only honest
   * answer to a step it cannot perform.
   */
  step?: "create_app" | "install_app" | (string & {});
  /**
   * Where a person goes for that step, when the engine can address it.
   *
   * EMPTY FOR "create_app", and that is the contract rather than an omission:
   * an app is created by POSTing a manifest from a page carrying the
   * operator's own session at the app, so there is no address to link to. The
   * screen asks the engine for a manifest and submits a form with it.
   */
  action_url?: string;
  /**
   * Where a person goes to DELETE what this seat holds at the third-party
   * app, when deleting it is not something the engine can do.
   *
   * GitHub is why: a teardown can uninstall an agent's app, which revokes its
   * access, but there is no endpoint at any permission for deleting the app
   * registration. That is done from its settings page by its owner, so a
   * disconnect hands over a link rather than claiming to have finished.
   */
  manage_url?: string;
}

/** What checking one typed credential at the app, without storing it, found (`setup.VerifyOutcomes`). */
export type VerifyOutcome = "works" | "refused" | "unreachable" | "unchecked";

/**
 * The answer of `POST /setup/integrations/{kind}/verify`: `works` (the app
 * accepted it), `refused` (the app answered and refused it), `unreachable`
 * (the app did not answer; nothing was stored) or `unchecked` (the app only
 * proves this value later).
 *
 * `summary` is the answer in one sentence, said beside the Test control;
 * `notes` are the facts after it, a sentence each, behind Details. Neither
 * ever repeats the value that was checked.
 */
export interface SetupVerdict {
  outcome: VerifyOutcome;
  summary: string;
  notes?: string[];
}
