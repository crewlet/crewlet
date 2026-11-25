/**
 * The `/setup` wire, held to the engine's Go types by
 * `internal/api/setupapi.TestTheSetupFormReadsWhatTheSetupWireSends`.
 */

import type { VendorTextPart } from "./vendortext.ts";

/** What sort of value a requirement holds (`setup.Kinds`); a toggle is a JSON boolean. */
export type SetupRequirementKind =
  | "secret"
  | "url"
  | "id"
  | "choice"
  | "text"
  | "handle"
  | "email"
  | "toggle"
  | "add_identity"
  | "consent"
  | "redirect";

/** One step at the app, its vendor UI names marked and its page linked. */
export interface SetupStep {
  text: VendorTextPart[];
  /** Permissions, roles or scopes to grant there, in the app's own words. */
  grants?: string[];
  /** The field the step produces a value for. */
  yields?: string;
  /** The integration this step connects first. */
  needs?: string;
  /** Whether `needs` is connected: on, with nothing it requires outstanding. */
  connected?: boolean;
  /** Whether the company is past this step; the first step not done is the one it is on. */
  done: boolean;
}

/** The one risk an app's credential carries, said once per form. */
export interface SetupCaution {
  title: string;
  body: string;
}

/** An identity a person adds at the vendor; `seat` names the agent when `level` is "seat". */
interface SetupIdentityStep {
  email: string;
  role: { id: string; name: string };
  where: VendorTextPart[];
  level: "company" | "seat";
  seat?: string;
  /** Whether the engine saw the identity hold the role. */
  state: "present" | "missing" | "unknown";
}

/** A `redirect` step's address: the engine's sign-in callback, "" with no public address, and the setting it is built on. */
interface SetupRedirectStep {
  url: string;
  key: string;
}

/** One input an integration needs; every word the form draws comes from here. */
export interface SetupRequirement {
  field: string;
  label: string;
  kind: SetupRequirementKind;
  config_path: string;
  secret_name?: string;
  required: boolean;
  /** Shown and required only while another field of the form holds a value. */
  required_when?: { field: string; equals: string };
  /** A field that establishes the connection: these lead the form. */
  connect?: boolean;
  /** One value across every surface of the tool, asked once (Atlassian's account). */
  shared?: boolean;
  /** A suggestion the form offers where the company has no value, never stored by itself. */
  default?: string;
  /** What a non-credential `${VAR}` here reads as, for the links built from it. */
  resolved_value?: string;
  /** The app can check a typed value, a key or not, without storing it, so the form offers Test. */
  checkable?: boolean;
  /** When the app proves a credential no Test can check ahead. */
  check_note?: string;
  /** The other fields one check of this credential is made with. */
  check_with?: string[];
  mintable?: boolean;
  /**
   * The engine's rendering of a minted value's public half, to copy to the app,
   * and the fields whose saved answers it is drawn from.
   */
  public?: { text: string; language: string; drawn_from?: string[] };
  /** The field whose block, run at the app, prints this value: needed to finish, never to save. */
  printed_by?: string;
  /** An `add_identity` step: the email to add at the vendor, at which role and where. */
  identity?: SetupIdentityStep;
  /** A `redirect` step: the address to register at the vendor. */
  redirect?: SetupRedirectStep;
  /** Written from its default without being asked. */
  hidden?: boolean;
  /** The one line under the field, under fifteen words; a link waits for each `{field}` in its address. */
  hint?: VendorTextPart[];
  /** What a person may want once about the field, behind its disclosure. */
  more?: VendorTextPart[];
  choices?: { value: string; label: string; hint?: string }[];
  format?: string;
  /** The one form a value has to take at the app; absent is a token. */
  shape?: string;
  /** A value that spans lines (a PEM key): a box that keeps line breaks. */
  multiline?: boolean;
  /** The file types such a value comes in, for a file picker. */
  accept?: string;
  /** The reconcile finding this input clears; the server's join, not read here. */
  blocks?: string;
  /** What the document holds here, for everything that is not a credential. */
  value?: string;
  /** The document names something here: a literal or a `${VAR}`. */
  present: boolean;
  /** Null for a field the document leaves empty. */
  resolved?: boolean | null;
  seat?: string;
}

/** One app's setup: its requirements and the words its form says around them. */
/** One step of an SSH server's latest Test connection. */
interface SetupSSHTestStep {
  id: string;
  ok: boolean;
  detail: string;
}

/** An SSH server's latest Test connection with the current authority. */
interface SetupSSHTest {
  host: string;
  /** The authority's SHA-256 fingerprint. */
  authority: string;
  at: string;
  by: string;
  by_kind: string;
  operator_id?: string;
  ok: boolean;
  steps: SetupSSHTestStep[];
}

/**
 * One registered SSH server, its keys as fingerprints and as the lines it pins,
 * which an edit sends back whole with its allowlist.
 */
export interface SetupSSHServer {
  id: string;
  address: string;
  port: number;
  fingerprints: string[];
  host_keys: string[];
  users: { read: string; operate?: string };
  description?: string;
  containers?: boolean;
  /** The commands the read user may run, where they replace the default list. */
  read_allowlist?: string[];
  /** Whether the engine itself reads the server for the atlas. Off when absent. */
  inventory?: boolean;
  /** Every agent naming it, on or off: it is not removed while one does. */
  granted_to: string[];
  last_test?: SetupSSHTest;
}

/**
 * One host key a server presented to a scan (`ssh.ScannedKey`): its line, its
 * type and fingerprint, and why it cannot be pinned where it cannot.
 */
export interface SetupSSHScannedKey {
  line: string;
  type: string;
  fingerprint_sha256: string;
  problem?: string;
}

/** One command of the atlas's inventory, as the server's gate takes it. */
interface SetupSSHInventoryCommand {
  command: string;
  /** Run only on a server whose read user may list containers. */
  containers: boolean;
}

/** The SSH integration's servers and the authority lines servers trust. */
interface SetupSSHServers {
  /** The revision the servers were read from: an edit's `if_match`. */
  revision_id: string;
  ca_public_key?: string;
  next_ca_public_key?: string;
  /** The lines a rotation retired, newest last. */
  retired_ca_public_keys: string[];
  setup_version: number;
  /** The closed set the engine runs on a server that opts in to the atlas's inventory. */
  inventory_commands: SetupSSHInventoryCommand[];
  hosts: SetupSSHServer[];
}

export interface SetupToolState {
  key: string;
  configured: boolean;
  enabled: boolean;
  requirements: SetupRequirement[];
  /** The SSH integration's servers, on its own answer alone. */
  ssh?: SetupSSHServers;
  /** What connecting does; Atlassian's organization says it for Jira and Confluence. */
  summary?: string;
  /** What to do at the app, in order. */
  prepare?: SetupStep[];
  caution?: SetupCaution;
  /** Nothing required is outstanding; not a health claim. */
  satisfied: boolean;
  /** Every box the dialog draws is answered, the seats' included. */
  form_complete: boolean;
  inbound_path?: string;
  public_url?: string;
  can_provision?: boolean;
  /** An agent without its own credential leaves the app unfinished (Slack, GitHub). */
  seats_required?: boolean;
  /** What a person clicks at the app to delete one agent's app. */
  manage_path?: string;
  /** The Crewlet service the document declares in this surface's place, so its block cannot be written beside it. */
  held_by?: "doclet" | "worklet" | (string & {});
  needs_operator?: SetupRequirement | null;
  /** Per-agent setup, for an app whose credentials live on the seat. */
  seats?: SetupSeatState[];
}

/** What one agent still needs before it can act as itself. */
export interface SetupSeatState {
  handle: string;
  name?: string;
  requirements: SetupRequirement[];
  /** Something is written down for this seat, working or not. */
  present?: boolean;
  /** The seat is meant to be covered, whether or not it holds anything yet. */
  enrolled?: boolean;
  satisfied: boolean;
  inbound_path?: string;
  public_url?: string;
  /** The variables this seat's own credentials live in: names, never values. */
  secrets?: string[];
  /** The app definition this agent's app is created from, ready to paste. */
  manifest?: string;
  /** Why there is no manifest, where there could have been one. */
  manifest_note?: string;
  detail?: string;
  /** What is outstanding, a closed set widened for a newer node's step. */
  step?: "create_app" | "install_app" | (string & {});
  action_url?: string;
  /** Where a person deletes what the engine cannot (a GitHub App registration). */
  manage_url?: string;
}

/** What a Test of a typed credential found (`setup.VerifyOutcomes`). */
export type VerifyOutcome = "works" | "refused" | "unreachable" | "unchecked";

/** One thing a Test read: a term and its value. */
export interface SetupVerdictFact {
  label: string;
  value: string;
  /** The value is an ID (a key, a team), drawn in the mono face. */
  identifier?: boolean;
}

/** One risk or problem a Test found, with what to do. A failed Test may leave the title to its summary. */
export interface SetupVerdictWarning {
  title?: string;
  action: string;
}

/** A Test's answer: one sentence beside the control, the facts it read and the warnings it raises. */
export interface SetupVerdict {
  outcome: VerifyOutcome;
  summary: string;
  facts?: SetupVerdictFact[];
  warnings?: SetupVerdictWarning[];
  /** The step a refused check stopped at, by its id; the steps after it did not run. */
  failed_at?: string;
  /** What each step that ran found, by its id. */
  steps?: SetupVerdictStep[];
}

/** What one step of a check found: why it failed or what it read, and the vendor's answers in a machine's line. */
export interface SetupVerdictStep {
  id: string;
  said?: string;
  detail?: string;
}

/** A consent sign-in (`POST /setup/integrations/{kind}/consent`, `GET /setup/consent-sign-ins/{id}`). */
export interface SetupConsentSignIn {
  id: string;
  kind: string;
  status: "pending" | "completing" | "approved" | "failed";
  detail?: string;
  started_at: string;
  expires_at: string;
  /** Where the browser goes to consent: on the start's answer alone. */
  authorize_url?: string;
}

/** A consent grant given up with its integration's block, on a forced disconnect's answer (`oauth`). */
export interface SetupConsentRevocation {
  /** What Google was asked: `shared` where revoking would have ended another product's sign-in. */
  revocation: "revoked" | "not_offered" | "failed" | "shared";
  said: string;
}
